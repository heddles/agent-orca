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
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"bufio"
	"sort"

	authv1 "k8s.io/api/authentication/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/prometheus/client_golang/prometheus"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
	"github.com/floppyfish14/agent-orc/internal/checkpoint"
	"github.com/floppyfish14/agent-orc/internal/postgresql"
	"github.com/floppyfish14/agent-orc/internal/security"
	"github.com/floppyfish14/agent-orc/internal/state"
)

// UIServer serves the REST API consumed by the React UI.
//
// When authEnabled is true every request must carry a valid Kubernetes SA token
// with audience "agentorc/ui", validated via TokenReview. In production the
// UIProxy pod holds such a token (projected volume) and injects it on every
// proxied request. Browsers never send the token directly.
//
// When externalAuth is non-nil, OIDC tenant JWTs (validated by the external
// auth middleware) are also accepted as an alternative to the K8s SA token.
// The resolved TenantIdentity scopes list operations to the tenant's namespace,
// enabling per-tenant run history filtering and resource access control.
//
// Set --ui-auth-enabled=false for local development where the proxy is not running.
type UIServer struct {
	crdClient       client.Client
	k8s             kubernetes.Interface
	checkpoint      checkpoint.Store
	store           state.Store
	stateConfigured bool
	authEnabled     bool

	// pgStore is the optional PostgreSQL archival store for completed runs.
	// When nil, run history endpoints return 503.
	pgStore *postgresql.Store

	// externalAuth enables OIDC tenant JWT validation as an alternative to
	// K8s SA tokens. When nil, only K8s SA tokens are accepted.
	externalAuth *ExternalAuth

	// alertManager evaluates subsystem health and fires webhooks on transitions.
	// When nil, alerting is disabled (health checks still run).
	alertManager *AlertManager

	// metricHistory is a rolling ring buffer of metric samples, populated on each
	// status scrape (throttled). It backs the time-range graphs/heatmap on the
	// status page. nil means sampling is disabled (size 0).
	metricHistory []MetricSample
	metricMu      sync.Mutex
	lastSampleAt  time.Time
	// cachedRouterTokens / lastRouterScrape back the model-router :9091 token
	// scrape so we don't list pods + HTTP-scrape every status poll.
	cachedRouterTokens float64
	lastRouterScrape   time.Time
}

// nsListOpts returns a ListOption slice scoped to ns, or empty (all namespaces) when ns is "".
func nsListOpts(ns string) []client.ListOption {
	if ns == "" {
		return nil
	}
	return []client.ListOption{client.InNamespace(ns)}
}

// tenantScope returns ListOptions scoped to the authenticated tenant's namespace
// when a TenantIdentity is present in the context (OIDC flow). When no tenant
// identity is present (K8s SA BFF flow, auth disabled), it falls back to the
// URL-provided namespace param or all namespaces.
//
// This is the hook that lets the run-history view and resource CRUD endpoints
// present only the resources visible to the authenticated user — the foundation
// for OIDC-based authorization that the plan reserves room for.
func tenantScope(ctx context.Context, ns string) []client.ListOption {
	if tenant, ok := TenantFromContext(ctx); ok && tenant != nil && tenant.Namespace != "" {
		return []client.ListOption{client.InNamespace(tenant.Namespace)}
	}
	return nsListOpts(ns)
}

// checkpointStore picks a durable session-checkpoint store when a state backend
// (Redis) is configured, falling back to the in-memory store for local dev. The
// durable store is what lets a chat's LastRunRef conversation chain survive an
// API-server or warm-pod restart, so a recycled pod can resume context.
func checkpointStore(store state.Store) checkpoint.Store {
	if store != nil {
		return checkpoint.NewRedisStore(store)
	}
	return checkpoint.NewInMemoryStore()
}

// NewUIServer creates a UIServer.
// stateConfigured should be true when a Redis-backed state store is active.
// store may be nil if no state backend is configured (in-memory checkpoint fallback).
// authEnabled requires a valid UIProxy SA token (audience agentorc/ui) on all API calls.
// pgStore may be nil to disable run archival/history.
// externalAuth may be nil to disable OIDC tenant JWT auth (K8s SA only).
// alertManager may be nil to disable alerting.
func NewUIServer(
	k8s kubernetes.Interface,
	crdClient client.Client,
	stateConfigured bool,
	store state.Store,
	authEnabled bool,
	externalAuth *ExternalAuth,
	pgStore *postgresql.Store,
	alertManager *AlertManager,
) *UIServer {
	return &UIServer{
		crdClient:       crdClient,
		k8s:             k8s,
		checkpoint:      checkpointStore(store),
		store:           store,
		stateConfigured: stateConfigured,
		authEnabled:     authEnabled,
		externalAuth:    externalAuth,
		pgStore:         pgStore,
		alertManager:    alertManager,
	}
}

// Handler returns the http.Handler for the UI API.
func (s *UIServer) Handler() http.Handler {
	mux := http.NewServeMux()
	// Liveness/readiness/version are public probes (do not require a UI session token).
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/readyz", readyzHandlerBuilder(k8sReady(s.k8s), s.store != nil, s.store))
	mux.HandleFunc("/version", versionHandler)
	mux.HandleFunc("/api/runs", s.handleListRuns)
	mux.HandleFunc("/api/runs/", s.handleRunOrStream)
	mux.HandleFunc("/api/agents", s.handleAgents)
	mux.HandleFunc("/api/modelselectors", s.handleListModelSelectors)
	mux.HandleFunc("/api/costs", s.handleCosts)
	mux.HandleFunc("/api/deployments", s.handleListDeployments)
	mux.HandleFunc("/api/deployments/", s.handleDeployment)
	mux.HandleFunc("/api/workflows", s.handleListWorkflows)
	mux.HandleFunc("/api/workflows/", s.handleGetWorkflow)
	mux.HandleFunc("/api/tools", s.handleListTools)
	mux.HandleFunc("/api/mcpservers", s.handleListMCPServers)
	mux.HandleFunc("/api/modelproviders", s.handleListModelProviders)
	mux.HandleFunc("/api/knowledgebases", s.handleListKnowledgeBases)
	mux.HandleFunc("/api/system/status", s.handleSystemStatus)
	mux.HandleFunc("/api/system/alerts", s.handleAlerts)
	mux.HandleFunc("/api/runs/history", s.handleListRunHistory)
	mux.HandleFunc("/api/resources", s.handleResourceCollection)
	mux.HandleFunc("/api/resources/", s.handleResource)
	// OpenAPI contract for the UI API.
	mux.HandleFunc("/api/openapi.json", s.handleUIOpenAPI)
	// Instrument the UI server (8083) so browser-driven traffic populates the
	// shared externalReg request/latency counters that the status page reads.
	// Previously only the external Task API (8084) was instrumented, so the UI
	// server's own traffic never incremented external_requests_total / the
	// request-duration histogram — the status page always showed zeros.
	return corsMiddleware(instrument("ui", s.requireAuth(mux)))
}

// requireAuth wraps next with Kubernetes TokenReview authentication.
// When authEnabled is false it is a no-op (local development).
// The token may be provided as an Authorization: Bearer header or as a
// ?token= query parameter (required for SSE, where EventSource cannot set headers).
//
// Authentication is tried in order:
//  1. K8s SA TokenReview with audience "agentorc/ui" (UIProxy BFF flow).
//  2. OIDC tenant JWT via externalAuth (when configured) — injects TenantIdentity
//     into the request context so downstream handlers can scope queries.
func (s *UIServer) requireAuth(next http.Handler) http.Handler {
	if !s.authEnabled {
		return next
	}
	// Public paths that must remain reachable without a session token.
	uiPublicPaths := map[string]bool{"/healthz": true, "/readyz": true, "/version": true}
	// Public API paths that the UI may call before a token is available (e.g. token exchange).
	uiPublicPaths["/api/system/status"] = true
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if uiPublicPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		token := extractBearer(r)
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if token == "" {
			writeUIAuthFailure(w, true, nil)
			return
		}
		// 1. Try K8s SA TokenReview (UIProxy BFF flow).
		if _, err := s.validateUIToken(r.Context(), token); err == nil {
			next.ServeHTTP(w, r)
			return
		}
		// 2. Fall back to OIDC tenant JWT (direct OIDC login flow).
		if s.externalAuth != nil {
			if identity, err := s.externalAuth.ValidateToken(r.Context(), token); err == nil && identity != nil {
				ctx := context.WithValue(r.Context(), tenantIdentityKey, identity)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		slog.Warn("UI auth rejected", "path", r.URL.Path)
		writeUIAuthFailure(w, false, fmt.Errorf("invalid or expired token"))
	})
}

// validateUIToken performs a Kubernetes TokenReview scoped to the UIProxy SA audience.
func (s *UIServer) validateUIToken(ctx context.Context, token string) (string, error) {
	tr := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{security.UITokenAudience},
		},
	}
	result, err := s.k8s.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("TokenReview failed: %w", err)
	}
	if !result.Status.Authenticated {
		return "", fmt.Errorf("token not authenticated")
	}
	return result.Status.User.Username, nil
}

// handleListRuns lists AgentRuns in the requested namespace (all namespaces when omitted).
func (s *UIServer) handleListRuns(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	opts := tenantScope(r.Context(), ns)
	if dep := r.URL.Query().Get("deployment"); dep != "" {
		opts = append(opts, client.MatchingLabels{"agentorc.io/deployment": dep})
	}
	var list agentorcv1alpha1.AgentRunList
	if err := s.crdClient.List(r.Context(), &list, opts...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type summary struct {
		Name           string `json:"name"`
		Namespace      string `json:"namespace"`
		AgentRef       string `json:"agentRef"`
		Phase          string `json:"phase"`
		SpendUSD       string `json:"spendUSD"`
		RestartCount   int    `json:"restartCount"`
		StartTime      string `json:"startTime,omitempty"`
		CompletionTime string `json:"completionTime,omitempty"`
	}
	out := make([]summary, 0, len(list.Items))
	for _, run := range list.Items {
		s := summary{
			Name:         run.Name,
			Namespace:    run.Namespace,
			AgentRef:     run.Spec.AgentRef,
			Phase:        string(run.Status.Phase),
			SpendUSD:     run.Status.SpendUSD,
			RestartCount: run.Status.RestartCount,
		}
		if run.Status.StartTime != nil {
			s.StartTime = run.Status.StartTime.UTC().Format(time.RFC3339)
		}
		if run.Status.CompletionTime != nil {
			s.CompletionTime = run.Status.CompletionTime.UTC().Format(time.RFC3339)
		}
		out = append(out, s)
	}
	jsonResponse(w, out)
}

// handleRunOrStream dispatches /api/runs/{id}/stream (SSE) vs /api/runs/{id} (status).
// handleRunOrStream dispatches /api/runs/{namespace}/{name}[/stream|stop|answer].
func (s *UIServer) handleRunOrStream(w http.ResponseWriter, r *http.Request) {
	// Path: /api/runs/{namespace}/{name}[/action]
	path := strings.TrimPrefix(r.URL.Path, "/api/runs/")
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 {
		http.Error(w, "invalid path: expected /api/runs/{namespace}/{name}", http.StatusBadRequest)
		return
	}

	// Archived run detail: /api/runs/history/{namespace}/{name}
	// Intercepted before the live-CRD path so archived-only runs (pruned from
	// Kubernetes but retained in PostgreSQL) are still viewable.
	if parts[0] == "history" && len(parts) == 3 {
		s.handleGetArchivedRun(w, r, parts[1], parts[2])
		return
	}

	ns, runName := parts[0], parts[1]

	if len(parts) == 3 {
		if after, ok := strings.CutPrefix(parts[2], "mcpapp/"); ok {
			s.handleMCPApp(w, r, after)
			return
		}
		switch parts[2] {
		case "stream":
			s.handleStream(w, r, ns, runName)
		case "stop":
			s.handleStopRun(w, r, ns, runName)
		case "answer":
			s.handleAnswerRun(w, r, ns, runName)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
		return
	}

	// Single run detail.
	var run agentorcv1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: runName, Namespace: ns}, &run); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	type runDetail struct {
		Name               string                `json:"name"`
		Namespace          string                `json:"namespace"`
		AgentRef           string                `json:"agentRef"`
		Input              string                `json:"input"`
		Phase              string                `json:"phase"`
		Output             string                `json:"output,omitempty"`
		SpendUSD           string                `json:"spendUSD"`
		RestartCount       int                   `json:"restartCount"`
		LastRestartReason  string                `json:"lastRestartReason,omitempty"`
		StartTime          string                `json:"startTime,omitempty"`
		CompletionTime     string                `json:"completionTime,omitempty"`
		RoutingDecisions   []routingDecisionJSON `json:"routingDecisions"`
		ChildRunRefs       []string              `json:"childRunRefs,omitempty"`
		ParentRunRef       string                `json:"parentRunRef,omitempty"`
		ClarifyQuestion    string                `json:"clarifyQuestion,omitempty"`
		ClarifyAnswer      string                `json:"clarifyAnswer,omitempty"`
		WaitingSince       string                `json:"waitingSince,omitempty"`
		ContinuationRunRef string                `json:"continuationRunRef,omitempty"`
		ContextUsedTokens  int                   `json:"contextUsedTokens"`
		MaxContextTokens   int                   `json:"maxContextTokens"`
	}

	detail := runDetail{
		Name:               run.Name,
		Namespace:          run.Namespace,
		AgentRef:           run.Spec.AgentRef,
		Input:              run.Spec.Input,
		Phase:              string(run.Status.Phase),
		Output:             run.Status.Output,
		SpendUSD:           run.Status.SpendUSD,
		RestartCount:       run.Status.RestartCount,
		LastRestartReason:  run.Status.LastRestartReason,
		RoutingDecisions:   make([]routingDecisionJSON, 0, len(run.Status.RoutingDecisions)),
		ChildRunRefs:       run.Status.ChildRunRefs,
		ParentRunRef:       run.Spec.ParentRunRef,
		ClarifyQuestion:    run.Status.ClarifyQuestion,
		ClarifyAnswer:      run.Status.ClarifyAnswer,
		ContinuationRunRef: run.Status.ContinuationRunRef,
		ContextUsedTokens:  run.Status.ContextUsedTokens,
		MaxContextTokens:   run.Status.MaxContextTokens,
	}
	if run.Status.WaitingSince != nil {
		detail.WaitingSince = run.Status.WaitingSince.UTC().Format(time.RFC3339)
	}
	if run.Status.StartTime != nil {
		detail.StartTime = run.Status.StartTime.UTC().Format(time.RFC3339)
	}
	if run.Status.CompletionTime != nil {
		detail.CompletionTime = run.Status.CompletionTime.UTC().Format(time.RFC3339)
	}
	for _, rd := range run.Status.RoutingDecisions {
		d := routingDecisionJSON{
			Model:      rd.Model,
			Provider:   rd.Provider,
			Strategy:   rd.Strategy,
			Reason:     rd.Reason,
			Confidence: rd.Confidence,
		}
		if rd.Timestamp != nil {
			d.Timestamp = rd.Timestamp.UTC().Format(time.RFC3339)
		}
		detail.RoutingDecisions = append(detail.RoutingDecisions, d)
	}

	jsonResponse(w, detail)
}

