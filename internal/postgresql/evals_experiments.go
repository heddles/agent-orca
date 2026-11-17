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

// Evaluation experiments (table eval_experiments, migration 0015) and the
// results the experiment runner writes into eval_results.
//
// The API calls (create, get, list, cancel, results) run in the caller's tenant
// scope. The runner claims experiments across tenants in the platform scope
// (ClaimEvalExperiments) and then does every other call, results and grades
// included, in the scope of the experiment's own tenant, so a bug in the runner
// cannot write across tenants.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/evals/compare"
)

var (
	// ErrTooManyExperiments: the tenant already has the most experiments it may run at once.
	ErrTooManyExperiments = errors.New("the tenant already has the most evaluation experiments it may run at once")
	// ErrExperimentFinished: the experiment is already complete, cancelled or failed.
	ErrExperimentFinished = errors.New("the experiment has finished")
)

// Experiment statuses (eval_experiments.status).
const (
	ExperimentQueued    = "queued"
	ExperimentRunning   = "running"
	ExperimentGrading   = "grading"
	ExperimentReview    = "awaiting_review"
	ExperimentComplete  = "complete"
	ExperimentCancelled = "cancelled"
	ExperimentFailed    = "failed"
)

// Experiment purposes.
const (
	PurposeCompare = "compare"
	PurposeGate    = "gate"
)

// Limits on an experiment.
const (
	MinExperimentArms             = 2
	MaxExperimentArms             = 4
	MaxExperimentRepeats          = 5
	MaxExperimentConcurrency      = 16
	DefaultExperimentConcurrency  = 4
	DefaultExperimentTimeoutSecs  = 600
	DefaultMaxActiveExperiments   = 2
	DefaultMaxCasesPerExperiment  = 500
	DefaultMaxExperimentBudgetUSD = 25.0
	// DefaultRunBudgetUSD is the per-run cap when the request names none.
	DefaultRunBudgetUSD = 1.0
	maxArmLabelLen      = 20
	maxExperimentName   = 120
	maxExperimentsList  = 200
)

var evalObjectNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// EvalArm is one way of running the dataset: an agent, optionally at a saved
// revision, optionally on a different model.
type EvalArm struct {
	// Label names the arm in results and in object names: a DNS label of at most
	// 20 characters. The first arm is the baseline.
	Label string `json:"label"`
	// Agent is the Agent in the experiment's namespace.
	Agent string `json:"agent"`
	// Revision is the number of a saved revision of the Agent (the revision
	// history of the UI API). 0 runs the live Agent, hashed when the experiment
	// starts.
	Revision int `json:"revision,omitempty"`
	// Hash is the content hash of the spec the arm ran. On create it is optional
	// and, when set with a revision, must match the revision (a stale client is
	// refused); the runner fills it in when it starts the experiment.
	Hash string `json:"hash,omitempty"`
	// ModelProvider runs the arm on this ModelProvider alone, through a
	// one-provider ModelSelector made for the experiment.
	ModelProvider string `json:"modelProvider,omitempty"`
}

