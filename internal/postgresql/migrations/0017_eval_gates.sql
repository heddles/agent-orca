-- 0017: the publish gate (docs/evaluations.md, docs/proposals/evaluations.md sections 5 and 7).
--
-- eval_gates holds one row per agent that has a gate: which dataset to
-- evaluate on, which saved revision is the baseline, how much worse than the
-- baseline the candidate may be (the non-inferiority margin), how many cases
-- an evaluation needs, and whether the gate is on. The gate is not on the Agent
-- object because anyone who can edit the Agent could then lower it; changing it
-- is an admin action and is audited.
--
-- The row carries tenant_id with the row-level security of 0010, and the
-- composite foreign key makes the database refuse a gate under another tenant
-- than its dataset's. The dataset is protected by RESTRICT: deleting the
-- dataset a gate rests on must not silently remove the gate, so the dataset
-- is refused until the gate is changed or deleted.
CREATE TABLE IF NOT EXISTS eval_gates (
    id                BIGSERIAL PRIMARY KEY,
    tenant_id         TEXT NOT NULL,
    namespace         TEXT NOT NULL,
    agent             TEXT NOT NULL,
    dataset_id        BIGINT NOT NULL,
    baseline_revision INTEGER NOT NULL CHECK (baseline_revision > 0),
    margin            NUMERIC(6,5) NOT NULL DEFAULT 0.05 CHECK (margin > 0 AND margin < 1),
    min_cases         INTEGER NOT NULL DEFAULT 30 CHECK (min_cases BETWEEN 1 AND 2000),
    min_pass_rate     NUMERIC(6,5) CHECK (min_pass_rate IS NULL OR (min_pass_rate >= 0 AND min_pass_rate <= 1)),
    max_regressions   INTEGER CHECK (max_regressions IS NULL OR max_regressions >= 0),
    budget_usd        NUMERIC(14,8) NOT NULL DEFAULT 5 CHECK (budget_usd > 0),
    enabled           BOOLEAN NOT NULL DEFAULT TRUE,
    updated_by        TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, namespace, agent),
    UNIQUE (id, tenant_id),
    FOREIGN KEY (dataset_id, tenant_id) REFERENCES eval_datasets (id, tenant_id) ON DELETE RESTRICT
);

-- The latest finished gate experiment of an agent is looked up on every publish.
CREATE INDEX IF NOT EXISTS idx_eval_experiments_gate ON eval_experiments (tenant_id, namespace, id DESC)
    WHERE purpose = 'gate' AND status = 'complete';

ALTER TABLE eval_gates ENABLE ROW LEVEL SECURITY;
ALTER TABLE eval_gates FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON eval_gates USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
