package apiserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// fakeAudit is an in-memory audit store; fail makes every append fail.
type fakeAudit struct {
	mu       sync.Mutex
	events   []postgresql.AuditEvent
	fail     bool
	attempts int
	lastList postgresql.AuditQuery
}

func (f *fakeAudit) AppendAudit(_ context.Context, e postgresql.AuditEvent) (postgresql.AuditEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.fail {
		return e, errors.New("postgres is down")
	}
	e.ID = int64(len(f.events) + 1)
	f.events = append(f.events, e)
	return e, nil
}

func (f *fakeAudit) ListAudit(_ context.Context, q postgresql.AuditQuery) ([]postgresql.AuditEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastList = q
	var out []postgresql.AuditEvent
	for i := len(f.events) - 1; i >= 0; i-- { // newest first
		e := f.events[i]
		if q.Before > 0 && e.ID >= q.Before {
			continue
		}
		if len(q.Namespaces) > 0 && !slices.Contains(q.Namespaces, e.Namespace) {
			continue
		}
		out = append(out, e)
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

type writeFixture struct {
	t     *testing.T
	s     *UIServer
	c     client.Client
	audit *fakeAudit
}

func newWriteFixture(t *testing.T, objs ...client.Object) *writeFixture {
	t.Helper()
	isoMode(t, tenancy.ModeEnforce)
	c := fake.NewClientBuilder().WithScheme(listCRDTestScheme(t)).WithObjects(objs...).Build()
	a := &fakeAudit{}
	return &writeFixture{t: t, s: &UIServer{crdClient: c, audit: a}, c: c, audit: a}
}

// as is an identity of tenant A holding role, with a name to find in the audit.
func (f *writeFixture) as(role tenancy.Role) *TenantIdentity {
	id := tenantA()
	id.Namespace, id.Namespaces = "shared", []string{"shared"}
	id.UserID, id.Roles = "ada", role.Expand() //nolint:goconst
	return id
}

func (f *writeFixture) do(id *TenantIdentity, method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if id != nil {
		req = asTenant(req, id)
	}
	rec := httptest.NewRecorder()
	switch {
	case strings.Count(strings.Trim(path, "/"), "/") >= 4:
		f.s.handleResource(rec, req)
	default:
		f.s.handleResourceCollection(rec, req)
	}
	return rec
}

func (f *writeFixture) tool(name string) *agentorcav1alpha1.Tool {
	f.t.Helper()
	var tl agentorcav1alpha1.Tool
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: "shared", Name: name}, &tl); err != nil {
		f.t.Fatalf("get tool %s: %v", name, err)
	}
	return &tl
}

const newAgentBody = `{"apiVersion":"agentorca.agentorca.io/v1alpha1","kind":"Agent",
 "metadata":{"name":"bot","namespace":"elsewhere","labels":{"keep":"me","app.kubernetes.io/managed-by":"argocd"},
   "annotations":{"agentorca.io/owner":"git","agentorca.io/last-modified-by":"mallory","note":"hi"}},
 "spec":{"modelSelectorRef":"sel","runtime":{"ociRef":"registry.example/a:1"}},
 "status":{"serviceAccountName":"evil"}}`

func TestCreateResourceOwnership(t *testing.T) {
	f := newWriteFixture(t)
	rec := f.do(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/resources/agents", newAgentBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var got agentorcav1alpha1.Agent
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: "shared", Name: "bot"}, &got); err != nil {
		t.Fatalf("created in the tenant's namespace, not the body's: %v", err)
	}
	a := got.Annotations
	if a[tenancy.AnnotationOwner] != tenancy.OwnerUI {
		t.Errorf("owner = %q, want ui (the body's %q must be dropped)", a[tenancy.AnnotationOwner], "git")
	}
	if a[tenancy.AnnotationLastModifiedBy] != "ada" || a[tenancy.AnnotationLastModifiedAt] == "" || a["note"] != "hi" {
		t.Errorf("annotations = %v", a)
	}
	if got.Labels["app.kubernetes.io/managed-by"] != "argocd" || got.Labels["keep"] != "me" {
		t.Errorf("the UI must not touch managed-by or other labels: %v", got.Labels)
	}
	if got.Labels[tenancy.LabelTenantID] != isoA {
		t.Errorf("tenant label = %v", got.Labels)
	}
	if got.Status.ServiceAccountName != "" {
		t.Errorf("status taken from the body: %+v", got.Status)
	}
	if len(f.audit.events) != 1 || f.audit.events[0].Action != postgresql.AuditCreate || f.audit.events[0].UserID != "ada" ||
		f.audit.events[0].Kind != "Agent" || f.audit.events[0].Name != "bot" || f.audit.events[0].ResourceVersion == "" {
		t.Errorf("audit = %+v", f.audit.events)
	}

	// The same name again is a 409.
	rec = f.do(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/resources/agents", newAgentBody)
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate create = %d", rec.Code)
	}
}

