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
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
	"github.com/floppyfish14/agent-orca/internal/security/oidc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var errProvider = errors.New("provider unavailable")

// fakeProvider implements oidc.PrincipalProvider for tests (no network/K8s).
type fakeProvider struct {
	principal *oidc.IDTokenPrincipal
	err       error
}

func (f *fakeProvider) AuthCodeURL(_, _ string) (string, string) {
	return "https://idp.example.com/authorize", "test-verifier"
}
func (f *fakeProvider) ExchangeAndVerify(_ context.Context, _, _, _ string) (*oidc.IDTokenPrincipal, error) {
	return f.principal, f.err
}

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return k
}

// testAuth builds an ExternalAuth with a real signing key and one login-capable
// federated tenant cached; enough for MintSessionForIdentity/verifySession and the
// picker/callback tests without K8s or network.
func testAuth(t *testing.T, key *rsa.PrivateKey, tc *agentorcav1alpha1.TenantConfig) *ExternalAuth {
	t.Helper()
	return &ExternalAuth{
		signingKey:   key,
		tenantCache:  map[string]*agentorcav1alpha1.TenantConfig{"federated:" + tc.Spec.Federated.IssuerURL + ":" + tc.Spec.Federated.MatchValue: tc},
		tenantByName: map[string]*agentorcav1alpha1.TenantConfig{tc.Name: tc},
	}
}

// loginTenant builds a login-capable federated tenant (has a clientSecretRef so
// the picker lists it). matchClaim/matchValue may be empty (issuer-only trust).
func loginTenant(name, matchClaim, matchValue string) *agentorcav1alpha1.TenantConfig {
	return &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agent-orca-system"},
		Spec: agentorcav1alpha1.TenantConfigSpec{
			AuthMode:        "federated",
			TargetNamespace: "default",
			Federated: &agentorcav1alpha1.FederatedAuthConfig{
				IssuerURL:   "https://idp.example.com",
				ClientID:    "agent-orca-dev",
				MatchClaim:  matchClaim,
				MatchValue:  matchValue,
				RedirectURI: "http://localhost:8083/oauth/callback",
				ClientSecretRef: agentorcav1alpha1.SecretKeyRef{
					Name: "oidc-client-secret", Key: "client-secret",
				},
			},
		},
	}
}

// factoryReturning returns a providerFactory whose provider yields the given
// principal (or error). This is the test seam that avoids K8s secret reads.
func factoryReturning(principal *oidc.IDTokenPrincipal, err error) providerFactory {
	// Always returns a (fake) provider; `err` drives ExchangeAndVerify failure so
	// the callback returns 401 (an unrecoverable provider error would be tested via
	// NewProviderForTenant in an integration test against a real IdP).
	return func(_ *agentorcav1alpha1.TenantConfig) (oidc.PrincipalProvider, error) {
		return &fakeProvider{principal: principal, err: err}, nil
	}
}

func newHandler(auth *ExternalAuth, p *oidc.IDTokenPrincipal) *OIDCLoginHandler {
	return NewOIDCLoginHandler(auth,
		WithOIDCCookieSecure(false),
		WithOIDCRedirectURI("/"),
		WithOIDCProviderFactory(factoryReturning(p, nil)))
}

// loginRedirect records the state cookie from a /oauth/login?tenant= flow.
func loginRedirect(t *testing.T, h *OIDCLoginHandler, tenant string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	path := "/oauth/login"
	if tenant != "" {
		path += "?tenant=" + tenant
	}
	h.handleLogin(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302", rec.Code)
	}
	return getCookie(rec, oidcStateCookie)
}