// handleStream sends a Server-Sent Events stream for a run.
//
// The stream has three phases:
//  1. Wait for the pod to be created (poll AgentRun status).
//  2. Tail pod logs in real-time, emitting each line as an SSE token event.
//  3. After the log stream closes, emit routing decisions and final_output/error.
//
// If the run is already terminal on connect, all data is emitted immediately.
func (s *UIServer) handleStream(w http.ResponseWriter, r *http.Request, ns, runName string) { //nolint:gocyclo

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ctx := r.Context()

	// emitRoutingDecisions sends any new routing decisions since the last call.
	// Pre-populated candidate entries (reason starts with "configured provider") are
	// skipped — only the actual runtime selection chosen by the router is emitted.
	var emittedRouting int
	emitRoutingDecisions := func(run *agentorcv1alpha1.AgentRun) {
		for i := emittedRouting; i < len(run.Status.RoutingDecisions); i++ {
			rd := run.Status.RoutingDecisions[i]
			if strings.HasPrefix(rd.Reason, "configured provider") {
				continue
			}
			conf := 0.0
			_, _ = fmt.Sscanf(rd.Confidence, "%f", &conf)
			writeSSE(w, map[string]any{
				"type":       "modelSelected",
				"model":      rd.Model,
				"reason":     fmt.Sprintf("[%s] %s — %s", rd.Strategy, rd.Provider, rd.Reason),
				"confidence": conf,
			})
		}
		emittedRouting = len(run.Status.RoutingDecisions)
	}

	// emitTerminal sends the final SSE event for a completed run. Returns true if terminal.
	emitTerminal := func(run *agentorcv1alpha1.AgentRun) bool {
		phase := string(run.Status.Phase)
		if phase == "Succeeded" {
			writeSSE(w, map[string]any{
				"type":   "finalOutput",
				"output": run.Status.Output,
			})
			flusher.Flush()
			return true
		}
		if phase == "Failed" {
			writeSSE(w, map[string]any{
				"type":    "error",
				"message": run.Status.LastRestartReason,
			})
			flusher.Flush()
			return true
		}
		if phase == "WaitingForInput" && run.Status.ClarifyQuestion != "" {
			writeSSE(w, map[string]any{
				"type":     "clarify",
				"question": run.Status.ClarifyQuestion,
			})
			flusher.Flush()
			return true
		}
		return false
	}

	// ── Phase 1: Open the Redis token stream immediately. ──
	// With Redis Streams we know the key from the run name alone — no need to
	// wait for the pod to be assigned. TailTokens blocks via XREAD until the
	// sidecar starts writing, replays history for refreshed subscribers, and
	// exits cleanly when it receives the done sentinel.
	//
	// Routing decisions are polled concurrently so they appear in the UI as
	// the controller reconciles them, without blocking token delivery.

	var output strings.Builder

	if s.store == nil {
		slog.Warn("token streaming disabled: state store is nil (STATE_BACKEND not set or Redis connection failed)", "run", runName)
	} else {
		streamKey := fmt.Sprintf("tokens:%s:%s", ns, runName)
		slog.Info("opening Redis token stream", "key", streamKey, "storeType", fmt.Sprintf("%T", s.store))

		// Use a cancellable child context so we can abort TailTokens as soon as
		// the AgentRun reaches a terminal state (Succeeded/Failed/WaitingForInput).
		// Without this, TailTokens blocks for up to 10 minutes when the model-router
		// uses stream:false and never writes tokens or a done sentinel to Redis.
		streamCtx, cancelStream := context.WithCancel(ctx)
		defer cancelStream()

		// Poll AgentRun status concurrently. Cancel the stream context the moment
		// a terminal state is detected.
		terminalRun := make(chan agentorcv1alpha1.AgentRun, 1)
		go func() {
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-streamCtx.Done():
					return
				case <-ticker.C:
					var run agentorcv1alpha1.AgentRun
					if err := s.crdClient.Get(streamCtx, client.ObjectKey{Name: runName, Namespace: ns}, &run); err != nil {
						continue
					}
					phase := run.Status.Phase
					if phase == agentorcv1alpha1.AgentRunPhaseSucceeded ||
						phase == agentorcv1alpha1.AgentRunPhaseFailed ||
						phase == agentorcv1alpha1.AgentRunPhaseWaitingForInput {
						select {
						case terminalRun <- run:
						default:
						}
						cancelStream()
						return
					}
				}
			}
		}()

		tokenCh, streamErr := s.store.TailTokens(streamCtx, streamKey)
		if streamErr != nil {
			slog.Warn("failed to open token stream", "key", streamKey, "err", streamErr)
		} else {
			tokenCount := 0
			for token := range tokenCh {
				tokenCount++
				if tokenCount == 1 {
					slog.Info("first token from Redis stream", "key", streamKey)
				}
				// Trace events are prefixed with \x00 followed by JSON.
				if len(token) > 1 && token[0] == '\x00' {
					var evt map[string]any
					if json.Unmarshal([]byte(token[1:]), &evt) == nil {
						writeSSE(w, evt)
						flusher.Flush()
						continue
					}
				}
				output.WriteString(token)
				writeSSE(w, map[string]any{
					"type":    "token",
					"content": token,
				})
				flusher.Flush()
			}
			slog.Info("Redis token stream closed", "key", streamKey, "tokens", tokenCount)
		}

		// If the poller detected a terminal state and we captured no token output,
		// emit the terminal event directly from the polled run — skip the fallback loop.
		if output.Len() == 0 {
			select {
			case run := <-terminalRun:
				emitRoutingDecisions(&run)
				emitTerminal(&run)
				return
			default:
			}
		}
	}

	// ── Phase 3: Emit final result. ──
	// If we captured output from the log stream, emit it directly as
	// final_output — no need to wait for the controller to reconcile.

	if output.Len() > 0 {
		// Wait briefly for the controller to reconcile — it may set WaitingForInput
		// if the output looks like a clarifying question (safety net in handlePodSuccess).
		// Without this delay, we'd emit final_output before the controller has a chance to
		// intercept and redirect to clarification.
		var run agentorcv1alpha1.AgentRun
		for range 6 {
			if err := s.crdClient.Get(ctx, client.ObjectKey{Name: runName, Namespace: ns}, &run); err == nil {
				if run.Status.Phase == agentorcv1alpha1.AgentRunPhaseWaitingForInput ||
					run.Status.Phase == agentorcv1alpha1.AgentRunPhaseSucceeded ||
					run.Status.Phase == agentorcv1alpha1.AgentRunPhaseFailed {
					break
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}

		emitRoutingDecisions(&run)

		// If the controller redirected to WaitingForInput, emit clarify instead of final_output.
		if run.Status.Phase == agentorcv1alpha1.AgentRunPhaseWaitingForInput && run.Status.ClarifyQuestion != "" {
			writeSSE(w, map[string]any{
				"type":     "clarify",
				"question": run.Status.ClarifyQuestion,
			})
			flusher.Flush()
			return
		}

		writeSSE(w, map[string]any{
			"type":   "finalOutput",
			"output": output.String(),
		})
		flusher.Flush()
		return
	}

	// No token output captured (store nil or stream failed). Fall back to
	// polling the AgentRun status until the controller updates it.
	fallbackTicker := time.NewTicker(500 * time.Millisecond)
	defer fallbackTicker.Stop()
	for range 60 {
		var run agentorcv1alpha1.AgentRun
		if err := s.crdClient.Get(ctx, client.ObjectKey{Name: runName, Namespace: ns}, &run); err != nil {
			writeSSE(w, map[string]any{"type": "error", "message": err.Error()})
			flusher.Flush()
			return
		}
		emitRoutingDecisions(&run)
		if emitTerminal(&run) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-fallbackTicker.C:
		}
	}

	writeSSE(w, map[string]any{
		"type":    "error",
		"message": "timed out waiting for run to complete",
	})
	flusher.Flush()
}

// handleStopRun cancels a Pending or Running AgentRun by marking it Failed.
// POST /api/runs/{id}/stop?namespace=default
func (s *UIServer) handleStopRun(w http.ResponseWriter, r *http.Request, ns, runName string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var run agentorcv1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: runName, Namespace: ns}, &run); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if run.Status.Phase != agentorcv1alpha1.AgentRunPhasePending &&
		run.Status.Phase != agentorcv1alpha1.AgentRunPhaseRunning &&
		run.Status.Phase != agentorcv1alpha1.AgentRunPhaseWaitingForInput {
		http.Error(w, "run is not in a cancellable state", http.StatusConflict)
		return
	}
	now := metav1.Now()
	run.Status.Phase = agentorcv1alpha1.AgentRunPhaseFailed
	run.Status.LastRestartReason = "cancelled by user"
	run.Status.CompletionTime = &now
	if err := s.crdClient.Status().Update(r.Context(), &run); err != nil {
		http.Error(w, fmt.Sprintf("cancelling run: %v", err), http.StatusInternalServerError)
		return
	}
	// Signal cancellation via Redis so the model-router sidecar aborts in-flight
	// LLM requests within ~1 second, rather than waiting for the next call.
	if s.store != nil {
		_ = s.store.SignalCancel(r.Context(), ns, runName)
	}
	jsonResponse(w, map[string]string{"status": "cancelled"})
}

