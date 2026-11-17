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

// Human review of evaluation results and the judge's bookkeeping (tables
// eval_review_assignments and the grade columns of migration 0016).
//
// The review queue is blind: an item holds the case, the output and what the
// grader asks of the reviewer, and nothing that says which arm, repeat or run
// the output came from. The queue is ordered by a random key made when the item
// is created, so its order says nothing either.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/evals/graders"
)

// ErrReviewClosed: the assignment has been labelled or closed already.
var ErrReviewClosed = errors.New("the review item has been labelled or closed")

// Assignment statuses (eval_review_assignments.status).
const (
	ReviewPending = "pending"
	ReviewDone    = "done"
	ReviewClosed  = "closed"
)

// Limits of the review queue.
const (
	MaxReviewComment     = 2048
	DefaultReviewPage    = 20
	MaxReviewPage        = 50
	reviewOrderKeyBound  = 9e15
	maxReviewCasesSample = 5000
)

// EvalReviewItem is one thing for a human to grade. It is blind: nothing in it
// names the arm, the repeat, the run, its cost or its tools.
type EvalReviewItem struct {
	// ID is the assignment's ID, not the result's.
	ID           int64  `json:"id"`
	ExperimentID int64  `json:"experimentId"`
	CaseID       int64  `json:"caseId"`
	Grader       string `json:"grader"`
	// Scale is passfail or 1to5; Instructions say what to look for.
	Scale        string  `json:"scale"`
	Instructions string  `json:"instructions,omitempty"`
	PassAt       float64 `json:"passAt"`
	Input        string  `json:"input"`
	Reference    string  `json:"reference,omitempty"`
	Output       string  `json:"output"`
	Status       string  `json:"status"`
	// The fields below are set once the item is labelled.
	Reviewer   string     `json:"reviewer,omitempty"`
	LabelledAt *time.Time `json:"labelledAt,omitempty"`
	Score      *float64   `json:"score,omitempty"`
	Pass       *bool      `json:"pass,omitempty"`
	Comment    string     `json:"comment,omitempty"`
}

// EvalReviewLabel is a reviewer's answer.
type EvalReviewLabel struct {
	// Pass is the answer on the passfail scale.
	Pass *bool `json:"pass,omitempty"`
	// Stars is the answer on the 1 to 5 scale.
	Stars int `json:"stars,omitempty"`
	// Comment is optional, at most MaxReviewComment bytes.
	Comment string `json:"comment,omitempty"`
}

// EvalReviewProgress counts an experiment's review items by status.
type EvalReviewProgress struct {
	Pending int `json:"pending"`
	Done    int `json:"done"`
	Closed  int `json:"closed"`
}