func TestCreateResourceNoManagedByStamp(t *testing.T) {
	f := newWriteFixture(t)
	body := `{"metadata":{"name":"plain","namespace":"shared"},"spec":{"modelSelectorRef":"s","runtime":{"ociRef":"r"}}}`
	if rec := f.do(nil, http.MethodPost, "/api/resources/agents", body); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var got agentorcav1alpha1.Agent
	_ = f.c.Get(context.Background(), client.ObjectKey{Namespace: "shared", Name: "plain"}, &got)
	if _, has := got.Labels["app.kubernetes.io/managed-by"]; has {
		t.Errorf("the UI stamped managed-by: %v", got.Labels)
	}
	if got.Annotations[tenancy.AnnotationOwner] != tenancy.OwnerUI || got.Annotations[tenancy.AnnotationLastModifiedBy] != "anonymous" {
		t.Errorf("annotations = %v", got.Annotations)
	}
}

func TestCreateResourceValidation(t *testing.T) {
	f := newWriteFixture(t)
	tests := []struct {
		name, body string
		want       int
	}{
		{"not JSON", `nope`, http.StatusBadRequest},
		{"not an object", `[1]`, http.StatusBadRequest},
		{"wrong kind", `{"kind":"Tool","metadata":{"name":"x","namespace":"shared"}}`, http.StatusBadRequest},
		{"wrong field type", `{"metadata":{"name":"x"},"spec":{"tools":"not-a-list"}}`, http.StatusBadRequest},
		{"no name", `{"metadata":{"namespace":"shared"}}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec := f.do(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/resources/agents", tt.body); rec.Code != tt.want {
				t.Errorf("code = %d %s, want %d", rec.Code, rec.Body.String(), tt.want)
			}
		})
	}
}

func uiOwnedTool(name string) *agentorcav1alpha1.Tool {
	return isoTool("shared", name, tenancy.LabelTenantID, isoA)
}

func TestUpdateResource(t *testing.T) {
	owned := uiOwnedTool("owned")
	owned.ResourceVersion = ""
	gitTool := isoTool("shared", "from-git", tenancy.LabelTenantID, isoA)
	gitTool.Annotations = map[string]string{tenancy.AnnotationOwner: "git"}
	unowned := isoTool("shared", "unowned", tenancy.LabelTenantID, isoA)
	unowned.Annotations = nil
	f := newWriteFixture(t, owned, gitTool, unowned)

	rv := f.tool("owned").ResourceVersion
	body := func(name, rv, extra string) string {
		r := ""
		if rv != "" {
			r = `"resourceVersion":"` + rv + `",`
		}
		return `{"metadata":{` + r + `"name":"` + name + `","annotations":{"agentorca.io/owner":"git"},"labels":{"agentorca.io/tenant-id":"` + isoB + `"}},"spec":{"ociRef":"edited"}` + extra + `}`
	}
	builder := f.as(tenancy.RoleAdmin) // Tool is an admin kind

	t.Run("updates an owned object and keeps ownership", func(t *testing.T) {
		rec := f.do(builder, http.MethodPut, "/api/resources/tools/shared/owned", body("owned", rv, `,"status":{"x":1}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
		}
		got := f.tool("owned")
		if got.Spec.OCIRef != "edited" || got.Annotations[tenancy.AnnotationOwner] != tenancy.OwnerUI ||
			got.Annotations[tenancy.AnnotationLastModifiedBy] != "ada" || got.Labels[tenancy.LabelTenantID] != isoA {
			t.Errorf("tool = %+v", got.ObjectMeta)
		}
		if got.ResourceVersion == rv {
			t.Error("resourceVersion did not move")
		}
		last := f.audit.events[len(f.audit.events)-1]
		if last.Action != postgresql.AuditUpdate || last.Name != "owned" || last.ResourceVersion != got.ResourceVersion {
			t.Errorf("audit = %+v", last)
		}
	})
	t.Run("a stale resourceVersion is a 409 naming the current one", func(t *testing.T) {
		rec := f.do(builder, http.MethodPut, "/api/resources/tools/shared/owned", body("owned", rv, ""))
		if rec.Code != http.StatusConflict {
			t.Fatalf("stale update = %d %s", rec.Code, rec.Body.String())
		}
		var out map[string]string
		decodeJSON(t, rec, &out)
		if out["currentResourceVersion"] != f.tool("owned").ResourceVersion || out["error"] == "" {
			t.Errorf("body = %v", out)
		}
	})
	t.Run("no resourceVersion is a 428", func(t *testing.T) {
		if rec := f.do(builder, http.MethodPut, "/api/resources/tools/shared/owned", body("owned", "", "")); rec.Code != http.StatusPreconditionRequired {
			t.Errorf("code = %d", rec.Code)
		}
	})
	t.Run("an object owned by git is a 409", func(t *testing.T) {
		rec := f.do(builder, http.MethodPut, "/api/resources/tools/shared/from-git", body("from-git", f.tool("from-git").ResourceVersion, ""))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not owned by the UI") {
			t.Errorf("code = %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("an object with no owner is a 409", func(t *testing.T) {
		rec := f.do(builder, http.MethodPut, "/api/resources/tools/shared/unowned", body("unowned", f.tool("unowned").ResourceVersion, ""))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not owned by the UI") {
			t.Errorf("code = %d %s", rec.Code, rec.Body.String())
		}
		if f.tool("unowned").Spec.OCIRef == "edited" {
			t.Error("the update went through")
		}
	})
	t.Run("a missing object is a 404", func(t *testing.T) {
		if rec := f.do(builder, http.MethodPut, "/api/resources/tools/shared/ghost", body("ghost", "1", "")); rec.Code != http.StatusNotFound {
			t.Errorf("code = %d", rec.Code)
		}
	})
}

