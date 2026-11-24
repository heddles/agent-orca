/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apiserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/security"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// POST /hooks/{id}: start a run of one agent from outside. There is no session:
// the request is authenticated by an HMAC over the timestamp and body, made
// with the key derived from the trigger's secret (docs/integrations.md).
//
// Order of the checks, cheapest and least revealing first. An unknown, disabled
// or deleted trigger is the same 404, so ids cannot be probed; a bad
// timestamp or signature is 401 and reaches neither the rate limiter nor the
// cluster. Everything after the lookup runs as the tenant the trigger row names,
// with the runner role at most, in the trigger's namespace only.

const (
	// HookSignatureHeader carries "sha256=" and the hex HMAC-SHA256 of
	// "<timestamp>.<body>".
	HookSignatureHeader = "X-AgentOrca-Signature"
	// HookTimestampHeader carries the Unix time, in seconds, the sender signed.
	HookTimestampHeader = "X-AgentOrca-Timestamp"

	// HookMaxBodyBytes is the largest body a hook takes.
	HookMaxBodyBytes = 64 << 10
	// HookReplayWindow is how far a timestamp may be from the operator's clock.
	HookReplayWindow = 5 * time.Minute

	// LabelTriggerID marks a run a trigger started.
	LabelTriggerID = "agentorca.io/trigger"
	// TriggerActorPrefix starts the created-by value of such a run.
	TriggerActorPrefix = "trigger:"
)

// hookState is what the hook endpoint keeps between requests.
type hookState struct {
	once    sync.Once
	limiter *RateLimiter
	replay  *replayCache

	// now and tenant are replaced in tests. A nil tenant resolves through the
	// TenantConfig registry.
	now    func() time.Time
	tenant func(tenantID string) (*TenantIdentity, bool)
}

func (h *hookState) init() {
	h.once.Do(func() {
		h.limiter = NewRateLimiter()
		h.replay = &replayCache{seen: map[string]time.Time{}}
		if h.now == nil {
			h.now = time.Now
		}
	})
}

// replayCache remembers the signatures accepted inside the replay window, so an
// intercepted request cannot be sent again while its timestamp is still fresh.
// It is per process: with several replicas a replay may land on another one
// (documented in docs/integrations.md).
type replayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// add records key until expires and reports false when it was already there.
func (c *replayCache) add(key string, now, expires time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, exp := range c.seen {
		if now.After(exp) {
			delete(c.seen, k)
		}
	}
	if _, dup := c.seen[key]; dup {
		return false
	}
	c.seen[key] = expires
	return true
}

func (c *replayCache) forget(key string) {
	c.mu.Lock()
	delete(c.seen, key)
	c.mu.Unlock()
}

