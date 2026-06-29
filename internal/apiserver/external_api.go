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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
	"github.com/floppyfish14/agent-orc/internal/security"
	"github.com/floppyfish14/agent-orc/internal/state"
)

// ExternalAPIServer handles the external-facing REST API for enterprise integrations.
// Port 8084, authenticated via ExternalAuth middleware.
type ExternalAPIServer struct {
	k8s       kubernetes.Interface
	crdClient client.Client
	auth      *ExternalAuth
	store     state.Store
	hasStore  bool
}

// NewExternalAPIServer creates a new external API server.
func NewExternalAPIServer(k8s kubernetes.Interface, crdClient client.Client, auth *ExternalAuth, hasStore bool, store state.Store) *ExternalAPIServer {
	return &ExternalAPIServer{
		k8s:       k8s,
		crdClient: crdClient,
		auth:      auth,
		store:     store,
		hasStore:  hasStore,
	}
}

// Handler returns an http.Handler for the external API.
func (s *ExternalAPIServer) Handler() http.Handler {
	mux := http.NewServeMux()

	// Token endpoint (unauthenticated).
	mux.HandleFunc("/oauth/token", s.auth.HandleTokenRequest)

	// Task endpoints (authenticated via middleware).
	mux.HandleFunc("/v1/tasks", s.handleTasks)
	mux.HandleFunc("/v1/tasks/", s.handleTaskByID)

	return corsMiddleware(s.auth.Middleware(mux))
}

// TaskSubmission is the request body for POST /v1/tasks.
type TaskSubmission struct {
	Agent    string            `json:"agent"`
	Input    string            `json:"input"`
	Timeout  string            `json:"timeout,omitempty"`
	Callback *TaskCallback     `json:"callback,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// TaskCallback configures webhook delivery on task completion.
type TaskCallback struct {
	URL       string `json:"url"`
	SecretRef string `json:"secretRef,omitempty"`
}

// TaskResponse is the response body for task operations.
type TaskResponse struct {
	ID          string            `json:"id"`
	Agent       string            `json:"agent"`
	Status      string            `json:"status"`
	Output      string            `json:"output,omitempty"`
	SpendUSD    string            `json:"spendUSD,omitempty"`
	CreatedAt   string            `json:"createdAt,omitempty"`
	CompletedAt string            `json:"completedAt,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Links       *TaskLinks        `json:"links,omitempty"`
}

// TaskLinks provides HATEOAS-style links for task navigation.
type TaskLinks struct {
	Self   string `json:"self"`
	Stream string `json:"stream,omitempty"`
}

// handleTasks dispatches GET (list) and POST (create) for /v1/tasks.
func (s *ExternalAPIServer) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.createTask(w, r)
	case http.MethodGet:
		s.listTasks(w, r)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// handleTaskByID dispatches requests for /v1/tasks/{id} and sub-resources.
