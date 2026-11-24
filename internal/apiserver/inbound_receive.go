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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/security"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// LabelInboundKind marks a run an inbound channel started.
const LabelInboundKind = "agentorca.io/inbound"

// InboundBridge connects the channel core to the cluster: it starts runs, reads
// them and reads connection Secrets, with the checks the webhook trigger makes.
type InboundBridge struct{ s *UIServer }

// InboundBridge returns the bridge for this server.
func (s *UIServer) InboundBridge() *InboundBridge { return &InboundBridge{s: s} }

// Launch starts a run of the connection's agent. As a webhook trigger's run it is
// created by an actor, never by a person: created-by is TriggerActorPrefix plus
// the connection id, so an invoker-mode MCP server has no user to act for and
// fails closed (run_invoker.go). What the sender is told or can do is limited to
// what the agent itself does.
func (b *InboundBridge) Launch(ctx context.Context, r inbound.LaunchRequest) (inbound.RunRef, error) {
	s := b.s
	gone := func(msg string) (inbound.RunRef, error) {
		return inbound.RunRef{}, &inbound.LaunchError{Gone: true, Message: msg}
	}
	identity, ok := s.tenantForHook(r.TenantID)
	if !ok {
		return gone("the tenant is not active")
	}
	ctx = contextWithTenant(ctx, identity)
	var agent agentorcav1alpha1.Agent
	if err := s.crdClient.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Agent}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return gone("the agent is gone")
		}
		return inbound.RunRef{}, err
	}
	if !canRead(ctx, &agent, kindAgent) || !namespaceAllowed(ctx, agent.Namespace) || !tenantCanAccessAgent(identity, agent.Name) ||
		agent.Labels[LabelTemplate] == templateMarker {
		return gone("the agent is not available to the tenant")
	}
	published, err := s.agentPublished(ctx, agent.Namespace, agent.Name)
	if err != nil {
		return inbound.RunRef{}, err
	}
	if !published {
		return gone("the agent is not published")
	}
	if qerr, ok := enforceQuotas(ctx, s.crdClient, s.externalAuth.RateLimiter(), identity).(*QuotaError); ok && qerr != nil {
		return inbound.RunRef{}, &inbound.LaunchError{Busy: true, Message: qerr.Message}
	}

	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("inbound-%s-", agent.Name),
			Namespace:    agent.Namespace,
			Labels: map[string]string{
				security.LabelManagedBy: security.ManagedByValue,
				"agentorca.io/tenant":   identity.TenantName,
				LabelTriggerID:          r.ConnectionID,
				LabelInboundKind:        r.Kind,
			},
			Annotations: map[string]string{AnnotationCreatedBy: TriggerActorPrefix + r.ConnectionID},
		},
		Spec: agentorcav1alpha1.AgentRunSpec{AgentRef: agent.Name, Input: r.Input, PriorRunRef: b.continuable(ctx, r, identity)},
	}
	tenancy.Stamp(run, r.TenantID)
	if err := s.crdClient.Create(ctx, run); err != nil {
		return inbound.RunRef{}, err
	}
	slog.Info("inbound run created", "connection", r.ConnectionID, "kind", r.Kind, "run", run.Name, "agent", agent.Name, "tenant", identity.TenantName)
	return inbound.RunRef{Namespace: run.Namespace, Name: run.Name}, nil
}

// continuable is the prior run to continue from: only a finished run, of this
// connection, of the same agent and tenant. Anything else starts fresh.
func (b *InboundBridge) continuable(ctx context.Context, r inbound.LaunchRequest, id *TenantIdentity) string {
	if r.PriorRun == "" {
		return ""
	}
	var prior agentorcav1alpha1.AgentRun
	if err := b.s.crdClient.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.PriorRun}, &prior); err != nil {
		return ""
	}
	if prior.Spec.AgentRef != r.Agent || prior.Labels[LabelTriggerID] != r.ConnectionID || prior.Labels[tenancy.LabelTenantID] != id.TenantID ||
		prior.Status.Phase != agentorcav1alpha1.AgentRunPhaseSucceeded {
		return ""
	}
	return prior.Name
}

