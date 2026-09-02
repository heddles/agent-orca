/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSecretIO is an in-memory SecretIO for tests.
type fakeSecretIO struct {
	mu   sync.Mutex
	data map[string]map[string][]byte
}

func newFakeSecretIO() *fakeSecretIO { return &fakeSecretIO{data: map[string]map[string][]byte{}} }

func (f *fakeSecretIO) key(ns, name string) string { return ns + "/" + name }

func (f *fakeSecretIO) ReadKey(_ context.Context, ns, name, k string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.data[f.key(ns, name)]
	if !ok {
		return nil, false, nil
	}
	v, ok := m[k]
	return v, ok, nil
}

func (f *fakeSecretIO) WriteKey(_ context.Context, ns, name, k string, v []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.data[f.key(ns, name)]
	if !ok {
		m = map[string][]byte{}
		f.data[f.key(ns, name)] = m
	}
	m[k] = v
	return nil
}

// fakeOAuthServer serves a minimal RFC 8414 / MCP OAuth metadata tree plus a token
// endpoint. Each successful token request returns a distinct access token and bumps a
// request counter so tests can assert refresh cadence.
type fakeOAuthServer struct {
	*httptest.Server
	mu            sync.Mutex
	tokenRequests int
}

func newFakeOAuthServer(t *testing.T) *fakeOAuthServer {
	t.Helper()
	f := &fakeOAuthServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ProtectedResourceMetadata{
			AuthorizationServers:   []string{f.Server.URL},
			BearerMethodsSupported: []string{"header", "form"},
			ScopesSupported:        []string{"search:read.public", "channels:history", "users:read"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AuthorizationServerMetadata{
			Issuer:                            f.Server.URL,
			AuthorizationEndpoint:             f.Server.URL + "/oauth/authorize",
			TokenEndpoint:                     f.Server.URL + "/oauth/token",
			GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
			TokenEndpointAuthMethodsSupported: []string{"client_secret_post"},
			CodeChallengeMethodsSupported:     []string{"S256"},
			ScopesSupported:                   []string{"search:read.public", "channels:history", "users:read"},
		})
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.tokenRequests++
		access := fmt.Sprintf("xoxp-FRESH-%d", f.tokenRequests)
		f.mu.Unlock()
		grant := r.PostForm.Get("grant_type")
		switch grant {
		case "refresh_token":
			rt := r.PostForm.Get("refresh_token")
			// Accept the seed refresh token and the rotated one the server itself mints.
			if rt != "valid-refresh" && rt != "xoxe-ROTATED-REFRESH" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_grant"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":            true,
				"access_token":  access,
				"refresh_token": "xoxe-ROTATED-REFRESH",
				"token_type":    "user",
				"expires_in":    3600,
			})
		case "authorization_code":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":            true,
				"access_token":  "xoxp-FROM-CODE",
				"refresh_token": "xoxe-FROM-CODE-REFRESH",
				"token_type":    "user",
				"expires_in":    3600,
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "unsupported_grant_type"})
		}
	})
	f.Server = httptest.NewServer(mux)
	return f
}

func (f *fakeOAuthServer) tokenRequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenRequests
}

func baseCfg(srv *fakeOAuthServer) Config {
	return Config{
		ServerURL:             srv.URL + "/mcp",
		ClientID:              "test-client-id",
		ClientSecret:          "test-client-secret",
		RedirectURI:           "http://localhost:8083/oauth/callback",
		Scopes:                []string{"search:read.public", "channels:history", "users:read"},
		RefreshToken:          "valid-refresh",
		RefreshTokenSecretRef: SecretKeyRef{Namespace: "default", Name: "slack-mcp-oauth", Key: "refresh_token"},
		AccessTokenSecretRef:  SecretKeyRef{Namespace: "default", Name: "slack-mcp-token", Key: "token"},
		RefreshInterval:       50 * time.Millisecond,
	}
}

