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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcv1alpha1 "github.com/floppyfish14/agent-orc/api/v1alpha1"
)

// acpScheme builds a runtime.Scheme with agent-orc CRDs + corev1 for fake client.
func acpScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := agentorcv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add agentorc scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	return s
}

// newACPServer builds an ACPServer backed by a fake K8s client pre-loaded
// with the given objects, and injects a tenant identity into request context
// via a custom auth middleware wrapper.
func newACPServer(t *testing.T, objs ...client.Object) *ACPServer {
	t.Helper()
	t.Setenv("POD_NAMESPACE", "agent-orc-system")
	cl := fake.NewClientBuilder().WithScheme(acpScheme(t)).WithObjects(objs...).Build()
	return &ACPServer{
		k8s:       nil,
		crdClient: cl,
		auth:      &ExternalAuth{},
		store:     nil,
	}
}

// withTenant wraps the ACP server's routes with a middleware that injects
// a test TenantIdentity into the request context, bypassing auth middleware.
// This is for unit testing handlers directly without going through JWT/SA validation.
func (s *ACPServer) withTenant(tenant *TenantIdentity) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/readyz", readyzHandlerBuilder(nil, false, nil))
	mux.HandleFunc("/version", versionHandler)
	mux.Handle("/metrics", metricsHandler())
	mux.HandleFunc("/openapi.json", s.handleACPOpenAPI)
	mux.HandleFunc("/ping", s.handlePing)
	mux.HandleFunc("/agents", s.handleListAgents)
	mux.HandleFunc("/agents/", s.handleAgentRoutes)
	mux.HandleFunc("/runs", s.handleCreateRun)
	mux.HandleFunc("/runs/", s.handleRunByID)
	mux.HandleFunc("/session/", s.handleGetSession)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), tenantIdentityKey, tenant)
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

func testTenant() *TenantIdentity {
	return &TenantIdentity{
		TenantName:    "acme",
		Namespace:     "tenant-acme",
		AllowedAgents: nil, // nil means all agents allowed
	}
}

func TestHandleAgentManifest(t *testing.T) {
	// Set up an Agent with tools, knowledge bases, and guardrail.
	agent := &agentorcv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "support-bot", Namespace: "tenant-acme"},
		Spec: agentorcv1alpha1.AgentSpec{
			SystemPrompt:       "You are a helpful support bot.",
			Tools:              []string{"web-search"},
			KnowledgeBases:     []string{"project-docs"},
			GuardrailPolicyRef: "phi-redact",
		},
	}
	tool := &agentorcv1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "web-search", Namespace: "tenant-acme"},
		Spec: agentorcv1alpha1.ToolSpec{
			Schema: &agentorcv1alpha1.ToolSchema{
				Description: "Search the web",
				Input: &runtime.RawExtension{
					Raw: []byte(`{"type":"object","properties":{"query":{"type":"string"}}}`),
				},
			},
		},
	}
	s := newACPServer(t, agent, tool)
	h := s.withTenant(testTenant())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/support-bot", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var manifest ACPAgentManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if manifest.Name != "support-bot" {
		t.Fatalf("expected name 'support-bot', got %q", manifest.Name)
	}
	if manifest.Description != "You are a helpful support bot." {
		t.Fatalf("expected system prompt as description, got %q", manifest.Description)
	}
	if manifest.GuardrailPolicy != "phi-redact" {
		t.Fatalf("expected guardrail 'phi-redact', got %q", manifest.GuardrailPolicy)
	}
	if !manifest.ClarifyAvailable {
		t.Fatal("expected clarify_available to be true")
	}
	if len(manifest.KnowledgeBases) != 1 || manifest.KnowledgeBases[0] != "project-docs" {
		t.Fatalf("expected knowledge bases [project-docs], got %v", manifest.KnowledgeBases)
	}
	if len(manifest.AllowedTools) < 2 {
		t.Fatalf("expected at least 2 tools (built-in + web-search), got %d", len(manifest.AllowedTools))
	}
	// Check that web-search tool has its schema
	found := false
	for _, ti := range manifest.AllowedTools {
		if ti.Name == "web-search" {
			found = true
			if ti.Description != "Search the web" {
				t.Fatalf("expected tool description 'Search the web', got %q", ti.Description)
			}
			if ti.InputSchema == nil {
				t.Fatal("expected input schema for web-search tool")
			}
		}
	}
	if !found {
		t.Fatal("web-search tool not found in allowed_tools")
	}
	// Check input/output schemas
	if manifest.InputSchema == nil {
		t.Fatal("expected input_schema")
	}
	if manifest.OutputSchema == nil {
		t.Fatal("expected output_schema")
	}
}