// PrepareEvalReview opens the review stage of an experiment whose runs are all
// recorded: for each human grader it picks the cases to review (all, or the
// grader's sample of them, at random), makes one assignment per result of those
// cases, and drops the pending grade of every result left out, which then has
// no grade from that grader. It is safe to repeat: a grader that already has
// assignments is left alone. It returns the number of assignments still pending.
func (s *Store) PrepareEvalReview(ctx context.Context, experimentID int64) (pending int, err error) {
	err = s.scoped(ctx, func(tx dbtx, scope string) error {
		var tenant string
		var datasetID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT tenant_id, dataset_id FROM eval_experiments WHERE id = $1 AND ($2::text = '*' OR tenant_id = $2) FOR UPDATE`,
			experimentID, scope).Scan(&tenant, &datasetID); err != nil {
			return notFound(err)
		}
		type humanGrader struct {
			id     int64
			sample int
		}
		var hg []humanGrader
		rows, err := tx.QueryContext(ctx, `SELECT id, config FROM eval_graders WHERE dataset_id = $1 AND tenant_id = $2 AND kind = $3 ORDER BY id`,
			datasetID, tenant, graders.KindHuman)
		if err != nil {
			return err
		}
		for rows.Next() {
			var g humanGrader
			var cfg []byte
			if err := rows.Scan(&g.id, &cfg); err != nil {
				_ = rows.Close()
				return err
			}
			if c, perr := graders.ParseHuman(cfg); perr == nil {
				g.sample = c.Sample
			}
			hg = append(hg, g)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		_ = rows.Close()
		for _, g := range hg {
			if err := prepareGraderReview(ctx, tx, tenant, experimentID, g.id, g.sample); err != nil {
				return err
			}
		}
		return tx.QueryRowContext(ctx,
			`SELECT count(*) FROM eval_review_assignments WHERE experiment_id = $1 AND tenant_id = $2 AND status = 'pending'`,
			experimentID, tenant).Scan(&pending)
	})
	if err != nil {
		return 0, wrapEval("opening the review", err)
	}
	return pending, nil
}

func prepareGraderReview(ctx context.Context, tx dbtx, tenant string, experimentID, graderID int64, sample int) error {
	var existing int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM eval_review_assignments WHERE experiment_id = $1 AND grader_id = $2 AND tenant_id = $3`,
		experimentID, graderID, tenant).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}
	if sample > 0 {
		rows, err := tx.QueryContext(ctx, `
			DELETE FROM eval_grades g USING eval_results r
			 WHERE g.result_id = r.id AND g.tenant_id = r.tenant_id AND g.tenant_id = $3
			   AND r.experiment_id = $1 AND g.grader_id = $2 AND g.status = 'pending_review'
			   AND r.case_id NOT IN (
			       SELECT case_id FROM (
			           SELECT DISTINCT r2.case_id FROM eval_results r2
			             JOIN eval_grades g2 ON g2.result_id = r2.id AND g2.tenant_id = r2.tenant_id
			            WHERE r2.experiment_id = $1 AND g2.grader_id = $2 AND g2.status = 'pending_review' AND r2.tenant_id = $3) d
			       ORDER BY random() LIMIT $4)
			RETURNING g.result_id`, experimentID, graderID, tenant, min(sample, maxReviewCasesSample))
		if err != nil {
			return err
		}
		var dropped []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			dropped = append(dropped, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		_ = rows.Close()
		for _, id := range dropped {
			if err := recomputeCasePass(ctx, tx, tenant, id); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO eval_review_assignments (tenant_id, experiment_id, result_id, grader_id, order_key)
		SELECT g.tenant_id, $1, g.result_id, g.grader_id, floor(random() * $4)::bigint
		  FROM eval_grades g JOIN eval_results r ON r.id = g.result_id AND r.tenant_id = g.tenant_id
		 WHERE r.experiment_id = $1 AND g.grader_id = $2 AND g.status = 'pending_review' AND g.tenant_id = $3
		ON CONFLICT (result_id, grader_id) DO NOTHING`, experimentID, graderID, tenant, reviewOrderKeyBound)
	return err
}

// AwaitEvalReview moves a running experiment to awaiting_review and stores the
// summary of the results graded so far. The runner keeps polling it: when no
// assignment is pending any more it ends the experiment.
func (s *Store) AwaitEvalReview(ctx context.Context, id int64, summary json.RawMessage) error {
	var sum any
	if len(summary) > 0 {
		sum = string(summary)
	}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		res, err := tx.ExecContext(ctx, `UPDATE eval_experiments
			SET status = 'awaiting_review', summary = $2::jsonb, updated_at = now(), lease_owner = '', lease_until = NULL
			WHERE id = $1 AND status IN ('queued','running','grading','awaiting_review') AND ($3::text = '*' OR tenant_id = $3)`, id, sum, scope)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrExperimentFinished
		}
		return nil
	})
	return wrapEvalErr("awaiting review", err)
}

// PendingEvalReviews counts the assignments of an experiment still waiting for a label.
func (s *Store) PendingEvalReviews(ctx context.Context, experimentID int64) (int, error) {
	var n int
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM eval_review_assignments
			WHERE experiment_id = $1 AND status = 'pending' AND ($2::text = '*' OR tenant_id = $2)`, experimentID, scope).Scan(&n)
	})
	if err != nil {
		return 0, wrapEval("counting pending reviews", err)
	}
	return n, nil
}

