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

// Graders of an evaluation dataset, and graded results (tables eval_graders,
// eval_results and eval_grades, migration 0014). Every call runs in the tenant
// scope of its context; a child row's tenant_id is copied from its parent inside
// the same transaction, never taken from the caller. Changing graders does not
// change the dataset's version: the version is about the cases.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/evals/graders"
)

// Limits on grader data.
const (
	MaxEvalGradersPerDataset = 50
	MaxEvalResultsPerDataset = 5000
	MaxEvalResultOutput      = 262144
	MaxEvalResultToolCalls   = 1000
	MaxEvalResultsPerPage    = 200
	defaultResultsPerPage    = 50
)

// EvalGrader is a named grader of a dataset.
type EvalGrader struct {
	ID        int64           `json:"id"`
	TenantID  string          `json:"tenantId,omitempty"`
	DatasetID int64           `json:"datasetId"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Config    json.RawMessage `json:"config"`
	// Weight is the grader's share of the case's mean score.
	Weight float64 `json:"weight"`
	// Required makes the case fail when this grader fails.
	Required  bool      `json:"required"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Spec returns the grader as the graders package runs it.
func (g EvalGrader) Spec() graders.Spec { return graders.Spec{Kind: g.Kind, Config: g.Config} }

// Validate reports why g cannot be stored, and canonicalises its config.
func (g *EvalGrader) Validate() error {
	switch {
	case !evalNameRe.MatchString(g.Name) || len(g.Name) > 63:
		return fmt.Errorf("%w: grader name must be a DNS label (lowercase letters, digits and dashes, at most 63 characters)", ErrInvalidEval)
	case g.Weight == 0:
		g.Weight = 1
	}
	if !(g.Weight > 0) || math.IsInf(g.Weight, 0) {
		return fmt.Errorf("%w: weight must be above 0", ErrInvalidEval)
	}
	if g.CreatedBy == "" {
		return fmt.Errorf("%w: created by is required", ErrInvalidEval)
	}
	cfg, err := graders.Normalize(g.Spec())
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidEval, strings.TrimPrefix(err.Error(), graders.ErrConfig.Error()+": "))
	}
	g.Config = cfg
	return nil
}

// EvalGrade is one grader's verdict on a result.
type EvalGrade struct {
	ID       int64 `json:"id,omitempty"`
	GraderID int64 `json:"graderId"`
	// Grader and Kind are the grader's name and kind. Filled on reads.
	Grader    string  `json:"grader"`
	Kind      string  `json:"kind,omitempty"`
	Score     float64 `json:"score"`
	Pass      bool    `json:"pass"`
	NotGraded bool    `json:"notGraded,omitempty"`
	Detail    string  `json:"detail"`
	// Status is GradeOK (the default), GradeError (the grader could not run),
	// GradeInvalid (an unusable judge reply) or GradePendingReview (a human has
	// not labelled it). Only an ok grade counts towards a case.
	Status string `json:"status,omitempty"`
	// JudgeCostUSD is what the judge call cost, counted in the experiment's spend.
	JudgeCostUSD float64 `json:"judgeCostUsd,omitempty"`
	// GradedBy is who labelled a human grade.
	GradedBy string `json:"gradedBy,omitempty"`
	// Inconsistent marks a pairwise grade whose two orderings disagreed.
	Inconsistent bool `json:"inconsistent,omitempty"`
}

// Grade statuses (eval_grades.status).
const (
	GradeOK            = graders.StatusOK
	GradeError         = graders.StatusError
	GradeInvalid       = graders.StatusInvalid
	GradePendingReview = graders.StatusPendingReview
)

// status returns the grade's status, ok when unset.
func (g EvalGrade) status() string {
	if g.Status == "" {
		return GradeOK
	}
	return g.Status
}

// Validate reports why a grade cannot be stored.
func (g EvalGrade) Validate() error {
	switch g.status() {
	case GradeOK, GradeError, GradeInvalid, GradePendingReview:
	default:
		return fmt.Errorf("%w: unknown grade status %q", ErrInvalidEval, g.Status)
	}
	if math.IsNaN(g.Score) || g.Score < 0 || g.Score > 1 {
		return fmt.Errorf("%w: a grade's score must be between 0 and 1", ErrInvalidEval)
	}
	if math.IsNaN(g.JudgeCostUSD) || math.IsInf(g.JudgeCostUSD, 0) || g.JudgeCostUSD < 0 || g.JudgeCostUSD >= 1e6 {
		return fmt.Errorf("%w: a grade's judge cost must be between 0 and 1000000", ErrInvalidEval)
	}
	return nil
}