// RunState reads a run for the reply worker.
func (b *InboundBridge) RunState(ctx context.Context, ns, name string) (inbound.RunState, error) {
	var run agentorcav1alpha1.AgentRun
	if err := b.s.crdClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &run); err != nil {
		if apierrors.IsNotFound(err) {
			return inbound.RunState{}, nil
		}
		return inbound.RunState{}, err
	}
	st := inbound.RunState{Found: true}
	switch run.Status.Phase {
	case agentorcav1alpha1.AgentRunPhaseSucceeded:
		st.Done, st.Succeeded, st.Output = true, true, run.Status.Output
	case agentorcav1alpha1.AgentRunPhaseFailed:
		st.Done, st.Reason = true, "the run failed"
	case agentorcav1alpha1.AgentRunPhaseWaitingForInput:
		st.Done, st.Reason = true, "the run is waiting for input"
	}
	return st, nil
}

// ReadSecret reads the Secret of a connection. It refuses a Secret that lacks the
// label an administrator puts on a Secret made for channels.
func (b *InboundBridge) ReadSecret(ctx context.Context, ns, name string) (map[string][]byte, error) {
	var sec corev1.Secret
	if err := b.s.crdClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errors.New("the Secret of the connection was not found")
		}
		return nil, errors.New("the Secret of the connection could not be read")
	}
	if sec.Labels[inbound.SecretLabel] != inbound.SecretLabelValue {
		return nil, fmt.Errorf("the Secret is not labelled %s=%s, so channels may not use it", inbound.SecretLabel, inbound.SecretLabelValue)
	}
	return sec.Data, nil
}

const maxInboundResponseWait = 25 * time.Second