// handleAnswerRun accepts a human's clarification answer for a WaitingForInput run.
// Instead of mutating the original run (which would require pod spec changes and
// token secret rotation), it creates a brand-new continuation AgentRun that loads
// the original run's checkpoint via PriorRunRef. The original run is kept as-is
// for auditing; only its status is annotated with the answer and continuation ref.
//
// POST /api/runs/{id}/answer?namespace=default
func (s *UIServer) handleAnswerRun(w http.ResponseWriter, r *http.Request, ns, runName string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Answer string `json:"answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Answer == "" {
		http.Error(w, "answer is required", http.StatusBadRequest)
		return
	}

	var run agentorcv1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: runName, Namespace: ns}, &run); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if run.Status.Phase != agentorcv1alpha1.AgentRunPhaseWaitingForInput {
		http.Error(w, "run is not waiting for input", http.StatusConflict)
		return
	}

	// Build the continuation run's input. If the model-router checkpointed the
	// conversation (including the _clarify tool call), the new model-router will
	// load it via PriorRunRef and inject the answer as a tool result. If no
	// checkpoint exists (controller safety net path), augment the input with the
	// full Q&A so the new run has context.
	continuationInput := run.Spec.Input
	hasCheckpoint := false
	if s.store != nil {
		checkpointKey := fmt.Sprintf("agentorc/runs/%s/state", runName)
		if msgs, err := s.store.LoadMessages(r.Context(), checkpointKey); err == nil && len(msgs) > 0 {
			hasCheckpoint = true
		}
	}
	if !hasCheckpoint {
		continuationInput = fmt.Sprintf(
			"%s\n\n---\nPrevious attempt asked: %s\nHuman answered: %s\n---\nPlease proceed with the above information.",
			run.Spec.Input, run.Status.ClarifyQuestion, req.Answer)
	}

	// Create a continuation AgentRun that picks up from the original run's checkpoint.
	continuation := &agentorcv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: runName + "-cont-",
			Namespace:    ns,
			Labels:       run.Labels, // preserve deployment, session, source labels
		},
		Spec: agentorcv1alpha1.AgentRunSpec{
			AgentRef:    run.Spec.AgentRef,
			Input:       continuationInput,
			Timeout:     run.Spec.Timeout,
			PriorRunRef: runName, // model-router loads this run's checkpoint
		},
	}
	if err := s.crdClient.Create(r.Context(), continuation); err != nil {
		http.Error(w, fmt.Sprintf("creating continuation run: %v", err), http.StatusInternalServerError)
		return
	}

	// Store the answer in Redis under the continuation run's name so its
	// model-router finds it during startup and injects it as a tool result.
	if s.store != nil {
		answerKey := fmt.Sprintf("clarify-answer:%s:%s", ns, continuation.Name)
		if err := s.store.SaveAnswer(r.Context(), answerKey, req.Answer, 1*time.Hour); err != nil {
			slog.Warn("saving clarify answer to state store", "err", err)
		}
	}

	// Transition the original run out of WaitingForInput so it won't be
	// rediscovered on page reload. Mark it Succeeded — the continuation run
	// carries the conversation forward.
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.Phase = agentorcv1alpha1.AgentRunPhaseSucceeded
	run.Status.ClarifyAnswer = req.Answer
	run.Status.ContinuationRunRef = continuation.Name
	if err := s.crdClient.Status().Patch(r.Context(), &run, patch); err != nil {
		slog.Warn("patching original run with continuation ref", "err", err)
	}

	// Update the session checkpoint: advance LastRunRef to the continuation run
	// and append the clarify Q&A so reloadHistory returns the complete history.
	if sessionID := run.Labels["agentorc.io/session"]; sessionID != "" && s.checkpoint != nil {
		if cp, err := s.checkpoint.Load(r.Context(), sessionID); err == nil && cp != nil {
			cp.LastRunRef = continuation.Name
			cp.ConversationHistory = append(cp.ConversationHistory,
				agentorcv1alpha1.ConversationMessage{Role: "assistant", Content: run.Status.ClarifyQuestion},
				agentorcv1alpha1.ConversationMessage{Role: "user", Content: req.Answer},
			)
			cp.Version++
			if _, err := s.checkpoint.Save(r.Context(), cp); err != nil {
				slog.Warn("failed to update checkpoint for continuation", "session", sessionID, "err", err)
			}
		}
	}

	slog.Info("clarify answer submitted",
		"originalRun", runName,
		"continuationRun", continuation.Name,
		"ns", ns)
	jsonResponse(w, map[string]any{
		"status":  "ok",
		"runName": continuation.Name,
	})
}

// handleAgents routes GET (list) and POST (create) for Agent CRDs.
func (s *UIServer) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListAgents(w, r)
	case http.MethodPost:
		s.handleCreateAgent(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleListAgents lists Agent CRDs (all namespaces when namespace param is omitted).
func (s *UIServer) handleListAgents(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	var list agentorcv1alpha1.AgentList
	if err := s.crdClient.List(r.Context(), &list, tenantScope(r.Context(), ns)...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type summary struct {
		Name               string   `json:"name"`
		Namespace          string   `json:"namespace"`
		ModelSelectorRef   string   `json:"modelSelectorRef"`
		Framework          string   `json:"framework"`
		Tools              []string `json:"tools"`
		SystemPrompt       string   `json:"systemPrompt,omitempty"`
		ServiceAccountName string   `json:"serviceAccountName"`
	}
	out := make([]summary, 0, len(list.Items))
	for _, a := range list.Items {
		tools := a.Spec.Tools
		if tools == nil {
			tools = []string{}
		}
		out = append(out, summary{
			Name:               a.Name,
			Namespace:          a.Namespace,
			ModelSelectorRef:   a.Spec.ModelSelectorRef,
			Framework:          a.Spec.Runtime.Framework,
			Tools:              tools,
			SystemPrompt:       a.Spec.SystemPrompt,
			ServiceAccountName: a.Status.ServiceAccountName,
		})
	}
	jsonResponse(w, out)
}

// handleCreateAgent creates a new Agent CRD.
func (s *UIServer) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name             string   `json:"name"`
		Namespace        string   `json:"namespace"`
		ModelSelectorRef string   `json:"modelSelectorRef"`
		SystemPrompt     string   `json:"systemPrompt"`
		OCIRef           string   `json:"ociRef"`
		Framework        string   `json:"framework"`
		Command          []string `json:"command"`
		Args             []string `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.ModelSelectorRef == "" || req.OCIRef == "" {
		http.Error(w, "name, modelSelectorRef, and ociRef are required", http.StatusBadRequest)
		return
	}
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	if req.Framework == "" {
		req.Framework = "openai-compatible"
	}

	agent := &agentorcv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
		},
		Spec: agentorcv1alpha1.AgentSpec{
			ModelSelectorRef: req.ModelSelectorRef,
			SystemPrompt:     req.SystemPrompt,
			Runtime: agentorcv1alpha1.AgentRuntime{
				OCIRef:    req.OCIRef,
				Framework: req.Framework,
				Command:   req.Command,
				Args:      req.Args,
			},
		},
	}
	if err := s.crdClient.Create(r.Context(), agent); err != nil {
		http.Error(w, fmt.Sprintf("creating agent: %v", err), http.StatusInternalServerError)
		return
	}

	// Auto-create an AgentDeployment referencing the new agent.
	replicas := int32(1)
	deployment := &agentorcv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agent.Name,
			Namespace: agent.Namespace,
		},
		Spec: agentorcv1alpha1.AgentDeploymentSpec{
			AgentRef: agent.Name,
			InputSource: &agentorcv1alpha1.InputSourceConfig{
				Type: agentorcv1alpha1.InputSourceChat,
			},
			Replicas: &replicas,
		},
	}
	if err := s.crdClient.Create(r.Context(), deployment); err != nil {
		// Agent was created successfully but deployment failed — report but don't fail the whole request.
		w.WriteHeader(http.StatusCreated)
		jsonResponse(w, map[string]any{
			"name":            agent.Name,
			"namespace":       agent.Namespace,
			"deploymentError": fmt.Sprintf("agent created but deployment failed: %v", err),
		})
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, map[string]string{"name": agent.Name, "namespace": agent.Namespace})
}

// handleListModelSelectors lists ModelSelector CRDs (all namespaces when namespace param is omitted).
// Used for both form dropdowns (create agent) and the ModelSelectors UI view.
func (s *UIServer) handleListModelSelectors(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	var list agentorcv1alpha1.ModelSelectorList
	if err := s.crdClient.List(r.Context(), &list, tenantScope(r.Context(), ns)...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type budgetSummary struct {
		PerRun string `json:"perRun,omitempty"`
		PerDay string `json:"perDay,omitempty"`
	}
	type providerEntry struct {
		Name        string `json:"name"`
		Weight      int    `json:"weight,omitempty"`
		RoutingHint string `json:"routingHint,omitempty"`
	}
	type summary struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		Strategy          string            `json:"strategy"`
		Providers         []providerEntry   `json:"providers,omitempty"`
		FallbackChain     []string          `json:"fallbackChain,omitempty"`
		CapabilityRouting map[string]string `json:"capabilityRouting,omitempty"`
		Budget            *budgetSummary    `json:"budget,omitempty"`
		ActiveProviders   []string          `json:"activeProviders,omitempty"`
	}
	out := make([]summary, 0, len(list.Items))
	for _, ms := range list.Items {
		providers := make([]providerEntry, len(ms.Spec.Providers))
		for i, pw := range ms.Spec.Providers {
			providers[i] = providerEntry{Name: pw.Name, Weight: pw.Weight, RoutingHint: pw.RoutingHint}
		}
		var budget *budgetSummary
		if ms.Spec.BudgetCap != nil {
			budget = &budgetSummary{PerRun: ms.Spec.BudgetCap.PerRun, PerDay: ms.Spec.BudgetCap.PerDay}
		}
		out = append(out, summary{
			Name:              ms.Name,
			Namespace:         ms.Namespace,
			Strategy:          ms.Spec.Strategy,
			Providers:         providers,
			FallbackChain:     ms.Spec.FallbackChain,
			CapabilityRouting: ms.Spec.CapabilityRouting,
			Budget:            budget,
			ActiveProviders:   ms.Status.ActiveProviders,
		})
	}
	jsonResponse(w, out)
}

