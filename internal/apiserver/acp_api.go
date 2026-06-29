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
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
	"github.com/floppyfish14/agent-orc/internal/security"
	"github.com/floppyfish14/agent-orc/internal/state"
)

// ACPServer implements the Agent Communication Protocol (ACP) API.
// ACP is a standardized RESTful API for agent interaction.
type ACPServer struct {
	k8s       kubernetes.Interface
	crdClient client.Client
	auth      *ExternalAuth
	store     state.Store
}

// NewACPServer creates a new ACP server.
func NewACPServer(k8s kubernetes.Interface, crdClient client.Client, auth *ExternalAuth, store state.Store) *ACPServer {
	return &ACPServer{
		k8s:       k8s,
		crdClient: crdClient,
		auth:      auth,
		store:     store,
	}
}

// Handler returns an http.Handler for the ACP API.
func (s *ACPServer) Handler() http.Handler {
	mux := http.NewServeMux()

	// Token endpoint (unauthenticated) - needed for client_credentials flow.
	mux.HandleFunc("/oauth/token", s.auth.HandleTokenRequest)

	mux.HandleFunc("/ping", s.handlePing)
	mux.HandleFunc("/agents", s.handleListAgents)
	mux.HandleFunc("/agents/", s.handleAgentManifest)
	mux.HandleFunc("/runs", s.handleCreateRun)
	mux.HandleFunc("/runs/", s.handleRunByID)
	mux.HandleFunc("/session/", s.handleGetSession)
	return corsMiddleware(s.auth.Middleware(mux))
}

// ACP types aligned with ACP 0.2.0 spec from https://agentcommunicationprotocol.dev

// ACPMessagePart represents a part of a message per ACP spec.
type ACPMessagePart struct {
	Name            string              `json:"name,omitempty"`
	ContentType     string              `json:"content_type"`
	Content         string              `json:"content,omitempty"`
	ContentEncoding string              `json:"content_encoding,omitempty"`
	ContentURL      string              `json:"content_url,omitempty"`
	Metadata        *ACPMessageMetadata `json:"metadata,omitempty"`
}

