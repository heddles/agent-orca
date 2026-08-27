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
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"sync"
	"time"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
	"github.com/floppyfish14/agent-orca/internal/security/oidc"
)

const (
	// oidcSessionCookie is the HttpOnly session cookie carrying the agent-orca-issued
	// JWT minted after a successful OIDC login (UI/browser flow).
	oidcSessionCookie = "agentorca_session"
	// oidcStateCookie carries the CSRF state value across the IdP redirect.
	oidcStateCookie = "agentorca_oidc_state"

	oidcLoginStateTTL = 5 * time.Minute
)

// providerFactory builds an OIDC provider for a given tenant. Production relies on
// ExternalAuth.NewProviderForTenant (reads the tenant's client_secret from K8s);
// tests inject a fake so no K8s cluster or real IdP is needed.
type providerFactory func(tc *agentorcav1alpha1.TenantConfig) (oidc.PrincipalProvider, error)

// loginState holds the ephemeral tenant + nonce + PKCE verifier bound to a login
// state. The tenant is carried in the state so the callback uses the right tenant's
// IdP/secret — this is what keeps the flow multi-tenant.
type loginState struct {
	tenant       string
	nonce        string
	codeVerifier string
	expiresAt    time.Time
}

// loginStateStore is a short-lived, in-process store for OAuth2 state/nonce values.
// Dev-only by design; production should back sessions with a shared store.
type loginStateStore struct {
	mu      sync.Mutex
	entries map[string]loginState
}

func newLoginStateStore() *loginStateStore {
	return &loginStateStore{entries: make(map[string]loginState)}
}

func (s *loginStateStore) Set(state, tenant, nonce, codeVerifier string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[state] = loginState{
		tenant:       tenant,
		nonce:        nonce,
		codeVerifier: codeVerifier,
		expiresAt:    time.Now().Add(oidcLoginStateTTL),
	}
}

func (s *loginStateStore) Take(state string) (loginState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.entries[state]
	if !ok || time.Now().After(st.expiresAt) {
		delete(s.entries, state)
		return loginState{}, false
	}
	delete(s.entries, state)
	return st, true
}

// OIDCLoginHandler implements the tenant-aware OIDC authorization-code login flow.
// Each federated TenantConfig that carries a clientSecretRef/redirectURI is a
// selectable login provider; the browser picks one at /oauth/login and the callback
// uses that tenant's IdP + client_secret to exchange the code. On success an
// agent-orca-issued session JWT is set as an HttpOnly cookie; requireAuth is
// cookie-only when this handler is wired, so the UI is never shown without a
// successful login.
type OIDCLoginHandler struct {
	auth         *ExternalAuth
	cookieSecure bool
	sessions     *loginStateStore
	redirectURI  string          // where to land the browser post-login/logout (default "/")
	factory      providerFactory // test seam; nil => auth.NewProviderForTenant
}

// OIDCLoginOption configures an OIDCLoginHandler.
type OIDCLoginOption func(*OIDCLoginHandler)

// WithOIDCCookieSecure sets the Secure attribute on session cookies (true behind
// TLS; false for plain-HTTP local dev).
func WithOIDCCookieSecure(b bool) OIDCLoginOption {
	return func(h *OIDCLoginHandler) { h.cookieSecure = b }
}

// WithOIDCRedirectURI overrides the post-login/logout redirect target (default "/").
func WithOIDCRedirectURI(u string) OIDCLoginOption {
	return func(h *OIDCLoginHandler) { h.redirectURI = u }
}

// WithOIDCProviderFactory injects a provider builder (for tests).
func WithOIDCProviderFactory(f providerFactory) OIDCLoginOption {
	return func(h *OIDCLoginHandler) { h.factory = f }
}