// handleListTools lists Tool CRDs (all namespaces when namespace param is omitted).
func (s *UIServer) handleListTools(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	var list agentorcv1alpha1.ToolList
	if err := s.crdClient.List(r.Context(), &list, tenantScope(r.Context(), ns)...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type summary struct {
		Name          string `json:"name"`
		Namespace     string `json:"namespace"`
		Type          string `json:"type"`
		ExecutionMode string `json:"executionMode"`
		Description   string `json:"description,omitempty"`
		Ready         bool   `json:"ready"`
		OCIRef        string `json:"ociRef,omitempty"`
		AgentRef      string `json:"agentRef,omitempty"`
	}
	out := make([]summary, 0, len(list.Items))
	for _, t := range list.Items {
		desc := ""
		if t.Spec.Schema != nil {
			desc = t.Spec.Schema.Description
		}
		out = append(out, summary{
			Name:          t.Name,
			Namespace:     t.Namespace,
			Type:          string(t.Spec.Type),
			ExecutionMode: string(t.Spec.ExecutionMode),
			Description:   desc,
			Ready:         t.Status.Ready,
			OCIRef:        t.Spec.OCIRef,
			AgentRef:      t.Spec.AgentRef,
		})
	}
	jsonResponse(w, out)
}

// handleMCPApp serves cached MCP App HTML for a given mcpServer/toolName key.
// The key is the remainder of the path after "/mcpapp/", i.e. "{mcpServer}/{toolName}".
func (s *UIServer) handleMCPApp(w http.ResponseWriter, r *http.Request, key string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := s.store.LoadKV(r.Context(), "mcpapp", key)
	if err != nil || data == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	nonce, err := randomCSPNonce()
	if err != nil {
		slog.Error("mcpapp CSP nonce", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	htmlOut, err := injectMCPAppCSPNonces(data, nonce)
	if err != nil {
		slog.Error("mcpapp CSP rewrite", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", mcpAppCSPHeader(nonce))
	_, _ = w.Write(htmlOut)
}

// handleListMCPServers lists MCPServer CRDs (all namespaces when namespace param is omitted).
func (s *UIServer) handleListMCPServers(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	var list agentorcv1alpha1.MCPServerList
	if err := s.crdClient.List(r.Context(), &list, tenantScope(r.Context(), ns)...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type mcpToolEntry struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	}
	type summary struct {
		Name          string         `json:"name"`
		Namespace     string         `json:"namespace"`
		Transport     string         `json:"transport"`
		URL           string         `json:"url,omitempty"`
		OCIRef        string         `json:"ociRef,omitempty"`
		ToolCount     int            `json:"toolCount"`
		Ready         bool           `json:"ready"`
		AllowedAgents []string       `json:"allowedAgents"`
		Tools         []mcpToolEntry `json:"tools,omitempty"`
		AllowApps     bool           `json:"allowApps"`
	}
	out := make([]summary, 0, len(list.Items))
	for _, m := range list.Items {
		allowed := m.Spec.AllowedAgents
		if allowed == nil {
			allowed = []string{}
		}
		tools := make([]mcpToolEntry, 0, len(m.Spec.Tools))
		for _, t := range m.Spec.Tools {
			entry := mcpToolEntry{
				Name:        t.Name,
				Description: t.Description,
			}
			if t.InputSchema != nil {
				entry.InputSchema = t.InputSchema.Raw
			}
			tools = append(tools, entry)
		}
		out = append(out, summary{
			Name:          m.Name,
			Namespace:     m.Namespace,
			Transport:     m.Spec.Transport,
			URL:           m.Spec.URL,
			OCIRef:        m.Spec.OCIRef,
			ToolCount:     m.Status.ToolCount,
			Ready:         m.Status.Ready,
			AllowedAgents: allowed,
			Tools:         tools,
			AllowApps:     m.Spec.AllowApps,
		})
	}
	jsonResponse(w, out)
}

// handleListKnowledgeBases lists KnowledgeBase CRDs in the requested namespace.
func (s *UIServer) handleListKnowledgeBases(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	var list agentorcv1alpha1.KnowledgeBaseList
	if err := s.crdClient.List(r.Context(), &list, tenantScope(r.Context(), ns)...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type summary struct {
		Name               string                  `json:"name"`
		Namespace          string                  `json:"namespace"`
		Description        string                  `json:"description,omitempty"`
		AllowedAgents      []string                `json:"allowedAgents"`
		Ready              bool                    `json:"ready"`
		Message            string                  `json:"message,omitempty"`
		Conditions         []metav1.Condition      `json:"conditions,omitempty"`
		DocumentCount      int                     `json:"documentCount"`
		ChunkCount         int                     `json:"chunkCount"`
		VectorStoreURL     string                  `json:"vectorStoreURL,omitempty"`
		CollectionName     string                  `json:"collectionName,omitempty"`
		ModelSelectorRef   string                  `json:"modelSelectorRef"`
		Dimensions         int                     `json:"dimensions"`
		ChunkSize          int                     `json:"chunkSize"`
		ChunkOverlap       int                     `json:"chunkOverlap"`
		StorageUsedPercent int                     `json:"storageUsedPercent"`
		LastSyncTime       string                  `json:"lastSyncTime,omitempty"`
	}
	out := make([]summary, 0, len(list.Items))
	for _, kb := range list.Items {
		allowed := kb.Spec.AllowedAgents
		if allowed == nil {
			allowed = []string{}
		}
		s := summary{
			Name:               kb.Name,
			Namespace:          kb.Namespace,
			Description:        kb.Spec.Description,
			AllowedAgents:      allowed,
			Ready:              kb.Status.Ready,
			Message:            kb.Status.Message,
			Conditions:         kb.Status.Conditions,
			DocumentCount:      kb.Status.DocumentCount,
			ChunkCount:         kb.Status.ChunkCount,
			VectorStoreURL:     kb.Status.VectorStoreURL,
			CollectionName:     kb.Status.CollectionName,
			ModelSelectorRef:   kb.Spec.Embedding.ModelSelectorRef,
			Dimensions:         kb.Status.EmbeddingDimensions,
			ChunkSize:          kb.Spec.Embedding.ChunkSize,
			ChunkOverlap:       kb.Spec.Embedding.ChunkOverlap,
			StorageUsedPercent: kb.Status.StorageUsedPercent,
		}
		if kb.Status.LastSyncTime != nil {
			s.LastSyncTime = kb.Status.LastSyncTime.UTC().Format(time.RFC3339)
		}
		out = append(out, s)
	}
	jsonResponse(w, out)
}

// handleListModelProviders lists ModelProvider CRDs (all namespaces when namespace param is omitted).
func (s *UIServer) handleListModelProviders(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	var list agentorcv1alpha1.ModelProviderList
	if err := s.crdClient.List(r.Context(), &list, tenantScope(r.Context(), ns)...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type summary struct {
		Name                       string   `json:"name"`
		Namespace                  string   `json:"namespace"`
		LiteLLMModel               string   `json:"litellmModel"`
		BaseURL                    string   `json:"baseURL,omitempty"`
		LatencyProfile             string   `json:"latencyProfile"`
		Capabilities               []string `json:"capabilities"`
		CostPerMillionInputTokens  string   `json:"costPerMillionInputTokens,omitempty"`
		CostPerMillionOutputTokens string   `json:"costPerMillionOutputTokens,omitempty"`
		ContextWindow              int      `json:"contextWindow,omitempty"`
		Ready                      bool     `json:"ready"`
	}
	out := make([]summary, 0, len(list.Items))
	for _, p := range list.Items {
		out = append(out, summary{
			Name:                       p.Name,
			Namespace:                  p.Namespace,
			LiteLLMModel:               p.Spec.LiteLLMModel,
			BaseURL:                    p.Spec.BaseURL,
			LatencyProfile:             p.Spec.LatencyProfile,
			Capabilities:               p.Spec.Capabilities,
			CostPerMillionInputTokens:  p.Spec.Constraints.CostPerMillionInputTokens,
			CostPerMillionOutputTokens: p.Spec.Constraints.CostPerMillionOutputTokens,
			ContextWindow:              p.Spec.Constraints.ContextWindow,
			Ready:                      p.Status.Ready,
		})
	}
	jsonResponse(w, out)
}

// handleListDeployments lists AgentDeployments in the requested namespace.
func (s *UIServer) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	var list agentorcv1alpha1.AgentDeploymentList
	if err := s.crdClient.List(r.Context(), &list, tenantScope(r.Context(), ns)...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type summary struct {
		Name            string `json:"name"`
		Namespace       string `json:"namespace"`
		AgentRef        string `json:"agentRef"`
		Phase           string `json:"phase"`
		ReadyReplicas   int32  `json:"readyReplicas"`
		InputSourceType string `json:"inputSourceType,omitempty"`
		LastUpdateTime  string `json:"lastUpdateTime,omitempty"`
		Message         string `json:"message,omitempty"`
	}
	out := make([]summary, 0, len(list.Items))
	for _, d := range list.Items {
		s := summary{
			Name:          d.Name,
			Namespace:     d.Namespace,
			AgentRef:      d.Spec.AgentRef,
			Phase:         string(d.Status.Phase),
			ReadyReplicas: d.Status.ReadyReplicas,
			Message:       d.Status.Message,
		}
		if d.Spec.InputSource != nil {
			s.InputSourceType = string(d.Spec.InputSource.Type)
		}
		if d.Status.LastUpdateTime != nil {
			s.LastUpdateTime = d.Status.LastUpdateTime.UTC().Format(time.RFC3339)
		}
		out = append(out, s)
	}
	jsonResponse(w, out)
}

// handleCosts returns aggregated spend data from AgentRun statuses.
// Optional query params:
//   - namespace: Kubernetes namespace (default "default")
//   - run: filter to a single AgentRun by name
//   - deployment: filter to runs owned by an AgentDeployment
func (s *UIServer) handleCosts(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	opts := tenantScope(r.Context(), ns)
	if dep := r.URL.Query().Get("deployment"); dep != "" {
		opts = append(opts, client.MatchingLabels{"agentorc.io/deployment": dep})
	}

	var list agentorcv1alpha1.AgentRunList
	if err := s.crdClient.List(r.Context(), &list, opts...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// If a specific run is requested, filter to just that run.
	runFilter := r.URL.Query().Get("run")

	byAgent := make(map[string]float64)
	var total float64
	for _, run := range list.Items {
		if runFilter != "" && run.Name != runFilter {
			continue
		}
		v := parseFloatCost(run.Status.SpendUSD)
		total += v
		byAgent[run.Spec.AgentRef] += v
	}

	type costData struct {
		TotalUSD string            `json:"totalUSD"`
		ByAgent  map[string]string `json:"byAgent"`
		ByModel  map[string]string `json:"byModel"`
		ByDay    []any             `json:"byDay"`
	}
	out := costData{
		TotalUSD: fmt.Sprintf("%.4f", total),
		ByAgent:  make(map[string]string),
		ByModel:  make(map[string]string),
		ByDay:    []any{},
	}
	for k, v := range byAgent {
		out.ByAgent[k] = fmt.Sprintf("%.4f", v)
	}
	jsonResponse(w, out)
}

func writeSSE(w http.ResponseWriter, data any) {
	b, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
}

func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// SystemSubSystemStatus describes the health of a single subsystem.
type SystemSubSystemStatus struct {
	Name      string `json:"name"`
	Status    string `json:"status"` // "up", "down", "degraded"
	Message   string `json:"message,omitempty"`
	LatencyMs int    `json:"latencyMs,omitempty"`
}

// SystemStatusResponse is the full system status payload returned to the UI.
type SystemStatusResponse struct {
	StateConfigured bool                `json:"stateConfigured"`
	Version         string              `json:"version"`
	SubSystems      []SystemSubSystemStatus `json:"subsystems"`
	// ModelProviders reports per-provider reachability and latency.
	ModelProviders []ProviderHealth `json:"modelProviders,omitempty"`
	// Metrics holds aggregated Prometheus-derived metrics.
	Metrics *SystemMetrics `json:"metrics,omitempty"`
	// RunHistoryConfigured is true when the PostgreSQL archival store is wired
	// up, so the UI can show an actionable message instead of a silent empty state.
	RunHistoryConfigured bool `json:"runHistoryConfigured"`
}

// ProviderHealth is a per-model-provider reachability probe result.
type ProviderHealth struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Ready       bool   `json:"ready"`
	LatencyMs   int    `json:"latencyMs,omitempty"`
	Message     string `json:"message,omitempty"`
}

// MetricSample is a point-in-time snapshot of system metrics, recorded on a
// rolling window so the status page can render time-series graphs and a
// latency heatmap rather than only a single "current" value.
type MetricSample struct {
	// Time is the recording timestamp (unix seconds, UTC).
	Time int64 `json:"time"`
	// RequestCount / EgressPublished / EgressFailed are CUMULATIVE counters as
	// exported by the registries; the UI derives per-bucket rates.
	RequestCount    int     `json:"requestCount"`
	EgressPublished int     `json:"egressPublished"`
	EgressFailed    int     `json:"egressFailed"`
	// Latency percentiles (ms) from the external-request duration histogram.
	P50LatencyMs float64 `json:"p50LatencyMs"`
	P95LatencyMs float64 `json:"p95LatencyMs"`
	P99LatencyMs float64 `json:"p99LatencyMs"`
	// TokenThroughput is a stub (model-router scraping is deferred).
	TokenThroughput float64 `json:"tokenThroughput"`
}

// SystemMetrics holds aggregated metrics scraped from the controller and model-routers.
type SystemMetrics struct {
	RequestCount24h int     `json:"requestCount24h"`
	P50LatencyMs    float64 `json:"p50LatencyMs"`
	P95LatencyMs    float64 `json:"p95LatencyMs"`
	P99LatencyMs    float64 `json:"p99LatencyMs"`
	TokenThroughput float64 `json:"tokenThroughput"`
	EgressPublished int     `json:"egressPublished"`
	EgressFailed    int     `json:"egressFailed"`
	// Samples is the time-series history for the requested range (see ?range=).
	Samples []MetricSample `json:"samples,omitempty"`
}

// handleSystemStatus returns operator-level and subsystem health for the UI.
// Enhanced: probes K8s API, Redis, model providers, and scrapes Prometheus
// metrics from the controller and model-router pods.
func (s *UIServer) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	resp := SystemStatusResponse{
		StateConfigured:       s.stateConfigured,
		Version:               Version,
		RunHistoryConfigured:  s.pgStore != nil,
		SubSystems:            []SystemSubSystemStatus{},
	}

	// ── Kubernetes API ──
	if s.k8s != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		start := time.Now()
		_, err := s.k8s.Discovery().ServerVersion()
		latency := int(time.Since(start).Milliseconds())
		_ = ctx // ServerVersion() doesn't accept a context in this client-go version.
		if err != nil {
			resp.SubSystems = append(resp.SubSystems, SystemSubSystemStatus{
				Name: "kubernetes-api", Status: "down", Message: err.Error(),
			})
		} else {
			resp.SubSystems = append(resp.SubSystems, SystemSubSystemStatus{
				Name: "kubernetes-api", Status: "up", LatencyMs: latency,
			})
		}
	}

	// ── Redis state store ──
	if s.store != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		start := time.Now()
		err := s.store.Ping(ctx)
		latency := int(time.Since(start).Milliseconds())
		if err != nil {
			resp.SubSystems = append(resp.SubSystems, SystemSubSystemStatus{
				Name: "redis", Status: "down", Message: err.Error(),
			})
		} else {
			resp.SubSystems = append(resp.SubSystems, SystemSubSystemStatus{
				Name: "redis", Status: "up", LatencyMs: latency,
			})
		}
	}

	// ── PostgreSQL archival store ──
	// Surfaces as a named subsystem so operators see "run history not available"
	// as a first-class health entry rather than a silent UI empty state.
	if s.pgStore != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		start := time.Now()
		err := s.pgStore.Ping(ctx)
		latency := int(time.Since(start).Milliseconds())
		if err != nil {
			resp.SubSystems = append(resp.SubSystems, SystemSubSystemStatus{
				Name: "postgres-archive", Status: "down", Message: err.Error(),
			})
		} else {
			resp.SubSystems = append(resp.SubSystems, SystemSubSystemStatus{
				Name: "postgres-archive", Status: "up", LatencyMs: latency,
			})
		}
	} else {
		// Not wired at all — report as degraded (not down: the operator itself
		// is healthy, but historical run tracking is unavailable).
		resp.SubSystems = append(resp.SubSystems, SystemSubSystemStatus{
			Name: "postgres-archive", Status: "degraded",
			Message: "PostgreSQL archival store not configured; run history is unavailable. Set PG_DSN.",
		})
	}

	// ── Model provider health probes ──
	resp.ModelProviders = s.probeModelProviders(r.Context())

	// ── Prometheus metrics (controller + model-routers) ──
	if metrics, err := s.scrapeMetrics(r.Context()); err == nil {
		// Record a throttled sample for the time-range graphs, then attach the
		// samples within the requested ?range= window (default 24h).
		metrics.Samples = s.samplesForRange(r.Context(), r.URL.Query().Get("range"))
		s.recordMetricSample(metrics)
		resp.Metrics = metrics
	}

	// ── Alerting evaluation ──
	// Evaluate each subsystem through the alert manager. For subsystems that
	// are up, pass the detail; for down, pass the error message. (Alerting
	// state is still tracked for the /api/system/alerts endpoint; the status
	// page no longer displays it — that's the org's responsibility.)
	if s.alertManager != nil {
		for _, ss := range resp.SubSystems {
			up := ss.Status == "up"
			s.alertManager.Evaluate(r.Context(), ss.Name, ss.Message, up)
		}
		for _, mp := range resp.ModelProviders {
			s.alertManager.Evaluate(r.Context(), "model-provider/"+mp.Namespace+"/"+mp.Name, mp.Message, mp.Ready)
		}
	}

	jsonResponse(w, resp)
}

// probeModelProviders pings each ModelProvider's base URL (if set) to determine
// reachability and latency. Returns an empty slice if no providers are configured.
func (s *UIServer) probeModelProviders(ctx context.Context) []ProviderHealth {
	var list agentorcv1alpha1.ModelProviderList
	if err := s.crdClient.List(ctx, &list); err != nil {
		return nil
	}
	var out []ProviderHealth
	for i := range list.Items {
		mp := &list.Items[i]
		health := ProviderHealth{Name: mp.Name, Namespace: mp.Namespace, Ready: mp.Status.Ready}
		if mp.Status.Ready {
			health.Message = "reachable"
		}
		out = append(out, health)
	}
	return out
}

// scrapeMetrics collects Prometheus metrics from the controller-runtime server
// and from model-router pods. The controller serves on :8080/metrics (or :8443);
// model-router pods expose :9091/metrics.
func (s *UIServer) scrapeMetrics(ctx context.Context) (*SystemMetrics, error) {
	m := &SystemMetrics{}

	// External API server counters live in `externalReg` (a dedicated registry),
	// so we sum across all label series to get a single value (these are
	// CounterVecs with server/method/path/status labels).
	m.RequestCount24h = int(getCounterValue(externalReg, "agentorc_external_requests_total"))
	// Latency percentiles (ms) pooled across all labeled histogram series.
	m.P50LatencyMs = getHistogramQuantile(externalReg, "agentorc_external_request_duration_seconds", 0.50) * 1000
	m.P95LatencyMs = getHistogramQuantile(externalReg, "agentorc_external_request_duration_seconds", 0.95) * 1000
	m.P99LatencyMs = getHistogramQuantile(externalReg, "agentorc_external_request_duration_seconds", 0.99) * 1000

	// Egress counters are registered via promauto in the internal/egress package,
	// which lands on the DEFAULT registry. The UI API server runs in the same
	// process as the controller (cmd/main.go), so we gather them from
	// prometheus.DefaultGatherer. (Previously these were read from externalReg,
	// which never held them — so egress always showed 0.)
	m.EgressPublished = int(getCounterValue(prometheus.DefaultGatherer, "agentorc_egress_published_total"))
	m.EgressFailed = int(getCounterValue(prometheus.DefaultGatherer, "agentorc_egress_failed_total"))

	// Model-router token throughput — the model-router runs as a separate pod
	// (:9091/metrics); scraping it is a follow-up. Until then this stays 0,
	// which the UI renders honestly as "—" rather than a phantom number.
	m.TokenThroughput = s.scrapeModelRouterTokenRate(ctx)

	return m, nil
}

// scrapeModelRouterTokenRate is a stub for scraping token counts from model-router
// Prometheus endpoints. In a full implementation this would list pods with the
// model-router container and scrape /metrics for token counts.
// scrapeModelRouterTokenRate aggregates the cumulative `agentorc_modelrouter_tokens_total`
// counter across all live model-router pods (each run's model-router exposes
// :9091/metrics). It is cached for routerScrapeInterval so a 5s status poll
// doesn't list+scrape pods every time. Any error (no k8s client, pod
// unreachable, network policy blocking pod:9091) degrades gracefully to 0 — the
// UI renders token throughput as "—" with a help note instead of crashing.
func (s *UIServer) scrapeModelRouterTokenRate(ctx context.Context) float64 {
	if s.k8s == nil || s.crdClient == nil {
		return 0
	}
	s.metricMu.Lock()
	if !s.lastRouterScrape.IsZero() && time.Since(s.lastRouterScrape) < routerScrapeInterval {
		v := s.cachedRouterTokens
		s.metricMu.Unlock()
		return v
	}
	s.metricMu.Unlock()

	scrapeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var runs agentorcv1alpha1.AgentRunList
	if err := s.crdClient.List(scrapeCtx, &runs); err != nil {
		return 0
	}

	var total float64
	scraped := 0
	for i := range runs.Items {
		if scraped >= maxRouterPodsToScrape {
			break
		}
		r := &runs.Items[i]
		phase := string(r.Status.Phase)
		if phase != "Running" && phase != "Pending" && phase != "WaitingForInput" {
			continue
		}
		// Combined-pod topology: the model-router is a sidecar in the agent pod
		// (scrape PodName). Split-pod topology: a dedicated router pod
		// (scrape RouterPodName).
		podName := r.Status.RouterPodName
		if podName == "" {
			podName = r.Status.PodName
		}
		if podName == "" {
			continue
		}
		pod, err := s.k8s.CoreV1().Pods(r.Namespace).Get(scrapeCtx, podName, metav1.GetOptions{})
		if err != nil || pod.Status.PodIP == "" {
			continue
		}
		if v, ok := scrapePromCounter(scrapeCtx, "http://"+pod.Status.PodIP+":9091/metrics", "agentorc_modelrouter_tokens_total"); ok && v > 0 {
			total += v
			scraped++
		}
	}

	s.metricMu.Lock()
	s.cachedRouterTokens = total
	s.lastRouterScrape = time.Now()
	s.metricMu.Unlock()
	return total
}

// scrapePromCounter HTTP-scrapes a Prometheus/OpenMetrics text endpoint and
// returns the sum of all series of the named counter. ok is false if the scrape
// fails or the counter is absent. Uses a small line-based parser to avoid a hard
// dependency on the prometheus client_model dto types for a single counter.
func scrapePromCounter(ctx context.Context, url, name string) (float64, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	found := false
	var sum float64
	sc := bufio.NewScanner(resp.Body)
	// Allow large metric lines (OpenMetrics exemplar lines can be long); 1 MiB.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Format: "metric_name{labels} value" or "metric_name value". The metric
		// name ends at the first '{' or space; the value is the last token.
		end := strings.IndexAny(line, "{ ")
		metricName := line
		if end > 0 {
			metricName = line[:end]
		}
		if metricName != name {
			continue
		}
		parts := strings.Split(line, " ")
		if len(parts) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(parts[len(parts)-1], 64)
		if err != nil {
			continue
		}
		sum += v
		found = true
	}
	return sum, found
}

const (
	routerScrapeInterval = 30 * time.Second
	maxRouterPodsToScrape = 10
)

// getCounterValue reads the current value of a Prometheus counter from a registry.
// For CounterVecs (labelled metrics) it sums every series so a single aggregate
// value is returned regardless of how many label combinations exist.
func getCounterValue(reg prometheus.Gatherer, name string) float64 {
	mfs, err := reg.Gather()
	if err != nil {
		return 0
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			var total float64
			for _, m := range mf.Metric {
				if c := m.GetCounter(); c != nil {
					total += c.GetValue()
				}
			}
			return total
		}
	}
	return 0
}

// getHistogramQuantile approximates the q-th quantile (q in [0,1]) of a
// histogram by pooling the cumulative bucket counts across ALL labelled series
// in the family (the latency histogram is a CounterVec keyed by server/path).
// Returns the bucket upper bound in the histogram's native units (seconds).
// Buckets are cumulative in the Prometheus text format.
func getHistogramQuantile(reg prometheus.Gatherer, name string, q float64) float64 {
	mfs, err := reg.Gather()
	if err != nil {
		return 0
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		if len(mf.Metric) == 0 {
			return 0
		}
		// Merge cumulative counts across series, keyed by bucket upper bound.
		merged := make(map[float64]uint64)
		var total uint64
		for _, m := range mf.Metric {
			h := m.GetHistogram()
			if h == nil {
				continue
			}
			if h.GetSampleCount() > 0 {
				total += h.GetSampleCount()
			}
			for _, b := range h.Bucket {
				merged[b.GetUpperBound()] += b.GetCumulativeCount()
			}
		}
		if total == 0 || len(merged) == 0 {
			return 0
		}
		bounds := make([]float64, 0, len(merged))
		for b := range merged {
			bounds = append(bounds, b)
		}
		sort.Float64s(bounds)
		threshold := float64(total) * q
		for _, b := range bounds {
			if float64(merged[b]) >= threshold {
				return b
			}
		}
		return bounds[len(bounds)-1]
	}
	return 0
}

// parseMetricRange converts a UI time-range selector into a time.Duration.
// Unknown/invalid values fall back to 24h.
func parseMetricRange(s string) time.Duration {
	switch s {
	case "1h":
		return time.Hour
	case "6h":
		return 6 * time.Hour
	case "24h":
		return 24 * time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// metricSampleInterval is the minimum spacing between recorded history samples
// (throttles the scrape-on-every-poll path so the rolling buffer stays bounded).
const metricSampleInterval = 30 * time.Second

// maxMetricSamples caps the rolling history (2880 samples @ 30s = 24h).
const maxMetricSamples = 2880

// maxMetricSamplesReturned caps the number of samples sent per response so the
// status payload stays small even when the buffer is full.
const maxMetricSamplesReturned = 240

// recordMetricSample appends the current metric snapshot to the rolling history
// at most once per metricSampleInterval, trimming to maxMetricSamples.
func (s *UIServer) recordMetricSample(m *SystemMetrics) {
	s.metricMu.Lock()
	defer s.metricMu.Unlock()
	now := time.Now().UTC()
	if !s.lastSampleAt.IsZero() && now.Sub(s.lastSampleAt) < metricSampleInterval {
		return
	}
	s.metricHistory = append(s.metricHistory, MetricSample{
		Time:            now.Unix(),
		RequestCount:    m.RequestCount24h,
		P50LatencyMs:    m.P50LatencyMs,
		P95LatencyMs:    m.P95LatencyMs,
		P99LatencyMs:    m.P99LatencyMs,
		EgressPublished: m.EgressPublished,
		EgressFailed:    m.EgressFailed,
		TokenThroughput: m.TokenThroughput,
	})
	if len(s.metricHistory) > maxMetricSamples {
		s.metricHistory = s.metricHistory[len(s.metricHistory)-maxMetricSamples:]
	}
	s.lastSampleAt = now
}

// samplesForRange returns the recorded samples within the requested time window
// (parsed from the ?range= query param), downsampled to maxMetricSamplesReturned
// so the response stays bounded. Returns a non-nil slice.
func (s *UIServer) samplesForRange(_ context.Context, rawRange string) []MetricSample {
	span := parseMetricRange(rawRange)
	cutoff := time.Now().UTC().Add(-span)
	s.metricMu.Lock()
	defer s.metricMu.Unlock()
	out := make([]MetricSample, 0, len(s.metricHistory))
	for _, sm := range s.metricHistory {
		if time.Unix(sm.Time, 0).After(cutoff) {
			out = append(out, sm)
		}
	}
	if len(out) == 0 {
		return []MetricSample{}
	}
	// Downsample to at most maxMetricSamplesReturned points (stride).
	if len(out) > maxMetricSamplesReturned {
		stride := (len(out) + maxMetricSamplesReturned - 1) / maxMetricSamplesReturned
		down := make([]MetricSample, 0, (len(out)+stride-1)/stride)
		for i := 0; i < len(out); i += stride {
			down = append(down, out[i])
		}
		return down
	}
	return out
}

// handleAlerts returns recent alert history from the alert manager.
func (s *UIServer) handleAlerts(w http.ResponseWriter, r *http.Request) {
	if s.alertManager == nil {
		jsonResponse(w, []any{})
		return
	}
	alerts := s.alertManager.ListAlerts(r.Context())
	jsonResponse(w, alerts)
}

func parseFloatCost(s string) float64 {
	if s == "" {
		return 0
	}
	var f float64
	_, _ = fmt.Sscanf(s, "%f", &f)
	return f
}

// handleDeployment routes deployment-related requests.
// Paths:
//
//	POST /api/deployments/{namespace}/{name}/execute - send input to deployment
//	GET /api/deployments/{namespace}/{name}/stream - SSE stream of responses
func (s *UIServer) handleDeployment(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/deployments/")
	parts := strings.SplitN(path, "/", 3)

	if len(parts) < 2 {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	namespace := parts[0]
	if namespace == "" {
		namespace = "default"
	}
	deploymentName := parts[1]

	// Check if this is execute, stream, history, complete, or deployment info.
	if len(parts) >= 3 {
		action := parts[2]
		switch {
		case r.Method == http.MethodPost && action == "execute":
			s.handleExecute(w, r, namespace, deploymentName)
		case r.Method == http.MethodPost && action == "complete":
			s.handleSaveResponse(w, r, namespace, deploymentName)
		case r.Method == http.MethodGet && action == "history":
			s.handleChatHistory(w, r, namespace, deploymentName)
		case r.Method == http.MethodGet && action == "stream":
			s.handleDeploymentStream(w, r, namespace, deploymentName)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	} else if r.Method == http.MethodGet {
		// GET /api/deployments/{namespace}/{name} - get deployment status
		s.handleGetDeployment(w, r, namespace, deploymentName)
	} else if r.Method == http.MethodDelete {
		// DELETE /api/deployments/{namespace}/{name}
		s.handleDeleteDeployment(w, r, namespace, deploymentName)
	} else {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleExecute processes POST /api/deployments/{namespace}/{name}/execute.
// It creates an AgentRun for the deployment's agent and returns the run name
// so the UI can subscribe to the existing run SSE stream for live progress.
func (s *UIServer) handleExecute(w http.ResponseWriter, r *http.Request, namespace, deploymentName string) {
	var req struct {
		Input     string `json:"input"`
		SessionID string `json:"sessionId,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Input == "" {
		http.Error(w, "input required", http.StatusBadRequest)
		return
	}

	// Generate session ID if not provided.
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = generateUUID()
	}

	// Verify the deployment exists and get the agent ref.
	var deployment agentorcv1alpha1.AgentDeployment
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: deploymentName, Namespace: namespace}, &deployment); err != nil {
		http.Error(w, "deployment not found", http.StatusNotFound)
		return
	}

	// Load existing checkpoint to build conversation context.
	existing, err := s.checkpoint.Load(r.Context(), sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("loading checkpoint: %v", err), http.StatusInternalServerError)
		return
	}

	// Save the user message to the checkpoint immediately.
	version := 1
	var history []agentorcv1alpha1.ConversationMessage
	if existing != nil {
		version = existing.Version + 1
		history = append(history, existing.ConversationHistory...)
	}
	history = append(history, agentorcv1alpha1.ConversationMessage{Role: "user", Content: req.Input})

	cp := &agentorcv1alpha1.Checkpoint{
		SessionID:           sessionID,
		Version:             version,
		ConversationHistory: history,
		Metadata: agentorcv1alpha1.CheckpointMetadata{
			TotalMessages:   len(history),
			StartTime:       &metav1.Time{Time: time.Now()},
			LastMessageTime: &metav1.Time{Time: time.Now()},
		},
	}
	if existing != nil && existing.Metadata.StartTime != nil {
		cp.Metadata.StartTime = existing.Metadata.StartTime
	}
	if _, err := s.checkpoint.Save(r.Context(), cp); err != nil {
		http.Error(w, fmt.Sprintf("saving checkpoint: %v", err), http.StatusInternalServerError)
		return
	}

	// Determine the prior run in this session (if any) for context chaining.
	priorRunRef := ""
	if existing != nil {
		priorRunRef = existing.LastRunRef
	}

	// Create an AgentRun for this chat message.
	run := &agentorcv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("chat-%s-", deploymentName),
			Namespace:    namespace,
			Labels: map[string]string{
				"agentorc.io/deployment": deploymentName,
				"agentorc.io/session":    sessionID,
				"agentorc.io/source":     "chat",
			},
		},
		Spec: agentorcv1alpha1.AgentRunSpec{
			AgentRef:    deployment.Spec.AgentRef,
			Input:       req.Input,
			PriorRunRef: priorRunRef,
		},
	}
	// Propagate the deployment's tool-execution timeout so long-holding tooling
	// (Sliver sessions, shells) isn't killed by the 60s default on every chat turn.
	if deployment.Spec.ToolExecutionTimeoutSec > 0 {
		run.Spec.Safeguards = &agentorcv1alpha1.AgentRunSafeguards{
			ToolExecutionTimeoutSec: deployment.Spec.ToolExecutionTimeoutSec,
		}
	}
	if err := s.crdClient.Create(r.Context(), run); err != nil {
		http.Error(w, fmt.Sprintf("creating agent run: %v", err), http.StatusInternalServerError)
		return
	}

	// Record the new run in the checkpoint so the next turn can chain to it.
	cp.LastRunRef = run.Name
	if _, err := s.checkpoint.Save(r.Context(), cp); err != nil {
		// Non-fatal: the run was created successfully; context chain will be broken
		// for the next turn but the current turn is unaffected.
		slog.Warn("failed to update checkpoint LastRunRef", "run", run.Name, "err", err)
	}

	// Also persist conversation history to the state store so the model-router can
	// load it via PriorRunRef when the run starts. This serves as a safety net
	// in case the prior run's finalization checkpoint wasn't written successfully.
	if s.store != nil && priorRunRef != "" {
		if err := s.ensureRunCheckpoint(r.Context(), priorRunRef, cp.ConversationHistory); err != nil {
			slog.Warn("safety-net checkpoint write failed", "run", priorRunRef, "err", err)
		}
	}

	jsonResponse(w, map[string]any{
		"runName":   run.Name,
		"sessionId": sessionID,
	})
}

// ensureRunCheckpoint is the safety net that lets a warm pod resume a run even if
// the model-router never checkpointed it (e.g. the pod was killed on its first turn
// before any LLM call). It writes the session transcript into the run's Redis
// checkpoint key — but ONLY when that key is empty. When the model-router DID
// checkpoint, its run-key messages are full-fidelity (tool calls / results); this
// function preserves them rather than overwriting with the session's
// role/content-only summary, which would degrade resumed context.
func (s *UIServer) ensureRunCheckpoint(ctx context.Context, runName string, history []agentorcv1alpha1.ConversationMessage) error {
	if s.store == nil || runName == "" {
		return nil
	}
	priorCheckpointKey := fmt.Sprintf("agentorc/runs/%s/state", runName)
	if existing, err := s.store.LoadMessages(ctx, priorCheckpointKey); err == nil && len(existing) > 0 {
		// Model-router already checkpointed this run — keep its full-fidelity history.
		return nil
	}
	var msgs []json.RawMessage
	for _, cm := range history {
		routerMsg := map[string]any{
			"role":    cm.Role,
			"content": cm.Content,
		}
		raw, _ := json.Marshal(routerMsg)
		msgs = append(msgs, raw)
	}
	return s.store.SaveMessages(ctx, priorCheckpointKey, msgs, 24*time.Hour)
}

// handleChatHistory returns the conversation history for a session.
// GET /api/deployments/{namespace}/{name}/history?sessionId=X
func (s *UIServer) handleChatHistory(w http.ResponseWriter, r *http.Request, namespace, deploymentName string) { //nolint:unparam

	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		jsonResponse(w, map[string]any{"messages": []any{}})
		return
	}

	cp, err := s.checkpoint.Load(r.Context(), sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("loading checkpoint: %v", err), http.StatusInternalServerError)
		return
	}
	if cp == nil {
		jsonResponse(w, map[string]any{"messages": []any{}})
		return
	}

	jsonResponse(w, map[string]any{
		"sessionId": cp.SessionID,
		"messages":  cp.ConversationHistory,
	})
}

// handleSaveResponse saves an assistant response to the session checkpoint.
// POST /api/deployments/{namespace}/{name}/complete
func (s *UIServer) handleSaveResponse(w http.ResponseWriter, r *http.Request, namespace, deploymentName string) { //nolint:unparam

	var req struct {
		SessionID    string `json:"sessionId"`
		Output       string `json:"output"`
		TraceEntries string `json:"traceEntries,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.SessionID == "" || req.Output == "" {
		http.Error(w, "sessionId and output required", http.StatusBadRequest)
		return
	}

	cp, err := s.checkpoint.Load(r.Context(), req.SessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("loading checkpoint: %v", err), http.StatusInternalServerError)
		return
	}
	if cp == nil {
		// Checkpoint was lost (e.g. server restart) — create a minimal one so the
		// assistant response is still persisted for subsequent history loads.
		cp = &agentorcv1alpha1.Checkpoint{
			SessionID: req.SessionID,
			Version:   0,
			Metadata: agentorcv1alpha1.CheckpointMetadata{
				StartTime: &metav1.Time{Time: time.Now()},
			},
		}
	}

	cp.Version++
	cp.ConversationHistory = append(cp.ConversationHistory, agentorcv1alpha1.ConversationMessage{
		Role:         "assistant",
		Content:      req.Output,
		TraceEntries: req.TraceEntries,
	})
	cp.Metadata.TotalMessages = len(cp.ConversationHistory)
	cp.Metadata.LastMessageTime = &metav1.Time{Time: time.Now()}

	// Populate TotalCostUSD from the current run's spend.
	if cp.LastRunRef != "" {
		var run agentorcv1alpha1.AgentRun
		if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: cp.LastRunRef, Namespace: namespace}, &run); err == nil && run.Status.SpendUSD != "" {
			// Accumulate: parse existing total, add this run's spend.
			existing, _ := strconv.ParseFloat(cp.Metadata.TotalCostUSD, 64)
			runSpend, _ := strconv.ParseFloat(run.Status.SpendUSD, 64)
			if runSpend > 0 {
				cp.Metadata.TotalCostUSD = fmt.Sprintf("%.6f", existing+runSpend)
			}
		}
	}

	if _, err := s.checkpoint.Save(r.Context(), cp); err != nil {
		http.Error(w, fmt.Sprintf("saving checkpoint: %v", err), http.StatusInternalServerError)
		return
	}

	// Also persist to state store under the current run's key for context chaining.
	// This serves as a safety net in case the model-router's finalization checkpoint
	// wasn't written or the run completed without triggering periodic checkpoints.
	if s.store != nil && cp.LastRunRef != "" {
		checkpointKey := fmt.Sprintf("agentorc/runs/%s/state", cp.LastRunRef)
		var msgs []json.RawMessage
		for _, cm := range cp.ConversationHistory {
			routerMsg := map[string]any{
				"role":    cm.Role,
				"content": cm.Content,
			}
			raw, _ := json.Marshal(routerMsg)
			msgs = append(msgs, raw)
		}
		_ = s.store.SaveMessages(r.Context(), checkpointKey, msgs, 24*time.Hour)
	}

	jsonResponse(w, map[string]string{"status": "ok"})
}

// handleDeploymentStream handles GET /api/deployments/{namespace}/{name}/stream
// Streams execution responses as Server-Sent Events.
func (s *UIServer) handleDeploymentStream(w http.ResponseWriter, r *http.Request, namespace, deploymentName string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	var deployment agentorcv1alpha1.AgentDeployment
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: deploymentName, Namespace: namespace}, &deployment); err != nil {
		http.Error(w, "deployment not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// TODO: Implement actual streaming logic
	// For now, send a placeholder event
	writeSSE(w, map[string]any{
		"type":    "placeholder",
		"message": "[Streaming implementation pending]",
	})
	flusher.Flush()
}

// handleGetDeployment handles GET /api/deployments/{namespace}/{name}
func (s *UIServer) handleGetDeployment(w http.ResponseWriter, r *http.Request, namespace, deploymentName string) {
	var deployment agentorcv1alpha1.AgentDeployment
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: deploymentName, Namespace: namespace}, &deployment); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	type summaryInfo struct {
		Name                string `json:"name"`
		Namespace           string `json:"namespace"`
		AgentRef            string `json:"agentRef"`
		Phase               string `json:"phase"`
		ReadyReplicas       int32  `json:"readyReplicas"`
		AvailableReplicas   int32  `json:"availableReplicas"`
		ConsecutiveFailures int    `json:"consecutiveFailures"`
		InputSourceType     string `json:"inputSourceType"`
		LastUpdateTime      string `json:"lastUpdateTime,omitempty"`
		Message             string `json:"message,omitempty"`
		ContextUsedTokens   int    `json:"contextUsedTokens"`
		MaxContextTokens    int    `json:"maxContextTokens"`
	}

	info := summaryInfo{
		Name:                deployment.Name,
		Namespace:           deployment.Namespace,
		AgentRef:            deployment.Spec.AgentRef,
		Phase:               string(deployment.Status.Phase),
		ReadyReplicas:       deployment.Status.ReadyReplicas,
		AvailableReplicas:   deployment.Status.AvailableReplicas,
		ConsecutiveFailures: deployment.Status.ConsecutiveFailures,
		Message:             deployment.Status.Message,
		ContextUsedTokens:   deployment.Status.ContextUsedTokens,
		MaxContextTokens:    deployment.Status.MaxContextTokens,
	}

	if deployment.Spec.InputSource != nil {
		info.InputSourceType = string(deployment.Spec.InputSource.Type)
	}

	if deployment.Status.LastUpdateTime != nil {
		info.LastUpdateTime = deployment.Status.LastUpdateTime.UTC().Format(time.RFC3339)
	}

	jsonResponse(w, info)
}

// handleDeleteDeployment handles DELETE /api/deployments/{namespace}/{name}.
func (s *UIServer) handleDeleteDeployment(w http.ResponseWriter, r *http.Request, namespace, deploymentName string) {
	var deployment agentorcv1alpha1.AgentDeployment
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: deploymentName, Namespace: namespace}, &deployment); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := s.crdClient.Delete(r.Context(), &deployment); err != nil {
		http.Error(w, fmt.Sprintf("deleting deployment: %v", err), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "ok"})
}

