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

// Package runner runs evaluation experiments (docs/evaluations.md). It is a
// poller that only the operator's leader runs. Each tick it claims the queued
// and running experiments from PostgreSQL (FOR UPDATE SKIP LOCKED with a lease),
// and for each one:
//
//   - on the first tick it resolves every arm to a content hash, checks that what
//     the arm's agent names exists, and makes the arm's hidden Agent
//     (eval-<experiment>-<arm>) and, for a model arm, a one-provider ModelSelector;
//   - creates one AgentRun per (case, arm, repeat), at most as many at once as
//     the experiment, the tenant and the budget allow;
//   - copies each finished run into eval_results, grades it with the dataset's
//     deterministic graders, and adds its cost to the experiment's spend;
//   - ends the experiment when every slot has a result, the budget is spent, or
//     a cancel was asked for, then deletes what it made.
//
// All state is in PostgreSQL and in the AgentRuns (found by label), so a restart
// resumes where the last tick left off, and a sweep on start deletes whatever a
// finished or vanished experiment left behind. Every call that reads or writes
// an experiment's rows runs in that experiment's tenant scope.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/evals/compare"
	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/tenancy"
)

// Defaults of the runner.
const (
	DefaultInterval = 5 * time.Second
	DefaultLease    = 60 * time.Second
	claimBatch      = 20
	// minRunBudgetUSD is the smallest per-run cap worth starting a run with.
	minRunBudgetUSD = 0.0001
	// MaxTenantEvalRuns is the most eval runs one tenant has active at once.
	MaxTenantEvalRuns = 16
	runnerUser        = "eval-runner"
)

// Why an experiment stopped before every slot had a result (Summary.StoppedBy).
const (
	StoppedByBudget      = "budget"
	StoppedByDailyBudget = "daily-budget"
	StoppedByCancel      = "cancelled"
)

// Store is the part of postgresql.Store the runner uses. The runner claims in
// the platform scope and then calls everything else in the experiment's tenant
// scope, so a fake need not care about scopes.
type Store interface {
	ClaimEvalExperiments(ctx context.Context, owner string, lease time.Duration, limit int) ([]postgresql.EvalExperiment, error)
	ActiveEvalExperiments(ctx context.Context, ids []int64) (map[int64]bool, error)
	StartEvalExperiment(ctx context.Context, id int64, arms []postgresql.EvalArm) error
	ListEvalExperimentCases(ctx context.Context, id int64) ([]postgresql.EvalCase, error)
	ListEvalExperimentResultKeys(ctx context.Context, id int64) ([]postgresql.EvalResultKey, error)
	RecordEvalRunResult(ctx context.Context, experimentID int64, r postgresql.EvalResult, grades []postgresql.EvalGrade) (bool, error)
	FinishEvalExperiment(ctx context.Context, id int64, status string, summary json.RawMessage, errText string) error
	EvalSummaryRows(ctx context.Context, id int64) ([]compare.Row, error)
	ListEvalGraders(ctx context.Context, datasetID int64) ([]postgresql.EvalGrader, error)
	// GetRun reads an archived run: the trace the tool graders read.
	GetRun(ctx context.Context, namespace, name string) (*postgresql.RunArchive, error)
	// The judge's and the review stage's calls.
	ListEvalExperimentResults(ctx context.Context, id int64, q postgresql.EvalExperimentResultQuery) ([]postgresql.EvalResult, error)
	SaveEvalGrades(ctx context.Context, experimentID, resultID int64, grades []postgresql.EvalGrade) error
	AddEvalSpend(ctx context.Context, experimentID int64, usd float64) error
	EvalJudgeSpendSince(ctx context.Context, since time.Time) (float64, error)
	EvalGraderStats(ctx context.Context, experimentID int64) ([]postgresql.EvalGraderStat, error)
	PrepareEvalReview(ctx context.Context, experimentID int64) (int, error)
	AwaitEvalReview(ctx context.Context, id int64, summary json.RawMessage) error
	PendingEvalReviews(ctx context.Context, experimentID int64) (int, error)
	GetRevision(ctx context.Context, q postgresql.RevisionQuery, n int) (postgresql.Revision, error)
}

// Limiter is the tenant request-rate bucket (apiserver.RateLimiter satisfies it).
type Limiter interface {
	Allow(tenant string, requestsPerMinute int) (time.Duration, bool)
}

// Config tunes the runner. The zero value takes the defaults.
type Config struct {
	// Owner names this runner in leases. Default: the host name, so a restarted
	// pod takes its experiments back at once.
	Owner string
	// Interval is the time between polls.
	Interval time.Duration
	// Lease is how long a claim holds an experiment. It must outlast a tick.
	Lease time.Duration
	// Limiter is the tenant's RateLimitRPM bucket. Nil means no rate limit.
	Limiter Limiter
	// Now is the clock. Default time.Now.
	Now func() time.Time
	// Judges finds the model an llm grader names. Nil means no judge: an
	// experiment whose dataset has an llm grader fails to start.
	Judges JudgeResolver
}