func TestDeleteResourceOwnership(t *testing.T) {
	gitTool := isoTool("shared", "from-git", tenancy.LabelTenantID, isoA)
	gitTool.Annotations = map[string]string{tenancy.AnnotationOwner: "git"}
	// What the old UI API left behind: a managed-by label and nothing else.
	legacy := isoTool("shared", "legacy-ui", tenancy.LabelTenantID, isoA)
	legacy.Annotations = nil
	f := newWriteFixture(t, uiOwnedTool("mine"), gitTool, legacy)
	admin := f.as(tenancy.RoleAdmin)

	for _, name := range []string{"from-git", "legacy-ui"} {
		rec := f.do(admin, http.MethodDelete, "/api/resources/tools/shared/"+name, "")
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not owned by the UI") {
			t.Errorf("delete %s = %d %s, want 403", name, rec.Code, rec.Body.String())
		}
		f.tool(name) // still there
	}
	if rec := f.do(admin, http.MethodDelete, "/api/resources/tools/shared/mine", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete owned = %d %s", rec.Code, rec.Body.String())
	}
	if err := f.c.Get(context.Background(), client.ObjectKey{Namespace: "shared", Name: "mine"}, &agentorcav1alpha1.Tool{}); err == nil {
		t.Error("owned tool survived")
	}
	if len(f.audit.events) != 1 || f.audit.events[0].Action != postgresql.AuditDelete || f.audit.events[0].Name != "mine" {
		t.Errorf("audit = %+v", f.audit.events)
	}
}

