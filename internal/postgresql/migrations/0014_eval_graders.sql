-- 0014: graders of an evaluation dataset, and graded results (docs/evaluations.md).
--
-- A grader belongs to a dataset: a name, a kind and a validated JSON config
-- (the kinds are in internal/evals/graders). eval_results holds one graded
-- output of an agent (a result supplied to the grade endpoint now; the
-- experiment runner writes the same rows later, which is why experiment_id has
-- no foreign key yet). eval_grades holds one grader's verdict on one result.
--
-- Every table carries tenant_id with the row-level security of 0010 and 0013,
-- and every child references its parent with a composite key that includes
-- tenant_id, so the database refuses a row under another tenant than its
-- parent's whatever the application does.

-- A case is referenced by results together with its tenant.
CREATE UNIQUE INDEX IF NOT EXISTS idx_eval_cases_id_tenant ON eval_cases (id, tenant_id);

CREATE TABLE IF NOT EXISTS eval_graders (
    id         BIGSERIAL PRIMARY KEY,
    tenant_id  TEXT NOT NULL,
    dataset_id BIGINT NOT NULL,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL,
    config     JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (octet_length(config::text) <= 131072),
    weight     REAL NOT NULL DEFAULT 1 CHECK (weight > 0),
    required   BOOLEAN NOT NULL DEFAULT FALSE,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, dataset_id, name),
    UNIQUE (id, tenant_id),
    FOREIGN KEY (dataset_id, tenant_id) REFERENCES eval_datasets (id, tenant_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS eval_results (
    id            BIGSERIAL PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    dataset_id    BIGINT NOT NULL,
    case_id       BIGINT,
    experiment_id BIGINT,
    arm           TEXT NOT NULL DEFAULT '',
    output        TEXT NOT NULL CHECK (octet_length(output) <= 262144),
    tool_calls    JSONB NOT NULL DEFAULT '[]'::jsonb,
    cost_usd      NUMERIC(14,8) NOT NULL DEFAULT 0,
    latency_ms    BIGINT NOT NULL DEFAULT 0,
    case_pass     BOOLEAN NOT NULL DEFAULT FALSE,
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_id),
    FOREIGN KEY (dataset_id, tenant_id) REFERENCES eval_datasets (id, tenant_id) ON DELETE CASCADE,
    FOREIGN KEY (case_id, tenant_id) REFERENCES eval_cases (id, tenant_id));

CREATE TABLE IF NOT EXISTS eval_grades (
    id         BIGSERIAL PRIMARY KEY,
    tenant_id  TEXT NOT NULL,
    result_id  BIGINT NOT NULL,
    grader_id  BIGINT NOT NULL,
    score      REAL NOT NULL CHECK (score BETWEEN 0 AND 1),
    pass       BOOLEAN NOT NULL,
    not_graded BOOLEAN NOT NULL DEFAULT FALSE,
    rationale  TEXT NOT NULL DEFAULT '' CHECK (octet_length(rationale) <= 4096),
    UNIQUE (result_id, grader_id),
    FOREIGN KEY (result_id, tenant_id) REFERENCES eval_results (id, tenant_id) ON DELETE CASCADE,
    FOREIGN KEY (grader_id, tenant_id) REFERENCES eval_graders (id, tenant_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_eval_graders_dataset ON eval_graders (tenant_id, dataset_id, name);
CREATE INDEX IF NOT EXISTS idx_eval_results_dataset ON eval_results (tenant_id, dataset_id, id);
CREATE INDEX IF NOT EXISTS idx_eval_grades_result   ON eval_grades (tenant_id, result_id);

ALTER TABLE eval_graders ENABLE ROW LEVEL SECURITY;
ALTER TABLE eval_graders FORCE ROW LEVEL SECURITY;
ALTER TABLE eval_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE eval_results FORCE ROW LEVEL SECURITY;
ALTER TABLE eval_grades  ENABLE ROW LEVEL SECURITY;
ALTER TABLE eval_grades  FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON eval_graders USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
CREATE POLICY tenant_isolation ON eval_results USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
CREATE POLICY tenant_isolation ON eval_grades  USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
