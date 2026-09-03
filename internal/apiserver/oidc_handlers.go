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
	"encoding/base64"
	"encoding/json"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
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
	logoResolver logoResolver    // test seam; nil => defaultLogoResolver
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

// WithOIDCLogoResolver injects a logo resolver used when rendering the
// /oauth/login picker (test seam). Production callers can omit it and
// defaultLogoResolver is used, which safely resolves each issuer's logo from its
// OIDC discovery document and falls back to the issuer origin favicon.
func WithOIDCLogoResolver(r logoResolver) OIDCLoginOption {
	return func(h *OIDCLoginHandler) { h.logoResolver = r }
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
	if h.logoResolver == nil {
		h.logoResolver = defaultLogoResolver
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
		h.renderPicker(w, r)
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

// ── Picker view model ────────────────────────────────────────────────────────

// pickerTenant is the view model for a single row in the /oauth/login picker.
type pickerTenant struct {
	Name, Issuer, LogoURL, LoginURL string
}

// pickerPage is the template data for the login picker.
type pickerPage struct {
	Tenants []pickerTenant
}

// renderPicker renders the OIDC tenant picker: a branded, dark-themed page that
// lists every federated tenant configured for the interactive login flow, each as
// a tappable card that redirects to that tenant's IdP.
//
// The markup mirrors the agent-orca operator UI's design-system tokens (colors,
// radii, elevation, typography) so the login screen feels like part of the same
// product. Lucide iconography (the same icon pack the React UI imports from
// "lucide-react") is inlined as source-identical SVGs, because this page is
// server-rendered and not part of the React bundle. Issuer logos are resolved
// per-tenant and only rendered when they are safe to display (see defaultLogoResolver).
func (h *OIDCLoginHandler) renderPicker(w http.ResponseWriter, r *http.Request) {
	tenants := h.auth.FederatedLoginTenants()
	sort.Slice(tenants, func(i, j int) bool { return tenants[i].Name < tenants[j].Name })

	logos := h.resolveLogos(r.Context(), tenants)

	rows := make([]pickerTenant, 0, len(tenants))
	for _, tc := range tenants {
		rows = append(rows, pickerTenant{
			Name:     tc.Name,
			Issuer:   tc.Spec.Federated.IssuerURL,
			LogoURL:  logos[tc.Name],
			LoginURL: "/oauth/login?tenant=" + url.QueryEscape(tc.Name),
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	setPickerSecurityHeaders(w)
	if err := pickerTmpl.Execute(w, pickerPage{Tenants: rows}); err != nil {
		slog.Warn("oidc picker template execution failed", "err", err)
	}
}

// resolveLogos resolves a logo URL for each tenant concurrently, bounding cold
// render latency (one per-issuer discovery timeout) and caching per issuer.
// Failing or slow issuers never block the picker: the favicon fallback is
// always available synchronously, and resolveLogos returns "" for unreachable
// issuers so the picker renders an inline icon instead.
func (h *OIDCLoginHandler) resolveLogos(ctx context.Context, tenants []*agentorcav1alpha1.TenantConfig) map[string]string {
	resolver := h.logoResolver
	if resolver == nil {
		resolver = defaultLogoResolver
	}
	out := make(map[string]string, len(tenants))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, tc := range tenants {
		wg.Add(1)
		go func(tc *agentorcav1alpha1.TenantConfig) {
			defer wg.Done()
			logo := resolver(ctx, tc.Spec.Federated.IssuerURL)
			mu.Lock()
			out[tc.Name] = logo
			mu.Unlock()
		}(tc)
	}
	wg.Wait()
	return out
}

// ── Issuer logo resolution ──────────────────────────────────────────────────

// logoResolver resolves a safe, browser-loadable image URL for an OIDC issuer's
// logo, or returns "" when no safe logo is available (the picker then renders a
// generic inline icon instead). See defaultLogoResolver for the security contract.
type logoResolver func(ctx context.Context, issuerURL string) string

// Picker logo resolution constants. The discovery fetch is best-effort and bounded;
// results are cached per issuer so the picker stays snappy after the first load.
const (
	logoDiscoveryTimeout = 1 * time.Second // bounds the cold first-render per issuer
	logoCacheTTL         = 30 * time.Minute
	logoFetchUA          = "agent-orca-oidc-picker/1.0"
)

// logoHTTP is the shared HTTP client for OIDC discovery lookups in the picker.
// It can be swapped in tests (see TestDefaultLogoResolverDiscovery).
var logoHTTP = &http.Client{Timeout: logoDiscoveryTimeout}

// logoCache memoizes resolved issuer logo URLs ("") means "use the favicon").
var logoCache sync.Map // issuerURL -> *logoCacheEntry

type logoCacheEntry struct {
	url     string
	expires time.Time
}

// defaultLogoResolver resolves a safe logo URL for issuerURL by consulting the
// issuer's OIDC discovery document and falling back to the issuer origin
// favicon. Returns "" when no safe logo can be derived (the picker renders an
// icon instead).
//
// Security contract (the caller asked for "if that does not pose a security
// risk"):
//   - Only HTTPS issuers are considered — matching OIDC's own requirement and
//     preventing mixed-content / insecure-image loads. Non-HTTPS (e.g. local
//     dev issuers) get the inline icon, never an image.
//   - The discovery document is fetched ONLY from admin-configured, HTTPS issuer
//     URLs — the same trust boundary NewProvider/getOrCreateVerifier already rely
//     on for federated token validation. No user input or bearer token reaches
//     this fetch, so it does not expand the SSRF surface.
//   - The image bytes are never fetched by the operator: the logo URL is handed
//     to the browser as a CSS background-image, so any tracking visibility lands
//     on the browser-to-issuer leg (inherent to choosing that IdP for login).
//   - Any resolved logo URL must be HTTPS and on the issuer's own host (see
//     safeLogoURL), so a crafted discovery document cannot redirect an in-browser
//     image load at an arbitrary tracker host or an HTTP endpoint.
func defaultLogoResolver(ctx context.Context, issuerURL string) string {
	issuer, err := url.Parse(issuerURL)
	if err != nil || !issuer.IsAbs() || issuer.Scheme != "https" || issuer.Host == "" {
		// Non-HTTPS issuers (e.g. local dev) get the inline icon, never an image.
		return ""
	}

	favicon := issuer.Scheme + "://" + issuer.Host + "/favicon.ico"

	if cached, ok := logoCache.Load(issuerURL); ok {
		if e := cached.(*logoCacheEntry); e.expires.After(time.Now()) {
			if e.url != "" {
				return e.url
			}
			return favicon
		}
	}

	logo := resolveLogoFromDiscovery(ctx, issuer)
	logoCache.Store(issuerURL, &logoCacheEntry{url: logo, expires: time.Now().Add(logoCacheTTL)})

	if logo != "" {
		return logo
	}
	return favicon
}

// resolveLogoFromDiscovery fetches <issuer>/.well-known/openid-configuration and
// returns the first valid, issuer-hosted HTTPS "logo" field, or "". The OIDC
// well-known document lives at {IssuerURL}/.well-known/openid-configuration,
// which preserves the issuer's path (e.g. https://idp/oauth2/default/.well-known/...).
// The OIDC Discovery 1.0 spec does not define a logo field, but some providers
// publish one (commonly "logo_uri"); we honor a small set of common names.
func resolveLogoFromDiscovery(ctx context.Context, issuer *url.URL) string {
	wellKnown := strings.TrimRight(issuer.String(), "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", logoFetchUA)
	req.Header.Set("Accept", "application/json")
	resp, err := logoHTTP.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&raw); err != nil {
		return ""
	}
	for _, key := range []string{"logo_uri", "logo_url", "logo"} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil || s == "" {
			continue
		}
		// Relative logo paths are resolved against the issuer origin.
		if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
			if u, err := issuer.Parse(s); err == nil {
				s = u.String()
			} else {
				continue
			}
		}
		if safeLogoURL(issuer, s) {
			return s
		}
	}
	return ""
}

// safeLogoURL reports whether logoURL is safe to render as the issuer's logo
// image. It is the data-layer gate that complements the picker's Content-Security
// Policy: only HTTPS URLs on the issuer's own host — and free of characters that
// could escape the inline style / url() context — are accepted.
func safeLogoURL(issuer *url.URL, logoURL string) bool {
	u, err := url.Parse(logoURL)
	if err != nil || !u.IsAbs() || u.Scheme != "https" || u.Host == "" {
		return false
	}
	// The logo must stay on the issuer's own origin so a crafted discovery
	// document cannot smuggle a cross-origin tracking pixel.
	if u.Host != issuer.Host {
		return false
	}
	// Block characters that could break out of the CSS url('...') context.
	for _, c := range logoURL {
		switch c {
		case '\'', '"', ';', ' ', '\t', '\n', '\r', '(', ')':
			return false
		}
	}
	return true
}

// pickerCSP hardens the login page: no inline scripts, no framing, and images
// restricted to HTTPS. Issuer favicons are rendered as CSS background-images
// (governed by img-src), and the only inline `style` used is a validated,
// issuer-hosted HTTPS URL or the design-system stylesheet.
const pickerCSP = "default-src 'self'; img-src https:; style-src 'self' 'unsafe-inline'; font-src 'self'; " +
	"script-src 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'"

// setPickerSecurityHeaders applies defense-in-depth response headers to the
// login page. These sit alongside the picker's own CSP; the picker renders no
// inline scripts and only ever references issuer-hosted HTTPS favicons.
func setPickerSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Content-Security-Policy", pickerCSP)
}

// pickerHTML is the /oauth/login picker template. It mirrors the agent-orca UI
// design system (dark default, light via prefers-color-scheme, tabular nums,
// reduced-motion, focus-visible) and inlines Lucide SVGs (1.5px stroke, 24x24
// viewBox — the same shapes lucide-react renders) so the page is
// self-contained and served from the Go backend without the React bundle.
//
// Values are placed through html/template so they are contextually escaped; the
// favicon background-image is additionally validated server-side by
// safeLogoURL before it reaches the template.
var pickerTmpl = template.Must(template.New("oidcPicker").Parse(pickerHTML))

// pickerHTML is the raw template text for the login picker.
const pickerHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in to agent-orca</title>
<style>
:root{
  --ds-bg:#0f172a;--ds-surface:#1e293b;--ds-surface-hover:#253244;--ds-border:#334155;
  --ds-text-primary:#f1f5f9;--ds-text-secondary:#94a3b8;--ds-text-muted:#647489;--ds-accent:#3b82f6;
  --ds-card-shadow:0 1px 3px rgba(0,0,0,.10),0 1px 2px rgba(0,0,0,.06);
  --ds-card-shadow-hover:0 4px 12px rgba(0,0,0,.15);
}
@media (prefers-color-scheme: light){
  :root{
    --ds-bg:#f8fafc;--ds-surface:#fff;--ds-surface-hover:#f1f5f6;--ds-border:#e2e8f0;
    --ds-text-primary:#0f172a;--ds-text-secondary:#475569;--ds-text-muted:#647489;--ds-accent:#2563eb;
    --ds-card-shadow:0 1px 3px rgba(0,0,0,.06),0 1px 2px rgba(0,0,0,.04);
    --ds-card-shadow-hover:0 4px 12px rgba(0,0,0,.08);
  }
}
@media (prefers-reduced-motion: reduce){*,*::before,*::after{transition-duration:.01ms!important;animation-duration:.01ms!important}}
*{margin:0;padding:0;box-sizing:border-box}
html{font-size:16px;-webkit-font-smoothing:antialiased;-moz-osx-font-smoothing:grayscale;font-family:system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;font-variant-numeric:tabular-nums;background:var(--ds-bg);color:var(--ds-text-primary);height:100%}
body{min-height:100%;display:flex;flex-direction:column;align-items:center;justify-content:center;padding:24px;font-size:13px;line-height:1.6}
.ao-topbar{height:52px;background:var(--ds-surface);border-bottom:1px solid var(--ds-border);display:flex;align-items:center;padding:0 20px;flex-shrink:0}
.ao-brand{display:inline-flex;align-items:center;gap:8px;font-size:15px;font-weight:700;color:var(--ds-text-primary);letter-spacing:-.03em;text-decoration:none}
.ao-brand-icon{width:20px;height:20px;color:var(--ds-accent)}
.ao-card{background:var(--ds-surface);border:1px solid var(--ds-border);border-radius:12px;box-shadow:var(--ds-card-shadow);padding:32px;width:100%;max-width:400px;transition-property:box-shadow,transform;transition-duration:.15s;transition-timing-function:ease}
.ao-card:hover{box-shadow:var(--ds-card-shadow-hover);transform:translateY(-1px)}
.ao-hero{text-align:center;margin-bottom:24px}
.ao-hero-icon{width:48px;height:48px;border-radius:12px;background:rgba(59,130,246,.10);display:inline-flex;align-items:center;justify-content:center;margin-bottom:16px;color:var(--ds-accent)}
.ao-hero-icon svg{width:24px;height:24px}
.ao-hero-title{font-size:24px;font-weight:600;margin-bottom:6px}
.ao-hero-sub{color:var(--ds-text-secondary);font-size:13px}
.ao-list{display:flex;flex-direction:column;gap:10px;width:100%}
.ao-tile{display:inline-flex;align-items:center;gap:12px;padding:12px 14px;border-radius:10px;background:var(--ds-surface);border:1px solid var(--ds-border);text-decoration:none;color:inherit;width:100%;transition-property:background-color,border-color,box-shadow,transform;transition-duration:.15s;transition-timing-function:ease}
.ao-tile:hover{background:var(--ds-surface-hover);transform:translateY(-1px);box-shadow:var(--ds-card-shadow-hover)}
.ao-tile:focus-visible{outline:2px solid var(--ds-accent);outline-offset:2px}
.ao-avatar{width:40px;height:40px;border-radius:9999px;background:var(--ds-surface-hover);flex-shrink:0;overflow:hidden;display:inline-flex;align-items:center;justify-content:center;background-size:32px 32px;background-position:center;background-repeat:no-repeat;color:var(--ds-text-muted)}
.ao-avatar svg{width:20px;height:20px}
.ao-tile-body{min-width:0;flex:1;overflow:hidden}
.ao-tile-name{font-size:14px;font-weight:600;color:var(--ds-text-primary);display:block;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.ao-tile-issuer{font-size:11px;color:var(--ds-text-muted);display:block;font-family:ui-monospace,"SFMono-Regular",Menlo,Monaco,"Consolas",monospace;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.ao-tile-sign{margin-left:auto;display:inline-flex;align-items:center;gap:6px;font-size:13px;font-weight:600;color:var(--ds-accent);flex-shrink:0}
.ao-tile-sign svg{width:16px;height:16px}
.ao-empty{color:var(--ds-text-secondary);font-size:13px;text-align:center;padding:24px 16px}
.ao-footer{color:var(--ds-text-muted);font-size:11px;text-align:center;margin-top:28px}
</style>
</head>
<body>
  <header class="ao-topbar">
    <span class="ao-brand" aria-label="agent-orca">
      <svg class="ao-brand-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <rect x="4" y="8" width="16" height="12" rx="2"/><path d="M12 8V4H8"/><path d="M2 14h2"/><path d="M20 14h2"/><path d="M15 13v2"/><path d="M9 13v2"/>
      </svg>
      <span>agent-orca</span>
    </span>
  </header>
  <main style="width:100%;max-width:400px">
  <div class="ao-card">
    <div class="ao-hero">
      <span class="ao-hero-icon" aria-hidden="true">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
          <rect x="4" y="8" width="16" height="12" rx="2"/><path d="M12 8V4H8"/><path d="M2 14h2"/><path d="M20 14h2"/><path d="M15 13v2"/><path d="M9 13v2"/>
        </svg>
      </span>
      <h1 class="ao-hero-title">Sign in to agent-orca</h1>
      <p class="ao-hero-sub">Choose your identity provider to continue</p>
    </div>
    {{if .Tenants}}
    <div class="ao-list">
      {{range .Tenants}}
      <a href="{{.LoginURL}}" class="ao-tile">
        {{if .LogoURL}}
        <span class="ao-avatar" style="background-image:url('{{.LogoURL}}')"></span>
        {{else}}
        <span class="ao-avatar">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <rect x="4" y="2" width="16" height="20" rx="2"/><path d="M12 10h.01"/><path d="M16 10h.01"/><path d="M8 10h.01"/><path d="M12 14h.01"/><path d="M16 14h.01"/><path d="M8 14h.01"/><path d="M12 6h.01"/><path d="M16 6h.01"/><path d="M8 6h.01"/><path d="M9 22v-3a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v3"/>
          </svg>
        </span>
        {{end}}
        <span class="ao-tile-body">
          <span class="ao-tile-name">{{.Name}}</span>
          <span class="ao-tile-issuer" title="{{.Issuer}}">{{.Issuer}}</span>
        </span>
        <span class="ao-tile-sign">Sign in
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <path d="m10 17 5-5-5-5"/><path d="M15 12H3"/><path d="M15 3h4a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-4"/>
          </svg>
        </span>
      </a>
      {{end}}
    </div>
    {{else}}
    <div class="ao-empty">
      <p style="margin-bottom:4px;font-weight:600">No identity providers configured</p>
      <p>If you expected to sign in, contact your administrator.</p>
    </div>
    {{end}}
  </div>
  <footer class="ao-footer">agent-orca</footer>
  </main>
</body>
</html>`

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
		Namespace:     firstNamespace(tc.Spec.AllowedNamespaces),
		Namespaces:    tc.Spec.AllowedNamespaces,
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
