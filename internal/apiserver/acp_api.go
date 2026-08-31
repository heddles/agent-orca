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
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
	"github.com/floppyfish14/agent-orca/internal/checkpoint"
	"github.com/floppyfish14/agent-orca/internal/security"
	"github.com/floppyfish14/agent-orca/internal/state"
)

// ACPServer implements the Agent Communication Protocol (ACP) API.
// ACP is a standardized RESTful API for agent interaction.

// acpAPIRequestTimeout bounds the total time for an ACP API request (auth +
// handler). The auth middleware adds its own authNetworkTimeout (10 s) for K8s
// TokenReview and OIDC JWKS calls; this timeout is the outer safety net that
// prevents a stuck handler (e.g. a slow CRD lookup) from holding the connection
// open until the ingress gives up with a 504.
const acpAPIRequestTimeout = 30 * time.Second

type ACPServer struct {
	k8s       kubernetes.Interface
	crdClient client.Client
	auth      *ExternalAuth
	store     state.Store
	// checkpoint persists session LastRunRef chains for warm-pool context continuity
	// across ACP turns. nil when no state store is configured (local dev).
	checkpoint checkpoint.Store

	// rateLimiter enforces per-tenant submission rate limits. nil disables it.
	rateLimiter *RateLimiter
}

// NewACPServer creates a new ACP server.
func NewACPServer(k8s kubernetes.Interface, crdClient client.Client, auth *ExternalAuth, store state.Store) *ACPServer {
	return &ACPServer{
		k8s:         k8s,
		crdClient:   crdClient,
		auth:        auth,
		store:       store,
		checkpoint:  checkpointStore(store),
		rateLimiter: auth.rateLimiter,
	}
}

// Handler returns an http.Handler for the ACP API.
func (s *ACPServer) Handler() http.Handler {
	mux := http.NewServeMux()

	// Observability (public — for scrapers/probes).
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/readyz", readyzHandlerBuilder(k8sReady(s.k8s), s.store != nil, s.store))
	mux.HandleFunc("/version", versionHandler)
	mux.Handle("/metrics", metricsHandler())

	// OpenAPI contract (unauthenticated — for SDK / tooling discovery).
	mux.HandleFunc("/openapi.json", s.handleACPOpenAPI)

	// Token endpoint (unauthenticated) - needed for client_credentials flow.
	mux.HandleFunc("/oauth/token", s.auth.HandleTokenRequest)

	mux.HandleFunc("/ping", s.handlePing)
	mux.HandleFunc("/agents", s.handleListAgents)
	mux.HandleFunc("/agents/", s.handleAgentRoutes)
	mux.HandleFunc("/runs", s.handleCreateRun)
	mux.HandleFunc("/runs/", s.handleRunByID)
	mux.HandleFunc("/session/", s.handleGetSession)
	return corsMiddleware(requestTimeoutMiddleware(s.auth.Middleware(instrument("acp", mux)), acpAPIRequestTimeout))
}

