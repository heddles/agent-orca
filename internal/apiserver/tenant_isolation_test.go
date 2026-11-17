package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/security"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	isoA = "tnt_01935c7e-aaaa-7aaa-8bbb-00000000000a"
	isoB = "tnt_01935c7e-bbbb-7aaa-8bbb-00000000000b"
)

func isoMode(t *testing.T, m tenancy.Mode) {
	t.Helper()
	prev := tenancy.CurrentMode()
	tenancy.SetMode(m)
	t.Cleanup(func() { tenancy.SetMode(prev) })
}

func isoTool(ns, name string, kv ...string) *agentorcav1alpha1.Tool {
	t := &agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app.kubernetes.io/managed-by": security.ManagedByValue}, Annotations: map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI}}}
	for i := 0; i < len(kv); i += 2 {
		t.Labels[kv[i]] = kv[i+1]
	}
	return t
}

func tenantA() *TenantIdentity {
	return &TenantIdentity{Roles: testAdminRoles, TenantID: isoA, TenantName: "acme", Namespace: "shared", Namespaces: []string{"shared", "a-only"}}
}

func asTenant(r *http.Request, t *TenantIdentity) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), tenantIdentityKey, t))
}

func toolBody(t *testing.T, labels map[string]string) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(&agentorcav1alpha1.Tool{
		TypeMeta:   metav1.TypeMeta{APIVersion: agentorcav1alpha1.GroupVersion.String(), Kind: "Tool"},
		ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: "elsewhere", Labels: labels},
	})
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

// Two tenants share a namespace: each sees and changes only its own objects, and
// platform-shared ones are read-only. A foreign object is a 404, never a 403.
func TestUIResourceTenantIsolation(t *testing.T) {
	objs := []client.Object{
		isoTool("shared", "mine", tenancy.LabelTenantID, isoA),
		isoTool("shared", "theirs", tenancy.LabelTenantID, isoB),
		isoTool("shared", "platform", tenancy.LabelScope, tenancy.ScopePlatform),
		isoTool("shared", "legacy"),
		isoTool("b-only", "mine-elsewhere", tenancy.LabelTenantID, isoA), // right tenant, namespace not allowed
	}
	c := fake.NewClientBuilder().WithScheme(listCRDTestScheme(t)).WithObjects(objs...).Build()
	s := &UIServer{crdClient: c}

	do := func(method, path string, body *bytes.Reader) *httptest.ResponseRecorder {
		var req *http.Request
		if body != nil {
			req = httptest.NewRequest(method, path, body)
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		rec := httptest.NewRecorder()
		s.handleResource(rec, asTenant(req, tenantA()))
		return rec
	}

	tests := []struct {
		name   string
		mode   tenancy.Mode
		method string
		path   string
		want   int
	}{
		{"get own", tenancy.ModeEnforce, http.MethodGet, "/api/resources/tools/shared/mine", http.StatusOK},
		{"get other tenant's in the shared namespace", tenancy.ModeEnforce, http.MethodGet, "/api/resources/tools/shared/theirs", http.StatusNotFound},
		{"get own tenant's object in a namespace not allowed", tenancy.ModeEnforce, http.MethodGet, "/api/resources/tools/b-only/mine-elsewhere", http.StatusNotFound},
		{"get platform-shared", tenancy.ModeEnforce, http.MethodGet, "/api/resources/tools/shared/platform", http.StatusOK},
		{"get unlabelled, enforce", tenancy.ModeEnforce, http.MethodGet, "/api/resources/tools/shared/legacy", http.StatusNotFound},
		{"get unlabelled, audit", tenancy.ModeAudit, http.MethodGet, "/api/resources/tools/shared/legacy", http.StatusOK},
		{"get missing", tenancy.ModeEnforce, http.MethodGet, "/api/resources/tools/shared/nope", http.StatusNotFound},
		{"delete other tenant's", tenancy.ModeEnforce, http.MethodDelete, "/api/resources/tools/shared/theirs", http.StatusNotFound},
		{"delete platform-shared", tenancy.ModeEnforce, http.MethodDelete, "/api/resources/tools/shared/platform", http.StatusForbidden},
		{"delete in a namespace not allowed", tenancy.ModeEnforce, http.MethodDelete, "/api/resources/tools/b-only/mine-elsewhere", http.StatusNotFound},
		{"delete own", tenancy.ModeEnforce, http.MethodDelete, "/api/resources/tools/shared/mine", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isoMode(t, tt.mode)
			if got := do(tt.method, tt.path, nil).Code; got != tt.want {
				t.Errorf("%s %s = %d, want %d", tt.method, tt.path, got, tt.want)
			}
		})
	}
	// The other tenant's object survived.
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "shared", Name: "theirs"}, &agentorcav1alpha1.Tool{}); err != nil {
		t.Errorf("other tenant's tool is gone: %v", err)
	}

	t.Run("update of another tenant's object is a 404", func(t *testing.T) {
		isoMode(t, tenancy.ModeEnforce)
		rec := do(http.MethodPut, "/api/resources/tools/shared/theirs", toolBody(t, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("code = %d", rec.Code)
		}
	})
	t.Run("update keeps the stored owner whatever the body says", func(t *testing.T) {
		isoMode(t, tenancy.ModeEnforce)
		// toolBody has a foreign tenant ID and a platform scope: both are dropped.
		rec := do(http.MethodPut, "/api/resources/tools/shared/platform", toolBody(t, map[string]string{tenancy.LabelTenantID: isoB}))
		if rec.Code != http.StatusForbidden {
			t.Errorf("update of platform-shared = %d, want 403", rec.Code)
		}
		rec = do(http.MethodPut, "/api/resources/tools/shared/legacy", toolBody(t, map[string]string{tenancy.LabelTenantID: isoB, tenancy.LabelScope: tenancy.ScopePlatform}))
		if rec.Code != http.StatusNotFound {
			// unlabelled in enforce mode is hidden, so it cannot be updated either
			t.Errorf("update of unlabelled in enforce mode = %d, want 404", rec.Code)
		}
	})
}

