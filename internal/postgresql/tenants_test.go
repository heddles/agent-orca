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
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/heddles/agent-orca/internal/tenancy"
)

const testTenantID = "tnt_01935c7e-1111-7aaa-8bbb-000000000001"

func TestTenantsMigrationCreatesRegistry(t *testing.T) {
	all, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, m := range all {
		if strings.Contains(m.Name, "tenants") {
			sql = m.SQL
		}
	}
	for _, col := range []string{"id ", "name ", "k8s_namespace ", "k8s_uid ", "state ", "created_at ", "deleted_at "} {
		if !strings.Contains(sql, col) {
			t.Errorf("tenants migration lacks column %q", col)
		}
	}
	if !strings.Contains(sql, "'active', 'suspended', 'deleted'") {
		t.Error("tenants migration does not constrain state")
	}
}

func TestGetTenant(t *testing.T) {
	s, mock := costMock(t)
	created := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cols := []string{"id", "name", "k8s_namespace", "k8s_uid", "state", "created_at", "deleted_at"}
	mock.ExpectQuery(`FROM tenants WHERE id = \$1`).WithArgs(testTenantID).
		WillReturnRows(sqlmock.NewRows(cols).AddRow(testTenantID, "acme", "acme-ns", "uid-1", "deleted", created, created))

	rec, err := s.GetTenant(context.Background(), testTenantID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != tenancy.StateDeleted || rec.DeletedAt == nil || rec.Name != "acme" || rec.Namespace != "acme-ns" || rec.UID != "uid-1" {
		t.Errorf("record = %+v", rec)
	}

	mock.ExpectQuery(`FROM tenants WHERE id = \$1`).WithArgs(testTenantID).WillReturnRows(sqlmock.NewRows(cols))
	if rec, err := s.GetTenant(context.Background(), testTenantID); rec != nil || err != nil {
		t.Errorf("missing tenant = %+v, %v; want nil, nil", rec, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestPutTenant_BindsEveryColumn(t *testing.T) {
	s, mock := costMock(t)
	created := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(`INSERT INTO tenants .* ON CONFLICT \(id\) DO UPDATE`).
		WithArgs(testTenantID, "acme", "acme-ns", "uid-1", "active", created, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := s.PutTenant(context.Background(), tenancy.Record{
		ID: testTenantID, Name: "acme", Namespace: "acme-ns", UID: "uid-1", State: tenancy.StateActive, CreatedAt: created,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestLiveTenantsByName(t *testing.T) {
	s, mock := costMock(t)
	cols := []string{"id", "name", "k8s_namespace", "k8s_uid", "state", "created_at", "deleted_at"}
	mock.ExpectQuery(`FROM tenants\s+WHERE k8s_namespace = \$1 AND name = \$2 AND state <> 'deleted'`).WithArgs("acme-ns", "acme").
		WillReturnRows(sqlmock.NewRows(cols).AddRow(testTenantID, "acme", "acme-ns", "uid-1", "active", time.Now(), nil))

	got, err := s.LiveTenantsByName(context.Background(), "acme-ns", "acme")
	if err != nil || len(got) != 1 || got[0].ID != testTenantID || got[0].DeletedAt != nil {
		t.Fatalf("LiveTenantsByName = %+v, %v", got, err)
	}
}
