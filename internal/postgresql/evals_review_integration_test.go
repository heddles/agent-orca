package postgresql

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/heddles/agent-orca/internal/evals/graders"
)

const (
	humanCfg  = `{"scale":"passfail","instructions":"is it right?"}`
	humanCfg5 = `{"scale":"1to5","passAt":0.75}`
	llmCfg    = `{"providerRef":"judge","rubric":"be strict"}`
)

type reviewFixture struct {
	d      EvalDataset
	cases  []EvalCase
	e      EvalExperiment
	human  EvalGrader
	llm    EvalGrader
	result map[string]EvalResult // arm+case index -> result
}

// reviewSetup makes an experiment with two cases and two arms, a human grader
// (required) and an llm grader, and records a result per (case, arm) with a
// pending human grade and an ok llm grade.
func reviewSetup(ctx context.Context, t *testing.T, s *Store, name, humanConfig string) reviewFixture {
	t.Helper()
	d, cases := evalExpFixture(ctx, t, s, name)
	f := reviewFixture{d: d, cases: cases, result: map[string]EvalResult{}}
	var err error
	f.human, _, err = s.UpsertEvalGrader(ctx, d.ID, EvalGrader{Name: "human", Kind: "human", Config: []byte(humanConfig), Required: true, CreatedBy: evalUser})
	if err != nil {
		t.Fatal(err)
	}
	f.llm, _, err = s.UpsertEvalGrader(ctx, d.ID, EvalGrader{Name: "judge", Kind: "llm", Config: []byte(llmCfg), CreatedBy: evalUser})
	if err != nil {
		t.Fatal(err)
	}
	f.e, err = s.CreateEvalExperiment(ctx, EvalExperiment{DatasetID: d.ID, Arms: twoArms(), BudgetUSD: 5, CreatedBy: evalUser}, EvalExperimentLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		for _, arm := range []string{"base", "cand"} {
			cid := c.ID
			res := EvalResult{CaseID: &cid, Arm: arm, Phase: "Succeeded", Output: "answer from " + arm, CostUSD: 0.01, CreatedBy: "eval-runner"}
			judgeOK := EvalGrade{GraderID: f.llm.ID, Score: 0.9, Pass: true, JudgeCostUSD: 0.002, Detail: "fine"}
			pending := EvalGrade{GraderID: f.human.ID, Status: GradePendingReview, Detail: "waiting for a reviewer"}
			ok, err := s.RecordEvalRunResult(ctx, f.e.ID, res, []EvalGrade{judgeOK, pending})
			if err != nil || !ok {
				t.Fatalf("recording %s/%d: %v %v", arm, i, ok, err)
			}
		}
	}
	return f
}

func TestEvalReviewFlowAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	ctx := WithTenant(context.Background(), tenantA)
	f := reviewSetup(ctx, t, s, "review", humanCfg)

	// Judge cost is counted in the experiment's spend with the run cost.
	if got, _ := s.GetEvalExperiment(ctx, f.e.ID); got.SpentUSD < 0.0479 || got.SpentUSD > 0.0481 {
		t.Errorf("spent = %v, want 4 runs of 0.01 and 4 judge calls of 0.002", got.SpentUSD)
	}
	if n, err := s.PendingEvalReviews(ctx, f.e.ID); err != nil || n != 0 {
		t.Fatalf("pending before the stage opens = %d, %v", n, err)
	}
	pending, err := s.PrepareEvalReview(ctx, f.e.ID)
	if err != nil || pending != 4 {
		t.Fatalf("PrepareEvalReview = %d, %v; want 4", pending, err)
	}
	if again, err := s.PrepareEvalReview(ctx, f.e.ID); err != nil || again != 4 {
		t.Errorf("a second PrepareEvalReview = %d, %v; want 4", again, err)
	}
	if err := s.AwaitEvalReview(ctx, f.e.ID, json.RawMessage(`{"results":4}`)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetEvalExperiment(ctx, f.e.ID); got.Status != ExperimentReview {
		t.Fatalf("status = %s", got.Status)
	}
	items := checkBlindQueue(t, s, ctx, f)
	checkLabels(t, s, ctx, f, items)
	checkLabelledResults(t, s, ctx, f)

	// Closing leaves the other two grades pending_review, never a pass.
	if n, err := s.CloseEvalReview(ctx, f.e.ID); err != nil || n != 2 {
		t.Fatalf("close = %d, %v", n, err)
	}
	if n, _ := s.PendingEvalReviews(ctx, f.e.ID); n != 0 {
		t.Errorf("pending after close = %d", n)
	}
	rows, err := s.EvalSummaryRows(ctx, f.e.ID)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]int{}
	for _, row := range rows {
		for _, g := range row.Grades {
			statuses[g.Status]++
		}
	}
	if statuses["pending_review"] != 2 || statuses["ok"] != 6 {
		t.Errorf("summary grade statuses = %v", statuses)
	}
	if err := s.FinishEvalExperiment(ctx, f.e.ID, ExperimentComplete, json.RawMessage(`{}`), ""); err != nil {
		t.Errorf("an experiment in review ends: %v", err)
	}
	if _, err := s.CloseEvalReview(ctx, f.e.ID); !errors.Is(err, ErrExperimentFinished) {
		t.Errorf("close of an ended experiment: %v", err)
	}
}