// NewOIDCLoginHandler builds a handler. auth supplies the tenant cache, signing key,
// and K8s client (for reading per-tenant client secrets).
func NewOIDCLoginHandler(auth *ExternalAuth, opts ...OIDCLoginOption) *OIDCLoginHandler {
	h := &OIDCLoginHandler{
		auth:        auth,
		sessions:    newLoginStateStore(),
		redirectURI: "/",
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Register mounts the login routes on the given mux.
func (h *OIDCLoginHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/oauth/login", h.handleLogin)
	mux.HandleFunc("/oauth/callback", h.handleCallback)
	mux.HandleFunc("/oauth/logout", h.handleLogout)
}

// providerFor returns the OIDC provider for a tenant, using the injected factory
// (tests) or building one from the tenant's config + K8s secret (production).
func (h *OIDCLoginHandler) providerFor(r *http.Request, tc *agentorcav1alpha1.TenantConfig) (oidc.PrincipalProvider, error) {
	if h.factory != nil {
		return h.factory(tc)
	}
	return h.auth.NewProviderForTenant(r.Context(), tc)
}

// handleLogin renders the tenant picker, or — when ?tenant=<name> is given —
// redirects the browser to that tenant's IdP.
func (h *OIDCLoginHandler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		http.Error(w, `{"error":"oidc login not configured"}`, http.StatusServiceUnavailable)
		return
	}
	tenant := r.URL.Query().Get("tenant")
	if tenant == "" {
		h.renderPicker(w)
		return
	}
	tc := h.auth.tenantConfigFor(tenant)
	if tc == nil || tc.Spec.AuthMode != "federated" || !fedIsLoginCapable(tc.Spec.Federated) {
		http.Error(w, `{"error":"no OIDC login configured for tenant "}`+tenant, http.StatusNotFound)
		return
	}
	provider, err := h.providerFor(r, tc)
	if err != nil {
		slog.Error("oidc login: building provider failed", "tenant", tenant, "err", err)
		http.Error(w, `{"error":"login provider unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	state, nonce := randomState(), randomState()
	authURL, codeVerifier := provider.AuthCodeURL(state, nonce)
	h.sessions.Set(state, tenant, nonce, codeVerifier)
	http.SetCookie(w, &http.Cookie{
		Name: oidcStateCookie, Value: state, Path: "/",
		MaxAge: int(oidcLoginStateTTL.Seconds()), HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: h.cookieSecure,
	})
	http.Redirect(w, r, authURL, http.StatusFound)
}

// renderPicker lists the federated tenants that support browser login.
func (h *OIDCLoginHandler) renderPicker(w http.ResponseWriter) {
	tenants := h.auth.FederatedLoginTenants()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	body := `<!doctype html><html><head><title>Sign in to agent-orca</title></head><body>
<h2>Sign in to agent-orca</h2>
<p>Choose an identity provider:</p>
<ul>`
	if len(tenants) == 0 {
		body += "<li><em>No OIDC login tenants configured.</em></li>"
	}
	for _, tc := range tenants {
		name := html.EscapeString(tc.Name)
		issuer := html.EscapeString(tc.Spec.Federated.IssuerURL)
		body += fmt.Sprintf(`<li><a href="/oauth/login?tenant=%s">%s</a> &mdash; <code>%s</code></li>`, name, name, issuer)
	}
	body += "</ul></body></html>"
	_, _ = w.Write([]byte(body))
}

// handleCallback completes the auth-code flow for the tenant carried in the state.
func (h *OIDCLoginHandler) handleCallback(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		http.Error(w, `{"error":"oidc login not configured"}`, http.StatusServiceUnavailable)
		return
	}
	code := r.FormValue("code")
	state := r.FormValue("state")

	// CSRF: the state returned by the IdP must match the cookie we set.
	stateCookie, err := r.Cookie(oidcStateCookie)
	if err != nil || stateCookie.Value != state {
		http.Error(w, `{"error":"invalid login state"}`, http.StatusBadRequest)
		return
	}
	st, ok := h.sessions.Take(state)
	if !ok {
		http.Error(w, `{"error":"login expired; try signing in again"}`, http.StatusBadRequest)
		return
	}
	tc := h.auth.tenantConfigFor(st.tenant)
	if tc == nil {
		http.Error(w, `{"error":"tenant not found"}`, http.StatusBadRequest)
		return
	}
	provider, err := h.providerFor(r, tc)
	if err != nil {
		slog.Error("oidc callback: building provider failed", "tenant", st.tenant, "err", err)
		http.Error(w, `{"error":"login provider unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	principal, err := provider.ExchangeAndVerify(r.Context(), code, st.nonce, st.codeVerifier)
	if err != nil {
		http.Error(w, `{"error":"authentication failed"}`, http.StatusUnauthorized)
		return
	}
	// Ensure the login was valid: reject unverified emails (Google/Keycloak).
	if ev, ok := principal.Claims["email_verified"]; ok && !claimIsTrue(ev) {
		http.Error(w, `{"error":"email not verified"}`, http.StatusUnauthorized)
		return
	}

	ident := attachQuotaFields(&TenantIdentity{
		TenantName:    tc.Name,
		Namespace:     tc.Spec.TargetNamespace,
		AllowedAgents: tc.Spec.AllowedAgents,
		UserID:        principal.UserID,
		Groups:        principal.Groups,
	}, tc)

	signed, err := h.auth.MintSessionForIdentity(ident)
	if err != nil {
		slog.Error("oidc: failed to mint session jwt", "err", err)
		http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
		return
	}

	// Clear the CSRF state cookie; set the session cookie.
	http.SetCookie(w, &http.Cookie{
		Name: oidcStateCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: h.cookieSecure,
	})
	http.SetCookie(w, &http.Cookie{
		Name: oidcSessionCookie, Value: signed, Path: "/",
		MaxAge: int(jwtDefaultExpiry.Seconds()), HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: h.cookieSecure,
	})
	http.Redirect(w, r, h.redirectURI, http.StatusFound)
}

// handleLogout destroys the session.
func (h *OIDCLoginHandler) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: oidcSessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: h.cookieSecure,
	})
	http.Redirect(w, r, h.redirectURI, http.StatusFound)
}

// verifySession reads the session cookie and validates the carried agent-orca-issued
// JWT via the existing ExternalAuth.ValidateToken path. Returns the identity, or
// (nil, false) when there is no valid session.
func (h *OIDCLoginHandler) verifySession(r *http.Request) (*TenantIdentity, bool) {
	if h.auth == nil {
		return nil, false
	}
	c, err := r.Cookie(oidcSessionCookie)
	if err != nil || c.Value == "" {
		return nil, false
	}
	ident, err := h.auth.ValidateToken(r.Context(), c.Value)
	if err != nil || ident == nil {
		return nil, false
	}
	return ident, true
}

// claimIsTrue reports whether an OIDC claim value denotes a truthy boolean.
// go-oidc unmarshals ID-token claims into map[string]any: booleans arrive as bool.
func claimIsTrue(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	if s, ok := v.(string); ok {
		return s == "true"
	}
	return false
}

// randomState returns a URL-safe opaque token for OAuth2 state/nonce.
func randomState() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
