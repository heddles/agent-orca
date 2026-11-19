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
	"fmt"
	"math"
	"time"
)

// Defaults and limits of a publish gate.
const (
	DefaultGateMargin    = 0.05
	DefaultGateMinCases  = 30
	DefaultGateBudgetUSD = 5.0
	MaxGateMinCases      = 2000
)

// ErrDatasetGated means a publish gate rests on the dataset, so it cannot be
// deleted until the gate is changed or removed.
var ErrDatasetGated = errors.New("a publish gate rests on this dataset: change or delete the gate first")

// EvalGate is the publish gate of one agent (table eval_gates, migration 0017):
// the evaluation an agent must have passed before it can be published.
type EvalGate struct {
	ID        int64  `json:"id"`
	TenantID  string `json:"tenantId,omitempty"`
	Namespace string `json:"namespace"`
	Agent     string `json:"agent"`
	// DatasetID is the dataset the evaluation runs on.
	DatasetID int64 `json:"datasetId"`
	// BaselineRevision is the saved revision of the agent the candidate is
	// compared with (the version that is live now).
	BaselineRevision int `json:"baselineRevision"`
	// Margin is the non-inferiority margin: how much lower than the baseline the
	// candidate's pass rate may plausibly be and still pass the gate.
	Margin float64 `json:"margin"`
	// MinCases is the fewest paired cases an evaluation needs to count.
	MinCases int `json:"minCases"`
	// MinPassRate, when set, is a floor on the candidate's own pass rate.
	MinPassRate *float64 `json:"minPassRate,omitempty"`
	// MaxRegressions, when set, is the most cases that may go from pass to fail.
	MaxRegressions *int `json:"maxRegressions,omitempty"`
	// BudgetUSD is the budget of the experiment the evaluate action starts.
	BudgetUSD float64   `json:"budgetUsd"`
	Enabled   bool      `json:"enabled"`
	UpdatedBy string    `json:"updatedBy"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Normalize fills the defaults of a gate being saved.
func (g *EvalGate) Normalize() {
	if g.Margin == 0 {
		g.Margin = DefaultGateMargin
	}
	if g.MinCases == 0 {
		g.MinCases = DefaultGateMinCases
	}
	if g.BudgetUSD == 0 {
		g.BudgetUSD = DefaultGateBudgetUSD
	}
}

// Validate reports why g cannot be stored. Call Normalize first.
func (g EvalGate) Validate() error {
	switch {
	case g.Namespace == "" || !evalObjectNameRe.MatchString(g.Agent):
		return fmt.Errorf("%w: a gate belongs to a namespace and an agent", ErrInvalidEval)
	case g.DatasetID <= 0:
		return fmt.Errorf("%w: datasetId is required", ErrInvalidEval)
	case g.BaselineRevision < 1:
		return fmt.Errorf("%w: baselineRevision must be a saved revision of the agent (1 or more)", ErrInvalidEval)
	case !(g.Margin > 0 && g.Margin < 1):
		return fmt.Errorf("%w: margin must be above 0 and below 1", ErrInvalidEval)
	case g.MinCases < 1 || g.MinCases > MaxGateMinCases:
		return fmt.Errorf("%w: minCases must be between 1 and %d", ErrInvalidEval, MaxGateMinCases)
	case g.MinPassRate != nil && !(*g.MinPassRate >= 0 && *g.MinPassRate <= 1):
		return fmt.Errorf("%w: minPassRate must be between 0 and 1", ErrInvalidEval)
	case g.MaxRegressions != nil && *g.MaxRegressions < 0:
		return fmt.Errorf("%w: maxRegressions must not be negative", ErrInvalidEval)
	case !validMoney(g.BudgetUSD):
		return fmt.Errorf("%w: budgetUsd must be above 0", ErrInvalidEval)
	case g.UpdatedBy == "":
		return fmt.Errorf("%w: updated by is required", ErrInvalidEval)
	}
	return nil
}

const evalGateCols = `g.id, g.tenant_id, g.namespace, g.agent, g.dataset_id, g.baseline_revision, g.margin::float8, g.min_cases,
	g.min_pass_rate::float8, g.max_regressions, g.budget_usd::float8, g.enabled, g.updated_by, g.created_at, g.updated_at`

func scanEvalGate(row interface{ Scan(...any) error }) (EvalGate, error) {
	var g EvalGate
	var minRate sql.NullFloat64
	var maxReg sql.NullInt64
	err := row.Scan(&g.ID, &g.TenantID, &g.Namespace, &g.Agent, &g.DatasetID, &g.BaselineRevision, &g.Margin, &g.MinCases,
		&minRate, &maxReg, &g.BudgetUSD, &g.Enabled, &g.UpdatedBy, &g.CreatedAt, &g.UpdatedAt)
	if minRate.Valid {
		v := math.Round(minRate.Float64*1e5) / 1e5
		g.MinPassRate = &v
	}
	if maxReg.Valid {
		v := int(maxReg.Int64)
		g.MaxRegressions = &v
	}
	g.Margin = math.Round(g.Margin*1e5) / 1e5
	return g, err
}

// GetEvalGate returns the gate of an agent, or ErrEvalNotFound when it has none.
func (s *Store) GetEvalGate(ctx context.Context, namespace, agent string) (EvalGate, error) {
	var g EvalGate
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		g, err = scanEvalGate(tx.QueryRowContext(ctx,
			`SELECT `+evalGateCols+` FROM eval_gates g WHERE g.namespace = $1 AND g.agent = $2 AND ($3::text = '*' OR g.tenant_id = $3)`,
			namespace, agent, scope))
		return notFound(err)
	})
	if err != nil {
		return EvalGate{}, wrapEval("reading the gate", err)
	}
	return g, nil
}

// PutEvalGate creates or replaces the gate of an agent. It returns the new gate
// and the one it replaced (nil when there was none), so the caller can audit
// the change. The tenant comes from the dataset, which must be in the gate's
// namespace. A dataset outside the call's scope is ErrEvalNotFound.
func (s *Store) PutEvalGate(ctx context.Context, g EvalGate) (EvalGate, *EvalGate, error) {
	g.Normalize()
	if err := g.Validate(); err != nil {
		return EvalGate{}, nil, err
	}
	var out EvalGate
	var old *EvalGate
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		tenant, ns, _, err := lockDataset(ctx, tx, scope, g.DatasetID)
		if err != nil {
			return err
		}
		if ns != g.Namespace {
			return fmt.Errorf("%w: the dataset is in workspace %s, not %s", ErrInvalidEval, ns, g.Namespace)
		}
		prev, err := scanEvalGate(tx.QueryRowContext(ctx,
			`SELECT `+evalGateCols+` FROM eval_gates g WHERE g.tenant_id = $1 AND g.namespace = $2 AND g.agent = $3 FOR UPDATE`,
			tenant, g.Namespace, g.Agent))
		switch {
		case err == nil:
			old = &prev
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		out, err = scanEvalGate(tx.QueryRowContext(ctx, `
			INSERT INTO eval_gates AS g (tenant_id, namespace, agent, dataset_id, baseline_revision, margin, min_cases,
			       min_pass_rate, max_regressions, budget_usd, enabled, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (tenant_id, namespace, agent) DO UPDATE SET dataset_id = EXCLUDED.dataset_id,
			       baseline_revision = EXCLUDED.baseline_revision, margin = EXCLUDED.margin, min_cases = EXCLUDED.min_cases,
			       min_pass_rate = EXCLUDED.min_pass_rate, max_regressions = EXCLUDED.max_regressions,
			       budget_usd = EXCLUDED.budget_usd, enabled = EXCLUDED.enabled, updated_by = EXCLUDED.updated_by,
			       updated_at = now()
			RETURNING `+evalGateCols,
			tenant, g.Namespace, g.Agent, g.DatasetID, g.BaselineRevision, g.Margin, g.MinCases,
			g.MinPassRate, g.MaxRegressions, g.BudgetUSD, g.Enabled, g.UpdatedBy))
		return err
	})
	if err != nil {
		return EvalGate{}, nil, wrapEval("saving the gate", err)
	}
	return out, old, nil
}

// DeleteEvalGate removes the gate of an agent and returns it, or ErrEvalNotFound.
func (s *Store) DeleteEvalGate(ctx context.Context, namespace, agent string) (EvalGate, error) {
	var g EvalGate
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		g, err = scanEvalGate(tx.QueryRowContext(ctx,
			`SELECT `+evalGateCols+` FROM eval_gates g WHERE g.namespace = $1 AND g.agent = $2 AND ($3::text = '*' OR g.tenant_id = $3) FOR UPDATE`,
			namespace, agent, scope))
		if err != nil {
			return notFound(err)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM eval_gates WHERE id = $1`, g.ID)
		return err
	})
	if err != nil {
		return EvalGate{}, wrapEval("deleting the gate", err)
	}
	return g, nil
}