func TestHandleAgentManifestNotFound(t *testing.T) {
	s := newACPServer(t)
	h := s.withTenant(testTenant())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/nonexistent", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentManifestNotAllowed(t *testing.T) {
	agent := &agentorcv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "bot-a", Namespace: "tenant-acme"},
		Spec:       agentorcv1alpha1.AgentSpec{SystemPrompt: "hello"},
	}
	s := newACPServer(t, agent)
	// Tenant with allowed agents that don't include bot-a
	tenant := &TenantIdentity{
		TenantName:    "acme",
		Namespace:     "tenant-acme",
		AllowedAgents: []string{"bot-b"},
	}
	h := s.withTenant(tenant)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/bot-a", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentManifestNoAuth(t *testing.T) {
	s := newACPServer(t)
	h := s.Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/bot-a", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestHandleAgentManifestDisableClarify(t *testing.T) {
	agent := &agentorcv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "auto-bot", Namespace: "tenant-acme"},
		Spec: agentorcv1alpha1.AgentSpec{
			SystemPrompt:   "Autonomous agent",
			DisableClarify: true,
		},
	}
	s := newACPServer(t, agent)
	h := s.withTenant(testTenant())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/auto-bot", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var manifest ACPAgentManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if manifest.ClarifyAvailable {
		t.Fatal("expected clarify_available to be false when DisableClarify is true")
	}
}

