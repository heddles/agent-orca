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

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/floppyfish14/agent-orca/internal/security/oidc"
)

// defaultOIDCRedirectURI is the loopback callback the CLI listens on by default.
// 127.0.0.1 (IPv4 loopback) is used instead of "localhost" so there is no
// IPv4/IPv6 resolution ambiguity between the browser redirect and the listener.
const defaultOIDCRedirectURI = "http://127.0.0.1:8765/callback"

// oidcLoginGracePeriod is subtracted from the id_token expiry when deciding
// whether a cached OIDC session is "still valid" — refreshing a hair early
// avoids race-losing a request to an expiring token.
const oidcLoginGracePeriod = 5 * time.Minute

// oidcLoginTimeout bounds the overall interactive OIDC login flow (browser
// callback wait). It must be generous enough for a human to complete the
// browser-based authorization-code login, but bounded so the process can never
// hang indefinitely — previously a missing deadline on context.Background()
// caused the process to block until SIGKILL when the IdP was unreachable or
// no browser was available.
const oidcLoginTimeout = 10 * time.Minute

// oidcHTTPTimeout bounds individual HTTP calls within the OIDC flow (provider
// discovery, JWKS verification, token exchange). These should complete in
// seconds; the long timeout is a safety net against a slow/unreachable IdP so
// a discovery hang doesn't consume the entire interactive deadline.
const oidcHTTPTimeout = 30 * time.Second

// callbackSuccessHTML is the page shown to the user's browser after the loopback
// callback receives the authorization code.
const callbackSuccessHTML = "<!doctype html><html><body>" +
	"<h1>Login successful</h1><p>You can close this window.</p></body></html>"

// OIDCLoginConfig is the credential + routing config for the interactive
// authorization-code (browser) OIDC login flow.
type OIDCLoginConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string // empty is allowed for public (PKCE-only) clients
	RedirectURI  string // e.g. http://127.0.0.1:8765/callback
	NoBrowser    bool
	// OpenBrowser launches the system browser to the IdP's authorization URL.
	// It is injected so the flow is testable without a real display; when nil
	// (and NoBrowser is false) a cross-platform opener is used.
	OpenBrowser func(url string) error
}

// OIDCLoginResult is what the CLI obtains from a successful OIDC login and
// persists for subsequent API calls + future refresh.
type OIDCLoginResult struct {
	// IDToken is the bearer token the External/ACP APIs accept as a federated
	// JWT (verified via the issuer's JWKS).
	IDToken string
	// RefreshToken may be empty when the IdP did not return one.
	RefreshToken string
	// ExpiresAt is the id_token's exp claim. Zero if it could not be parsed.
	ExpiresAt time.Time
	// Principal is the fully-verified principal (carries subject/email/groups).
	Principal *oidc.IDTokenPrincipal
}