// GateEvidence is the latest finished gate experiment of an agent, with what
// the publish check needs besides the experiment itself.
type GateEvidence struct {
	Experiment EvalExperiment
	// GradersChanged is true when a grader of the experiment's dataset was added,
	// changed or removed after the experiment was queued: the bar it was graded
	// against is not the bar now. (Changing graders does not change a dataset's
	// version, so the version alone cannot say.)
	GradersChanged bool
}

// LatestGateExperiment returns the latest complete experiment with purpose gate
// whose candidate arm (the second) is the agent, and whether graders changed
// since. The latest counts, not the best, so an author cannot rerun until lucky.
// found is false when there is none.
func (s *Store) LatestGateExperiment(ctx context.Context, namespace, agent string) (ev GateEvidence, found bool, err error) {
	err = s.scoped(ctx, func(tx dbtx, scope string) error {
		e, err := scanEvalExperiment(tx.QueryRowContext(ctx, `
			SELECT `+evalExpCols+` FROM eval_experiments e
			WHERE e.namespace = $1 AND e.purpose = 'gate' AND e.status = 'complete'
			  AND e.arms->1->>'agent' = $2 AND ($3::text = '*' OR e.tenant_id = $3)
			ORDER BY e.id DESC LIMIT 1`, namespace, agent, scope))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found, ev.Experiment = true, e
		// A grader added or edited since the experiment was queued has a newer
		// updated_at. A grader removed took its grades with it, so the experiment
		// has grades from fewer graders than the dataset has now.
		return tx.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM eval_graders gr WHERE gr.dataset_id = $1 AND gr.tenant_id = $2 AND gr.updated_at > $3)
			    OR (SELECT count(*) FROM eval_graders gr WHERE gr.dataset_id = $1 AND gr.tenant_id = $2)
			     <> (SELECT count(DISTINCT g.grader_id) FROM eval_grades g JOIN eval_results r ON r.id = g.result_id AND r.tenant_id = g.tenant_id
			          WHERE r.experiment_id = $4 AND r.tenant_id = $2)`,
			e.DatasetID, e.TenantID, e.CreatedAt, e.ID).Scan(&ev.GradersChanged)
	})
	if err != nil {
		return GateEvidence{}, false, wrapEval("reading the gate experiment", err)
	}
	return ev, found, nil
}

// gateUsesDataset reports whether a gate rests on the dataset.
func gateUsesDataset(ctx context.Context, tx dbtx, id int64) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM eval_gates WHERE dataset_id = $1`, id).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}
