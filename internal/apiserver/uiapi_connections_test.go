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
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/connections"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	connTestUser    = "alice"
	connOtherUser   = "bob"
	connStateValue  = "state-value-123"
	connSecretValue = "client-secret-SENTINEL"
	connTokenValue  = "refresh-token-SENTINEL"
)

type fakeConnBackend struct {
	mu          sync.Mutex
	started     []connections.StartRequest
	completed   []connections.CompleteRequest
	disconnects []string
	completeErr error
	startErr    error
}

func (f *fakeConnBackend) Start(_ context.Context, req connections.StartRequest) (connections.Started, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, req)
	if f.startErr != nil {
		return connections.Started{}, f.startErr
	}
	return connections.Started{AuthorizeURL: "https://as.example/authorize?state=" + connStateValue, State: connections.Secret(connStateValue)}, nil
}

func (f *fakeConnBackend) Complete(ctx context.Context, req connections.CompleteRequest) (postgresql.MCPConnection, error) {
	f.mu.Lock()
	f.completed = append(f.completed, req)
	err := f.completeErr
	f.mu.Unlock()
	if err != nil {
		return postgresql.MCPConnection{}, err
	}
	// Exercise the resolver the way the service does.
	ref, rerr := req.Resolve(ctx, "shared", "github")
	if rerr != nil {
		return postgresql.MCPConnection{}, rerr
	}
	return postgresql.MCPConnection{TenantID: req.TenantID, UserID: req.UserID, ServerNS: ref.Namespace, ServerName: ref.Name,
		Scopes: []string{"repo:read"}, AccountName: req.AccountLabel, Status: postgresql.ConnectionActive,
		RefreshTokenEnc: []byte(connTokenValue)}, nil
}

func (f *fakeConnBackend) Disconnect(_ context.Context, tenantID, userID string, ref connections.ServerRef) (connections.DisconnectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnects = append(f.disconnects, tenantID+"/"+userID+"/"+ref.Namespace+"/"+ref.Name)
	return connections.DisconnectResult{Connection: postgresql.MCPConnection{AccountName: userID}, Revoked: true}, nil
}

func (f *fakeConnBackend) SealClientSecret(_, _, _, secret string) (string, []byte, error) {
	return "k1", []byte("sealed:" + strings.ToUpper(secret[:1])), nil
}

type fakeConnStore struct {
	conns []postgresql.MCPConnection
	apps  map[string]postgresql.MCPOAuthApp
}

func (f *fakeConnStore) scope(ctx context.Context) string {
	s, _ := postgresql.ScopeFrom(ctx)
	return s
}

func (f *fakeConnStore) ListConnections(ctx context.Context, user string) ([]postgresql.MCPConnection, error) {
	var out []postgresql.MCPConnection
	for _, c := range f.conns {
		if c.UserID == user && c.TenantID == f.scope(ctx) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeConnStore) ListServerConnections(ctx context.Context, ns, name string) ([]postgresql.ConnectionSummary, error) {
	out := []postgresql.ConnectionSummary{}
	for _, c := range f.conns {
		if c.ServerNS == ns && c.ServerName == name && c.TenantID == f.scope(ctx) {
			out = append(out, postgresql.ConnectionSummary{UserID: c.UserID, Status: c.Status, Scopes: c.Scopes})
		}
	}
	return out, nil
}

func (f *fakeConnStore) GetConnection(ctx context.Context, user, ns, name string) (postgresql.MCPConnection, error) {
	for _, c := range f.conns {
		if c.UserID == user && c.ServerNS == ns && c.ServerName == name && c.TenantID == f.scope(ctx) {
			return c, nil
		}
	}
	return postgresql.MCPConnection{}, postgresql.ErrConnectionNotFound
}

func (f *fakeConnStore) UpsertOAuthApp(ctx context.Context, a postgresql.MCPOAuthApp) (postgresql.MCPOAuthApp, bool, error) {
	key := f.scope(ctx) + "/" + a.ServerNS + "/" + a.AppRef
	old, existed := f.apps[key]
	a.TenantID = f.scope(ctx)
	if len(a.ClientSecretEnc) == 0 {
		a.ClientSecretEnc, a.KeyID = old.ClientSecretEnc, old.KeyID
	}
	a.HasSecret = len(a.ClientSecretEnc) > 0
	a.UpdatedAt = time.Now()
	f.apps[key] = a
	return a, !existed, nil
}

func (f *fakeConnStore) GetOAuthApp(ctx context.Context, ns, ref string) (postgresql.MCPOAuthApp, error) {
	a, ok := f.apps[f.scope(ctx)+"/"+ns+"/"+ref]
	if !ok {
		return postgresql.MCPOAuthApp{}, postgresql.ErrConnectionNotFound
	}
	return a, nil
}

type connFixture struct {
	t       *testing.T
	h       http.Handler
	backend *fakeConnBackend
	store   *fakeConnStore
	audit   *fakeAudit
	srv     *UIServer
}

func connMCPServer(ns, name, tenant, mode string, labels map[string]string) *agentorcav1alpha1.MCPServer {
	l := map[string]string{}
	if tenant != "" {
		l[tenancy.LabelTenantID] = tenant
	}
	maps.Copy(l, labels)
	return &agentorcav1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: l, UID: types.UID("uid-" + name)},
		Spec: agentorcav1alpha1.MCPServerSpec{Transport: "http", URL: "https://mcp." + name + ".example/mcp",
			Auth: &agentorcav1alpha1.MCPAuthConfig{OAuth: &agentorcav1alpha1.MCPOAuthConfig{ConnectionMode: mode, AppRef: name + "-app", UserScopes: []string{"repo:read"}}}},
	}
}