// POST /inbound/{kind}/{id}: a channel that calls us (a mail provider's inbound
// webhook, a chat platform's event). There is no session: the adapter
// authenticates the request (internal/inbound/signed.go for the signed ones).
//
// An unknown, disabled or mismatching connection is the same 404, so ids cannot
// be probed. An unauthenticated request is 401 and reaches neither the ledger
// nor the cluster. A message that was recorded is answered 202 whatever became
// of it, so the sender does not retry what policy refused; only a failure to
// record it is 503, which makes the sender retry.
func (s *UIServer) handleInboundReceive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, kind := r.PathValue("id"), r.PathValue("kind")
	store := s.inboundSource()
	if store == nil || s.inboundPipe == nil || !postgresql.ValidInboundID(id) {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	sc, err := store.GetInboundConnection(postgresql.WithPlatform(r.Context()), id)
	if errors.Is(err, postgresql.ErrInboundNotFound) || (err == nil && (!sc.Enabled || sc.Kind != kind)) {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("reading an inbound connection", "connection", id, "err", err)
		jsonError(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	a, _ := s.inboundPipe.Registry.Get(sc.Kind)
	recv, ok := a.(inbound.Receiver)
	if !ok {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxInboundResponseWait)
	defer cancel()
	c, err := s.inboundPipe.Resolve(ctx, sc)
	if err != nil {
		slog.Warn("an inbound connection is not usable", "connection", id, "err", err)
		jsonError(w, "the connection is not ready", http.StatusServiceUnavailable)
		return
	}
	m, err := recv.Receive(ctx, c, r)
	var authErr *inbound.AuthError
	var rej *inbound.Rejection
	var direct *inbound.Response
	switch {
	case errors.Is(err, inbound.ErrNotReceiving):
		jsonError(w, "not found", http.StatusNotFound)
		return
	case errors.As(err, &authErr):
		jsonError(w, authErr.Msg, http.StatusUnauthorized)
		return
	case errors.As(err, &direct):
		writeInboundResponse(w, direct)
		return
	case err != nil && !errors.As(err, &rej):
		jsonError(w, "the message could not be read", http.StatusBadRequest)
		return
	}
	var outcome inbound.Outcome
	if a.Capabilities().AckFirst {
		var ok bool
		if outcome, ok = s.handleAckFirst(ctx, c, recv, m, err); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(30))
			jsonError(w, "the message could not be recorded; send it again", http.StatusServiceUnavailable)
			return
		}
	} else {
		if enr, ok := recv.(inbound.Enricher); ok {
			enr.Enrich(ctx, c, &m)
		}
		outcome, err = s.inboundPipe.Handle(ctx, c, m, err)
		if err != nil {
			slog.Error("handling an inbound message", "connection", id, "err", err)
			w.Header().Set("Retry-After", strconv.Itoa(30))
			jsonError(w, "the message could not be recorded; send it again", http.StatusServiceUnavailable)
			return
		}
	}
	if ack, ok := recv.(inbound.Acknowledger); ok {
		if resp := ack.AckResponse(m, outcome); resp != nil {
			writeInboundResponse(w, resp)
			return
		}
	}
	status := http.StatusAccepted
	if a.Capabilities().AckFirst {
		status = http.StatusOK // platforms that retry on silence look for a plain 200
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"status":"` + strings.ToLower(string(outcome)) + `"}`))
}

func writeInboundResponse(w http.ResponseWriter, r *inbound.Response) {
	status := r.Status
	if status == 0 {
		status = http.StatusOK
	}
	if r.ContentType != "" {
		w.Header().Set("Content-Type", r.ContentType)
	}
	w.WriteHeader(status)
	_, _ = w.Write(r.Body)
}

// Ack-first handling of a message. A chat platform wants its request answered
// within about three seconds and sends it again when it is not, so the endpoint
// never holds the request on anything that can be slow (the lookups of an
// adapter, the database, the cluster, a notice to the sender). The work runs in
// the background with its own bounded context; the request waits for it only
// ackBudget, and a message the work finishes inside that time is answered with
// its true outcome. If the work fails inside the budget the platform is told to
// retry (the pipeline is idempotent on the message id). If it fails after the
// budget the failure is retried here a few times and then only logged: the
// message was acknowledged, so it is the one thing this design can lose, and
// only when the database and the cluster both stay down.
var ackBudget = 2 * time.Second

const (
	maxInboundAsync    = 64
	inboundAsyncWork   = 2 * time.Minute
	inboundAsyncTrials = 3
)

func (s *UIServer) handleAckFirst(ctx context.Context, c inbound.Connection, recv inbound.Receiver, m inbound.Message, recvErr error) (inbound.Outcome, bool) {
	s.inboundAsyncOnce.Do(func() { s.inboundAsync = make(chan struct{}, maxInboundAsync) })
	select {
	case s.inboundAsync <- struct{}{}:
	default:
		return "", false // too much in flight: let the platform retry later
	}
	type result struct {
		o   inbound.Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-s.inboundAsync }()
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), inboundAsyncWork)
		defer cancel()
		if enr, ok := recv.(inbound.Enricher); ok {
			enr.Enrich(bg, c, &m)
		}
		var res result
		for i := range inboundAsyncTrials {
			res.o, res.err = s.inboundPipe.Handle(bg, c, m, recvErr)
			if res.err == nil || bg.Err() != nil {
				break
			}
			select {
			case <-bg.Done():
			case <-time.After(time.Duration(i+1) * time.Second):
			}
		}
		if res.err != nil {
			slog.Error("handling an acknowledged inbound message", "connection", c.ID, "kind", c.Kind, "err", res.err)
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res.err != nil {
			return "", false
		}
		return res.o, true
	case <-time.After(ackBudget):
		return inbound.OutcomeAccepted, true
	}
}

// inboundAuthPublic reports whether path is a channel endpoint that carries its
// own authentication.
func inboundAuthPublic(path string) bool { return strings.HasPrefix(path, "/inbound/") }