// loginOIDC runs the OIDC authorization-code flow end-to-end: discovery, browser
// launch, a local loopback callback server to receive the authorization code,
// then code exchange + id_token verification. The returned ID token is accepted
// by the agent-orca External/ACP API as a federated bearer token (validated via
// the issuer's JWKS by ExternalAuth.validateFederatedToken), so no server-side
// changes are required for this path.
func loginOIDC(ctx context.Context, cfg OIDCLoginConfig, out io.Writer) (*OIDCLoginResult, error) {
	if cfg.IssuerURL == "" || cfg.ClientID == "" {
		return nil, errors.New("oidc: --issuer-url and --client-id are required")
	}
	if cfg.RedirectURI == "" {
		cfg.RedirectURI = defaultOIDCRedirectURI
	}
	// A CLI callback listener must be loopback — binding a remote host would
	// expose the authorization code to the network.
	if !callbackHostIsLoopback(cfg.RedirectURI) {
		return nil, fmt.Errorf("oidc: --redirect-uri must use a loopback address (got %q); a CLI callback cannot bind a remote host", cfg.RedirectURI) //nolint:lll
	}

	// Discovery: bound with a short HTTP timeout so an unreachable/slow IdP
	// fails fast instead of hanging for the full interactive deadline.
	discoverCtx, discoverCancel := context.WithTimeout(ctx, oidcHTTPTimeout)
	defer discoverCancel()
	provider, err := oidc.NewProvider(discoverCtx, oidc.Config{
		IssuerURL:    cfg.IssuerURL,
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURI:  cfg.RedirectURI,
		PKCE:         true,
	})
	if err != nil {
		return nil, fmt.Errorf("oidc: %w", err)
	}

	state, nonce := randomState(), randomState()
	authURL, codeVerifier := provider.AuthCodeURL(state, nonce)

	listenAddr := parseListenAddr(cfg.RedirectURI)
	codeCh, cancel, err := startCallbackServer(listenAddr, cfg.RedirectURI, state, out)
	if err != nil {
		return nil, err
	}
	defer cancel()

	// Informing message. --no-browser tells the user to open the URL themselves;
	// otherwise the CLI attempts to open it. The URL is always printed too — the
	// auto-open can fail silently in headless/CI environments, and a printed URL
	// lets the user open it manually on any machine that can reach the IdP.
	if cfg.NoBrowser {
		_, _ = fmt.Fprintf(out, "Visit this URL in your browser to sign in:\n  %s\n", authURL)
	} else {
		_, _ = fmt.Fprintln(out, "Opening your browser to sign in…")
		_, _ = fmt.Fprintf(out, "If a browser does not open, visit: %s\n", authURL)
	}
	_, _ = fmt.Fprintln(out, "Waiting for sign-in to complete… (Ctrl+C to abort)")

	// Drive the browser visit. An injected opener (tests) is always invoked so
	// the flow can be driven programmatically; otherwise, unless --no-browser,
	// the platform opener is used. --no-browser with no injected opener leaves
	// the block waiting for a human to open the URL.
	switch {
	case cfg.OpenBrowser != nil:
		_ = cfg.OpenBrowser(authURL)
	case !cfg.NoBrowser:
		_ = defaultOpenBrowser(authURL)
	}

	select {
	case res := <-codeCh:
		if res.err != nil {
			return nil, res.err
		}
		// Token exchange + JWKS verification: bound with a short HTTP timeout.
		exchangeCtx, exchangeCancel := context.WithTimeout(ctx, oidcHTTPTimeout)
		principal, err := provider.ExchangeAndVerify(exchangeCtx, res.code, nonce, codeVerifier)
		exchangeCancel()
		if err != nil {
			return nil, fmt.Errorf("exchanging authorization code: %w", err)
		}
		if principal.RawIDToken == "" {
			return nil, errors.New("oidc: login succeeded but no id_token was returned")
		}
		exp, _ := jwtExpiry(principal.RawIDToken) // best-effort; expiry is optional for the caller
		_, _ = fmt.Fprintln(out, "logged in via OIDC.")
		// Diagnostic summary so the user can confirm the federated TenantConfig on
		// the operator matches the token the IdP issued (a mismatch here is the
		// usual cause of a 401 after a successful login).
		_, _ = fmt.Fprintf(out, "  issuer:  %s\n", principal.Issuer)
		if aud := audClaim(principal.Claims); aud != "" {
			_, _ = fmt.Fprintf(out, "  audience: %s\n", aud)
		}
		if !exp.IsZero() {
			_, _ = fmt.Fprintf(out, "  expires: %s\n", exp.UTC().Format(time.RFC3339))
		}
		if principal.RefreshToken != "" {
			_, _ = fmt.Fprintln(out, "  refresh: captured (session auto-refreshes)")
		} else {
			_, _ = fmt.Fprintln(out, "  refresh: none (re-login before expiry)")
		}
		return &OIDCLoginResult{
			IDToken:      principal.RawIDToken,
			RefreshToken: principal.RefreshToken,
			ExpiresAt:    exp,
			Principal:    principal,
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// callbackResult carries the authorization code (or an error) received by the
// local callback server from the IdP redirect.
type callbackResult struct {
	code string
	err  error
}

// startCallbackServer starts an HTTP server on listenAddr that handles the OIDC
// redirect at the path embedded in redirectURI. It returns a channel that
// receives exactly one callbackResult, and a cancel func that shuts the server
// down. expectedState is validated as a CSRF guard: a mismatched state is
// rejected.
func startCallbackServer(
	listenAddr, redirectURI, expectedState string,
	out io.Writer,
) (<-chan callbackResult, func(), error) {
	callbackPath := callbackPath(redirectURI)
	if callbackPath == "" {
		callbackPath = "/"
	}

	mux := http.NewServeMux()
	resCh := make(chan callbackResult, 1)
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if state := r.FormValue("state"); state != expectedState {
			resCh <- callbackResult{err: errors.New("oidc: state mismatch (CSRF check failed)")}
			http.Error(w, "invalid login state", http.StatusBadRequest)
			return
		}
		code := r.FormValue("code")
		if code == "" {
			resCh <- callbackResult{err: errors.New("oidc: authorization code missing from callback")}
			http.Error(w, "missing authorization code", http.StatusBadRequest)
			return
		}
		// The browser (or test fake) followed the IdP 302 here. A short
		// success page lets a browser close the tab cleanly.
		_, _ = io.WriteString(w, callbackSuccessHTML)
		resCh <- callbackResult{code: code}
	})

	srv := &http.Server{Addr: listenAddr, Handler: mux}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("starting local callback server on %s: %w", listenAddr, err)
	}
	go func() {
		if serr := srv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			_, _ = fmt.Fprintf(out, "oidc callback server stopped: %v\n", serr)
		}
	}()
	cancel := func() { _ = srv.Close() }
	return resCh, cancel, nil
}