// EvalExperiment is a comparison of arms over a dataset at a frozen version.
type EvalExperiment struct {
	ID             int64     `json:"id"`
	TenantID       string    `json:"tenantId,omitempty"`
	Namespace      string    `json:"namespace"`
	DatasetID      int64     `json:"datasetId"`
	DatasetVersion int       `json:"datasetVersion"`
	Name           string    `json:"name,omitempty"`
	Status         string    `json:"status"`
	Arms           []EvalArm `json:"arms"`
	Repeats        int       `json:"repeats"`
	// BudgetUSD is the hard cap on the experiment's runs together; SpentUSD what
	// they cost so far. RunBudgetUSD caps one run.
	BudgetUSD    float64 `json:"budgetUsd"`
	SpentUSD     float64 `json:"spentUsd"`
	RunBudgetUSD float64 `json:"runBudgetUsd"`
	// TimeoutSeconds is each run's timeout.
	TimeoutSeconds int    `json:"timeoutSeconds"`
	Concurrency    int    `json:"concurrency"`
	Purpose        string `json:"purpose"`
	// CancelRequested is true once a cancel was asked for and the runner has not
	// finished stopping the experiment.
	CancelRequested bool `json:"cancelRequested,omitempty"`
	// Summary is computed when the experiment ends.
	Summary json.RawMessage `json:"summary,omitempty"`
	Error   string          `json:"error,omitempty"`
	// Cases is the number of cases at DatasetVersion; ResultsDone the results so
	// far (skipped ones included). The total is Cases * len(Arms) * Repeats.
	Cases       int        `json:"cases"`
	ResultsDone int        `json:"resultsDone"`
	CreatedBy   string     `json:"createdBy"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
}

// EvalExperimentLimits are the configurable limits on creating an experiment.
// A zero field uses the default.
type EvalExperimentLimits struct {
	MaxActivePerTenant    int
	MaxCasesPerExperiment int
	MaxBudgetUSD          float64
}

func (l EvalExperimentLimits) active() int {
	if l.MaxActivePerTenant <= 0 {
		return DefaultMaxActiveExperiments
	}
	return l.MaxActivePerTenant
}

func (l EvalExperimentLimits) cases() int {
	if l.MaxCasesPerExperiment <= 0 {
		return DefaultMaxCasesPerExperiment
	}
	return l.MaxCasesPerExperiment
}

func (l EvalExperimentLimits) budget() float64 {
	if l.MaxBudgetUSD <= 0 {
		return DefaultMaxExperimentBudgetUSD
	}
	return l.MaxBudgetUSD
}

// Normalize fills the defaults of a new experiment.
func (e *EvalExperiment) Normalize() {
	if e.Repeats == 0 {
		e.Repeats = 1
	}
	if e.Concurrency == 0 {
		e.Concurrency = DefaultExperimentConcurrency
	}
	if e.TimeoutSeconds == 0 {
		e.TimeoutSeconds = DefaultExperimentTimeoutSecs
	}
	if e.Purpose == "" {
		e.Purpose = PurposeCompare
	}
	if e.RunBudgetUSD == 0 && e.BudgetUSD > 0 {
		e.RunBudgetUSD = math.Min(e.BudgetUSD, DefaultRunBudgetUSD)
	}
	e.Name = strings.TrimSpace(e.Name)
}

func validMoney(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) && v < 1e6 }

// Validate reports why e cannot be stored. Call Normalize first.
func (e EvalExperiment) Validate(l EvalExperimentLimits) error {
	switch {
	case len(e.Arms) < MinExperimentArms || len(e.Arms) > MaxExperimentArms:
		return fmt.Errorf("%w: an experiment takes %d to %d arms", ErrInvalidEval, MinExperimentArms, MaxExperimentArms)
	case e.Repeats < 1 || e.Repeats > MaxExperimentRepeats:
		return fmt.Errorf("%w: repeats must be between 1 and %d", ErrInvalidEval, MaxExperimentRepeats)
	case e.Concurrency < 1 || e.Concurrency > MaxExperimentConcurrency:
		return fmt.Errorf("%w: concurrency must be between 1 and %d", ErrInvalidEval, MaxExperimentConcurrency)
	case e.TimeoutSeconds < 10 || e.TimeoutSeconds > 86400:
		return fmt.Errorf("%w: timeoutSeconds must be between 10 and 86400", ErrInvalidEval)
	case !validMoney(e.BudgetUSD) || e.BudgetUSD > l.budget():
		return fmt.Errorf("%w: budgetUsd must be above 0 and at most %g", ErrInvalidEval, l.budget())
	case !validMoney(e.RunBudgetUSD) || e.RunBudgetUSD > e.BudgetUSD:
		return fmt.Errorf("%w: runBudgetUsd must be above 0 and at most the experiment budget", ErrInvalidEval)
	case e.Purpose != PurposeCompare && e.Purpose != PurposeGate:
		return fmt.Errorf("%w: purpose must be compare or gate", ErrInvalidEval)
	case len(e.Name) > maxExperimentName:
		return fmt.Errorf("%w: name is over %d bytes", ErrInvalidEval, maxExperimentName)
	case e.CreatedBy == "":
		return fmt.Errorf("%w: created by is required", ErrInvalidEval)
	}
	labels := map[string]bool{}
	identities := map[string]bool{}
	for i, a := range e.Arms {
		if !evalNameRe.MatchString(a.Label) || len(a.Label) > maxArmLabelLen {
			return fmt.Errorf("%w: arm %d: label must be a DNS label of at most %d characters", ErrInvalidEval, i+1, maxArmLabelLen)
		}
		if labels[a.Label] {
			return fmt.Errorf("%w: two arms are labelled %q", ErrInvalidEval, a.Label)
		}
		labels[a.Label] = true
		if !evalObjectNameRe.MatchString(a.Agent) || len(a.Agent) > 253 {
			return fmt.Errorf("%w: arm %q: agent must be an Agent name", ErrInvalidEval, a.Label)
		}
		if a.ModelProvider != "" && (!evalObjectNameRe.MatchString(a.ModelProvider) || len(a.ModelProvider) > 253) {
			return fmt.Errorf("%w: arm %q: modelProvider must be a ModelProvider name", ErrInvalidEval, a.Label)
		}
		if a.Revision < 0 {
			return fmt.Errorf("%w: arm %q: revision must not be negative", ErrInvalidEval, a.Label)
		}
		if a.Hash != "" && !hashRe.MatchString(a.Hash) {
			return fmt.Errorf("%w: arm %q: hash must be a 64 character hex SHA-256", ErrInvalidEval, a.Label)
		}
		id := fmt.Sprintf("%s|%d|%s", a.Agent, a.Revision, a.ModelProvider)
		if identities[id] {
			return fmt.Errorf("%w: arm %q repeats another arm: the same agent, revision and model", ErrInvalidEval, a.Label)
		}
		identities[id] = true
	}
	return nil
}

var hashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// EvalExperimentQuery selects experiments.
type EvalExperimentQuery struct {
	// Namespaces limits the list to these workspaces. Nil means all of the
	// scope's; an empty non-nil slice matches nothing.
	Namespaces []string
	Namespace  string
	DatasetID  int64
	Status     string
	// Before is a cursor: only experiments with an ID below it.
	Before int64
	Limit  int
}

const evalExpCols = `e.id, e.tenant_id, e.namespace, e.dataset_id, e.dataset_version, e.name, e.status, e.arms,
	e.repeats, e.budget_usd::float8, e.spent_usd::float8, e.run_budget_usd::float8, e.timeout_seconds, e.concurrency,
	e.purpose, e.cancel_requested, e.summary, e.error,
	(SELECT count(*) FROM eval_cases c WHERE c.dataset_id = e.dataset_id AND c.tenant_id = e.tenant_id
	   AND c.added_in <= e.dataset_version AND (c.removed_in IS NULL OR c.removed_in > e.dataset_version)),
	(SELECT count(*) FROM eval_results r WHERE r.experiment_id = e.id AND r.tenant_id = e.tenant_id),
	e.created_by, e.created_at, e.updated_at, e.started_at, e.finished_at`

func scanEvalExperiment(row interface{ Scan(...any) error }) (EvalExperiment, error) {
	var e EvalExperiment
	var arms, summary []byte
	var started, finished sql.NullTime
	err := row.Scan(&e.ID, &e.TenantID, &e.Namespace, &e.DatasetID, &e.DatasetVersion, &e.Name, &e.Status, &arms,
		&e.Repeats, &e.BudgetUSD, &e.SpentUSD, &e.RunBudgetUSD, &e.TimeoutSeconds, &e.Concurrency,
		&e.Purpose, &e.CancelRequested, &summary, &e.Error, &e.Cases, &e.ResultsDone,
		&e.CreatedBy, &e.CreatedAt, &e.UpdatedAt, &started, &finished)
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(arms, &e.Arms); err != nil {
		return e, fmt.Errorf("reading arms: %w", err)
	}
	if len(summary) > 0 && string(summary) != "null" {
		e.Summary = summary
	}
	if started.Valid {
		e.StartedAt = &started.Time
	}
	if finished.Valid {
		e.FinishedAt = &finished.Time
	}
	return e, nil
}

// CreateEvalExperiment queues an experiment over a dataset. The dataset's
// current version is frozen into it. The tenant comes from the dataset. It
// fails with ErrEvalNotFound for a dataset outside the call's scope,
// ErrInvalidEval when the experiment or the dataset (no cases, or more than the
// limit) will not do, and ErrTooManyExperiments when the tenant already has the
// most it may run at once.
func (s *Store) CreateEvalExperiment(ctx context.Context, e EvalExperiment, l EvalExperimentLimits) (EvalExperiment, error) {
	e.Normalize()
	if err := e.Validate(l); err != nil {
		return EvalExperiment{}, err
	}
	arms, _ := json.Marshal(e.Arms)
	var id int64
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, ns, version, err := lockDataset(ctx, tx, scope, e.DatasetID)
		if err != nil {
			return err
		}
		if e.Namespace == "" {
			e.Namespace = ns
		}
		if e.Namespace != ns {
			return fmt.Errorf("%w: the experiment runs in the dataset's workspace (%s)", ErrInvalidEval, ns)
		}
		// lockDataset locks the dataset row; the tenant lock below serialises the
		// count of active experiments across the tenant's datasets.
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "eval-experiments|"+tenant); err != nil {
			return err
		}
		var cases, active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM eval_cases WHERE dataset_id = $1 AND tenant_id = $2 AND removed_in IS NULL`,
			e.DatasetID, tenant).Scan(&cases); err != nil {
			return err
		}
		if cases == 0 {
			return fmt.Errorf("%w: the dataset has no cases", ErrInvalidEval)
		}
		if cases > l.cases() {
			return fmt.Errorf("%w: the dataset has %d cases, an experiment takes at most %d", ErrInvalidEval, cases, l.cases())
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM eval_experiments WHERE tenant_id = $1 AND status IN ('queued','running','grading')`,
			tenant).Scan(&active); err != nil {
			return err
		}
		if active >= l.active() {
			return fmt.Errorf("%w (%d)", ErrTooManyExperiments, l.active())
		}
		return tx.QueryRowContext(ctx, `
		INSERT INTO eval_experiments (tenant_id, namespace, dataset_id, dataset_version, name, arms, repeats,
		       budget_usd, run_budget_usd, timeout_seconds, concurrency, purpose, created_by)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id`,
			tenant, e.Namespace, e.DatasetID, version, e.Name, string(arms), e.Repeats,
			e.BudgetUSD, e.RunBudgetUSD, e.TimeoutSeconds, e.Concurrency, e.Purpose, e.CreatedBy).Scan(&id)
	})
	if err != nil {
		return EvalExperiment{}, wrapEval("creating the experiment", err)
	}
	return s.GetEvalExperiment(ctx, id)
}

// GetEvalExperiment returns one experiment, or ErrEvalNotFound.
func (s *Store) GetEvalExperiment(ctx context.Context, id int64) (EvalExperiment, error) {
	var e EvalExperiment
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		e, err = scanEvalExperiment(tx.QueryRowContext(ctx,
			`SELECT `+evalExpCols+` FROM eval_experiments e WHERE e.id = $1 AND ($2::text = '*' OR e.tenant_id = $2)`, id, scope))
		return notFound(err)
	})
	if err != nil {
		return EvalExperiment{}, wrapEval("loading the experiment", err)
	}
	return e, nil
}

// ListEvalExperiments returns experiments, newest first.
func (s *Store) ListEvalExperiments(ctx context.Context, q EvalExperimentQuery) ([]EvalExperiment, error) {
	limit := q.Limit
	if limit <= 0 || limit > maxExperimentsList {
		limit = 50
	}
	out := []EvalExperiment{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		where := []string{"($1::text = '*' OR e.tenant_id = $1)"}
		args := []any{scope}
		arg := func(v any) string {
			args = append(args, v)
			return fmt.Sprintf("$%d", len(args))
		}
		if q.Namespaces != nil {
			j, _ := json.Marshal(q.Namespaces)
			where = append(where, "e.namespace IN (SELECT jsonb_array_elements_text("+arg(string(j))+"::jsonb))")
		}
		if q.Namespace != "" {
			where = append(where, "e.namespace = "+arg(q.Namespace))
		}
		if q.DatasetID > 0 {
			where = append(where, "e.dataset_id = "+arg(q.DatasetID))
		}
		if q.Status != "" {
			where = append(where, "e.status = "+arg(q.Status))
		}
		if q.Before > 0 {
			where = append(where, "e.id < "+arg(q.Before))
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+evalExpCols+` FROM eval_experiments e WHERE `+strings.Join(where, " AND ")+
			` ORDER BY e.id DESC LIMIT `+arg(limit), args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			e, err := scanEvalExperiment(rows)
			if err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrapEval("listing experiments", err)
	}
	return out, nil
}

// RequestEvalCancel stops an experiment. A queued one is cancelled at once; a
// running one is marked, and the runner stops it, deletes its runs and keeps the
// results so far. It returns the experiment as it is after the call, or
// ErrExperimentFinished when it has ended already.
func (s *Store) RequestEvalCancel(ctx context.Context, id int64) (EvalExperiment, error) {
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var status string
		err := tx.QueryRowContext(ctx,
			`SELECT status FROM eval_experiments WHERE id = $1 AND ($2::text = '*' OR tenant_id = $2) FOR UPDATE`, id, scope).Scan(&status)
		if err != nil {
			return notFound(err)
		}
		switch status {
		case ExperimentQueued:
			_, err = tx.ExecContext(ctx, `UPDATE eval_experiments SET status = 'cancelled', finished_at = now(), updated_at = now(),
				lease_owner = '', lease_until = NULL WHERE id = $1`, id)
		case ExperimentRunning, ExperimentGrading:
			_, err = tx.ExecContext(ctx, `UPDATE eval_experiments SET cancel_requested = TRUE, updated_at = now() WHERE id = $1`, id)
		default:
			return ErrExperimentFinished
		}
		return err
	})
	if err != nil {
		return EvalExperiment{}, wrapEval("cancelling the experiment", err)
	}
	return s.GetEvalExperiment(ctx, id)
}

// EvalExperimentResultQuery pages the results of one experiment.
type EvalExperimentResultQuery struct {
	After  int64
	Limit  int
	Arm    string
	CaseID int64
}

// ListEvalExperimentResults returns an experiment's results in ID order, with
// their grades. ErrEvalNotFound when the experiment is not in the call's scope.
func (s *Store) ListEvalExperimentResults(ctx context.Context, id int64, q EvalExperimentResultQuery) ([]EvalResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultResultsPerPage
	}
	if limit > MaxEvalResultsPerPage {
		limit = MaxEvalResultsPerPage
	}
	out := []EvalResult{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM eval_experiments WHERE id = $1 AND ($2::text = '*' OR tenant_id = $2)`, id, scope).Scan(&one); err != nil {
			return notFound(err)
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT `+evalResultCols+` FROM eval_results r
			 WHERE r.experiment_id = $1 AND r.id > $2 AND ($3::text = '' OR r.arm = $3) AND ($4::bigint = 0 OR r.case_id = $4)
			   AND ($5::text = '*' OR r.tenant_id = $5)
			 ORDER BY r.id LIMIT $6`, id, q.After, q.Arm, q.CaseID, scope, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			r, err := scanEvalResult(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		_ = rows.Close()
		return loadGrades(ctx, tx, scope, out)
	})
	if err != nil {
		return nil, wrapEval("listing experiment results", err)
	}
	return out, nil
}

// EvalSpendQuery selects the experiments whose spend the cost view reports.
type EvalSpendQuery struct {
	// Namespaces limits the sum to these workspaces; nil means all of the scope's.
	Namespaces []string
	// Since drops experiments created before it.
	Since *time.Time
}

// EvalSpend is what experiments cost, kept apart from production spend.
type EvalSpend struct {
	TotalUSD    float64 `json:"totalUsd"`
	Experiments int     `json:"experiments"`
}

// QueryEvalSpend sums the spend of experiments.
func (s *Store) QueryEvalSpend(ctx context.Context, q EvalSpendQuery) (EvalSpend, error) {
	var out EvalSpend
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		where := []string{"($1::text = '*' OR tenant_id = $1)"}
		args := []any{scope}
		if q.Namespaces != nil {
			j, _ := json.Marshal(q.Namespaces)
			args = append(args, string(j))
			where = append(where, fmt.Sprintf("namespace IN (SELECT jsonb_array_elements_text($%d::jsonb))", len(args)))
		}
		if q.Since != nil {
			args = append(args, *q.Since)
			where = append(where, fmt.Sprintf("created_at >= $%d", len(args)))
		}
		return tx.QueryRowContext(ctx, `SELECT COALESCE(sum(spent_usd), 0)::float8, count(*) FROM eval_experiments WHERE `+strings.Join(where, " AND "), args...).
			Scan(&out.TotalUSD, &out.Experiments)
	})
	if err != nil {
		return EvalSpend{}, fmt.Errorf("summing experiment spend: %w", err)
	}
	return out, nil
}

// ── the runner's calls ──────────────────────────────────────────────────────

// ClaimEvalExperiments leases up to limit queued or running experiments to owner
// for lease. It must run in the platform scope: it spans tenants. It uses
// FOR UPDATE SKIP LOCKED, so two claimers never take the same experiment, and an
// experiment already leased to owner is claimed again (its lease is renewed) so
// the caller's restart resumes its own work at once. An experiment leased to
// another owner is skipped until that lease runs out.
func (s *Store) ClaimEvalExperiments(ctx context.Context, owner string, lease time.Duration, limit int) ([]EvalExperiment, error) {
	if limit <= 0 {
		limit = 10
	}
	out := []EvalExperiment{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		if scope != PlatformScope {
			return fmt.Errorf("%w: claiming experiments needs the platform scope", ErrNoTenantScope)
		}
		rows, err := tx.QueryContext(ctx, `
		WITH picked AS (
			SELECT id FROM eval_experiments
			WHERE status IN ('queued','running','awaiting_review')
			  AND (lease_owner = $1 OR lease_until IS NULL OR lease_until < now())
			ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $3)
		UPDATE eval_experiments e
		   SET lease_owner = $1, lease_until = now() + make_interval(secs => $2)
		  FROM picked WHERE e.id = picked.id
		RETURNING `+evalExpCols, owner, lease.Seconds(), limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			e, err := scanEvalExperiment(rows)
			if err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("claiming experiments: %w", err)
	}
	return out, nil
}

// ActiveEvalExperiments reports, for each ID, whether the experiment exists and
// has not ended. Platform scope: the runner's orphan sweep uses it.
func (s *Store) ActiveEvalExperiments(ctx context.Context, ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	j, _ := json.Marshal(ids)
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM eval_experiments
			WHERE id IN (SELECT jsonb_array_elements_text($1::jsonb)::bigint)
			  AND status IN ('queued','running','grading','awaiting_review') AND ($2::text = '*' OR tenant_id = $2)`, string(j), scope)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out[id] = true
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("checking experiments: %w", err)
	}
	return out, nil
}

