package postgresql

import (
	"context"
	"fmt"
	"time"
)

// PurgeEvalResultsBefore applies result retention (docs/evaluations.md): for
// every result created before cutoff whose experiment is over (or that belongs
// to no experiment), it deletes the result's grades and empties its output and
// tool calls. The row itself stays, with its cost, latency and pass flag, so
// totals and trend lines survive. Results of an experiment still queued,
// running, grading or awaiting review are never touched.
//
// It runs in the platform scope only (the operator's sweep). Re-running it
// finds nothing new: a result with an empty output is skipped.
func (s *Store) PurgeEvalResultsBefore(ctx context.Context, cutoff time.Time) (results, grades int64, err error) {
	err = s.scoped(ctx, func(tx dbtx, scope string) error {
		if scope != PlatformScope {
			return fmt.Errorf("%w: result retention needs the platform scope", ErrNoTenantScope)
		}
		const old = `
			r.created_at < $1
			AND (r.experiment_id IS NULL OR NOT EXISTS (
				SELECT 1 FROM eval_experiments e
				WHERE e.id = r.experiment_id AND e.tenant_id = r.tenant_id
				  AND e.status IN ('queued','running','grading','awaiting_review')))`
		res, err := tx.ExecContext(ctx, `DELETE FROM eval_grades g USING eval_results r
			WHERE g.result_id = r.id AND g.tenant_id = r.tenant_id AND`+old, cutoff)
		if err != nil {
			return err
		}
		grades, _ = res.RowsAffected()
		res, err = tx.ExecContext(ctx, `UPDATE eval_results r SET output = '', tool_calls = '[]'::jsonb
			WHERE (r.output <> '' OR r.tool_calls <> '[]'::jsonb) AND`+old, cutoff)
		if err != nil {
			return err
		}
		results, _ = res.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("applying result retention: %w", err)
	}
	return results, grades, nil
}