func (s *ExternalAPIServer) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	// Path: /v1/tasks/{id} or /v1/tasks/{id}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/v1/tasks/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, `{"error":"task ID required"}`, http.StatusBadRequest)
		return
	}
	taskID := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch {
	case r.Method == http.MethodGet && action == "":
		s.getTask(w, r, taskID)
	case r.Method == http.MethodGet && action == "stream":
		s.streamTask(w, r, taskID)
	case r.Method == http.MethodPost && action == "answer":
		s.answerTask(w, r, taskID)
	case r.Method == http.MethodDelete && action == "":
		s.cancelTask(w, r, taskID)
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

// createTask handles POST /v1/tasks.
func (s *ExternalAPIServer) createTask(w http.ResponseWriter, r *http.Request) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		http.Error(w, `{"error":"no tenant identity"}`, http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, `{"error":"reading body"}`, http.StatusBadRequest)
		return
	}

	var req TaskSubmission
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"invalid JSON: %s"}`, err), http.StatusBadRequest)
		return
	}

	if req.Agent == "" || req.Input == "" {
		http.Error(w, `{"error":"agent and input are required"}`, http.StatusBadRequest)
		return
	}

	// Check if the tenant is allowed to invoke this agent.
	if tenant.AllowedAgents != nil && !slices.Contains(tenant.AllowedAgents, req.Agent) {
		http.Error(w, fmt.Sprintf(`{"error":"agent %q is not allowed for this tenant"}`, req.Agent), http.StatusForbidden)
		return
	}

	// Verify the agent exists in the target namespace.
	var agent agentorcv1alpha1.Agent
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{
		Name: req.Agent, Namespace: tenant.Namespace,
	}, &agent); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"agent %q not found in namespace %q"}`, req.Agent, tenant.Namespace), http.StatusNotFound)
		return
	}

	// Build the AgentRun CR.
	run := &agentorcv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("task-%s-", req.Agent),
			Namespace:    tenant.Namespace,
			Labels: map[string]string{
				security.LabelManagedBy:     security.ManagedByValue,
				"agentorc.io/external-task": "true",
				"agentorc.io/tenant":        tenant.TenantName,
			},
		},
		Spec: agentorcv1alpha1.AgentRunSpec{
			AgentRef: req.Agent,
			Input:    req.Input,
		},
	}

	// Set correlation ID from metadata.
	if req.Metadata != nil {
		if run.Annotations == nil {
			run.Annotations = make(map[string]string)
		}
		if cid, ok := req.Metadata["correlationId"]; ok {
			run.Labels["agentorc.io/correlation-id"] = cid
		}
		// Store all metadata as annotations.
		for k, v := range req.Metadata {
			run.Annotations["agentorc.io/meta-"+k] = v
		}
	}

	// Set timeout.
	if req.Timeout != "" {
		d, err := time.ParseDuration(req.Timeout)
		if err == nil {
			run.Spec.Timeout = &metav1.Duration{Duration: d}
		}
	}

	// Set callbacks.
	if req.Callback != nil && req.Callback.URL != "" {
		run.Spec.Callbacks = &agentorcv1alpha1.CallbackConfig{
			OnComplete: req.Callback.URL,
			OnFailed:   req.Callback.URL,
		}
		if req.Callback.SecretRef != "" {
			if run.Annotations == nil {
				run.Annotations = make(map[string]string)
			}
			run.Annotations["agentorc.io/callback-secret"] = req.Callback.SecretRef
		}
	}

	if err := s.crdClient.Create(r.Context(), run); err != nil {
		slog.Error("creating task AgentRun", "err", err)
		http.Error(w, fmt.Sprintf(`{"error":"creating task: %s"}`, err), http.StatusInternalServerError)
		return
	}

	slog.Info("External task created", "task", run.Name, "agent", req.Agent, "tenant", tenant.TenantName)

	resp := TaskResponse{
		ID:        run.Name,
		Agent:     req.Agent,
		Status:    string(agentorcv1alpha1.AgentRunPhasePending),
		CreatedAt: run.CreationTimestamp.Format(time.RFC3339),
		Metadata:  req.Metadata,
		Links: &TaskLinks{
			Self:   "/v1/tasks/" + run.Name,
			Stream: "/v1/tasks/" + run.Name + "/stream",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

// getTask handles GET /v1/tasks/{id}.
func (s *ExternalAPIServer) getTask(w http.ResponseWriter, r *http.Request, taskID string) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		http.Error(w, `{"error":"no tenant identity"}`, http.StatusUnauthorized)
		return
	}

	var run agentorcv1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{
		Name: taskID, Namespace: tenant.Namespace,
	}, &run); err != nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	// Verify the run belongs to this tenant.
	if run.Labels["agentorc.io/tenant"] != tenant.TenantName {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	resp := s.runToTaskResponse(&run)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// listTasks handles GET /v1/tasks?agent=X&status=Y.
func (s *ExternalAPIServer) listTasks(w http.ResponseWriter, r *http.Request) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		http.Error(w, `{"error":"no tenant identity"}`, http.StatusUnauthorized)
		return
	}

	labels := client.MatchingLabels{
		"agentorc.io/external-task": "true",
		"agentorc.io/tenant":        tenant.TenantName,
	}

	var runList agentorcv1alpha1.AgentRunList
	if err := s.crdClient.List(r.Context(), &runList,
		client.InNamespace(tenant.Namespace), labels); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"listing tasks: %s"}`, err), http.StatusInternalServerError)
		return
	}

	// Filter by query params.
	agentFilter := r.URL.Query().Get("agent")
	statusFilter := r.URL.Query().Get("status")

	var tasks []TaskResponse
	for i := range runList.Items {
		run := &runList.Items[i]
		if agentFilter != "" && run.Spec.AgentRef != agentFilter {
			continue
		}
		if statusFilter != "" && string(run.Status.Phase) != statusFilter {
			continue
		}
		tasks = append(tasks, s.runToTaskResponse(run))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"tasks": tasks,
		"count": len(tasks),
	})
}

// streamTask handles GET /v1/tasks/{id}/stream (SSE).
func (s *ExternalAPIServer) streamTask(w http.ResponseWriter, r *http.Request, taskID string) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		http.Error(w, `{"error":"no tenant identity"}`, http.StatusUnauthorized)
		return
	}

	var run agentorcv1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{
		Name: taskID, Namespace: tenant.Namespace,
	}, &run); err != nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	if run.Labels["agentorc.io/tenant"] != tenant.TenantName {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	if !s.hasStore || s.store == nil {
		http.Error(w, `{"error":"streaming not available (no state store)"}`, http.StatusServiceUnavailable)
		return
	}

	// Set SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}

	// Emit initial status event.
	fmt.Fprintf(w, "event: status\ndata: %s\n\n", mustJSON(map[string]string{
		"phase": string(run.Status.Phase),
	}))
	flusher.Flush()

	// Subscribe to the Redis token stream.
	streamKey := fmt.Sprintf("tokens:%s:%s", tenant.Namespace, taskID)
	tokenCh, err := s.store.TailTokens(r.Context(), streamKey)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", mustJSON(map[string]string{
			"error": "failed to subscribe to token stream",
		}))
		flusher.Flush()
		return
	}

	for token := range tokenCh {
		if token == "" {
			// Empty token is the done sentinel.
			break
		}
		fmt.Fprintf(w, "event: token\ndata: %s\n\n", mustJSON(map[string]string{
			"content": token,
		}))
		flusher.Flush()
	}

	// Re-fetch the run for final status.
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{
		Name: taskID, Namespace: tenant.Namespace,
	}, &run); err == nil {
		resp := s.runToTaskResponse(&run)
		fmt.Fprintf(w, "event: complete\ndata: %s\n\n", mustJSON(resp))
		flusher.Flush()
	}
}

// answerTask handles POST /v1/tasks/{id}/answer for clarification responses.
func (s *ExternalAPIServer) answerTask(w http.ResponseWriter, r *http.Request, taskID string) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		http.Error(w, `{"error":"no tenant identity"}`, http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, `{"error":"reading body"}`, http.StatusBadRequest)
		return
	}

	var req struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Answer == "" {
		http.Error(w, `{"error":"answer is required"}`, http.StatusBadRequest)
		return
	}

	var run agentorcv1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{
		Name: taskID, Namespace: tenant.Namespace,
	}, &run); err != nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	if run.Labels["agentorc.io/tenant"] != tenant.TenantName {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	if run.Status.Phase != agentorcv1alpha1.AgentRunPhaseWaitingForInput {
		http.Error(w, `{"error":"task is not waiting for input"}`, http.StatusConflict)
		return
	}

	// Store the answer in the state store for the model-router to pick up.
	if s.hasStore && s.store != nil {
		answerKey := fmt.Sprintf("answer:%s:%s", tenant.Namespace, taskID)
		if err := s.store.SaveAnswer(r.Context(), answerKey, req.Answer, 24*time.Hour); err != nil {
			slog.Warn("failed to save answer to state store", "task", taskID, "err", err)
		}
	}

	// Update the AgentRun status with the answer.
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.ClarifyAnswer = req.Answer
	if err := s.crdClient.Status().Patch(r.Context(), &run, patch); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"updating task: %s"}`, err), http.StatusInternalServerError)
		return
	}

	slog.Info("Clarification answer submitted", "task", taskID, "tenant", tenant.TenantName)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
}