// StartEvalExperiment moves a queued experiment to running and stores its arms
// as resolved (hashes filled in). It is a no-op for one already running.
func (s *Store) StartEvalExperiment(ctx context.Context, id int64, arms []EvalArm) error {
	j, _ := json.Marshal(arms)
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		res, err := tx.ExecContext(ctx, `UPDATE eval_experiments
			SET status = 'running', arms = $2::jsonb, started_at = COALESCE(started_at, now()), updated_at = now()
			WHERE id = $1 AND status IN ('queued','running') AND ($3::text = '*' OR tenant_id = $3)`, id, string(j), scope)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrExperimentFinished
		}
		return nil
	})
	return wrapEvalErr("starting the experiment", err)
}

// ListEvalExperimentCases returns the cases an experiment runs: those live at
// its frozen dataset version, in ID order.
func (s *Store) ListEvalExperimentCases(ctx context.Context, id int64) ([]EvalCase, error) {
	out := []EvalCase{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+evalCaseCols+evalCaseFrom+`
			JOIN eval_experiments e ON e.dataset_id = c.dataset_id AND e.tenant_id = c.tenant_id
			WHERE e.id = $1 AND c.added_in <= e.dataset_version AND (c.removed_in IS NULL OR c.removed_in > e.dataset_version)
			  AND ($2::text = '*' OR c.tenant_id = $2)
			ORDER BY c.id`, id, scope)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			c, err := scanEvalCase(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrapEval("listing the experiment's cases", err)
	}
	return out, nil
}