// SignHook returns the signature header value for a request: what a sender
// computes. key is postgresql.SigningKeyFor(secret).
func SignHook(key, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(timestamp))
	m.Write([]byte("."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// verifyHookSignature compares header with the signature of the request in constant time.
func verifyHookSignature(key, timestamp string, body []byte, header string) bool {
	got, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	want, _ := hex.DecodeString(strings.TrimPrefix(SignHook(key, timestamp, body), "sha256="))
	return hmac.Equal(got, want)
}

// tenantForHook is the identity a trigger's runs are made under: the tenant the
// trigger row names if it is still active, limited to the runner role. A
// suspended or deleted tenant yields false.
func (s *UIServer) tenantForHook(tenantID string) (*TenantIdentity, bool) {
	if s.hooks.tenant != nil {
		return s.hooks.tenant(tenantID)
	}
	a := s.externalAuth
	tc := a.tenantConfigByID(tenantID)
	if tc == nil || a.checkTenant(tenantID, tc.Name) != nil {
		return nil, false
	}
	namespaces := slices.Clone(tc.Spec.AllowedNamespaces)
	id := &TenantIdentity{
		TenantID:      tenantID,
		TenantName:    tc.Name,
		Namespaces:    namespaces,
		AllowedAgents: slices.Clone(tc.Spec.AllowedAgents),
		Roles:         tenancy.RoleRunner.Expand(),
	}
	if len(namespaces) > 0 {
		id.Namespace = namespaces[0]
	}
	return attachQuotaFields(id, tc), true
}

func (s *UIServer) handleHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.hooks.init()
	id := r.PathValue("id")
	store := s.triggerSource()
	if store == nil || !postgresql.ValidTriggerID(id) {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	t, err := store.GetTrigger(postgresql.WithPlatform(r.Context()), id)
	if errors.Is(err, postgresql.ErrTriggerNotFound) || (err == nil && !t.Enabled) {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("reading a trigger", "trigger", id, "err", err)
		jsonError(w, "trigger lookup failed", http.StatusInternalServerError)
		return
	}

	if r.ContentLength > HookMaxBodyBytes {
		jsonError(w, fmt.Sprintf("the body is over %d bytes", HookMaxBodyBytes), http.StatusRequestEntityTooLarge)
		return
	}
	body, err := readAllLimited(w, r, HookMaxBodyBytes)
	if err != nil {
		jsonError(w, fmt.Sprintf("the body is unreadable or over %d bytes", HookMaxBodyBytes), http.StatusRequestEntityTooLarge)
		return
	}

	now := s.hooks.now()
	ts := r.Header.Get(HookTimestampHeader)
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		jsonError(w, "missing or malformed "+HookTimestampHeader, http.StatusUnauthorized)
		return
	}
	if d := now.Sub(time.Unix(sec, 0)); d > HookReplayWindow || d < -HookReplayWindow {
		jsonError(w, "timestamp outside the replay window", http.StatusUnauthorized)
		return
	}
	sig := r.Header.Get(HookSignatureHeader)
	if !verifyHookSignature(t.SigningKey, ts, body, sig) {
		jsonError(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	replayKey := t.ID + "|" + sig
	if !s.hooks.replay.add(replayKey, now, now.Add(2*HookReplayWindow)) {
		jsonError(w, "this request was already received", http.StatusUnauthorized)
		return
	}
	if wait, ok := s.hooks.limiter.Allow("trigger:"+t.ID, t.RateLimitPerMin); !ok {
		s.hooks.replay.forget(replayKey)
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
		jsonError(w, "rate limit of this trigger exceeded", http.StatusTooManyRequests)
		return
	}

	input, status, msg := hookInput(body, t.InputPath)
	if status != 0 {
		jsonError(w, msg, status)
		return
	}

	identity, ok := s.tenantForHook(t.TenantID)
	if !ok {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	ctx := contextWithTenant(r.Context(), identity)

	var agent agentorcav1alpha1.Agent
	if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Agent}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			jsonError(w, "not found", http.StatusNotFound)
			return
		}
		jsonError(w, "reading the agent failed", http.StatusInternalServerError)
		return
	}
	if !canRead(ctx, &agent, kindAgent) || !namespaceAllowed(ctx, agent.Namespace) || !tenantCanAccessAgent(identity, agent.Name) ||
		agent.Labels[LabelTemplate] == templateMarker {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}

	if qerr, ok := enforceQuotas(ctx, s.crdClient, s.externalAuth.RateLimiter(), identity).(*QuotaError); ok && qerr != nil {
		if qerr.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(qerr.RetryAfter.Seconds())))
		}
		jsonError(w, qerr.Message, qerr.Code)
		return
	}

	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("hook-%s-", agent.Name),
			Namespace:    agent.Namespace,
			Labels: map[string]string{
				security.LabelManagedBy: security.ManagedByValue,
				"agentorca.io/tenant":   identity.TenantName,
				LabelTriggerID:          t.ID,
			},
			Annotations: map[string]string{AnnotationCreatedBy: TriggerActorPrefix + t.ID},
		},
		Spec: agentorcav1alpha1.AgentRunSpec{AgentRef: agent.Name, Input: input},
	}
	// The tenant ID comes from the trigger row, never from the request.
	tenancy.Stamp(run, t.TenantID)
	if err := s.crdClient.Create(ctx, run); err != nil {
		slog.Error("creating a hook run", "trigger", t.ID, "err", err)
		jsonError(w, "creating the run failed", http.StatusInternalServerError)
		return
	}
	if err := store.RecordTriggerFire(postgresql.WithPlatform(r.Context()), t.ID, now); err != nil {
		slog.Warn("recording a trigger fire", "trigger", t.ID, "err", err)
	}
	slog.Info("hook run created", "trigger", t.ID, "run", run.Name, "agent", agent.Name, "tenant", identity.TenantName)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"runName": run.Name, "namespace": run.Namespace})
}

// hookInput picks the run's input from the body: all of it, or the value at
// the trigger's dotted JSON path. It returns an HTTP status (0 for none) and
// message when the body does not fit the trigger.
func hookInput(body []byte, path string) (string, int, string) {
	text := strings.TrimSpace(string(body))
	if path == "" {
		if text == "" {
			return "", http.StatusUnprocessableEntity, "the body is empty"
		}
		return text, 0, ""
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", http.StatusUnprocessableEntity, "this trigger reads the input from a JSON field, but the body is not JSON"
	}
	for seg := range strings.SplitSeq(path, ".") {
		switch v := doc.(type) {
		case map[string]any:
			next, ok := v[seg]
			if !ok {
				return "", http.StatusUnprocessableEntity, "the body has no field " + path
			}
			doc = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(v) {
				return "", http.StatusUnprocessableEntity, "the body has no field " + path
			}
			doc = v[i]
		default:
			return "", http.StatusUnprocessableEntity, "the body has no field " + path
		}
	}
	switch v := doc.(type) {
	case string:
		text = strings.TrimSpace(v)
	case nil:
		text = ""
	default:
		b, _ := json.Marshal(v)
		text = string(b)
	}
	if text == "" {
		return "", http.StatusUnprocessableEntity, "the field " + path + " is empty"
	}
	return text, 0, ""
}

// readAllLimited reads at most max bytes of the body and fails when there are more.
func readAllLimited(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, max))
}

// contextWithTenant puts a tenant identity on ctx, as the auth middleware does.
func contextWithTenant(ctx context.Context, t *TenantIdentity) context.Context {
	return context.WithValue(ctx, tenantIdentityKey, t)
}

// isPublicEndpoint is true for the paths that carry their own authentication
// and so are served without a session.
func isPublicEndpoint(path string) bool {
	return strings.HasPrefix(path, "/hooks/") || inboundAuthPublic(path)
}