// ACPMessageMetadata represents citation or trajectory metadata.
type ACPMessageMetadata struct {
	Kind string `json:"kind"` // "citation" or "trajectory"
	// Citation fields
	StartIndex  *int   `json:"start_index,omitempty"`
	EndIndex    *int   `json:"end_index,omitempty"`
	URL         string `json:"url,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// Trajectory fields
	Message    string      `json:"message,omitempty"`
	ToolName   string      `json:"tool_name,omitempty"`
	ToolInput  interface{} `json:"tool_input,omitempty"`
	ToolOutput interface{} `json:"tool_output,omitempty"`
}

// ACPMessage represents a message with parts per ACP spec.
type ACPMessage struct {
	Role        string           `json:"role"`
	Parts       []ACPMessagePart `json:"parts"`
	CreatedAt   string           `json:"created_at,omitempty"`
	CompletedAt string           `json:"completed_at,omitempty"`
}

// ACPRunStatus matches ACP RunStatus enum.
type ACPRunStatus string

const (
	ACPRunCreated    ACPRunStatus = "created"
	ACPRunInProgress ACPRunStatus = "in-progress"
	ACPRunAwaiting   ACPRunStatus = "awaiting"
	ACPRunCancelling ACPRunStatus = "cancelling"
	ACPRunCancelled  ACPRunStatus = "cancelled"
	ACPRunCompleted  ACPRunStatus = "completed"
	ACPRunFailed     ACPRunStatus = "failed"
)

// ACPRunMode matches ACP RunMode enum.
type ACPRunMode string

const (
	ACPRunModeSync   ACPRunMode = "sync"
	ACPRunModeAsync  ACPRunMode = "async"
	ACPRunModeStream ACPRunMode = "stream"
)

// ACPSession represents a session per ACP spec.
type ACPSession struct {
	ID      string   `json:"id"`
	History []string `json:"history"`
	State   string   `json:"state,omitempty"`
}

// ACPRunCreateRequest is the request body for POST /runs.
type ACPRunCreateRequest struct {
	AgentName string       `json:"agent_name"`
	Input     []ACPMessage `json:"input"`
	SessionID string       `json:"session_id,omitempty"`
	Session   *ACPSession  `json:"session,omitempty"`
	Mode      ACPRunMode   `json:"mode,omitempty"`
}

// ACPAwaitRequest describes what is awaited from the client.
type ACPAwaitRequest struct {
	// Extend with actual fields as needed
}

// ACPRun is the response body for run operations.
type ACPRun struct {
	AgentName    string           `json:"agent_name"`
	SessionID    string           `json:"session_id,omitempty"`
	RunID        string           `json:"run_id"`
	Status       ACPRunStatus     `json:"status"`
	AwaitRequest *ACPAwaitRequest `json:"await_request,omitempty"`
	Output       []ACPMessage     `json:"output,omitempty"`
	Error        *ACPErr          `json:"error,omitempty"`
	CreatedAt    string           `json:"created_at"`
	FinishedAt   string           `json:"finished_at,omitempty"`
}

// ACPAgentStatus represents agent status metrics per spec.
type ACPAgentStatus struct {
	AvgRunTokens      *float64 `json:"avg_run_tokens,omitempty"`
	AvgRunTimeSeconds *float64 `json:"avg_run_time_seconds,omitempty"`
	SuccessRate       *float64 `json:"success_rate,omitempty"`
}

// ACPAgentMetadata represents agent metadata per spec.
type ACPAgentMetadata struct {
	Annotations         map[string]interface{} `json:"annotations,omitempty"`
	Documentation       string                 `json:"documentation,omitempty"`
	License             string                 `json:"license,omitempty"`
	ProgrammingLanguage string                 `json:"programming_language,omitempty"`
	NaturalLanguages    []string               `json:"natural_languages,omitempty"`
	Framework           string                 `json:"framework,omitempty"`
}

// ACPAgentManifest is the response body for GET /agents/{name}.
type ACPAgentManifest struct {
	Name               string            `json:"name"`
	Description        string            `json:"description"`
	InputContentTypes  []string          `json:"input_content_types"`
	OutputContentTypes []string          `json:"output_content_types"`
	Metadata           *ACPAgentMetadata `json:"metadata,omitempty"`
	Status             *ACPAgentStatus   `json:"status,omitempty"`
}

// ACPAgentsListResponse is the response body for GET /agents.
type ACPAgentsListResponse struct {
	Agents []ACPAgentManifest `json:"agents"`
}

// ACPErr represents an error response.
type ACPErr struct {
	Code    string      `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// ACP Event types for SSE streaming

// ACPEvent is the base event type.
type ACPEvent struct {
	Type string `json:"type"`
}

// ACPMessageCreatedEvent is emitted when a new message is created.
type ACPMessageCreatedEvent struct {
	Type    string     `json:"type"`
	Message ACPMessage `json:"message"`
}

// ACPMessagePartEvent is emitted when a message part is streamed.
type ACPMessagePartEvent struct {
	Type string         `json:"type"`
	Part ACPMessagePart `json:"part"`
}

// ACPMessageCompletedEvent is emitted when a message is complete.
type ACPMessageCompletedEvent struct {
	Type    string     `json:"type"`
	Message ACPMessage `json:"message"`
}

// ACPGenericEvent is for generic events.
type ACPGenericEvent struct {
	Type    string      `json:"type"`
	Generic interface{} `json:"generic"`
}

// ACPRunCreatedEvent is emitted when a run is created.
type ACPRunCreatedEvent struct {
	Type string `json:"type"`
	Run  ACPRun `json:"run"`
}

// ACPRunInProgressEvent is emitted when a run starts processing.
type ACPRunInProgressEvent struct {
	Type string `json:"type"`
	Run  ACPRun `json:"run"`
}

// ACPRunAwaitingEvent is emitted when a run is waiting for input.
type ACPRunAwaitingEvent struct {
	Type string `json:"type"`
	Run  ACPRun `json:"run"`
}

// ACPRunCompletedEvent is emitted when a run completes.
type ACPRunCompletedEvent struct {
	Type string `json:"type"`
	Run  ACPRun `json:"run"`
}

// ACPRunCancelledEvent is emitted when a run is cancelled.
type ACPRunCancelledEvent struct {
	Type string `json:"type"`
	Run  ACPRun `json:"run"`
}

// ACPRunFailedEvent is emitted when a run fails.
type ACPRunFailedEvent struct {
	Type string `json:"type"`
	Run  ACPRun `json:"run"`
}

// ACPErrEvent is emitted on errors.
type ACPErrEvent struct {
	Type  string `json:"type"`
	Error ACPErr `json:"error"`
}

// handlePing returns a simple ping response.
func (s *ACPServer) handlePing(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, map[string]interface{}{"status": "ok"})
}

