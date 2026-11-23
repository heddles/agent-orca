package apiserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/checkpoint"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	secretChat   = "my password is hunter2 and the key is sk-live-123"
	secretAnswer = "the production token is tok-ABC-999"
	secretNote   = "this agent leaked my ssn 078-05-1120"
)

// requireOneAudit checks the single audit row of a handler: who, what, where,
// and that no message text reached it.
func requireOneAudit(t *testing.T, a *fakeAudit, action, kind, ns, name, user string, secrets ...string) postgresql.AuditEvent {
	t.Helper()
	if len(a.events) != 1 {
		t.Fatalf("audit rows = %+v, want exactly one", a.events)
	}
	e := a.events[0]
	if e.Action != action || e.Kind != kind || e.Namespace != ns || e.Name != name || e.UserID != user {
		t.Errorf("audit = %+v, want %s %s %s/%s by %s", e, action, kind, ns, name, user)
	}
	for _, s := range secrets {
		if strings.Contains(e.Summary, s) {
			t.Errorf("audit summary leaks message content: %q", e.Summary)
		}
	}
	return e
}

func TestExecuteWritesAChatAuditRow(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(&agentorcav1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "assistant", Namespace: "team-a"},
		Spec:       agentorcav1alpha1.AgentDeploymentSpec{AgentRef: "helper"},
	}).Build()
	a := &fakeAudit{}
	s := &UIServer{crdClient: cl, audit: a, checkpoint: checkpoint.NewInMemoryStore()}

	body := `{"input":"` + secretChat + `","sessionId":"sess-1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/deployments/team-a/assistant/execute", strings.NewReader(body)).WithContext(tenantCtx(ada))
	rec := httptest.NewRecorder()
	s.handleExecute(rec, req, "team-a", "assistant")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	e := requireOneAudit(t, a, postgresql.AuditChat, "AgentDeployment", "team-a", "assistant", "ada", secretChat, "hunter2", "sk-live")
	if !strings.Contains(e.Summary, "sess-1") || !strings.Contains(e.Summary, "bytes") {
		t.Errorf("summary = %q, want the session and the size", e.Summary)
	}
}

func TestExecuteWithBadInputWritesNoAuditRow(t *testing.T) {
	a := &fakeAudit{}
	s := &UIServer{audit: a}
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"input":""}`)).WithContext(tenantCtx(ada))
	rec := httptest.NewRecorder()
	s.handleExecute(rec, req, "team-a", "assistant")
	if rec.Code != http.StatusBadRequest || len(a.events) != 0 {
		t.Errorf("status = %d, audit = %+v; want 400 and no row", rec.Code, a.events)
	}
}

func TestAnswerRunWritesAnAnswerAuditRow(t *testing.T) {
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: routingTestRun, Namespace: "team-a"},
		Spec:       agentorcav1alpha1.AgentRunSpec{AgentRef: "helper", Input: "go"},
		Status:     agentorcav1alpha1.AgentRunStatus{Phase: agentorcav1alpha1.AgentRunPhaseWaitingForInput, ClarifyQuestion: "which token?"},
	}
	cl := fake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(run).WithStatusSubresource(run).Build()
	a := &fakeAudit{}
	s := &UIServer{crdClient: cl, audit: a}

	req := httptest.NewRequest(http.MethodPost, "/api/runs/team-a/run-1/answer", strings.NewReader(`{"answer":"`+secretAnswer+`"}`)).WithContext(tenantCtx(ada))
	rec := httptest.NewRecorder()
	s.handleRunOrStream(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	e := requireOneAudit(t, a, postgresql.AuditAnswer, "AgentRun", "team-a", routingTestRun, "ada", secretAnswer, "tok-ABC")
	if !strings.Contains(e.Summary, "continuation run") {
		t.Errorf("summary = %q", e.Summary)
	}

	// A refused answer (the run is no longer waiting) is not audited.
	a.events = nil
	rec = httptest.NewRecorder()
	s.handleRunOrStream(rec, httptest.NewRequest(http.MethodPost, "/api/runs/team-a/run-1/answer", strings.NewReader(`{"answer":"again"}`)).WithContext(tenantCtx(ada)))
	if rec.Code != http.StatusConflict || len(a.events) != 0 {
		t.Errorf("second answer: status = %d, audit = %+v; want 409 and no row", rec.Code, a.events)
	}
}

func TestRunFeedbackWritesAFeedbackAuditRow(t *testing.T) {
	store := &fakeFeedbackStore{}
	s := feedbackUIServer(t, store)
	a := &fakeAudit{}
	s.audit = a

	rec := feedbackReq(s, tenantCtx(ada), http.MethodPost, "/api/runs/team-a/run-1/feedback",
		`{"rating":-1,"comment":"`+secretNote+`","decisionId":"d-1"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	e := requireOneAudit(t, a, postgresql.AuditFeedback, "AgentRun", "team-a", routingTestRun, "ada", secretNote, "078-05")
	if !strings.Contains(e.Summary, "d-1") || !strings.Contains(e.Summary, "down") || !strings.Contains(e.Summary, "comment 36 bytes") {
		t.Errorf("summary = %q", e.Summary)
	}

	// Reading feedback and a rejected rating write nothing.
	a.events = nil
	feedbackReq(s, tenantCtx(ada), http.MethodGet, "/api/runs/team-a/run-1/feedback", "")
	feedbackReq(s, tenantCtx(ada), http.MethodPost, "/api/runs/team-a/run-1/feedback", `{"rating":7}`)
	if len(a.events) != 0 {
		t.Errorf("audit = %+v, want none for a read and an invalid rating", a.events)
	}
}

func TestConversationAuditNeedsTheRunnerRoleAndIsTenantScoped(t *testing.T) {
	store := &fakeFeedbackStore{}
	s := feedbackUIServer(t, store)
	a := &fakeAudit{}
	s.audit = a
	viewer := &TenantIdentity{Roles: tenancy.RoleViewer.Expand(), TenantName: shadowTenant, Namespaces: []string{"team-a"}, UserID: "vic"}
	if rec := feedbackReq(s, tenantCtx(viewer), http.MethodPost, "/api/runs/team-a/run-1/feedback", `{"rating":1}`); rec.Code != http.StatusForbidden || len(a.events) != 0 {
		t.Errorf("viewer: status = %d, audit = %+v; want 403 and no row", rec.Code, a.events)
	}
	other := &TenantIdentity{Roles: testAdminRoles, TenantName: shadowTenant, Namespaces: []string{"team-z"}, UserID: "eve"}
	if rec := feedbackReq(s, tenantCtx(other), http.MethodPost, "/api/runs/team-a/run-1/feedback", `{"rating":1}`); rec.Code != http.StatusForbidden || len(a.events) != 0 {
		t.Errorf("other tenant: status = %d, audit = %+v; want 403 and no row", rec.Code, a.events)
	}
}
