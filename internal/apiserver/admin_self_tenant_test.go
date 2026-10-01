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
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

// federatedAuthForTest builds an ExternalAuth whose cache trusts one federated
// tenant served by a local mock OIDC issuer. signingKey is set so ValidateToken's
// issued-JWT branch doesn't nil-deref, and k8s is a no-op fake so the SA-token
// fallback returns "not authenticated" quickly instead of hanging on a nil client.
func federatedAuthForTest(t *testing.T) (*ExternalAuth, string, *rsa.PrivateKey) {
	t.Helper()
	key := newTestKey(t)
	issuerURL := githubIssuerURL
	startOIDCIssuer(t, key, &issuerURL)
	a := seedFederatedTenant(t, issuerURL)
	a.signingKey = newTestKey(t)
	a.k8s = k8sfake.NewSimpleClientset() //nolint:staticcheck // fake SA fallback; NewClientset needs apply configs
	return a, issuerURL, key
}

func doRequestWithPath(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithMethod(t, h, http.MethodGet, path, token)
}

// doRequestWithMethod issues a request with the given method and bearer token.
func doRequestWithMethod(t *testing.T, h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://example"+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

const saAdminToken = "sa-admin-token"

// Test fixtures for the GitHub-style federated OIDC tenant used by the
// self-tenant admin tests (and shared with other federated auth tests in
// this package).
const (
	// githubIssuerURL is GitHub's OIDC issuer URL, mimicked by a local mock issuer.
	githubIssuerURL = "https://token.actions.githubusercontent.com"
	// githubOIDCTenant is the TenantConfig name of the federated test tenant.
	githubOIDCTenant = "github-oidc"
)

func saAdminReviewer(username, token string) func(context.Context, string) (string, bool, error) {
	return func(_ context.Context, t string) (string, bool, error) {
		if t == token {
			return username, true, nil
		}
		return "", false, nil
	}
}

func TestSelfTenantOp(t *testing.T) {
	cases := []struct {
		method string
		path   string
		name   string
		ok     bool
	}{
		{http.MethodGet, "/admin/tenants/acme", "acme", true},             // self read
		{http.MethodPost, "/admin/tenants/acme/rotate-secret", "", false}, // mutation - SA-only
		{http.MethodDelete, "/admin/tenants/acme", "", false},             // delete - SA-only
		{http.MethodGet, "/admin/tenants", "", false},                     // list - SA-only
		{http.MethodGet, "/admin/tenants/", "", false},                    // empty name
		{http.MethodGet, "/admin/tenants/acme/sub", "", false},            // multi-segment
		{http.MethodGet, "/admin/agents", "", false},                      // wrong resource
		{http.MethodGet, "/v1/tasks", "", false},
	}
	for _, c := range cases {
		name, _, ok := selfTenantOp(c.method, c.path)
		if ok != c.ok || name != c.name {
			t.Errorf("selfTenantOp(%q,%q) = (%q,%v), want (%q,%v)", c.method, c.path, name, ok, c.name, c.ok)
		}
	}
}

// TestRequireSAOrSelfTenant_Unit checks the auth wrapper in isolation with a
// stub next handler: SA admin (allow), federated id_token for own tenant
// (allow), wrong tenant (403), no/bad token (401).
func TestRequireSAOrSelfTenant_Unit(t *testing.T) {
	auth, issuerURL, key := federatedAuthForTest(t)
	ownToken := mintOIDCToken(t, key, issuerURL, "agent-orca-dev", map[string]any{"actor": "floppyfish14"})

	// Sanity: the federated token validates to TenantName "github-oidc".
	if ident, err := auth.validateFederatedToken(context.Background(), ownToken); err != nil {
		t.Fatalf("sanity: federated token should validate, got: %v", err)
	} else if ident.TenantName != githubOIDCTenant {
		t.Fatalf("sanity: expected tenant github-oidc, got %q", ident.TenantName)
	}

	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// readCap mirrors the entry in selfTenantAdminOps today; the role-gated
	// variant below is built from it to exercise the RBAC hook.
	readCap := adminCapability{method: http.MethodGet, pathPrefix: "/admin/tenants/"}

	base := &ExternalAPIServer{
		auth:          auth,
		reviewSAToken: func(context.Context, string) (string, bool, error) { return "", false, nil },
		isAdminSA:     func(context.Context, string, string) (bool, error) { return false, nil },
	}

	t.Run("SA admin allowed", func(t *testing.T) {
		sa := *base
		sa.reviewSAToken = saAdminReviewer("system:serviceaccount:agent-orca-system:agentorca-admin", saAdminToken)
		sa.isAdminSA = func(context.Context, string, string) (bool, error) { return true, nil }
		rr := doRequestWithPath(t, sa.requireSAOrSelfTenant(githubOIDCTenant, readCap, stub), "/admin/tenants/github-oidc", saAdminToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("SA admin: expected 200, got %d %q", rr.Code, rr.Body.String())
		}
	})

	t.Run("federated own tenant allowed", func(t *testing.T) {
		rr := doRequestWithPath(t, base.requireSAOrSelfTenant(githubOIDCTenant, readCap, stub), "/admin/tenants/github-oidc", ownToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("federated own tenant: expected 200, got %d %q", rr.Code, rr.Body.String())
		}
	})

	t.Run("federated other tenant hidden", func(t *testing.T) {
		rr := doRequestWithPath(t, base.requireSAOrSelfTenant("some-other-tenant", readCap, stub), "/admin/tenants/some-other-tenant", ownToken)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("federated other tenant: expected 404, got %d %q", rr.Code, rr.Body.String())
		}
	})

	t.Run("federated missing required role denied", func(t *testing.T) {
		// Future-RBAC hook: a capability gated on a role the caller lacks is denied.
		gated := adminCapability{method: http.MethodPost, pathPrefix: "/admin/tenants/", role: "tenant-admin"}
		rr := doRequestWithPath(t, base.requireSAOrSelfTenant(githubOIDCTenant, gated, stub), "/admin/tenants/github-oidc", ownToken)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("missing required role: expected 404, got %d %q", rr.Code, rr.Body.String())
		}
	})

	t.Run("SA admin bypasses role gate", func(t *testing.T) {
		sa := *base
		sa.reviewSAToken = saAdminReviewer("system:serviceaccount:agent-orca-system:agentorca-admin", saAdminToken)
		sa.isAdminSA = func(context.Context, string, string) (bool, error) { return true, nil }
		gated := adminCapability{method: http.MethodPost, pathPrefix: "/admin/tenants/", role: "tenant-admin"}
		rr := doRequestWithPath(t, sa.requireSAOrSelfTenant(githubOIDCTenant, gated, stub), "/admin/tenants/github-oidc", saAdminToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("SA admin with role gate: expected 200, got %d %q", rr.Code, rr.Body.String())
		}
	})

	t.Run("no token unauthorized", func(t *testing.T) {
		rr := doRequestWithPath(t, base.requireSAOrSelfTenant(githubOIDCTenant, readCap, stub), "/admin/tenants/github-oidc", "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("no token: expected 401, got %d", rr.Code)
		}
	})

	t.Run("garbage token unauthorized", func(t *testing.T) {
		rr := doRequestWithPath(t, base.requireSAOrSelfTenant(githubOIDCTenant, readCap, stub), "/admin/tenants/github-oidc", "not-a-jwt")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("garbage token: expected 401, got %d", rr.Code)
		}
	})
}

