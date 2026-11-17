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

package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/heddles/agent-orca/internal/evals/graders"
	"github.com/heddles/agent-orca/internal/evals/judge"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// JudgeResolver finds the provider an llm grader names. judge.KubeResolver is the
// real one.
type JudgeResolver interface {
	Resolve(ctx context.Context, caller tenancy.Caller, namespace, ref string) (judge.Provider, error)
}

// judgeEntry is a resolved judge, or the reason there is none.
type judgeEntry struct {
	j   *judge.Judge
	cfg graders.LLMConfig
	err error
}

const (
	detailPendingReview = "waiting for a reviewer"
	detailNoJudge       = "no judge is configured on this operator"
)

func callerOf(e postgresql.EvalExperiment, tenantName string) tenancy.Caller {
	return tenancy.Caller{TenantID: e.TenantID, TenantName: tenantName, Namespaces: []string{e.Namespace}}
}

// guard is the budget check of a judge call: the experiment's budget and the
// tenant's daily budget must both have room for the spend the call may add.
func (st *state) guard() func(pending float64) bool {
	return func(pending float64) bool {
		if st.spent+st.unrecorded+pending > st.e.BudgetUSD {
			return false
		}
		return !st.dailyLimited || pending <= st.dailyLeft
	}
}

// judgeFor returns the judge of an llm grader, made once per tick.
func (r *Runner) judgeFor(ctx context.Context, st *state, g postgresql.EvalGrader) judgeEntry {
	if je, ok := st.judges[g.ID]; ok {
		return je
	}
	je := r.newJudge(ctx, st.e, st.lim.Name, g, st.guard())
	st.judges[g.ID] = je
	return je
}

func (r *Runner) newJudge(ctx context.Context, e postgresql.EvalExperiment, tenantName string, g postgresql.EvalGrader, guard func(float64) bool) judgeEntry {
	cfg, err := graders.ParseLLM(g.Config)
	if err != nil {
		return judgeEntry{err: err}
	}
	if r.cfg.Judges == nil {
		return judgeEntry{cfg: cfg, err: errors.New(detailNoJudge)}
	}
	p, err := r.cfg.Judges.Resolve(ctx, callerOf(e, tenantName), e.Namespace, cfg.ProviderRef)
	if err != nil {
		return judgeEntry{cfg: cfg, err: err}
	}
	return judgeEntry{cfg: cfg, j: &judge.Judge{Provider: p, Config: cfg, Guard: guard}}
}

func errorGrade(g postgresql.EvalGrader, format string, a ...any) postgresql.EvalGrade {
	return postgresql.EvalGrade{GraderID: g.ID, Status: postgresql.GradeError, Detail: judge.CleanRationale(fmt.Sprintf(format, a...))}
}

// gradeLLM runs one llm grader (reference or rubric mode) on one output. The
// pairwise mode is graded when the experiment's runs are all in (gradePairs).
func (r *Runner) gradeLLM(ctx context.Context, st *state, g postgresql.EvalGrader, c postgresql.EvalCase, res postgresql.EvalResult) postgresql.EvalGrade {
	je := r.judgeFor(ctx, st, g)
	if je.err != nil {
		return errorGrade(g, "the judge cannot be used: %v", je.err)
	}
	if je.cfg.Mode == graders.ModeReference && c.Reference == "" {
		return postgresql.EvalGrade{GraderID: g.ID, NotGraded: true, Detail: "the case has no reference to compare with"}
	}
	o := je.j.Grade(ctx, judge.Input{Case: c.Input, Reference: c.Reference, Output: res.Output})
	return gradeOf(g, o)
}

// gradeOf turns a judge outcome into a stored grade.
func gradeOf(g postgresql.EvalGrader, o judge.Outcome) postgresql.EvalGrade {
	eg := postgresql.EvalGrade{GraderID: g.ID, Status: o.Status, Detail: o.Rationale, JudgeCostUSD: o.CostUSD, Inconsistent: o.Inconsistent}
	if o.Status == judge.StatusOK {
		eg.Score, eg.Pass = o.Score, o.Pass
	}
	return eg
}