// Kinds that reach beyond one agent need the admin role; the rest need builder.
func TestResourceWriteRoleByKind(t *testing.T) {
	f := newWriteFixture(t)
	kinds := map[string]tenancy.Role{
		"agents": tenancy.RoleBuilder, "knowledgebases": tenancy.RoleBuilder, "agentdeployments": tenancy.RoleBuilder, "agentworkflows": tenancy.RoleBuilder,
		"tools": tenancy.RoleAdmin, "mcpservers": tenancy.RoleAdmin, "modelproviders": tenancy.RoleAdmin, "modelselectors": tenancy.RoleAdmin,
	}
	for kind, min := range kinds {
		for _, role := range allRoles {
			for _, req := range []struct{ method, path, body string }{
				{http.MethodPost, "/api/resources/" + kind, `{}`},
				{http.MethodPut, "/api/resources/" + kind + "/shared/x", `{}`},
				{http.MethodDelete, "/api/resources/" + kind + "/shared/x", ""},
			} {
				rec := f.do(f.as(role), req.method, req.path, req.body)
				if denied, want := isRoleDenial(rec), !role.Includes(min); denied != want {
					t.Errorf("%s %s as %s: %d %s; role denial = %v, want %v", req.method, req.path, role, rec.Code, rec.Body.String(), denied, want)
				}
			}
		}
	}
	if len(f.audit.events) != 0 {
		t.Errorf("failed writes were audited: %+v", f.audit.events)
	}
}

func TestResourceReadsNeedOnlyViewer(t *testing.T) {
	f := newWriteFixture(t, uiOwnedTool("mine"))
	for _, path := range []string{"/api/resources/tools", "/api/resources/tools/shared/mine"} {
		if rec := f.do(f.as(tenancy.RoleViewer), http.MethodGet, path, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s as viewer = %d", path, rec.Code)
		}
	}
}

// ── audit failure policy ────────────────────────────────────────────────────

func TestAuditFailureDoesNotFailTheWriteButIsLoud(t *testing.T) {
	f := newWriteFixture(t)
	f.audit.fail = true
	rec := f.do(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/resources/agents", newAgentBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: the change happened, so the request must not fail", rec.Code)
	}
	if rec.Header().Get("X-Audit-Status") != "failed" {
		t.Error("no X-Audit-Status: failed header")
	}
	if f.audit.attempts != auditAttempts {
		t.Errorf("attempts = %d, want %d", f.audit.attempts, auditAttempts)
	}
	// The annotation still names the actor.
	var got agentorcav1alpha1.Agent
	_ = f.c.Get(context.Background(), client.ObjectKey{Namespace: "shared", Name: "bot"}, &got)
	if got.Annotations[tenancy.AnnotationLastModifiedBy] != "ada" {
		t.Errorf("annotations = %v", got.Annotations)
	}
}

func TestAuditWithoutStoreStillWrites(t *testing.T) {
	f := newWriteFixture(t)
	f.s.audit = nil // no PostgreSQL
	rec := f.do(f.as(tenancy.RoleBuilder), http.MethodPost, "/api/resources/agents", newAgentBody)
	if rec.Code != http.StatusCreated || rec.Header().Get("X-Audit-Status") != "" {
		t.Errorf("create = %d, audit header %q", rec.Code, rec.Header().Get("X-Audit-Status"))
	}
}

func TestMachineActorIsNamed(t *testing.T) {
	f := newWriteFixture(t)
	id := f.as(tenancy.RoleBuilder)
	id.UserID = ""
	f.do(id, http.MethodPost, "/api/resources/agents", newAgentBody)
	if got := f.audit.events[0].UserID; got != "machine:acme" {
		t.Errorf("actor = %q", got)
	}
}

// ── GET /api/audit ──────────────────────────────────────────────────────────