func TestOIDCLogin_PickerListsLoginCapableTenants(t *testing.T) {
	key := testRSAKey(t)
	login := loginTenant("github-oidc", "repository_owner", "floppyfish14")
	// A bearer-only tenant (no clientSecretRef AND no redirectURI) must NOT appear.
	bearer := loginTenant("bearer-only", "", "")
	bearer.Spec.Federated.ClientSecretRef = agentorcav1alpha1.SecretKeyRef{}
	bearer.Spec.Federated.RedirectURI = ""
	auth := &ExternalAuth{
		signingKey:   key,
		tenantByName: map[string]*agentorcav1alpha1.TenantConfig{login.Name: login, bearer.Name: bearer},
		tenantCache:  map[string]*agentorcav1alpha1.TenantConfig{},
	}
	h := NewOIDCLoginHandler(auth,
		WithOIDCProviderFactory(factoryReturning(nil, nil)), WithOIDCCookieSecure(false))

	rec := httptest.NewRecorder()
	h.handleLogin(rec, httptest.NewRequest("GET", "/oauth/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("picker status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "github-oidc") {
		t.Fatalf("picker missing login-capable tenant: %s", body)
	}
	if strings.Contains(body, "bearer-only") {
		t.Fatalf("picker should not list bearer-only tenant: %s", body)
	}
}

func TestOIDCLogin_LoginRedirectsToIdP(t *testing.T) {
	key := testRSAKey(t)
	auth := testAuth(t, key, loginTenant("github-oidc", "", ""))
	h := newHandler(auth, &oidc.IDTokenPrincipal{Issuer: "https://idp.example.com"})

	state := loginRedirect(t, h, "github-oidc")
	if state == "" {
		t.Fatal("expected state cookie after login redirect")
	}
	st, ok := h.sessions.Take(state)
	if !ok || st.tenant != "github-oidc" || st.nonce == "" || st.codeVerifier == "" {
		t.Fatalf("stored login state not as expected: %+v ok=%v", st, ok)
	}
}

func TestOIDCLogin_LoginUnknownTenant(t *testing.T) {
	key := testRSAKey(t)
	auth := testAuth(t, key, loginTenant("github-oidc", "", ""))
	h := newHandler(auth, nil)
	rec := httptest.NewRecorder()
	h.handleLogin(rec, httptest.NewRequest("GET", "/oauth/login?tenant=nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown tenant", rec.Code)
	}
}

func TestOIDCLogin_CallbackMintsVerifiableSession(t *testing.T) {
	key := testRSAKey(t)
	tc := loginTenant("github-oidc", "repository_owner", "floppyfish14")
	auth := testAuth(t, key, tc)
	principal := &oidc.IDTokenPrincipal{
		Issuer: "https://idp.example.com",
		UserID: "sub-123",
		Groups: []string{"org:engineering"},
		Claims: map[string]any{
			"repository_owner": "floppyfish14",
			"email":            "matt@example.com",
			"email_verified":   true,
		},
	}
	h := newHandler(auth, principal)

	state := loginRedirect(t, h, "github-oidc")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/oauth/callback?code=x&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: state})
	h.handleCallback(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302", rec.Code)
	}
	session := getCookie(rec, oidcSessionCookie)
	if session == "" {
		t.Fatal("expected session cookie after callback")
	}
	req2 := httptest.NewRequest("GET", "/api/runs", nil)
	req2.AddCookie(&http.Cookie{Name: oidcSessionCookie, Value: session})
	ident, ok := h.verifySession(req2)
	if !ok {
		t.Fatal("verifySession rejected a valid session")
	}
	if ident.UserID != "sub-123" || ident.Namespace != "default" || ident.TenantName != "github-oidc" {
		t.Fatalf("unexpected identity: %+v", ident)
	}
	if !equalStrings(ident.Groups, []string{"org:engineering"}) {
		t.Fatalf("Groups = %v", ident.Groups)
	}
}

func TestOIDCLogin_Callback_RejectsUnverifiedEmail(t *testing.T) {
	key := testRSAKey(t)
	auth := testAuth(t, key, loginTenant("github-oidc", "", ""))
	principal := &oidc.IDTokenPrincipal{
		Issuer: "https://idp.example.com",
		Claims: map[string]any{"email_verified": false, "repository_owner": "floppyfish14"},
	}
	h := newHandler(auth, principal)
	state := loginRedirect(t, h, "github-oidc")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/oauth/callback?code=x&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: state})
	h.handleCallback(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for unverified email", rec.Code)
	}
	if getCookie(rec, oidcSessionCookie) != "" {
		t.Fatal("session cookie must not be set when email is not verified")
	}
}

