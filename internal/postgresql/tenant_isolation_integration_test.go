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
	"database/sql"
	"net/url"
	"os"
	"testing"
	"time"
)

// appStore migrates a scratch database and returns a Store connected as a
// plain (non-superuser) role. Superusers bypass row-level security, so testing
// the policies as the DSN's own user would prove nothing.
func appStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("AGENTORCA_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("AGENTORCA_TEST_POSTGRES_DSN not set")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, admin); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP ROLE IF EXISTS ao_rls_test`,
		`CREATE ROLE ao_rls_test LOGIN PASSWORD 'rls' NOSUPERUSER NOBYPASSRLS`,
		`GRANT USAGE ON SCHEMA public TO ao_rls_test`,
		`GRANT ALL ON ALL TABLES IN SCHEMA public TO ao_rls_test`,
		`GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO ao_rls_test`,
	} {
		if _, err := admin.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("ao_rls_test", "rls")
	app, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return &Store{db: app}, app
}

func TestTenantIsolationAgainstPostgres(t *testing.T) {
	s, raw := appStore(t)
	ctx := context.Background()
	a, b := WithTenant(ctx, tenantA), WithTenant(ctx, tenantB)
	plat := WithPlatform(ctx)

	mk := func(c context.Context, src string) Approval {
		ap, err := s.RequestApproval(c, Approval{Namespace: "shared", RunName: "r", Kind: ApprovalKindEgress,
			SourceKey: src, Summary: "s", ExpiresAt: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatalf("request %s: %v", src, err)
		}
		return ap
	}
	apA, apB := mk(a, "a-1"), mk(b, "b-1")
	if apA.TenantID != tenantA || apB.TenantID != tenantB {
		t.Fatalf("tenant ids = %q, %q", apA.TenantID, apB.TenantID)
	}

	// Two tenants share a namespace; each sees only its own row.
	list, err := s.ListApprovals(a, ApprovalQuery{Namespace: "shared"})
	if err != nil || len(list) != 1 || list[0].ID != apA.ID {
		t.Fatalf("A lists %+v, %v", list, err)
	}
	// Even naming B in the query does not let A read B's rows.
	list, err = s.ListApprovals(a, ApprovalQuery{TenantID: tenantB})
	if err != nil || len(list) != 1 || list[0].ID != apA.ID {
		t.Fatalf("A listing tenant B got %+v, %v", list, err)
	}
	if _, err := s.GetApproval(a, apB.ID); err != sql.ErrNoRows {
		t.Errorf("A reads B's approval: err = %v", err)
	}
	if _, err := s.DecideApproval(a, apB.ID, ApprovalApproved, "ada", ""); err == nil {
		t.Error("A decided B's approval")
	}
	if list, _ = s.ListApprovals(plat, ApprovalQuery{}); len(list) != 2 {
		t.Errorf("platform sees %d approvals, want 2", len(list))
	}
	if list, _ = s.ListApprovals(plat, ApprovalQuery{TenantID: tenantB}); len(list) != 1 || list[0].ID != apB.ID {
		t.Errorf("platform filtered to B = %+v", list)
	}

	checkUnsetScopeSeesNothing(a, t, s, raw)
	checkOtherTables(a, b, plat, t, s)
}

func checkUnsetScopeSeesNothing(a context.Context, t *testing.T, s *Store, raw *sql.DB) {
	t.Helper()
	ctx := context.Background()
	// No scope: the store refuses; raw SQL with no setting returns nothing.
	if _, err := s.ListApprovals(ctx, ApprovalQuery{}); err == nil {
		t.Error("a call without a scope listed approvals")
	}
	var n int
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM approvals`).Scan(&n); err != nil || n != 0 {
		t.Errorf("unset setting returned %d rows, %v; want 0", n, err)
	}
	// An empty setting (left over by an earlier transaction) is no better.
	if err := s.inScopeValue(ctx, PlatformScope, func(tx dbtx, _ string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM approvals`).Scan(&n); err != nil || n != 0 {
		t.Errorf("empty setting returned %d rows, %v; want 0", n, err)
	}
	// The database refuses a row for another tenant even if the store did not.
	err := s.scoped(a, func(tx dbtx, _ string) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO approvals (id, tenant_id, namespace, run_name, kind, source_key, summary, expires_at)
			VALUES ('x', $1, 'n', 'r', 'egress', 'k', 's', now())`, tenantB)
		return err
	})
	if err == nil {
		t.Error("RLS let A write a row for B")
	}
	// A query with no tenant_id filter at all still only sees the scope's rows.
	err = s.scoped(a, func(tx dbtx, _ string) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM approvals`).Scan(&n)
	})
	if err != nil || n != 1 {
		t.Errorf("unfiltered query under A saw %d rows, %v; want 1", n, err)
	}
}