// EvalResult is one graded output.
type EvalResult struct {
	ID        int64  `json:"id"`
	TenantID  string `json:"tenantId,omitempty"`
	DatasetID int64  `json:"datasetId"`
	// CaseID is the case the output answers, nil for a free-standing output.
	CaseID *int64 `json:"caseId,omitempty"`
	// Arm labels the output (a model, a revision); free text.
	Arm       string      `json:"arm,omitempty"`
	Output    string      `json:"output"`
	ToolCalls []string    `json:"toolCalls"`
	CostUSD   float64     `json:"costUsd"`
	LatencyMs int64       `json:"latencyMs"`
	CasePass  bool        `json:"casePass"`
	CreatedBy string      `json:"createdBy"`
	CreatedAt time.Time   `json:"createdAt"`
	Grades    []EvalGrade `json:"grades"`

	// The fields below are set on a result an experiment's runner wrote
	// (migration 0015) and are empty on a free-standing result.
	ExperimentID *int64 `json:"experimentId,omitempty"`
	Repeat       int    `json:"repeat,omitempty"`
	// RunNamespace and RunName name the AgentRun the output came from.
	RunNamespace string `json:"runNamespace,omitempty"`
	RunName      string `json:"runName,omitempty"`
	// Phase is the run's outcome: Succeeded, Failed, TimedOut, Cancelled or Skipped.
	Phase string `json:"phase,omitempty"`
	// CostIncomplete is true when the run's cost could not be fully captured, so
	// CostUSD is a floor.
	CostIncomplete bool   `json:"costIncomplete,omitempty"`
	Error          string `json:"error,omitempty"`
}

// Validate reports why r cannot be stored.
func (r EvalResult) Validate() error {
	switch {
	case len(r.Output) > MaxEvalResultOutput:
		return fmt.Errorf("%w: output is %d bytes, the limit is %d", ErrEvalTooLarge, len(r.Output), MaxEvalResultOutput)
	case len(r.ToolCalls) > MaxEvalResultToolCalls:
		return fmt.Errorf("%w: at most %d tool calls", ErrInvalidEval, MaxEvalResultToolCalls)
	case len(r.Arm) > 128:
		return fmt.Errorf("%w: arm is over 128 bytes", ErrInvalidEval)
	case math.IsNaN(r.CostUSD) || math.IsInf(r.CostUSD, 0) || r.CostUSD < 0 || r.CostUSD >= 1e6:
		return fmt.Errorf("%w: costUsd must be between 0 and 1000000", ErrInvalidEval)
	case r.LatencyMs < 0:
		return fmt.Errorf("%w: latencyMs must not be negative", ErrInvalidEval)
	case r.CreatedBy == "":
		return fmt.Errorf("%w: created by is required", ErrInvalidEval)
	}
	return nil
}

const evalGraderCols = `g.id, g.tenant_id, g.dataset_id, g.name, g.kind, g.config, g.weight, g.required, g.created_by, g.created_at, g.updated_at`

func scanEvalGrader(row interface{ Scan(...any) error }) (EvalGrader, error) {
	var g EvalGrader
	var cfg []byte
	err := row.Scan(&g.ID, &g.TenantID, &g.DatasetID, &g.Name, &g.Kind, &cfg, &g.Weight, &g.Required, &g.CreatedBy, &g.CreatedAt, &g.UpdatedAt)
	g.Config = cfg
	return g, err
}