func TestOIDCLogin_Callback_GoogleStyle(t *testing.T) {
	key := testRSAKey(t)
	// Issuer-only trust (empty matchClaim); email mapped to UserID.
	auth := testAuth(t, key, loginTenant("google-oidc", "", ""))
	auth.tenantByName["google-oidc"].Spec.Federated.IssuerURL = "https://accounts.google.com"
	auth.tenantCache = map[string]*agentorcav1alpha1.TenantConfig{
		"federated:https://accounts.google.com:": auth.tenantByName["google-oidc"],
	}
	principal := &oidc.IDTokenPrincipal{
		Issuer: "https://accounts.google.com",
		UserID: "matt@example.com",
		Email:  "matt@example.com",
		Groups: []string{"org:engineering"},
		Claims: map[string]any{
			"iss": "https://accounts.google.com", "sub": "google-sub-123",
			"email": "matt@example.com", "email_verified": true,
		},
	}
	h := newHandler(auth, principal)
	state := loginRedirect(t, h, "google-oidc")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/oauth/callback?code=x&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: state})
	h.handleCallback(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302", rec.Code)
	}
	session := getCookie(rec, oidcSessionCookie)
	if session == "" {
		t.Fatal("expected session cookie")
	}
	req2 := httptest.NewRequest("GET", "/api/runs", nil)
	req2.AddCookie(&http.Cookie{Name: oidcSessionCookie, Value: session})
	ident, ok := h.verifySession(req2)
	if !ok {
		t.Fatal("verifySession rejected a valid Google session")
	}
	if ident.UserID != "matt@example.com" || ident.TenantName != "google-oidc" {
		t.Fatalf("unexpected identity: %+v", ident)
	}
	if !equalStrings(ident.Groups, []string{"org:engineering"}) {
		t.Fatalf("Groups = %v", ident.Groups)
	}
}

func TestOIDCLogin_Callback_ExchangeFailed(t *testing.T) {
	key := testRSAKey(t)
	auth := testAuth(t, key, loginTenant("github-oidc", "", ""))
	h := NewOIDCLoginHandler(auth,
		WithOIDCCookieSecure(false), WithOIDCRedirectURI("/"),
		WithOIDCProviderFactory(factoryReturning(nil, errors.New("token exchange failed"))))
	state := loginRedirect(t, h, "github-oidc")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/oauth/callback?code=x&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: state})
	h.handleCallback(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 on exchange failure", rec.Code)
	}
}

func TestOIDCLogin_Callback_WrongState(t *testing.T) {
	key := testRSAKey(t)
	auth := testAuth(t, key, loginTenant("github-oidc", "", ""))
	h := newHandler(auth, &oidc.IDTokenPrincipal{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/oauth/callback?code=x&state=not-the-state", nil)
	req.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: "the-real-state"})
	h.handleCallback(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for wrong state", rec.Code)
	}
}

func TestUIRunAuth_OIDCEnabledIgnoresBearerToken(t *testing.T) {
	key := testRSAKey(t)
	auth := testAuth(t, key, loginTenant("github-oidc", "", ""))
	h := newHandler(auth, &oidc.IDTokenPrincipal{Issuer: "https://idp.example.com"})
	ui := NewUIServer(nil, nil, false, nil, true, auth, nil, nil)
	ui.SetOIDCLogin(h)
	called := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { called = true })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/runs", nil)
	req.Header.Set("Authorization", "Bearer some-bearer-token")
	ui.requireAuth(next).ServeHTTP(rec, req)
	if called {
		t.Fatal("next handler must NOT be called without a session cookie (OIDC login required)")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no UI without login)", rec.Code)
	}
}

func TestUIRunAuth_AcceptsSessionCookie(t *testing.T) {
	key := testRSAKey(t)
	auth := testAuth(t, key, loginTenant("github-oidc", "", ""))
	h := newHandler(auth, &oidc.IDTokenPrincipal{Issuer: "https://idp.example.com"})
	ui := NewUIServer(nil, nil, false, nil, true, auth, nil, nil)
	ui.SetOIDCLogin(h)
	ident := &TenantIdentity{TenantName: "github-oidc", Namespace: "default", UserID: "sub-1", Groups: []string{"g"}}
	signed, err := auth.MintSessionForIdentity(ident)
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	var got *TenantIdentity
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if ti, ok := TenantFromContext(r.Context()); ok {
			got = ti
		}
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/runs", nil)
	req.AddCookie(&http.Cookie{Name: oidcSessionCookie, Value: signed})
	ui.requireAuth(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got == nil || got.UserID != "sub-1" {
		t.Fatalf("identity not injected into context: %+v", got)
	}
}

func getCookie(rec *httptest.ResponseRecorder, name string) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
