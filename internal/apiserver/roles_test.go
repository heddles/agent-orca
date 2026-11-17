package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

var (
	allRoles = []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner, tenancy.RoleBuilder, tenancy.RoleAdmin}
)

// roleTenant is an identity holding exactly role (and the roles below it).
func roleTenant(role tenancy.Role) *TenantIdentity {
	return &TenantIdentity{
		TenantID: isoA, TenantName: "acme", Namespace: "ns", Namespaces: []string{"ns"},
		UserID: "ada", Roles: role.Expand(),
	}
}

func serveAs(h http.Handler, id *TenantIdentity, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if id != nil {
		req = asTenant(req, id)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func isRoleDenial(rec *httptest.ResponseRecorder) bool {
	return rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "role in your tenant")
}

type roleCase struct {
	name, method, path, body string
	min                      tenancy.Role
}

func runRoleMatrix(t *testing.T, h http.Handler, cases []roleCase) {
	t.Helper()
	for _, tc := range cases {
		for _, role := range allRoles {
			t.Run(tc.name+"/"+string(role), func(t *testing.T) {
				rec := serveAs(h, roleTenant(role), tc.method, tc.path, tc.body)
				wantDenied := !role.Includes(tc.min)
				if got := isRoleDenial(rec); got != wantDenied {
					t.Errorf("%s %s as %s: code %d body %q; role denial = %v, want %v",
						tc.method, tc.path, role, rec.Code, rec.Body.String(), got, wantDenied)
				}
			})
		}
		// No identity (authentication disabled) is not subject to roles.
		t.Run(tc.name+"/no identity", func(t *testing.T) {
			if rec := serveAs(h, nil, tc.method, tc.path, tc.body); isRoleDenial(rec) {
				t.Errorf("%s %s without an identity was denied: %s", tc.method, tc.path, rec.Body.String())
			}
		})
	}
}

func TestUIRoleMatrix(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(listCRDTestScheme(t)).Build()
	s := &UIServer{crdClient: c, approvals: &fakeApprovals{rows: map[string]postgresql.Approval{}}}
	cases := []roleCase{
		{"list runs", http.MethodGet, "/api/runs", "", tenancy.RoleViewer},
		{"list approvals", http.MethodGet, "/api/approvals", "", tenancy.RoleViewer},
		{"list resources", http.MethodGet, "/api/resources/agents", "", tenancy.RoleViewer},
		{"read feedback", http.MethodGet, "/api/runs/ns/r/feedback", "", tenancy.RoleViewer},
		{"stop run", http.MethodPost, "/api/runs/ns/r/stop", "", tenancy.RoleRunner},
		{"answer run", http.MethodPost, "/api/runs/ns/r/answer", `{"answer":"x"}`, tenancy.RoleRunner},
		{"send feedback", http.MethodPost, "/api/runs/ns/r/feedback", `{}`, tenancy.RoleRunner},
		{"chat execute", http.MethodPost, "/api/deployments/ns/d/execute", `{"input":"hi"}`, tenancy.RoleRunner},
		{"chat save response", http.MethodPost, "/api/deployments/ns/d/complete", `{}`, tenancy.RoleRunner},
		{"retry workflow step", http.MethodPost, "/api/workflows/ns/w/steps/s/retry", "", tenancy.RoleRunner},
		{"create agent", http.MethodPost, "/api/agents", `{}`, tenancy.RoleBuilder},
		{"delete deployment", http.MethodDelete, "/api/deployments/ns/d", "", tenancy.RoleBuilder},
		{"routing recommendation action", http.MethodPost, "/api/routing/recommendations/ns/r/action", `{}`, tenancy.RoleBuilder},
		{"decide approval", http.MethodPost, "/api/approvals/a1/approve", `{}`, tenancy.RoleAdmin},
		{"reject approval", http.MethodPost, "/api/approvals/a1/reject", `{}`, tenancy.RoleAdmin},
	}
	runRoleMatrix(t, s.Handler(), cases)
}

func TestACPRoleMatrix(t *testing.T) {
	s := newACPServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/runs", s.handleCreateRun)
	mux.HandleFunc("/runs/", s.handleRunByID)
	runRoleMatrix(t, mux, []roleCase{
		{"create run", http.MethodPost, "/runs", `{}`, tenancy.RoleRunner},
		{"cancel run", http.MethodPost, "/runs/r1/cancel", "", tenancy.RoleRunner},
		{"fs pending", http.MethodGet, "/runs/r1/fs/pending", "", tenancy.RoleRunner},
		{"fs respond", http.MethodPost, "/runs/r1/fs/pending/q1/response", `{}`, tenancy.RoleRunner},
		{"get run", http.MethodGet, "/runs/r1", "", tenancy.RoleViewer},
		{"run events", http.MethodGet, "/runs/r1/events", "", tenancy.RoleViewer},
	})
}

func TestACPResumeNeedsRunner(t *testing.T) {
	s := newACPServer(t)
	run := &agentorcav1alpha1.AgentRun{}
	for _, role := range allRoles {
		rec := httptest.NewRecorder()
		req := asTenant(httptest.NewRequest(http.MethodPost, "/runs/r1", strings.NewReader(`{}`)), roleTenant(role))
		s.resumeACPRun(rec, req, run)
		if got, want := isRoleDenial(rec) || strings.Contains(rec.Body.String(), "runner role"), !role.Includes(tenancy.RoleRunner); got != want {
			t.Errorf("resume as %s: denied = %v, want %v (%d %s)", role, got, want, rec.Code, rec.Body.String())
		}
	}
}

func TestACPAgentRunNeedsRunner(t *testing.T) {
	s := newACPServer(t)
	for _, role := range allRoles {
		rec := httptest.NewRecorder()
		req := asTenant(httptest.NewRequest(http.MethodPost, "/agents/a/run", strings.NewReader(`{}`)), roleTenant(role))
		s.handleAgentRun(rec, req, roleTenant(role), &agentorcav1alpha1.Agent{})
		if got, want := rec.Code == http.StatusForbidden, !role.Includes(tenancy.RoleRunner); got != want {
			t.Errorf("agent run as %s: %d %s", role, rec.Code, rec.Body.String())
		}
	}
}

func TestExternalTaskRoleMatrix(t *testing.T) {
	s := &ExternalAPIServer{auth: &ExternalAuth{}, crdClient: fake.NewClientBuilder().WithScheme(adminScheme(t)).Build()}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tasks", s.handleTasks)
	mux.HandleFunc("/v1/tasks/", s.handleTaskByID)
	runRoleMatrix(t, mux, []roleCase{
		{"submit task", http.MethodPost, "/v1/tasks", `{"agent":"a","input":"x"}`, tenancy.RoleRunner},
		{"answer task", http.MethodPost, "/v1/tasks/t1/answer", `{"answer":"x"}`, tenancy.RoleRunner},
		{"task feedback", http.MethodPost, "/v1/tasks/t1/feedback", `{}`, tenancy.RoleRunner},
		{"cancel task", http.MethodDelete, "/v1/tasks/t1", "", tenancy.RoleRunner},
		{"get task", http.MethodGet, "/v1/tasks/t1", "", tenancy.RoleViewer},
		{"list tasks", http.MethodGet, "/v1/tasks", "", tenancy.RoleViewer},
	})
}

// An identity that holds no role cannot read either, except to ask /api/me.
func TestNoRoleCannotEvenRead(t *testing.T) {
	s := &UIServer{crdClient: fake.NewClientBuilder().WithScheme(listCRDTestScheme(t)).Build()}
	inner := s.Handler()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = asTenant(r, &TenantIdentity{TenantID: isoA, Namespaces: []string{"ns"}, UserID: "ada"})
		if requireViewer(w, r) {
			inner.ServeHTTP(w, r)
		}
	})
	if rec := serveAs(h, nil, http.MethodGet, "/api/runs", ""); rec.Code != http.StatusForbidden {
		t.Errorf("read with no role = %d, want 403", rec.Code)
	}
	if rec := serveAs(h, nil, http.MethodGet, "/api/me", ""); rec.Code != http.StatusOK {
		t.Errorf("/api/me with no role = %d, want 200", rec.Code)
	}
}

