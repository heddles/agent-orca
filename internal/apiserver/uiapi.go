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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
	"github.com/floppyfish14/agent-orc/internal/checkpoint"
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
// Set --ui-auth-enabled=false for local development where the proxy is not running.
type UIServer struct {
	crdClient       client.Client
	k8s             kubernetes.Interface
	checkpoint      checkpoint.Store
	store           state.Store
	stateConfigured bool
	authEnabled     bool
}

// nsListOpts returns a ListOption slice scoped to ns, or empty (all namespaces) when ns is "".
func nsListOpts(ns string) []client.ListOption {
	if ns == "" {
		return nil
	}
	return []client.ListOption{client.InNamespace(ns)}
}

// NewUIServer creates a UIServer.
// stateConfigured should be true when a Redis-backed state store is active.
// store may be nil if no state backend is configured.
// authEnabled requires a valid UIProxy SA token (audience agentorc/ui) on all API calls.
func NewUIServer(k8s kubernetes.Interface, crdClient client.Client, stateConfigured bool, store state.Store, authEnabled bool) *UIServer {
	return &UIServer{
		crdClient:       crdClient,
		k8s:             k8s,
		checkpoint:      checkpoint.NewInMemoryStore(),
		store:           store,
		stateConfigured: stateConfigured,
		authEnabled:     authEnabled,
	}
}

// Handler returns the http.Handler for the UI API.
func (s *UIServer) Handler() http.Handler {
	mux := http.NewServeMux()
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
	return corsMiddleware(s.requireAuth(mux))
}