// cancelTask handles DELETE /v1/tasks/{id}.
func (s *ExternalAPIServer) cancelTask(w http.ResponseWriter, r *http.Request, taskID string) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		http.Error(w, `{"error":"no tenant identity"}`, http.StatusUnauthorized)
		return
	}

	var run agentorcv1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{
		Name: taskID, Namespace: tenant.Namespace,
	}, &run); err != nil {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	if run.Labels["agentorc.io/tenant"] != tenant.TenantName {
		http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
		return
	}

	// Only cancel running or pending tasks.
	if run.Status.Phase != agentorcv1alpha1.AgentRunPhasePending &&
		run.Status.Phase != agentorcv1alpha1.AgentRunPhaseRunning &&
		run.Status.Phase != agentorcv1alpha1.AgentRunPhaseWaitingForInput {
		http.Error(w, `{"error":"task is already in a terminal state"}`, http.StatusConflict)
		return
	}

	// Mark the run as failed with a cancellation reason.
	patch := client.MergeFrom(run.DeepCopy())
	now := metav1.Now()
	run.Status.Phase = agentorcv1alpha1.AgentRunPhaseFailed
	run.Status.FailureReason = "cancelled by tenant via external API"
	run.Status.CompletionTime = &now
	if err := s.crdClient.Status().Patch(r.Context(), &run, patch); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"cancelling task: %s"}`, err), http.StatusInternalServerError)
		return
	}

	// Signal cancellation via Redis so the model-router sidecar aborts in-flight
	// LLM requests within ~1 second, rather than waiting for the next call.
	if s.store != nil {
		_ = s.store.SignalCancel(r.Context(), run.Namespace, run.Name)
	}

	slog.Info("Task cancelled", "task", taskID, "tenant", tenant.TenantName)
	w.WriteHeader(http.StatusNoContent)
}

// runToTaskResponse converts an AgentRun to a TaskResponse.
func (s *ExternalAPIServer) runToTaskResponse(run *agentorcv1alpha1.AgentRun) TaskResponse {
	resp := TaskResponse{
		ID:       run.Name,
		Agent:    run.Spec.AgentRef,
		Status:   string(run.Status.Phase),
		Output:   run.Status.Output,
		SpendUSD: run.Status.SpendUSD,
		Links: &TaskLinks{
			Self:   "/v1/tasks/" + run.Name,
			Stream: "/v1/tasks/" + run.Name + "/stream",
		},
	}
	if !run.CreationTimestamp.IsZero() {
		resp.CreatedAt = run.CreationTimestamp.Format(time.RFC3339)
	}
	if run.Status.CompletionTime != nil {
		resp.CompletedAt = run.Status.CompletionTime.Format(time.RFC3339)
	}
	// Reconstruct metadata from annotations.
	meta := make(map[string]string)
	for k, v := range run.Annotations {
		if strings.HasPrefix(k, "agentorc.io/meta-") {
			meta[strings.TrimPrefix(k, "agentorc.io/meta-")] = v
		}
	}
	if len(meta) > 0 {
		resp.Metadata = meta
	}
	return resp
}

// mustJSON marshals v to JSON, returning "{}" on error.
func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