func TestUIResourceCreateStampsCallerTenant(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(listCRDTestScheme(t)).Build()
	s := &UIServer{crdClient: c}
	isoMode(t, tenancy.ModeEnforce)

	// The body claims another tenant and platform scope, and another namespace.
	req := httptest.NewRequest(http.MethodPost, "/api/resources/tools",
		toolBody(t, map[string]string{tenancy.LabelTenantID: isoB, tenancy.LabelScope: tenancy.ScopePlatform}))
	rec := httptest.NewRecorder()
	s.handleResourceCollection(rec, asTenant(req, tenantA()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body)
	}
	var got agentorcav1alpha1.Tool
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "shared", Name: "new"}, &got); err != nil {
		t.Fatalf("tool not created in the tenant's namespace: %v", err)
	}
	if got.Labels[tenancy.LabelTenantID] != isoA || got.Labels[tenancy.LabelScope] != "" {
		t.Errorf("labels = %v, want tenant-id %s and no scope", got.Labels, isoA)
	}

	t.Run("a tenant with no ID cannot create", func(t *testing.T) {
		noID := tenantA()
		noID.TenantID = ""
		rec := httptest.NewRecorder()
		s.handleResourceCollection(rec, asTenant(httptest.NewRequest(http.MethodPost, "/api/resources/tools", toolBody(t, nil)), noID))
		if rec.Code != http.StatusForbidden {
			t.Errorf("create = %d, want 403", rec.Code)
		}
	})
}

func TestUIResourceListFiltersByTenant(t *testing.T) {
	objs := []client.Object{
		isoTool("shared", "mine", tenancy.LabelTenantID, isoA),
		isoTool("shared", "theirs", tenancy.LabelTenantID, isoB),
		isoTool("shared", "platform", tenancy.LabelScope, tenancy.ScopePlatform),
		isoTool("shared", "legacy"),
		isoTool("b-only", "foreign-ns", tenancy.LabelTenantID, isoA),
	}
	c := fake.NewClientBuilder().WithScheme(listCRDTestScheme(t)).WithObjects(objs...).Build()
	s := &UIServer{crdClient: c}
	for _, tt := range []struct {
		mode tenancy.Mode
		want []string
	}{
		{tenancy.ModeEnforce, []string{"mine", "platform"}},
		{tenancy.ModeAudit, []string{"legacy", "mine", "platform"}},
	} {
		isoMode(t, tt.mode)
		rec := httptest.NewRecorder()
		s.handleResourceCollection(rec, asTenant(httptest.NewRequest(http.MethodGet, "/api/resources/tools", nil), tenantA()))
		var list agentorcav1alpha1.ToolList
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(list.Items))
		for _, it := range list.Items {
			names = append(names, it.Name)
		}
		slices.Sort(names)
		if !slices.Equal(names, tt.want) {
			t.Errorf("%s mode: list = %v, want %v", tt.mode, names, tt.want)
		}
	}
}

