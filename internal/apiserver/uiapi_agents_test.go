package apiserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

func agentsUIServer(t *testing.T) *UIServer {
	t.Helper()
	agent := func(ns, name string, labels, ann map[string]string) *agentorcav1alpha1.Agent {
		return &agentorcav1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, Annotations: ann},
			Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "auto", SystemPrompt: "prompt of " + name},
		}
	}
	run := func(name, spend string) *agentorcav1alpha1.AgentRun {
		return &agentorcav1alpha1.AgentRun{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", Labels: map[string]string{tenancy.LabelTenantID: "tnt_a"}},
			Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "helper"},
			Status:     agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseSucceeded, SpendUSD: spend},
		}
	}
	cl := fake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(
		agent("team-a", "helper", map[string]string{tenancy.LabelTenantID: "tnt_a"}, map[string]string{
			AnnotationDescription:   "Helps with things",
			AnnotationExampleInputs: `["do x","do y"]`,
			AnnotationInputSchema:   `{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}`,
		}),
		agent("team-b", "secret", map[string]string{tenancy.LabelTenantID: "tnt_b"}, nil),
		&agentorcav1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "helper-chat", Namespace: "team-a", Labels: map[string]string{tenancy.LabelTenantID: "tnt_a"}},
			Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "helper"},
		},
		run("r1", "0.10"), run("r2", "0.30"), run("r3", "0.20"),
	).Build()
	return &UIServer{crdClient: cl}
}