// handleListWorkflows lists AgentWorkflows in the requested namespace.
func (s *UIServer) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	var list agentorcv1alpha1.AgentWorkflowList
	if err := s.crdClient.List(r.Context(), &list, tenantScope(r.Context(), ns)...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type summary struct {
		Name           string `json:"name"`
		Namespace      string `json:"namespace"`
		Description    string `json:"description,omitempty"`
		Phase          string `json:"phase"`
		StepCount      int    `json:"stepCount"`
		TotalSpendUSD  string `json:"totalSpendUSD"`
		StartTime      string `json:"startTime,omitempty"`
		CompletionTime string `json:"completionTime,omitempty"`
	}
	out := make([]summary, 0, len(list.Items))
	for _, wf := range list.Items {
		s := summary{
			Name:          wf.Name,
			Namespace:     wf.Namespace,
			Description:   wf.Spec.Description,
			Phase:         string(wf.Status.Phase),
			StepCount:     len(wf.Spec.Steps),
			TotalSpendUSD: wf.Status.TotalSpendUSD,
		}
		if wf.Status.StartTime != nil {
			s.StartTime = wf.Status.StartTime.UTC().Format(time.RFC3339)
		}
		if wf.Status.CompletionTime != nil {
			s.CompletionTime = wf.Status.CompletionTime.UTC().Format(time.RFC3339)
		}
		out = append(out, s)
	}
	jsonResponse(w, out)
}