// parseListenAddr extracts the host:port from a redirect URI suitable for
// net.Listen. A redirect URI of "http://localhost:8765/callback" yields
// "localhost:8765". loopback-only is enforced by the caller for security.
func parseListenAddr(redirectURI string) string {
	u, err := url.Parse(redirectURI)
	if err != nil || u.Host == "" {
		return "127.0.0.1:8765"
	}
	return u.Host
}

// callbackPath returns the URL path portion of a redirect URI (e.g. "/callback").
func callbackPath(redirectURI string) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return ""
	}
	return u.Path
}

// callbackHost is the loopback host of a redirect URI, normalized so "localhost"
// is accepted as loopback.
func callbackHostIsLoopback(redirectURI string) bool {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// defaultOpenBrowser opens a URL with the platform's default browser.
func defaultOpenBrowser(u string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = "open", []string{u}
	case "linux":
		name, args = "xdg-open", []string{u}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", u}
	default:
		return fmt.Errorf("unsupported platform for browser opening: %s", runtime.GOOS)
	}
	return exec.Command(name, args...).Start()
}

// randomState returns a URL-safe opaque token for OAuth2 state/nonce.
func randomState() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// jwtExpiry parses the unverified `exp` claim of a JWT. It is used to decide
// when an OIDC id_token is close enough to expiry to refresh. Verification of
// the token is performed elsewhere (ExchangeAndVerify / validateFederatedToken).
func jwtExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("not a jwt")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("decoding jwt payload: %w", err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("parsing jwt claims: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, errors.New("jwt has no exp claim")
	}
	return time.Unix(claims.Exp, 0), nil
}

// audClaim renders the `aud` claim of an id_token for the login diagnostic. The
// audience is either a single string or a list of strings (per OIDC spec).
func audClaim(claims map[string]any) string {
	if claims == nil {
		return ""
	}
	switch v := claims["aud"].(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	}
	return ""
}