func TestDiscover(t *testing.T) {
	srv := newFakeOAuthServer(t)
	defer srv.Close()
	m := New(srv.Client(), newFakeSecretIO())

	meta, err := m.Discover(context.Background(), srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if meta.AuthorizationEndpoint != srv.URL+"/oauth/authorize" {
		t.Errorf("AuthorizationEndpoint = %q", meta.AuthorizationEndpoint)
	}
	if meta.TokenEndpoint != srv.URL+"/oauth/token" {
		t.Errorf("TokenEndpoint = %q", meta.TokenEndpoint)
	}
	if !supportsGrant(meta.GrantTypes, "refresh_token") {
		t.Errorf("grant types %v missing refresh_token", meta.GrantTypes)
	}
	if !supportsString(meta.CodeChallengeMethods, "S256") {
		t.Errorf("code challenge methods %v missing S256", meta.CodeChallengeMethods)
	}
}

func TestRefreshWritesAccessTokenAndRotatesRefresh(t *testing.T) {
	srv := newFakeOAuthServer(t)
	defer srv.Close()
	secrets := newFakeSecretIO()
	m := New(srv.Client(), secrets)
	meta, err := m.Discover(context.Background(), srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	cfg := baseCfg(srv)

	if _, err := m.RefreshOnce(context.Background(), meta, &cfg); err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}
	if n := srv.tokenRequestCount(); n != 1 {
		t.Fatalf("expected exactly 1 token request, got %d", n)
	}

	got, ok, err := secrets.ReadKey(context.Background(), "default", "slack-mcp-token", "token")
	if err != nil || !ok {
		t.Fatalf("access token secret not written: ok=%v err=%v", ok, err)
	}
	if string(got) != "xoxp-FRESH-1" {
		t.Errorf("access token = %q, want xoxp-FRESH-1", string(got))
	}
	// Refresh token persisted + rotated in the config for the next tick.
	rt, ok, err := secrets.ReadKey(context.Background(), "default", "slack-mcp-oauth", "refresh_token")
	if err != nil || !ok {
		t.Fatalf("refresh token secret not written: ok=%v err=%v", ok, err)
	}
	if string(rt) != "xoxe-ROTATED-REFRESH" {
		t.Errorf("refresh token = %q, want xoxe-ROTATED-REFRESH", string(rt))
	}
	if cfg.RefreshToken != "xoxe-ROTATED-REFRESH" {
		t.Errorf("cfg.RefreshToken not updated = %q", cfg.RefreshToken)
	}
}

// TestRefreshIsPure verifies the one-shot Refresh helper performs the token exchange
// but does NOT persist anything to the Secret store (only RefreshOnce/Run do).
func TestRefreshIsPure(t *testing.T) {
	srv := newFakeOAuthServer(t)
	defer srv.Close()
	secrets := newFakeSecretIO()
	m := New(srv.Client(), secrets)
	meta, err := m.Discover(context.Background(), srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	cfg := baseCfg(srv)

	if _, err := m.Refresh(context.Background(), meta, &cfg); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if n := srv.tokenRequestCount(); n != 1 {
		t.Errorf("expected 1 token request, got %d", n)
	}
	if _, ok, _ := secrets.ReadKey(context.Background(), "default", "slack-mcp-token", "token"); ok {
		t.Error("Refresh must not write the access token secret")
	}
}

func TestRefreshInvalidGrantWritesNoToken(t *testing.T) {
	srv := newFakeOAuthServer(t)
	defer srv.Close()
	secrets := newFakeSecretIO()
	m := New(srv.Client(), secrets)
	meta, err := m.Discover(context.Background(), srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	cfg := baseCfg(srv)
	cfg.RefreshToken = "revoked-refresh" // server rejects this

	if _, err := m.RefreshOnce(context.Background(), meta, &cfg); err == nil {
		t.Fatal("expected error for invalid_grant, got nil")
	}
	// No bogus token must be written to the secret.
	if _, ok, _ := secrets.ReadKey(context.Background(), "default", "slack-mcp-token", "token"); ok {
		t.Error("expected NO access token written on invalid_grant")
	}
}

func TestExchangeCode(t *testing.T) {
	srv := newFakeOAuthServer(t)
	defer srv.Close()
	secrets := newFakeSecretIO()
	m := New(srv.Client(), secrets)
	meta, err := m.Discover(context.Background(), srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	cfg := baseCfg(srv)
	cfg.RefreshToken = "" // pure seed path

	resp, err := m.ExchangeCode(context.Background(), meta, &cfg, "authcode", "verifier")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if resp.AccessToken != "xoxp-FROM-CODE" {
		t.Errorf("access token = %q", resp.AccessToken)
	}
	if resp.RefreshToken != "xoxe-FROM-CODE-REFRESH" {
		t.Errorf("refresh token = %q", resp.RefreshToken)
	}
}

func TestLoginURLS256Challenge(t *testing.T) {
	srv := newFakeOAuthServer(t)
	defer srv.Close()
	m := New(srv.Client(), newFakeSecretIO())
	meta, err := m.Discover(context.Background(), srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	cfg := baseCfg(srv)
	authURL, verifier, err := m.LoginURL(meta, &cfg)
	if err != nil {
		t.Fatalf("LoginURL: %v", err)
	}
	if !strings.HasPrefix(authURL, meta.AuthorizationEndpoint+"?") {
		t.Errorf("authURL = %q", authURL)
	}
	if !strings.Contains(authURL, "code_challenge_method=S256") {
		t.Errorf("authURL missing S256: %q", authURL)
	}
	if !strings.Contains(authURL, "code_challenge=") {
		t.Errorf("authURL missing challenge: %q", authURL)
	}
	// The challenge must verify against the returned verifier (S256).
	q, _ := url.ParseQuery(strings.TrimPrefix(authURL, meta.AuthorizationEndpoint+"?"))
	if !verifyPKCE(verifier, q.Get("code_challenge")) {
		t.Error("code_challenge does not match code_verifier under S256")
	}
}

func TestRunRefreshesOnCadence(t *testing.T) {
	srv := newFakeOAuthServer(t)
	defer srv.Close()
	secrets := newFakeSecretIO()
	m := New(srv.Client(), secrets)
	meta, err := m.Discover(context.Background(), srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	cfg := baseCfg(srv)
	cfg.RefreshInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := m.Run(ctx, meta, &cfg); err != nil {
		t.Fatalf("Run returned error before ctx cancel: %v", err)
	}

	// At least the initial refresh + one tick refresh happened.
	if n := srv.tokenRequestCount(); n < 2 {
		t.Errorf("expected >=2 token requests, got %d", n)
	}
	// The secret holds a rotated access token past the initial one, proving the loop
	// rewrote it. (Exact count-vs-secret match is avoided: a request in-flight at ctx
	// cancellation may bump the counter without a successful write.)
	got, ok, err := secrets.ReadKey(context.Background(), "default", "slack-mcp-token", "token")
	if err != nil || !ok {
		t.Fatalf("access token secret not written: ok=%v err=%v", ok, err)
	}
	if !strings.HasPrefix(string(got), "xoxp-FRESH-") || string(got) == "xoxp-FRESH-1" {
		t.Errorf("secret token = %q, want a rotated xoxp-FRESH-N (N>1)", string(got))
	}
}