// handleGetWorkflow returns detail for a single AgentWorkflow including step statuses.
// Path: /api/workflows/{namespace}/{name}
func (s *UIServer) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/workflows/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		http.Error(w, "invalid path: expected /api/workflows/{namespace}/{name}", http.StatusBadRequest)
		return
	}
	namespace, name := parts[0], parts[1]

	var wf agentorcv1alpha1.AgentWorkflow
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: namespace}, &wf); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	type stepStatus struct {
		Name           string `json:"name"`
		AgentRef       string `json:"agentRef"`
		Input          string `json:"input,omitempty"`
		Phase          string `json:"phase"`
		AgentRunRef    string `json:"agentRunRef,omitempty"`
		Output         string `json:"output,omitempty"`
		SpendUSD       string `json:"spendUSD,omitempty"`
		FailureReason  string `json:"failureReason,omitempty"`
		StartTime      string `json:"startTime,omitempty"`
		CompletionTime string `json:"completionTime,omitempty"`
	}
	type detail struct {
		Name           string       `json:"name"`
		Namespace      string       `json:"namespace"`
		Description    string       `json:"description,omitempty"`
		Phase          string       `json:"phase"`
		StepCount      int          `json:"stepCount"`
		TotalSpendUSD  string       `json:"totalSpendUSD"`
		Steps          []stepStatus `json:"steps"`
		StartTime      string       `json:"startTime,omitempty"`
		CompletionTime string       `json:"completionTime,omitempty"`
	}

	d := detail{
		Name:          wf.Name,
		Namespace:     wf.Namespace,
		Description:   wf.Spec.Description,
		Phase:         string(wf.Status.Phase),
		StepCount:     len(wf.Spec.Steps),
		TotalSpendUSD: wf.Status.TotalSpendUSD,
		Steps:         make([]stepStatus, 0, len(wf.Status.Steps)),
	}
	if wf.Status.StartTime != nil {
		d.StartTime = wf.Status.StartTime.UTC().Format(time.RFC3339)
	}
	if wf.Status.CompletionTime != nil {
		d.CompletionTime = wf.Status.CompletionTime.UTC().Format(time.RFC3339)
	}

	// Build maps from step name to spec fields for the detail view.
	agentRefByStep := make(map[string]string, len(wf.Spec.Steps))
	inputByStep := make(map[string]string, len(wf.Spec.Steps))
	for _, step := range wf.Spec.Steps {
		agentRefByStep[step.Name] = step.AgentRef
		inputByStep[step.Name] = step.Input
	}

	// Build a map of step outputs so we can resolve template variables.
	outputByStep := make(map[string]string, len(wf.Status.Steps))
	for _, ss := range wf.Status.Steps {
		outputByStep[ss.Name] = ss.Output
	}

	for _, ss := range wf.Status.Steps {
		// Resolve {{steps.<name>.output}} placeholders so the UI shows
		// the actual values that were sent to the agent.
		resolvedInput := inputByStep[ss.Name]
		for name, output := range outputByStep {
			resolvedInput = strings.ReplaceAll(resolvedInput,
				fmt.Sprintf("{{steps.%s.output}}", name), output)
		}
		st := stepStatus{
			Name:          ss.Name,
			AgentRef:      agentRefByStep[ss.Name],
			Input:         resolvedInput,
			Phase:         string(ss.Phase),
			AgentRunRef:   ss.AgentRunRef,
			Output:        ss.Output,
			SpendUSD:      ss.SpendUSD,
			FailureReason: ss.FailureReason,
		}
		if ss.StartTime != nil {
			st.StartTime = ss.StartTime.UTC().Format(time.RFC3339)
		}
		if ss.CompletionTime != nil {
			st.CompletionTime = ss.CompletionTime.UTC().Format(time.RFC3339)
		}
		d.Steps = append(d.Steps, st)
	}
	jsonResponse(w, d)
}

