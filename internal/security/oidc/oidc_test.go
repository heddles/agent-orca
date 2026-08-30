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

package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
)

// testNonce is the fixed nonce reused across mintIDToken call sites in these tests.
const testNonce = "nonce1"

func newTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	return k
}

func jwksBytes(t *testing.T, key *rsa.PrivateKey) []byte {
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

// mintIDToken mints an RS256 JWT signed by key, with the given issuer/audience/
// nonce and extra GitHub-style claims (actor, groups).
func mintIDToken(t *testing.T, key *rsa.PrivateKey, issuer, aud, nonce string, extra map[string]any) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":   issuer,
		"sub":   "test-subject",
		"aud":   aud,
		"iat":   time.Now().Add(-time.Minute).Unix(),
		"nbf":   time.Now().Add(-time.Minute).Unix(),
		"exp":   time.Now().Add(10 * time.Minute).Unix(),
		"jti":   "test-jti",
		"nonce": nonce,
	}
	maps.Copy(claims, extra)
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("signing id token: %v", err)
	}
	return s
}

// newMockIdP starts an in-process OIDC provider that serves discovery, an
// authorization endpoint, a token endpoint (returns the id_token from idTokenFn),
// and a JWKS endpoint. Issuer/endpoint URLs are derived from the live request host
// so the discovery doc is always self-consistent with the running server.
func newMockIdP(t *testing.T, key *rsa.PrivateKey, idTokenFn func() string) string {
	t.Helper()
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
			"claims_supported": []string{
				"iss", "sub", "aud", "exp", "iat", "nbf", "jti", "nonce", "actor", "groups", "email",
			},
		})
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.FormValue("grant_type") {
		case "refresh_token":
			// Issue a fresh id_token (no nonce — refresh isn't nonce-bound) and a
			// rotated refresh token so Provider.Refresh can be exercised.
			issuer := "http://" + r.Host
			fresh := mintIDToken(t, key, issuer, "agent-orca-dev", "", nil)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id_token":      fresh,
				"access_token":  "refreshed-access-token",
				"refresh_token": "rotated-refresh-token",
				"token_type":    "Bearer",
				"expires_in":    3600,
			})
		default: // authorization_code (used by ExchangeAndVerify)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id_token":      idTokenFn(),
				"access_token":  "mock-access-token",
				"refresh_token": "test-refresh-token",
				"token_type":    "Bearer",
				"expires_in":    3600,
			})
		}
	})
	mux.HandleFunc("/.well-known/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksBytes(t, key))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func newTestProvider(t *testing.T, issuerURL string) *Provider {
	t.Helper()
	p, err := NewProvider(context.Background(), Config{
		IssuerURL:    issuerURL,
		ClientID:     "agent-orca-dev",
		ClientSecret: "secret",
		RedirectURI:  "http://localhost:8083/oauth/callback",
		Scopes:       []string{"openid", "email", "profile"},
		PKCE:         true,
		ClaimMappings: struct {
			UserID string
			Groups string
			Email  string
		}{UserID: "sub", Groups: "groups", Email: "email"},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

func TestProvider_AuthCodeURL_GeneratesPKCE(t *testing.T) {
	key := newTestKey(t)
	var idToken string
	issuer := newMockIdP(t, key, func() string { return idToken })
	p := newTestProvider(t, issuer)

	authURL, codeVerifier := p.AuthCodeURL("state1", testNonce)
	if !strings.Contains(authURL, "code_challenge=") {
		t.Fatalf("auth URL missing code_challenge: %s", authURL)
	}
	if !strings.Contains(authURL, "code_challenge_method=S256") {
		t.Fatalf("auth URL missing code_challenge_method=S256: %s", authURL)
	}
	if !strings.Contains(authURL, "nonce=nonce1") {
		t.Fatalf("auth URL missing nonce param: %s", authURL)
	}
	if codeVerifier == "" {
		t.Fatal("expected non-empty PKCE code_verifier when PKCE is enabled")
	}
}

func TestExchangeAndVerify_CapturesRefreshToken(t *testing.T) {
	key := newTestKey(t)
	var idToken string
	issuer := newMockIdP(t, key, func() string { return idToken })
	p := newTestProvider(t, issuer)

	_, verifier := p.AuthCodeURL("s", testNonce)
	idToken = mintIDToken(t, key, issuer, "agent-orca-dev", testNonce, nil)

	principal, err := p.ExchangeAndVerify(context.Background(), "any-code", testNonce, verifier)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if principal.RefreshToken != "test-refresh-token" {
		t.Fatalf("RefreshToken = %q, want test-refresh-token", principal.RefreshToken)
	}
}

func TestRefresh_Positive(t *testing.T) {
	key := newTestKey(t)
	var idToken string
	issuer := newMockIdP(t, key, func() string { return idToken })
	p := newTestProvider(t, issuer)

	// First do a normal exchange to obtain a refresh token.
	_, verifier := p.AuthCodeURL("s", testNonce)
	idToken = mintIDToken(t, key, issuer, "agent-orca-dev", testNonce, nil)
	principal, err := p.ExchangeAndVerify(context.Background(), "any-code", testNonce, verifier)
	if err != nil {
		t.Fatalf("initial exchange: %v", err)
	}
	if principal.RefreshToken != "test-refresh-token" {
		t.Fatalf("RefreshToken = %q, want test-refresh-token", principal.RefreshToken)
	}

	// Now refresh — the mock mints a fresh id_token + rotated refresh token.
	newID, newRefresh, err := p.Refresh(context.Background(), principal.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if newID == "" || newID == principal.RawIDToken {
		t.Fatalf("expected a fresh, different id_token; got empty or unchanged")
	}
	if newRefresh != "rotated-refresh-token" {
		t.Fatalf("rotated RefreshToken = %q, want rotated-refresh-token", newRefresh)
	}
}

func TestRefresh_EmptyToken(t *testing.T) {
	key := newTestKey(t)
	var idToken string
	issuer := newMockIdP(t, key, func() string { return idToken })
	p := newTestProvider(t, issuer)
	if _, _, err := p.Refresh(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty refresh token")
	}
}

func TestRefresh_WithoutNonce(t *testing.T) {
	// A refresh-token grant response does not carry the original nonce; Refresh
	// must still verify the minted id_token by iss/aud/exp only.
	key := newTestKey(t)
	var idToken string
	issuer := newMockIdP(t, key, func() string { return idToken })
	p := newTestProvider(t, issuer)

	_, verifier := p.AuthCodeURL("s", testNonce)
	idToken = mintIDToken(t, key, issuer, "agent-orca-dev", testNonce, nil)
	principal, err := p.ExchangeAndVerify(context.Background(), "any-code", testNonce, verifier)
	if err != nil {
		t.Fatalf("initial exchange: %v", err)
	}
	if _, _, err := p.Refresh(context.Background(), principal.RefreshToken); err != nil {
		t.Fatalf("Refresh without nonce should succeed: %v", err)
	}
}

func TestExchangeAndVerify_Positive(t *testing.T) {
	key := newTestKey(t)
	var idToken string
	issuer := newMockIdP(t, key, func() string { return idToken })
	p := newTestProvider(t, issuer)

	nonce := testNonce
	_, codeVerifier := p.AuthCodeURL("state1", nonce)

	idToken = mintIDToken(t, key, issuer, "agent-orca-dev", nonce, map[string]any{
		"actor":  "floppyfish14",
		"groups": []string{"org:engineering", "team:platform"},
		"email":  "matt@poolside.ai",
	})

	principal, err := p.ExchangeAndVerify(context.Background(), "any-code", nonce, codeVerifier)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if principal.Subject != "test-subject" {
		t.Fatalf("Subject = %q, want test-subject", principal.Subject)
	}
	if principal.UserID != "test-subject" { // default UserIDClaim = "sub"
		t.Fatalf("UserID = %q, want test-subject", principal.UserID)
	}
	if principal.Email != "matt@poolside.ai" {
		t.Fatalf("Email = %q, want matt@poolside.ai", principal.Email)
	}
	wantGroups := []string{"org:engineering", "team:platform"}
	if !equalStrings(principal.Groups, wantGroups) {
		t.Fatalf("Groups = %v, want %v", principal.Groups, wantGroups)
	}
	if principal.Claims["actor"] != "floppyfish14" {
		t.Fatalf("raw actor claim = %v, want floppyfish14", principal.Claims["actor"])
	}
}

func TestExchangeAndVerify_WrongNonce(t *testing.T) {
	key := newTestKey(t)
	var idToken string
	issuer := newMockIdP(t, key, func() string { return idToken })
	p := newTestProvider(t, issuer)
	_, codeVerifier := p.AuthCodeURL("s", "nonce-real")

	// ID token carries a *different* nonce than the one passed to ExchangeAndVerify.
	idToken = mintIDToken(t, key, issuer, "agent-orca-dev", "nonce-other", nil)
	_, err := p.ExchangeAndVerify(context.Background(), "code", "nonce-real", codeVerifier)
	if err == nil {
		t.Fatal("expected nonce mismatch error, got nil")
	}
}

func TestExchangeAndVerify_WrongAudience(t *testing.T) {
	key := newTestKey(t)
	var idToken string
	issuer := newMockIdP(t, key, func() string { return idToken })
	p := newTestProvider(t, issuer)
	nonce := testNonce
	_, codeVerifier := p.AuthCodeURL("s", nonce)
	idToken = mintIDToken(t, key, issuer, "wrong-audience", nonce, nil)
	_, err := p.ExchangeAndVerify(context.Background(), "code", nonce, codeVerifier)
	if err == nil {
		t.Fatal("expected audience verification error, got nil")
	}
}

func TestExchangeAndVerify_ExpiredToken(t *testing.T) {
	key := newTestKey(t)
	var idToken string
	issuer := newMockIdP(t, key, func() string { return idToken })
	p := newTestProvider(t, issuer)
	nonce := testNonce
	_, codeVerifier := p.AuthCodeURL("s", nonce)

	claims := jwt.MapClaims{
		"iss":   issuer,
		"sub":   "test-subject",
		"aud":   "agent-orca-dev",
		"iat":   time.Now().Add(-2 * time.Hour).Unix(),
		"exp":   time.Now().Add(-time.Hour).Unix(),
		"nbf":   time.Now().Add(-2 * time.Hour).Unix(),
		"nonce": nonce,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	idToken = s

	_, err = p.ExchangeAndVerify(context.Background(), "code", nonce, codeVerifier)
	if err == nil {
		t.Fatal("expected expired-token error, got nil")
	}
}

func TestExchangeAndVerify_TamperedSignature(t *testing.T) {
	key := newTestKey(t)
	other := newTestKey(t)
	var idToken string
	issuer := newMockIdP(t, key, func() string { return idToken })
	p := newTestProvider(t, issuer)
	nonce := testNonce
	_, codeVerifier := p.AuthCodeURL("s", nonce)
	// Sign with a *different* key than the one advertised in the JWKS.
	idToken = mintIDToken(t, other, issuer, "agent-orca-dev", nonce, nil)
	_, err := p.ExchangeAndVerify(context.Background(), "code", nonce, codeVerifier)
	if err == nil {
		t.Fatal("expected signature verification error, got nil")
	}
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