// ACP types aligned with ACP v1 spec from https://agentclientprotocol.com/protocol/v1/

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
	Message    string `json:"message,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolInput  any    `json:"tool_input,omitempty"`
	ToolOutput any    `json:"tool_output,omitempty"`
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

// ACPAwaitRequest describes what the client should provide next when a run is
// awaiting human input (e.g. the answer to a _clarify question).
type ACPAwaitRequest struct {
	// Question is the human-readable clarifying question the agent asked,
	// copied from the AgentRun's ClarifyQuestion status field.
	Question string `json:"question,omitempty"`
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
	// ContinuationRunRef is the name of a follow-up AgentRun created when a
	// waiting run is resumed with a clarify answer (see resumeACPRun). Clients
	// should poll this run instead of the original.
	ContinuationRunRef string `json:"continuationRunRef,omitempty"`
}

// ACPAgentStatus represents agent status metrics per spec.
type ACPAgentStatus struct {
	AvgRunTokens      *float64 `json:"avg_run_tokens,omitempty"`
	AvgRunTimeSeconds *float64 `json:"avg_run_time_seconds,omitempty"`
	SuccessRate       *float64 `json:"success_rate,omitempty"`
}

// ACPAgentMetadata represents agent metadata per spec.
type ACPAgentMetadata struct {
	Annotations         map[string]any `json:"annotations,omitempty"`
	Documentation       string         `json:"documentation,omitempty"`
	License             string         `json:"license,omitempty"`
	ProgrammingLanguage string         `json:"programming_language,omitempty"`
	NaturalLanguages    []string       `json:"natural_languages,omitempty"`
	Framework           string         `json:"framework,omitempty"`
}

// ACPAgentManifest is the response body for GET /agents/{name}.
type ACPAgentManifest struct {
	Name               string            `json:"name"`
	Namespace          string            `json:"namespace,omitempty"`
	Description        string            `json:"description"`
	InputContentTypes  []string          `json:"input_content_types"`
	OutputContentTypes []string          `json:"output_content_types"`
	Metadata           *ACPAgentMetadata `json:"metadata,omitempty"`
	Status             *ACPAgentStatus   `json:"status,omitempty"`

	// InputSchema is a JSON Schema describing the expected input format
	// (derived from the agent's configured tools + built-in tools).
	InputSchema map[string]any `json:"input_schema,omitempty"`

	// OutputSchema is a JSON Schema describing the expected output format.
	OutputSchema map[string]any `json:"output_schema,omitempty"`

	// AllowedTools lists the tools available to this agent with descriptions.
	AllowedTools []ACPToolInfo `json:"allowed_tools,omitempty"`

	// KnowledgeBases lists the KnowledgeBase names available to this agent.
	KnowledgeBases []string `json:"knowledge_bases,omitempty"`

	// GuardrailPolicy is the name of the GuardrailPolicy applied to this agent, if any.
	GuardrailPolicy string `json:"guardrail_policy,omitempty"`

	// ClarifyAvailable indicates whether the _clarify built-in tool is available
	// (i.e. human-in-the-loop is possible). False when DisableClarify is true.
	ClarifyAvailable bool `json:"clarify_available"`
}

// ACPToolInfo describes a tool available to an agent.
type ACPToolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// ACPAgentsListResponse is the response body for GET /agents.
type ACPAgentsListResponse struct {
	Agents []ACPAgentManifest `json:"agents"`
}

// ACPErr represents an error response.
type ACPErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
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
	Type    string `json:"type"`
	Generic any    `json:"generic"`
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
	jsonResponse(w, map[string]any{"status": "ok"})
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
	offset := max(parseIntQueryParam(r, "offset", 0), 0)

	var agentList agentorcav1alpha1.AgentList
	if err := s.crdClient.List(r.Context(), &agentList, client.InNamespace(tenant.Namespace)); err != nil {
		writeACPError(w, "server_error", err.Error(), http.StatusInternalServerError)
		return
	}

	// Filter by allowed agents if specified
	var filteredAgents []agentorcav1alpha1.Agent
	for _, agent := range agentList.Items {
		if tenant.AllowedAgents != nil && len(tenant.AllowedAgents) > 0 { //nolint:staticcheck

			found := slices.Contains(tenant.AllowedAgents, agent.Name)
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
	end := min(offset+limit, len(filteredAgents))

	agents := make([]ACPAgentManifest, 0, end-offset)
	for _, agent := range filteredAgents[offset:end] {
		manifest := ACPAgentManifest{
			Name:               agent.Name,
			Namespace:          tenant.Namespace,
			Description:        agent.Spec.SystemPrompt,
			InputContentTypes:  []string{"text/plain", "application/json"},
			OutputContentTypes: []string{"text/plain", "application/json"},
			KnowledgeBases:     agent.Spec.KnowledgeBases,
			GuardrailPolicy:    agent.Spec.GuardrailPolicyRef,
			ClarifyAvailable:   !agent.Spec.DisableClarify,
		}
		for _, kb := range agent.Spec.KnowledgeBaseRefs {
			manifest.KnowledgeBases = append(manifest.KnowledgeBases, kb.Name)
		}
		agents = append(agents, manifest)
	}

	jsonResponse(w, ACPAgentsListResponse{Agents: agents})
}

// handleAgentRoutes dispatches GET /agents/{name} (manifest) and POST /agents/{name}/run.
func (s *ACPServer) handleAgentRoutes(w http.ResponseWriter, r *http.Request) {
	tenant, ok := TenantFromContext(r.Context())
	if !ok {
		writeACPError(w, "unauthorized", "no tenant identity", http.StatusUnauthorized)
		return
	}

	// Extract agent name and optional sub-path from: /agents/{name}[/{sub}]
	path := strings.TrimPrefix(r.URL.Path, "/agents/")
	if path == "" {
		writeACPError(w, "invalid_input", "agent name required", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(path, "/", 2)
	name := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}

	// Check if tenant is allowed to access this agent
	if !tenantCanAccessAgent(tenant, name) {
		writeACPError(w, "unauthorized", "agent not allowed for tenant", http.StatusForbidden)
		return
	}

	// Verify the agent exists
	var agent agentorcav1alpha1.Agent
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: name, Namespace: tenant.Namespace}, &agent); err != nil {
		writeACPError(w, "not_found", fmt.Sprintf("agent %q not found", name), http.StatusNotFound)
		return
	}

	switch sub {
	case "":
		if r.Method != http.MethodGet {
			writeACPError(w, "invalid_input", "GET required", http.StatusMethodNotAllowed)
			return
		}
		s.writeAgentManifest(w, r, &agent, tenant.Namespace)
	case "run":
		if r.Method != http.MethodPost {
			writeACPError(w, "invalid_input", "POST required", http.StatusMethodNotAllowed)
			return
		}
		s.handleAgentRun(w, r, tenant, &agent)
	default:
		writeACPError(w, "invalid_input", fmt.Sprintf("unknown sub-path %q", sub), http.StatusBadRequest)
	}
}

// tenantCanAccessAgent checks if the tenant is allowed to access the given agent.
func tenantCanAccessAgent(tenant *TenantIdentity, name string) bool {
	if tenant.AllowedAgents == nil || len(tenant.AllowedAgents) == 0 { //nolint:staticcheck

		return true
	}
	return slices.Contains(tenant.AllowedAgents, name)
}

// writeAgentManifest builds and writes the enriched agent manifest.
func (s *ACPServer) writeAgentManifest(w http.ResponseWriter, r *http.Request, agent *agentorcav1alpha1.Agent, namespace string) {
	manifest := ACPAgentManifest{
		Name:               agent.Name,
		Namespace:          namespace,
		Description:        agent.Spec.SystemPrompt,
		InputContentTypes:  []string{"text/plain", "application/json"},
		OutputContentTypes: []string{"text/plain", "application/json"},
	}

	// Enrich manifest with tool schemas, knowledge bases, and guardrail info.
	manifest.AllowedTools = buildAllowedTools(r.Context(), s.crdClient, agent)
	manifest.KnowledgeBases = agent.Spec.KnowledgeBases
	for _, kb := range agent.Spec.KnowledgeBaseRefs {
		manifest.KnowledgeBases = append(manifest.KnowledgeBases, kb.Name)
	}
	manifest.GuardrailPolicy = agent.Spec.GuardrailPolicyRef
	manifest.ClarifyAvailable = !agent.Spec.DisableClarify

	// Build input/output schemas from the agent's tools.
	manifest.InputSchema = buildInputSchema(manifest.AllowedTools)
	manifest.OutputSchema = buildOutputSchema()

	jsonResponse(w, manifest)
}

// ACPAgentRunRequest is the body for POST /agents/{name}/run.
type ACPAgentRunRequest struct {
	Input     []ACPMessage `json:"input"`
	SessionID string       `json:"session_id,omitempty"`
}

// handleAgentRun creates a new AgentRun for the given agent, validating input
// against the agent's manifest schema. This is a thin, UX-friendly wrapper
// around the ACP POST /runs flow.
func (s *ACPServer) handleAgentRun(w http.ResponseWriter, r *http.Request, tenant *TenantIdentity, agent *agentorcav1alpha1.Agent) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeACPError(w, "invalid_input", "reading body", http.StatusBadRequest)
		return
	}

	var req ACPAgentRunRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeACPError(w, "invalid_input", fmt.Sprintf("invalid JSON: %s", err), http.StatusBadRequest)
		return
	}

	if len(req.Input) == 0 {
		writeACPError(w, "invalid_input", "input is required", http.StatusBadRequest)
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
	if inputText == "" {
		writeACPError(w, "invalid_input", "input content is required", http.StatusBadRequest)
		return
	}

	// Enforce tenant quotas.
	if qerr, ok := enforceQuotas(r.Context(), s.crdClient, s.rateLimiter, tenant).(*QuotaError); ok && qerr != nil {
		if qerr.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(qerr.RetryAfter.Seconds())))
		}
		writeACPError(w, "quota_exceeded", qerr.Message, qerr.Code)
		return
	}

	// Determine session ID
	sessionID := req.SessionID

	// Create AgentRun CR
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("acp-%s-", agent.Name),
			Namespace:    tenant.Namespace,
			Labels: map[string]string{
				security.LabelManagedBy: "agent-orca",
				"agentorca.io/tenant":   tenant.TenantName,
			},
		},
		Spec: agentorcav1alpha1.AgentRunSpec{
			AgentRef: agent.Name,
			Input:    inputText,
		},
	}
	if sessionID != "" {
		run.Labels["agentorca.io/session-id"] = sessionID
	}
	// Enable warm-pod claiming + context chaining.
	s.enrichACPRunForWarmPods(r.Context(), run, agent.Name, sessionID)

	if err := s.crdClient.Create(r.Context(), run); err != nil {
		slog.Error("creating ACP agent run", "err", err)
		writeACPError(w, "server_error", fmt.Sprintf("creating run: %s", err), http.StatusInternalServerError)
		return
	}

	slog.Info("ACP agent run created", "run", run.Name, "agent", agent.Name, "tenant", tenant.TenantName)

	// Advance the session checkpoint so the next ACP turn chains context.
	s.updateSessionCheckpoint(r.Context(), sessionID, run.Name)

	resp := ACPRun{
		AgentName: agent.Name,
		SessionID: sessionID,
		RunID:     run.Name,
		Status:    ACPRunCreated,
		CreatedAt: run.CreationTimestamp.Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// buildAllowedTools fetches the Tool CRDs referenced by an agent and returns
// their names and JSON schemas for the manifest.
func buildAllowedTools(ctx context.Context, k8sClient client.Client, agent *agentorcav1alpha1.Agent) []ACPToolInfo {
	// Built-in tools always available.
	tools := []ACPToolInfo{
		{Name: "_clarify", Description: "Ask the human a clarifying question before proceeding"},
		{Name: "_rag_search", Description: "Search the configured knowledge bases"},
		{Name: "_rag_ingest", Description: "Ingest a document into a knowledge base"},
	}

	// Fetch each configured Tool CR and extract its schema.
	for _, toolName := range agent.Spec.Tools {
		var tool agentorcav1alpha1.Tool
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: toolName, Namespace: agent.Namespace}, &tool); err != nil {
			slog.Warn("failed to fetch tool for manifest", "tool", toolName, "err", err)
			tools = append(tools, ACPToolInfo{Name: toolName})
			continue
		}
		info := ACPToolInfo{
			Name: toolName,
		}
		if tool.Spec.Schema != nil {
			info.Description = tool.Spec.Schema.Description
			if tool.Spec.Schema.Input != nil {
				info.InputSchema = rawExtensionToMap(tool.Spec.Schema.Input)
			}
		}
		tools = append(tools, info)
	}

	return tools
}

// rawExtensionToMap converts a runtime.RawExtension to a map[string]interface{}.
func rawExtensionToMap(re *runtime.RawExtension) map[string]any {
	if re == nil {
		return nil
	}
	if len(re.Raw) > 0 {
		var m map[string]any
		if err := json.Unmarshal(re.Raw, &m); err == nil {
			return m
		}
	}
	return nil
}

// buildInputSchema constructs a JSON Schema for the agent's input based on
// its available tools. The input is a text field plus optional tool call parameters.
func buildInputSchema(tools []ACPToolInfo) map[string]any {
	properties := map[string]any{
		"input": map[string]any{
			"type":        "string",
			"description": "The task or message to send to the agent",
		},
	}
	toolNames := make([]string, 0, len(tools))
	for _, t := range tools {
		toolNames = append(toolNames, t.Name)
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   []string{"input"},
		"tool_names": toolNames,
	}
}

// buildOutputSchema constructs a JSON Schema for the agent's output.
func buildOutputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"output": map[string]any{
				"type":        "string",
				"description": "The final text output from the agent",
			},
			"status": map[string]any{
				"type": "string",
				"enum": []string{"succeeded", "failed", "awaiting"},
			},
		},
		"required": []string{"output", "status"},
	}
}

// resolveDeploymentForAgent finds the first AgentDeployment in the namespace
// that references the given agent name. Returns (nil, nil) if none exists.
func (s *ACPServer) resolveDeploymentForAgent(ctx context.Context, ns, agentName string) (*agentorcav1alpha1.AgentDeployment, error) {
	var list agentorcav1alpha1.AgentDeploymentList
	if err := s.crdClient.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Spec.AgentRef == agentName {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

// enrichACPRunForWarmPods sets the labels and PriorRunRef needed for the
// controller to claim a warm pod and the model-router to load prior conversation
// context. If no deployment is found for the agent, the run falls through to the
// existing one-shot pod path (no labels, no PriorRunRef).
func (s *ACPServer) enrichACPRunForWarmPods(ctx context.Context, run *agentorcav1alpha1.AgentRun, agentName, sessionID string) {
	if sessionID == "" {
		return
	}
	dep, err := s.resolveDeploymentForAgent(ctx, run.Namespace, agentName)
	if err != nil || dep == nil {
		slog.Debug("ACP run has no matching deployment; using one-shot pod path",
			"agent", agentName, "err", err)
		return
	}
	run.Labels["agentorca.io/deployment"] = dep.Name
	run.Labels["agentorca.io/session"] = sessionID
	run.Labels["agentorca.io/source"] = "acp"

	// Chain to the prior run in this session so the warm pod's model-router
	// loads the prior conversation checkpoint.
	if s.checkpoint != nil {
		if cp, err := s.checkpoint.Load(ctx, sessionID); err == nil && cp != nil {
			run.Spec.PriorRunRef = cp.LastRunRef
		}
	}
}

// updateSessionCheckpoint advances the session's LastRunRef to the new run name
// so the next ACP turn in the same session chains to it.
func (s *ACPServer) updateSessionCheckpoint(ctx context.Context, sessionID, runName string) {
	if sessionID == "" || s.checkpoint == nil {
		return
	}
	cp, _ := s.checkpoint.Load(ctx, sessionID)
	if cp == nil {
		cp = &agentorcav1alpha1.Checkpoint{SessionID: sessionID}
	}
	cp.LastRunRef = runName
	cp.Version++
	if _, err := s.checkpoint.Save(ctx, cp); err != nil {
		slog.Warn("failed to update ACP session checkpoint", "session", sessionID, "run", runName, "err", err)
	}
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
	if tenant.AllowedAgents != nil && len(tenant.AllowedAgents) > 0 { //nolint:staticcheck

		found := slices.Contains(tenant.AllowedAgents, req.AgentName)
		if !found {
			writeACPError(w, "unauthorized", "agent not allowed for tenant", http.StatusForbidden)
			return
		}
	}

	// Verify the agent exists
	var agent agentorcav1alpha1.Agent
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: req.AgentName, Namespace: tenant.Namespace}, &agent); err != nil {
		writeACPError(w, "not_found", fmt.Sprintf("agent %q not found", req.AgentName), http.StatusNotFound)
		return
	}

	// Enforce tenant quotas (rate limit, concurrent runs, daily budget).
	if qerr, ok := enforceQuotas(r.Context(), s.crdClient, s.rateLimiter, tenant).(*QuotaError); ok && qerr != nil {
		if qerr.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(qerr.RetryAfter.Seconds())))
		}
		writeACPError(w, "quota_exceeded", qerr.Message, qerr.Code)
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
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("acp-%s-", req.AgentName),
			Namespace:    tenant.Namespace,
			Labels: map[string]string{
				security.LabelManagedBy: "agent-orca",
				"agentorca.io/tenant":   tenant.TenantName,
			},
		},
		Spec: agentorcav1alpha1.AgentRunSpec{
			AgentRef: req.AgentName,
			Input:    inputText,
		},
	}
	if sessionID != "" {
		run.Labels["agentorca.io/session-id"] = sessionID
	}
	// Enable warm-pod claiming + context chaining via the deployment that owns
	// this agent. Falls back to one-shot pod path if no deployment exists.
	s.enrichACPRunForWarmPods(r.Context(), run, req.AgentName, sessionID)

	if err := s.crdClient.Create(r.Context(), run); err != nil {
		slog.Error("creating ACP run", "err", err)
		writeACPError(w, "server_error", fmt.Sprintf("creating run: %s", err), http.StatusInternalServerError)
		return
	}

	slog.Info("ACP run created", "run", run.Name, "agent", req.AgentName, "tenant", tenant.TenantName)

	// Advance the session checkpoint so the next ACP turn chains context.
	s.updateSessionCheckpoint(r.Context(), sessionID, run.Name)

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
	_ = json.NewEncoder(w).Encode(resp)
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

	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: runID, Namespace: tenant.Namespace}, &run); err != nil {
		writeACPError(w, "not_found", "run not found", http.StatusNotFound)
		return
	}

	// Verify run belongs to tenant
	if run.Labels["agentorca.io/tenant"] != tenant.TenantName {
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
	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: runID, Namespace: tenant.Namespace}, &run); err != nil {
		writeACPError(w, "not_found", "run not found", http.StatusNotFound)
		return
	}

	// Verify run belongs to tenant
	if run.Labels["agentorca.io/tenant"] != tenant.TenantName {
		writeACPError(w, "not_found", "run not found", http.StatusNotFound)
		return
	}

	// Only allow cancelling pending, running, or waiting runs
	switch run.Status.Phase {
	case agentorcav1alpha1.AgentRunPhasePending,
		agentorcav1alpha1.AgentRunPhaseRunning,
		agentorcav1alpha1.AgentRunPhaseWaitingForInput:
		// Valid states to cancel
	default:
		writeACPError(w, "invalid_input", "run is already in a terminal state", http.StatusConflict)
		return
	}

	// Mark as failed with cancellation reason (no dedicated Cancelled phase exists)
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseFailed
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
		SessionID:  run.Labels["agentorca.io/session-id"],
		RunID:      run.Name,
		Status:     ACPRunCancelled,
		CreatedAt:  run.CreationTimestamp.Format(time.RFC3339),
		FinishedAt: now.Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleListRunEvents handles GET /runs/{run_id}/events.
func (s *ACPServer) handleListRunEvents(w http.ResponseWriter, r *http.Request, tenant *TenantIdentity, runID string) {
	var run agentorcav1alpha1.AgentRun
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: runID, Namespace: tenant.Namespace}, &run); err != nil {
		writeACPError(w, "not_found", "run not found", http.StatusNotFound)
		return
	}

	// Verify run belongs to tenant
	if run.Labels["agentorca.io/tenant"] != tenant.TenantName {
		writeACPError(w, "not_found", "run not found", http.StatusNotFound)
		return
	}

	// Build events list based on run state using interface{} for polymorphism
	events := []any{}

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
	if run.Status.Phase == agentorcav1alpha1.AgentRunPhaseSucceeded && run.Status.Output != "" {
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
	} else if run.Status.Phase == agentorcav1alpha1.AgentRunPhaseFailed {
		events = append(events, ACPRunFailedEvent{
			Type: "run.failed",
			Run: ACPRun{
				AgentName: run.Spec.AgentRef,
				RunID:     run.Name,
				Status:    ACPRunFailed,
				CreatedAt: run.CreationTimestamp.Format(time.RFC3339),
			},
		})
	} else if run.Status.Phase == agentorcav1alpha1.AgentRunPhaseWaitingForInput {
		awaitingRun := ACPRun{
			AgentName: run.Spec.AgentRef,
			RunID:     run.Name,
			Status:    ACPRunAwaiting,
			CreatedAt: run.CreationTimestamp.Format(time.RFC3339),
			AwaitRequest: &ACPAwaitRequest{
				Question: run.Status.ClarifyQuestion,
			},
		}
		if run.Status.ClarifyQuestion != "" {
			awaitingRun.Output = []ACPMessage{{
				Role: "agent",
				Parts: []ACPMessagePart{{
					ContentType: "text/plain",
					Content:     run.Status.ClarifyQuestion,
				}},
			}}
		}
		events = append(events, ACPRunAwaitingEvent{
			Type: "run.awaiting",
			Run:  awaitingRun,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"events": events})
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
	labels := client.MatchingLabels{"agentorca.io/session-id": sessionID}
	var runList agentorcav1alpha1.AgentRunList
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
func (s *ACPServer) getACPRun(w http.ResponseWriter, r *http.Request, run *agentorcav1alpha1.AgentRun) {
	acpStatus := mapAgentRunPhaseToACPStatus(run.Status.Phase)

	// Check if client wants streaming
	accept := r.Header.Get("Accept")
	if accept == "text/event-stream" && run.Status.Phase == agentorcav1alpha1.AgentRunPhaseRunning {
		s.streamACPRun(w, r, run)
		return
	}

	// Get session ID from labels if present
	sessionID := run.Labels["agentorca.io/session-id"]

	resp := ACPRun{
		AgentName: run.Spec.AgentRef,
		SessionID: sessionID,
		RunID:     run.Name,
		Status:    acpStatus,
		CreatedAt: run.CreationTimestamp.Format(time.RFC3339),
	}

	if run.Status.Phase == agentorcav1alpha1.AgentRunPhaseSucceeded && run.Status.Output != "" {
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

	if run.Status.Phase == agentorcav1alpha1.AgentRunPhaseFailed {
		resp.Status = ACPRunFailed
		resp.FinishedAt = time.Now().Format(time.RFC3339)
	}

	if run.Status.Phase == agentorcav1alpha1.AgentRunPhaseWaitingForInput {
		resp.Status = ACPRunAwaiting
		resp.AwaitRequest = &ACPAwaitRequest{Question: run.Status.ClarifyQuestion}
		// Surface the clarification question as agent output so clients/bridges
		// can display it without a separate tool call.
		if run.Status.ClarifyQuestion != "" {
			resp.Output = []ACPMessage{{
				Role: "agent",
				Parts: []ACPMessagePart{{
					ContentType: "text/plain",
					Content:     run.Status.ClarifyQuestion,
				}},
			}}
		}
	}

	jsonResponse(w, resp)
}

// streamACPRun streams tokens via SSE using ACP-compliant event format.
func (s *ACPServer) streamACPRun(w http.ResponseWriter, r *http.Request, run *agentorcav1alpha1.AgentRun) {
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
	sessionID := run.Labels["agentorca.io/session-id"]

	// Emit run.created event (ACP spec format)
	acpRun := ACPRun{
		AgentName: run.Spec.AgentRef,
		SessionID: sessionID,
		RunID:     run.Name,
		Status:    ACPRunInProgress,
		CreatedAt: run.CreationTimestamp.Format(time.RFC3339),
	}
	_, _ = fmt.Fprintf(w, "event: run.created\ndata: %s\n\n", mustJSON(ACPRunCreatedEvent{Type: "run.created", Run: acpRun}))
	flusher.Flush()

	// Emit run.in-progress event
	_, _ = fmt.Fprintf(w, "event: run.in-progress\ndata: %s\n\n", mustJSON(ACPRunInProgressEvent{Type: "run.in-progress", Run: acpRun}))
	flusher.Flush()

	// Stream tokens
	streamKey := fmt.Sprintf("tokens:%s:%s", run.Namespace, run.Name)
	tokenCh, err := s.store.TailTokens(r.Context(), streamKey)
	if err != nil {
		slog.Warn("failed to open token stream for ACP", "key", streamKey, "err", err)
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", mustJSON(ACPErrEvent{Type: "error", Error: ACPErr{Code: "server_error", Message: err.Error()}}))
		flusher.Flush()
		return
	}

	// Create message for output
	msg := ACPMessage{
		Role:  "agent",
		Parts: []ACPMessagePart{},
	}
	_, _ = fmt.Fprintf(w, "event: message.created\ndata: %s\n\n", mustJSON(ACPMessageCreatedEvent{Type: "message.created", Message: msg}))
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
		_, _ = fmt.Fprintf(w, "event: message.part\ndata: %s\n\n", mustJSON(ACPMessagePartEvent{Type: "message.part", Part: part}))
		flusher.Flush()
	}

	// Re-fetch run for final status
	if err := s.crdClient.Get(r.Context(), client.ObjectKey{Name: run.Name, Namespace: run.Namespace}, run); err == nil {
		finalStatus := mapAgentRunPhaseToACPStatus(run.Status.Phase)
		acpRun.Status = finalStatus

		if run.Status.Phase == agentorcav1alpha1.AgentRunPhaseSucceeded && run.Status.Output != "" {
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

		// Surface the clarification question as output for awaiting runs so the
		// client can display it even when consuming the SSE stream.
		if run.Status.Phase == agentorcav1alpha1.AgentRunPhaseWaitingForInput && run.Status.ClarifyQuestion != "" {
			acpRun.Output = []ACPMessage{{
				Role: "agent",
				Parts: []ACPMessagePart{{
					ContentType: "text/plain",
					Content:     run.Status.ClarifyQuestion,
				}},
			}}
		}

		_, _ = fmt.Fprintf(w, "event: message.completed\ndata: %s\n\n", mustJSON(ACPMessageCompletedEvent{Type: "message.completed", Message: msg}))
		flusher.Flush()

		switch finalStatus {
		case ACPRunCompleted:
			_, _ = fmt.Fprintf(w, "event: run.completed\ndata: %s\n\n", mustJSON(ACPRunCompletedEvent{Type: "run.completed", Run: acpRun}))
		case ACPRunFailed:
			_, _ = fmt.Fprintf(w, "event: run.failed\ndata: %s\n\n", mustJSON(ACPRunFailedEvent{Type: "run.failed", Run: acpRun}))
		case ACPRunAwaiting:
			_, _ = fmt.Fprintf(w, "event: run.awaiting\ndata: %s\n\n", mustJSON(ACPRunAwaitingEvent{Type: "run.awaiting", Run: acpRun}))
		}
		flusher.Flush()
	}
}

// resumeACPRun handles POST /runs/{run_id} to resume a waiting run. Instead of
// mutating the original run in place (which doesn't work for warm pods — the
// model-router only checks for a clarify answer at startup, not while running),
// it creates a CONTINUATION AgentRun that picks up the prior run's checkpoint via
// PriorRunRef. The continuation run claims its own warm pod, and its model-router
// injects the human's answer at startup. The original run is marked Succeeded so
// the bridge can follow the continuation.
func (s *ACPServer) resumeACPRun(w http.ResponseWriter, r *http.Request, run *agentorcav1alpha1.AgentRun) {
	if r.Method != http.MethodPost {
		writeACPError(w, "invalid_input", "POST required", http.StatusMethodNotAllowed)
		return
	}

	if run.Status.Phase != agentorcav1alpha1.AgentRunPhaseWaitingForInput {
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

	// Build continuation input. If the model-router checkpointed the prior run,
	// the continuation's model-router will load it via PriorRunRef and inject the
	// answer at startup. If no checkpoint exists (controller safety-net path),
	// bake the Q&A into the input so the continuation has full context.
	continuationInput := run.Spec.Input
	if s.store != nil {
		checkpointKey := fmt.Sprintf("agentorca/runs/%s/state", run.Name)
		if msgs, lerr := s.store.LoadMessages(r.Context(), checkpointKey); lerr != nil || len(msgs) == 0 {
			continuationInput = fmt.Sprintf(
				"%s\n\n---\nPrevious attempt asked: %s\nHuman answered: %s\n---\nPlease proceed with the above information.", //nolint:lll
				run.Spec.Input, run.Status.ClarifyQuestion, req.AwaitResume.Answer)
		}
	}

	// Create a continuation AgentRun that loads the original's checkpoint via
	// PriorRunRef. Inherit the original's labels (deployment, session, source) so
	// the controller claims a warm pod for it.
	continuation := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: run.Name + "-cont-",
			Namespace:    run.Namespace,
			Labels:       run.Labels, // shallow copy — inherits deployment/session/source
		},
		Spec: agentorcav1alpha1.AgentRunSpec{
			AgentRef:    run.Spec.AgentRef,
			Input:       continuationInput,
			PriorRunRef: run.Name,
		},
	}
	if err := s.crdClient.Create(r.Context(), continuation); err != nil {
		writeACPError(w, "server_error", fmt.Sprintf("creating continuation run: %s", err), http.StatusInternalServerError)
		return
	}

	// Store the answer under the CONTINUATION run's name so its model-router
	// (which starts fresh) finds it during initialization / ClaimRun.
	if s.store != nil {
		answerKey := fmt.Sprintf("clarify-answer:%s:%s", run.Namespace, continuation.Name)
		if err := s.store.SaveAnswer(r.Context(), answerKey, req.AwaitResume.Answer, 1*time.Hour); err != nil {
			slog.Warn("failed to save ACP clarify answer", "run", continuation.Name, "err", err)
		}
	}

	// Mark the original run as Succeeded with a forward pointer to the continuation.
	origPatch := client.MergeFrom(run.DeepCopy())
	run.Status.Phase = agentorcav1alpha1.AgentRunPhaseSucceeded
	run.Status.ClarifyAnswer = req.AwaitResume.Answer
	run.Status.ContinuationRunRef = continuation.Name
	_ = s.crdClient.Status().Patch(r.Context(), run, origPatch)

	// Advance the session checkpoint so the next turn chains to the continuation.
	sessionID := run.Labels["agentorca.io/session"]
	s.updateSessionCheckpoint(r.Context(), sessionID, continuation.Name)

	slog.Info("ACP run resumed via continuation", "original", run.Name, "continuation", continuation.Name)

	resp := ACPRun{
		AgentName:          run.Spec.AgentRef,
		SessionID:          sessionID,
		RunID:              continuation.Name,
		Status:             ACPRunInProgress,
		ContinuationRunRef: continuation.Name,
		CreatedAt:          continuation.CreationTimestamp.Format(time.RFC3339),
	}
	jsonResponse(w, resp)
}

// mapAgentRunPhaseToACPStatus converts agent-orca phase to ACP status.
func mapAgentRunPhaseToACPStatus(phase agentorcav1alpha1.AgentRunPhase) ACPRunStatus {
	switch phase {
	case agentorcav1alpha1.AgentRunPhasePending:
		return ACPRunCreated
	case agentorcav1alpha1.AgentRunPhaseRunning:
		return ACPRunInProgress
	case agentorcav1alpha1.AgentRunPhaseWaitingForInput:
		return ACPRunAwaiting
	case agentorcav1alpha1.AgentRunPhaseSucceeded:
		return ACPRunCompleted
	case agentorcav1alpha1.AgentRunPhaseFailed:
		return ACPRunFailed
	default:
		return ACPRunCreated
	}
}

func writeACPError(w http.ResponseWriter, code, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ACPErr{Code: code, Message: message})
}
