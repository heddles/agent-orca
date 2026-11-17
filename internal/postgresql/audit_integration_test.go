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
)

func TestAuditEventsAgainstPostgres(t *testing.T) {
	s, raw := appStore(t)
	ctx := context.Background()
	a, b, plat := WithTenant(ctx, tenantA), WithTenant(ctx, tenantB), WithPlatform(ctx)

	ev := func(c context.Context, name string) AuditEvent {
		e, err := s.AppendAudit(c, AuditEvent{UserID: "ada", Action: AuditCreate, Kind: "Agent", Namespace: "shared", Name: name, ResourceVersion: "7", Summary: "s"})
		if err != nil {
			t.Fatalf("append %s: %v", name, err)
		}
		return e
	}
	a1, a2, b1 := ev(a, "a-1"), ev(a, "a-2"), ev(b, "b-1")
	if a1.TenantID != tenantA || b1.TenantID != tenantB || a1.ID == 0 || a1.CreatedAt.IsZero() {
		t.Fatalf("stamped events = %+v, %+v", a1, b1)
	}

	// Each tenant sees only its own, newest first, even when it asks for another's.
	got, err := s.ListAudit(a, AuditQuery{TenantID: tenantB})
	if err != nil || len(got) != 2 || got[0].ID != a2.ID || got[1].ID != a1.ID {
		t.Fatalf("A lists %+v, %v", got, err)
	}
	if got, _ = s.ListAudit(b, AuditQuery{}); len(got) != 1 || got[0].ID != b1.ID {
		t.Errorf("B lists %+v", got)
	}
	// Keyset pagination.
	if got, _ = s.ListAudit(a, AuditQuery{Limit: 1}); len(got) != 1 || got[0].ID != a2.ID {
		t.Fatalf("page 1 = %+v", got)
	}
	if got, _ = s.ListAudit(a, AuditQuery{Limit: 1, Before: a2.ID}); len(got) != 1 || got[0].ID != a1.ID {
		t.Errorf("page 2 = %+v", got)
	}
	if got, _ = s.ListAudit(plat, AuditQuery{}); len(got) != 3 {
		t.Errorf("platform sees %d, want 3", len(got))
	}
	if got, _ = s.ListAudit(plat, AuditQuery{TenantID: tenantB}); len(got) != 1 {
		t.Errorf("platform filtered to B sees %d, want 1", len(got))
	}

	// A call for another tenant's row is refused, as is a call with no scope.
	if _, err := s.AppendAudit(a, AuditEvent{TenantID: tenantB, Action: AuditCreate, Kind: "Agent"}); err == nil {
		t.Error("A wrote an event for B")
	}
	if _, err := s.AppendAudit(ctx, AuditEvent{Action: AuditCreate, Kind: "Agent"}); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("unscoped append err = %v", err)
	}
	if _, err := s.AppendAudit(a, AuditEvent{Kind: "Agent"}); !errors.Is(err, ErrInvalidAudit) {
		t.Errorf("append without action err = %v", err)
	}

	// The database itself enforces it: no setting, no rows; a raw query under A
	// sees only A's.
	var n int
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM audit_events`).Scan(&n); err != nil || n != 0 {
		t.Errorf("unset setting returned %d rows, %v; want 0", n, err)
	}
	err = s.scoped(a, func(tx dbtx, _ string) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM audit_events`).Scan(&n)
	})
	if err != nil || n != 2 {
		t.Errorf("unfiltered query under A saw %d rows, %v; want 2", n, err)
	}
	err = s.scoped(a, func(tx dbtx, _ string) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO audit_events (tenant_id, action, kind) VALUES ($1, 'create', 'Agent')`, tenantB)
		return err
	})
	if err == nil {
		t.Error("RLS let A insert a row for B")
	}
	err = s.scoped(a, func(tx dbtx, _ string) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM audit_events WHERE id = $1`, b1.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ = s.ListAudit(b, AuditQuery{}); len(got) != 1 {
		t.Error("A deleted B's event")
	}

	// A platform-scoped append with no tenant is unattributed: only the platform sees it.
	if _, err := s.AppendAudit(plat, AuditEvent{Action: AuditUpdate, Kind: "Agent"}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.ListAudit(a, AuditQuery{}); len(got) != 2 {
		t.Errorf("A sees the unattributed row: %+v", got)
	}
}