func newConnFixture(t *testing.T) *connFixture {
	t.Helper()
	isoMode(t, tenancy.ModeEnforce)
	prev := tenancy.PlatformNamespaces()
	tenancy.SetPlatformNamespaces([]string{"platform-ns"})
	t.Cleanup(func() { tenancy.SetPlatformNamespaces(prev) })
	legacy := connMCPServer("shared", "legacy", isoA, "", nil)
	legacy.Spec.Auth.OAuth = &agentorcav1alpha1.MCPOAuthConfig{Credentials: &agentorcav1alpha1.LocalObjectRef{Name: "c"}}
	tool := &agentorcav1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "shared", Labels: map[string]string{tenancy.LabelTenantID: isoA},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "agentorca.agentorca.io/v1alpha1", Kind: "MCPServer", Name: "github", UID: "uid-github"}}}}
	cl := fake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(
		connMCPServer("shared", "github", isoA, "maintainer", nil),
		legacy,
		connMCPServer("b-only", "theirs", isoB, "maintainer", nil),
		connMCPServer("platform-ns", "plat", "", "maintainer", map[string]string{tenancy.LabelScope: tenancy.ScopePlatform}),
		tool,
		&agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: "shared", Labels: map[string]string{tenancy.LabelTenantID: isoA},
			Annotations: map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI}},
			Spec: agentorcav1alpha1.AgentSpec{Tools: []string{"github"}}},
	).WithStatusSubresource(&agentorcav1alpha1.Agent{}).Build()
	f := &connFixture{t: t, backend: &fakeConnBackend{}, store: &fakeConnStore{apps: map[string]postgresql.MCPOAuthApp{}}, audit: &fakeAudit{}}
	f.srv = &UIServer{crdClient: cl, audit: f.audit}
	f.srv.SetConnections(f.backend, f.store, true)
	f.h = f.srv.Handler()
	return f
}

func connUser(role tenancy.Role, tenant, user string) *TenantIdentity {
	id := idFor(role, tenant)
	id.UserID = user
	return id
}

