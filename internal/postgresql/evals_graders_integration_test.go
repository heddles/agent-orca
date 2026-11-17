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
	"errors"
	"strings"
	"testing"
)

const evalContainsCfg = `{"value":"refund"}`

func evalGrader(ctx context.Context, t *testing.T, s *Store, ds int64, name string) EvalGrader {
	t.Helper()
	g, _, err := s.UpsertEvalGrader(ctx, ds, EvalGrader{Name: name, Kind: "contains", Config: []byte(evalContainsCfg), CreatedBy: evalUser})
	if err != nil {
		t.Fatalf("grader %s: %v", name, err)
	}
	return g
}

func TestEvalGradersAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	ctx := WithTenant(context.Background(), tenantA)
	d := evalDS(ctx, t, s, evalNSA, "graded")
	cases, v, err := s.AddEvalCases(ctx, d.ID, []EvalCase{{Input: "q", Reference: "r"}}, EvalLimits{})
	if err != nil {
		t.Fatal(err)
	}

	g, created, err := s.UpsertEvalGrader(ctx, d.ID, EvalGrader{Name: "g", Kind: "tool_called", Config: []byte(` {"tool": "x"} `), Required: true, CreatedBy: evalUser})
	if err != nil || !created || g.Weight != 1 || string(g.Config) != `{"tool":"x"}` || g.TenantID != tenantA {
		t.Fatalf("create = %+v %v %v", g, created, err)
	}
	g2, created, err := s.UpsertEvalGrader(ctx, d.ID, EvalGrader{Name: "g", Kind: "max_cost", Config: []byte(`{"usd":1}`), Weight: 3, CreatedBy: "other"})
	if err != nil || created || g2.ID != g.ID || g2.Kind != "max_cost" || g2.Required || g2.CreatedBy != evalUser {
		t.Fatalf("replace = %+v %v %v", g2, created, err)
	}
	if got, _ := s.GetEvalDataset(ctx, d.ID); got.Version != v {
		t.Errorf("graders changed the dataset version: %d, want %d", got.Version, v)
	}
	checkEvalGraderRefusals(t, s, ctx, d)
	res := checkEvalResultRoundTrip(t, s, ctx, d, g, cases[0].ID)
	checkEvalResultRefusals(t, s, ctx, d, g)

	// Deleting a grader takes its grades; deleting the dataset takes the rest.
	if _, err := s.DeleteEvalGrader(ctx, d.ID, "g"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetEvalResult(ctx, res.ID); len(got.Grades) != 0 {
		t.Errorf("grades survived their grader: %+v", got.Grades)
	}
	if _, err := s.DeleteEvalGrader(ctx, d.ID, "g"); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("second delete err = %v", err)
	}
	if _, err := s.DeleteEvalDataset(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetEvalResult(ctx, res.ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("result survived its dataset: %v", err)
	}
}

func checkEvalGraderRefusals(t *testing.T, s *Store, ctx context.Context, d EvalDataset) {
	t.Helper()
	for _, bad := range []EvalGrader{
		{Name: "Bad", Kind: "contains", Config: []byte(evalContainsCfg)},
		{Name: "x", Kind: "regex", Config: []byte(`{"pattern":"("}`)},
		{Name: "x", Kind: "nope", Config: []byte(`{}`)},
		{Name: "x", Kind: "contains", Config: []byte(evalContainsCfg), Weight: -1},
	} {
		bad.CreatedBy = evalUser
		if _, _, err := s.UpsertEvalGrader(ctx, d.ID, bad); !errors.Is(err, ErrInvalidEval) {
			t.Errorf("%+v: err = %v", bad, err)
		}
	}
	if list, err := s.ListEvalGraders(ctx, d.ID); err != nil || len(list) != 1 {
		t.Errorf("list = %+v %v", list, err)
	}
}