// ── resolution ──────────────────────────────────────────────────────────────

func rolesAuth(t *testing.T, roles *agentorcav1alpha1.TenantRoles) (*ExternalAuth, *agentorcav1alpha1.TenantConfig) {
	t.Helper()
	tc := tenantWithID("acme", tidNew)
	tc.Spec.AllowedNamespaces = []string{defaultNamespace}
	tc.Spec.Roles = roles
	a := testAuth(t, testRSAKey(t), tc)
	a.SetTenantRegistry(registryWith(rec(tidNew, "acme", tenancy.StateActive)))
	return a, tc
}

func sessionFor(t *testing.T, a *ExternalAuth, user string, groups ...string) *TenantIdentity {
	t.Helper()
	tok, err := a.MintSessionForIdentity(&TenantIdentity{
		TenantID: tidNew, TenantName: "acme", Namespace: defaultNamespace, Namespaces: []string{defaultNamespace},
		UserID: user, Groups: groups,
		Roles: tenancy.RoleAdmin.Expand(), // a token cannot grant itself roles
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := a.validateIssuedToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRolesResolveFromGroupsAndLiveTenantConfig(t *testing.T) {
	a, tc := rolesAuth(t, nil)

	// Roles unset: everyone signed in is a runner.
	if got := sessionFor(t, a, "ada", "anything").Roles; !slices.Equal(got, []string{"viewer", "runner"}) {
		t.Errorf("roles unset: %v", got)
	}

	tc.Spec.Roles = &agentorcav1alpha1.TenantRoles{Builder: []string{"devs"}, Admin: []string{"platform"}, Viewer: []string{"all"}}
	tok, _ := a.MintSessionForIdentity(&TenantIdentity{TenantID: tidNew, TenantName: "acme", Namespace: defaultNamespace,
		Namespaces: []string{defaultNamespace}, UserID: "ada", Groups: []string{"devs"}})
	for want, groups := range map[string][]string{"builder": {"devs"}, "admin": {"devs", "platform"}, "viewer": {"all"}} {
		id := sessionFor(t, a, "u", groups...)
		if !slices.Contains(id.Roles, want) || id.Roles[len(id.Roles)-1] != want {
			t.Errorf("groups %v: roles %v, want top role %s", groups, id.Roles, want)
		}
	}
	if id := sessionFor(t, a, "u", "strangers"); len(id.Roles) != 0 {
		t.Errorf("unlisted group with no default: %v, want none", id.Roles)
	}

	// A change to the TenantConfig reaches a session already issued.
	before, err := a.validateIssuedToken(tok)
	if err != nil || before.Roles[len(before.Roles)-1] != "builder" {
		t.Fatalf("before: %v %v", before, err)
	}
	tc.Spec.Roles = &agentorcav1alpha1.TenantRoles{Viewer: []string{"devs"}}
	after, err := a.validateIssuedToken(tok)
	if err != nil || !slices.Equal(after.Roles, []string{"viewer"}) {
		t.Errorf("after the demotion: %v %v", after.Roles, err)
	}
}

func TestMachineCallersGetTheMachineDefaultRole(t *testing.T) {
	tc := idTenant("acme", isoA, "a-ns")
	tc.Spec.Roles = &agentorcav1alpha1.TenantRoles{Admin: []string{"platform"}} // groups do not matter to a machine
	a := saAuth(t, "system:serviceaccount:a-ns:ci", tc)
	a.SetTenantRegistry(nil)

	id, err := a.validateK8sToken(context.Background(), "tok")
	if err != nil || !slices.Equal(id.Roles, []string{"viewer", "runner"}) {
		t.Errorf("service account, default: %v %v", id.Roles, err)
	}
	if err := a.SetMachineDefaultRole("viewer"); err != nil {
		t.Fatal(err)
	}
	if id, _ = a.validateK8sToken(context.Background(), "tok"); !slices.Equal(id.Roles, []string{"viewer"}) {
		t.Errorf("service account, machineDefaultRole viewer: %v", id.Roles)
	}
	if err := a.SetMachineDefaultRole("root"); err == nil {
		t.Error("an unknown machine default role was accepted")
	}

	// An issued client-credentials session has no user either.
	a2, _ := rolesAuth(t, &agentorcav1alpha1.TenantRoles{Admin: []string{"platform"}})
	if err := a2.SetMachineDefaultRole("builder"); err != nil {
		t.Fatal(err)
	}
	tok, _ := a2.MintSessionForIdentity(&TenantIdentity{TenantID: tidNew, TenantName: "acme", Namespace: defaultNamespace, Namespaces: []string{defaultNamespace}})
	got, err := a2.validateIssuedToken(tok)
	if err != nil || got.Roles[len(got.Roles)-1] != "builder" {
		t.Errorf("client credentials: %v %v", got, err)
	}
}

func TestHandleMeReportsRoles(t *testing.T) {
	s := &UIServer{}
	for _, role := range allRoles {
		rec := httptest.NewRecorder()
		s.handleMe(rec, asTenant(httptest.NewRequest(http.MethodGet, "/api/me", nil), roleTenant(role)))
		var out meResponse
		decodeJSON(t, rec, &out)
		if !slices.Equal(out.Roles, role.Expand()) {
			t.Errorf("%s: roles = %v", role, out.Roles)
		}
	}
	rec := httptest.NewRecorder()
	s.handleMe(rec, asTenant(httptest.NewRequest(http.MethodGet, "/api/me", nil), &TenantIdentity{TenantID: isoA, UserID: "x"}))
	var out meResponse
	decodeJSON(t, rec, &out)
	if out.Roles == nil || len(out.Roles) != 0 {
		t.Errorf("no role: %v", out.Roles)
	}
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
}
