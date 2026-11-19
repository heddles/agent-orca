package postgresql

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"
)

func TestEvalResultRetentionAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	admin, err := sql.Open("pgx", os.Getenv("AGENTORCA_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	a, b := WithTenant(context.Background(), tenantA), WithTenant(context.Background(), tenantB)

	save := func(ctx context.Context, name string) (EvalResult, EvalDataset) {
		d := evalDS(ctx, t, s, evalNSA, name)
		g := evalGrader(ctx, t, s, d.ID, "g")
		r, err := s.SaveEvalResult(ctx, d.ID, EvalResult{Arm: "a", Output: "out", ToolCalls: []string{"x"}, CostUSD: 0.5, CreatedBy: evalUser},
			[]EvalGrade{{GraderID: g.ID, Score: 1, Pass: true}})
		if err != nil {
			t.Fatal(err)
		}
		return r, d
	}
	oldA, _ := save(a, "old-a")
	newA, _ := save(a, "new-a")
	oldB, _ := save(b, "old-b")
	for _, id := range []int64{oldA.ID, oldB.ID} {
		if _, err := admin.ExecContext(context.Background(),
			`UPDATE eval_results SET created_at = now() - interval '100 days' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}

	// A tenant scope may not run the sweep.
	if _, _, err := s.PurgeEvalResultsBefore(a, time.Now().Add(-90*24*time.Hour)); !errors.Is(err, ErrNoTenantScope) {
		t.Fatalf("a tenant ran the sweep: %v", err)
	}
	plat := s
	results, grades, err := plat.PurgeEvalResultsBefore(WithPlatform(context.Background()), time.Now().Add(-90*24*time.Hour))
	if err != nil || results != 2 || grades != 2 {
		t.Fatalf("purge = %d results, %d grades, %v; want 2, 2", results, grades, err)
	}
	// The old results lose output, tool calls and grades in every tenant; the
	// row, cost and the recent result stay.
	for _, c := range []struct {
		ctx  context.Context
		id   int64
		gone bool
	}{{a, oldA.ID, true}, {b, oldB.ID, true}, {a, newA.ID, false}} {
		got, err := s.GetEvalResult(c.ctx, c.id)
		if err != nil {
			t.Fatal(err)
		}
		if c.gone && (got.Output != "" || len(got.ToolCalls) != 0 || len(got.Grades) != 0 || got.CostUSD != 0.5) {
			t.Errorf("result %d was not purged cleanly: %+v", c.id, got)
		}
		if !c.gone && (got.Output != "out" || len(got.Grades) != 1) {
			t.Errorf("recent result %d was purged: %+v", c.id, got)
		}
	}
	// Idempotent.
	if r, g, _ := plat.PurgeEvalResultsBefore(WithPlatform(context.Background()), time.Now().Add(-90*24*time.Hour)); r != 0 || g != 0 {
		t.Errorf("second sweep = %d, %d", r, g)
	}
}