func checkOtherTables(a, b, plat context.Context, t *testing.T, s *Store) {
	t.Helper()
	// Feedback, routing events, shadow results and leases isolate the same way.
	fb := Feedback{Namespace: "shared", RunName: "r", Agent: "x", Rating: 1, UserID: "ada", Source: FeedbackSourceUI}
	if err := s.UpsertFeedback(a, fb); err != nil {
		t.Fatal(err)
	}
	if got, err := s.FeedbackForRun(b, "shared", "r", ""); err != nil || len(got) != 0 {
		t.Errorf("B reads A's feedback: %+v, %v", got, err)
	}
	if got, err := s.FeedbackForRun(a, "shared", "r", ""); err != nil || len(got) != 1 {
		t.Errorf("A reads its feedback: %+v, %v", got, err)
	}
	ev := RoutingEvent{DecisionID: "d-a", Namespace: "shared", RunName: "r", Provider: "p"}
	if err := s.UpsertRoutingEvents(a, []RoutingEvent{ev}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.RoutingEventsForRun(b, "shared", "r", 0); len(got) != 0 {
		t.Errorf("B reads A's routing events: %+v", got)
	}
	if got, _ := s.RoutingEventsForRun(a, "shared", "r", 0); len(got) != 1 {
		t.Errorf("A reads its routing events: %+v", got)
	}
	// A tenant cannot prune; only the platform can.
	if _, err := s.PruneRoutingEvents(a, time.Now().Add(time.Hour)); err == nil {
		t.Error("a tenant pruned routing events")
	}
	if n, err := s.PruneRoutingEvents(plat, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Errorf("platform pruned %d, %v", n, err)
	}
	now := time.Now()
	lease := LeaseRequest{Namespace: "shared", Benchmark: "bench", Agent: "x", SourceKind: SourceKindRun, SourceName: "r",
		AmountUSD: 0.1, DayStart: now.Truncate(24 * time.Hour), PerDayUSD: 1, TTL: time.Minute, Now: now}
	if _, err := s.TakeShadowLease(a, lease); err != nil {
		t.Fatal(err)
	}
	if sp, err := s.ShadowSpendSince(b, "shared", "bench", now.Add(-time.Hour), now); err != nil || sp.LeasedUSD != 0 {
		t.Errorf("B sees A's lease: %+v, %v", sp, err)
	}
	if sp, err := s.ShadowSpendSince(a, "shared", "bench", now.Add(-time.Hour), now); err != nil || sp.LeasedUSD != 0.1 {
		t.Errorf("A sees its lease: %+v, %v", sp, err)
	}
}

func TestBackfillTenantIDsAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	ctx := context.Background()
	plat := WithPlatform(ctx)
	// Unattributed rows from before the migration: platform-written with no tenant.
	for _, ns := range []string{"team-a", "orphan"} {
		if _, err := s.RequestApproval(plat, Approval{Namespace: ns, RunName: "r", Kind: ApprovalKindEgress,
			SourceKey: ns, Summary: "s", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	// Hidden from tenants until attributed.
	if list, _ := s.ListApprovals(WithTenant(ctx, tenantA), ApprovalQuery{}); len(list) != 0 {
		t.Fatalf("unattributed rows visible to a tenant: %+v", list)
	}
	left, err := s.BackfillTenantIDs(plat, func(ns string) string {
		if ns == "team-a" {
			return tenantA
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if left["approvals"] != 1 {
		t.Errorf("left = %v, want 1 unattributed approval", left)
	}
	if list, _ := s.ListApprovals(WithTenant(ctx, tenantA), ApprovalQuery{}); len(list) != 1 || list[0].Namespace != "team-a" {
		t.Errorf("after backfill A sees %+v", list)
	}
}
