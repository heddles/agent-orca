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
	"fmt"
	"sync"
	"testing"
)

func rev(action, name, obj string) Revision {
	return Revision{Action: action, Kind: "Agent", Namespace: "shared", Name: name, UserID: "ada", Summary: "s", ResourceVersion: "7", Object: []byte(obj)}
}

//nolint:gocyclo // one scenario on one database
func TestRevisionsAgainstPostgres(t *testing.T) {
	s, raw := appStore(t)
	ctx := context.Background()
	a, b, plat := WithTenant(ctx, tenantA), WithTenant(ctx, tenantB), WithPlatform(ctx)
	q := RevisionQuery{Kind: "Agent", Namespace: "shared", Name: "bot"}

	// Numbers count up per object; a no-op save stores nothing.
	r1, stored, err := s.AppendRevision(a, rev("create", "bot", `{"spec":{"v":1}}`), 0)
	if err != nil || !stored || r1.Revision != 1 || r1.TenantID != tenantA || r1.CreatedAt.IsZero() {
		t.Fatalf("first = %+v stored=%v err=%v", r1, stored, err)
	}
	again, stored, err := s.AppendRevision(a, rev("update", "bot", `{ "spec": {"v": 1} }`), 0)
	if err != nil || stored || again.Revision != 1 {
		t.Errorf("a save with the same content must not add a revision: %+v stored=%v err=%v", again, stored, err)
	}
	if r2, stored, _ := s.AppendRevision(a, rev("update", "bot", `{"spec":{"v":2}}`), 0); !stored || r2.Revision != 2 {
		t.Errorf("second = %+v", r2)
	}
	if o, _, _ := s.AppendRevision(a, rev("create", "other", `{"spec":{"v":1}}`), 0); o.Revision != 1 {
		t.Errorf("another object starts at 1, got %d", o.Revision)
	}
	// A delete is always stored, and a create after it with the same content too.
	if d, stored, _ := s.AppendRevision(a, rev("delete", "bot", `{"spec":{"v":2}}`), 0); !stored || d.Revision != 3 {
		t.Errorf("delete = %+v stored=%v", d, stored)
	}
	if c, stored, _ := s.AppendRevision(a, rev("create", "bot", `{"spec":{"v":2}}`), 0); !stored || c.Revision != 4 {
		t.Errorf("recreate = %+v stored=%v", c, stored)
	}
	restored := rev("restore", "bot", `{"spec":{"v":1}}`)
	n := 1
	restored.RestoredFrom = &n
	if r5, _, _ := s.AppendRevision(a, restored, 0); r5.Revision != 5 || r5.RestoredFrom == nil || *r5.RestoredFrom != 1 {
		t.Errorf("restore = %+v", r5)
	}

	// Tenant B has its own object of the same name, numbered from 1.
	if rb, _, err := s.AppendRevision(b, rev("create", "bot", `{"spec":{"b":true}}`), 0); err != nil || rb.Revision != 1 {
		t.Fatalf("B's revision = %+v, %v", rb, err)
	}

	// Listing: newest first, paged by number, never another tenant's.
	list, err := s.ListRevisions(a, q)
	if err != nil || len(list) != 5 || list[0].Revision != 5 || list[4].Revision != 1 || list[0].Object != nil {
		t.Fatalf("A lists %+v, %v", list, err)
	}
	if got, _ := s.ListRevisions(a, RevisionQuery{Kind: "Agent", Namespace: "shared", Name: "bot", TenantID: tenantB}); len(got) != 5 {
		t.Errorf("a tenant scope ignores the tenant asked for, got %d rows", len(got))
	}
	if got, _ := s.ListRevisions(a, RevisionQuery{Kind: "Agent", Namespace: "shared", Name: "bot", Limit: 2, Before: 4}); len(got) != 2 || got[0].Revision != 3 {
		t.Errorf("page = %+v", got)
	}
	if got, _ := s.ListRevisions(b, q); len(got) != 1 {
		t.Errorf("B lists %d, want 1", len(got))
	}
	if got, _ := s.ListRevisions(plat, RevisionQuery{Kind: "Agent", Namespace: "shared", Name: "bot", TenantID: tenantB}); len(got) != 1 {
		t.Errorf("platform filtered to B lists %d, want 1", len(got))
	}

	// Detail and cross-tenant reads.
	d, err := s.GetRevision(a, q, 2)
	if err != nil || string(d.Object) != `{"spec": {"v": 2}}` || d.Action != "update" {
		t.Fatalf("detail = %+v, %v", d, err)
	}
	if _, err := s.GetRevision(b, q, 2); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("B reads A's revision 2: err = %v", err)
	}
	if _, err := s.GetRevision(a, q, 99); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("missing revision err = %v", err)
	}

	// Writes: not for another tenant, not without a scope, not invalid.
	if _, _, err := s.AppendRevision(a, Revision{TenantID: tenantB, Action: "create", Kind: "Agent", Name: "x", Object: []byte(`{}`)}, 0); err == nil {
		t.Error("A wrote a revision for B")
	}
	if _, _, err := s.AppendRevision(ctx, rev("create", "x", `{}`), 0); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("unscoped append err = %v", err)
	}
	if _, _, err := s.AppendRevision(a, rev("", "x", `{}`), 0); !errors.Is(err, ErrInvalidRevision) {
		t.Errorf("append without action err = %v", err)
	}
	big := Revision{Action: "create", Kind: "Agent", Name: "x", Object: make([]byte, MaxRevisionObjectBytes+1)}
	if _, _, err := s.AppendRevision(a, big, 0); !errors.Is(err, ErrInvalidRevision) {
		t.Errorf("oversized object err = %v", err)
	}

	// The database itself enforces it, below the store's own filters.
	var cnt int
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM revisions`).Scan(&cnt); err != nil || cnt != 0 {
		t.Errorf("unset setting returned %d rows, %v; want 0", cnt, err)
	}
	err = s.scoped(b, func(tx dbtx, _ string) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM revisions`).Scan(&cnt)
	})
	if err != nil || cnt != 1 {
		t.Errorf("unfiltered query under B saw %d rows, %v; want 1", cnt, err)
	}
	err = s.scoped(a, func(tx dbtx, _ string) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO revisions (tenant_id, kind, namespace, name, revision, action, object) VALUES ($1, 'Agent', 's', 'x', 1, 'create', '{}')`, tenantB)
		return err
	})
	if err == nil {
		t.Error("RLS let A insert a row for B")
	}
	err = s.scoped(a, func(tx dbtx, _ string) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM revisions WHERE tenant_id = $1`, tenantB)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 0 {
			return fmt.Errorf("A deleted %d of B's rows", n)
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}
}

func TestRevisionRetentionAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	a := WithTenant(context.Background(), tenantA)
	q := RevisionQuery{Kind: "Agent", Namespace: "shared", Name: "bot"}
	for i := 1; i <= 7; i++ {
		if _, _, err := s.AppendRevision(a, rev("update", "bot", fmt.Sprintf(`{"v":%d}`, i)), 3); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListRevisions(a, q)
	if err != nil || len(list) != 3 || list[0].Revision != 7 || list[2].Revision != 5 {
		t.Fatalf("after pruning to 3: %+v, %v", list, err)
	}
	// Numbers keep counting after a prune.
	if r, _, _ := s.AppendRevision(a, rev("update", "bot", `{"v":8}`), 3); r.Revision != 8 {
		t.Errorf("next number = %d, want 8", r.Revision)
	}
	// A delete snapshot is kept like any other and counts toward the limit.
	if _, _, err := s.AppendRevision(a, rev("delete", "bot", `{"v":8}`), 3); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.ListRevisions(a, q); len(list) != 3 || list[0].Action != "delete" || list[0].Revision != 9 {
		t.Errorf("after delete: %+v", list)
	}
	// keep of 1 leaves exactly the newest, which is the live content.
	if _, _, err := s.AppendRevision(a, rev("create", "bot", `{"v":1}`), 1); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.ListRevisions(a, q); len(list) != 1 || list[0].Revision != 10 {
		t.Errorf("keep=1: %+v", list)
	}
}

func TestRevisionNumbersUnderConcurrentWriters(t *testing.T) {
	s, _ := appStore(t)
	a := WithTenant(context.Background(), tenantA)
	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Go(func() {
			_, _, err := s.AppendRevision(a, rev("update", "bot", fmt.Sprintf(`{"v":%d}`, i)), 0)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListRevisions(a, RevisionQuery{Kind: "Agent", Namespace: "shared", Name: "bot"})
	if err != nil || len(list) != writers {
		t.Fatalf("got %d revisions, %v; want %d", len(list), err, writers)
	}
	for i, r := range list {
		if r.Revision != writers-i {
			t.Fatalf("numbers are not 1..%d without gaps or repeats: %v", writers, list)
		}
	}
}