// requireAuth wraps next with Kubernetes TokenReview authentication.
// When authEnabled is false it is a no-op (local development).
// The token may be provided as an Authorization: Bearer header or as a
// ?token= query parameter (required for SSE, where EventSource cannot set headers).
func (s *UIServer) requireAuth(next http.Handler) http.Handler {
	if !s.authEnabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := extractBearer(r)
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if token == "" {
			writeUIAuthFailure(w, true, nil)
			return
		}
		if _, err := s.validateUIToken(r.Context(), token); err != nil {
			slog.Warn("UI auth rejected", "err", err, "path", r.URL.Path)
			writeUIAuthFailure(w, false, err)
			return
		}
		next.ServeHTTP(w, r)
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
	opts := nsListOpts(ns)
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
	ns, runName := parts[0], parts[1]

	if len(parts) == 3 {
		if strings.HasPrefix(parts[2], "mcpapp/") {
			s.handleMCPApp(w, r, strings.TrimPrefix(parts[2], "mcpapp/"))
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

	type routingDecisionJSON struct {
		Model      string `json:"model"`
		Provider   string `json:"provider"`
		Strategy   string `json:"strategy"`
		Reason     string `json:"reason"`
		Confidence string `json:"confidence"`
		Timestamp  string `json:"timestamp,omitempty"`
	}
	type runDetail struct {
		Name                      string                `json:"name"`
		Namespace                 string                `json:"namespace"`
		AgentRef                  string                `json:"agentRef"`
		Input                     string                `json:"input"`
		Phase                     string                `json:"phase"`
		Output                    string                `json:"output,omitempty"`
		SpendUSD                  string                `json:"spendUSD"`
		RestartCount              int                   `json:"restartCount"`
		LastRestartReason         string                `json:"lastRestartReason,omitempty"`
		StartTime                 string                `json:"startTime,omitempty"`
		CompletionTime            string                `json:"completionTime,omitempty"`
		RoutingDecisions          []routingDecisionJSON `json:"routingDecisions"`
		ChildRunRefs              []string              `json:"childRunRefs,omitempty"`
		ParentRunRef              string                `json:"parentRunRef,omitempty"`
		ClarifyQuestion           string                `json:"clarifyQuestion,omitempty"`
		ClarifyAnswer             string                `json:"clarifyAnswer,omitempty"`
		WaitingSince              string                `json:"waitingSince,omitempty"`
		ContinuationRunRef        string                `json:"continuationRunRef,omitempty"`
		ContextUsedTokens         int                   `json:"contextUsedTokens"`
		MaxContextTokens          int                   `json:"maxContextTokens"`
	}

	detail := runDetail{
		Name:                      run.Name,
		Namespace:                 run.Namespace,
		AgentRef:                  run.Spec.AgentRef,
		Input:                     run.Spec.Input,
		Phase:                     string(run.Status.Phase),
		Output:                    run.Status.Output,
		SpendUSD:                  run.Status.SpendUSD,
		RestartCount:              run.Status.RestartCount,
		LastRestartReason:         run.Status.LastRestartReason,
		RoutingDecisions:          make([]routingDecisionJSON, 0, len(run.Status.RoutingDecisions)),
		ChildRunRefs:              run.Status.ChildRunRefs,
		ParentRunRef:              run.Spec.ParentRunRef,
		ClarifyQuestion:           run.Status.ClarifyQuestion,
		ClarifyAnswer:             run.Status.ClarifyAnswer,
		ContinuationRunRef:        run.Status.ContinuationRunRef,
		ContextUsedTokens:         run.Status.ContextUsedTokens,
		MaxContextTokens:          run.Status.MaxContextTokens,
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
func (s *UIServer) handleStream(w http.ResponseWriter, r *http.Request, ns, runName string) {
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
			fmt.Sscanf(rd.Confidence, "%f", &conf)
			writeSSE(w, map[string]interface{}{
				"type": "modelSelected",
				"model":         rd.Model,
				"reason":        fmt.Sprintf("[%s] %s — %s", rd.Strategy, rd.Provider, rd.Reason),
				"confidence":    conf,
			})
		}
		emittedRouting = len(run.Status.RoutingDecisions)
	}

	// emitTerminal sends the final SSE event for a completed run. Returns true if terminal.
	emitTerminal := func(run *agentorcv1alpha1.AgentRun) bool {
		phase := string(run.Status.Phase)
		if phase == "Succeeded" {
			writeSSE(w, map[string]interface{}{
				"type": "finalOutput",
				"output":        run.Status.Output,
			})
			flusher.Flush()
			return true
		}
		if phase == "Failed" {
			writeSSE(w, map[string]interface{}{
				"type": "error",
				"message":       run.Status.LastRestartReason,
			})
			flusher.Flush()
			return true
		}
		if phase == "WaitingForInput" && run.Status.ClarifyQuestion != "" {
			writeSSE(w, map[string]interface{}{
				"type": "clarify",
				"question":      run.Status.ClarifyQuestion,
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
					var evt map[string]interface{}
					if json.Unmarshal([]byte(token[1:]), &evt) == nil {
						writeSSE(w, evt)
						flusher.Flush()
						continue
					}
				}
				output.WriteString(token)
				writeSSE(w, map[string]interface{}{
					"type": "token",
					"content":       token,
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
		for i := 0; i < 6; i++ {
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
			writeSSE(w, map[string]interface{}{
				"type": "clarify",
				"question":      run.Status.ClarifyQuestion,
			})
			flusher.Flush()
			return
		}

		writeSSE(w, map[string]interface{}{
			"type": "finalOutput",
			"output":        output.String(),
		})
		flusher.Flush()
		return
	}

	// No token output captured (store nil or stream failed). Fall back to
	// polling the AgentRun status until the controller updates it.
	fallbackTicker := time.NewTicker(500 * time.Millisecond)
	defer fallbackTicker.Stop()
	for i := 0; i < 60; i++ {
		var run agentorcv1alpha1.AgentRun
		if err := s.crdClient.Get(ctx, client.ObjectKey{Name: runName, Namespace: ns}, &run); err != nil {
			writeSSE(w, map[string]interface{}{"type": "error", "message": err.Error()})
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

	writeSSE(w, map[string]interface{}{
		"type": "error",
		"message":       "timed out waiting for run to complete",
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
	jsonResponse(w, map[string]interface{}{
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
	if err := s.crdClient.List(r.Context(), &list, nsListOpts(ns)...); err != nil {
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
		jsonResponse(w, map[string]interface{}{
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
	if err := s.crdClient.List(r.Context(), &list, nsListOpts(ns)...); err != nil {
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
	if err := s.crdClient.List(r.Context(), &list, nsListOpts(ns)...); err != nil {
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
	if err := s.crdClient.List(r.Context(), &list, nsListOpts(ns)...); err != nil {
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
	if err := s.crdClient.List(r.Context(), &list, nsListOpts(ns)...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type summary struct {
		Name               string   `json:"name"`
		Namespace          string   `json:"namespace"`
		Description        string   `json:"description,omitempty"`
		AllowedAgents      []string `json:"allowedAgents"`
		Ready              bool     `json:"ready"`
		DocumentCount      int      `json:"documentCount"`
		ChunkCount         int      `json:"chunkCount"`
		VectorStoreURL     string   `json:"vectorStoreURL,omitempty"`
		CollectionName     string   `json:"collectionName,omitempty"`
		ModelSelectorRef   string   `json:"modelSelectorRef"`
		Dimensions         int      `json:"dimensions"`
		ChunkSize          int      `json:"chunkSize"`
		ChunkOverlap       int      `json:"chunkOverlap"`
		StorageUsedPercent int      `json:"storageUsedPercent"`
		LastSyncTime       string   `json:"lastSyncTime,omitempty"`
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
	if err := s.crdClient.List(r.Context(), &list, nsListOpts(ns)...); err != nil {
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
	if err := s.crdClient.List(r.Context(), &list, nsListOpts(ns)...); err != nil {
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
	}
	out := make([]summary, 0, len(list.Items))
	for _, d := range list.Items {
		s := summary{
			Name:          d.Name,
			Namespace:     d.Namespace,
			AgentRef:      d.Spec.AgentRef,
			Phase:         string(d.Status.Phase),
			ReadyReplicas: d.Status.ReadyReplicas,
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
	opts := nsListOpts(ns)
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
		ByDay    []interface{}     `json:"byDay"`
	}
	out := costData{
		TotalUSD: fmt.Sprintf("%.4f", total),
		ByAgent:  make(map[string]string),
		ByModel:  make(map[string]string),
		ByDay:    []interface{}{},
	}
	for k, v := range byAgent {
		out.ByAgent[k] = fmt.Sprintf("%.4f", v)
	}
	jsonResponse(w, out)
}

func writeSSE(w http.ResponseWriter, data interface{}) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func jsonResponse(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// handleSystemStatus returns operator-level feature flags for the UI.
func (s *UIServer) handleSystemStatus(w http.ResponseWriter, _ *http.Request) {
	jsonResponse(w, map[string]interface{}{
		"stateConfigured": s.stateConfigured,
	})
}

func parseFloatCost(s string) float64 {
	if s == "" {
		return 0
	}
	var f float64
	fmt.Sscanf(s, "%f", &f)
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
		priorCheckpointKey := fmt.Sprintf("agentorc/runs/%s/state", priorRunRef)
		var msgs []json.RawMessage
		for _, cm := range cp.ConversationHistory {
			routerMsg := map[string]interface{}{
				"role":    cm.Role,
				"content": cm.Content,
			}
			raw, _ := json.Marshal(routerMsg)
			msgs = append(msgs, raw)
		}
		s.store.SaveMessages(r.Context(), priorCheckpointKey, msgs, 24*time.Hour)
	}

	jsonResponse(w, map[string]interface{}{
		"runName":   run.Name,
		"sessionId": sessionID,
	})
}

// handleChatHistory returns the conversation history for a session.
// GET /api/deployments/{namespace}/{name}/history?sessionId=X
func (s *UIServer) handleChatHistory(w http.ResponseWriter, r *http.Request, namespace, deploymentName string) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		jsonResponse(w, map[string]interface{}{"messages": []interface{}{}})
		return
	}

	cp, err := s.checkpoint.Load(r.Context(), sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("loading checkpoint: %v", err), http.StatusInternalServerError)
		return
	}
	if cp == nil {
		jsonResponse(w, map[string]interface{}{"messages": []interface{}{}})
		return
	}

	jsonResponse(w, map[string]interface{}{
		"sessionId": cp.SessionID,
		"messages":  cp.ConversationHistory,
	})
}

// handleSaveResponse saves an assistant response to the session checkpoint.
// POST /api/deployments/{namespace}/{name}/complete
func (s *UIServer) handleSaveResponse(w http.ResponseWriter, r *http.Request, namespace, deploymentName string) {
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
			routerMsg := map[string]interface{}{
				"role":    cm.Role,
				"content": cm.Content,
			}
			raw, _ := json.Marshal(routerMsg)
			msgs = append(msgs, raw)
		}
		s.store.SaveMessages(r.Context(), checkpointKey, msgs, 24*time.Hour)
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
	writeSSE(w, map[string]interface{}{
		"type": "placeholder",
		"message":       "[Streaming implementation pending]",
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
		Name                 string `json:"name"`
		Namespace            string `json:"namespace"`
		AgentRef             string `json:"agentRef"`
		Phase                string `json:"phase"`
		ReadyReplicas        int32  `json:"readyReplicas"`
		AvailableReplicas    int32  `json:"availableReplicas"`
		ConsecutiveFailures  int    `json:"consecutiveFailures"`
		InputSourceType      string `json:"inputSourceType"`
		LastUpdateTime       string `json:"lastUpdateTime,omitempty"`
		Message              string `json:"message,omitempty"`
		ContextUsedTokens    int    `json:"contextUsedTokens"`
		MaxContextTokens     int    `json:"maxContextTokens"`
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
	if err := s.crdClient.List(r.Context(), &list, nsListOpts(ns)...); err != nil {
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