func checkEvalResultRoundTrip(t *testing.T, s *Store, ctx context.Context, d EvalDataset, g EvalGrader, cid int64) EvalResult {
	t.Helper()
	res, err := s.SaveEvalResult(ctx, d.ID, EvalResult{CaseID: &cid, Arm: "a", Output: "out", ToolCalls: []string{"x"}, CostUSD: 0.0123, LatencyMs: 9, CasePass: true, CreatedBy: evalUser},
		[]EvalGrade{{GraderID: g.ID, Score: 1, Pass: true, Detail: "ok"}})
	if err != nil || res.ID == 0 || len(res.Grades) != 1 || res.Grades[0].ID == 0 {
		t.Fatalf("save = %+v %v", res, err)
	}
	got, err := s.GetEvalResult(ctx, res.ID)
	if err != nil || got.CostUSD != 0.0123 || got.Output != "out" || len(got.ToolCalls) != 1 || len(got.Grades) != 1 ||
		got.Grades[0].Grader != "g" || got.Grades[0].Kind != "max_cost" || got.CaseID == nil || *got.CaseID != cid {
		t.Fatalf("get = %+v %v", got, err)
	}
	if _, err := s.SaveEvalResult(ctx, d.ID, EvalResult{Output: "free", CreatedBy: evalUser}, nil); err != nil {
		t.Errorf("a result without a case: %v", err)
	}
	if list, _ := s.ListEvalResults(ctx, d.ID, EvalResultQuery{Limit: 1}); len(list) != 1 || list[0].ID != res.ID || len(list[0].Grades) != 1 {
		t.Errorf("page 1 = %+v", list)
	}
	if list, _ := s.ListEvalResults(ctx, d.ID, EvalResultQuery{After: res.ID}); len(list) != 1 || list[0].CaseID != nil {
		t.Errorf("page 2 = %+v", list)
	}
	if list, _ := s.ListEvalResults(ctx, d.ID, EvalResultQuery{CaseID: cid}); len(list) != 1 {
		t.Errorf("by case = %+v", list)
	}
	return res
}

func checkEvalResultRefusals(t *testing.T, s *Store, ctx context.Context, d EvalDataset, g EvalGrader) {
	t.Helper()
	before, _ := s.ListEvalResults(ctx, d.ID, EvalResultQuery{Limit: MaxEvalResultsPerPage})
	missing := int64(999999)
	for name, tc := range map[string]struct {
		r      EvalResult
		grades []EvalGrade
	}{
		"unknown case":   {EvalResult{CaseID: &missing, Output: "x", CreatedBy: evalUser}, nil},
		"unknown grader": {EvalResult{Output: "x", CreatedBy: evalUser}, []EvalGrade{{GraderID: missing, Score: 1, Pass: true}}},
		"score over one": {EvalResult{Output: "x", CreatedBy: evalUser}, []EvalGrade{{GraderID: g.ID, Score: 2, Pass: true}}},
		"huge output":    {EvalResult{Output: strings.Repeat("a", MaxEvalResultOutput+1), CreatedBy: evalUser}, nil},
		"negative cost":  {EvalResult{Output: "x", CostUSD: -1, CreatedBy: evalUser}, nil},
	} {
		if _, err := s.SaveEvalResult(ctx, d.ID, tc.r, tc.grades); err == nil {
			t.Errorf("%s was stored", name)
		}
	}
	after, _ := s.ListEvalResults(ctx, d.ID, EvalResultQuery{Limit: MaxEvalResultsPerPage})
	if len(after) != len(before) {
		t.Errorf("refused saves left %d results behind", len(after)-len(before))
	}
	// A removed case is not gradable.
	cs, _, _ := s.AddEvalCases(ctx, d.ID, []EvalCase{{Input: "gone"}}, EvalLimits{})
	if _, err := s.DeleteEvalCase(ctx, cs[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveEvalResult(ctx, d.ID, EvalResult{CaseID: &cs[0].ID, Output: "x", CreatedBy: evalUser}, nil); !errors.Is(err, ErrInvalidEval) {
		t.Errorf("a removed case: %v", err)
	}
}

func TestEvalGraderTablesTenantIsolationAgainstPostgres(t *testing.T) {
	s, raw := appStore(t)
	base := context.Background()
	a, b, plat := WithTenant(base, tenantA), WithTenant(base, tenantB), WithPlatform(base)
	dA, dB := evalDS(a, t, s, "shared", "d"), evalDS(b, t, s, "shared", "d")
	gA, gB := evalGrader(a, t, s, dA.ID, "g"), evalGrader(b, t, s, dB.ID, "g")
	rB, err := s.SaveEvalResult(b, dB.ID, EvalResult{Output: "b secret", CreatedBy: evalUser}, []EvalGrade{{GraderID: gB.ID, Score: 1, Pass: true}})
	if err != nil {
		t.Fatal(err)
	}

	// Reads and writes through the store.
	if _, err := s.GetEvalResult(a, rB.ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A reads B's result: %v", err)
	}
	if _, err := s.ListEvalResults(a, dB.ID, EvalResultQuery{}); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A lists B's results: %v", err)
	}
	if _, err := s.ListEvalGraders(a, dB.ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A lists B's graders: %v", err)
	}
	if _, err := s.GetEvalGrader(a, dB.ID, "g"); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A reads B's grader: %v", err)
	}
	if _, err := s.DeleteEvalGrader(a, dB.ID, "g"); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A deletes B's grader: %v", err)
	}
	if _, _, err := s.UpsertEvalGrader(a, dB.ID, EvalGrader{Name: "g", Kind: "contains", Config: []byte(evalContainsCfg), CreatedBy: evalUser}); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A replaces B's grader: %v", err)
	}
	if _, err := s.SaveEvalResult(a, dB.ID, EvalResult{Output: "planted", CreatedBy: evalUser}, nil); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A saves into B's dataset: %v", err)
	}
	// A cannot grade with B's grader even in its own dataset.
	if _, err := s.SaveEvalResult(a, dA.ID, EvalResult{Output: "x", CreatedBy: evalUser}, []EvalGrade{{GraderID: gB.ID, Score: 1, Pass: true}}); err == nil {
		t.Error("A graded with B's grader")
	}
	if got, err := s.GetEvalGrader(b, dB.ID, "g"); err != nil || got.Kind != "contains" {
		t.Errorf("B's grader was changed: %+v %v", got, err)
	}
	if list, _ := s.ListEvalResults(plat, dB.ID, EvalResultQuery{}); len(list) != 1 {
		t.Errorf("platform sees %d of B's results", len(list))
	}
	checkEvalGraderRLS(t, s, raw, a, plat, dA, dB, gA)
}