func agentsReq(s *UIServer, id *TenantIdentity, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if id != nil {
		req = req.WithContext(tenantCtx(id))
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

var catalogTenant = &TenantIdentity{TenantID: "tnt_a", TenantName: "a", Namespaces: []string{"team-a"}, UserID: "ada@example.com"}

func TestCatalogAndManifestAreTenantScoped(t *testing.T) {
	s := agentsUIServer(t)

	rec := agentsReq(s, catalogTenant, http.MethodGet, "/api/catalog", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"helper"`) || strings.Contains(body, "secret") {
		t.Fatalf("catalog should show only the caller's agents: %s", body)
	}
	for _, want := range []string{`"published":true`, `"typicalCostUSD":0.2`, `"description":"Helps with things"`, `"do x"`} {
		if !strings.Contains(body, want) {
			t.Errorf("catalog missing %s: %s", want, body)
		}
	}

	rec = agentsReq(s, catalogTenant, http.MethodGet, "/api/agents/team-a/helper/manifest", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"topic"`) {
		t.Fatalf("manifest should carry the input schema override: %d %s", rec.Code, rec.Body)
	}

	// Another tenant's agent is not found, whether or not it exists.
	for _, p := range []string{"/api/agents/team-b/secret/manifest", "/api/agents/team-a/nope/manifest"} {
		if rec := agentsReq(s, catalogTenant, http.MethodGet, p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s: want 404, got %d", p, rec.Code)
		}
	}
	rec = agentsReq(s, catalogTenant, http.MethodPost, "/api/agents/team-b/secret/run", `{"input":[{"parts":[{"content":"hi"}]}]}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("run of foreign agent: want 404, got %d", rec.Code)
	}
}

func TestRunCreatesTenantRunWithCreator(t *testing.T) {
	s := agentsUIServer(t)
	rec := agentsReq(s, catalogTenant, http.MethodPost, "/api/agents/team-a/helper/run",
		`{"input":[{"role":"user","parts":[{"content_type":"text/plain","content":"hello"}]}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("run: %d %s", rec.Code, rec.Body)
	}
	var list agentorcav1alpha1.AgentRunList
	if err := s.crdClient.List(t.Context(), &list); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range list.Items {
		if r.Annotations[AnnotationCreatedBy] == "ada@example.com" {
			found = true
			if r.Labels[tenancy.LabelTenantID] != "tnt_a" || r.Namespace != "team-a" || r.Spec.Input != "hello" {
				t.Errorf("unexpected run: %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("no run recorded the creator")
	}
	if rec := agentsReq(s, catalogTenant, http.MethodGet, "/api/agents/team-a/helper/run", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET run: want 405, got %d", rec.Code)
	}
}

func TestMyRunsPrefersTheUsersOwnRuns(t *testing.T) {
	s := agentsUIServer(t)

	// No run records a creator yet: the tenant's runs are shown, and labelled so.
	rec := agentsReq(s, catalogTenant, http.MethodGet, "/api/my/runs", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"scope":"tenant"`) {
		t.Fatalf("want tenant scope: %d %s", rec.Code, rec.Body)
	}

	_ = agentsReq(s, catalogTenant, http.MethodPost, "/api/agents/team-a/helper/run",
		`{"input":[{"parts":[{"content":"mine"}]}]}`)
	rec = agentsReq(s, catalogTenant, http.MethodGet, "/api/my/runs", "")
	body := rec.Body.String()
	if !strings.Contains(body, `"scope":"user"`) || !strings.Contains(body, `"input":"mine"`) || strings.Contains(body, `"r1"`) {
		t.Fatalf("want only the user's run: %s", body)
	}
}

func TestPlatformTemplatesAreVisibleToEveryTenantButNeverRunnable(t *testing.T) {
	tenancy.SetPlatformNamespaces([]string{"platform-ns"})
	t.Cleanup(func() { tenancy.SetPlatformNamespaces(nil) })

	agent := func(name string, labels map[string]string) *agentorcav1alpha1.Agent {
		return &agentorcav1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "platform-ns", Labels: labels},
			Spec:       agentorcav1alpha1.AgentSpec{ModelSelectorRef: "auto"},
		}
	}
	cl := fake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(
		agent("tpl", map[string]string{tenancy.LabelScope: tenancy.ScopePlatform, LabelTemplate: "true"}),
		agent("platform-tool-agent", map[string]string{tenancy.LabelScope: tenancy.ScopePlatform}),
		agent("private", map[string]string{tenancy.LabelTenantID: "tnt_a"}),
	).Build()
	s := &UIServer{crdClient: cl}
	other := &TenantIdentity{TenantID: "tnt_b", TenantName: "b", Namespaces: []string{"team-b"}, UserID: "bo"}

	rec := agentsReq(s, other, http.MethodGet, "/api/catalog", "")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `"tpl"`) || strings.Contains(body, "private") {
		t.Fatalf("another tenant should see the template and nothing else: %d %s", rec.Code, body)
	}
	if rec := agentsReq(s, other, http.MethodGet, "/api/agents/platform-ns/tpl/manifest", ""); rec.Code != http.StatusOK {
		t.Errorf("template manifest: want 200, got %d", rec.Code)
	}
	if rec := agentsReq(s, other, http.MethodGet, "/api/agents/platform-ns/private/manifest", ""); rec.Code != http.StatusNotFound {
		t.Errorf("non-platform object in the platform namespace: want 404, got %d", rec.Code)
	}
	run := `{"input":[{"parts":[{"content":"hi"}]}]}`
	if rec := agentsReq(s, other, http.MethodPost, "/api/agents/platform-ns/tpl/run", run); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "template") {
		t.Errorf("running a template: want 409, got %d %s", rec.Code, rec.Body)
	}
	// A platform agent that is not a template still cannot run for a tenant outside the namespace.
	if rec := agentsReq(s, other, http.MethodPost, "/api/agents/platform-ns/platform-tool-agent/run", run); rec.Code != http.StatusNotFound {
		t.Errorf("run in a namespace the tenant may not use: want 404, got %d", rec.Code)
	}
	var runs agentorcav1alpha1.AgentRunList
	if err := cl.List(t.Context(), &runs); err != nil || len(runs.Items) != 0 {
		t.Errorf("no run may be created in the platform namespace: %v %d", err, len(runs.Items))
	}
}