// gradeRun grades one finished run with every grader of the dataset that can be
// graded now, and works out the case verdict. A human grader gets a pending
// grade; a pairwise llm grader gets none until gradePairs. The tool graders read
// the run's trace: when it is not known their grade is an error, never a pass.
func (r *Runner) gradeRun(ctx context.Context, st *state, c postgresql.EvalCase, res postgresql.EvalResult, tr runTrace) ([]postgresql.EvalGrade, bool) {
	in := graders.Input{
		Output: res.Output, Reference: c.Reference,
		Trace: graders.Trace{ToolCalls: tr.ToolCalls, CostUSD: res.CostUSD, LatencyMs: res.LatencyMs},
	}
	grades := make([]postgresql.EvalGrade, 0, len(st.graders))
	graded := make([]graders.Graded, 0, len(st.graders))
	for _, g := range st.graders {
		var eg postgresql.EvalGrade
		switch g.Kind {
		case graders.KindHuman:
			eg = postgresql.EvalGrade{GraderID: g.ID, Status: postgresql.GradePendingReview, Detail: detailPendingReview}
		case graders.KindLLM:
			if cfg, err := graders.ParseLLM(g.Config); err == nil && cfg.Mode == graders.ModePairwise {
				continue
			}
			eg = r.gradeLLM(ctx, st, g, c, res)
		case graders.KindToolCalled, graders.KindToolNotCalled:
			if !tr.Known {
				eg = traceGrade(g)
				break
			}
			fallthrough
		default:
			out := graders.Grade(g.Spec(), in)
			eg = postgresql.EvalGrade{GraderID: g.ID, Score: out.Score, Pass: out.Pass, NotGraded: out.NotGraded, Detail: out.Detail}
		}
		eg.Status = statusOf(eg)
		grades = append(grades, eg)
		graded = append(graded, graders.GradedFor(eg.Status,
			graders.Result{Pass: eg.Pass, Score: eg.Score, NotGraded: eg.NotGraded, Detail: eg.Detail}, g.Weight, g.Required))
	}
	return grades, len(graded) > 0 && graders.CasePass(graded)
}

func statusOf(g postgresql.EvalGrade) string {
	if g.Status == "" {
		return postgresql.GradeOK
	}
	return g.Status
}

// judgeCostOf sums what the judge cost over grades.
func judgeCostOf(grades []postgresql.EvalGrade) float64 {
	var sum float64
	for _, g := range grades {
		sum += g.JudgeCostUSD
	}
	return sum
}

// ── the canary, at start ────────────────────────────────────────────────────

// checkJudges runs each llm grader's canary check, once per experiment, and
// refuses a pairwise grader in an experiment that has not exactly two arms. A
// judge that cannot be used or that misgrades a known answer fails the experiment
// before a single run is made. What the canary cost is added to the experiment's
// spend. A failure is an experimentError; any other error is worth a retry.
func (r *Runner) checkJudges(ctx context.Context, e postgresql.EvalExperiment, lim tenantLimits) error {
	gs, err := r.store.ListEvalGraders(ctx, e.DatasetID)
	if err != nil {
		return err
	}
	for _, g := range gs {
		if g.Kind != graders.KindLLM {
			continue
		}
		key := fmt.Sprintf("%d/%d", e.ID, g.ID)
		if _, done := r.canaryOK.Load(key); done {
			continue
		}
		cfg, err := graders.ParseLLM(g.Config)
		if err != nil {
			return failf("grader %q: %v", g.Name, err)
		}
		if cfg.Mode == graders.ModePairwise && len(e.Arms) != 2 {
			return failf("grader %q compares two arms, and the experiment has %d", g.Name, len(e.Arms))
		}
		var spent float64
		guard := func(pending float64) bool {
			if spent+pending > e.BudgetUSD {
				return false
			}
			return lim.DailyUSD <= 0 || pending <= lim.DailyUSD
		}
		je := r.newJudge(ctx, e, lim.Name, g, guard)
		if je.err != nil {
			return failf("grader %q: the judge cannot be used: %v", g.Name, je.err)
		}
		cost, cerr := je.j.Canary(ctx)
		spent += cost
		if cost > 0 {
			if err := r.store.AddEvalSpend(ctx, e.ID, cost); err != nil {
				return err
			}
		}
		if cerr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return failf("grader %q: %v", g.Name, cerr)
		}
		r.canaryOK.Store(key, true)
	}
	return nil
}

// ── pairwise grading, when the runs are in ──────────────────────────────────

const resultsPage = 200

// pairKey identifies the two results a pairwise grader compares.
type pairKey struct {
	caseID int64
	repeat int
}

