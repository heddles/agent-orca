package postgresql

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func twoArms() []EvalArm {
	return []EvalArm{{Label: "base", Agent: "support"}, {Label: "cand", Agent: "support", ModelProvider: "cheap"}}
}

func evalExpFixture(ctx context.Context, t *testing.T, s *Store, ns, name string) (EvalDataset, []EvalCase) {
	t.Helper()
	d := evalDS(ctx, t, s, ns, name)
	cases, _, err := s.AddEvalCases(ctx, d.ID, []EvalCase{{Input: "one", Reference: "1"}, {Input: "two"}}, EvalLimits{})
	if err != nil {
		t.Fatal(err)
	}
	return d, cases
}

func TestEvalExperimentsAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	ctx := WithTenant(context.Background(), tenantA)
	d, cases := evalExpFixture(ctx, t, s, evalNSA, "exp")
	lim := EvalExperimentLimits{}

	// Create: the dataset version is frozen, defaults are filled in.
	e, err := s.CreateEvalExperiment(ctx, EvalExperiment{DatasetID: d.ID, Arms: twoArms(), BudgetUSD: 2, CreatedBy: evalUser}, lim)
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != ExperimentQueued || e.DatasetVersion != 2 || e.Cases != 2 || e.Repeats != 1 || e.Concurrency != 4 ||
		e.RunBudgetUSD != 1 || e.Namespace != evalNSA || e.TenantID != tenantA || len(e.Arms) != 2 {
		t.Fatalf("created = %+v", e)
	}
	// A dataset edit after the start does not change what the experiment runs.
	if _, _, err := s.AddEvalCases(ctx, d.ID, []EvalCase{{Input: "three"}}, EvalLimits{}); err != nil {
		t.Fatal(err)
	}
	frozen, err := s.ListEvalExperimentCases(ctx, e.ID)
	if err != nil || len(frozen) != 2 {
		t.Fatalf("frozen cases = %d, %v", len(frozen), err)
	}
	if got, _ := s.GetEvalExperiment(ctx, e.ID); got.Cases != 2 {
		t.Errorf("cases at the frozen version = %d", got.Cases)
	}

	// Refusals.
	bad := map[string]EvalExperiment{
		"one arm":        {DatasetID: d.ID, Arms: twoArms()[:1], BudgetUSD: 1, CreatedBy: evalUser},
		"same arm twice": {DatasetID: d.ID, Arms: []EvalArm{{Label: "a", Agent: "x"}, {Label: "b", Agent: "x"}}, BudgetUSD: 1, CreatedBy: evalUser},
		"no budget":      {DatasetID: d.ID, Arms: twoArms(), CreatedBy: evalUser},
		"over cap":       {DatasetID: d.ID, Arms: twoArms(), BudgetUSD: 26, CreatedBy: evalUser},
		"other ns":       {DatasetID: d.ID, Namespace: evalNSB, Arms: twoArms(), BudgetUSD: 1, CreatedBy: evalUser},
	}
	for name, x := range bad {
		if _, err := s.CreateEvalExperiment(ctx, x, lim); !errors.Is(err, ErrInvalidEval) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	empty := evalDS(ctx, t, s, evalNSA, "empty")
	if _, err := s.CreateEvalExperiment(ctx, EvalExperiment{DatasetID: empty.ID, Arms: twoArms(), BudgetUSD: 1, CreatedBy: evalUser}, lim); !errors.Is(err, ErrInvalidEval) {
		t.Errorf("empty dataset: %v", err)
	}
	if _, err := s.CreateEvalExperiment(ctx, EvalExperiment{DatasetID: d.ID, Arms: twoArms(), BudgetUSD: 1, CreatedBy: evalUser}, EvalExperimentLimits{MaxCasesPerExperiment: 1}); !errors.Is(err, ErrInvalidEval) {
		t.Errorf("too many cases: %v", err)
	}

	// At most two active experiments per tenant.
	if _, err := s.CreateEvalExperiment(ctx, EvalExperiment{DatasetID: d.ID, Arms: twoArms(), BudgetUSD: 1, CreatedBy: evalUser}, lim); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateEvalExperiment(ctx, EvalExperiment{DatasetID: d.ID, Arms: twoArms(), BudgetUSD: 1, CreatedBy: evalUser}, lim); !errors.Is(err, ErrTooManyExperiments) {
		t.Fatalf("third experiment: %v", err)
	}
	// Another tenant has its own allowance.
	b := WithTenant(context.Background(), tenantB)
	dB, _ := evalExpFixture(b, t, s, evalNSA, "exp")
	if _, err := s.CreateEvalExperiment(b, EvalExperiment{DatasetID: dB.ID, Arms: twoArms(), BudgetUSD: 1, CreatedBy: evalUser}, lim); err != nil {
		t.Fatalf("tenant B: %v", err)
	}

	checkExperimentRunnerCalls(t, s, ctx, e, cases)
}