// EvalReviewProgressOf counts an experiment's review items by status.
func (s *Store) EvalReviewProgressOf(ctx context.Context, experimentID int64) (EvalReviewProgress, error) {
	var p EvalReviewProgress
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM eval_experiments WHERE id = $1 AND ($2::text = '*' OR tenant_id = $2)`,
			experimentID, scope).Scan(&one); err != nil {
			return notFound(err)
		}
		rows, err := tx.QueryContext(ctx, `SELECT status, count(*) FROM eval_review_assignments
			WHERE experiment_id = $1 AND ($2::text = '*' OR tenant_id = $2) GROUP BY status`, experimentID, scope)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var st string
			var n int
			if err := rows.Scan(&st, &n); err != nil {
				return err
			}
			switch st {
			case ReviewPending:
				p.Pending = n
			case ReviewDone:
				p.Done = n
			case ReviewClosed:
				p.Closed = n
			}
		}
		return rows.Err()
	})
	if err != nil {
		return EvalReviewProgress{}, wrapEval("counting reviews", err)
	}
	return p, nil
}

// CloseEvalReview closes every pending assignment of an experiment. The grades
// stay pending_review, so those results are left out of the comparison, never
// counted as passes. It returns how many it closed.
func (s *Store) CloseEvalReview(ctx context.Context, experimentID int64) (int, error) {
	var n int64
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM eval_experiments WHERE id = $1 AND ($2::text = '*' OR tenant_id = $2) FOR UPDATE`,
			experimentID, scope).Scan(&status); err != nil {
			return notFound(err)
		}
		if status != ExperimentReview {
			return ErrExperimentFinished
		}
		res, err := tx.ExecContext(ctx, `UPDATE eval_review_assignments SET status = 'closed'
			WHERE experiment_id = $1 AND status = 'pending' AND ($2::text = '*' OR tenant_id = $2)`, experimentID, scope)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, wrapEval("closing the review", err)
	}
	return int(n), nil
}

// EvalReviewQuery selects review items.
type EvalReviewQuery struct {
	// Status limits the list to pending, done or closed; "" lists all.
	Status string
	// Limit is the page size, default DefaultReviewPage, at most MaxReviewPage.
	Limit int
}

const reviewSelect = `
	SELECT a.id, a.experiment_id, a.status, a.reviewer, a.labelled_at,
	       g.name, g.config, r.case_id, c.input, COALESCE(c.reference, ''), r.output,
	       ge.score, ge.pass, ge.rationale
	  FROM eval_review_assignments a
	  JOIN eval_graders g  ON g.id = a.grader_id AND g.tenant_id = a.tenant_id
	  JOIN eval_results r  ON r.id = a.result_id AND r.tenant_id = a.tenant_id
	  JOIN eval_cases c    ON c.id = r.case_id AND c.tenant_id = r.tenant_id
	  JOIN eval_grades ge  ON ge.result_id = a.result_id AND ge.grader_id = a.grader_id AND ge.tenant_id = a.tenant_id`

func scanReviewItem(row interface{ Scan(...any) error }) (EvalReviewItem, error) {
	var it EvalReviewItem
	var cfg []byte
	var labelled sql.NullTime
	var score float64
	var pass bool
	var comment string
	if err := row.Scan(&it.ID, &it.ExperimentID, &it.Status, &it.Reviewer, &labelled,
		&it.Grader, &cfg, &it.CaseID, &it.Input, &it.Reference, &it.Output, &score, &pass, &comment); err != nil {
		return it, err
	}
	hc, err := graders.ParseHuman(cfg)
	if err != nil {
		return it, fmt.Errorf("reading the human grader's config: %w", err)
	}
	it.Scale, it.Instructions, it.PassAt = hc.Scale, hc.Instructions, hc.PassScore()
	if it.Status == ReviewDone {
		it.Score, it.Pass, it.Comment = &score, &pass, comment
		if labelled.Valid {
			it.LabelledAt = &labelled.Time
		}
	}
	return it, nil
}

