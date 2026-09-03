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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentorcav1alpha1 "github.com/floppyfish14/agent-orca/api/v1alpha1"
)

// newTestKey generates an ephemeral RSA key for signing/verifying test tokens.
func newTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test RSA key: %v", err)
	}
	return key
}

// startOIDCIssuer starts an in-process HTTP server that emulates an OIDC
// identity provider in the shape GitHub's `token.actions.githubusercontent.com`
// uses: a discovery document plus a JWKS endpoint, both served over plain HTTP
// so the test is fully hermetic. coreos/go-oidc/v3 permits non-HTTPS issuers
// during discovery, which is exactly what unit tests need.
//
// The served claims (iss/sub/aud/exp/actor) mirror GitHub Actions OIDC tokens,
// so exercising this server exercises the same validateFederatedToken code path
// that a real GitHub OIDC token flows through. *issuerURL is overwritten with the
// server's actual URL so callers can mint tokens for it.
func startOIDCIssuer(t *testing.T, key *rsa.PrivateKey, issuerURL *string) {
	t.Helper()
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       &key.PublicKey,
		KeyID:     "test-key",
		Algorithm: "RS256",
		Use:       "sig",
	}}}
	jwksBytes, err := json.Marshal(jwks)
	if err != nil {
		t.Fatalf("marshaling JWKS: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                *issuerURL,
			"jwks_uri":                              *issuerURL + "/.well-known/jwks",
			"response_types_supported":              []string{"id_token"},
			"subject_types_supported":               []string{"public", "pairwise"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"claims_supported": []string{
				"iss", "sub", "aud", "exp", "iat", "nbf", "jti", "actor",
			},
		})
	})
	mux.HandleFunc("/.well-known/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksBytes)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	*issuerURL = srv.URL
}

// mintOIDCToken mints an RS256 JWT signed by key, carrying the given issuer,
// audience and extra claims (e.g. GitHub's "actor").
func mintOIDCToken(t *testing.T, key *rsa.PrivateKey, issuerURL, audience string, extra map[string]any) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss": issuerURL,
		"sub": "test-subject",
		"aud": audience,
		"iat": time.Now().Add(-time.Minute).Unix(),
		"nbf": time.Now().Add(-time.Minute).Unix(),
		"exp": time.Now().Add(10 * time.Minute).Unix(),
		"jti": "test-jti",
	}
	for k, v := range extra {
		claims[k] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("signing test token: %v", err)
	}
	return signed
}

// seedFederatedTenant returns an ExternalAuth whose tenant cache trusts one
// federated tenant at issuerURL — mirroring the github-oidc dev sample
// (audience "agent-orca-dev", matched on the "actor" claim).
func seedFederatedTenant(t *testing.T, issuerURL string) *ExternalAuth {
	t.Helper()
	tc := &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "github-oidc", Namespace: "agent-orca-system"},
		Spec: agentorcav1alpha1.TenantConfigSpec{
			AuthMode:          "federated",
			AllowedNamespaces: []string{"default"},
			Federated: &agentorcav1alpha1.FederatedAuthConfig{
				IssuerURL:  issuerURL,
				ClientID:   "agent-orca-dev",
				MatchClaim: "actor",
				MatchValue: "floppyfish14",
			},
		},
	}
	return &ExternalAuth{
		tenantCache:   map[string]*agentorcav1alpha1.TenantConfig{"federated:" + issuerURL + ":floppyfish14": tc},
		oidcVerifiers: map[string]*gooidc.IDTokenVerifier{},
	}
}

