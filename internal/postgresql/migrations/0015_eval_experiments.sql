-- 0015: evaluation experiments (docs/evaluations.md, docs/proposals/evaluations.md
-- section 3). An experiment runs 2 to 4 arms (agent revisions, or one revision on
-- different models) over a dataset at a frozen version. The operator's experiment
-- runner owns the row while it runs: it claims it with a lease (FOR UPDATE SKIP
-- LOCKED), so a second operator cannot run the same experiment and a restart
-- resumes it.
--
-- eval_results (0014) gains the columns an experiment run needs: which repeat, the
-- AgentRun it came from, its phase and whether its cost was fully captured. The
-- table now also holds the free-standing results the grade endpoint writes; those
-- have no experiment.
--
-- Every table carries tenant_id with the row-level security of 0010 and 0013, and
-- a child references its parent with a composite key that includes tenant_id, so
-- the database refuses a row under another tenant than its parent's.

CREATE TABLE IF NOT EXISTS eval_experiments (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    namespace       TEXT NOT NULL,
    dataset_id      BIGINT NOT NULL,
    dataset_version INTEGER NOT NULL,
    name            TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued','running','grading','awaiting_review','complete','cancelled','failed')),
    arms            JSONB NOT NULL CHECK (jsonb_typeof(arms) = 'array'),
    repeats         SMALLINT NOT NULL DEFAULT 1 CHECK (repeats BETWEEN 1 AND 5),
    budget_usd      NUMERIC(14,8) NOT NULL CHECK (budget_usd > 0),
    spent_usd       NUMERIC(14,8) NOT NULL DEFAULT 0,
    run_budget_usd  NUMERIC(14,8) NOT NULL CHECK (run_budget_usd > 0),
    timeout_seconds INTEGER NOT NULL DEFAULT 600 CHECK (timeout_seconds BETWEEN 10 AND 86400),
    concurrency     SMALLINT NOT NULL DEFAULT 4 CHECK (concurrency BETWEEN 1 AND 16),
    purpose         TEXT NOT NULL DEFAULT 'compare' CHECK (purpose IN ('compare','gate')),
    cancel_requested BOOLEAN NOT NULL DEFAULT FALSE,
    lease_owner     TEXT NOT NULL DEFAULT '',
    lease_until     TIMESTAMPTZ,
    summary         JSONB,
    error           TEXT NOT NULL DEFAULT '' CHECK (octet_length(error) <= 4096),
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,
    UNIQUE (id, tenant_id),
    FOREIGN KEY (dataset_id, tenant_id) REFERENCES eval_datasets (id, tenant_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_eval_experiments_tenant ON eval_experiments (tenant_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_eval_experiments_active ON eval_experiments (status) WHERE status IN ('queued','running');

ALTER TABLE eval_results ADD COLUMN IF NOT EXISTS repeat        SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE eval_results ADD COLUMN IF NOT EXISTS run_namespace TEXT NOT NULL DEFAULT '';
ALTER TABLE eval_results ADD COLUMN IF NOT EXISTS run_name      TEXT NOT NULL DEFAULT '';
ALTER TABLE eval_results ADD COLUMN IF NOT EXISTS phase         TEXT NOT NULL DEFAULT '';
ALTER TABLE eval_results ADD COLUMN IF NOT EXISTS cost_complete BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE eval_results ADD COLUMN IF NOT EXISTS error         TEXT NOT NULL DEFAULT '' CHECK (octet_length(error) <= 4096);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'eval_results_experiment_fk') THEN
        ALTER TABLE eval_results ADD CONSTRAINT eval_results_experiment_fk
            FOREIGN KEY (experiment_id, tenant_id) REFERENCES eval_experiments (id, tenant_id) ON DELETE CASCADE;
    END IF;
END $$;

-- One result per (case, arm, repeat) of an experiment: recording a finished run twice is a no-op.
CREATE UNIQUE INDEX IF NOT EXISTS idx_eval_results_slot ON eval_results (experiment_id, case_id, arm, repeat)
    WHERE experiment_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_eval_results_experiment ON eval_results (tenant_id, experiment_id, id)
    WHERE experiment_id IS NOT NULL;

ALTER TABLE eval_experiments ENABLE ROW LEVEL SECURITY;
ALTER TABLE eval_experiments FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON eval_experiments USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