// UpsertEvalGrader creates the grader or replaces the one of that name in the
// dataset. It reports whether it was created. The grader's config is checked
// and canonicalised; an invalid one is an ErrInvalidEval.
func (s *Store) UpsertEvalGrader(ctx context.Context, datasetID int64, g EvalGrader) (EvalGrader, bool, error) {
	if err := g.Validate(); err != nil {
		return EvalGrader{}, false, err
	}
	var created bool
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, _, _, err := lockDataset(ctx, tx, scope, datasetID)
		if err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM eval_graders WHERE dataset_id = $1 AND tenant_id = $2 AND name <> $3`,
			datasetID, tenant, g.Name).Scan(&n); err != nil {
			return err
		}
		if n >= MaxEvalGradersPerDataset {
			return fmt.Errorf("%w: a dataset holds at most %d graders", ErrDatasetFull, MaxEvalGradersPerDataset)
		}
		var xmaxZero bool
		err = tx.QueryRowContext(ctx, `
		INSERT INTO eval_graders (tenant_id, dataset_id, name, kind, config, weight, required, created_by)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8)
		ON CONFLICT (tenant_id, dataset_id, name) DO UPDATE
		   SET kind = EXCLUDED.kind, config = EXCLUDED.config, weight = EXCLUDED.weight,
		       required = EXCLUDED.required, updated_at = now()
		RETURNING id, created_by, created_at, updated_at, (xmax = 0)`,
			tenant, datasetID, g.Name, g.Kind, string(g.Config), g.Weight, g.Required, g.CreatedBy,
		).Scan(&g.ID, &g.CreatedBy, &g.CreatedAt, &g.UpdatedAt, &xmaxZero)
		g.TenantID, g.DatasetID, created = tenant, datasetID, xmaxZero
		return err
	})
	if err != nil {
		return EvalGrader{}, false, wrapEval("saving the grader", err)
	}
	return g, created, nil
}

// ListEvalGraders returns a dataset's graders by name. ErrEvalNotFound when the
// dataset is not in the call's scope.
func (s *Store) ListEvalGraders(ctx context.Context, datasetID int64) ([]EvalGrader, error) {
	out := []EvalGrader{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		if err := visibleDataset(ctx, tx, scope, datasetID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT `+evalGraderCols+` FROM eval_graders g
			 WHERE g.dataset_id = $1 AND ($2::text = '*' OR g.tenant_id = $2) ORDER BY g.name`, datasetID, scope)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			g, err := scanEvalGrader(rows)
			if err != nil {
				return err
			}
			out = append(out, g)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrapEval("listing graders", err)
	}
	return out, nil
}

// GetEvalGrader returns one grader, or ErrEvalNotFound.
func (s *Store) GetEvalGrader(ctx context.Context, datasetID int64, name string) (EvalGrader, error) {
	var g EvalGrader
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		g, err = scanEvalGrader(tx.QueryRowContext(ctx,
			`SELECT `+evalGraderCols+` FROM eval_graders g
			 WHERE g.dataset_id = $1 AND g.name = $2 AND ($3::text = '*' OR g.tenant_id = $3)`, datasetID, name, scope))
		return notFound(err)
	})
	if err != nil {
		return EvalGrader{}, wrapEval("loading the grader", err)
	}
	return g, nil
}

// DeleteEvalGrader removes a grader and, with it, its grades. It returns the
// grader as it was.
func (s *Store) DeleteEvalGrader(ctx context.Context, datasetID int64, name string) (EvalGrader, error) {
	var g EvalGrader
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		g, err = scanEvalGrader(tx.QueryRowContext(ctx,
			`SELECT `+evalGraderCols+` FROM eval_graders g
			 WHERE g.dataset_id = $1 AND g.name = $2 AND ($3::text = '*' OR g.tenant_id = $3) FOR UPDATE`, datasetID, name, scope))
		if err != nil {
			return notFound(err)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM eval_graders WHERE id = $1`, g.ID)
		return err
	})
	if err != nil {
		return EvalGrader{}, wrapEval("deleting the grader", err)
	}
	return g, nil
}

// visibleDataset checks that the scope can see a dataset, without locking it.
func visibleDataset(ctx context.Context, tx dbtx, scope string, id int64) error {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM eval_datasets WHERE id = $1 AND ($2::text = '*' OR tenant_id = $2)`, id, scope).Scan(&one)
	return notFound(err)
}