// ListEvalReviews returns an experiment's review items in their random order.
// ErrEvalNotFound when the experiment is not in the call's scope.
func (s *Store) ListEvalReviews(ctx context.Context, experimentID int64, q EvalReviewQuery) ([]EvalReviewItem, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultReviewPage
	}
	limit = min(limit, MaxReviewPage)
	out := []EvalReviewItem{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM eval_experiments WHERE id = $1 AND ($2::text = '*' OR tenant_id = $2)`,
			experimentID, scope).Scan(&one); err != nil {
			return notFound(err)
		}
		rows, err := tx.QueryContext(ctx, reviewSelect+`
			WHERE a.experiment_id = $1 AND ($2::text = '' OR a.status = $2) AND ($3::text = '*' OR a.tenant_id = $3)
			ORDER BY a.order_key, a.id LIMIT $4`, experimentID, q.Status, scope, limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			it, err := scanReviewItem(rows)
			if err != nil {
				return err
			}
			out = append(out, it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrapEval("listing review items", err)
	}
	return out, nil
}

// GetEvalReview returns one review item, or ErrEvalNotFound.
func (s *Store) GetEvalReview(ctx context.Context, id int64) (EvalReviewItem, error) {
	var it EvalReviewItem
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var err error
		it, err = scanReviewItem(tx.QueryRowContext(ctx, reviewSelect+` WHERE a.id = $1 AND ($2::text = '*' OR a.tenant_id = $2)`, id, scope))
		return notFound(err)
	})
	if err != nil {
		return EvalReviewItem{}, wrapEval("loading the review item", err)
	}
	return it, nil
}

// SubmitEvalReview stores a reviewer's label: the pending grade becomes an ok
// grade graded by reviewer, the assignment is done, and the result's case
// verdict is worked out again. One label per item: a second is ErrReviewClosed.
func (s *Store) SubmitEvalReview(ctx context.Context, id int64, label EvalReviewLabel, reviewer string) (EvalReviewItem, error) {
	if reviewer == "" {
		return EvalReviewItem{}, fmt.Errorf("%w: a reviewer is required", ErrInvalidEval)
	}
	if len(label.Comment) > MaxReviewComment {
		return EvalReviewItem{}, fmt.Errorf("%w: the comment is over %d bytes", ErrEvalTooLarge, MaxReviewComment)
	}
	label.Comment = strings.ToValidUTF8(label.Comment, "")
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var tenant, status string
		var resultID, graderID int64
		var cfg []byte
		err := tx.QueryRowContext(ctx, `
			SELECT a.tenant_id, a.status, a.result_id, a.grader_id, g.config
			  FROM eval_review_assignments a JOIN eval_graders g ON g.id = a.grader_id AND g.tenant_id = a.tenant_id
			 WHERE a.id = $1 AND ($2::text = '*' OR a.tenant_id = $2) FOR UPDATE OF a`, id, scope).Scan(&tenant, &status, &resultID, &graderID, &cfg)
		if err != nil {
			return notFound(err)
		}
		if status != ReviewPending {
			return ErrReviewClosed
		}
		hc, err := graders.ParseHuman(cfg)
		if err != nil {
			return fmt.Errorf("reading the human grader's config: %w", err)
		}
		score, ok := hc.ScoreOf(label.Pass, label.Stars)
		if !ok {
			return fmt.Errorf("%w: the label does not fit the %s scale", ErrInvalidEval, hc.Scale)
		}
		pass := score >= hc.PassScore()
		if hc.Scale == graders.ScalePassFail {
			pass = label.Pass != nil && *label.Pass
		}
		res, err := tx.ExecContext(ctx, `UPDATE eval_grades
			SET score = $4, pass = $5, status = 'ok', not_graded = FALSE, rationale = $6, graded_by = $7
			WHERE result_id = $1 AND grader_id = $2 AND tenant_id = $3 AND status = 'pending_review'`,
			resultID, graderID, tenant, score, pass, label.Comment, reviewer)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrReviewClosed
		}
		if _, err := tx.ExecContext(ctx, `UPDATE eval_review_assignments SET status = 'done', reviewer = $2, labelled_at = now() WHERE id = $1`, id, reviewer); err != nil {
			return err
		}
		return recomputeCasePass(ctx, tx, tenant, resultID)
	})
	if err != nil {
		return EvalReviewItem{}, wrapEval("saving the label", err)
	}
	return s.GetEvalReview(ctx, id)
}

// recomputeCasePass works out a result's case verdict again from its grades.
func recomputeCasePass(ctx context.Context, tx dbtx, tenant string, resultID int64) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT e.status, e.score, e.pass, e.not_graded, gr.weight, gr.required
		  FROM eval_grades e JOIN eval_graders gr ON gr.id = e.grader_id AND gr.tenant_id = e.tenant_id
		 WHERE e.result_id = $1 AND e.tenant_id = $2`, resultID, tenant)
	if err != nil {
		return err
	}
	var graded []graders.Graded
	for rows.Next() {
		var status string
		var res graders.Result
		var weight float64
		var required bool
		if err := rows.Scan(&status, &res.Score, &res.Pass, &res.NotGraded, &weight, &required); err != nil {
			_ = rows.Close()
			return err
		}
		graded = append(graded, graders.GradedFor(status, res, weight, required))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_ = rows.Close()
	pass := len(graded) > 0 && graders.CasePass(graded)
	_, err = tx.ExecContext(ctx, `UPDATE eval_results SET case_pass = $3 WHERE id = $1 AND tenant_id = $2`, resultID, tenant, pass)
	return err
}