func TestRunLookupIgnoresForeignTenant(t *testing.T) {
	isoMode(t, tenancy.ModeEnforce)
	run := func(name, tid string) *agentorcav1alpha1.AgentRun {
		return &agentorcav1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shared", Labels: map[string]string{
			tenancy.LabelTenantID: tid, tenancy.LabelTenantName: "acme", // the name label is no defence
		}}}
	}
	c := fake.NewClientBuilder().WithScheme(listCRDTestScheme(t)).WithObjects(run("mine", isoA), run("theirs", isoB)).Build()
	if _, err := findRunInNamespaces(context.Background(), c, tenantA(), "mine"); err != nil {
		t.Errorf("own run: %v", err)
	}
	if _, err := findRunInNamespaces(context.Background(), c, tenantA(), "theirs"); err == nil {
		t.Error("another tenant's run was found by name")
	}
	s := &UIServer{crdClient: c}
	rec := httptest.NewRecorder()
	s.handleRunOrStream(rec, asTenant(httptest.NewRequest(http.MethodGet, "/api/runs/shared/theirs", nil), tenantA()))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET another tenant's run = %d, want 404", rec.Code)
	}
}

func TestHandleMe(t *testing.T) {
	s := &UIServer{}
	get := func(ident *TenantIdentity) meResponse {
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		if ident != nil {
			req = asTenant(req, ident)
		}
		rec := httptest.NewRecorder()
		s.handleMe(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d", rec.Code)
		}
		var out meResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	id := tenantA()
	id.UserID, id.Groups = "grace", []string{"devs"}
	me := get(id)
	if me.UserID != "grace" || me.Tenant == nil || me.Tenant.ID != isoA || me.Tenant.Name != "acme" ||
		!slices.Equal(me.Groups, []string{"devs"}) || len(me.Workspaces) != 2 || !me.Workspaces[0].Primary ||
		me.Workspaces[0].Namespace != "shared" || !slices.Equal(me.Roles, testAdminRoles) { //nolint:goconst
		t.Errorf("me = %+v", me)
	}
	anon := get(nil)
	if anon.Tenant != nil || len(anon.Workspaces) != 0 || anon.Groups == nil {
		t.Errorf("no identity: %+v", anon)
	}
	rec := httptest.NewRecorder()
	s.handleMe(rec, httptest.NewRequest(http.MethodPost, "/api/me", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", rec.Code)
	}
}

// ── auth gaps ───────────────────────────────────────────────────────────────

func saAuth(t *testing.T, saUser string, tcs ...*agentorcav1alpha1.TenantConfig) *ExternalAuth {
	t.Helper()
	k := k8sfake.NewSimpleClientset() //nolint:staticcheck // NewClientset needs apply configs
	k.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: true, User: authv1.UserInfo{Username: saUser}}}, nil
	})
	a := &ExternalAuth{k8s: k, tenantByName: map[string]*agentorcav1alpha1.TenantConfig{}}
	for _, tc := range tcs {
		a.tenantByName[tc.Name] = tc
	}
	return a
}

func idTenant(name, id string, namespaces ...string) *agentorcav1alpha1.TenantConfig {
	return &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agent-orca-system"},
		Spec:       agentorcav1alpha1.TenantConfigSpec{TenantID: id, AllowedNamespaces: namespaces, AllowedAgents: []string{"only-this"}},
		Status:     agentorcav1alpha1.TenantConfigStatus{TenantID: id},
	}
}