func TestHandleAgentRun(t *testing.T) {
	agent := &agentorcv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "support-bot", Namespace: "tenant-acme"},
		Spec:       agentorcv1alpha1.AgentSpec{SystemPrompt: "You are a helpful support bot."},
	}
	s := newACPServer(t, agent)
	h := s.withTenant(testTenant())

	body := `{"input":[{"role":"user","parts":[{"content_type":"text/plain","content":"How do I reset my password?"}]}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agents/support-bot/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ACPRun
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp.AgentName != "support-bot" {
		t.Fatalf("expected agent name 'support-bot', got %q", resp.AgentName)
	}
	if resp.Status != ACPRunCreated {
		t.Fatalf("expected status 'created', got %q", resp.Status)
	}
	if resp.RunID == "" {
		t.Fatal("expected non-empty run ID")
	}
}

func TestHandleAgentRunNotFound(t *testing.T) {
	s := newACPServer(t)
	h := s.withTenant(testTenant())

	body := `{"input":[{"role":"user","parts":[{"content_type":"text/plain","content":"hello"}]}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agents/nonexistent/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentRunNotAllowed(t *testing.T) {
	agent := &agentorcv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "bot-a", Namespace: "tenant-acme"},
		Spec:       agentorcv1alpha1.AgentSpec{SystemPrompt: "hello"},
	}
	s := newACPServer(t, agent)
	tenant := &TenantIdentity{
		TenantName:    "acme",
		Namespace:     "tenant-acme",
		AllowedAgents: []string{"bot-b"},
	}
	h := s.withTenant(tenant)

	body := `{"input":[{"role":"user","parts":[{"content_type":"text/plain","content":"hello"}]}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agents/bot-a/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentRunMissingInput(t *testing.T) {
	agent := &agentorcv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "bot", Namespace: "tenant-acme"},
		Spec:       agentorcv1alpha1.AgentSpec{SystemPrompt: "hello"},
	}
	s := newACPServer(t, agent)
	h := s.withTenant(testTenant())

	body := `{}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agents/bot/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentRunEmptyInput(t *testing.T) {
	agent := &agentorcv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "bot", Namespace: "tenant-acme"},
		Spec:       agentorcv1alpha1.AgentSpec{SystemPrompt: "hello"},
	}
	s := newACPServer(t, agent)
	h := s.withTenant(testTenant())

	body := `{"input":[{"role":"user","content":""}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agents/bot/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentRunInvalidJSON(t *testing.T) {
	agent := &agentorcv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "bot", Namespace: "tenant-acme"},
		Spec:       agentorcv1alpha1.AgentSpec{SystemPrompt: "hello"},
	}
	s := newACPServer(t, agent)
	h := s.withTenant(testTenant())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agents/bot/run", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentRunWrongMethod(t *testing.T) {
	agent := &agentorcv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "bot", Namespace: "tenant-acme"},
		Spec:       agentorcv1alpha1.AgentSpec{SystemPrompt: "hello"},
	}
	s := newACPServer(t, agent)
	h := s.withTenant(testTenant())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/bot/run", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentRunWithSession(t *testing.T) {
	agent := &agentorcv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "bot", Namespace: "tenant-acme"},
		Spec:       agentorcv1alpha1.AgentSpec{SystemPrompt: "hello"},
	}
	s := newACPServer(t, agent)
	h := s.withTenant(testTenant())

	body := `{"input":[{"role":"user","parts":[{"content_type":"text/plain","content":"hello"}]}],"session_id":"sess-123"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agents/bot/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp ACPRun
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp.SessionID != "sess-123" {
		t.Fatalf("expected session ID 'sess-123', got %q", resp.SessionID)
	}

	// Verify the AgentRun was created with the session label.
	var run agentorcv1alpha1.AgentRun
	runName := resp.RunID
	if err := s.crdClient.Get(context.Background(), types.NamespacedName{Name: runName, Namespace: "tenant-acme"}, &run); err != nil {
		t.Fatalf("getting run: %v", err)
	}
	if run.Labels["agentorc.io/session-id"] != "sess-123" {
		t.Fatalf("expected session label 'sess-123', got %q", run.Labels["agentorc.io/session-id"])
	}
}

func TestHandleAgentRoutesUnknownSubpath(t *testing.T) {
	agent := &agentorcv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "bot", Namespace: "tenant-acme"},
		Spec:       agentorcv1alpha1.AgentSpec{SystemPrompt: "hello"},
	}
	s := newACPServer(t, agent)
	h := s.withTenant(testTenant())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/bot/unknown", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentRoutesMissingName(t *testing.T) {
	s := newACPServer(t)
	h := s.withTenant(testTenant())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleListAgentsEnriched(t *testing.T) {
	agent := &agentorcv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "bot-a", Namespace: "tenant-acme"},
		Spec: agentorcv1alpha1.AgentSpec{
			SystemPrompt:       "Bot A description",
			KnowledgeBases:     []string{"kb1"},
			GuardrailPolicyRef: "guard-1",
		},
	}
	s := newACPServer(t, agent)
	h := s.withTenant(testTenant())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp ACPAgentsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(resp.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(resp.Agents))
	}
	if resp.Agents[0].KnowledgeBases[0] != "kb1" {
		t.Fatalf("expected knowledge base 'kb1', got %v", resp.Agents[0].KnowledgeBases)
	}
	if resp.Agents[0].GuardrailPolicy != "guard-1" {
		t.Fatalf("expected guardrail 'guard-1', got %q", resp.Agents[0].GuardrailPolicy)
	}
	if !resp.Agents[0].ClarifyAvailable {
		t.Fatal("expected clarify_available to be true")
	}
}