// ── the judge's bookkeeping ─────────────────────────────────────────────────

// SaveEvalGrades adds grades to a result of an experiment (the pairwise judge
// grades two results together, after both are recorded), adds their judge cost to
// the experiment's spend and works the result's case verdict out again. A grader
// that already has a grade on the result is an ErrInvalidEval.
func (s *Store) SaveEvalGrades(ctx context.Context, experimentID, resultID int64, grades []EvalGrade) error {
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		var tenant string
		var datasetID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT e.tenant_id, e.dataset_id FROM eval_experiments e
			  WHERE e.id = $1 AND ($2::text = '*' OR e.tenant_id = $2) FOR UPDATE`, experimentID, scope).Scan(&tenant, &datasetID); err != nil {
			return notFound(err)
		}
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM eval_results WHERE id = $1 AND experiment_id = $2 AND tenant_id = $3`,
			resultID, experimentID, tenant).Scan(&one); err != nil {
			return notFound(err)
		}
		var spend float64
		for _, g := range grades {
			err := insertGrade(ctx, tx, tenant, datasetID, resultID, &g)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: a grader of the dataset was deleted during the experiment", ErrInvalidEval)
			}
			if err != nil {
				return err
			}
			spend += g.JudgeCostUSD
		}
		if _, err := tx.ExecContext(ctx, `UPDATE eval_experiments SET spent_usd = spent_usd + $2, updated_at = now() WHERE id = $1`, experimentID, spend); err != nil {
			return err
		}
		return recomputeCasePass(ctx, tx, tenant, resultID)
	})
	return wrapEvalErr("saving grades", err)
}

// AddEvalSpend adds the cost of a call that belongs to no grade (the judge's
// canary check) to the experiment's spend. It is also kept as canary_usd, so the
// tenant's daily budget can count it.
func (s *Store) AddEvalSpend(ctx context.Context, experimentID int64, usd float64) error {
	if usd < 0 || usd >= 1e6 {
		return fmt.Errorf("%w: spend must be between 0 and 1000000", ErrInvalidEval)
	}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		res, err := tx.ExecContext(ctx, `UPDATE eval_experiments
			SET spent_usd = spent_usd + $2, canary_usd = canary_usd + $2, updated_at = now()
			WHERE id = $1 AND ($3::text = '*' OR tenant_id = $3)`, experimentID, usd, scope)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrEvalNotFound
		}
		return nil
	})
	return wrapEvalErr("adding spend", err)
}