// EvalResultKey is the identity and outcome of a recorded result, without its
// text. The runner rebuilds what is left to do from these.
type EvalResultKey struct {
	CaseID  int64
	Arm     string
	Repeat  int
	Phase   string
	CostUSD float64
}

// ListEvalExperimentResultKeys returns every result recorded for an experiment.
func (s *Store) ListEvalExperimentResultKeys(ctx context.Context, id int64) ([]EvalResultKey, error) {
	out := []EvalResultKey{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `SELECT case_id, arm, repeat, phase, cost_usd::float8 FROM eval_results
			WHERE experiment_id = $1 AND ($2::text = '*' OR tenant_id = $2) ORDER BY id`, id, scope)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var k EvalResultKey
			var caseID sql.NullInt64
			if err := rows.Scan(&caseID, &k.Arm, &k.Repeat, &k.Phase, &k.CostUSD); err != nil {
				return err
			}
			k.CaseID = caseID.Int64
			out = append(out, k)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrapEval("listing recorded results", err)
	}
	return out, nil
}

// RecordEvalRunResult stores the result of one (case, arm, repeat) with its
// grades and adds its cost to the experiment's spend, in one transaction. The
// tenant and dataset come from the experiment. A result already recorded for the
// same slot is left alone and recorded is false, so recording is safe to repeat
// after a restart. Each grade names a grader of the experiment's dataset.
func (s *Store) RecordEvalRunResult(ctx context.Context, experimentID int64, r EvalResult, grades []EvalGrade) (recorded bool, err error) {
	if err := r.Validate(); err != nil {
		return false, err
	}
	if r.CaseID == nil {
		return false, fmt.Errorf("%w: a case is required", ErrInvalidEval)
	}
	if r.ToolCalls == nil {
		r.ToolCalls = []string{}
	}
	if len(r.Error) > 4096 {
		r.Error = strings.ToValidUTF8(r.Error[:4096], "")
	}
	calls, _ := json.Marshal(r.ToolCalls)
	err = s.scoped(ctx, func(tx dbtx, scope string) error {
		var tenant string
		var datasetID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT tenant_id, dataset_id FROM eval_experiments WHERE id = $1 AND ($2::text = '*' OR tenant_id = $2) FOR UPDATE`,
			experimentID, scope).Scan(&tenant, &datasetID); err != nil {
			return notFound(err)
		}
		err := tx.QueryRowContext(ctx, `
		INSERT INTO eval_results (tenant_id, dataset_id, case_id, experiment_id, arm, repeat, run_namespace, run_name, phase,
		       output, tool_calls, cost_usd, cost_complete, latency_ms, case_pass, error, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (experiment_id, case_id, arm, repeat) WHERE experiment_id IS NOT NULL DO NOTHING
		RETURNING id, created_at`,
			tenant, datasetID, *r.CaseID, experimentID, r.Arm, r.Repeat, r.RunNamespace, r.RunName, r.Phase,
			r.Output, string(calls), r.CostUSD, !r.CostIncomplete, r.LatencyMs, r.CasePass, r.Error, r.CreatedBy,
		).Scan(&r.ID, &r.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // already recorded
		}
		if err != nil {
			return err
		}
		recorded = true
		spend := r.CostUSD
		for _, g := range grades {
			err := insertGrade(ctx, tx, tenant, datasetID, r.ID, &g)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: a grader of the dataset was deleted during the experiment", ErrInvalidEval)
			}
			if err != nil {
				return err
			}
			spend += g.JudgeCostUSD
		}
		_, err = tx.ExecContext(ctx, `UPDATE eval_experiments SET spent_usd = spent_usd + $2, updated_at = now() WHERE id = $1`, experimentID, spend)
		return err
	})
	if err != nil {
		return false, wrapEval("recording the result", err)
	}
	return recorded, nil
}

// FinishEvalExperiment ends an experiment with a status (complete, cancelled or
// failed), its summary (JSON, may be empty) and an error text. It returns
// ErrExperimentFinished when the experiment had ended already.
func (s *Store) FinishEvalExperiment(ctx context.Context, id int64, status string, summary json.RawMessage, errText string) error {
	switch status {
	case ExperimentComplete, ExperimentCancelled, ExperimentFailed:
	default:
		return fmt.Errorf("%w: %q is not an end state", ErrInvalidEval, status)
	}
	if len(errText) > 4096 {
		errText = strings.ToValidUTF8(errText[:4096], "")
	}
	var sum any
	if len(summary) > 0 {
		sum = string(summary)
	}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		res, err := tx.ExecContext(ctx, `UPDATE eval_experiments
			SET status = $2, summary = $3::jsonb, error = $4, finished_at = now(), updated_at = now(),
			    cancel_requested = FALSE, lease_owner = '', lease_until = NULL
			WHERE id = $1 AND status IN ('queued','running','grading','awaiting_review') AND ($5::text = '*' OR tenant_id = $5)`,
			id, status, sum, errText, scope)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrExperimentFinished
		}
		return nil
	})
	return wrapEvalErr("finishing the experiment", err)
}

// wrapEvalErr is wrapEval for a call that may succeed.
func wrapEvalErr(what string, err error) error {
	if err == nil {
		return nil
	}
	return wrapEval(what, err)
}

// EvalSummaryRows returns an experiment's results in the shape the comparison
// statistics read: one row per (case, arm, repeat) with its grades and no text.
// A grader that had nothing to grade (no reference) is left out of the grades of
// a result unless it is required, when it counts as a failure, the same rule
// graders.CasePass applies.
func (s *Store) EvalSummaryRows(ctx context.Context, id int64) ([]compare.Row, error) {
	out := []compare.Row{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT r.id, r.case_id, r.arm, r.repeat, r.phase, r.cost_usd::float8, r.cost_complete, r.latency_ms,
			       g.grader_id, g.score, g.pass, g.not_graded, gr.weight, gr.required, g.status
			FROM eval_results r
			LEFT JOIN eval_grades g ON g.result_id = r.id AND g.tenant_id = r.tenant_id
			LEFT JOIN eval_graders gr ON gr.id = g.grader_id AND gr.tenant_id = g.tenant_id
			WHERE r.experiment_id = $1 AND ($2::text = '*' OR r.tenant_id = $2)
			ORDER BY r.id, g.grader_id`, id, scope)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var lastID int64 = -1
		for rows.Next() {
			var (
				rid                      int64
				caseID                   sql.NullInt64
				row                      compare.Row
				latency                  int64
				graderID                 sql.NullInt64
				score, weight            sql.NullFloat64
				pass, notGraded, require sql.NullBool
				status                   sql.NullString
			)
			if err := rows.Scan(&rid, &caseID, &row.Arm, &row.Repeat, &row.Phase, &row.CostUSD, &row.CostComplete, &latency,
				&graderID, &score, &pass, &notGraded, &weight, &require, &status); err != nil {
				return err
			}
			if rid != lastID {
				row.CaseID = caseID.Int64
				if latency > 0 {
					ms := int(latency)
					row.LatencyMS = &ms
				}
				out = append(out, row)
				lastID = rid
			}
			if !graderID.Valid || (notGraded.Bool && !require.Bool) {
				continue
			}
			g := compare.Grade{GraderID: graderID.Int64, Score: score.Float64, Pass: pass.Bool, Status: status.String,
				Weight: weight.Float64, Required: require.Bool}
			if notGraded.Bool {
				g.Score, g.Pass = 0, false
			}
			last := &out[len(out)-1]
			last.Grades = append(last.Grades, g)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrapEval("reading the experiment's results", err)
	}
	return out, nil
}
