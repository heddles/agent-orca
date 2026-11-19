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
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
)

// The chat widget (docs/integrations.md): a script a page loads to chat with one
// published agent, with no credential of the tenant in the page.
//
//	GET  /widget/v1.js                       the script (public, cacheable)
//	POST /widget/v1/{key}/sessions           issue a short-lived session token
//	POST /widget/v1/{key}/messages           send a message in the session
//	GET  /widget/v1/{key}/runs/{run}         poll for the answer
//
// What stands between the open internet and the agent:
//
//   - the key names one agent, in one tenant; the visitor never names an
//     agent, a deployment or a namespace;
//   - the Origin header must be one of the key's allowed origins. A browser
//     sets it and a page cannot change it, so another site cannot use the key
//     from a visitor's browser. A script outside a browser can send any Origin:
//     the limits below, not the origin, are what bound it;
//   - a session token (HMAC, 30 minutes, bound to key, session and origin) is
//     needed for messages and polls; it carries no tenant credential and is
//     useless for any other endpoint;
//   - per-key request rate, per-key concurrency and a per-key daily budget,
//     then the tenant's own quotas.
//
// Messages go through startChatTurn and saveChatResponse, the code of the UI's
// chat, so a widget conversation is a normal chat session of the deployment.

const (
	// WidgetSessionTTL is how long a session token is valid.
	WidgetSessionTTL = 30 * time.Minute
	// widgetHeader carries the session token. It is not Authorization, which the
	// UI proxy replaces.
	widgetHeader = "X-Widget-Session"

	widgetMaxBody     = 8 << 10
	widgetMaxInput    = 4000
	widgetMaxActive   = 5
	widgetPollsPerMsg = 30
	// widgetRequestTimeout bounds one widget request.
	widgetRequestTimeout = 30 * time.Second

	// The phases a poll answers with besides "running".
	widgetPhaseDone   = "done"
	widgetPhaseFailed = "failed"

	// LabelWidgetKey and LabelWidgetSession mark a run a widget started.
	LabelWidgetKey = "agentorca.io/widget-key"
	// WidgetActorPrefix starts the created-by value of such a run.
	WidgetActorPrefix = "widget:"
)

//go:embed widget/v1.js
var widgetScript []byte

// widgetState is what the public widget endpoints keep. Its limiter, clock and
// tenant resolver are those of hooks (s.hooks).
type widgetState struct {
	// secret overrides the session signing key (tests).
	secret []byte
	saveMu sync.Mutex
}

// widgetSecret is the key session tokens are signed with: derived from the
// operator's persisted signing key with its own label, so every replica agrees
// on it and it can sign nothing else.
func (s *UIServer) widgetSecret() []byte {
	if s.widget.secret != nil {
		return s.widget.secret
	}
	if s.externalAuth == nil || s.externalAuth.signingKey == nil {
		return nil
	}
	h := hmac.New(sha256.New, []byte("agentorca-widget-session-v1"))
	h.Write(s.externalAuth.signingKey.D.Bytes())
	return h.Sum(nil)
}

type widgetToken struct {
	Key     string `json:"k"`
	Session string `json:"s"`
	Origin  string `json:"o"`
	Expires int64  `json:"e"`
}

func signWidgetToken(secret []byte, t widgetToken) string {
	payload, _ := json.Marshal(t)
	body := base64.RawURLEncoding.EncodeToString(payload)
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// verifyWidgetToken returns the claims of token when its signature is right
// and it has not expired.
func verifyWidgetToken(secret []byte, token string, now time.Time) (widgetToken, bool) {
	body, sig, ok := strings.Cut(token, ".")
	if !ok || len(secret) == 0 {
		return widgetToken{}, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return widgetToken{}, false
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(body))
	if !hmac.Equal(got, m.Sum(nil)) {
		return widgetToken{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return widgetToken{}, false
	}
	var t widgetToken
	if json.Unmarshal(payload, &t) != nil || now.Unix() >= t.Expires {
		return widgetToken{}, false
	}
	return t, true
}

func (s *UIServer) widgetMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/widget/v1.js", s.handleWidgetScript)
	mux.HandleFunc("/widget/v1/{key}/sessions", s.handleWidgetSession)
	mux.HandleFunc("/widget/v1/{key}/messages", s.handleWidgetMessage)
	mux.HandleFunc("/widget/v1/{key}/runs/{run}", s.handleWidgetRun)
	return mux
}

func (s *UIServer) handleWidgetScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(widgetScript)
}

