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
	"errors"
	"testing"

	"github.com/heddles/agent-orca/internal/postgresql"
)

func withIdentity(id *TenantIdentity) context.Context {
	return context.WithValue(context.Background(), tenantIdentityKey, id)
}

func TestPgScope(t *testing.T) {
	tests := []struct {
		name      string
		ctx       context.Context
		wantScope string
		wantOK    bool
	}{
		{"no identity is the platform (auth disabled, UI proxy)", context.Background(), postgresql.PlatformScope, true},
		{"nil identity is the platform", withIdentity(nil), postgresql.PlatformScope, true},
		{"tenant with an ID", withIdentity(&TenantIdentity{Roles: testAdminRoles, TenantID: isoA, TenantName: "acme"}), isoA, true},
		{"empty identity gets no scope", withIdentity(&TenantIdentity{Roles: testAdminRoles}), "", true},
		{"named tenant without an ID gets no scope", withIdentity(&TenantIdentity{Roles: testAdminRoles, TenantName: "acme"}), "", true},
		{"namespace-only identity gets no scope", withIdentity(&TenantIdentity{Roles: testAdminRoles, Namespace: "a-ns"}), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope, ok := postgresql.ScopeFrom(pgScope(tt.ctx))
			if scope != tt.wantScope || ok != tt.wantOK {
				t.Errorf("scope = %q, %v; want %q, %v", scope, ok, tt.wantScope, tt.wantOK)
			}
		})
	}
}

// An identity with no tenant ID is refused by the store even when the store
// defaults to the platform, because pgScope sets an explicitly empty scope.
func TestPgScopeEmptyIdentityIsRefusedByAPlatformStore(t *testing.T) {
	store := (&postgresql.Store{}).AsPlatform()
	_, err := store.GetRun(pgScope(withIdentity(&TenantIdentity{Roles: testAdminRoles, TenantName: "legacy"})), "ns", "run")
	if !errors.Is(err, postgresql.ErrNoTenantScope) {
		t.Errorf("GetRun err = %v, want ErrNoTenantScope", err)
	}
}

// A Kubernetes ServiceAccount token resolves to its tenant's ID and so lands in
// that tenant's Postgres scope, not the platform's.
func TestServiceAccountTokenFlowsIntoATenantScope(t *testing.T) {
	a := saAuth(t, "system:serviceaccount:a-ns:ci", idTenant("acme", isoA, "a-ns"))
	id, err := a.validateK8sToken(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	scope, ok := postgresql.ScopeFrom(pgScope(withIdentity(id)))
	if !ok || scope != isoA {
		t.Errorf("service account scope = %q, %v; want %q", scope, ok, isoA)
	}
}
