package postgresql

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// archivedRow is what a test inserts as the archive of one run.
type archivedRow struct {
	id, tenant string
	age        time.Duration // how long ago the run finished and was last written
	output     string
	trace      string // "" stores NULL
}

func TestReduceArchivedRunsAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	admin, err := sql.Open("pgx", os.Getenv("AGENTORCA_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	ctx := context.Background()
	a, b, plat := WithTenant(ctx, tenantA), WithTenant(ctx, tenantB), WithPlatform(ctx)
	now := time.Now().UTC()

	insert := func(r archivedRow) {
		t.Helper()
		done := now.Add(-r.age)
		var trace any
		if r.trace != "" {
			trace = r.trace
		}
		_, err := admin.ExecContext(ctx, `
			INSERT INTO archived_runs (id, name, namespace, agent_ref, input, parent_run_ref, prior_run_ref, pod_name, tenant, phase, spend_usd, cost_capture, output, raw_output,
				start_time, completion_time, context_used_tokens, routing_decisions, trace_events, tenant_id, created_at, updated_at)
			VALUES ($1, split_part($1, '/', 2), split_part($1, '/', 1), 'bot', 'the input', '', '', 'pod', 'acme', 'Succeeded', '0.42', 'complete', $2, $2,
				$3, $3, 1234, '[{"model":"m1"}]'::jsonb, $4::jsonb, $5, $3, $3)`,
			r.id, r.output, done, trace, r.tenant)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(archivedRow{"shared/old-a", tenantA, 100 * 24 * time.Hour, "out-a", `[{"id":"1"}]`})
	insert(archivedRow{"shared/old-b", tenantB, 100 * 24 * time.Hour, "out-b", `[{"id":"2"}]`})
	insert(archivedRow{"shared/new-a", tenantA, 24 * time.Hour, "out-new", `[{"id":"3"}]`})
	insert(archivedRow{"shared/old-empty", tenantA, 100 * 24 * time.Hour, "", ""})
	insert(archivedRow{"shared/old-trace-only", tenantA, 100 * 24 * time.Hour, "", `[{"id":"4"}]`})
	// Finished long ago but written again recently (re-archived): not touched.
	insert(archivedRow{"shared/rewritten", tenantA, 100 * 24 * time.Hour, "out-rewritten", `[{"id":"5"}]`})
	if _, err := admin.ExecContext(ctx, `UPDATE archived_runs SET updated_at = now() WHERE id = 'shared/rewritten'`); err != nil {
		t.Fatal(err)
	}
	// A dataset case that points at an old run and has copied what it needs.
	d := evalDS(a, t, s, evalNSA, "support")
	cases, _, err := s.AddEvalCases(a, d.ID, []EvalCase{{
		Input: "copied input", Reference: "copied reference", ReferenceKind: ReferenceExample,
		SourceRunNamespace: "shared", SourceRun: "old-a", SourceUser: evalUser,
	}}, EvalLimits{})
	if err != nil {
		t.Fatal(err)
	}

	cutoff := now.Add(-90 * 24 * time.Hour)
	// A tenant scope may not run the sweep.
	if _, err := s.ReduceArchivedRunsBefore(a, cutoff); !errors.Is(err, ErrNoTenantScope) {
		t.Fatalf("a tenant ran the sweep: %v", err)
	}
	if _, err := s.ReduceArchivedRunsBefore(ctx, cutoff); !errors.Is(err, ErrNoTenantScope) {
		t.Fatalf("a call with no scope ran the sweep: %v", err)
	}
	n, err := s.ReduceArchivedRunsBefore(plat, cutoff)
	if err != nil || n != 3 {
		t.Fatalf("sweep reduced %d, %v; want 3 (old-a, old-b, old-trace-only, across both tenants)", n, err)
	}

	type state struct {
		output, raw string
		trace       sql.NullString
		pruned      bool
		spend, in   string
		tokens      int
		routing     string
	}
	load := func(id string) state {
		t.Helper()
		var st state
		var prunedAt sql.NullTime
		err := admin.QueryRowContext(ctx, `SELECT COALESCE(output,''), COALESCE(raw_output,''), trace_events::text, payload_pruned_at,
			spend_usd, input, context_used_tokens, routing_decisions::text FROM archived_runs WHERE id = $1`, id).
			Scan(&st.output, &st.raw, &st.trace, &prunedAt, &st.spend, &st.in, &st.tokens, &st.routing)
		if err != nil {
			t.Fatal(err)
		}
		st.pruned = prunedAt.Valid
		return st
	}
	for _, id := range []string{"shared/old-a", "shared/old-b", "shared/old-trace-only"} {
		st := load(id)
		if st.output != "" || st.raw != "" || st.trace.Valid || !st.pruned {
			t.Errorf("%s was not reduced: %+v", id, st)
		}
		// The summary survives: cost, tokens, input, and the model the cost dashboard reads.
		if st.spend != "0.42" || st.tokens != 1234 || st.in != "the input" || st.routing != `[{"model": "m1"}]` {
			t.Errorf("%s lost its summary: %+v", id, st)
		}
	}
	for _, id := range []string{"shared/new-a", "shared/rewritten"} {
		if st := load(id); st.output == "" || !st.trace.Valid || st.pruned {
			t.Errorf("%s is recent and must be intact: %+v", id, st)
		}
	}
	// A row with no payload is not stamped as reduced.
	if st := load("shared/old-empty"); st.pruned {
		t.Errorf("old-empty was marked reduced although it had nothing to reduce")
	}

	checkReducedRunsStillServe(t, s, a, b, cases[0].ID)

	// Idempotent: nothing is left to reduce.
	if n, err := s.ReduceArchivedRunsBefore(plat, cutoff); err != nil || n != 0 {
		t.Errorf("second sweep reduced %d, %v; want 0", n, err)
	}

	// Archiving a run again writes its payload and clears the mark; a later sweep takes it again.
	if _, err := admin.ExecContext(ctx, `UPDATE archived_runs SET output = 'again', payload_pruned_at = NULL, updated_at = $2 WHERE id = $1`,
		"shared/old-b", now.Add(-100*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ReduceArchivedRunsBefore(plat, cutoff); err != nil || n != 1 {
		t.Errorf("sweep after a re-archive reduced %d, %v; want 1", n, err)
	}
}

func TestReduceArchivedRunsBatches(t *testing.T) {
	s, _ := appStore(t)
	admin, err := sql.Open("pgx", os.Getenv("AGENTORCA_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	ctx := context.Background()
	total := archivedRunsSweepBatch*2 + 7
	if _, err := admin.ExecContext(ctx, `
		INSERT INTO archived_runs (id, name, namespace, agent_ref, phase, output, completion_time, tenant_id, created_at, updated_at)
		SELECT 'ns/r' || i, 'r' || i, 'ns', 'bot', 'Succeeded', 'out', now() - interval '200 days', $1, now() - interval '200 days', now() - interval '200 days'
		FROM generate_series(1, $2) i`, tenantA, total); err != nil {
		t.Fatal(err)
	}
	n, err := s.ReduceArchivedRunsBefore(WithPlatform(ctx), time.Now().Add(-30*24*time.Hour))
	if err != nil || n != int64(total) {
		t.Errorf("reduced %d, %v; want %d across several batches", n, err, total)
	}
}

// checkReducedRunsStillServe checks what keeps working on reduced rows: cost
// rows, history, the run page, tenant isolation, and the dataset case that
// points at a reduced run.
func checkReducedRunsStillServe(t *testing.T, s *Store, a, b context.Context, caseID int64) {
	t.Helper()
	// Cost, history and the run page still work on a reduced row, per tenant.
	costs, err := s.QueryCostRows(a, CostQuery{})
	if err != nil || len(costs) != 5 { // 5 archived runs of tenant A, whatever their payload
		t.Errorf("tenant A cost rows = %d, %v; want 5", len(costs), err)
	}
	for _, c := range costs {
		if c.SpendUSD != "0.42" || c.Model != "m1" {
			t.Errorf("cost row %s = %+v", c.Name, c)
		}
	}
	if costs, _ := s.QueryCostRows(b, CostQuery{}); len(costs) != 1 {
		t.Errorf("tenant B cost rows = %d, want 1", len(costs))
	}
	page, err := s.QueryHistory(a, HistoryQuery{})
	if err != nil || page.Total != 5 {
		t.Errorf("history total = %+v, %v; want 5", page, err)
	}
	run, err := s.GetRun(a, "shared", "old-a")
	if err != nil || run == nil || run.PayloadPrunedAt == nil || run.Output != "" || run.Input != "the input" {
		t.Errorf("GetRun of a reduced run = %+v, %v", run, err)
	}
	if other, _ := s.GetRun(b, "shared", "old-a"); other != nil {
		t.Errorf("tenant B reads tenant A's archived run")
	}

	// The dataset case copied its data and is unaffected by the sweep.
	got, err := s.GetEvalCase(a, caseID)
	if err != nil || got.Input != "copied input" || got.Reference != "copied reference" || got.SourceRun != "old-a" {
		t.Errorf("case after the sweep = %+v, %v", got, err)
	}

}

// The operator archives every finished run again whenever it restarts, and the
// live AgentRun still has its output. That must not undo the retention sweep:
// a reduced run stays reduced, and a run that has not changed keeps the
// updated_at the sweep goes by.
func TestArchiveRunAgainKeepsRetention(t *testing.T) {
	s, _ := appStore(t)
	admin, err := sql.Open("pgx", os.Getenv("AGENTORCA_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	ctx := context.Background()
	a, plat := WithTenant(ctx, tenantA), WithPlatform(ctx)

	old := time.Now().UTC().Add(-100 * 24 * time.Hour).Truncate(time.Second)
	done := metav1.NewTime(old)
	run := archivedRun("old-run", nil)
	run.Status.Output = "the output"
	run.Status.StartTime, run.Status.CompletionTime = &done, &done
	if err := s.ArchiveRun(a, run, []byte(`[{"id":"1"}]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `UPDATE archived_runs SET updated_at = $1, created_at = $1 WHERE name = 'old-run'`, old); err != nil {
		t.Fatal(err)
	}

	// Archived again, unchanged: it does not look freshly written.
	if err := s.ArchiveRun(a, run, []byte(`[{"id":"1"}]`)); err != nil {
		t.Fatal(err)
	}
	var updated time.Time
	if err := admin.QueryRowContext(ctx, `SELECT updated_at FROM archived_runs WHERE name = 'old-run'`).Scan(&updated); err != nil {
		t.Fatal(err)
	}
	if !updated.Equal(old) {
		t.Errorf("re-archiving an unchanged run moved updated_at to %s, want %s", updated, old)
	}

	if n, err := s.ReduceArchivedRunsBefore(plat, time.Now().Add(-90*24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("sweep reduced %d, %v; want 1", n, err)
	}
	// The restart archives it again from the live run, which still has its output.
	if err := s.ArchiveRun(a, run, []byte(`[{"id":"1"}]`)); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(a, evalNSA, "old-run")
	if err != nil || got == nil {
		t.Fatalf("GetRun = %+v, %v", got, err)
	}
	if got.PayloadPrunedAt == nil || got.Output != "" || len(got.TraceEventsJSON) != 0 {
		t.Errorf("a reduced run came back whole after being archived again: prunedAt=%v output=%q trace=%s", got.PayloadPrunedAt, got.Output, got.TraceEventsJSON)
	}
}