// generateUUID generates a simple pseudo-UUID for execution IDs.
// In production, use github.com/google/uuid
func generateUUID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// ── Run history (PostgreSQL archival) ─────────────────────────────────────

// handleListRunHistory returns archived AgentRuns from PostgreSQL with filtering.
// GET /api/runs/history?limit=50&offset=0&phase=Succeeded&agentRef=my-agent&search=foo
func (s *UIServer) handleListRunHistory(w http.ResponseWriter, r *http.Request) {
	if s.pgStore == nil {
		http.Error(w, `{"error":"run archival not configured"}`, http.StatusServiceUnavailable)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	phase := r.URL.Query().Get("phase")
	agentRef := r.URL.Query().Get("agentRef")
	search := r.URL.Query().Get("search")

	q := postgresql.HistoryQuery{
		Limit:    limit,
		Offset:   offset,
		Phase:    phase,
		AgentRef: agentRef,
		Search:   search,
	}
	// Apply tenant scoping: when a tenant identity is present, restrict to
	// the tenant's namespace so users only see runs they are authorized for.
	if tenant, ok := TenantFromContext(r.Context()); ok && tenant != nil && tenant.Namespace != "" {
		q.Namespace = tenant.Namespace
	} else {
		q.Namespace = r.URL.Query().Get("namespace")
	}

	page, err := s.pgStore.QueryHistory(r.Context(), q)
	if err != nil {
		slog.Error("querying run history", "err", err)
		http.Error(w, "querying run history: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, page)
}

// handleGetArchivedRun returns the full detail of a single archived AgentRun
// from PostgreSQL, or 404 if it is not present. This mirrors the live run-detail
// shape (runDetail) so the UI can reuse the same presentational components for
// runs that have been garbage-collected from Kubernetes.
//
// GET /api/runs/history/{namespace}/{name}
func (s *UIServer) handleGetArchivedRun(w http.ResponseWriter, r *http.Request, namespace, runName string) {
	if s.pgStore == nil {
		http.Error(w, "run archival not configured", http.StatusServiceUnavailable)
		return
	}
	archive, err := s.pgStore.GetRun(r.Context(), namespace, runName)
	if err != nil {
		slog.Error("loading archived run", "run", runName, "ns", namespace, "err", err)
		http.Error(w, "loading archived run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if archive == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Re-hydrate the JSON-serialised fields captured at archival time.
	var routing []agentorcv1alpha1.RoutingDecision
	if len(archive.RoutingDecisionsJSON) > 0 {
		_ = json.Unmarshal(archive.RoutingDecisionsJSON, &routing)
	}
	var childRefs []string
	if len(archive.ChildRunRefsJSON) > 0 {
		_ = json.Unmarshal(archive.ChildRunRefsJSON, &childRefs)
	}

	resolvedModel := ""
	if len(routing) > 0 {
		// The last decision is the runtime-selected model (non-candidate, if any).
		for i := len(routing) - 1; i >= 0; i-- {
			if !strings.HasPrefix(routing[i].Reason, "configured provider") {
				resolvedModel = routing[i].Model
				break
			}
		}
		if resolvedModel == "" {
			resolvedModel = routing[len(routing)-1].Model
		}
	}

	detail := archivedRunDetail{
		Name:               archive.Name,
		Namespace:          archive.Namespace,
		AgentRef:           archive.AgentRef,
		Input:              archive.Input,
		Phase:              archive.Phase,
		Output:             archive.Output,
		SpendUSD:           archive.SpendUSD,
		RestartCount:       archive.RestartCount,
		PodName:            archive.PodName,
		ContextUsedTokens:  archive.ContextUsedTokens,
		MaxContextTokens:   archive.MaxContextTokens,
		StartTime:          formatTimePtr(archive.StartTime),
		CompletionTime:     formatTimePtr(archive.CompletionTime),
		RoutingDecisions:   routingDecisionsToJSON(routing),
		ChildRunRefs:       childRefs,
		ResolvedModel:      resolvedModel,
	}

	// Resolve the Agent CRD to surface tools/MCP surface area alongside the run.
	if s.crdClient != nil {
		var agent agentorcv1alpha1.Agent
		if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: archive.AgentRef, Namespace: archive.Namespace}, &agent); err == nil {
			tools := agent.Spec.Tools
			if tools == nil {
				tools = []string{}
			}
			detail.Tools = tools
			// Resolve MCP servers in the agent's namespace whose allowedAgents
			// explicitly lists this agent (default-deny: empty = no access).
			var mcpList agentorcv1alpha1.MCPServerList
			if err := s.crdClient.List(r.Context(), &mcpList, client.InNamespace(archive.Namespace)); err == nil {
				for i := range mcpList.Items {
					mp := &mcpList.Items[i]
					if len(mp.Spec.AllowedAgents) == 0 {
						continue
					}
					for _, a := range mp.Spec.AllowedAgents {
						if a == archive.AgentRef {
							detail.MCPServers = append(detail.MCPServers, mp.Name)
							break
						}
					}
				}
			}
		}
	}

	jsonResponse(w, detail)
}

// routingDecisionJSON is the JSON shape for a single routing decision as
// returned to the UI. Shared by the live run-detail and archived run-detail
// handlers so the UI consumes one shape.
type routingDecisionJSON struct {
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	Strategy   string `json:"strategy"`
	Reason     string `json:"reason"`
	Confidence string `json:"confidence"`
	Timestamp  string `json:"timestamp,omitempty"`
}

// archivedRunDetail is the JSON shape returned for an archived run. It mirrors
// the live runDetail struct (so the UI reuses its presentational components)
// and adds tool/mcp resolution plus a computed resolved-model string.
type archivedRunDetail struct {
	Name               string              `json:"name"`
	Namespace          string              `json:"namespace"`
	AgentRef           string              `json:"agentRef"`
	Input              string              `json:"input"`
	Phase              string              `json:"phase"`
	Output             string              `json:"output,omitempty"`
	SpendUSD           string              `json:"spendUSD"`
	RestartCount       int                 `json:"restartCount"`
	LastRestartReason  string              `json:"lastRestartReason,omitempty"`
	StartTime          string              `json:"startTime,omitempty"`
	CompletionTime     string              `json:"completionTime,omitempty"`
	RoutingDecisions   []routingDecisionJSON `json:"routingDecisions"`
	ChildRunRefs       []string            `json:"childRunRefs,omitempty"`
	ParentRunRef       string              `json:"parentRunRef,omitempty"`
	ClarifyQuestion    string              `json:"clarifyQuestion,omitempty"`
	ClarifyAnswer      string              `json:"clarifyAnswer,omitempty"`
	WaitingSince       string              `json:"waitingSince,omitempty"`
	ContinuationRunRef string              `json:"continuationRunRef,omitempty"`
	ContextUsedTokens  int                 `json:"contextUsedTokens"`
	MaxContextTokens   int                 `json:"maxContextTokens"`
	PodName            string              `json:"podName,omitempty"`
	Tools              []string            `json:"tools,omitempty"`
	MCPServers         []string            `json:"mcps,omitempty"`
	ResolvedModel      string              `json:"resolvedModel,omitempty"`
}

// routingDecisionsToJSON converts CRD routing decisions into the JSON shape,
// formatting the timestamp to RFC3339 (UTC). Reused by both the live and
// archived run-detail handlers.
func routingDecisionsToJSON(rds []agentorcv1alpha1.RoutingDecision) []routingDecisionJSON {
	if len(rds) == 0 {
		return []routingDecisionJSON{}
	}
	out := make([]routingDecisionJSON, 0, len(rds))
	for i := range rds {
		d := routingDecisionJSON{
			Model:      rds[i].Model,
			Provider:   rds[i].Provider,
			Strategy:   rds[i].Strategy,
			Reason:     rds[i].Reason,
			Confidence: rds[i].Confidence,
		}
		if rds[i].Timestamp != nil {
			d.Timestamp = rds[i].Timestamp.UTC().Format(time.RFC3339)
		}
		out = append(out, d)
	}
	return out
}

// formatTimePtr renders a *time.Time to RFC3339 (UTC) or "" when nil.
func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ── Generic resource CRUD ───────────────────────────────────────────────────
//
// These endpoints provide full read/write access to all CRD types via a single
//   - The agentorc.io/managed-by label is checked on delete to prevent
//     users from deleting resources not created by agent-orc.
//   - Tenant scoping: when a TenantIdentity is in context, all list/get
//     operations are restricted to the tenant's namespace.

// crdKindInfo maps URL path segments to (listType, objectType) pairs.
type crdKindInfo struct {
	listType client.ObjectList
	newObj   func() client.Object
	gvk      string // for OpenAPI schema reference
}

var crdKinds = map[string]crdKindInfo{
	"agents":          {&agentorcv1alpha1.AgentList{}, func() client.Object { return &agentorcv1alpha1.Agent{} }, "Agent"},
	"tools":           {&agentorcv1alpha1.ToolList{}, func() client.Object { return &agentorcv1alpha1.Tool{} }, "Tool"},
	"mcpservers":      {&agentorcv1alpha1.MCPServerList{}, func() client.Object { return &agentorcv1alpha1.MCPServer{} }, "MCPServer"},
	"modelproviders":  {&agentorcv1alpha1.ModelProviderList{}, func() client.Object { return &agentorcv1alpha1.ModelProvider{} }, "ModelProvider"},
	"knowledgebases":  {&agentorcv1alpha1.KnowledgeBaseList{}, func() client.Object { return &agentorcv1alpha1.KnowledgeBase{} }, "KnowledgeBase"},
	"modelselectors":  {&agentorcv1alpha1.ModelSelectorList{}, func() client.Object { return &agentorcv1alpha1.ModelSelector{} }, "ModelSelector"},
	"agentdeployments": {&agentorcv1alpha1.AgentDeploymentList{}, func() client.Object { return &agentorcv1alpha1.AgentDeployment{} }, "AgentDeployment"},
	"agentworkflows": {&agentorcv1alpha1.AgentWorkflowList{}, func() client.Object { return &agentorcv1alpha1.AgentWorkflow{} }, "AgentWorkflow"},
}

// handleResourceCollection handles GET (list) and POST (create) on /api/resources/{kind}.
func (s *UIServer) handleResourceCollection(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/resources/")
	kind, _, _ := strings.Cut(path, "/")
	info, ok := crdKinds[kind]
	if !ok || kind == "" {
		http.Error(w, `{"error":"unknown resource kind"}`, http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.listResource(w, r, info)
	case http.MethodPost:
		s.createResource(w, r, info)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleResource handles GET (read), PUT (update), DELETE on /api/resources/{kind}/{ns}/{name}.
func (s *UIServer) handleResource(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/resources/")
	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		http.Error(w, "expected /api/resources/{kind}/{namespace}/{name}", http.StatusBadRequest)
		return
	}
	kind, ns, name := parts[0], parts[1], parts[2]
	info, ok := crdKinds[kind]
	if !ok {
		http.Error(w, `{"error":"unknown resource kind"}`, http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getResource(w, r, info, ns, name)
	case http.MethodPut:
		s.updateResource(w, r, info, ns, name)
	case http.MethodDelete:
		s.deleteResource(w, r, info, ns, name)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// listResource lists CRDs scoped to the tenant's namespace (or the ?namespace= param).
func (s *UIServer) listResource(w http.ResponseWriter, r *http.Request, info crdKindInfo) {
	ns := r.URL.Query().Get("namespace")
	opts := tenantScope(r.Context(), ns)
	if err := s.crdClient.List(r.Context(), info.listType, opts...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, info.listType)
}

// createResource handles POST /api/resources/{kind} — creates a new CRD from
// the request body. The body is expected to be the full CRD JSON (including
// metadata.name and metadata.namespace). The managed-by label is applied
// automatically. Validating webhooks fire on Create, preserving admission control.
func (s *UIServer) createResource(w http.ResponseWriter, r *http.Request, info crdKindInfo) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20)) // 2 MB cap
	if err != nil {
		http.Error(w, "reading body: "+err.Error(), http.StatusBadRequest)
		return
	}
	obj := info.newObj()
	if err := json.Unmarshal(body, obj); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Apply managed-by label so the resource is tracked as user-created via the UI.
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	if _, exists := labels["app.kubernetes.io/managed-by"]; !exists {
		labels["app.kubernetes.io/managed-by"] = "agent-orc"
		obj.SetLabels(labels)
	}
	// Enforce tenant scoping: when a tenant identity is present, force the
	// namespace to the tenant's namespace (don't trust the body).
	if tenant, ok := TenantFromContext(r.Context()); ok && tenant != nil && tenant.Namespace != "" {
		obj.SetNamespace(tenant.Namespace)
	}

	if err := s.crdClient.Create(r.Context(), obj); err != nil {
		http.Error(w, "creating resource: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, obj)
}

// getResource fetches a single CRD by name, returning spec + metadata (status included read-only).
func (s *UIServer) getResource(w http.ResponseWriter, r *http.Request, info crdKindInfo, ns, name string) {
	obj := info.newObj()
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: ns}, obj); err != nil {
		code := http.StatusInternalServerError
		if k8serrors.IsNotFound(err) {
			code = http.StatusNotFound
		}
		http.Error(w, err.Error(), code)
		return
	}
	jsonResponse(w, obj)
}

// updateResource applies a PUT (full spec replacement). Status is stripped from
// the incoming object to enforce controller-managed immutability. The validating
// webhook fires on Update, preserving admission control (image allowlist, etc.).
func (s *UIServer) updateResource(w http.ResponseWriter, r *http.Request, info crdKindInfo, ns, name string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20)) // 2 MB cap
	if err != nil {
		http.Error(w, "reading body: "+err.Error(), http.StatusBadRequest)
		return
	}

	obj := info.newObj()
	if err := json.Unmarshal(body, obj); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Enforce name/namespace from URL path — never trust the body.
	obj.SetName(name)
	obj.SetNamespace(ns)

	// Clear status — the controller owns it. If the client sent status fields,
	// we strip them so a PUT never clobbers observed state.
	if statusWriter, ok := obj.(client.Object); ok {
		_ = statusWriter
	}
	if accessor, ok := obj.(metav1.Object); ok {
		// Set the managed-by label so we never accidentally create unmodified resources.
		labels := accessor.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		if _, exists := labels["app.kubernetes.io/managed-by"]; !exists {
			labels["app.kubernetes.io/managed-by"] = "agent-orc"
			accessor.SetLabels(labels)
		}
	}

	if err := s.crdClient.Update(r.Context(), obj); err != nil {
		http.Error(w, "updating resource: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, obj)
}

// deleteResource deletes a CRD. Checks the agentorc.io/managed-by label to
// prevent deletion of resources not created by agent-orc.
func (s *UIServer) deleteResource(w http.ResponseWriter, r *http.Request, info crdKindInfo, ns, name string) {
	obj := info.newObj()
	obj.SetName(name)
	obj.SetNamespace(ns)
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: ns}, obj); err != nil {
		code := http.StatusInternalServerError
		if k8serrors.IsNotFound(err) {
			code = http.StatusNotFound
		}
		http.Error(w, err.Error(), code)
		return
	}
	// Managed-by guard: only allow deletion of agent-orc-managed resources.
	if !isManagedByAgentOrc(obj) {
		http.Error(w, `{"error":"resource is not managed by agent-orc"}`, http.StatusForbidden)
		return
	}
	if err := s.crdClient.Delete(r.Context(), obj); err != nil {
		http.Error(w, "deleting resource: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "deleted"})
}

// isManagedByAgentOrc returns true if the object is managed by agent-orc.
// Resources are considered managed if they carry any of:
//   - app.kubernetes.io/managed-by: agent-orc (controllers, system-created AgentRuns)
//   - agentorc.io/managed-by: agent-orc (some controllers use this variant)
//   - agentorc.io/source (chat-created runs via the UI API)
//   - agentorc.io/deployment (runs belonging to a deployment)
func isManagedByAgentOrc(obj client.Object) bool {
	labels := obj.GetLabels()
	return labels["app.kubernetes.io/managed-by"] == "agent-orc" ||
		labels["agentorc.io/managed-by"] == "agent-orc" ||
		labels["agentorc.io/source"] != "" ||
		labels["agentorc.io/deployment"] != ""
}

// ── OpenAPI ─────────────────────────────────────────────────────────────────

//go:embed schemas/openapi-ui.yaml
var openapiUIYAML []byte

var (
	uiOpenAPIOnce    sync.Once
	uiOpenAPIJSON    []byte
	uiOpenAPIErr     error
)

func uiOpenAPIJSONBytes() ([]byte, error) {
	uiOpenAPIOnce.Do(func() {
		uiOpenAPIJSON, uiOpenAPIErr = yaml.YAMLToJSON(openapiUIYAML)
	})
	return uiOpenAPIJSON, uiOpenAPIErr
}

// handleUIOpenAPI serves the UI API OpenAPI document.
func (s *UIServer) handleUIOpenAPI(w http.ResponseWriter, r *http.Request) {
	b, err := uiOpenAPIJSONBytes()
	if err != nil {
		http.Error(w, `{"error":"openapi spec unavailable"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(b)
}
