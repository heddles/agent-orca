-- 0001: the run archive. Written to be idempotent (IF NOT EXISTS) because it
-- adopts databases created before versioned migrations, which already have it.
CREATE TABLE IF NOT EXISTS archived_runs (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    namespace       TEXT NOT NULL,
    labels          JSONB DEFAULT '{}',
    agent_ref       TEXT NOT NULL,
    input           TEXT,
    parent_run_ref  TEXT,
    prior_run_ref   TEXT,
    timeout_sec     BIGINT,
    phase           TEXT NOT NULL,
    pod_name        TEXT,
    spend_usd       TEXT,
    output          TEXT,
    raw_output      TEXT,
    restart_count   INTEGER DEFAULT 0,
    start_time      TIMESTAMPTZ,
    completion_time TIMESTAMPTZ,
    context_used_tokens  INTEGER DEFAULT 0,
    max_context_tokens   INTEGER DEFAULT 0,
    tenant          TEXT,
    routing_decisions JSONB,
    child_run_refs  JSONB,
    trace_events     JSONB,
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    updated_at      TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_archived_runs_tenant    ON archived_runs(tenant);
CREATE INDEX IF NOT EXISTS idx_archived_runs_namespace ON archived_runs(namespace);
CREATE INDEX IF NOT EXISTS idx_archived_runs_phase     ON archived_runs(phase);
CREATE INDEX IF NOT EXISTS idx_archived_runs_created   ON archived_runs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_archived_runs_name      ON archived_runs(name);

-- Back-fill: add the trace_events column to tables created before the full
-- execution-trace archival feature shipped. ADD COLUMN IF NOT EXISTS is a no-op
-- for fresh tables (which already declare the column above).
ALTER TABLE archived_runs ADD COLUMN IF NOT EXISTS trace_events JSONB;

-- Mirrors the AgentRun CostCapture condition so archived runs whose spend could
-- not be read stay distinguishable from runs that cost nothing: 'complete',
-- 'incomplete', or NULL for rows archived before the column existed.
ALTER TABLE archived_runs ADD COLUMN IF NOT EXISTS cost_capture TEXT;

-- Enable lz4 TOAST compression on columns that can hold large values
-- (trace events, routing decisions, output). PostgreSQL automatically TOASTs
-- values > 2KB; SET COMPRESSION controls the algorithm. lz4 is fast and gives
-- a good ratio. A VACUUM (FULL if possible) is needed in production to
-- retroactively compress existing rows; new inserts use lz4 automatically.
ALTER TABLE archived_runs ALTER COLUMN trace_events SET COMPRESSION lz4;
ALTER TABLE archived_runs ALTER COLUMN routing_decisions SET COMPRESSION lz4;
ALTER TABLE archived_runs ALTER COLUMN output SET COMPRESSION lz4;
ALTER TABLE archived_runs ALTER COLUMN raw_output SET COMPRESSION lz4;
ALTER TABLE archived_runs ALTER COLUMN input SET COMPRESSION lz4;
