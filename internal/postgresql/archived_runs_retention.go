package postgresql

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// ArchivedRunsRetentionInterval is how often the archived-run sweep runs.
const ArchivedRunsRetentionInterval = 6 * time.Hour

// archivedRunsSweepBatch bounds the rows one UPDATE reduces, so a first sweep
// over a large archive does not hold one huge transaction.
const archivedRunsSweepBatch = 500

var archivedRunsSwept = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "agentorca_archived_runs_swept_total",
	Help: "Archived runs whose output and execution trace were emptied by ARCHIVED_RUNS_RETENTION_DAYS. The summary row stays.",
})

func init() {
	ctrlmetrics.Registry.MustRegister(archivedRunsSwept)
}

// ReduceArchivedRunsBefore applies archived-run retention (docs/observability.md):
// for every archived run last written before cutoff and finished (or started)
// before it, it empties output, raw_output and trace_events and stamps
// payload_pruned_at. The row itself stays with its status, spend, cost capture,
// token counts, timestamps, routing decisions (the cost dashboard reads the
// model from them), input and labels, so cost totals, history lists and the
// fleet view are unchanged.
//
// Evaluation dataset cases are not a reason to skip a run: a case copies the
// run's input (and reference) when it is created and keeps only a pointer to
// the run for provenance (see handleRunToDataset), so a case never reads the
// archive again.
//
// It runs in the platform scope only (the operator's sweep). Re-running it
// finds nothing new: a reduced row has payload_pruned_at set, and a row with no
// payload is not touched.
func (s *Store) ReduceArchivedRunsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	var total int64
	err := s.scoped(ctx, func(tx dbtx, scope string) error {
		if scope != PlatformScope {
			return fmt.Errorf("%w: archived-run retention needs the platform scope", ErrNoTenantScope)
		}
		for {
			res, err := tx.ExecContext(ctx, `
				UPDATE archived_runs SET output = '', raw_output = '', trace_events = NULL, payload_pruned_at = now()
				WHERE id IN (
					SELECT id FROM archived_runs
					WHERE payload_pruned_at IS NULL
					  AND updated_at < $1
					  AND COALESCE(completion_time, start_time, created_at) < $1
					  AND (COALESCE(output, '') <> '' OR COALESCE(raw_output, '') <> '' OR trace_events IS NOT NULL)
					ORDER BY updated_at
					LIMIT $2)`, cutoff, archivedRunsSweepBatch)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			total += n
			if n < archivedRunsSweepBatch {
				return nil
			}
		}
	})
	if err != nil {
		return 0, fmt.Errorf("applying archived-run retention: %w", err)
	}
	return total, nil
}

// RunArchivedRunsRetention reduces archived runs older than retention, once now
// and then every interval, until ctx ends. A retention of 0 or less keeps
// everything and returns immediately.
func (s *Store) RunArchivedRunsRetention(ctx context.Context, retention, interval time.Duration) error {
	return runArchivedRunsRetention(ctx, s.ReduceArchivedRunsBefore, time.Now, retention, interval)
}

// runArchivedRunsRetention is RunArchivedRunsRetention with its sweep function
// and clock injected. A failed sweep is logged and retried on the next tick.
func runArchivedRunsRetention(ctx context.Context, reduce func(context.Context, time.Time) (int64, error), now func() time.Time, retention, interval time.Duration) error {
	if retention <= 0 {
		return nil
	}
	run := func() {
		n, err := reduce(ctx, now().Add(-retention))
		switch {
		case err != nil:
			if ctx.Err() == nil {
				slog.Error("applying archived-run retention", "err", err)
			}
		case n > 0:
			archivedRunsSwept.Add(float64(n))
			slog.Info("emptied the output and trace of old archived runs", "runs", n, "retention", retention)
		}
	}
	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			run()
		}
	}
}