// parseIntQueryParam parses an integer query parameter with a default value.
func parseIntQueryParam(r *http.Request, name string, defaultValue int) int {
	val := r.URL.Query().Get(name)
	if val == "" {
		return defaultValue
	}
	var result int
	if _, err := fmt.Sscanf(val, "%d", &result); err != nil {
		return defaultValue
	}
	return result
}

// handleListAgents returns all agents in the tenant namespace.
func (s *ACPServer) handleListAgents(w http.ResponseWriter, r *http.Request) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		writeACPError(w, "unauthorized", "no tenant identity", http.StatusUnauthorized)
		return
	}

	// Parse limit and offset query parameters (ACP spec compliant)
	limit := parseIntQueryParam(r, "limit", 10)
	if limit < 1 {
		limit = 10
	}
	if limit > 1000 {
		limit = 1000
	}
	offset := parseIntQueryParam(r, "offset", 0)
	if offset < 0 {
		offset = 0
	}

	var agentList agentorcv1alpha1.AgentList
	if err := s.crdClient.List(r.Context(), &agentList, client.InNamespace(tenant.Namespace)); err != nil {
		writeACPError(w, "server_error", err.Error(), http.StatusInternalServerError)
		return
	}

	// Filter by allowed agents if specified
	var filteredAgents []agentorcv1alpha1.Agent
	for _, agent := range agentList.Items {
		if tenant.AllowedAgents != nil && len(tenant.AllowedAgents) > 0 {
			found := false
			for _, allowed := range tenant.AllowedAgents {
				if allowed == agent.Name {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		filteredAgents = append(filteredAgents, agent)
	}

	// Apply offset and limit
	if offset > len(filteredAgents) {
		offset = len(filteredAgents)
	}
	end := offset + limit
	if end > len(filteredAgents) {
		end = len(filteredAgents)
	}

	agents := make([]ACPAgentManifest, 0, end-offset)
	for _, agent := range filteredAgents[offset:end] {
		agents = append(agents, ACPAgentManifest{
			Name:               agent.Name,
			Description:        agent.Spec.SystemPrompt,
			InputContentTypes:  []string{"text/plain", "application/json"},
			OutputContentTypes: []string{"text/plain", "application/json"},
		})
	}

	jsonResponse(w, ACPAgentsListResponse{Agents: agents})
}

// handleAgentManifest returns the manifest for a specific agent.
func (s *ACPServer) handleAgentManifest(w http.ResponseWriter, r *http.Request) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		writeACPError(w, "unauthorized", "no tenant identity", http.StatusUnauthorized)
		return
	}

	// Extract agent name from path: /agents/{name}
	name := strings.TrimPrefix(r.URL.Path, "/agents/")
	if name == "" {
		writeACPError(w, "invalid_input", "agent name required", http.StatusBadRequest)
		return
	}

	var agent agentorcv1alpha1.Agent
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: tenant.Namespace}, &agent); err != nil {
		writeACPError(w, "not_found", fmt.Sprintf("agent %q not found", name), http.StatusNotFound)
		return
	}

	// Check if tenant is allowed to access this agent
	if tenant.AllowedAgents != nil && len(tenant.AllowedAgents) > 0 {
		found := false
		for _, allowed := range tenant.AllowedAgents {
			if allowed == name {
				found = true
				break
			}
		}
		if !found {
			writeACPError(w, "unauthorized", "agent not allowed for tenant", http.StatusForbidden)
			return
		}
	}

	manifest := ACPAgentManifest{
		Name:               agent.Name,
		Description:        agent.Spec.SystemPrompt,
		InputContentTypes:  []string{"text/plain", "application/json"},
		OutputContentTypes: []string{"text/plain", "application/json"},
	}
	jsonResponse(w, manifest)
}

