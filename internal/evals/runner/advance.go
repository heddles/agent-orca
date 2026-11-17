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
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/evals/compare"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// labelAttempt numbers the tries of one slot: 0, or 1 after a platform failure.
const labelAttempt = "agentorca.io/eval-attempt"

// annotationCreatedBy names who started the run, as the UI API does.
const annotationCreatedBy = "agentorca.io/created-by"

// Phases of a result, as in eval_results.phase.
const (
	phaseSucceeded = compare.PhaseSucceeded
	phaseFailed    = compare.PhaseFailed
	phaseTimedOut  = compare.PhaseTimedOut
	phaseCancelled = compare.PhaseCancelled
	phaseSkipped   = compare.PhaseSkipped
)

// state is what one tick sees of an experiment.
type state struct {
	e       postgresql.EvalExperiment
	cases   []postgresql.EvalCase
	caseBy  map[int64]postgresql.EvalCase
	done    map[slot]string
	runs    map[slot][]*agentorcav1alpha1.AgentRun
	graders []postgresql.EvalGrader
	// spent is what the experiment has spent, the recorded results included.
	spent float64
}

func isTerminal(run *agentorcav1alpha1.AgentRun) bool {
	switch run.Status.Phase {
	case agentorcav1alpha1.AgentRunPhaseSucceeded, agentorcav1alpha1.AgentRunPhaseFailed, agentorcav1alpha1.AgentRunPhaseHandedOff:
		return true
	}
	return false
}

func attemptOf(run *agentorcav1alpha1.AgentRun) int {
	n, _ := strconv.Atoi(run.Labels[labelAttempt])
	return n
}

func slotOf(run *agentorcav1alpha1.AgentRun) (slot, bool) {
	c, err := strconv.ParseInt(run.Labels[tenancy.LabelEvalCase], 10, 64)
	if err != nil {
		return slot{}, false
	}
	rep, err := strconv.Atoi(run.Labels[tenancy.LabelEvalRepeat])
	if err != nil {
		return slot{}, false
	}
	arm := run.Labels[tenancy.LabelEvalArm]
	return slot{caseID: c, arm: arm, repeat: rep}, arm != ""
}