// checkEvalGraderRLS goes under the store, with raw statements: the policy and
// the composite keys refuse what the Go code is never asked to do.
func checkEvalGraderRLS(t *testing.T, s *Store, raw *sql.DB, a, plat context.Context, dA, dB EvalDataset, gA EvalGrader) {
	t.Helper()
	base := context.Background()
	for _, table := range []string{"eval_graders", "eval_results", "eval_grades"} {
		var n int
		if err := raw.QueryRowContext(base, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: unset setting returned %d rows, %v; want 0", table, n, err)
		}
	}
	// Under A an unfiltered count sees only A's rows.
	err := s.scoped(a, func(tx dbtx, _ string) error {
		var n int
		if err := tx.QueryRowContext(base, `SELECT count(*) FROM eval_graders`).Scan(&n); err != nil || n != 1 {
			t.Errorf("A sees %d graders, %v; want 1", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	const insGrader = `INSERT INTO eval_graders (tenant_id, dataset_id, name, kind, created_by) VALUES ($1, $2, 'r', 'contains', 'x')`
	refused := map[string]struct {
		scope context.Context
		query string
		args  []any
	}{
		"RLS: a grader row for B written by A":                            {a, insGrader, []any{tenantB, dB.ID}},
		"composite key: a grader under another tenant than its dataset's": {plat, insGrader, []any{tenantB, dA.ID}},
		"composite key: a result under another tenant than its dataset's": {plat,
			`INSERT INTO eval_results (tenant_id, dataset_id, output, created_by) VALUES ($1, $2, 'x', 'x')`, []any{tenantB, dA.ID}},
		"composite key: a grade under another tenant than its grader's": {plat,
			`INSERT INTO eval_grades (tenant_id, result_id, grader_id, score, pass)
			 SELECT $1, r.id, $2, 1, true FROM eval_results r WHERE r.dataset_id = $3 LIMIT 1`, []any{tenantB, gA.ID, dB.ID}},
		"a grader weight of zero": {a,
			`INSERT INTO eval_graders (tenant_id, dataset_id, name, kind, weight, created_by) VALUES ($1, $2, 'r', 'contains', 0, 'x')`, []any{tenantA, dA.ID}},
	}
	for name, c := range refused {
		err := s.scoped(c.scope, func(tx dbtx, _ string) error {
			_, err := tx.ExecContext(base, c.query, c.args...)
			return err
		})
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	var forced int
	if err := raw.QueryRowContext(base, `SELECT count(*) FROM pg_class WHERE relname IN ('eval_graders','eval_results','eval_grades') AND relrowsecurity AND relforcerowsecurity`).Scan(&forced); err != nil || forced != 3 {
		t.Errorf("tables with ENABLE and FORCE ROW LEVEL SECURITY = %d, %v; want 3", forced, err)
	}
}