// handleCreateRun creates and starts a new agent run.
func (s *ACPServer) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		writeACPError(w, "unauthorized", "no tenant identity", http.StatusUnauthorized)
		return
	}

	if r.Method != http.MethodPost {
		writeACPError(w, "invalid_input", "POST required", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeACPError(w, "invalid_input", "reading body", http.StatusBadRequest)
		return
	}

	var req ACPRunCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeACPError(w, "invalid_input", fmt.Sprintf("invalid JSON: %s", err), http.StatusBadRequest)
		return
	}

	if req.AgentName == "" {
		writeACPError(w, "invalid_input", "agent_name is required", http.StatusBadRequest)
		return
	}

	if len(req.Input) == 0 {
		writeACPError(w, "invalid_input", "input is required", http.StatusBadRequest)
		return
	}

	// Check if tenant is allowed to access this agent
	if tenant.AllowedAgents != nil && len(tenant.AllowedAgents) > 0 {
		found := false
		for _, allowed := range tenant.AllowedAgents {
			if allowed == req.AgentName {
				found = true
				break
			}
		}
		if !found {
			writeACPError(w, "unauthorized", "agent not allowed for tenant", http.StatusForbidden)
			return
		}
	}

	// Verify the agent exists
	var agent agentorcv1alpha1.Agent
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: req.AgentName, Namespace: tenant.Namespace}, &agent); err != nil {
		writeACPError(w, "not_found", fmt.Sprintf("agent %q not found", req.AgentName), http.StatusNotFound)
		return
	}

	// Extract input text from ACP messages
	var inputText string
	for _, msg := range req.Input {
		for _, part := range msg.Parts {
			if part.Content != "" {
				inputText += part.Content + "\n"
			}
		}
	}
	inputText = strings.TrimSpace(inputText)

	// Determine session ID
	sessionID := req.SessionID
	if req.Session != nil && req.Session.ID != "" {
		sessionID = req.Session.ID
	}

	// Create AgentRun CR
	run := &agentorcv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("acp-%s-", req.AgentName),
			Namespace:    tenant.Namespace,
			Labels: map[string]string{
				security.LabelManagedBy: "agent-orc",
				"agentorc.io/tenant":    tenant.TenantName,
			},
		},
		Spec: agentorcv1alpha1.AgentRunSpec{
			AgentRef: req.AgentName,
			Input:    inputText,
		},
	}
	if sessionID != "" {
		run.Labels["agentorc.io/session-id"] = sessionID
	}

	if err := s.crdClient.Create(r.Context(), run); err != nil {
		slog.Error("creating ACP run", "err", err)
		writeACPError(w, "server_error", fmt.Sprintf("creating run: %s", err), http.StatusInternalServerError)
		return
	}

	slog.Info("ACP run created", "run", run.Name, "agent", req.AgentName, "tenant", tenant.TenantName)

	// Return ACP run object with RFC3339 timestamp per spec
	resp := ACPRun{
		AgentName: req.AgentName,
		SessionID: sessionID,
		RunID:     run.Name,
		Status:    ACPRunCreated,
		CreatedAt: run.CreationTimestamp.Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

// handleRunByID handles GET and POST for /runs/{run_id}.
func (s *ACPServer) handleRunByID(w http.ResponseWriter, r *http.Request) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		writeACPError(w, "unauthorized", "no tenant identity", http.StatusUnauthorized)
		return
	}

	// Extract run ID and potential sub-path from path
	path := strings.TrimPrefix(r.URL.Path, "/runs/")
	parts := strings.Split(path, "/")
	runID := parts[0]

	if runID == "" {
		writeACPError(w, "invalid_input", "run_id required", http.StatusBadRequest)
		return
	}

	// Handle /runs/{run_id}/cancel
	if len(parts) >= 2 && parts[1] == "cancel" {
		s.handleCancelRun(w, r, tenant, runID)
		return
	}

	// Handle /runs/{run_id}/events
	if len(parts) >= 2 && parts[1] == "events" {
		s.handleListRunEvents(w, r, tenant, runID)
		return
	}

	var run agentorcv1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: runID, Namespace: tenant.Namespace}, &run); err != nil {
		writeACPError(w, "not_found", "run not found", http.StatusNotFound)
		return
	}

	// Verify run belongs to tenant
	if run.Labels["agentorc.io/tenant"] != tenant.TenantName {
		writeACPError(w, "not_found", "run not found", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getACPRun(w, r, &run)
	case http.MethodPost:
		s.resumeACPRun(w, r, &run)
	default:
		writeACPError(w, "invalid_input", "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleCancelRun handles POST /runs/{run_id}/cancel.
func (s *ACPServer) handleCancelRun(w http.ResponseWriter, r *http.Request, tenant *TenantIdentity, runID string) {
	var run agentorcv1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: runID, Namespace: tenant.Namespace}, &run); err != nil {
		writeACPError(w, "not_found", "run not found", http.StatusNotFound)
		return
	}

	// Verify run belongs to tenant
	if run.Labels["agentorc.io/tenant"] != tenant.TenantName {
		writeACPError(w, "not_found", "run not found", http.StatusNotFound)
		return
	}

	// Only allow cancelling pending, running, or waiting runs
	switch run.Status.Phase {
	case agentorcv1alpha1.AgentRunPhasePending,
		agentorcv1alpha1.AgentRunPhaseRunning,
		agentorcv1alpha1.AgentRunPhaseWaitingForInput:
		// Valid states to cancel
	default:
		writeACPError(w, "invalid_input", "run is already in a terminal state", http.StatusConflict)
		return
	}

	// Mark as failed with cancellation reason (no dedicated Cancelled phase exists)
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.Phase = agentorcv1alpha1.AgentRunPhaseFailed
	run.Status.FailureReason = "cancelled by user"
	now := metav1.Now()
	run.Status.CompletionTime = &now
	if err := s.crdClient.Status().Patch(r.Context(), &run, patch); err != nil {
		writeACPError(w, "server_error", fmt.Sprintf("cancelling run: %s", err), http.StatusInternalServerError)
		return
	}

	slog.Info("ACP run cancelled", "run", runID, "tenant", tenant.TenantName)

	resp := ACPRun{
		AgentName:  run.Spec.AgentRef,
		SessionID:  run.Labels["agentorc.io/session-id"],
		RunID:      run.Name,
		Status:     ACPRunCancelled,
		CreatedAt:  run.CreationTimestamp.Format(time.RFC3339),
		FinishedAt: now.Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(resp)
}

// handleListRunEvents handles GET /runs/{run_id}/events.
func (s *ACPServer) handleListRunEvents(w http.ResponseWriter, r *http.Request, tenant *TenantIdentity, runID string) {
	var run agentorcv1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: runID, Namespace: tenant.Namespace}, &run); err != nil {
		writeACPError(w, "not_found", "run not found", http.StatusNotFound)
		return
	}

	// Verify run belongs to tenant
	if run.Labels["agentorc.io/tenant"] != tenant.TenantName {
		writeACPError(w, "not_found", "run not found", http.StatusNotFound)
		return
	}

	// Build events list based on run state using interface{} for polymorphism
	events := []interface{}{}

	// Run created event
	events = append(events, ACPRunCreatedEvent{
		Type: "run.created",
		Run: ACPRun{
			AgentName: run.Spec.AgentRef,
			RunID:     run.Name,
			Status:    ACPRunCreated,
			CreatedAt: run.CreationTimestamp.Format(time.RFC3339),
		},
	})

	// Run in-progress event
	events = append(events, ACPRunInProgressEvent{
		Type: "run.in-progress",
		Run: ACPRun{
			AgentName: run.Spec.AgentRef,
			RunID:     run.Name,
			Status:    ACPRunInProgress,
			CreatedAt: run.CreationTimestamp.Format(time.RFC3339),
		},
	})

	// Output message if run succeeded
	if run.Status.Phase == agentorcv1alpha1.AgentRunPhaseSucceeded && run.Status.Output != "" {
		events = append(events, ACPMessageCreatedEvent{
			Type: "message.created",
			Message: ACPMessage{
				Role: "agent",
				Parts: []ACPMessagePart{{
					ContentType: "text/plain",
					Content:     run.Status.Output,
				}},
			},
		})
		events = append(events, ACPMessageCompletedEvent{
			Type: "message.completed",
			Message: ACPMessage{
				Role: "agent",
				Parts: []ACPMessagePart{{
					ContentType: "text/plain",
					Content:     run.Status.Output,
				}},
			},
		})
		events = append(events, ACPRunCompletedEvent{
			Type: "run.completed",
			Run: ACPRun{
				AgentName: run.Spec.AgentRef,
				RunID:     run.Name,
				Status:    ACPRunCompleted,
				CreatedAt: run.CreationTimestamp.Format(time.RFC3339),
			},
		})
		// Set FinishedAt if CompletionTime is available
		if run.Status.CompletionTime != nil {
			lastEvent := events[len(events)-1].(ACPRunCompletedEvent)
			lastEvent.Run.FinishedAt = run.Status.CompletionTime.Format(time.RFC3339)
			events[len(events)-1] = lastEvent
		}
	} else if run.Status.Phase == agentorcv1alpha1.AgentRunPhaseFailed {
		events = append(events, ACPRunFailedEvent{
			Type: "run.failed",
			Run: ACPRun{
				AgentName: run.Spec.AgentRef,
				RunID:     run.Name,
				Status:    ACPRunFailed,
				CreatedAt: run.CreationTimestamp.Format(time.RFC3339),
			},
		})
	} else if run.Status.Phase == agentorcv1alpha1.AgentRunPhaseWaitingForInput {
		events = append(events, ACPRunAwaitingEvent{
			Type: "run.awaiting",
			Run: ACPRun{
				AgentName: run.Spec.AgentRef,
				RunID:     run.Name,
				Status:    ACPRunAwaiting,
				CreatedAt: run.CreationTimestamp.Format(time.RFC3339),
			},
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"events": events})
}

// handleGetSession handles GET /session/{session_id}.
func (s *ACPServer) handleGetSession(w http.ResponseWriter, r *http.Request) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		writeACPError(w, "unauthorized", "no tenant identity", http.StatusUnauthorized)
		return
	}

	if r.Method != http.MethodGet {
		writeACPError(w, "invalid_input", "GET required", http.StatusMethodNotAllowed)
		return
	}

	// Extract session ID from path
	sessionID := strings.TrimPrefix(r.URL.Path, "/session/")
	if sessionID == "" {
		writeACPError(w, "invalid_input", "session_id required", http.StatusBadRequest)
		return
	}

	// Find runs with this session ID to build session history
	labels := client.MatchingLabels{"agentorc.io/session-id": sessionID}
	var runList agentorcv1alpha1.AgentRunList
	if err := s.crdClient.List(r.Context(), &runList,
		client.InNamespace(tenant.Namespace), labels); err != nil {
		writeACPError(w, "server_error", err.Error(), http.StatusInternalServerError)
		return
	}

	// Build history URIs
	history := make([]string, 0, len(runList.Items))
	for _, run := range runList.Items {
		history = append(history, fmt.Sprintf("/runs/%s", run.Name))
	}

	session := ACPSession{
		ID:      sessionID,
		History: history,
	}
	jsonResponse(w, session)
}

