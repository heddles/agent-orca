-- 0022: archived-run retention. The operator can empty the heavy payloads of old
-- archived runs (output, raw output, execution trace) while keeping the summary
-- row, so cost dashboards and the history list keep working
-- (ARCHIVED_RUNS_RETENTION_DAYS, off by default). payload_pruned_at marks a row
-- that was reduced, so the sweep does not pick it up again and the run page can
-- say why the output and trace are gone. Re-archiving a run writes its payload
-- again and clears the mark.
ALTER TABLE archived_runs ADD COLUMN IF NOT EXISTS payload_pruned_at TIMESTAMPTZ;

-- The sweep looks for unreduced rows that have not been written for a while.
CREATE INDEX IF NOT EXISTS idx_archived_runs_unpruned ON archived_runs (updated_at) WHERE payload_pruned_at IS NULL;