func TestAuditEndpoint(t *testing.T) {
	f := newWriteFixture(t)
	f.audit.events = []postgresql.AuditEvent{
		{ID: 1, Action: "create", Kind: "Agent", Namespace: "shared", Name: "a"},
		{ID: 2, Action: "update", Kind: "Agent", Namespace: "shared", Name: "a"},
		{ID: 3, Action: "delete", Kind: "Agent", Namespace: "other-tenants-ns", Name: "z"},
		{ID: 4, Action: "create", Kind: "Tool", Namespace: "shared", Name: "t"},
	}
	get := func(id *TenantIdentity, query string) (*httptest.ResponseRecorder, auditPage) {
		req := httptest.NewRequest(http.MethodGet, "/api/audit"+query, nil)
		if id != nil {
			req = asTenant(req, id)
		}
		rec := httptest.NewRecorder()
		f.s.handleAudit(rec, req)
		var page auditPage
		if rec.Code == http.StatusOK {
			decodeJSON(t, rec, &page)
		}
		return rec, page
	}

	for _, role := range []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner, tenancy.RoleBuilder} {
		if rec, _ := get(f.as(role), ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s reads the audit log: %d", role, rec.Code)
		}
	}
	rec, page := get(f.as(tenancy.RoleAdmin), "")
	if rec.Code != http.StatusOK || len(page.Events) != 3 || page.Events[0].ID != 4 || page.NextCursor != "" {
		t.Fatalf("admin read = %d %+v", rec.Code, page)
	}
	if !slices.Equal(f.audit.lastList.Namespaces, []string{"shared"}) {
		t.Errorf("the query was not limited to the tenant's namespaces: %+v", f.audit.lastList)
	}

	// Pagination: two per page, then the rest.
	_, page = get(f.as(tenancy.RoleAdmin), "?limit=2")
	if len(page.Events) != 2 || page.Events[1].ID != 2 || page.NextCursor != "2" {
		t.Fatalf("page 1 = %+v", page)
	}
	_, page = get(f.as(tenancy.RoleAdmin), "?limit=2&cursor="+page.NextCursor)
	if len(page.Events) != 1 || page.Events[0].ID != 1 || page.NextCursor != "" {
		t.Errorf("page 2 = %+v", page)
	}

	if rec, _ := get(f.as(tenancy.RoleAdmin), "?namespace=other-tenants-ns"); rec.Code != http.StatusForbidden {
		t.Errorf("another namespace = %d", rec.Code)
	}
	for _, q := range []string{"?limit=0", "?limit=x", "?limit=501", "?cursor=abc", "?cursor=-1"} {
		if rec, _ := get(f.as(tenancy.RoleAdmin), q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", q, rec.Code)
		}
	}
	// Without PostgreSQL there is no trail to read.
	f.s.audit = nil
	if rec, _ := get(f.as(tenancy.RoleAdmin), ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no store = %d", rec.Code)
	}
}

// ── other audited writes ────────────────────────────────────────────────────

func TestApprovalDecisionIsAudited(t *testing.T) {
	s, _ := approvalsServer()
	a := &fakeAudit{}
	s.audit = a
	req := asUser(httptest.NewRequest(http.MethodPost, "/api/approvals/a2/approve", strings.NewReader(`{"reason":"ok"}`)), "alice", "platform")
	rec := httptest.NewRecorder()
	s.handleApproval(rec, req)
	if rec.Code != http.StatusOK || len(a.events) != 1 {
		t.Fatalf("decide = %d, audit = %+v", rec.Code, a.events)
	}
	if e := a.events[0]; e.Action != postgresql.AuditApprove || e.UserID != "alice" || e.Kind != "Approval" || e.Name != "a2" || e.Namespace != "team-a" {
		t.Errorf("audit = %+v", e)
	}

	// A decision that did not happen leaves no event.
	rec = httptest.NewRecorder()
	s.handleApproval(rec, asUser(httptest.NewRequest(http.MethodPost, "/api/approvals/a2/reject", nil), "alice"))
	if rec.Code != http.StatusConflict || len(a.events) != 1 {
		t.Errorf("second decision = %d, events = %d", rec.Code, len(a.events))
	}
}

func TestCreateAgentIsStampedAndAudited(t *testing.T) {
	f := newWriteFixture(t)
	body := `{"name":"chatty","namespace":"shared","modelSelectorRef":"sel","ociRef":"registry.example/a:1"}`
	req := asTenant(httptest.NewRequest(http.MethodPost, "/api/agents", strings.NewReader(body)), f.as(tenancy.RoleBuilder))
	rec := httptest.NewRecorder()
	f.s.handleAgents(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var ag agentorcav1alpha1.Agent
	var dep agentorcav1alpha1.AgentDeployment
	key := client.ObjectKey{Namespace: "shared", Name: "chatty"}
	if err := f.c.Get(context.Background(), key, &ag); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Get(context.Background(), key, &dep); err != nil {
		t.Fatal(err)
	}
	for _, o := range []metav1.Object{&ag, &dep} {
		if o.GetAnnotations()[tenancy.AnnotationOwner] != tenancy.OwnerUI || o.GetAnnotations()[tenancy.AnnotationLastModifiedBy] != "ada" {
			t.Errorf("annotations = %v", o.GetAnnotations())
		}
	}
	if len(f.audit.events) != 2 {
		t.Errorf("audit = %+v", f.audit.events)
	}
}