// getACPRun returns the current run status.
func (s *ACPServer) getACPRun(w http.ResponseWriter, r *http.Request, run *agentorcv1alpha1.AgentRun) {
	acpStatus := mapAgentRunPhaseToACPStatus(run.Status.Phase)

	// Check if client wants streaming
	accept := r.Header.Get("Accept")
	if accept == "text/event-stream" && run.Status.Phase == agentorcv1alpha1.AgentRunPhaseRunning {
		s.streamACPRun(w, r, run)
		return
	}

	// Get session ID from labels if present
	sessionID := run.Labels["agentorc.io/session-id"]

	resp := ACPRun{
		AgentName: run.Spec.AgentRef,
		SessionID: sessionID,
		RunID:     run.Name,
		Status:    acpStatus,
		CreatedAt: run.CreationTimestamp.Format(time.RFC3339),
	}

	if run.Status.Phase == agentorcv1alpha1.AgentRunPhaseSucceeded && run.Status.Output != "" {
		resp.Output = []ACPMessage{{
			Role: "agent",
			Parts: []ACPMessagePart{{
				ContentType: "text/plain",
				Content:     run.Status.Output,
			}},
		}}
		if run.Status.CompletionTime != nil {
			resp.FinishedAt = run.Status.CompletionTime.Format(time.RFC3339)
		}
		resp.Status = ACPRunCompleted
	}

	if run.Status.Phase == agentorcv1alpha1.AgentRunPhaseFailed {
		resp.Status = ACPRunFailed
		resp.FinishedAt = time.Now().Format(time.RFC3339)
	}

	if run.Status.Phase == agentorcv1alpha1.AgentRunPhaseWaitingForInput {
		resp.Status = ACPRunAwaiting
		resp.AwaitRequest = &ACPAwaitRequest{}
	}

	jsonResponse(w, resp)
}