// checkBlindQueue reads the queue and checks nothing in it names an arm.
func checkBlindQueue(t *testing.T, s *Store, ctx context.Context, f reviewFixture) []EvalReviewItem {
	t.Helper()
	items, err := s.ListEvalReviews(ctx, f.e.ID, EvalReviewQuery{Status: ReviewPending})
	if err != nil || len(items) != 4 {
		t.Fatalf("queue = %d, %v", len(items), err)
	}
	raw, _ := json.Marshal(items)
	text := strings.ReplaceAll(string(raw), "answer from", "answer")
	for _, leak := range []string{`"arm"`, `"base"`, `"cand"`, `"repeat"`, `"runName"`, `"costUsd"`, `"toolCalls"`, `"resultId"`, "from base", "from cand"} {
		if strings.Contains(text, leak) {
			t.Errorf("a review item shows %s: %s", leak, raw)
		}
	}
	if items[0].Scale != "passfail" || items[0].Instructions != "is it right?" || items[0].Input == "" {
		t.Errorf("item = %+v", items[0])
	}
	return items
}

// checkLabels labels two items and the refusals around labelling.
func checkLabels(t *testing.T, s *Store, ctx context.Context, f reviewFixture, items []EvalReviewItem) {
	t.Helper()
	yes, no := true, false
	done, err := s.SubmitEvalReview(ctx, items[0].ID, EvalReviewLabel{Pass: &yes, Comment: "good"}, "rita")
	if err != nil || done.Status != ReviewDone || done.Pass == nil || !*done.Pass || done.Reviewer != "rita" {
		t.Fatalf("submit = %+v, %v", done, err)
	}
	if _, err := s.SubmitEvalReview(ctx, items[0].ID, EvalReviewLabel{Pass: &no}, "sam"); !errors.Is(err, ErrReviewClosed) {
		t.Errorf("a second label: %v", err)
	}
	if _, err := s.SubmitEvalReview(ctx, items[1].ID, EvalReviewLabel{Stars: 3}, "rita"); !errors.Is(err, ErrInvalidEval) {
		t.Errorf("stars on a passfail scale: %v", err)
	}
	if _, err := s.SubmitEvalReview(ctx, items[1].ID, EvalReviewLabel{Pass: &no, Comment: strings.Repeat("x", MaxReviewComment+1)}, "rita"); !errors.Is(err, ErrInvalidEval) {
		t.Errorf("a long comment: %v", err)
	}
	if _, err := s.SubmitEvalReview(ctx, items[1].ID, EvalReviewLabel{Pass: &no}, ""); !errors.Is(err, ErrInvalidEval) {
		t.Errorf("no reviewer: %v", err)
	}
	if _, err := s.SubmitEvalReview(ctx, items[1].ID, EvalReviewLabel{Pass: &no}, "rita"); err != nil {
		t.Fatal(err)
	}
	progress, _ := s.EvalReviewProgressOf(ctx, f.e.ID)
	if progress != (EvalReviewProgress{Pending: 2, Done: 2}) {
		t.Errorf("progress = %+v", progress)
	}
}