func TestServiceAccountResolvesToTenant(t *testing.T) {
	a1 := idTenant("acme", isoA, "a-ns", "shared")
	b1 := idTenant("globex", isoB, "b-ns", "shared")
	tests := []struct {
		name    string
		sa      string
		wantTID string
		wantErr bool
	}{
		{"namespace claimed by one tenant", "system:serviceaccount:a-ns:ci", isoA, false},
		{"shared namespace, SA named like a tenant", "system:serviceaccount:shared:globex", isoB, false},
		{"shared namespace, SA named like none", "system:serviceaccount:shared:ci", "", true},
		{"namespace nobody claims", "system:serviceaccount:orphan:acme", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := saAuth(t, tt.sa, a1, b1)
			a.SetTenantRegistry(nil)
			id, err := a.validateK8sToken(context.Background(), "tok")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if id.TenantID != tt.wantTID {
				t.Errorf("TenantID = %q, want %q", id.TenantID, tt.wantTID)
			}
			// Limited to the SA's own namespace, not the tenant's whole set.
			if len(id.Namespaces) != 1 || id.Namespaces[0] != id.Namespace {
				t.Errorf("Namespaces = %v, Namespace = %q", id.Namespaces, id.Namespace)
			}
			if !slices.Equal(id.AllowedAgents, []string{"only-this"}) {
				t.Errorf("AllowedAgents = %v", id.AllowedAgents)
			}
		})
	}
}

func TestServiceAccountOfUnregisteredTenantIsRejected(t *testing.T) {
	a := saAuth(t, "system:serviceaccount:a-ns:ci", idTenant("acme", "", "a-ns"))
	if _, err := a.validateK8sToken(context.Background(), "tok"); err == nil {
		t.Error("a service account of a tenant with no ID was accepted")
	}
}

// A token carries namespaces and agents as they were when it was minted. The
// live TenantConfig of its tid wins, so tightening or widening a tenant takes
// effect on the next request.
func TestIssuedTokenUsesLiveTenantConfig(t *testing.T) {
	tc := tenantWithID("acme", tidNew)
	tc.Spec.AllowedNamespaces = []string{defaultNamespace}
	a := testAuth(t, testRSAKey(t), tc)
	a.SetTenantRegistry(registryWith(rec(tidNew, "acme", tenancy.StateActive)))

	tok, err := a.MintSessionForIdentity(&TenantIdentity{Roles: testAdminRoles,
		TenantID: tidNew, TenantName: "acme", Namespace: "old-ns", Namespaces: []string{"old-ns", defaultNamespace},
		AllowedAgents: []string{"stale-agent"}, UserID: "grace",
	})
	if err != nil {
		t.Fatal(err)
	}
	ident, err := a.validateIssuedToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ident.Namespaces, []string{defaultNamespace}) || ident.Namespace != defaultNamespace {
		t.Errorf("namespaces = %v, primary %q; want the live set [default]", ident.Namespaces, ident.Namespace)
	}
	if len(ident.AllowedAgents) != 0 {
		t.Errorf("allowed agents = %v, want the live (empty) list", ident.AllowedAgents)
	}

	// Widening is live too.
	tc.Spec.AllowedNamespaces = []string{defaultNamespace, "extra"}
	tc.Spec.AllowedAgents = []string{"fresh-agent"}
	ident, err = a.validateIssuedToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ident.Namespaces, []string{defaultNamespace, "extra"}) || !slices.Equal(ident.AllowedAgents, []string{"fresh-agent"}) {
		t.Errorf("identity = %+v", ident)
	}
}

func TestLegacyIssuedTokenKeepsItsClaims(t *testing.T) {
	tc := tenantWithID("acme", "")
	a := testAuth(t, testRSAKey(t), tc)
	tok, err := a.MintSessionForIdentity(&TenantIdentity{Roles: testAdminRoles,
		TenantName: "acme", Namespace: "claimed", Namespaces: []string{"claimed"}, AllowedAgents: []string{"x"}, UserID: "grace",
	})
	if err != nil {
		t.Fatal(err)
	}
	ident, err := a.validateIssuedToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ident.Namespaces, []string{"claimed"}) || !slices.Equal(ident.AllowedAgents, []string{"x"}) {
		t.Errorf("identity = %+v", ident)
	}
}