func (f *connFixture) do(id *TenantIdentity, method, path, body string, mut ...func(*http.Request)) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if id != nil {
		req = asTenant(req, id)
	}
	for _, m := range mut {
		m(req)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func TestConnections_RoleMatrix(t *testing.T) {
	f := newConnFixture(t)
	type call struct{ method, path, body string }
	calls := map[tenancy.Role][]call{
		tenancy.RoleViewer: {{http.MethodGet, "/api/connections", ""}},
		tenancy.RoleRunner: {{http.MethodPost, "/api/connections/shared/github/start", "{}"}, {http.MethodDelete, "/api/connections/shared/github", ""},
			{http.MethodGet, "/api/me/connections/required?agent=shared/support", ""}},
		tenancy.RoleAdmin: {{http.MethodGet, "/api/mcpservers/shared/github/connections", ""}, {http.MethodGet, "/api/mcp-oauth-apps/shared/github", ""},
			{http.MethodPut, "/api/mcp-oauth-apps/shared/github", `{"clientId":"c"}`}},
	}
	order := []tenancy.Role{tenancy.RoleViewer, tenancy.RoleRunner, tenancy.RoleBuilder, tenancy.RoleAdmin}
	rank := map[tenancy.Role]int{tenancy.RoleViewer: 0, tenancy.RoleRunner: 1, tenancy.RoleBuilder: 2, tenancy.RoleAdmin: 3}
	for need, cs := range calls {
		for _, c := range cs {
			for _, have := range order {
				rec := f.do(connUser(have, isoA, connTestUser), c.method, c.path, c.body)
				forbidden := rec.Code == http.StatusForbidden
				if rank[have] < rank[need] && !forbidden {
					t.Errorf("%s %s as %s = %d, want 403", c.method, c.path, have, rec.Code)
				}
				if rank[have] >= rank[need] && forbidden {
					t.Errorf("%s %s as %s = 403, want allowed: %s", c.method, c.path, have, rec.Body)
				}
			}
		}
	}
}

func TestConnections_NeedsUserAndFeature(t *testing.T) {
	f := newConnFixture(t)
	machine := idFor(tenancy.RoleAdmin, isoA)
	machine.UserID = ""
	if rec := f.do(machine, http.MethodPost, "/api/connections/shared/github/start", "{}"); rec.Code != http.StatusForbidden {
		t.Errorf("no user = %d", rec.Code)
	}
	noTenant := connUser(tenancy.RoleAdmin, isoA, connTestUser)
	noTenant.TenantID = ""
	if rec := f.do(noTenant, http.MethodGet, "/api/connections", ""); rec.Code != http.StatusForbidden {
		t.Errorf("no tenant id = %d", rec.Code)
	}
	f.srv.conns = nil
	if rec := f.do(connUser(tenancy.RoleAdmin, isoA, connTestUser), http.MethodGet, "/api/connections", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("feature off = %d", rec.Code)
	}
}

func TestConnections_StartSetsCookieAndUsesTheSession(t *testing.T) {
	f := newConnFixture(t)
	rec := f.do(connUser(tenancy.RoleRunner, isoA, connTestUser), http.MethodPost, "/api/connections/shared/github/start", `{"scopes":["repo:read"]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "authorizeUrl") {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	if len(f.backend.started) != 1 {
		t.Fatalf("started = %+v", f.backend.started)
	}
	got := f.backend.started[0]
	if got.TenantID != isoA || got.UserID != connTestUser || got.Server.UID != "uid-github" || got.Server.AppRef != "github-app" ||
		got.Server.URL != "https://mcp.github.example/mcp" || len(got.Scopes) != 1 {
		t.Errorf("start request = %+v", got)
	}
	c := rec.Result().Cookies()
	if len(c) != 1 || c[0].Name != connCookieSecure || c[0].Value != connStateValue || !c[0].HttpOnly || !c[0].Secure ||
		c[0].SameSite != http.SameSiteLaxMode || c[0].Path != "/" {
		t.Errorf("cookie = %+v", c)
	}
	// Servers that are not the caller's, or not per-user, are not startable.
	for path, want := range map[string]int{
		"/api/connections/b-only/theirs/start":    http.StatusNotFound,
		"/api/connections/shared/missing/start":   http.StatusNotFound,
		"/api/connections/shared/legacy/start":    http.StatusConflict,
		"/api/connections/platform-ns/plat/start": http.StatusOK,
		"/api/connections/not-allowed-ns/x/start": http.StatusNotFound,
	} {
		if rec := f.do(connUser(tenancy.RoleRunner, isoA, connTestUser), http.MethodPost, path, "{}"); rec.Code != want {
			t.Errorf("POST %s = %d, want %d", path, rec.Code, want)
		}
	}
	f.backend.startErr = connections.ErrNotConfigured
	if rec := f.do(connUser(tenancy.RoleRunner, isoA, connTestUser), http.MethodPost, "/api/connections/shared/github/start", "{}"); rec.Code != http.StatusConflict {
		t.Errorf("no app = %d", rec.Code)
	}
}

func callbackURL(q url.Values) string { return "/api/connections/callback?" + q.Encode() }

func TestConnections_CallbackPassesSessionNotRequest(t *testing.T) {
	f := newConnFixture(t)
	q := url.Values{"state": {connStateValue}, "code": {"auth-code-SENTINEL"}, "iss": {"https://as.example"}, "user": {"mallory"}, "tenant": {isoB}}
	rec := f.do(connUser(tenancy.RoleViewer, isoA, connTestUser), http.MethodGet, callbackURL(q), "", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: connCookieSecure, Value: connStateValue})
	})
	if rec.Code != http.StatusFound {
		t.Fatalf("callback = %d %s", rec.Code, rec.Body)
	}
	if len(f.backend.completed) != 1 {
		t.Fatal("not completed")
	}
	c := f.backend.completed[0]
	if c.TenantID != isoA || c.UserID != connTestUser || c.State != connStateValue || c.CookieState != connStateValue || c.Iss != "https://as.example" || c.Code != "auth-code-SENTINEL" {
		t.Errorf("complete request = %+v", c)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Path != "/connections" || loc.Query().Get("status") != "connected" || loc.Query().Get("server") != "shared/github" {
		t.Errorf("location = %s", rec.Header().Get("Location"))
	}
	for _, leak := range []string{"auth-code-SENTINEL", connStateValue, connTokenValue} {
		if strings.Contains(rec.Header().Get("Location"), leak) || strings.Contains(rec.Body.String(), leak) {
			t.Errorf("callback response leaks %q", leak)
		}
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", rec.Header())
	}
	// The cookie is cleared (single use).
	cleared := false
	for _, ck := range rec.Result().Cookies() {
		cleared = cleared || (ck.Name == connCookieSecure && ck.MaxAge < 0)
	}
	if !cleared {
		t.Error("state cookie not cleared")
	}
	if len(f.audit.events) != 1 || f.audit.events[0].Action != postgresql.AuditConnect || f.audit.events[0].Kind != kindMCPConnection ||
		f.audit.events[0].UserID != connTestUser || strings.Contains(f.audit.events[0].Summary, connTokenValue) {
		t.Errorf("audit = %+v", f.audit.events)
	}
}

func TestConnections_CallbackFailuresRedirectWithACode(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"denied":   {connections.ErrProviderDenied, "denied"},
		"expired":  {connections.ErrBadState, "expired"},
		"scopes":   {connections.ErrScopesExceeded, "scopes"},
		"mismatch": {connections.ErrStateMismatch, "failed"},
		"iss":      {connections.ErrIssuerMismatch, "failed"},
		"other":    {errors.New("boom: token=refresh-token-SENTINEL"), "failed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newConnFixture(t)
			f.backend.completeErr = tc.err
			rec := f.do(connUser(tenancy.RoleViewer, isoA, connTestUser), http.MethodGet, callbackURL(url.Values{"state": {"s"}, "code": {"c"}}), "")
			loc, _ := url.Parse(rec.Header().Get("Location"))
			if rec.Code != http.StatusFound || loc.Query().Get("status") != tc.want {
				t.Fatalf("callback = %d %s", rec.Code, rec.Header().Get("Location"))
			}
			if strings.Contains(rec.Header().Get("Location"), "SENTINEL") || len(f.audit.events) != 0 {
				t.Errorf("leak or audit on failure: %v %v", rec.Header(), f.audit.events)
			}
		})
	}
	// Missing cookie reaches the service as an empty CookieState, which it refuses.
	f := newConnFixture(t)
	f.do(connUser(tenancy.RoleViewer, isoA, connTestUser), http.MethodGet, callbackURL(url.Values{"state": {"s"}, "code": {"c"}}), "")
	if f.backend.completed[0].CookieState != "" {
		t.Error("cookie state invented")
	}
	// No session: no flow.
	if rec := f.do(nil, http.MethodGet, callbackURL(url.Values{"state": {"s"}}), ""); rec.Code != http.StatusForbidden {
		t.Errorf("no session = %d", rec.Code)
	}
}

func TestConnections_ListIsTheCallersOwn(t *testing.T) {
	f := newConnFixture(t)
	mk := func(tenant, user string) postgresql.MCPConnection {
		return postgresql.MCPConnection{TenantID: tenant, UserID: user, ServerNS: "shared", ServerName: "github", Scopes: []string{"repo:read"},
			Status: postgresql.ConnectionActive, RefreshTokenEnc: []byte(connTokenValue), AccessTokenEnc: []byte("access-SENTINEL"), KeyID: "k1"}
	}
	f.store.conns = []postgresql.MCPConnection{mk(isoA, connTestUser), mk(isoA, connOtherUser), mk(isoB, connTestUser)}
	rec := f.do(connUser(tenancy.RoleViewer, isoA, connTestUser), http.MethodGet, "/api/connections", "")
	got := evalDecode[[]connectionView](t, rec)
	if len(got) != 1 || got[0].Server != "github" {
		t.Fatalf("list = %+v", got)
	}
	if strings.Contains(rec.Body.String(), "SENTINEL") || strings.Contains(rec.Body.String(), connTestUser) || strings.Contains(rec.Body.String(), "k1") {
		t.Errorf("list leaks: %s", rec.Body)
	}
	// Another tenant's alice sees her own.
	if got := evalDecode[[]connectionView](t, f.do(connUser(tenancy.RoleViewer, isoB, connTestUser), http.MethodGet, "/api/connections", "")); len(got) != 1 {
		t.Errorf("tenant B list = %+v", got)
	}
	// The admin view lists users and statuses, no tokens, only this tenant's.
	rec = f.do(connUser(tenancy.RoleAdmin, isoA, "admin"), http.MethodGet, "/api/mcpservers/shared/github/connections", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "SENTINEL") || !strings.Contains(rec.Body.String(), connOtherUser) ||
		!strings.Contains(rec.Body.String(), `"active":2`) {
		t.Errorf("admin view = %d %s", rec.Code, rec.Body)
	}
}

func TestConnections_DisconnectAndRevoke(t *testing.T) {
	f := newConnFixture(t)
	if rec := f.do(connUser(tenancy.RoleRunner, isoA, connTestUser), http.MethodDelete, "/api/connections/shared/github", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("disconnect = %d %s", rec.Code, rec.Body)
	}
	if len(f.backend.disconnects) != 1 || f.backend.disconnects[0] != isoA+"/alice/shared/github" {
		t.Errorf("disconnects = %v", f.backend.disconnects)
	}
	if e := f.audit.events[0]; e.Action != postgresql.AuditDisconnect || e.UserID != connTestUser || e.Kind != kindMCPConnection {
		t.Errorf("audit = %+v", e)
	}
	// A runner cannot revoke someone else's; an admin can, and it is audited as revoke.
	if rec := f.do(connUser(tenancy.RoleRunner, isoA, connTestUser), http.MethodDelete, "/api/connections/shared/github?user=bob", ""); rec.Code != http.StatusForbidden {
		t.Errorf("runner revoking bob = %d", rec.Code)
	}
	if rec := f.do(connUser(tenancy.RoleAdmin, isoA, "root"), http.MethodDelete, "/api/connections/shared/github?user=bob", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("admin revoke = %d", rec.Code)
	}
	if f.backend.disconnects[1] != isoA+"/bob/shared/github" {
		t.Errorf("disconnects = %v", f.backend.disconnects)
	}
	if e := f.audit.events[1]; e.Action != postgresql.AuditRevoke || e.UserID != "root" {
		t.Errorf("revoke audit = %+v", e)
	}
	// Another tenant's namespace is not found.
	if rec := f.do(connUser(tenancy.RoleAdmin, isoA, "root"), http.MethodDelete, "/api/connections/b-only/theirs?user=bob", ""); rec.Code != http.StatusNotFound {
		t.Errorf("other tenant = %d", rec.Code)
	}
}

func TestConnections_OAuthAppSecretIsWriteOnly(t *testing.T) {
	f := newConnFixture(t)
	admin := connUser(tenancy.RoleAdmin, isoA, "root")
	rec := f.do(admin, http.MethodPut, "/api/mcp-oauth-apps/shared/github", `{"clientId":"cid-1","clientSecret":"`+connSecretValue+`","scopesAllowed":["repo:read"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("register = %d %s", rec.Code, rec.Body)
	}
	app := evalDecode[oauthAppView](t, rec)
	if !app.Registered || !app.HasSecret || app.ClientID != "cid-1" || app.AppRef != "github-app" {
		t.Errorf("app = %+v", app)
	}
	// Rotation without a secret keeps it; the secret is in no response and no audit row.
	rec2 := f.do(admin, http.MethodPut, "/api/mcp-oauth-apps/shared/github", `{"clientId":"cid-2"}`)
	rec3 := f.do(admin, http.MethodGet, "/api/mcp-oauth-apps/shared/github", "")
	if !evalDecode[oauthAppView](t, rec3).HasSecret || evalDecode[oauthAppView](t, rec3).ClientID != "cid-2" {
		t.Errorf("kept secret = %s", rec3.Body)
	}
	for _, b := range []string{rec.Body.String(), rec2.Body.String(), rec3.Body.String()} {
		if strings.Contains(b, connSecretValue) || strings.Contains(b, "sealed:") {
			t.Errorf("response leaks the secret: %s", b)
		}
	}
	if len(f.audit.events) != 2 || f.audit.events[0].Action != postgresql.AuditAppRegister || f.audit.events[1].Action != postgresql.AuditAppRotate {
		t.Fatalf("audit = %+v", f.audit.events)
	}
	for _, e := range f.audit.events {
		if strings.Contains(e.Summary, connSecretValue) {
			t.Errorf("audit summary leaks the secret: %+v", e)
		}
	}
	// Tenant B's admin cannot see or touch tenant A's server, and A's app is not B's.
	if rec := f.do(connUser(tenancy.RoleAdmin, isoB, "root"), http.MethodPut, "/api/mcp-oauth-apps/shared/github", `{"clientId":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("tenant B put = %d", rec.Code)
	}
	if rec := f.do(admin, http.MethodPut, "/api/mcp-oauth-apps/shared/github", `{"clientId":""}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty client id = %d", rec.Code)
	}
}

func TestConnections_RequiredLists(t *testing.T) {
	f := newConnFixture(t)
	f.store.conns = []postgresql.MCPConnection{{TenantID: isoA, UserID: connTestUser, ServerNS: "shared", ServerName: "github", ServerUID: "uid-github", Status: postgresql.ConnectionActive}}
	type resp struct {
		Servers  []requiredServer `json:"servers"`
		Required []requiredServer `json:"required"`
	}
	get := func(user string) resp {
		return evalDecode[resp](t, f.do(connUser(tenancy.RoleRunner, isoA, user), http.MethodGet, "/api/me/connections/required?agent=shared/support", ""))
	}
	alice, bob := get(connTestUser), get(connOtherUser)
	if len(alice.Servers) != 1 || !alice.Servers[0].Connected || alice.Servers[0].Mode != "maintainer" || len(alice.Required) != 0 {
		t.Errorf("alice = %+v", alice)
	}
	// Maintainer mode uses the maintainer's connection, so bob is not asked to connect.
	if len(bob.Servers) != 1 || bob.Servers[0].Connected || len(bob.Required) != 0 {
		t.Errorf("bob = %+v", bob)
	}
	if rec := f.do(connUser(tenancy.RoleRunner, isoA, connTestUser), http.MethodGet, "/api/me/connections/required?agent=bad", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad agent = %d", rec.Code)
	}
}

func TestDefaultMCPConnectionMode(t *testing.T) {
	mk := func(oauth map[string]any) *unstructured.Unstructured {
		o := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"transport": "http"}}}
		if oauth != nil {
			_ = unstructured.SetNestedMap(o.Object, oauth, "spec", "auth", "oauth")
		}
		return o
	}
	mode := func(o *unstructured.Unstructured) string {
		v, _, _ := unstructured.NestedString(o.Object, "spec", "auth", "oauth", "connectionMode")
		return v
	}
	for name, tc := range map[string]struct {
		in   map[string]any
		want string
	}{
		"new oauth server, no credentials": {map[string]any{"appRef": "gh"}, "maintainer"},
		"explicit mode is kept":            {map[string]any{"appRef": "gh", "connectionMode": "shared"}, "shared"},
		"credentials mean shared":          {map[string]any{"credentials": map[string]any{"name": "c"}}, ""},
		"no oauth block":                   {nil, ""},
	} {
		o := mk(tc.in)
		defaultMCPConnectionMode(o)
		if got := mode(o); got != tc.want {
			t.Errorf("%s: mode = %q, want %q", name, got, tc.want)
		}
	}
}