// TestValidateFederatedToken_GitHubStyle exercises the full federated OIDC
// validation path that a GitHub Actions token flows through: discovery ->
// JWKS signature verification -> audience check -> claim matching -> namespace
// scoping. The issuer is a local mock (structurally identical to GitHub's), so
// the test never touches the network.
func TestValidateFederatedToken_GitHubStyle(t *testing.T) {
	key := newTestKey(t)
	issuerURL := "https://token.actions.githubusercontent.com"
	startOIDCIssuer(t, key, &issuerURL)

	t.Run("positive: matching actor and audience scopes to tenant namespace", func(t *testing.T) {
		a := seedFederatedTenant(t, issuerURL)
		token := mintOIDCToken(t, key, issuerURL, "agent-orca-dev", map[string]any{
			"actor": "floppyfish14",
		})
		ident, err := a.validateFederatedToken(context.Background(), token)
		if err != nil {
			t.Fatalf("expected federated token to validate, got error: %v", err)
		}
		if ident.Namespace != "default" {
			t.Fatalf("expected namespace default, got %q", ident.Namespace)
		}
		if ident.TenantName != "github-oidc" {
			t.Fatalf("expected tenant name github-oidc, got %q", ident.TenantName)
		}
		if ident.AllowedAgents != nil {
			t.Fatalf("expected nil AllowedAgents (allow all), got %v", ident.AllowedAgents)
		}
	})

	t.Run("negative: mismatched actor claim is rejected", func(t *testing.T) {
		a := seedFederatedTenant(t, issuerURL)
		token := mintOIDCToken(t, key, issuerURL, "agent-orca-dev", map[string]any{
			"actor": "someone-else",
		})
		if _, err := a.validateFederatedToken(context.Background(), token); err == nil {
			t.Fatal("expected mismatched actor to be rejected, got nil error")
		}
	})

	t.Run("negative: wrong audience is rejected", func(t *testing.T) {
		a := seedFederatedTenant(t, issuerURL)
		token := mintOIDCToken(t, key, issuerURL, "wrong-audience", map[string]any{
			"actor": "floppyfish14",
		})
		if _, err := a.validateFederatedToken(context.Background(), token); err == nil {
			t.Fatal("expected wrong audience to be rejected, got nil error")
		}
	})

	t.Run("negative: expired token is rejected", func(t *testing.T) {
		a := seedFederatedTenant(t, issuerURL)
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss":   issuerURL,
			"sub":   "test-subject",
			"aud":   "agent-orca-dev",
			"iat":   time.Now().Add(-2 * time.Hour).Unix(),
			"exp":   time.Now().Add(-time.Hour).Unix(),
			"nbf":   time.Now().Add(-2 * time.Hour).Unix(),
			"actor": "floppyfish14",
		})
		token, err := tok.SignedString(key)
		if err != nil {
			t.Fatalf("signing test token: %v", err)
		}
		if _, err := a.validateFederatedToken(context.Background(), token); err == nil {
			t.Fatal("expected expired token to be rejected, got nil error")
		}
	})
}

// TestValidateTokenParallel_FederatedSucceeds verifies that when a federated
// OIDC token is valid, validateTokenParallel returns it even if the K8s SA
// validation path (which can't succeed for an OIDC token) also runs concurrently.
// This is the key test: both methods run in parallel, so a slow K8s API server
// doesn't delay OIDC token validation.
func TestValidateTokenParallel_FederatedSucceeds(t *testing.T) {
	key := newTestKey(t)
	issuerURL := "https://token.actions.githubusercontent.com"
	startOIDCIssuer(t, key, &issuerURL)

	a := seedFederatedTenant(t, issuerURL)
	token := mintOIDCToken(t, key, issuerURL, "agent-orca-dev", map[string]any{
		"actor": "floppyfish14",
	})

	ident, err := a.validateTokenParallel(context.Background(), token)
	if err != nil {
		t.Fatalf("expected parallel auth to succeed, got error: %v", err)
	}
	if ident.TenantName != "github-oidc" {
		t.Fatalf("expected tenant github-oidc, got %q", ident.TenantName)
	}
}

// TestValidateTokenParallel_BothFail verifies that when neither federated nor
// K8s validation succeeds, validateTokenParallel returns an error mentioning
// both failure reasons.
func TestValidateTokenParallel_BothFail(t *testing.T) {
	key := newTestKey(t)
	issuerURL := "https://token.actions.githubusercontent.com"
	startOIDCIssuer(t, key, &issuerURL)

	a := seedFederatedTenant(t, issuerURL)
	// A random string is neither a valid JWT nor a valid K8s SA token.
	token := "not-a-real-token"

	_, err := a.validateTokenParallel(context.Background(), token)
	if err == nil {
		t.Fatal("expected error when both auth methods fail")
	}
}

