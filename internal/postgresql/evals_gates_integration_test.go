package postgresql

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/routingbench"
)

// gateSetup makes a dataset with a deterministic grader and two cases.
func gateSetup(ctx context.Context, t *testing.T, s *Store, name string) (EvalDataset, EvalGrader, []EvalCase) {
	t.Helper()
	d := evalDS(ctx, t, s, evalNSA, name)
	cases, _, err := s.AddEvalCases(ctx, d.ID, []EvalCase{{Input: "one", Tags: []string{"billing"}}, {Input: "two"}}, EvalLimits{})
	if err != nil {
		t.Fatal(err)
	}
	g, _, err := s.UpsertEvalGrader(ctx, d.ID, EvalGrader{Name: "has-x", Kind: "contains", Config: []byte(`{"value":"x"}`), Required: true, CreatedBy: evalUser})
	if err != nil {
		t.Fatal(err)
	}
	return d, g, cases
}

func TestEvalGatesAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	ctx := WithTenant(context.Background(), tenantA)
	d, _, _ := gateSetup(ctx, t, s, "gated")

	if _, err := s.GetEvalGate(ctx, evalNSA, "support"); !errors.Is(err, ErrEvalNotFound) {
		t.Fatalf("a gate that was never set: %v", err)
	}
	checkGateValidation(ctx, t, s, d)

	rate, regs := 0.8, 2
	g, old, err := s.PutEvalGate(ctx, EvalGate{Namespace: evalNSA, Agent: "support", DatasetID: d.ID, BaselineRevision: 3, MinPassRate: &rate, MaxRegressions: &regs, Enabled: true, UpdatedBy: evalUser})
	if err != nil || old != nil {
		t.Fatalf("put = %v, old %v", err, old)
	}
	if g.TenantID != tenantA || g.Margin != DefaultGateMargin || g.MinCases != DefaultGateMinCases || g.BudgetUSD != DefaultGateBudgetUSD ||
		g.MinPassRate == nil || *g.MinPassRate != 0.8 || g.MaxRegressions == nil || *g.MaxRegressions != 2 || !g.Enabled {
		t.Fatalf("gate = %+v", g)
	}
	g2, old, err := s.PutEvalGate(ctx, EvalGate{Namespace: evalNSA, Agent: "support", DatasetID: d.ID, BaselineRevision: 4, MinCases: 10, Margin: 0.1, Enabled: false, UpdatedBy: "root"})
	if err != nil || old == nil || old.BaselineRevision != 3 || old.MinPassRate == nil {
		t.Fatalf("replace = %v, old %+v", err, old)
	}
	if g2.ID != g.ID || g2.MinPassRate != nil || g2.MaxRegressions != nil || g2.Enabled || g2.MinCases != 10 || g2.Margin != 0.1 || g2.UpdatedBy != "root" {
		t.Errorf("replaced = %+v", g2)
	}
	if got, err := s.GetEvalGate(ctx, evalNSA, "support"); err != nil || got.ID != g.ID || got.BaselineRevision != 4 {
		t.Errorf("get = %+v, %v", got, err)
	}

	checkGateDeletion(ctx, t, s, d)
}

// checkGateDeletion: a gated dataset cannot be deleted until its gate is.
func checkGateDeletion(ctx context.Context, t *testing.T, s *Store, d EvalDataset) {
	t.Helper()
	if _, err := s.DeleteEvalDataset(ctx, d.ID); !errors.Is(err, ErrDatasetGated) {
		t.Fatalf("deleting a gated dataset: %v", err)
	}
	if _, err := s.DeleteEvalGate(ctx, evalNSA, "support"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteEvalGate(ctx, evalNSA, "support"); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("deleting twice: %v", err)
	}
	if _, err := s.DeleteEvalDataset(ctx, d.ID); err != nil {
		t.Errorf("deleting the dataset after the gate: %v", err)
	}
}

func TestLatestGateExperimentAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	ctx := WithTenant(context.Background(), tenantA)
	d, grader, cases := gateSetup(ctx, t, s, "latest")
	gateArms := []EvalArm{{Label: "baseline", Agent: "support", Revision: 1}, {Label: "candidate", Agent: "support"}}
	finish := func(purpose string, arms []EvalArm) EvalExperiment {
		t.Helper()
		e, err := s.CreateEvalExperiment(ctx, EvalExperiment{DatasetID: d.ID, Arms: arms, BudgetUSD: 1, Purpose: purpose, CreatedBy: evalUser}, EvalExperimentLimits{MaxActivePerTenant: 20})
		if err != nil {
			t.Fatal(err)
		}
		cid := cases[0].ID
		res := EvalResult{CaseID: &cid, Arm: arms[0].Label, Phase: "Succeeded", Output: "x", CreatedBy: "eval-runner"}
		if _, err := s.RecordEvalRunResult(ctx, e.ID, res, []EvalGrade{{GraderID: grader.ID, Score: 1, Pass: true}}); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishEvalExperiment(ctx, e.ID, ExperimentComplete, json.RawMessage(`{}`), ""); err != nil {
			t.Fatal(err)
		}
		return e
	}
	if _, found, err := s.LatestGateExperiment(ctx, evalNSA, "support"); err != nil || found {
		t.Fatalf("nothing finished yet: %v %v", found, err)
	}
	finish(PurposeCompare, gateArms) // a comparison is not a gate experiment
	if _, found, _ := s.LatestGateExperiment(ctx, evalNSA, "support"); found {
		t.Fatal("a compare experiment counted as a gate experiment")
	}
	first := finish(PurposeGate, gateArms)
	queued, err := s.CreateEvalExperiment(ctx, EvalExperiment{DatasetID: d.ID, Arms: gateArms, BudgetUSD: 1, Purpose: PurposeGate, CreatedBy: evalUser}, EvalExperimentLimits{MaxActivePerTenant: 20})
	if err != nil {
		t.Fatal(err)
	}
	ev, found, err := s.LatestGateExperiment(ctx, evalNSA, "support")
	if err != nil || !found || ev.Experiment.ID != first.ID {
		t.Fatalf("latest = %+v %v %v: a queued experiment must not count", ev.Experiment.ID, found, err)
	}
	if err := s.FinishEvalExperiment(ctx, queued.ID, ExperimentFailed, nil, "no budget"); err != nil {
		t.Fatal(err)
	}
	second := finish(PurposeGate, gateArms)
	ev, found, err = s.LatestGateExperiment(ctx, evalNSA, "support")
	if err != nil || !found || ev.Experiment.ID != second.ID || ev.Experiment.DatasetVersion != 2 || ev.GradersChanged {
		t.Fatalf("latest = %d %v %v, graders changed %v: the latest finished one counts", ev.Experiment.ID, found, err, ev.GradersChanged)
	}
	if _, found, _ := s.LatestGateExperiment(ctx, evalNSA, "other-agent"); found {
		t.Error("another agent's gate experiment was found")
	}

	// A grader added after the experiment changes the bar it was graded against.
	time.Sleep(10 * time.Millisecond)
	if _, _, err := s.UpsertEvalGrader(ctx, d.ID, EvalGrader{Name: "has-y", Kind: "contains", Config: []byte(`{"value":"y"}`), CreatedBy: evalUser}); err != nil {
		t.Fatal(err)
	}
	if ev, _, _ := s.LatestGateExperiment(ctx, evalNSA, "support"); !ev.GradersChanged {
		t.Error("a grader added since the experiment did not stale it")
	}
}