func TestConnections_PublishNeedsThePublishersOwnConnectionAndRecordsThem(t *testing.T) {
	f := newConnFixture(t)
	publish := func(user string) *httptest.ResponseRecorder {
		return f.do(connUser(tenancy.RoleBuilder, isoA, user), http.MethodPost, "/api/agents/shared/support/publish", "")
	}
	agent := func() agentorcav1alpha1.Agent {
		var a agentorcav1alpha1.Agent
		if err := f.srv.crdClient.Get(context.Background(), client.ObjectKey{Namespace: "shared", Name: "support"}, &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	// No connection: refused, nothing created, nobody recorded.
	rec := publish(connTestUser)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "github") {
		t.Fatalf("publish without a connection = %d %s", rec.Code, rec.Body)
	}
	var deps agentorcav1alpha1.AgentDeploymentList
	_ = f.srv.crdClient.List(context.Background(), &deps)
	if len(deps.Items) != 0 || agent().Status.PublishedBy != "" {
		t.Errorf("a refused publish left state: %d deployments, publishedBy %q", len(deps.Items), agent().Status.PublishedBy)
	}
	// Somebody else's connection does not count; a needs_reauth one does not either.
	conn := postgresql.MCPConnection{TenantID: isoA, UserID: connOtherUser, ServerNS: "shared", ServerName: "github", ServerUID: "uid-github", Status: postgresql.ConnectionActive}
	f.store.conns = []postgresql.MCPConnection{conn}
	if rec := publish(connTestUser); rec.Code != http.StatusConflict {
		t.Errorf("publish on bob's connection = %d", rec.Code)
	}
	conn.UserID, conn.Status = connTestUser, postgresql.ConnectionNeedsReauth
	f.store.conns = []postgresql.MCPConnection{conn}
	if rec := publish(connTestUser); rec.Code != http.StatusConflict {
		t.Errorf("publish on a needs_reauth connection = %d", rec.Code)
	}
	conn.Status, conn.ServerUID = postgresql.ConnectionActive, "uid-old"
	f.store.conns = []postgresql.MCPConnection{conn}
	if rec := publish(connTestUser); rec.Code != http.StatusConflict {
		t.Errorf("publish on a connection of a recreated server = %d", rec.Code)
	}
	// An active connection of the publisher: published, and the publisher recorded.
	conn.ServerUID = "uid-github"
	f.store.conns = []postgresql.MCPConnection{conn}
	if rec := publish(connTestUser); rec.Code != http.StatusCreated {
		t.Fatalf("publish with a connection = %d %s", rec.Code, rec.Body)
	}
	if a := agent(); a.Status.PublishedBy != connTestUser || a.Status.PublishedAt == nil {
		t.Errorf("status = %+v", a.Status)
	}
	// Republishing as somebody else needs their connection, and then they become the maintainer.
	if rec := publish(connOtherUser); rec.Code != http.StatusConflict {
		t.Errorf("bob without a connection = %d", rec.Code)
	}
	if agent().Status.PublishedBy != connTestUser {
		t.Error("a refused republish changed the maintainer")
	}
	f.store.conns = append(f.store.conns, postgresql.MCPConnection{TenantID: isoA, UserID: connOtherUser, ServerNS: "shared", ServerName: "github", ServerUID: "uid-github", Status: postgresql.ConnectionActive})
	if rec := publish(connOtherUser); rec.Code != http.StatusOK {
		t.Fatalf("bob with a connection = %d %s", rec.Code, rec.Body)
	}
	if agent().Status.PublishedBy != connOtherUser {
		t.Errorf("publishedBy = %q", agent().Status.PublishedBy)
	}
}

func TestConnections_PublishOfAnAgentWithoutPerUserServersIsUnchanged(t *testing.T) {
	f := newConnFixture(t)
	cl := f.srv.crdClient
	plain := &agentorcav1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "shared", Labels: map[string]string{tenancy.LabelTenantID: isoA},
		Annotations: map[string]string{tenancy.AnnotationOwner: tenancy.OwnerUI}}}
	if err := cl.Create(context.Background(), plain); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(connUser(tenancy.RoleBuilder, isoA, connTestUser), http.MethodPost, "/api/agents/shared/plain/publish", ""); rec.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body)
	}
	var got agentorcav1alpha1.Agent
	_ = cl.Get(context.Background(), client.ObjectKeyFromObject(plain), &got)
	if got.Status.PublishedBy != "" {
		t.Errorf("publishedBy = %q for an agent with no maintainer servers", got.Status.PublishedBy)
	}
}