func (r *Runner) observe(ctx context.Context, e postgresql.EvalExperiment) (*state, error) {
	cases, err := r.store.ListEvalExperimentCases(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	keys, err := r.store.ListEvalExperimentResultKeys(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	gs, err := r.store.ListEvalGraders(ctx, e.DatasetID)
	if err != nil {
		return nil, err
	}
	var list agentorcav1alpha1.AgentRunList
	if err := r.kube.List(ctx, &list, client.InNamespace(e.Namespace),
		client.MatchingLabels{tenancy.LabelEvalExperiment: tenancy.ExperimentLabel(e.ID), tenancy.LabelTenantID: e.TenantID}); err != nil {
		return nil, err
	}
	st := &state{e: e, cases: cases, caseBy: map[int64]postgresql.EvalCase{}, done: map[slot]string{},
		runs: map[slot][]*agentorcav1alpha1.AgentRun{}, graders: gs, spent: e.SpentUSD}
	for _, c := range cases {
		st.caseBy[c.ID] = c
	}
	for _, k := range keys {
		st.done[slot{k.CaseID, k.Arm, k.Repeat}] = k.Phase
	}
	for i := range list.Items {
		run := &list.Items[i]
		if s, ok := slotOf(run); ok {
			st.runs[s] = append(st.runs[s], run)
		}
	}
	for s := range st.runs {
		rs := st.runs[s]
		sort.Slice(rs, func(i, j int) bool { return attemptOf(rs[i]) < attemptOf(rs[j]) })
	}
	return st, nil
}

// latest is the newest run of a slot, nil when it has none.
func (st *state) latest(s slot) *agentorcav1alpha1.AgentRun {
	rs := st.runs[s]
	if len(rs) == 0 {
		return nil
	}
	return rs[len(rs)-1]
}

// slots lists the work in the order it is started: by case, then repeat, then
// arm, so every arm gets the same case before the next case starts and an
// experiment that stops early still has paired data.
func (st *state) slots() []slot {
	out := make([]slot, 0, len(st.cases)*st.e.Repeats*len(st.e.Arms))
	for _, c := range st.cases {
		for rep := 0; rep < st.e.Repeats; rep++ {
			for _, a := range st.e.Arms {
				out = append(out, slot{c.ID, a.Label, rep})
			}
		}
	}
	return out
}

// advance is one step of a running experiment: record the runs that finished,
// start runs while there is room, and end the experiment when it is over.
func (r *Runner) advance(ctx context.Context, e postgresql.EvalExperiment, lim tenantLimits) error {
	if err := r.ensureArmObjects(ctx, e, lim.Name); err != nil {
		return r.failOrRetry(ctx, e, err)
	}
	st, err := r.observe(ctx, e)
	if err != nil {
		return err
	}
	inflight, committed, err := r.ingest(ctx, st, false)
	if err != nil {
		return err
	}
	all := st.slots()
	remaining := 0
	for _, s := range all {
		if _, ok := st.done[s]; !ok {
			remaining++
		}
	}
	if remaining == 0 {
		return r.complete(ctx, st, "")
	}
	stopped, err := r.start(ctx, st, lim, all, inflight, committed)
	if err != nil {
		return err
	}
	if stopped != "" && inflight == 0 {
		return r.complete(ctx, st, stopped)
	}
	return nil
}

func (r *Runner) failOrRetry(ctx context.Context, e postgresql.EvalExperiment, err error) error {
	if fatal, ok := err.(*experimentError); ok {
		r.cleanup(ctx, e)
		return r.finish(ctx, e, postgresql.ExperimentFailed, nil, fatal.Error())
	}
	return err
}

// ingest records the runs that are over. It returns how many are still in
// flight and the caps they hold. With cancelling, a run that is not over is
// recorded Cancelled and deleted.
func (r *Runner) ingest(ctx context.Context, st *state, cancelling bool) (inflight int, committed float64, err error) {
	for _, s := range st.slots() {
		if _, ok := st.done[s]; ok {
			continue
		}
		run := st.latest(s)
		if run == nil {
			continue
		}
		switch {
		case !isTerminal(run) && cancelling:
			if err := r.record(ctx, st, s, run, phaseCancelled, "the experiment was cancelled"); err != nil {
				return 0, 0, err
			}
			if err := r.kube.Delete(ctx, run); client.IgnoreNotFound(err) != nil {
				return 0, 0, err
			}
		case !isTerminal(run):
			inflight++
			committed += runCapOf(run)
		default:
			phase, reason := phaseOf(run)
			if phase == phaseFailed && platformFailure(reason) {
				if cancelling || attemptOf(run) >= 1 {
					phase = phaseSkipped
					reason = "the platform could not run this case twice: " + reason
				} else {
					// A retry is made by start; this slot stays open.
					continue
				}
			}
			if err := r.record(ctx, st, s, run, phase, reason); err != nil {
				return 0, 0, err
			}
		}
	}
	return inflight, committed, nil
}

func runCapOf(run *agentorcav1alpha1.AgentRun) float64 {
	if run.Spec.Budget == nil {
		return 0
	}
	v, _ := strconv.ParseFloat(run.Spec.Budget.MaxUSD, 64)
	return v
}

func parseUSD(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// phaseOf maps a finished run to a result phase and the reason it failed.
func phaseOf(run *agentorcav1alpha1.AgentRun) (phase, reason string) {
	switch run.Status.Phase {
	case agentorcav1alpha1.AgentRunPhaseSucceeded, agentorcav1alpha1.AgentRunPhaseHandedOff:
		return phaseSucceeded, ""
	}
	reason = run.Status.FailureReason
	l := strings.ToLower(reason)
	if strings.Contains(l, "deadline") || strings.Contains(l, "timed out") || strings.Contains(l, "timeout") {
		return phaseTimedOut, reason
	}
	return phaseFailed, reason
}

// platformMarkers are failure reasons that say the platform, not the agent,
// could not run the case: a pod that was not scheduled or was evicted, a router
// that could not be built, a sandbox that went away.
var platformMarkers = []string{
	"pod not found", "sandbox", "resolving ", "building router config", "owner ref",
	"evict", "unschedulable", "preempt", "init container", "lost before", "missing from the workflow",
}

// platformFailure reports whether a failure is the platform's. A failing agent
// is data and is not retried; a platform failure is retried once and then the
// case is skipped in every arm.
func platformFailure(reason string) bool {
	l := strings.ToLower(reason)
	for _, m := range platformMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// record copies a finished (or cancelled) run into eval_results, with grades.
func (r *Runner) record(ctx context.Context, st *state, s slot, run *agentorcav1alpha1.AgentRun, phase, reason string) error {
	c := st.caseBy[s.caseID]
	res := postgresql.EvalResult{
		CaseID: &c.ID, Arm: s.arm, Repeat: s.repeat, Phase: phase, Error: reason, CreatedBy: runnerUser,
		RunNamespace: run.Namespace, RunName: run.Name,
		Output:  trimOutput(run.Status.Output),
		CostUSD: parseUSD(run.Status.SpendUSD),
	}
	for _, cond := range run.Status.Conditions {
		if cond.Type == "CostCapture" && cond.Status == metav1.ConditionFalse {
			res.CostIncomplete = true
		}
	}
	if run.Status.StartTime != nil && run.Status.CompletionTime != nil {
		res.LatencyMs = max(run.Status.CompletionTime.Sub(run.Status.StartTime.Time).Milliseconds(), 0)
	}
	var grades []postgresql.EvalGrade
	if phase == phaseSucceeded {
		grades, res.CasePass = gradeResult(st.graders, c, res)
	}
	recorded, err := r.store.RecordEvalRunResult(ctx, st.e.ID, res, grades)
	if err != nil {
		return err
	}
	st.done[s] = phase
	if recorded {
		st.spent += res.CostUSD
	}
	return nil
}

// recordSkipped records a slot that never got a run.
func (r *Runner) recordSkipped(ctx context.Context, st *state, s slot, reason string) error {
	c := st.caseBy[s.caseID]
	_, err := r.store.RecordEvalRunResult(ctx, st.e.ID, postgresql.EvalResult{
		CaseID: &c.ID, Arm: s.arm, Repeat: s.repeat, Phase: phaseSkipped, Error: reason, CreatedBy: runnerUser,
	}, nil)
	if err == nil {
		st.done[s] = phaseSkipped
	}
	return err
}

// siblingSkipped reports whether another arm's run of the same case and repeat
// was skipped, which drops the pair from the comparison.
func (st *state) siblingSkipped(s slot) bool {
	for _, a := range st.e.Arms {
		if a.Label != s.arm && st.done[slot{s.caseID, a.Label, s.repeat}] == phaseSkipped {
			return true
		}
	}
	return false
}

// start creates runs while there is room. It returns why it stopped early
// ("budget", "daily-budget") when no run can be started however long it waits for
// the runs in flight, and "" otherwise.
func (r *Runner) start(ctx context.Context, st *state, lim tenantLimits, all []slot, inflight int, committed float64) (string, error) {
	e := st.e
	perExp, perTenant := runLimit(e.Concurrency, lim)
	tenantActive, err := r.activeTenantEvalRuns(ctx, e.TenantID)
	if err != nil {
		return "", err
	}
	dailyLeft := -1.0
	if lim.DailyUSD > 0 {
		used, err := r.todaySpend(ctx, e.TenantID)
		if err != nil {
			return "", err
		}
		dailyLeft = lim.DailyUSD - used
	}
	for _, s := range all {
		if _, ok := st.done[s]; ok {
			continue
		}
		run := st.latest(s)
		if run != nil && !isTerminal(run) {
			continue
		}
		if run == nil && st.siblingSkipped(s) {
			if err := r.recordSkipped(ctx, st, s, "the paired arm was skipped for this case"); err != nil {
				return "", err
			}
			continue
		}
		if inflight >= perExp || tenantActive >= perTenant {
			return "", nil
		}
		remaining := e.BudgetUSD - st.spent
		if dailyLeft >= 0 && dailyLeft < remaining {
			return StoppedByDailyBudget, nil
		}
		c := runCap(e.RunBudgetUSD, e.BudgetUSD, st.spent, committed, inflight)
		if c < minRunBudgetUSD {
			if remaining < minRunBudgetUSD {
				return StoppedByBudget, nil
			}
			return "", nil // the runs in flight hold the rest; wait for them
		}
		if r.cfg.Limiter != nil && lim.RateLimitRPM > 0 {
			if _, ok := r.cfg.Limiter.Allow(e.TenantID, lim.RateLimitRPM); !ok {
				return "", nil
			}
		}
		attempt := 0
		if run != nil {
			attempt = attemptOf(run) + 1
		}
		if err := r.createRun(ctx, st, lim, s, attempt, c); err != nil {
			return "", err
		}
		inflight++
		tenantActive++
		committed += c
	}
	return "", nil
}

func (r *Runner) createRun(ctx context.Context, st *state, lim tenantLimits, s slot, attempt int, capUSD float64) error {
	e := st.e
	idx := 0
	for i, a := range e.Arms {
		if a.Label == s.arm {
			idx = i
		}
	}
	timeout := metav1.Duration{Duration: time.Duration(e.TimeoutSeconds) * time.Second}
	labels := objectLabels(e, lim.Name)
	labels[tenancy.LabelEvalArm] = s.arm
	labels[tenancy.LabelEvalCase] = strconv.FormatInt(s.caseID, 10)
	labels[tenancy.LabelEvalRepeat] = strconv.Itoa(s.repeat)
	labels[labelAttempt] = strconv.Itoa(attempt)
	run := &agentorcav1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: runNameOf(e.ID, idx, s.caseID, s.repeat, attempt), Namespace: e.Namespace, Labels: labels,
			Annotations: map[string]string{annotationCreatedBy: e.CreatedBy},
		},
		// No callbacks and no egress: nothing an evaluation does is published.
		Spec: agentorcav1alpha1.AgentRunSpec{
			AgentRef: agentNameOf(e.ID, s.arm),
			Input:    st.caseBy[s.caseID].Input,
			Timeout:  &timeout,
			Budget: &agentorcav1alpha1.RunBudget{
				MaxUSD:     strconv.FormatFloat(capUSD, 'f', 6, 64),
				OnExceeded: agentorcav1alpha1.RunBudgetFail,
			},
		},
	}
	if err := r.kube.Create(ctx, run); err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating the run for %s: %w", s, err)
	}
	return nil
}

// ── ending ──────────────────────────────────────────────────────────────────

// Summary is eval_experiments.summary.
type Summary struct {
	// StoppedBy says why the experiment ended before every slot had a result.
	StoppedBy string `json:"stoppedBy,omitempty"`
	// Partial is true when the comparison covers only the cases every arm finished.
	Partial bool `json:"partial,omitempty"`
	// Results is the number of results; Skipped those that were skipped.
	Results  int     `json:"results"`
	Skipped  int     `json:"skipped"`
	SpentUSD float64 `json:"spentUsd"`
	// Comparison is the paired comparison of the arms against the first, absent
	// when there was nothing to compare.
	Comparison      *compare.Summary `json:"comparison,omitempty"`
	ComparisonError string           `json:"comparisonError,omitempty"`
}

func (r *Runner) summarise(ctx context.Context, e postgresql.EvalExperiment, spent float64, stoppedBy string) ([]byte, error) {
	rows, err := r.store.EvalSummaryRows(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	sum := Summary{StoppedBy: stoppedBy, Partial: stoppedBy != "", Results: len(rows), SpentUSD: spent}
	for _, row := range rows {
		if row.Phase == phaseSkipped {
			sum.Skipped++
		}
	}
	if len(e.Arms) > 0 {
		cmp, err := compare.Summarize(rows, e.Arms[0].Label, compare.Options{Seed: uint64(e.ID)})
		if err != nil {
			sum.ComparisonError = err.Error()
		} else {
			sum.Comparison = &cmp
		}
	}
	return json.Marshal(sum)
}

// complete ends the experiment as complete, with its summary.
func (r *Runner) complete(ctx context.Context, st *state, stoppedBy string) error {
	sum, err := r.summarise(ctx, st.e, st.spent, stoppedBy)
	if err != nil {
		return err
	}
	r.cleanup(ctx, st.e)
	return r.finish(ctx, st.e, postgresql.ExperimentComplete, sum, "")
}

// cancel stops an experiment: runs that finished are recorded, the rest are
// recorded as cancelled and deleted, and the experiment ends cancelled with a
// summary of the paired data it has.
func (r *Runner) cancel(ctx context.Context, e postgresql.EvalExperiment) error {
	st, err := r.observe(ctx, e)
	if err != nil {
		return err
	}
	if _, _, err := r.ingest(ctx, st, true); err != nil {
		return err
	}
	sum, err := r.summarise(ctx, e, st.spent, StoppedByCancel)
	if err != nil {
		return err
	}
	r.cleanup(ctx, e)
	return r.finish(ctx, e, postgresql.ExperimentCancelled, sum, "")
}
