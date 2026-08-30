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
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
	"github.com/spf13/cobra"
)

// Shared test fixtures so goconst stays happy and the mock + assertions agree.
const (
	testClientID = "agent-orca-dev"
	testSecret   = "secret"
	testRefreshA = "refresh-token-1"
	testRefreshB = "refresh-token-2"
)

// --- mock OIDC identity provider (full auth-code flow) ---

// mintIDToken mints an RS256 id_token with the standard OIDC claims so the
// go-oidc verifier accepts it. Mirrors the helper in internal/security/oidc tests.
func mintIDToken(t *testing.T, key *rsa.PrivateKey, issuer, aud, nonce string, extra map[string]any) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss": issuer,
		"sub": "test-subject",
		"aud": aud,
		"iat": time.Now().Add(-time.Minute).Unix(),
		"nbf": time.Now().Add(-time.Minute).Unix(),
		"exp": time.Now().Add(10 * time.Minute).Unix(),
		"jti": "test-jti",
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	merged := map[string]any{"email": "matt@example.com", "email_verified": true}
	maps.Copy(merged, extra)
	maps.Copy(claims, merged)
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("signing id token: %v", err)
	}
	return s
}

func jwksJSON(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       &key.PublicKey,
		KeyID:     "test-key",
		Algorithm: "RS256",
		Use:       "sig",
	}}}
	b, err := json.Marshal(jwks)
	if err != nil {
		t.Fatalf("marshaling JWKS: %v", err)
	}
	return b
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}

func newTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	return k
}

// authzRecord is what the mock IdP stores per issued authorization code so the
// token endpoint can validate PKCE and bind the id_token's nonce.
type authzRecord struct {
	nonce         string
	codeChallenge string
}

// mockIdP is an in-process OIDC provider implementing discovery, the authorize
// endpoint (issues a code + redirects to the loopback callback), the token
// endpoint (authorization_code + refresh_token grants, with PKCE validation), and
// a JWKS endpoint.
type mockIdP struct {
	server *httptest.Server

	mu    sync.Mutex
	codes map[string]authzRecord // code -> record
}

func newMockIdP(t *testing.T, key *rsa.PrivateKey) *mockIdP {
	t.Helper()
	m := &mockIdP{codes: make(map[string]authzRecord)}
	mux := http.NewServeMux()

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		issuer := "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/oauth/authorize",
			"token_endpoint":                        issuer + "/oauth/token",
			"jwks_uri":                              issuer + "/.well-known/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})

	mux.HandleFunc("/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("client_id") != testClientID || r.FormValue("response_type") != "code" {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
		state := r.FormValue("state")
		redirectURI := r.FormValue("redirect_uri")
		code := "code-" + randHex(12)
		m.mu.Lock()
		m.codes[code] = authzRecord{
			nonce:         r.FormValue("nonce"),
			codeChallenge: r.FormValue("code_challenge"),
		}
		m.mu.Unlock()
		u, perr := url.Parse(redirectURI)
		if perr != nil {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
		q := u.Query()
		q.Set("code", code)
		q.Set("state", state)
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})

	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.FormValue("grant_type") {
		case "authorization_code":
			code := r.FormValue("code")
			m.mu.Lock()
			rec, ok := m.codes[code]
			if ok {
				delete(m.codes, code)
			}
			m.mu.Unlock()
			if !ok {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			if rec.codeChallenge != "" {
				sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
				if base64.RawURLEncoding.EncodeToString(sum[:]) != rec.codeChallenge {
					http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
					return
				}
			}
			iss := "http://" + r.Host
			idToken := mintIDToken(t, key, iss, testClientID, rec.nonce, map[string]any{
				"groups": []string{"org:engineering"},
			})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id_token":      idToken,
				"access_token":  "access-token",
				"refresh_token": testRefreshA,
				"token_type":    "Bearer",
				"expires_in":    3600,
			})
		case "refresh_token":
			iss := "http://" + r.Host
			idToken := mintIDToken(t, key, iss, testClientID, "", nil)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id_token":      idToken,
				"access_token":  "refreshed-access",
				"refresh_token": testRefreshB,
				"token_type":    "Bearer",
				"expires_in":    3600,
			})
		default:
			http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		}
	})

	mux.HandleFunc("/.well-known/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwksJSON(t, key))
	})

	m.server = httptest.NewServer(mux)
	t.Cleanup(func() { m.server.Close() })
	return m
}

func (m *mockIdP) issuerURL() string { return m.server.URL }