// TestHandler_AdminSelfTenantRoute exercises the full HTTP routing in Handler().
func TestHandler_AdminSelfTenantRoute(t *testing.T) {
	auth, issuerURL, key := federatedAuthForTest(t)
	ownerToken := mintOIDCToken(t, key, issuerURL, "agent-orca-dev", map[string]any{"actor": "floppyfish14"})

	tc := &agentorcav1alpha1.TenantConfig{
		ObjectMeta: metav1.ObjectMeta{Name: githubOIDCTenant, Namespace: "agent-orca-system"},
		Spec: agentorcav1alpha1.TenantConfigSpec{
			AuthMode:          "federated",
			AllowedNamespaces: []string{defaultNamespace},
			Federated: &agentorcav1alpha1.FederatedAuthConfig{
				IssuerURL:  issuerURL,
				ClientID:   "agent-orca-dev",
				MatchClaim: "actor",
				MatchValue: "floppyfish14",
			},
		},
	}
	cl := ctrlfake.NewClientBuilder().WithScheme(adminScheme(t)).WithObjects(tc).Build()

	srv := &ExternalAPIServer{
		crdClient:     cl,
		auth:          auth,
		reviewSAToken: saAdminReviewer("system:serviceaccount:agent-orca-system:agentorca-admin", saAdminToken),
		isAdminSA:     func(context.Context, string, string) (bool, error) { return true, nil },
	}
	h := srv.Handler()

	// sanity: client is non-nil so getTenant can read the TC.
	if srv.crdClient == nil {
		t.Fatal("crdClient must be set")
	}

	t.Run("federated reads own tenant", func(t *testing.T) {
		rr := doRequestWithPath(t, h, "/admin/tenants/github-oidc", ownerToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 for own tenant, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp AdminTenantResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
		if resp.Name != githubOIDCTenant || firstNamespace(resp.AllowedNamespaces) != defaultNamespace {
			t.Fatalf("bad tenant response: %+v", resp)
		}
		if resp.ClientSecret != "" {
			t.Fatalf("client secret must not be leaked: %+v", resp)
		}
	})

	t.Run("federated other tenant hidden", func(t *testing.T) {
		rr := doRequestWithPath(t, h, "/admin/tenants/other-tenant", ownerToken)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404 to hide other tenant, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("federated cannot rotate own secret", func(t *testing.T) {
		// rotate-secret is not in selfTenantAdminOps today, so it stays SA-only
		// and an OIDC token is rejected with 401 before reaching the handler.
		rr := doRequestWithMethod(t, h, http.MethodPost, "/admin/tenants/github-oidc/rotate-secret", ownerToken)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("rotate must stay SA-only; expected 401 for OIDC, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("federated cannot list all tenants", func(t *testing.T) {
		rr := doRequestWithPath(t, h, "/admin/tenants", ownerToken)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("list must stay SA-only; expected 401 for OIDC, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("no token unauthorized", func(t *testing.T) {
		rr := doRequestWithPath(t, h, "/admin/tenants/github-oidc", "")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", rr.Code)
		}
	})

	t.Run("SA admin token full access", func(t *testing.T) {
		rr := doRequestWithPath(t, h, "/admin/tenants/github-oidc", saAdminToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("SA admin should still work; expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
	})
}