// checkLabelledResults checks the labels reached the results: a result whose
// required human grade failed is not a case pass, one still pending is not either.
func checkLabelledResults(t *testing.T, s *Store, ctx context.Context, f reviewFixture) {
	t.Helper()
	results, err := s.ListEvalExperimentResults(ctx, f.e.ID, EvalExperimentResultQuery{})
	if err != nil || len(results) != 4 {
		t.Fatal(len(results), err)
	}
	var passed, failed, pendingCases int
	for _, r := range results {
		for _, g := range r.Grades {
			if g.Kind != graders.KindHuman {
				continue
			}
			switch g.Status {
			case GradePendingReview:
				pendingCases++
				if r.CasePass {
					t.Errorf("result %d passes with a required grade pending", r.ID)
				}
			case GradeOK:
				if g.GradedBy != "rita" {
					t.Errorf("graded by %q", g.GradedBy)
				}
				if r.CasePass {
					passed++
				} else {
					failed++
				}
			}
		}
	}
	if passed != 1 || failed != 1 || pendingCases != 2 {
		t.Errorf("passed %d failed %d pending %d", passed, failed, pendingCases)
	}
}

func TestEvalReviewSampleAndScale(t *testing.T) {
	s, _ := appStore(t)
	ctx := WithTenant(context.Background(), tenantA)
	f := reviewSetup(ctx, t, s, "sample", `{"scale":"1to5","passAt":0.75,"sample":1}`)
	pending, err := s.PrepareEvalReview(ctx, f.e.ID)
	if err != nil {
		t.Fatal(err)
	}
	// One case of two is sampled: both arms' results of it are reviewed, the
	// other case's results have no human grade at all.
	if pending != 2 {
		t.Fatalf("pending = %d, want the 2 results of 1 sampled case", pending)
	}
	if again, _ := s.PrepareEvalReview(ctx, f.e.ID); again != 2 {
		t.Errorf("repeat changed the sample: %d", again)
	}
	results, _ := s.ListEvalExperimentResults(ctx, f.e.ID, EvalExperimentResultQuery{})
	humanGrades := 0
	for _, r := range results {
		for _, g := range r.Grades {
			if g.Kind == "human" {
				humanGrades++
			}
		}
		// A result left out of the sample has no human grade, so the case rule
		// runs on the grades it has.
		if len(r.Grades) == 1 && !r.CasePass {
			t.Errorf("a result left out of the sample fails on a good llm grade: %+v", r)
		}
	}
	if humanGrades != 2 {
		t.Errorf("human grades = %d", humanGrades)
	}
	items, _ := s.ListEvalReviews(ctx, f.e.ID, EvalReviewQuery{})
	if items[0].Scale != "1to5" || items[0].PassAt != 0.75 {
		t.Fatalf("item = %+v", items[0])
	}
	if _, err := s.SubmitEvalReview(ctx, items[0].ID, EvalReviewLabel{Stars: 6}, "rita"); !errors.Is(err, ErrInvalidEval) {
		t.Errorf("6 stars: %v", err)
	}
	done, err := s.SubmitEvalReview(ctx, items[0].ID, EvalReviewLabel{Stars: 4}, "rita")
	if err != nil || done.Score == nil || *done.Score != 0.75 || done.Pass == nil || !*done.Pass {
		t.Fatalf("4 stars = %+v, %v", done, err)
	}
	done, err = s.SubmitEvalReview(ctx, items[1].ID, EvalReviewLabel{Stars: 3}, "rita")
	if err != nil || done.Pass == nil || *done.Pass {
		t.Fatalf("3 stars = %+v, %v", done, err)
	}
}