// widgetGate looks the key up and applies the origin rule and CORS. It reports
// whether the handler may go on; when it returns false the answer is written
// (a preflight is answered here too).
func (s *UIServer) widgetGate(w http.ResponseWriter, r *http.Request, wantMethod string) (postgresql.WidgetKey, bool) {
	s.hooks.init()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	store := s.widgetKeySource()
	id := r.PathValue("key")
	if store == nil || !postgresql.ValidWidgetKeyID(id) {
		jsonError(w, "not found", http.StatusNotFound)
		return postgresql.WidgetKey{}, false
	}
	k, err := store.GetWidgetKey(postgresql.WithPlatform(r.Context()), id)
	if errors.Is(err, postgresql.ErrWidgetKeyNotFound) || (err == nil && !k.Enabled) {
		jsonError(w, "not found", http.StatusNotFound)
		return postgresql.WidgetKey{}, false
	}
	if err != nil {
		slog.Error("reading a widget key", "key", id, "err", err)
		jsonError(w, "widget lookup failed", http.StatusInternalServerError)
		return postgresql.WidgetKey{}, false
	}
	origin := r.Header.Get("Origin")
	if !k.OriginAllowed(origin) {
		jsonError(w, "this page is not allowed to use the widget", http.StatusForbidden)
		return postgresql.WidgetKey{}, false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Vary", "Origin")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", wantMethod+", OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, "+widgetHeader)
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return postgresql.WidgetKey{}, false
	}
	if r.Method != wantMethod {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return postgresql.WidgetKey{}, false
	}
	return k, true
}

// widgetLimit takes one request from the key's bucket named kind. The rate is
// in the bucket's name so changing a key's rate starts a fresh bucket.
func (s *UIServer) widgetLimit(w http.ResponseWriter, k postgresql.WidgetKey, kind string, perMin int) bool {
	wait, ok := s.hooks.limiter.Allow(fmt.Sprintf("widget-%s:%s:%d", kind, k.ID, perMin), perMin)
	if ok {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
	jsonError(w, "too many messages, try again in a moment", http.StatusTooManyRequests)
	return false
}

// widgetSession verifies the session token of a request for key k.
func (s *UIServer) widgetSession(w http.ResponseWriter, r *http.Request, k postgresql.WidgetKey) (widgetToken, bool) {
	t, ok := verifyWidgetToken(s.widgetSecret(), r.Header.Get(widgetHeader), s.hooks.now())
	if !ok || t.Key != k.ID || t.Origin != r.Header.Get("Origin") || !k.OriginAllowed(t.Origin) {
		jsonError(w, "the session is missing or expired", http.StatusUnauthorized)
		return widgetToken{}, false
	}
	return t, true
}

func (s *UIServer) handleWidgetSession(w http.ResponseWriter, r *http.Request) {
	k, ok := s.widgetGate(w, r, http.MethodPost)
	if !ok {
		return
	}
	secret := s.widgetSecret()
	if len(secret) == 0 {
		jsonError(w, "the widget is not available", http.StatusServiceUnavailable)
		return
	}
	if !s.widgetLimit(w, k, "session", k.RateLimitPerMin) {
		return
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		jsonError(w, "creating the session failed", http.StatusInternalServerError)
		return
	}
	exp := s.hooks.now().Add(WidgetSessionTTL)
	token := signWidgetToken(secret, widgetToken{Key: k.ID, Session: "wgs-" + hex.EncodeToString(b[:]), Origin: r.Header.Get("Origin"), Expires: exp.Unix()})
	jsonResponse(w, map[string]any{"token": token, "expiresAt": exp.UTC().Format(time.RFC3339), "agent": k.Agent})
}

// widgetTarget resolves the tenant, the agent and the deployment a key stands
// for, from the key's row alone. ok is false (and the answer written) when any
// of them is gone or no longer allowed.
func (s *UIServer) widgetTarget(w http.ResponseWriter, r *http.Request, k postgresql.WidgetKey) (context.Context, *TenantIdentity, *agentorcav1alpha1.AgentDeployment, bool) {
	identity, ok := s.tenantForHook(k.TenantID)
	if !ok {
		jsonError(w, "not found", http.StatusNotFound)
		return nil, nil, nil, false
	}
	ctx := contextWithTenant(r.Context(), identity)
	var agent agentorcav1alpha1.Agent
	if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: k.Agent}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			jsonError(w, "not found", http.StatusNotFound)
		} else {
			jsonError(w, "reading the agent failed", http.StatusInternalServerError)
		}
		return nil, nil, nil, false
	}
	if !canRead(ctx, &agent, kindAgent) || !namespaceAllowed(ctx, agent.Namespace) || !tenantCanAccessAgent(identity, agent.Name) || agent.Labels[LabelTemplate] == templateMarker {
		jsonError(w, "not found", http.StatusNotFound)
		return nil, nil, nil, false
	}
	var deps agentorcav1alpha1.AgentDeploymentList
	if err := s.crdClient.List(ctx, &deps, client.InNamespace(k.Namespace)); err != nil {
		jsonError(w, "reading the agent failed", http.StatusInternalServerError)
		return nil, nil, nil, false
	}
	for i := range deps.Items {
		if d := &deps.Items[i]; d.Spec.AgentRef == k.Agent && canWrite(ctx, d, "AgentDeployment") {
			return ctx, identity, d, true
		}
	}
	jsonError(w, "this agent is not published", http.StatusNotFound)
	return nil, nil, nil, false
}