func TestBuildAllowedTools(t *testing.T) {
	agent := &agentorcv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "bot", Namespace: "tenant-acme"},
		Spec: agentorcv1alpha1.AgentSpec{
			Tools: []string{"web-search", "missing-tool"},
		},
	}
	tool := &agentorcv1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "web-search", Namespace: "tenant-acme"},
		Spec: agentorcv1alpha1.ToolSpec{
			Schema: &agentorcv1alpha1.ToolSchema{
				Description: "Search the web",
				Input: &runtime.RawExtension{
					Raw: []byte(`{"type":"object","properties":{"query":{"type":"string"}}}`),
				},
			},
		},
	}
	s := newACPServer(t, agent, tool)
	tools := buildAllowedTools(context.Background(), s.crdClient, agent)

	// Should have: _clarify, _rag_search, _rag_ingest, web-search, missing-tool
	if len(tools) != 5 {
		t.Fatalf("expected 5 tools, got %d", len(tools))
	}

	// Check built-in tools are present
	names := make(map[string]bool)
	for _, ti := range tools {
		names[ti.Name] = true
	}
	for _, expected := range []string{"_clarify", "_rag_search", "_rag_ingest", "web-search", "missing-tool"} {
		if !names[expected] {
			t.Fatalf("expected tool %q in list", expected)
		}
	}

	// Check web-search has its schema
	for _, ti := range tools {
		if ti.Name == "web-search" {
			if ti.Description != "Search the web" {
				t.Fatalf("expected description 'Search the web', got %q", ti.Description)
			}
			if ti.InputSchema == nil {
				t.Fatal("expected input schema for web-search")
			}
		}
	}
}

func TestBuildInputSchema(t *testing.T) {
	tools := []ACPToolInfo{
		{Name: "_clarify"},
		{Name: "web-search", Description: "Search the web"},
	}
	schema := buildInputSchema(tools)
	if schema["type"] != "object" { //nolint:goconst

		t.Fatalf("expected type 'object', got %v", schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties map")
	}
	if _, ok := props["input"]; !ok {
		t.Fatal("expected 'input' property")
	}
	toolNames, ok := schema["tool_names"].([]string)
	if !ok {
		t.Fatal("expected tool_names list")
	}
	if len(toolNames) != 2 {
		t.Fatalf("expected 2 tool names, got %d", len(toolNames))
	}
}

func TestBuildOutputSchema(t *testing.T) {
	schema := buildOutputSchema()
	if schema["type"] != "object" {
		t.Fatalf("expected type 'object', got %v", schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties map")
	}
	if _, ok := props["output"]; !ok {
		t.Fatal("expected 'output' property")
	}
	if _, ok := props["status"]; !ok {
		t.Fatal("expected 'status' property")
	}
}

func TestTenantCanAccessAgent(t *testing.T) {
	// Nil AllowedAgents means all allowed
	tenant := &TenantIdentity{AllowedAgents: nil}
	if !tenantCanAccessAgent(tenant, "any-agent") {
		t.Fatal("expected all agents allowed when AllowedAgents is nil")
	}

	// Empty AllowedAgents means all allowed
	tenant = &TenantIdentity{AllowedAgents: []string{}}
	if !tenantCanAccessAgent(tenant, "any-agent") {
		t.Fatal("expected all agents allowed when AllowedAgents is empty")
	}

	// Specific allowed list
	tenant = &TenantIdentity{AllowedAgents: []string{"bot-a", "bot-b"}}
	if !tenantCanAccessAgent(tenant, "bot-a") {
		t.Fatal("expected bot-a to be allowed")
	}
	if tenantCanAccessAgent(tenant, "bot-c") {
		t.Fatal("expected bot-c to be denied")
	}
}

func TestRawExtensionToMap(t *testing.T) {
	// Valid JSON
	re := &runtime.RawExtension{Raw: []byte(`{"type":"object","properties":{"q":{"type":"string"}}}`)}
	m := rawExtensionToMap(re)
	if m == nil {
		t.Fatal("expected non-nil map")
	}
	if m["type"] != "object" {
		t.Fatalf("expected type 'object', got %v", m["type"])
	}

	// Nil RawExtension
	m = rawExtensionToMap(nil)
	if m != nil {
		t.Fatalf("expected nil for nil input, got %v", m)
	}

	// Invalid JSON
	re = &runtime.RawExtension{Raw: []byte("not json")}
	m = rawExtensionToMap(re)
	if m != nil {
		t.Fatalf("expected nil for invalid JSON, got %v", m)
	}
}