// streamACPRun streams tokens via SSE using ACP-compliant event format.
func (s *ACPServer) streamACPRun(w http.ResponseWriter, r *http.Request, run *agentorcv1alpha1.AgentRun) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeACPError(w, "server_error", "streaming not supported", http.StatusInternalServerError)
		return
	}

	if s.store == nil {
		writeACPError(w, "server_error", "streaming not available (no state store)", http.StatusServiceUnavailable)
		return
	}

	// Get session ID from labels if present
	sessionID := run.Labels["agentorc.io/session-id"]

	// Emit run.created event (ACP spec format)
	acpRun := ACPRun{
		AgentName: run.Spec.AgentRef,
		SessionID: sessionID,
		RunID:     run.Name,
		Status:    ACPRunInProgress,
		CreatedAt: run.CreationTimestamp.Format(time.RFC3339),
	}
	fmt.Fprintf(w, "event: run.created\ndata: %s\n\n", mustJSON(ACPRunCreatedEvent{Type: "run.created", Run: acpRun}))
	flusher.Flush()

	// Emit run.in-progress event
	fmt.Fprintf(w, "event: run.in-progress\ndata: %s\n\n", mustJSON(ACPRunInProgressEvent{Type: "run.in-progress", Run: acpRun}))
	flusher.Flush()

	// Stream tokens
	streamKey := fmt.Sprintf("tokens:%s:%s", run.Namespace, run.Name)
	tokenCh, err := s.store.TailTokens(r.Context(), streamKey)
	if err != nil {
		slog.Warn("failed to open token stream for ACP", "key", streamKey, "err", err)
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", mustJSON(ACPErrEvent{Type: "error", Error: ACPErr{Code: "server_error", Message: err.Error()}}))
		flusher.Flush()
		return
	}

	// Create message for output
	msg := ACPMessage{
		Role:  "agent",
		Parts: []ACPMessagePart{},
	}
	fmt.Fprintf(w, "event: message.created\ndata: %s\n\n", mustJSON(ACPMessageCreatedEvent{Type: "message.created", Message: msg}))
	flusher.Flush()

	var fullOutput strings.Builder
	for token := range tokenCh {
		if token == "" {
			break
		}
		// Skip trace events (prefixed with \x00)
		if len(token) > 1 && token[0] == '\x00' {
			continue
		}
		fullOutput.WriteString(token)
		part := ACPMessagePart{
			ContentType: "text/plain",
			Content:     token,
		}
		msg.Parts = append(msg.Parts, part)
		fmt.Fprintf(w, "event: message.part\ndata: %s\n\n", mustJSON(ACPMessagePartEvent{Type: "message.part", Part: part}))
		flusher.Flush()
	}

	// Re-fetch run for final status
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: run.Name, Namespace: run.Namespace}, run); err == nil {
		finalStatus := mapAgentRunPhaseToACPStatus(run.Status.Phase)
		acpRun.Status = finalStatus

		if run.Status.Phase == agentorcv1alpha1.AgentRunPhaseSucceeded && run.Status.Output != "" {
			acpRun.Output = []ACPMessage{{
				Role: "agent",
				Parts: []ACPMessagePart{{
					ContentType: "text/plain",
					Content:     run.Status.Output,
				}},
			}}
			if run.Status.CompletionTime != nil {
				acpRun.FinishedAt = run.Status.CompletionTime.Format(time.RFC3339)
			}
		}

		fmt.Fprintf(w, "event: message.completed\ndata: %s\n\n", mustJSON(ACPMessageCompletedEvent{Type: "message.completed", Message: msg}))
		flusher.Flush()

		switch finalStatus {
		case ACPRunCompleted:
			fmt.Fprintf(w, "event: run.completed\ndata: %s\n\n", mustJSON(ACPRunCompletedEvent{Type: "run.completed", Run: acpRun}))
		case ACPRunFailed:
			fmt.Fprintf(w, "event: run.failed\ndata: %s\n\n", mustJSON(ACPRunFailedEvent{Type: "run.failed", Run: acpRun}))
		}
		flusher.Flush()
	}
}