// freePort returns a TCP port that is currently free on the loopback interface.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// fakeBrowser simulates the user clicking the authorization URL by fetching it.
// The IdP 302-redirects back into the CLI's already-listening callback server.
func fakeBrowser(u string) error {
	go func() {
		resp, err := http.Get(u)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return nil
}

// newInteractiveRoot builds a root command wired for interactive tests: a
// buffered stdin, an "always a TTY" terminal check, nil password reader (so
// secrets read from stdin) and AOCTL_CONFIG_DIR pointed at a temp dir.
func newInteractiveRoot(t *testing.T, stdin string) (*cobra.Command, *settings) {
	t.Helper()
	t.Setenv("AOCTL_CONFIG_DIR", t.TempDir())
	root, s := newRootCmd()
	s.out = &bytes.Buffer{}
	s.errw = &bytes.Buffer{}
	s.stdin = bufio.NewReader(strings.NewReader(stdin))
	s.isTerminal = func() bool { return true }
	s.readPassword = nil // read secrets from the injected stdin
	return root, s
}

func testStdout(s *settings) string {
	if b, ok := s.out.(*bytes.Buffer); ok {
		return b.String()
	}
	return ""
}

// TestLoginOIDC_EndToEnd drives the standalone loginOIDC flow against the mock
// IdP: browser redirect -> loopback callback -> code exchange -> id_token.
func TestLoginOIDC_EndToEnd(t *testing.T) {
	key := newTestKey(t)
	idp := newMockIdP(t, key)

	port := freePort(t)
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	cfg := OIDCLoginConfig{
		IssuerURL:    idp.issuerURL(),
		ClientID:     testClientID,
		ClientSecret: testSecret,
		RedirectURI:  redirect,
		OpenBrowser:  fakeBrowser,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	out := &bytes.Buffer{}
	res, err := loginOIDC(ctx, cfg, out)
	if err != nil {
		t.Fatalf("loginOIDC: %v\nstdout: %s", err, out.String())
	}
	if res.IDToken == "" {
		t.Fatal("expected non-empty id_token")
	}
	if _, err := jwtExpiry(res.IDToken); err != nil {
		t.Fatalf("id_token is not a parseable JWT: %v", err)
	}
	if res.RefreshToken != testRefreshA {
		t.Fatalf("RefreshToken = %q, want %s", res.RefreshToken, testRefreshA)
	}
	if res.Principal == nil || res.Principal.Subject != "test-subject" {
		t.Fatalf("bad principal: %+v", res.Principal)
	}
}

// TestLoginOIDC_DiagnosticOutput verifies the post-login summary prints the
// id_token issuer/audience/expiry so a user can compare them against the
// federated TenantConfig when diagnosing a subsequent 401.
func TestLoginOIDC_DiagnosticOutput(t *testing.T) {
	key := newTestKey(t)
	idp := newMockIdP(t, key)
	port := freePort(t)
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	cfg := OIDCLoginConfig{
		IssuerURL:    idp.issuerURL(),
		ClientID:     testClientID,
		ClientSecret: testSecret,
		RedirectURI:  redirect,
		OpenBrowser:  fakeBrowser,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	out := &bytes.Buffer{}
	res, err := loginOIDC(ctx, cfg, out)
	if err != nil {
		t.Fatalf("loginOIDC: %v\nstdout: %s", err, out.String())
	}
	log := out.String()
	for _, want := range []string{
		"logged in via OIDC.", "issuer:", "audience:", "expires:",
		"refresh: captured (session auto-refreshes)",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("expected %q in login output, got:\n%s", want, log)
		}
	}
	if !strings.Contains(log, idp.issuerURL()) {
		t.Fatalf("issuer in output should match the IdP: %q", log)
	}
	if res.RefreshToken != testRefreshA {
		t.Fatalf("RefreshToken = %q, want %s", res.RefreshToken, testRefreshA)
	}
}

// TestLoginOIDC_NoBrowser verifies --no-browser prints the URL and the flow
// still completes (an injected opener drives the redirect).
func TestLoginOIDC_NoBrowser(t *testing.T) {
	key := newTestKey(t)
	idp := newMockIdP(t, key)
	port := freePort(t)
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	cfg := OIDCLoginConfig{
		IssuerURL:    idp.issuerURL(),
		ClientID:     testClientID,
		ClientSecret: "",
		RedirectURI:  redirect,
		NoBrowser:    true,
		OpenBrowser:  fakeBrowser,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	out := &bytes.Buffer{}
	res, err := loginOIDC(ctx, cfg, out)
	if err != nil {
		t.Fatalf("loginOIDC: %v\nstdout: %s", err, out.String())
	}
	if res.IDToken == "" {
		t.Fatal("expected id_token")
	}
	if !strings.Contains(out.String(), "Visit this URL") {
		t.Fatalf("expected URL prompt in output, got: %q", out.String())
	}
}

// TestLoginOIDC_StateMismatch is a CSRF-safety regression: a callback carrying
// a state that doesn't match the one generated must fail.
func TestLoginOIDC_StateMismatch(t *testing.T) {
	port := freePort(t)
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	listenAddr := parseListenAddr(redirect)
	codeCh, cancel, err := startCallbackServer(listenAddr, redirect, "expected-state", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("startCallbackServer: %v", err)
	}
	defer cancel()

	go func() {
		_, _ = http.Get(redirect + "?code=x&state=wrong-state")
	}()

	select {
	case res := <-codeCh:
		if res.err == nil {
			t.Fatal("expected a state-mismatch error, got none")
		}
		if !strings.Contains(res.err.Error(), "state mismatch") {
			t.Fatalf("unexpected error: %v", res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for callback")
	}
}

// --- cobra-level interactive login tests ---

// TestCmdLogin_Picker_OAuth selects OAuth from the interactive menu and
// completes the client_credentials exchange using prompted credentials.
func TestCmdLogin_Picker_OAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-oauth","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()

	stdin := "1\nmy-client-id\nmy-client-secret\n"
	root, s := newInteractiveRoot(t, stdin)
	root.SetArgs([]string{"login", "--endpoint", srv.URL, "--acp-endpoint", srv.URL})

	if err := root.Execute(); err != nil {
		t.Fatalf("login: %v\nstdout: %s", err, testStdout(s))
	}
	if !strings.Contains(testStdout(s), "logged in to") {
		t.Fatalf("expected login message, got: %q", testStdout(s))
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Token != "tok-oauth" || cfg.AuthMethod != loginMethodOAuth {
		t.Fatalf("bad persisted config: token=%q method=%q", cfg.Token, cfg.AuthMethod)
	}
}

// TestCmdLogin_Picker_OIDC selects OIDC from the interactive menu, is prompted
// for issuer/client-id/secret, and completes the browser flow.
func TestCmdLogin_Picker_OIDC(t *testing.T) {
	key := newTestKey(t)
	idp := newMockIdP(t, key)
	port := freePort(t)
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	stdin := fmt.Sprintf("2\n%s\n%s\n\n", idp.issuerURL(), testClientID)
	root, s := newInteractiveRoot(t, stdin)
	s.openBrowser = fakeBrowser
	root.SetArgs([]string{"login", "--redirect-uri", redirect})

	if err := root.Execute(); err != nil {
		t.Fatalf("login: %v\nstdout: %s", err, testStdout(s))
	}
	if !strings.Contains(testStdout(s), "logged in via OIDC") {
		t.Fatalf("expected OIDC login message, got: %q", testStdout(s))
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.AuthMethod != loginMethodOIDC {
		t.Fatalf("AuthMethod = %q, want oidc", cfg.AuthMethod)
	}
	if cfg.Token == "" || cfg.RefreshToken != testRefreshA {
		t.Fatalf("bad persisted OIDC config: token=%q refresh=%q", cfg.Token, cfg.RefreshToken)
	}
	if cfg.IssuerURL != idp.issuerURL() {
		t.Fatalf("IssuerURL = %q, want %q", cfg.IssuerURL, idp.issuerURL())
	}
}

// TestCmdLogin_OIDC_Flags runs OIDC non-interactively via flags + a fake browser.
func TestCmdLogin_OIDC_Flags(t *testing.T) {
	key := newTestKey(t)
	idp := newMockIdP(t, key)
	port := freePort(t)
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	root, s := newInteractiveRoot(t, "")
	s.openBrowser = fakeBrowser
	s.isTerminal = func() bool { return false } // flags cover everything; no TTY needed
	root.SetArgs([]string{
		"login",
		"--auth-method", "oidc",
		"--issuer-url", idp.issuerURL(),
		"--client-id", testClientID,
		"--client-secret", testSecret,
		"--redirect-uri", redirect,
	})

	if err := root.Execute(); err != nil {
		t.Fatalf("login: %v\nstdout: %s", err, testStdout(s))
	}
	if !strings.Contains(testStdout(s), "logged in via OIDC") {
		t.Fatalf("expected OIDC login message, got: %q", testStdout(s))
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.AuthMethod != loginMethodOIDC || cfg.RefreshToken != testRefreshA {
		t.Fatalf("bad persisted config: %+v", cfg)
	}
}

// TestCmdLogin_Picker_NonTTY_Errors confirms that without flags and without a
// TTY the picker does not block — it returns a clear error.
func TestCmdLogin_Picker_NonTTY_Errors(t *testing.T) {
	root, s := newInteractiveRoot(t, "")
	s.isTerminal = func() bool { return false }
	root.SetArgs([]string{"login"})

	err := root.Execute()
	if err == nil {
		t.Fatalf("expected an error for non-TTY login with no flags\ndata: %s", testStdout(s))
	}
	if !strings.Contains(err.Error(), "no login method specified") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestCmdLogin_OIDC_NonTTY_MissingFlags confirms a non-TTY OIDC login without
// issuer/client-id flags errors rather than hanging on a prompt.
func TestCmdLogin_OIDC_NonTTY_MissingFlags(t *testing.T) {
	root, s := newInteractiveRoot(t, "")
	s.isTerminal = func() bool { return false }
	root.SetArgs([]string{"login", "--auth-method", "oidc"})

	err := root.Execute()
	if err == nil {
		t.Fatalf("expected an error for non-TTY OIDC login without issuer-url\ndata: %s", testStdout(s))
	}
	if !strings.Contains(err.Error(), "issuer-url") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestPromptSelection parses valid + invalid input and re-prompts.
func TestPromptSelection(t *testing.T) {
	opts := []promptOption{
		{Value: "oauth", Label: "OAuth"},
		{Value: "oidc", Label: "OIDC"},
	}
	t.Run("by_index", func(t *testing.T) {
		got, err := promptSelection(&bytes.Buffer{}, bufio.NewReader(strings.NewReader("1\n")), "pick", opts)
		if err != nil || got != "oauth" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("by_value", func(t *testing.T) {
		got, err := promptSelection(&bytes.Buffer{}, bufio.NewReader(strings.NewReader("oidc\n")), "pick", opts)
		if err != nil || got != "oidc" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	t.Run("reprompts_on_invalid", func(t *testing.T) {
		got, err := promptSelection(&bytes.Buffer{}, bufio.NewReader(strings.NewReader("9\noauth\n")), "pick", opts)
		if err != nil || got != "oauth" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
}

// --- refresh tests ---

// TestRefreshOIDCIfNeeded_ExchangesAndPersists verifies that an expired OIDC
// session is refreshed (token + refresh token rotated) and the new session is
// persisted to the config dir.
func TestRefreshOIDCIfNeeded_ExchangesAndPersists(t *testing.T) {
	key := newTestKey(t)
	idp := newMockIdP(t, key)

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	seed := &Config{
		Endpoint:      defaultEndpoint,
		ACP:           defaultACP,
		Token:         "expired-id-token",
		AuthMethod:    loginMethodOIDC,
		IssuerURL:     idp.issuerURL(),
		ClientID:      testClientID,
		ClientSecret:  testSecret,
		RedirectURI:   defaultOIDCRedirectURI,
		RefreshToken:  testRefreshA,
		IDTokenExpiry: past,
	}
	if err := saveConfig(seed); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	s := &settings{errw: &bytes.Buffer{}}
	fresh, refreshed, err := s.refreshOIDCIfNeeded(seed)
	if err != nil {
		t.Fatalf("refreshOIDCIfNeeded: %v", err)
	}
	if !refreshed || fresh == "" || fresh == "expired-id-token" {
		t.Fatalf("expected a fresh token; got refreshed=%v token=%q", refreshed, fresh)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Token != fresh || cfg.RefreshToken != testRefreshB {
		t.Fatalf("persisted config not updated: token=%q refresh=%q", cfg.Token, cfg.RefreshToken)
	}
	if cfg.IDTokenExpiry == "" {
		t.Fatal("expected idTokenExpiry to be persisted")
	}
}

// TestRefreshOIDCIfNeeded_SkipsWhenValid verifies a non-expired token is left
// untouched (no network call to the IdP).
func TestRefreshOIDCIfNeeded_SkipsWhenValid(t *testing.T) {
	future := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	seed := &Config{
		Token:         "valid-id-token",
		AuthMethod:    loginMethodOIDC,
		IssuerURL:     "http://127.0.0.1:0", // never contacted — refresh is skipped
		ClientID:      testClientID,
		ClientSecret:  testSecret,
		RedirectURI:   defaultOIDCRedirectURI,
		RefreshToken:  testRefreshA,
		IDTokenExpiry: future,
	}
	s := &settings{errw: &bytes.Buffer{}}
	fresh, refreshed, err := s.refreshOIDCIfNeeded(seed)
	if err != nil {
		t.Fatalf("refreshOIDCIfNeeded: %v", err)
	}
	if refreshed || fresh != "" {
		t.Fatalf("expected NO refresh; got refreshed=%v fresh=%q", refreshed, fresh)
	}
}