// Runner runs experiments.
type Runner struct {
	store Store
	kube  client.Client
	cfg   Config
	log   *slog.Logger
	// canaryOK remembers the judges that passed their canary check, by
	// experiment and grader, so a retried start does not pay for it twice.
	canaryOK sync.Map
}

// New returns a runner. kube is the operator's client (the cache is fine).
func New(store Store, kube client.Client, cfg Config) *Runner {
	if cfg.Owner == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "operator"
		}
		cfg.Owner = host
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Lease <= 0 {
		cfg.Lease = DefaultLease
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Runner{store: store, kube: kube, cfg: cfg, log: slog.Default().With("component", "eval-runner")}
}

// Start sweeps orphans and then polls until ctx ends. It satisfies
// controller-runtime's Runnable and, because it does not opt out, runs only
// where the manager's leader election has made this process the leader.
func (r *Runner) Start(ctx context.Context) error {
	if err := r.Sweep(ctx); err != nil {
		r.log.Error("sweeping evaluation leftovers", "err", err)
	}
	t := time.NewTicker(r.cfg.Interval)
	defer t.Stop()
	for {
		if err := r.Tick(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("evaluation tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick claims the experiments that need work and advances each one step. An
// error in one experiment is logged and does not stop the others.
func (r *Runner) Tick(ctx context.Context) error {
	claimed, err := r.store.ClaimEvalExperiments(postgresql.WithPlatform(ctx), r.cfg.Owner, r.cfg.Lease, claimBatch)
	if err != nil {
		return err
	}
	tcs, err := r.tenantConfigs(ctx)
	if err != nil {
		return err
	}
	for i := range claimed {
		e := claimed[i]
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.processSafely(ctx, e, tcs)
	}
	return nil
}

func (r *Runner) processSafely(ctx context.Context, e postgresql.EvalExperiment, tcs []agentorcav1alpha1.TenantConfig) {
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("evaluation experiment panicked", "experiment", e.ID, "panic", fmt.Sprint(p))
		}
	}()
	tctx := postgresql.WithTenant(ctx, e.TenantID)
	if err := r.process(tctx, e, limitsOf(tcs, e.TenantID)); err != nil && ctx.Err() == nil {
		r.log.Error("advancing evaluation experiment", "experiment", e.ID, "tenant", e.TenantID, "err", err)
	}
}

// process advances one experiment by one step. ctx carries the experiment's tenant scope.
func (r *Runner) process(ctx context.Context, e postgresql.EvalExperiment, lim tenantLimits) error {
	if e.Status == postgresql.ExperimentReview {
		return r.reviewTick(ctx, e)
	}
	if e.CancelRequested {
		return r.cancel(ctx, e)
	}
	if e.Status == postgresql.ExperimentQueued {
		arms, err := r.prepare(ctx, e, lim)
		if err != nil {
			var fatal *experimentError
			if errors.As(err, &fatal) {
				r.log.Info("evaluation experiment failed to start", "experiment", e.ID, "reason", fatal.Error())
				r.cleanup(ctx, e)
				return r.finish(ctx, e, postgresql.ExperimentFailed, nil, fatal.Error())
			}
			return err
		}
		e.Arms = arms
		if err := r.store.StartEvalExperiment(ctx, e.ID, arms); err != nil {
			if errors.Is(err, postgresql.ErrExperimentFinished) {
				return nil
			}
			return err
		}
		e.Status = postgresql.ExperimentRunning
	}
	return r.advance(ctx, e, lim)
}

// experimentError is a failure of the experiment itself (a missing reference, a
// stale revision), as opposed to a transient error worth retrying next tick.
type experimentError struct{ msg string }

func (e *experimentError) Error() string { return e.msg }

func failf(format string, a ...any) error { return &experimentError{msg: fmt.Sprintf(format, a...)} }

func (r *Runner) finish(ctx context.Context, e postgresql.EvalExperiment, status string, summary json.RawMessage, errText string) error {
	err := r.store.FinishEvalExperiment(ctx, e.ID, status, summary, errText)
	if errors.Is(err, postgresql.ErrExperimentFinished) {
		return nil
	}
	return err
}

// ── naming and labels ───────────────────────────────────────────────────────

// agentNameOf is the hidden Agent (and ModelSelector) of an arm.
func agentNameOf(experimentID int64, arm string) string {
	return fmt.Sprintf("eval-%d-%s", experimentID, arm)
}

// runNameOf is the AgentRun of one slot. The name is a function of the slot, so
// creating it twice (after a crash between the create and the next tick) finds
// the first. A retry after a platform failure has its own name.
func runNameOf(experimentID int64, armIndex int, caseID int64, repeat, attempt int) string {
	n := fmt.Sprintf("eval-%d-%d-%d-%d", experimentID, armIndex, caseID, repeat)
	if attempt > 0 {
		n += fmt.Sprintf("-r%d", attempt)
	}
	return n
}

func objectLabels(e postgresql.EvalExperiment, tenantName string) map[string]string {
	l := map[string]string{
		tenancy.LabelTenantID:       e.TenantID,
		tenancy.LabelEval:           tenancy.LabelEvalValue,
		tenancy.LabelEvalExperiment: tenancy.ExperimentLabel(e.ID),
	}
	if tenantName != "" {
		l["agentorca.io/tenant"] = tenantName
	}
	return l
}

// slot identifies one run of an experiment.
type slot struct {
	caseID int64
	arm    string
	repeat int
}

func (s slot) String() string {
	return fmt.Sprintf("case %d, arm %s, repeat %d", s.caseID, s.arm, s.repeat)
}

// ── cleanup and sweep ───────────────────────────────────────────────────────

// cleanup deletes the Agents and ModelSelectors made for an experiment. It lists
// and deletes one by one: the operator's role has no deletecollection verb.
func (r *Runner) cleanup(ctx context.Context, e postgresql.EvalExperiment) {
	sel := client.MatchingLabels{tenancy.LabelEvalExperiment: tenancy.ExperimentLabel(e.ID), tenancy.LabelTenantID: e.TenantID}
	var agents agentorcav1alpha1.AgentList
	if err := r.kube.List(ctx, &agents, client.InNamespace(e.Namespace), sel); err != nil {
		r.log.Error("listing an experiment's agents", "experiment", e.ID, "err", err)
	}
	for i := range agents.Items {
		if err := client.IgnoreNotFound(r.kube.Delete(ctx, &agents.Items[i])); err != nil {
			r.log.Error("deleting an experiment's agent", "experiment", e.ID, "agent", agents.Items[i].Name, "err", err)
		}
	}
	var selectors agentorcav1alpha1.ModelSelectorList
	if err := r.kube.List(ctx, &selectors, client.InNamespace(e.Namespace), sel); err != nil {
		r.log.Error("listing an experiment's model selectors", "experiment", e.ID, "err", err)
	}
	for i := range selectors.Items {
		if err := client.IgnoreNotFound(r.kube.Delete(ctx, &selectors.Items[i])); err != nil {
			r.log.Error("deleting an experiment's model selector", "experiment", e.ID, "selector", selectors.Items[i].Name, "err", err)
		}
	}
}

// Sweep deletes what experiments that are over, or gone, left in the cluster:
// their hidden Agents and ModelSelectors, and any AgentRun still running. It
// runs on start, so a crash leaves no orphans.
func (r *Runner) Sweep(ctx context.Context) error {
	var agents agentorcav1alpha1.AgentList
	var selectors agentorcav1alpha1.ModelSelectorList
	var runs agentorcav1alpha1.AgentRunList
	has := client.HasLabels{tenancy.LabelEvalExperiment}
	if err := r.kube.List(ctx, &agents, has); err != nil {
		return err
	}
	if err := r.kube.List(ctx, &selectors, has); err != nil {
		return err
	}
	if err := r.kube.List(ctx, &runs, has); err != nil {
		return err
	}
	ids := map[int64]bool{}
	note := func(l map[string]string) {
		var id int64
		if _, err := fmt.Sscanf(l[tenancy.LabelEvalExperiment], "%d", &id); err == nil && id > 0 {
			ids[id] = true
		}
	}
	for i := range agents.Items {
		note(agents.Items[i].Labels)
	}
	for i := range selectors.Items {
		note(selectors.Items[i].Labels)
	}
	for i := range runs.Items {
		note(runs.Items[i].Labels)
	}
	if len(ids) == 0 {
		return nil
	}
	list := make([]int64, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	active, err := r.store.ActiveEvalExperiments(postgresql.WithPlatform(ctx), list)
	if err != nil {
		return err
	}
	orphan := func(l map[string]string) bool {
		var id int64
		_, err := fmt.Sscanf(l[tenancy.LabelEvalExperiment], "%d", &id)
		return err != nil || !active[id]
	}
	var errs []error
	for i := range agents.Items {
		if orphan(agents.Items[i].Labels) {
			errs = append(errs, client.IgnoreNotFound(r.kube.Delete(ctx, &agents.Items[i])))
		}
	}
	for i := range selectors.Items {
		if orphan(selectors.Items[i].Labels) {
			errs = append(errs, client.IgnoreNotFound(r.kube.Delete(ctx, &selectors.Items[i])))
		}
	}
	for i := range runs.Items {
		run := &runs.Items[i]
		if orphan(run.Labels) && !isTerminal(run) {
			errs = append(errs, client.IgnoreNotFound(r.kube.Delete(ctx, run)))
		}
	}
	return errors.Join(errs...)
}

func trimOutput(s string) string {
	if len(s) > postgresql.MaxEvalResultOutput {
		return strings.ToValidUTF8(s[:postgresql.MaxEvalResultOutput], "")
	}
	return s
}