// widgetUsage is the spend of today's runs of the key and how many are still running.
func (s *UIServer) widgetUsage(ctx context.Context, k postgresql.WidgetKey) (spend float64, active int, err error) {
	var runs agentorcav1alpha1.AgentRunList
	if err := s.crdClient.List(ctx, &runs, client.InNamespace(k.Namespace), client.MatchingLabels{LabelWidgetKey: k.ID}); err != nil {
		return 0, 0, err
	}
	today := s.hooks.now().UTC().Truncate(24 * time.Hour)
	for i := range runs.Items {
		r := &runs.Items[i]
		switch r.Status.Phase {
		case agentorcav1alpha1.AgentRunPhaseSucceeded, agentorcav1alpha1.AgentRunPhaseFailed, agentorcav1alpha1.AgentRunPhaseHandedOff, agentorcav1alpha1.AgentRunPhaseWaitingForInput:
		default:
			active++
		}
		if !r.CreationTimestamp.IsZero() && r.CreationTimestamp.UTC().Before(today) {
			continue
		}
		if v, err := strconv.ParseFloat(r.Status.SpendUSD, 64); err == nil {
			spend += v
		}
	}
	return spend, active, nil
}

func (s *UIServer) handleWidgetMessage(w http.ResponseWriter, r *http.Request) {
	k, ok := s.widgetGate(w, r, http.MethodPost)
	if !ok {
		return
	}
	tok, ok := s.widgetSession(w, r, k)
	if !ok {
		return
	}
	var req struct {
		Input string `json:"input"`
	}
	if !readJSONBody(w, r, widgetMaxBody, &req) {
		return
	}
	input := strings.TrimSpace(req.Input)
	if input == "" || len([]rune(input)) > widgetMaxInput {
		jsonError(w, fmt.Sprintf("write a message of up to %d characters", widgetMaxInput), http.StatusBadRequest)
		return
	}
	if !s.widgetLimit(w, k, "message", k.RateLimitPerMin) {
		return
	}
	ctx, identity, dep, ok := s.widgetTarget(w, r, k)
	if !ok {
		return
	}
	spend, active, err := s.widgetUsage(ctx, k)
	if err != nil {
		jsonError(w, "checking the budget failed", http.StatusInternalServerError)
		return
	}
	if spend >= k.BudgetUSDPerDay {
		jsonError(w, "this chat has reached its daily limit; try again tomorrow", http.StatusPaymentRequired)
		return
	}
	if active >= widgetMaxActive {
		w.Header().Set("Retry-After", "5")
		jsonError(w, "the agent is busy, try again in a moment", http.StatusTooManyRequests)
		return
	}
	if qerr, ok := enforceQuotas(ctx, s.crdClient, s.externalAuth.RateLimiter(), identity).(*QuotaError); ok && qerr != nil {
		if qerr.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(qerr.RetryAfter.Seconds())))
		}
		jsonError(w, "the agent is not available right now", qerr.Code)
		return
	}
	run, cerr := s.startChatTurn(ctx, dep, k.Namespace, dep.Name, tok.Session, input,
		map[string]string{LabelWidgetKey: k.ID},
		map[string]string{AnnotationCreatedBy: WidgetActorPrefix + k.ID})
	if cerr != nil {
		slog.Error("starting a widget chat turn", "key", k.ID, "err", cerr.msg)
		jsonError(w, "the message could not be sent", cerr.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"runName": run.Name})
}