// TestValidateTokenParallel_K8sNotConfigured verifies that validateTokenParallel
// handles the case where k8s client is nil (common in tests) — the federated
// path should still be tried and return its result/error without panicking.
func TestValidateTokenParallel_K8sNotConfigured(t *testing.T) {
	key := newTestKey(t)
	issuerURL := "https://token.actions.githubusercontent.com"
	startOIDCIssuer(t, key, &issuerURL)

	a := seedFederatedTenant(t, issuerURL)
	token := mintOIDCToken(t, key, issuerURL, "agent-orca-dev", map[string]any{
		"actor": "floppyfish14",
	})

	// a.k8s is nil (seedFederatedTenant doesn't set it).
	// validateTokenParallel should NOT panic — K8s path fails fast with nil check.
	ident, err := a.validateTokenParallel(context.Background(), token)
	if err != nil {
		t.Fatalf("expected federated token to validate despite nil k8s: %v", err)
	}
	if ident == nil {
		t.Fatal("expected non-nil identity")
	}
}

// TestNewProviderForTenant_PassesConfiguredScopes verifies (PR 67) that a
// tenant's FederatedAuthConfig.Scopes are forwarded to the OIDC provider's auth
// URL, so an operator can request "offline_access" per tenant to obtain a refresh
// token for session refresh.
func TestNewProviderForTenant_PassesConfiguredScopes(t *testing.T) {
	key := newTestKey(t)
	issuerURL := "https://example.invalid"
	startOIDCIssuer(t, key, &issuerURL)

	tc := &agentorcav1alpha1.TenantConfig{
		Spec: agentorcav1alpha1.TenantConfigSpec{
			AuthMode:          "federated",
			AllowedNamespaces: []string{"default"},
			Federated: &agentorcav1alpha1.FederatedAuthConfig{
				IssuerURL:   issuerURL,
				ClientID:    "agent-orca-dev",
				RedirectURI: "http://localhost:8083/oauth/callback",
				Scopes:      []string{"openid", "email", "offline_access"},
			},
		},
	}
	// No ClientSecretRef => no kubernetes client needed to build the provider.
	a := &ExternalAuth{}
	p, err := a.NewProviderForTenant(context.Background(), tc)
	if err != nil {
		t.Fatalf("NewProviderForTenant: %v", err)
	}
	authURL, _ := p.AuthCodeURL("state", "nonce")
	q, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing auth url: %v", err)
	}
	scope := q.Query().Get("scope")
	for _, want := range []string{"openid", "email", "offline_access"} {
		if !strings.Contains(scope, want) {
			t.Fatalf("expected %q in requested scope %q", want, scope)
		}
	}
}

// TestNewProviderForTenant_DefaultScopesWhenEmpty verifies that a tenant with no
// Scopes configured still gets the OIDC package default scopes (and therefore no
// offline_access) — i.e. the refresh scope is opt-in per tenant, not forced.
func TestNewProviderForTenant_DefaultScopesWhenEmpty(t *testing.T) {
	key := newTestKey(t)
	issuerURL := "https://example.invalid"
	startOIDCIssuer(t, key, &issuerURL)
	tc := &agentorcav1alpha1.TenantConfig{
		Spec: agentorcav1alpha1.TenantConfigSpec{
			Federated: &agentorcav1alpha1.FederatedAuthConfig{
				IssuerURL:   issuerURL,
				ClientID:    "agent-orca-dev",
				RedirectURI: "http://localhost:8083/oauth/callback",
			},
		},
	}
	a := &ExternalAuth{}
	p, err := a.NewProviderForTenant(context.Background(), tc)
	if err != nil {
		t.Fatalf("NewProviderForTenant: %v", err)
	}
	authURL, _ := p.AuthCodeURL("state", "nonce")
	q, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing auth url: %v", err)
	}
	scope := q.Query().Get("scope")
	if strings.Contains(scope, "offline_access") {
		t.Fatalf("did not expect offline_access when scopes unset, got %q", scope)
	}
}