func TestEvalGatesTenantIsolation(t *testing.T) {
	s, raw := appStore(t)
	base := context.Background()
	a, b := WithTenant(base, tenantA), WithTenant(base, tenantB)
	plat := WithPlatform(base)
	dA, _, _ := gateSetup(a, t, s, "iso")
	dB := evalDS(b, t, s, evalNSA, "iso")
	if _, _, err := s.PutEvalGate(a, EvalGate{Namespace: evalNSA, Agent: "support", DatasetID: dA.ID, BaselineRevision: 1, Enabled: true, UpdatedBy: evalUser}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutEvalGate(b, EvalGate{Namespace: evalNSA, Agent: "support", DatasetID: dB.ID, BaselineRevision: 1, Enabled: true, UpdatedBy: evalUser}); err != nil {
		t.Fatalf("B has its own gate for the same workspace and agent name: %v", err)
	}
	// A cannot put over B's dataset, and sees only its own gate.
	if _, _, err := s.PutEvalGate(a, EvalGate{Namespace: evalNSA, Agent: "other", DatasetID: dB.ID, BaselineRevision: 1, UpdatedBy: evalUser}); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A gates on B's dataset: %v", err)
	}
	ga, err := s.GetEvalGate(a, evalNSA, "support")
	if err != nil || ga.TenantID != tenantA || ga.DatasetID != dA.ID {
		t.Errorf("A reads %+v, %v", ga, err)
	}
	gb, err := s.GetEvalGate(b, evalNSA, "support")
	if err != nil || gb.TenantID != tenantB || gb.DatasetID != dB.ID {
		t.Errorf("B reads %+v, %v", gb, err)
	}
	if _, err := s.DeleteEvalGate(WithTenant(base, "tnt_01935c7e-0000-7000-8000-00000000000c"), evalNSA, "support"); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("a stranger deletes a gate: %v", err)
	}
	if _, err := s.GetEvalGate(base, evalNSA, "support"); err == nil {
		t.Error("an unscoped read of gates succeeded")
	}
	if _, err := s.GetEvalGate(plat, evalNSA, "support"); err != nil && !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("platform read: %v", err)
	}

	var n int
	if err := raw.QueryRowContext(base, `SELECT count(*) FROM eval_gates`).Scan(&n); err != nil || n != 0 {
		t.Errorf("eval_gates: unset setting returned %d rows, %v; want 0", n, err)
	}
	var forced int
	if err := raw.QueryRowContext(base, `SELECT count(*) FROM pg_class WHERE relname = 'eval_gates' AND relrowsecurity AND relforcerowsecurity`).Scan(&forced); err != nil || forced != 1 {
		t.Errorf("eval_gates ENABLE and FORCE ROW LEVEL SECURITY = %d, %v", forced, err)
	}
	refused := map[string]struct {
		scope context.Context
		query string
		args  []any
	}{
		"RLS: a gate for B written by A": {a,
			`INSERT INTO eval_gates (tenant_id, namespace, agent, dataset_id, baseline_revision, updated_by) VALUES ($1, 'x', 'y', $2, 1, 'x')`, []any{tenantB, dB.ID}},
		"composite key: a gate under another tenant than its dataset's": {plat,
			`INSERT INTO eval_gates (tenant_id, namespace, agent, dataset_id, baseline_revision, updated_by) VALUES ($1, 'x', 'y', $2, 1, 'x')`, []any{tenantB, dA.ID}},
		"a baseline revision of zero": {a,
			`INSERT INTO eval_gates (tenant_id, namespace, agent, dataset_id, baseline_revision, updated_by) VALUES ($1, 'x', 'y', $2, 0, 'x')`, []any{tenantA, dA.ID}},
		"a margin of zero": {a,
			`INSERT INTO eval_gates (tenant_id, namespace, agent, dataset_id, baseline_revision, margin, updated_by) VALUES ($1, 'x', 'y', $2, 1, 0, 'x')`, []any{tenantA, dA.ID}},
		"a second gate for one agent": {a,
			`INSERT INTO eval_gates (tenant_id, namespace, agent, dataset_id, baseline_revision, updated_by) VALUES ($1, $2, 'support', $3, 1, 'x')`, []any{tenantA, evalNSA, dA.ID}},
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
	// A cannot update or delete B's gate row even by guessing its ID.
	err = s.scoped(a, func(tx dbtx, _ string) error {
		res, err := tx.ExecContext(base, `UPDATE eval_gates SET enabled = FALSE WHERE id = $1`, gb.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 0 {
			t.Errorf("A updated %d of B's gates", n)
		}
		res, err = tx.ExecContext(base, `DELETE FROM eval_gates WHERE id = $1`, gb.ID)
		if n, _ := res.RowsAffected(); err == nil && n != 0 {
			t.Errorf("A deleted %d of B's gates", n)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetEvalGate(b, evalNSA, "support"); !got.Enabled {
		t.Error("B's gate was changed by A")
	}
	// Another tenant's gate experiment is not found either.
	if _, found, _ := s.LatestGateExperiment(b, evalNSA, "support"); found {
		t.Error("B found an experiment it never ran")
	}
}

// gateExperimentOf makes a finished model-comparison experiment of the "support"
// agent with a result and a grade per case and arm: the second arm passes the
// cases in pass, the first the others.
func modelExperiment(ctx context.Context, t *testing.T, s *Store, d EvalDataset, g EvalGrader, cases []EvalCase, arms []EvalArm, baselinePasses map[int]bool) {
	t.Helper()
	e, err := s.CreateEvalExperiment(ctx, EvalExperiment{DatasetID: d.ID, Arms: arms, BudgetUSD: 1, CreatedBy: evalUser}, EvalExperimentLimits{MaxActivePerTenant: 20})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		for ai, arm := range arms {
			pass := ai > 0 || baselinePasses[i]
			score := 0.0
			if pass {
				score = 1
			}
			cid := c.ID
			res := EvalResult{CaseID: &cid, Arm: arm.Label, Phase: "Succeeded", Output: "x", CostUSD: 0.01 * float64(len(arms)-ai), CreatedBy: "eval-runner"}
			if _, err := s.RecordEvalRunResult(ctx, e.ID, res, []EvalGrade{{GraderID: g.ID, Score: score, Pass: pass}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.FinishEvalExperiment(ctx, e.ID, ExperimentComplete, json.RawMessage(`{}`), ""); err != nil {
		t.Fatal(err)
	}
}

func TestRoutingEvidenceFromExperimentsAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	base := context.Background()
	a, b := WithTenant(base, tenantA), WithTenant(base, tenantB)
	d, g, cases := gateSetup(a, t, s, "routing")
	arms := []EvalArm{{Label: "strong", Agent: "support", ModelProvider: "strong-model"}, {Label: "cheap", Agent: "support", ModelProvider: "cheap-model"}}
	modelExperiment(a, t, s, d, g, cases, arms, map[int]bool{0: true}) // case 0 both pass, case 1 only cheap passes
	// An experiment whose baseline names no provider cannot say which model served.
	modelExperiment(a, t, s, d, g, cases, []EvalArm{{Label: "live", Agent: "support"}, {Label: "cheap", Agent: "support", ModelProvider: "cheap-model"}}, nil)
	// Another tenant's experiment on the same workspace and agent names.
	dB, gB, casesB := gateSetup(b, t, s, "routing")
	modelExperiment(b, t, s, dB, gB, casesB, arms, nil)

	now := time.Now()
	q := routingbench.Query{Namespace: evalNSA, Agents: []string{"support"}, Since: now.Add(-time.Hour), Now: now.Add(time.Minute)}
	off, err := s.LoadObservations(a, q)
	if err != nil || len(off.Samples) != 0 {
		t.Fatalf("flag off: %d samples, %v: experiments must not be read unless asked for", len(off.Samples), err)
	}
	q.IncludeExperiments = true
	on, err := s.LoadObservations(a, q)
	if err != nil {
		t.Fatal(err)
	}
	// Two cases, the second also tagged: case 0 is "billing", case 1 untagged.
	got := map[string]routingbench.Sample{}
	for _, sm := range on.Samples {
		if !sm.FromExperiment || sm.Agent != "support" || sm.ServedProvider != "strong-model" || sm.ShadowProvider != "cheap-model" {
			t.Errorf("sample = %+v", sm)
		}
		got[sm.Category] = sm
	}
	if len(on.Samples) != 2 || got["tag:billing"].Winner != routingbench.WinnerTie || got["tag:untagged"].Winner != routingbench.WinnerShadow {
		t.Fatalf("samples = %+v", on.Samples)
	}
	if sm := got["tag:untagged"]; sm.ServedCostUSD != 0.02 || sm.ShadowCostUSD != 0.01 {
		t.Errorf("costs = %v %v", sm.ServedCostUSD, sm.ShadowCostUSD)
	}
	// Outside the window, or for other agents, nothing.
	q2 := q
	q2.Since, q2.Now = now.Add(-3*time.Hour), now.Add(-2*time.Hour)
	if ev, _ := s.LoadObservations(a, q2); len(ev.Samples) != 0 {
		t.Errorf("an experiment outside the window gave %d samples", len(ev.Samples))
	}
	q3 := q
	q3.Agents = []string{"someone-else"}
	if ev, _ := s.LoadObservations(a, q3); len(ev.Samples) != 0 {
		t.Errorf("another agent gave %d samples", len(ev.Samples))
	}
	// B reads only its own experiment.
	if ev, err := s.LoadObservations(b, q); err != nil || len(ev.Samples) != 2 {
		t.Errorf("B has %d samples, %v", len(ev.Samples), err)
	}
}

// checkGateValidation: gates the tables or the store would not take.
func checkGateValidation(ctx context.Context, t *testing.T, s *Store, d EvalDataset) {
	t.Helper()
	for name, bad := range map[string]EvalGate{
		"no baseline":     {Namespace: evalNSA, Agent: "support", DatasetID: d.ID, UpdatedBy: evalUser},
		"margin of one":   {Namespace: evalNSA, Agent: "support", DatasetID: d.ID, BaselineRevision: 1, Margin: 1, UpdatedBy: evalUser},
		"too many cases":  {Namespace: evalNSA, Agent: "support", DatasetID: d.ID, BaselineRevision: 1, MinCases: MaxGateMinCases + 1, UpdatedBy: evalUser},
		"another space":   {Namespace: evalNSB, Agent: "support", DatasetID: d.ID, BaselineRevision: 1, UpdatedBy: evalUser},
		"no updater":      {Namespace: evalNSA, Agent: "support", DatasetID: d.ID, BaselineRevision: 1},
		"unknown dataset": {Namespace: evalNSA, Agent: "support", DatasetID: d.ID + 999, BaselineRevision: 1, UpdatedBy: evalUser},
	} {
		if _, _, err := s.PutEvalGate(ctx, bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