// resumeACPRun handles POST /runs/{run_id} to resume a waiting run.
func (s *ACPServer) resumeACPRun(w http.ResponseWriter, r *http.Request, run *agentorcv1alpha1.AgentRun) {
	if r.Method != http.MethodPost {
		writeACPError(w, "invalid_input", "POST required", http.StatusMethodNotAllowed)
		return
	}

	if run.Status.Phase != agentorcv1alpha1.AgentRunPhaseWaitingForInput {
		writeACPError(w, "invalid_input", "run is not awaiting input", http.StatusConflict)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeACPError(w, "invalid_input", "reading body", http.StatusBadRequest)
		return
	}

	var req struct {
		AwaitResume struct {
			Answer string `json:"answer,omitempty"`
		} `json:"await_resume"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeACPError(w, "invalid_input", fmt.Sprintf("invalid JSON: %s", err), http.StatusBadRequest)
		return
	}

	if req.AwaitResume.Answer == "" {
		writeACPError(w, "invalid_input", "await_resume.answer is required", http.StatusBadRequest)
		return
	}

	// Store the answer
	namespace := run.Namespace
	answerKey := fmt.Sprintf("answer:%s:%s", namespace, run.Name)
	if s.store != nil {
		if err := s.store.SaveAnswer(r.Context(), answerKey, req.AwaitResume.Answer, 24*time.Hour); err != nil {
			slog.Warn("failed to save ACP answer", "run", run.Name, "err", err)
		}
	}

	// Update run status
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.ClarifyAnswer = req.AwaitResume.Answer
	if err := s.crdClient.Status().Patch(r.Context(), run, patch); err != nil {
		writeACPError(w, "server_error", fmt.Sprintf("updating run: %s", err), http.StatusInternalServerError)
		return
	}

	resp := ACPRun{
		AgentName: run.Spec.AgentRef,
		SessionID: run.Labels["agentorc.io/session-id"],
		RunID:     run.Name,
		Status:    ACPRunInProgress,
		CreatedAt: run.CreationTimestamp.Format(time.RFC3339),
	}
	jsonResponse(w, resp)
}

// mapAgentRunPhaseToACPStatus converts agent-orc phase to ACP status.
func mapAgentRunPhaseToACPStatus(phase agentorcv1alpha1.AgentRunPhase) ACPRunStatus {
	switch phase {
	case agentorcv1alpha1.AgentRunPhasePending:
		return ACPRunCreated
	case agentorcv1alpha1.AgentRunPhaseRunning:
		return ACPRunInProgress
	case agentorcv1alpha1.AgentRunPhaseWaitingForInput:
		return ACPRunAwaiting
	case agentorcv1alpha1.AgentRunPhaseSucceeded:
		return ACPRunCompleted
	case agentorcv1alpha1.AgentRunPhaseFailed:
		return ACPRunFailed
	default:
		return ACPRunCreated
	}
}

func writeACPError(w http.ResponseWriter, code, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ACPErr{Code: code, Message: message})
}