func (s *UIServer) handleWidgetRun(w http.ResponseWriter, r *http.Request) {
	k, ok := s.widgetGate(w, r, http.MethodGet)
	if !ok {
		return
	}
	tok, ok := s.widgetSession(w, r, k)
	if !ok {
		return
	}
	if !s.widgetLimit(w, k, "poll", k.RateLimitPerMin*widgetPollsPerMsg) {
		return
	}
	ctx, _, _, ok := s.widgetTarget(w, r, k)
	if !ok {
		return
	}
	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: r.PathValue("run")}, &run); err != nil ||
		run.Labels[LabelWidgetKey] != k.ID || run.Labels["agentorca.io/session"] != tok.Session {
		// Not this session's run: the same answer as a run that does not exist.
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	switch run.Status.Phase {
	case agentorcav1alpha1.AgentRunPhaseSucceeded:
		s.widgetFinish(ctx, k, tok.Session, run.Name, run.Status.Output)
		jsonResponse(w, map[string]string{"phase": widgetPhaseDone, "output": run.Status.Output})
	case agentorcav1alpha1.AgentRunPhaseWaitingForInput:
		out := run.Status.ClarifyQuestion
		s.widgetFinish(ctx, k, tok.Session, run.Name, out)
		jsonResponse(w, map[string]string{"phase": widgetPhaseDone, "output": out})
	case agentorcav1alpha1.AgentRunPhaseFailed, agentorcav1alpha1.AgentRunPhaseHandedOff:
		jsonResponse(w, map[string]string{"phase": widgetPhaseFailed, "error": "The agent could not answer. Please try again."})
	default:
		jsonResponse(w, map[string]string{"phase": "running"})
	}
}

// widgetFinish appends the answer of a finished run to the session once, as the
// UI does when its stream ends. A poll that finds the answer already saved (the
// last message is the assistant's) does nothing.
func (s *UIServer) widgetFinish(ctx context.Context, k postgresql.WidgetKey, session, runName, output string) {
	if s.checkpoint == nil || output == "" {
		return
	}
	s.widget.saveMu.Lock()
	defer s.widget.saveMu.Unlock()
	cp, err := sessionStore(ctx, s.checkpoint).Load(ctx, session)
	if err != nil || cp == nil || cp.LastRunRef != runName || len(cp.ConversationHistory) == 0 ||
		cp.ConversationHistory[len(cp.ConversationHistory)-1].Role != "user" {
		return
	}
	if cerr := s.saveChatResponse(ctx, k.Namespace, session, output, ""); cerr != nil {
		slog.Warn("saving a widget answer", "key", k.ID, "run", runName, "err", cerr.msg)
	}
}