func checkExperimentRunnerCalls(t *testing.T, s *Store, ctx context.Context, e EvalExperiment, cases []EvalCase) {
	t.Helper()
	plat := WithPlatform(context.Background())
	// Claiming needs the platform scope, takes each queued experiment once per
	// owner, and skips one leased to somebody else.
	if _, err := s.ClaimEvalExperiments(ctx, "op-1", time.Minute, 10); !errors.Is(err, ErrNoTenantScope) {
		t.Errorf("tenant-scoped claim: %v", err)
	}
	claimed, err := s.ClaimEvalExperiments(plat, "op-1", time.Minute, 10)
	if err != nil || len(claimed) != 3 {
		t.Fatalf("claim = %d, %v", len(claimed), err)
	}
	if other, _ := s.ClaimEvalExperiments(plat, "op-2", time.Minute, 10); len(other) != 0 {
		t.Errorf("a second owner claimed %d leased experiments", len(other))
	}
	if again, _ := s.ClaimEvalExperiments(plat, "op-1", time.Minute, 10); len(again) != 3 {
		t.Errorf("the owner re-claimed %d, want 3 (resume after restart)", len(again))
	}

	// Start with resolved arms, then record results.
	arms := e.Arms
	arms[0].Hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := s.StartEvalExperiment(ctx, e.ID, arms); err != nil {
		t.Fatal(err)
	}
	g, _, err := s.UpsertEvalGrader(ctx, e.DatasetID, EvalGrader{Name: "g", Kind: "contains", Config: []byte(evalContainsCfg), CreatedBy: evalUser})
	if err != nil {
		t.Fatal(err)
	}
	cid := cases[0].ID
	res := EvalResult{CaseID: &cid, Arm: "base", RunNamespace: evalNSA, RunName: "eval-1", Phase: "Succeeded", Output: "refund", CostUSD: 0.25, CostIncomplete: true, CreatedBy: "runner"}
	ok, err := s.RecordEvalRunResult(ctx, e.ID, res, []EvalGrade{{GraderID: g.ID, Score: 1, Pass: true}})
	if err != nil || !ok {
		t.Fatalf("record = %v, %v", ok, err)
	}
	// Recording the same slot again changes nothing, and adds no cost.
	if ok, err := s.RecordEvalRunResult(ctx, e.ID, res, nil); err != nil || ok {
		t.Fatalf("second record = %v, %v", ok, err)
	}
	got, _ := s.GetEvalExperiment(ctx, e.ID)
	if got.Status != ExperimentRunning || got.SpentUSD != 0.25 || got.ResultsDone != 1 || got.StartedAt == nil || got.Arms[0].Hash == "" {
		t.Errorf("after record: %+v", got)
	}
	keys, _ := s.ListEvalExperimentResultKeys(ctx, e.ID)
	if len(keys) != 1 || keys[0].Arm != "base" || keys[0].Phase != "Succeeded" {
		t.Errorf("keys = %+v", keys)
	}
	list, err := s.ListEvalExperimentResults(ctx, e.ID, EvalExperimentResultQuery{})
	if err != nil || len(list) != 1 || len(list[0].Grades) != 1 || !list[0].CostIncomplete || list[0].RunName != "eval-1" ||
		list[0].ExperimentID == nil || *list[0].ExperimentID != e.ID {
		t.Fatalf("results = %+v, %v", list, err)
	}
	// Experiment results stay out of the dataset's free-standing results.
	if free, _ := s.ListEvalResults(ctx, e.DatasetID, EvalResultQuery{}); len(free) != 0 {
		t.Errorf("dataset results hold %d experiment results", len(free))
	}
	if spend, err := s.QueryEvalSpend(ctx, EvalSpendQuery{}); err != nil || spend.TotalUSD != 0.25 || spend.Experiments != 2 {
		t.Errorf("spend = %+v, %v", spend, err)
	}
	if spend, _ := s.QueryEvalSpend(ctx, EvalSpendQuery{Namespaces: []string{}}); spend.Experiments != 0 {
		t.Errorf("no namespaces matched %d", spend.Experiments)
	}

	// Cancel: running is marked, finishing it records the summary once.
	c, err := s.RequestEvalCancel(ctx, e.ID)
	if err != nil || !c.CancelRequested || c.Status != ExperimentRunning {
		t.Fatalf("cancel running = %+v, %v", c, err)
	}
	active, _ := s.ActiveEvalExperiments(plat, []int64{e.ID, 999999})
	if !active[e.ID] || active[999999] {
		t.Errorf("active = %v", active)
	}
	if err := s.FinishEvalExperiment(ctx, e.ID, ExperimentCancelled, json.RawMessage(`{"partial":true}`), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishEvalExperiment(ctx, e.ID, ExperimentComplete, nil, ""); !errors.Is(err, ErrExperimentFinished) {
		t.Errorf("finish twice: %v", err)
	}
	if _, err := s.RequestEvalCancel(ctx, e.ID); !errors.Is(err, ErrExperimentFinished) {
		t.Errorf("cancel finished: %v", err)
	}
	done, _ := s.GetEvalExperiment(ctx, e.ID)
	if done.Status != ExperimentCancelled || done.FinishedAt == nil || string(done.Summary) != `{"partial": true}` || done.CancelRequested {
		t.Errorf("finished = %+v (%s)", done, done.Summary)
	}
	if active, _ := s.ActiveEvalExperiments(plat, []int64{e.ID}); active[e.ID] {
		t.Error("a finished experiment is active")
	}
	// A queued experiment is cancelled at once.
	queued, _ := s.ListEvalExperiments(ctx, EvalExperimentQuery{Status: ExperimentQueued})
	if len(queued) != 1 {
		t.Fatalf("queued = %d", len(queued))
	}
	if c, err := s.RequestEvalCancel(ctx, queued[0].ID); err != nil || c.Status != ExperimentCancelled {
		t.Errorf("cancel queued = %+v, %v", c, err)
	}
	if all, _ := s.ListEvalExperiments(ctx, EvalExperimentQuery{Limit: 1}); len(all) != 1 || all[0].ID != queued[0].ID {
		t.Errorf("newest first: %+v", all)
	}
	// Deleting a dataset takes its experiments and their results.
	if _, err := s.DeleteEvalDataset(ctx, e.DatasetID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetEvalExperiment(ctx, e.ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("experiment survived its dataset: %v", err)
	}
}

func TestEvalExperimentsTenantIsolation(t *testing.T) {
	s, raw := appStore(t)
	base := context.Background()
	a, b, plat := WithTenant(base, tenantA), WithTenant(base, tenantB), WithPlatform(base)
	dA, casesA := evalExpFixture(a, t, s, evalNSA, "iso")
	dB, casesB := evalExpFixture(b, t, s, evalNSA, "iso")
	eA, err := s.CreateEvalExperiment(a, EvalExperiment{DatasetID: dA.ID, Arms: twoArms(), BudgetUSD: 1, CreatedBy: evalUser}, EvalExperimentLimits{})
	if err != nil {
		t.Fatal(err)
	}
	eB, err := s.CreateEvalExperiment(b, EvalExperiment{DatasetID: dB.ID, Arms: twoArms(), BudgetUSD: 1, CreatedBy: evalUser}, EvalExperimentLimits{})
	if err != nil {
		t.Fatal(err)
	}
	// A cannot create over, read, list, cancel, start, record into or finish B's.
	if _, err := s.CreateEvalExperiment(a, EvalExperiment{DatasetID: dB.ID, Arms: twoArms(), BudgetUSD: 1, CreatedBy: evalUser}, EvalExperimentLimits{}); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A creates over B's dataset: %v", err)
	}
	if _, err := s.GetEvalExperiment(a, eB.ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A reads B's experiment: %v", err)
	}
	if _, err := s.RequestEvalCancel(a, eB.ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A cancels B's experiment: %v", err)
	}
	if _, err := s.ListEvalExperimentResults(a, eB.ID, EvalExperimentResultQuery{}); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A lists B's results: %v", err)
	}
	if err := s.StartEvalExperiment(a, eB.ID, eB.Arms); !errors.Is(err, ErrExperimentFinished) {
		t.Errorf("A starts B's experiment: %v", err)
	}
	if err := s.FinishEvalExperiment(a, eB.ID, ExperimentFailed, nil, "x"); !errors.Is(err, ErrExperimentFinished) {
		t.Errorf("A finishes B's experiment: %v", err)
	}
	cidB := casesB[0].ID
	if _, err := s.RecordEvalRunResult(a, eB.ID, EvalResult{CaseID: &cidB, Arm: "base", Output: "x", CreatedBy: "x"}, nil); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A records into B's experiment: %v", err)
	}
	if got, _ := s.GetEvalExperiment(b, eB.ID); got.Status != ExperimentQueued || got.ResultsDone != 0 {
		t.Errorf("B's experiment was changed: %+v", got)
	}
	if list, _ := s.ListEvalExperiments(a, EvalExperimentQuery{}); len(list) != 1 || list[0].ID != eA.ID {
		t.Errorf("A lists %+v", list)
	}
	if list, _ := s.ListEvalExperiments(plat, EvalExperimentQuery{}); len(list) != 2 {
		t.Errorf("platform lists %d", len(list))
	}
	// An experiment's results are a tenant table with a composite key to it.
	base2 := context.Background()
	for _, table := range []string{"eval_experiments", "eval_results"} {
		var n int
		if err := raw.QueryRowContext(base2, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: unset setting returned %d rows, %v; want 0", table, n, err)
		}
	}
	cidA := casesA[0].ID
	refused := map[string]struct {
		scope context.Context
		query string
		args  []any
	}{
		"RLS: an experiment for B written by A": {a,
			`INSERT INTO eval_experiments (tenant_id, namespace, dataset_id, dataset_version, arms, budget_usd, run_budget_usd, created_by)
			 VALUES ($1, 'x', $2, 1, '[]', 1, 1, 'x')`, []any{tenantB, dB.ID}},
		"composite key: an experiment under another tenant than its dataset's": {plat,
			`INSERT INTO eval_experiments (tenant_id, namespace, dataset_id, dataset_version, arms, budget_usd, run_budget_usd, created_by)
			 VALUES ($1, 'x', $2, 1, '[]', 1, 1, 'x')`, []any{tenantB, dA.ID}},
		"composite key: a result under another tenant than its experiment's": {plat,
			`INSERT INTO eval_results (tenant_id, dataset_id, case_id, experiment_id, arm, output, created_by)
			 VALUES ($1, $2, $3, $4, 'x', '', 'x')`, []any{tenantB, dB.ID, casesB[0].ID, eA.ID}},
		"a budget of zero": {a,
			`INSERT INTO eval_experiments (tenant_id, namespace, dataset_id, dataset_version, arms, budget_usd, run_budget_usd, created_by)
			 VALUES ($1, 'x', $2, 1, '[]', 0, 1, 'x')`, []any{tenantA, dA.ID}},
		"six repeats": {a,
			`INSERT INTO eval_experiments (tenant_id, namespace, dataset_id, dataset_version, arms, repeats, budget_usd, run_budget_usd, created_by)
			 VALUES ($1, 'x', $2, 1, '[]', 6, 1, 1, 'x')`, []any{tenantA, dA.ID}},
	}
	for name, c := range refused {
		err := s.scoped(c.scope, func(tx dbtx, _ string) error {
			_, err := tx.ExecContext(base2, c.query, c.args...)
			return err
		})
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// A result row for the same slot twice is refused by the unique index.
	if _, err := s.RecordEvalRunResult(a, eA.ID, EvalResult{CaseID: &cidA, Arm: "base", Output: "x", CreatedBy: "x"}, nil); err != nil {
		t.Fatal(err)
	}
	err = s.scoped(a, func(tx dbtx, _ string) error {
		_, err := tx.ExecContext(base2, `INSERT INTO eval_results (tenant_id, dataset_id, case_id, experiment_id, arm, output, created_by)
			VALUES ($1, $2, $3, $4, 'base', '', 'x')`, tenantA, dA.ID, cidA, eA.ID)
		return err
	})
	if err == nil {
		t.Error("a second result for one slot was accepted")
	}
	var forced int
	if err := raw.QueryRowContext(base2, `SELECT count(*) FROM pg_class WHERE relname = 'eval_experiments' AND relrowsecurity AND relforcerowsecurity`).Scan(&forced); err != nil || forced != 1 {
		t.Errorf("eval_experiments ENABLE and FORCE ROW LEVEL SECURITY = %d, %v", forced, err)
	}
}