// SaveEvalResult stores a graded output with its grades. Each grade names a
// grader of the dataset by GraderID; one that is not (a grader deleted since it
// was read) is an ErrInvalidEval and nothing is stored. A CaseID must be a live
// case of the dataset. TenantID and CreatedAt on the input are ignored.
func (s *Store) SaveEvalResult(ctx context.Context, datasetID int64, r EvalResult, grades []EvalGrade) (EvalResult, error) {
	if err := r.Validate(); err != nil {
		return EvalResult{}, err
	}
	if r.ToolCalls == nil {
		r.ToolCalls = []string{}
	}
	calls, _ := json.Marshal(r.ToolCalls)
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, _, _, err := lockDataset(ctx, tx, scope, datasetID)
		if err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM eval_results WHERE dataset_id = $1 AND tenant_id = $2 AND experiment_id IS NULL`,
			datasetID, tenant).Scan(&n); err != nil {
			return err
		}
		if n >= MaxEvalResultsPerDataset {
			return fmt.Errorf("%w: a dataset keeps at most %d graded results", ErrDatasetFull, MaxEvalResultsPerDataset)
		}
		if r.CaseID != nil {
			var one int
			err := tx.QueryRowContext(ctx,
				`SELECT 1 FROM eval_cases WHERE id = $1 AND dataset_id = $2 AND tenant_id = $3 AND removed_in IS NULL`,
				*r.CaseID, datasetID, tenant).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: the case is not a live case of this dataset", ErrInvalidEval)
			}
			if err != nil {
				return err
			}
		}
		r.TenantID, r.DatasetID = tenant, datasetID
		if err := tx.QueryRowContext(ctx, `
		INSERT INTO eval_results (tenant_id, dataset_id, case_id, arm, output, tool_calls, cost_usd, latency_ms, case_pass, created_by)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10)
		RETURNING id, created_at`,
			tenant, datasetID, r.CaseID, r.Arm, r.Output, string(calls), r.CostUSD, r.LatencyMs, r.CasePass, r.CreatedBy,
		).Scan(&r.ID, &r.CreatedAt); err != nil {
			return err
		}
		r.Grades = make([]EvalGrade, 0, len(grades))
		for _, g := range grades {
			err := insertGrade(ctx, tx, tenant, datasetID, r.ID, &g)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: a grader changed while grading; grade again", ErrInvalidEval)
			}
			if err != nil {
				return err
			}
			r.Grades = append(r.Grades, g)
		}
		return nil
	})
	if err != nil {
		return EvalResult{}, wrapEval("saving the result", err)
	}
	return r, nil
}

// insertGrade stores one grade of a result under the tenant of its parents,
// naming a grader of the dataset. sql.ErrNoRows means the grader is not one.
func insertGrade(ctx context.Context, tx dbtx, tenant string, datasetID, resultID int64, g *EvalGrade) error {
	if err := g.Validate(); err != nil {
		return err
	}
	g.Status = g.status()
	detail := g.Detail
	if len(detail) > 4096 {
		detail = strings.ToValidUTF8(detail[:4096], "")
	}
	return tx.QueryRowContext(ctx, `
		INSERT INTO eval_grades (tenant_id, result_id, grader_id, score, pass, not_graded, rationale, status, judge_cost_usd, graded_by, inconsistent)
		SELECT $1, $2, id, $4, $5, $6, $7, $8, $9, $10, $11 FROM eval_graders WHERE id = $3 AND dataset_id = $12 AND tenant_id = $1
		RETURNING id`,
		tenant, resultID, g.GraderID, g.Score, g.Pass, g.NotGraded, detail, g.Status, g.JudgeCostUSD, g.GradedBy, g.Inconsistent, datasetID).Scan(&g.ID)
}

const evalResultCols = `r.id, r.tenant_id, r.dataset_id, r.case_id, r.arm, r.output, r.tool_calls,
	r.cost_usd::float8, r.latency_ms, r.case_pass, r.created_by, r.created_at,
	r.experiment_id, r.repeat, r.run_namespace, r.run_name, r.phase, NOT r.cost_complete, r.error`

func scanEvalResult(row interface{ Scan(...any) error }) (EvalResult, error) {
	var r EvalResult
	var caseID, experimentID sql.NullInt64
	var calls []byte
	err := row.Scan(&r.ID, &r.TenantID, &r.DatasetID, &caseID, &r.Arm, &r.Output, &calls,
		&r.CostUSD, &r.LatencyMs, &r.CasePass, &r.CreatedBy, &r.CreatedAt,
		&experimentID, &r.Repeat, &r.RunNamespace, &r.RunName, &r.Phase, &r.CostIncomplete, &r.Error)
	if err != nil {
		return r, err
	}
	if experimentID.Valid {
		r.ExperimentID = &experimentID.Int64
	}
	if caseID.Valid {
		r.CaseID = &caseID.Int64
	}
	r.ToolCalls = []string{}
	if len(calls) > 0 {
		if err := json.Unmarshal(calls, &r.ToolCalls); err != nil {
			return r, fmt.Errorf("reading tool calls: %w", err)
		}
	}
	r.Grades = []EvalGrade{}
	return r, nil
}

// loadGrades fills the grades of each result, in grader name order.
func loadGrades(ctx context.Context, tx dbtx, scope string, results []EvalResult) error {
	if len(results) == 0 {
		return nil
	}
	index := make(map[int64]*EvalResult, len(results))
	ids := make([]int64, len(results))
	for i := range results {
		index[results[i].ID] = &results[i]
		ids[i] = results[i].ID
	}
	idsJSON, _ := json.Marshal(ids)
	rows, err := tx.QueryContext(ctx, `
		SELECT e.id, e.result_id, e.grader_id, g.name, g.kind, e.score, e.pass, e.not_graded, e.rationale,
		       e.status, e.judge_cost_usd::float8, e.graded_by, e.inconsistent
		FROM eval_grades e JOIN eval_graders g ON g.id = e.grader_id AND g.tenant_id = e.tenant_id
		WHERE e.result_id IN (SELECT jsonb_array_elements_text($1::jsonb)::bigint)
		  AND ($2::text = '*' OR e.tenant_id = $2)
		ORDER BY e.result_id, g.name`, string(idsJSON), scope)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var g EvalGrade
		var resultID int64
		if err := rows.Scan(&g.ID, &resultID, &g.GraderID, &g.Grader, &g.Kind, &g.Score, &g.Pass, &g.NotGraded, &g.Detail,
			&g.Status, &g.JudgeCostUSD, &g.GradedBy, &g.Inconsistent); err != nil {
			return err
		}
		if r := index[resultID]; r != nil {
			r.Grades = append(r.Grades, g)
		}
	}
	return rows.Err()
}

// GetEvalResult returns one result with its grades, or ErrEvalNotFound.
func (s *Store) GetEvalResult(ctx context.Context, id int64) (EvalResult, error) {
	var out []EvalResult
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		r, err := scanEvalResult(tx.QueryRowContext(ctx,
			`SELECT `+evalResultCols+` FROM eval_results r WHERE r.id = $1 AND ($2::text = '*' OR r.tenant_id = $2)`, id, scope))
		if err != nil {
			return notFound(err)
		}
		out = []EvalResult{r}
		return loadGrades(ctx, tx, scope, out)
	})
	if err != nil {
		return EvalResult{}, wrapEval("loading the result", err)
	}
	return out[0], nil
}

// EvalResultQuery pages ListEvalResults.
type EvalResultQuery struct {
	// After is a cursor: only results with an ID above it.
	After int64
	// Limit is the page size: default 50, at most MaxEvalResultsPerPage.
	Limit int
	// CaseID limits the list to one case's results when above 0.
	CaseID int64
}

// ListEvalResults returns a dataset's free-standing results in ID order, with
// their grades. An experiment's results are listed by ListEvalExperimentResults.
func (s *Store) ListEvalResults(ctx context.Context, datasetID int64, q EvalResultQuery) ([]EvalResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultResultsPerPage
	}
	if limit > MaxEvalResultsPerPage {
		limit = MaxEvalResultsPerPage
	}
	out := []EvalResult{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		if err := visibleDataset(ctx, tx, scope, datasetID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT `+evalResultCols+` FROM eval_results r
			 WHERE r.dataset_id = $1 AND r.experiment_id IS NULL AND r.id > $2 AND ($3::bigint = 0 OR r.case_id = $3) AND ($4::text = '*' OR r.tenant_id = $4)
			 ORDER BY r.id LIMIT $5`, datasetID, q.After, q.CaseID, scope, limit)
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
		return nil, wrapEval("listing results", err)
	}
	return out, nil
}