func TestEvalJudgeSpendAndCalibrationPairs(t *testing.T) {
	s, _ := appStore(t)
	ctx := WithTenant(context.Background(), tenantA)
	f := reviewSetup(ctx, t, s, "calib", humanCfg)
	if err := s.AddEvalSpend(ctx, f.e.ID, 0.05); err != nil {
		t.Fatal(err)
	}
	if err := s.AddEvalSpend(ctx, f.e.ID, -1); !errors.Is(err, ErrInvalidEval) {
		t.Errorf("negative spend: %v", err)
	}
	since := time.Now().Add(-time.Hour)
	if got, err := s.EvalJudgeSpendSince(ctx, since); err != nil || got < 0.0579 || got > 0.0581 {
		t.Errorf("judge spend since = %v, %v; want 4 x 0.002 + 0.05", got, err)
	}
	if got, _ := s.EvalJudgeSpendSince(ctx, time.Now().Add(time.Hour)); got != 0 {
		t.Errorf("judge spend in the future = %v", got)
	}
	if got, _ := s.EvalJudgeSpendSince(WithTenant(context.Background(), tenantB), since); got != 0 {
		t.Errorf("B sees A's judge spend: %v", got)
	}

	if _, err := s.PrepareEvalReview(ctx, f.e.ID); err != nil {
		t.Fatal(err)
	}
	items, _ := s.ListEvalReviews(ctx, f.e.ID, EvalReviewQuery{})
	yes := true
	for _, it := range items {
		if _, err := s.SubmitEvalReview(ctx, it.ID, EvalReviewLabel{Pass: &yes}, "rita"); err != nil {
			t.Fatal(err)
		}
	}
	pairs, err := s.EvalCalibrationPairs(ctx, f.d.ID, "judge", "human", 0)
	if err != nil || len(pairs) != 4 {
		t.Fatalf("pairs = %d, %v", len(pairs), err)
	}
	if p := pairs[0]; !p.LLMPass || !p.HumanPass || p.LLMScore < 0.899 || p.LLMScore > 0.901 || p.HumanScore != 1 {
		t.Errorf("pair = %+v", p)
	}
	if _, err := s.EvalCalibrationPairs(ctx, f.d.ID, "judge", "judge", 0); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("same grader twice: %v", err)
	}
	if _, err := s.EvalCalibrationPairs(ctx, f.d.ID, "nope", "human", 0); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("unknown grader: %v", err)
	}
	if none, err := s.EvalCalibrationPairs(ctx, f.d.ID, "judge", "human", f.e.ID+999); err != nil || len(none) != 0 {
		t.Errorf("other experiment: %d, %v", len(none), err)
	}
	if _, err := s.EvalCalibrationPairs(WithTenant(context.Background(), tenantB), f.d.ID, "judge", "human", 0); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("B reads A's pairs: %v", err)
	}
}