// gradePairs grades the pairwise llm graders of a two-arm experiment: for each
// (case, repeat) whose two arms both produced an output, the judge compares them
// (asked twice with the outputs swapped), and each result gets its grade, 1 for
// the winner, 0 for the loser, 0.5 each for a tie. A result whose partner has no
// output is not graded by it. It does nothing when the dataset has no pairwise
// grader.
func (r *Runner) gradePairs(ctx context.Context, st *state) error {
	var pairwise []postgresql.EvalGrader
	for _, g := range st.graders {
		if g.Kind != graders.KindLLM {
			continue
		}
		if cfg, err := graders.ParseLLM(g.Config); err == nil && cfg.Mode == graders.ModePairwise {
			pairwise = append(pairwise, g)
		}
	}
	if len(pairwise) == 0 {
		return nil
	}
	results, err := r.allResults(ctx, st.e.ID)
	if err != nil {
		return err
	}
	byPair := map[pairKey]map[string]postgresql.EvalResult{}
	for _, res := range results {
		if res.CaseID == nil {
			continue
		}
		k := pairKey{*res.CaseID, res.Repeat}
		if byPair[k] == nil {
			byPair[k] = map[string]postgresql.EvalResult{}
		}
		byPair[k][res.Arm] = res
	}
	for _, g := range pairwise {
		if err := r.gradePairsFor(ctx, st, g, byPair); err != nil {
			return err
		}
	}
	return nil
}

func hasGrade(res postgresql.EvalResult, graderID int64) bool {
	for _, g := range res.Grades {
		if g.GraderID == graderID {
			return true
		}
	}
	return false
}

func (r *Runner) gradePairsFor(ctx context.Context, st *state, g postgresql.EvalGrader, byPair map[pairKey]map[string]postgresql.EvalResult) error {
	e := st.e
	je := r.judgeFor(ctx, st, g)
	for _, c := range st.cases {
		for rep := 0; rep < e.Repeats; rep++ {
			pair := byPair[pairKey{c.ID, rep}]
			var a, b postgresql.EvalResult
			var okA, okB bool
			if len(e.Arms) == 2 {
				a, okA = pair[e.Arms[0].Label]
				b, okB = pair[e.Arms[1].Label]
			}
			if (okA && hasGrade(a, g.ID)) && (okB && hasGrade(b, g.ID)) {
				continue
			}
			var ga, gb postgresql.EvalGrade
			switch {
			case len(e.Arms) != 2:
				ga = errorGrade(g, "a pairwise grader compares two arms")
				gb = ga
			case !okA || !okB || a.Phase != phaseSucceeded || b.Phase != phaseSucceeded:
				ga = postgresql.EvalGrade{GraderID: g.ID, NotGraded: true, Detail: "the other arm has no output for this case"}
				gb = ga
			case je.err != nil:
				ga = errorGrade(g, "the judge cannot be used: %v", je.err)
				gb = ga
			default:
				o := je.j.Pair(ctx, c.Input, c.Reference, a.Output, b.Output)
				ga, gb = gradeOf(g, o.A), gradeOf(g, o.B)
				st.spent += o.A.CostUSD + o.B.CostUSD
				if st.dailyLimited {
					st.dailyLeft -= o.A.CostUSD + o.B.CostUSD
				}
			}
			// The first arm's grade carries the pair's judge cost, the second's is free.
			for _, w := range []struct {
				ok  bool
				res postgresql.EvalResult
				g   postgresql.EvalGrade
			}{{okA, a, ga}, {okB, b, gb}} {
				if !w.ok || w.res.Phase != phaseSucceeded || hasGrade(w.res, g.ID) {
					continue
				}
				w.g.Status = statusOf(w.g)
				if err := r.store.SaveEvalGrades(ctx, e.ID, w.res.ID, []postgresql.EvalGrade{w.g}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// allResults reads every result of an experiment, a page at a time.
func (r *Runner) allResults(ctx context.Context, id int64) ([]postgresql.EvalResult, error) {
	var out []postgresql.EvalResult
	var after int64
	for {
		page, err := r.store.ListEvalExperimentResults(ctx, id, postgresql.EvalExperimentResultQuery{After: after, Limit: resultsPage})
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < resultsPage {
			return out, nil
		}
		after = page[len(page)-1].ID
	}
}

// ── the review stage ────────────────────────────────────────────────────────

// reviewTick ends an experiment that was waiting for human labels once none is
// pending (all given, or the review was closed). Until then it only keeps the
// lease. The summary is made again, with the labels in.
func (r *Runner) reviewTick(ctx context.Context, e postgresql.EvalExperiment) error {
	n, err := r.store.PendingEvalReviews(ctx, e.ID)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	stopped := stoppedByOf(e.Summary)
	sum, err := r.summarise(ctx, e, e.SpentUSD, stopped)
	if err != nil {
		return err
	}
	return r.finish(ctx, e, postgresql.ExperimentComplete, sum, "")
}