// EvalJudgeSpendSince sums what judge calls cost since a moment, in the call's
// scope: the judge cost of grades written since then, plus the canary checks of
// experiments created since then. The runner adds it to the tenant's runs when it
// checks the daily budget.
func (s *Store) EvalJudgeSpendSince(ctx context.Context, since time.Time) (float64, error) {
	var total float64
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		return tx.QueryRowContext(ctx, `
			SELECT (SELECT COALESCE(sum(judge_cost_usd), 0)::float8 FROM eval_grades WHERE created_at >= $1 AND ($2::text = '*' OR tenant_id = $2))
			     + (SELECT COALESCE(sum(canary_usd), 0)::float8 FROM eval_experiments WHERE created_at >= $1 AND ($2::text = '*' OR tenant_id = $2))`,
			since, scope).Scan(&total)
	})
	if err != nil {
		return 0, fmt.Errorf("summing judge spend: %w", err)
	}
	return total, nil
}

// ── calibration ─────────────────────────────────────────────────────────────

// EvalCalibrationPair is one result graded by both an llm grader and a human grader.
type EvalCalibrationPair struct {
	ResultID   int64
	CaseID     int64
	LLMScore   float64
	LLMPass    bool
	HumanScore float64
	HumanPass  bool
}

// EvalCalibrationPairs returns the results of a dataset that both graders graded
// (both grades ok), across its experiments or, when experimentID is above 0, in
// that experiment. A grader that is not in the dataset is ErrEvalNotFound.
func (s *Store) EvalCalibrationPairs(ctx context.Context, datasetID int64, llmGrader, humanGrader string, experimentID int64) ([]EvalCalibrationPair, error) {
	out := []EvalCalibrationPair{}
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		if err := visibleDataset(ctx, tx, scope, datasetID); err != nil {
			return err
		}
		var found int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM eval_graders WHERE dataset_id = $1 AND name IN ($2, $3) AND ($4::text = '*' OR tenant_id = $4)`,
			datasetID, llmGrader, humanGrader, scope).Scan(&found); err != nil {
			return err
		}
		if llmGrader == humanGrader || found != 2 {
			return ErrEvalNotFound
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT r.id, COALESCE(r.case_id, 0), gl.score, gl.pass, gh.score, gh.pass
			  FROM eval_results r
			  JOIN eval_graders ll ON ll.dataset_id = r.dataset_id AND ll.tenant_id = r.tenant_id AND ll.name = $2 AND ll.kind = 'llm'
			  JOIN eval_graders hh ON hh.dataset_id = r.dataset_id AND hh.tenant_id = r.tenant_id AND hh.name = $3 AND hh.kind = 'human'
			  JOIN eval_grades gl ON gl.result_id = r.id AND gl.grader_id = ll.id AND gl.tenant_id = r.tenant_id AND gl.status = 'ok' AND NOT gl.not_graded
			  JOIN eval_grades gh ON gh.result_id = r.id AND gh.grader_id = hh.id AND gh.tenant_id = r.tenant_id AND gh.status = 'ok' AND NOT gh.not_graded
			 WHERE r.dataset_id = $1 AND ($4::bigint = 0 OR r.experiment_id = $4) AND ($5::text = '*' OR r.tenant_id = $5)
			 ORDER BY r.id`, datasetID, llmGrader, humanGrader, experimentID, scope)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var p EvalCalibrationPair
			if err := rows.Scan(&p.ResultID, &p.CaseID, &p.LLMScore, &p.LLMPass, &p.HumanScore, &p.HumanPass); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrapEval("reading calibration pairs", err)
	}
	return out, nil
}
