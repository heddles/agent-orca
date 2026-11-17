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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/tenancy"
)

const (
	tidOld = "tnt_01935c7e-1111-7aaa-8bbb-000000000001"
	tidNew = "tnt_01935c7e-2222-7aaa-8bbb-000000000002"
	tidOth = "tnt_01935c7e-3333-7aaa-8bbb-000000000003"
)

func tenantWithID(name, tid string) *agentorcav1alpha1.TenantConfig {
	tc := loginTenant(name, "", "")
	tc.Status.TenantID = tid
	return tc
}

func registryWith(recs ...tenancy.Record) *tenancy.MemoryRegistry {
	reg := tenancy.NewMemoryRegistry()
	for _, r := range recs {
		_ = reg.PutTenant(context.Background(), r)
	}
	return reg
}

func rec(id, name string, state tenancy.State) tenancy.Record {
	return tenancy.Record{ID: id, Name: name, Namespace: "agent-orca-system", State: state}
}

// issuedToken mints a session token the way the OIDC callback does.
func issuedToken(t *testing.T, a *ExternalAuth, tid string) string {
	t.Helper()
	tok, err := a.MintSessionForIdentity(&TenantIdentity{Roles: testAdminRoles,
		TenantID: tid, TenantName: "acme", Namespace: "default", Namespaces: []string{"default"}, UserID: "ada",
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestCheckTenant(t *testing.T) {
	tests := []struct {
		name        string
		tid, tenant string
		recs        []tenancy.Record
		noRegistry  bool
		status      string // TenantConfig.status.tenantID when there is no registry
		rejectLeg   bool
		wantErr     error
	}{
		{name: "active tenant", tid: tidOld, tenant: "acme", recs: []tenancy.Record{rec(tidOld, "acme", tenancy.StateActive)}},
		{name: "unknown tenant", tid: tidOld, tenant: "acme", wantErr: errTenantInactive},
		{name: "suspended tenant", tid: tidOld, tenant: "acme", recs: []tenancy.Record{rec(tidOld, "acme", tenancy.StateSuspended)}, wantErr: errTenantInactive},
		{name: "deleted tenant", tid: tidOld, tenant: "acme", recs: []tenancy.Record{rec(tidOld, "acme", tenancy.StateDeleted)}, wantErr: errTenantInactive},
		{name: "tid of a different tenant's name", tid: tidOld, tenant: "acme", recs: []tenancy.Record{rec(tidOld, "globex", tenancy.StateActive)}, wantErr: errTenantInactive},
		{name: "legacy token allowed", tenant: "acme"},
		{name: "legacy token refused", tenant: "acme", rejectLeg: true, wantErr: errLegacyTokenRefused},
		{name: "no registry: matches the status mirror", tid: tidOld, tenant: "acme", noRegistry: true, status: tidOld},
		{name: "no registry: stale tid after a recreate", tid: tidOld, tenant: "acme", noRegistry: true, status: tidNew, wantErr: errTenantInactive},
		{name: "no registry: tenant not yet registered", tid: tidOld, tenant: "acme", noRegistry: true, wantErr: errTenantInactive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := testAuth(t, testRSAKey(t), tenantWithID(tt.tenant, tt.status))
			if !tt.noRegistry {
				a.SetTenantRegistry(registryWith(tt.recs...))
			}
			a.SetAllowLegacyTokens(!tt.rejectLeg)
			err := a.checkTenant(tt.tid, tt.tenant)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("checkTenant = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// A TenantConfig recreated under the same name gets a new ID, so a session
// minted for the first one must stop working while the new one's works.
func TestIssuedTokenFromBeforeSameNameRecreateIsRejected(t *testing.T) {
	tc := tenantWithID("acme", tidNew)
	a := testAuth(t, testRSAKey(t), tc)
	a.SetTenantRegistry(registryWith(
		rec(tidOld, "acme", tenancy.StateDeleted),
		rec(tidNew, "acme", tenancy.StateActive),
	))

	if _, err := a.validateIssuedToken(issuedToken(t, a, tidOld)); !errors.Is(err, errTenantInactive) {
		t.Errorf("old session: err = %v, want errTenantInactive", err)
	}
	ident, err := a.validateIssuedToken(issuedToken(t, a, tidNew))
	if err != nil {
		t.Fatalf("new session rejected: %v", err)
	}
	if ident.TenantID != tidNew || ident.TenantName != "acme" { //nolint:goconst
		t.Errorf("identity = %+v", ident)
	}
}

func TestIssuedTokenLegacyHandling(t *testing.T) {
	tc := tenantWithID("acme", tidNew)
	a := testAuth(t, testRSAKey(t), tc)
	a.SetTenantRegistry(registryWith(rec(tidNew, "acme", tenancy.StateActive)))
	legacy := issuedToken(t, a, "") // no tid claim

	ident, err := a.validateIssuedToken(legacy)
	if err != nil {
		t.Fatalf("legacy token refused by default: %v", err)
	}
	if ident.TenantID != tidNew {
		t.Errorf("legacy identity TenantID = %q, want the tenant's current %q", ident.TenantID, tidNew)
	}

	a.SetAllowLegacyTokens(false)
	if _, err := a.validateIssuedToken(legacy); !errors.Is(err, errLegacyTokenRefused) {
		t.Errorf("with legacy disabled: err = %v, want errLegacyTokenRefused", err)
	}
}

func TestSessionTokensCarryTid(t *testing.T) {
	a := testAuth(t, testRSAKey(t), tenantWithID("acme", tidNew))
	tok := issuedToken(t, a, tidNew)
	parsed, _, err := jwt.NewParser().ParseUnverified(tok, jwt.MapClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Claims.(jwt.MapClaims)["tid"]; got != tidNew {
		t.Errorf("tid claim = %v, want %s", got, tidNew)
	}
}

func TestClientCredentialsTokenCarriesTid(t *testing.T) {
	tc := tenantWithID("acme", tidNew)
	tc.Spec.AuthMode = "issued"
	tc.Spec.Issued = &agentorcav1alpha1.IssuedAuthConfig{
		ClientID:        "acme-client",
		ClientSecretRef: agentorcav1alpha1.SecretKeyRef{Name: "acme-secret", Key: "secret"},
	}
	a := testAuth(t, testRSAKey(t), tc)
	a.tenantCache["issued:acme-client"] = tc
	a.k8s = k8sfake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-secret", Namespace: tc.Namespace},
		Data:       map[string][]byte{"secret": []byte("s3cret")},
	})
	a.SetTenantRegistry(registryWith(rec(tidNew, "acme", tenancy.StateActive)))

	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {"acme-client"}, "client_secret": {"s3cret"}}
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.HandleTokenRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var resp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	ident, err := a.validateIssuedToken(resp.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if ident.TenantID != tidNew {
		t.Errorf("TenantID = %q, want %q", ident.TenantID, tidNew)
	}
}

func TestMiddlewareReturns401ForInactiveTenant(t *testing.T) {
	a := testAuth(t, testRSAKey(t), tenantWithID("acme", tidNew))
	reg := registryWith(rec(tidNew, "acme", tenancy.StateActive))
	a.SetTenantRegistry(reg)
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	tok := issuedToken(t, a, tidNew)

	call := func() int {
		req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if got := call(); got != http.StatusOK {
		t.Fatalf("active tenant: status = %d", got)
	}

	// Suspending ends the session once the cache is dropped (RefreshTenants does
	// this on the replica that saw the change; the TTL covers the others).
	suspended := rec(tidNew, "acme", tenancy.StateSuspended)
	_ = reg.PutTenant(context.Background(), suspended)
	if got := call(); got != http.StatusOK {
		t.Fatalf("within the TTL the cached answer applies: status = %d", got)
	}
	a.resetTenantCache()
	if got := call(); got != http.StatusUnauthorized {
		t.Errorf("suspended tenant: status = %d, want 401", got)
	}
}

type countingRegistry struct {
	tenancy.Registry
	gets atomic.Int32
}

func (c *countingRegistry) GetTenant(ctx context.Context, id string) (*tenancy.Record, error) {
	c.gets.Add(1)
	return c.Registry.GetTenant(ctx, id)
}

func TestTenantResolutionIsCachedForTheTTL(t *testing.T) {
	reg := &countingRegistry{Registry: registryWith(rec(tidNew, "acme", tenancy.StateActive))}
	a := testAuth(t, testRSAKey(t), tenantWithID("acme", tidNew))
	a.SetTenantRegistry(reg)

	for range 5 {
		if err := a.checkTenant(tidNew, "acme"); err != nil {
			t.Fatal(err)
		}
	}
	if got := reg.gets.Load(); got != 1 {
		t.Errorf("registry reads = %d, want 1 within the TTL", got)
	}

	a.tidMu.Lock()
	e := a.tidCache[tidNew]
	e.expires = time.Now().Add(-time.Second)
	a.tidCache[tidNew] = e
	a.tidMu.Unlock()
	_ = a.checkTenant(tidNew, "acme")
	if got := reg.gets.Load(); got != 2 {
		t.Errorf("registry reads = %d, want a re-read after expiry", got)
	}

	// Unknown tenants are cached too, so a flood of forged tids is one read each.
	for range 3 {
		_ = a.checkTenant(tidOth, "acme")
	}
	if got := reg.gets.Load(); got != 3 {
		t.Errorf("registry reads = %d, want 1 more for the unknown tid", got)
	}
}

type failingRegistry struct{ tenancy.Registry }

func (failingRegistry) GetTenant(context.Context, string) (*tenancy.Record, error) {
	return nil, errors.New("postgres down")
}

func TestRegistryOutageFailsClosed(t *testing.T) {
	a := testAuth(t, testRSAKey(t), tenantWithID("acme", tidNew))
	a.SetTenantRegistry(failingRegistry{})
	if _, err := a.validateIssuedToken(issuedToken(t, a, tidNew)); err == nil {
		t.Error("token accepted while the registry is unreachable")
	}
}

func TestFederatedIdentityGetsTenantIDFromTheTenantConfig(t *testing.T) {
	key := newTestKey(t)
	issuerURL := githubIssuerURL
	startOIDCIssuer(t, key, &issuerURL)
	token := mintOIDCToken(t, key, issuerURL, "agent-orca-dev", map[string]any{"actor": "floppyfish14"})

	tests := []struct {
		name    string
		state   tenancy.State
		wantErr bool
	}{
		{"active", tenancy.StateActive, false},
		{"deleted", tenancy.StateDeleted, true},
		{"suspended", tenancy.StateSuspended, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := seedFederatedTenant(t, issuerURL)
			for _, tc := range a.tenantCache {
				tc.Status.TenantID = tidNew
			}
			a.SetTenantRegistry(registryWith(rec(tidNew, githubOIDCTenant, tt.state)))
			ident, err := a.validateFederatedToken(context.Background(), token)
			if tt.wantErr {
				if !errors.Is(err, errTenantInactive) {
					t.Errorf("err = %v, want errTenantInactive", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ident.TenantID != tidNew {
				t.Errorf("TenantID = %q, want %q", ident.TenantID, tidNew)
			}
		})
	}
}
