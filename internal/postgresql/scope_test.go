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

package postgresql

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	tenantA = "tnt_01935c7e-0000-7000-8000-00000000000a"
	tenantB = "tnt_01935c7e-0000-7000-8000-00000000000b"
)

// expectScopeBegin expects the transaction and RLS settings every scoped call opens.
func expectScopeBegin(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectExec(`set_config\('agentorca\.tenant_id'`).WillReturnResult(sqlmock.NewResult(0, 0))
}

func TestScopeResolution(t *testing.T) {
	strict := &Store{}
	bg := context.Background()
	if _, err := strict.scopeOf(bg); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("no scope: err = %v, want ErrNoTenantScope", err)
	}
	if _, err := strict.scopeOf(WithTenant(bg, "")); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("empty tenant: err = %v", err)
	}
	if _, err := strict.scopeOf(WithTenant(bg, "acme")); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("malformed tenant: err = %v", err)
	}
	if got, err := strict.scopeOf(WithTenant(bg, tenantA)); err != nil || got != tenantA {
		t.Errorf("tenant = %q, %v", got, err)
	}
	if got, _ := strict.scopeOf(WithPlatform(bg)); got != PlatformScope {
		t.Errorf("platform = %q", got)
	}
	plat := strict.AsPlatform()
	if got, _ := plat.scopeOf(bg); got != PlatformScope {
		t.Errorf("platform store default = %q", got)
	}
	if got, _ := plat.scopeOf(WithTenant(bg, tenantB)); got != tenantB {
		t.Errorf("a ctx tenant must narrow a platform store, got %q", got)
	}
	if _, err := plat.scopeOf(WithTenant(bg, "bad")); err == nil {
		t.Error("a malformed tenant must not widen to platform")
	}
}

func TestStoreWithoutScopeNeverTouchesTheDatabase(t *testing.T) {
	s, mock := costMock(t)
	s.defaultScope = ""
	if _, err := s.ListApprovals(context.Background(), ApprovalQuery{}); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRowTenant(t *testing.T) {
	if got, err := rowTenant(tenantA, ""); err != nil || got != tenantA {
		t.Errorf("fills from scope: %q, %v", got, err)
	}
	if _, err := rowTenant(tenantA, tenantB); err == nil {
		t.Error("a row for another tenant was accepted")
	}
	if got, _ := rowTenant(PlatformScope, tenantB); got != tenantB {
		t.Errorf("platform keeps the row tenant, got %q", got)
	}
	if got, _ := rowTenant(PlatformScope, ""); got != "" {
		t.Errorf("platform unattributed, got %q", got)
	}
}

// A tenant-scoped call is limited to its own tenant even when the query names
// another: the other tenant's ID never reaches the database.
func TestListApprovalsFiltersByTenantScope(t *testing.T) {
	s, mock := costMock(t)
	s.defaultScope = ""
	expectScopeBegin(mock)
	mock.ExpectQuery(`FROM approvals WHERE tenant_id = \$1 AND namespace = \$2`).WithArgs(tenantA, "ns", 100).
		WillReturnRows(sqlmock.NewRows(approvalCols()))
	mock.ExpectCommit()
	if _, err := s.ListApprovals(WithTenant(context.Background(), tenantA), ApprovalQuery{Namespace: "ns", TenantID: tenantB}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func approvalCols() []string {
	return []string{"id", "tenant", "tenant_id", "namespace", "run_name", "kind", "source_key", "summary", "details",
		"approver_groups", "status", "requested_at", "expires_at", "decided_by", "decided_at", "reason", "applied_at"}
}