func TestEvalReviewTenantIsolation(t *testing.T) {
	s, raw := appStore(t)
	base := context.Background()
	a, b, plat := WithTenant(base, tenantA), WithTenant(base, tenantB), WithPlatform(base)
	fa := reviewSetup(a, t, s, "iso-review", humanCfg)
	fb := reviewSetup(b, t, s, "iso-review", humanCfg)
	for _, x := range []struct {
		ctx context.Context
		id  int64
	}{{a, fa.e.ID}, {b, fb.e.ID}} {
		if _, err := s.PrepareEvalReview(x.ctx, x.id); err != nil {
			t.Fatal(err)
		}
	}
	itemsA, _ := s.ListEvalReviews(a, fa.e.ID, EvalReviewQuery{})
	itemsB, _ := s.ListEvalReviews(b, fb.e.ID, EvalReviewQuery{})
	if len(itemsA) != 4 || len(itemsB) != 4 {
		t.Fatalf("items %d %d", len(itemsA), len(itemsB))
	}
	yes := true
	if _, err := s.ListEvalReviews(a, fb.e.ID, EvalReviewQuery{}); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A lists B's queue: %v", err)
	}
	if _, err := s.GetEvalReview(a, itemsB[0].ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A reads B's item: %v", err)
	}
	if _, err := s.SubmitEvalReview(a, itemsB[0].ID, EvalReviewLabel{Pass: &yes}, "mallory"); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A labels B's item: %v", err)
	}
	if _, err := s.EvalReviewProgressOf(a, fb.e.ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A reads B's progress: %v", err)
	}
	if n, err := s.PendingEvalReviews(a, fb.e.ID); err != nil || n != 0 {
		t.Errorf("A counts B's pending = %d, %v", n, err)
	}
	if _, err := s.PrepareEvalReview(a, fb.e.ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A opens B's review: %v", err)
	}
	if _, err := s.CloseEvalReview(a, fb.e.ID); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A closes B's review: %v", err)
	}
	if err := s.AddEvalSpend(a, fb.e.ID, 1); !errors.Is(err, ErrEvalNotFound) {
		t.Errorf("A adds spend to B's experiment: %v", err)
	}
	if still, _ := s.GetEvalReview(b, itemsB[0].ID); still.Status != ReviewPending {
		t.Errorf("B's item changed: %+v", still)
	}
	var n int
	if err := raw.QueryRowContext(base, `SELECT count(*) FROM eval_review_assignments`).Scan(&n); err != nil || n != 0 {
		t.Errorf("an unset tenant setting returned %d assignments, %v; want 0", n, err)
	}
	var forced int
	if err := raw.QueryRowContext(base, `SELECT count(*) FROM pg_class WHERE relname = 'eval_review_assignments' AND relrowsecurity AND relforcerowsecurity`).Scan(&forced); err != nil || forced != 1 {
		t.Errorf("eval_review_assignments ENABLE and FORCE ROW LEVEL SECURITY = %d, %v", forced, err)
	}
	// The database refuses what the application would never write.
	var resultB int64
	if err := s.scoped(plat, func(tx dbtx, _ string) error {
		return tx.QueryRowContext(base, `SELECT id FROM eval_results WHERE experiment_id = $1 LIMIT 1`, fb.e.ID).Scan(&resultB)
	}); err != nil {
		t.Fatal(err)
	}
	refused := map[string]struct {
		scope context.Context
		query string
		args  []any
	}{
		"RLS: an assignment for B written by A": {a,
			`INSERT INTO eval_review_assignments (tenant_id, experiment_id, result_id, grader_id, order_key) VALUES ($1, $2, $3, $4, 1)`,
			[]any{tenantB, fb.e.ID, resultB, fb.human.ID}},
		"composite key: an assignment under another tenant than its experiment's": {plat,
			`INSERT INTO eval_review_assignments (tenant_id, experiment_id, result_id, grader_id, order_key) VALUES ($1, $2, $3, $4, 1)`,
			[]any{tenantA, fb.e.ID, resultB, fb.human.ID}},
		"composite key: an assignment of A's experiment over B's result": {plat,
			`INSERT INTO eval_review_assignments (tenant_id, experiment_id, result_id, grader_id, order_key) VALUES ($1, $2, $3, $4, 1)`,
			[]any{tenantA, fa.e.ID, resultB, fa.human.ID}},
		"an unknown status": {plat,
			`UPDATE eval_review_assignments SET status = 'maybe' WHERE experiment_id = $1`, []any{fa.e.ID}},
		"an unknown grade status": {plat,
			`UPDATE eval_grades SET status = 'great' WHERE tenant_id = $1`, []any{tenantA}},
		"a negative judge cost": {plat,
			`UPDATE eval_grades SET judge_cost_usd = -1 WHERE tenant_id = $1`, []any{tenantA}},
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
	// A cannot update B's grades through raw SQL either.
	var changed int64
	if err := s.scoped(a, func(tx dbtx, _ string) error {
		res, err := tx.ExecContext(base, `UPDATE eval_grades SET status = 'error' WHERE tenant_id = $1`, tenantB)
		if err == nil {
			changed, _ = res.RowsAffected()
		}
		return err
	}); err != nil || changed != 0 {
		t.Errorf("A updated %d of B's grades, %v", changed, err)
	}
}
