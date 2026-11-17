-- 0013: evaluation datasets and their cases (docs/evaluations.md).
--
-- A dataset is a named, versioned set of cases for one workspace. Cases hold
-- user data copied from runs or typed in, so both tables carry tenant_id with
-- the same row-level security as the other tenant tables (see 0010).
--
-- eval_datasets.version starts at 1 and is raised by one on every change to the
-- dataset's cases (add, edit, remove; a bulk add counts once). The publish gate
-- compares it. eval_cases.added_in is the version a case first appeared in,
-- removed_in the version that removed it (a soft delete, so an older version can
-- be read back).
--
-- A case's tenant_id must equal its dataset's. The composite foreign key makes
-- the database refuse a case written under another tenant than its dataset's,
-- whatever the application does.
CREATE TABLE IF NOT EXISTS eval_datasets (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    namespace   TEXT NOT NULL,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    agent       TEXT NOT NULL DEFAULT '',
    version     INTEGER NOT NULL DEFAULT 1,
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, namespace, name),
    UNIQUE (id, tenant_id)
);

CREATE TABLE IF NOT EXISTS eval_cases (
    id             BIGSERIAL PRIMARY KEY,
    tenant_id      TEXT NOT NULL,
    dataset_id     BIGINT NOT NULL,
    added_in       INTEGER NOT NULL,
    removed_in     INTEGER,
    input          TEXT NOT NULL CHECK (octet_length(input) <= 65536),
    reference      TEXT CHECK (octet_length(reference) <= 65536),
    reference_kind TEXT NOT NULL DEFAULT 'none'
                   CHECK (reference_kind IN ('none', 'exact', 'example', 'rubric')),
    tags           TEXT[] NOT NULL DEFAULT '{}',
    weight         REAL NOT NULL DEFAULT 1 CHECK (weight > 0),
    source_run_ns  TEXT,
    source_run     TEXT,
    source_user    TEXT NOT NULL DEFAULT '',
    redacted       BOOLEAN NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (dataset_id, tenant_id) REFERENCES eval_datasets (id, tenant_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_eval_datasets_tenant ON eval_datasets (tenant_id, namespace, name);
CREATE INDEX IF NOT EXISTS idx_eval_cases_dataset   ON eval_cases (tenant_id, dataset_id, id);

ALTER TABLE eval_datasets ENABLE ROW LEVEL SECURITY;
ALTER TABLE eval_datasets FORCE ROW LEVEL SECURITY;
ALTER TABLE eval_cases    ENABLE ROW LEVEL SECURITY;
ALTER TABLE eval_cases    FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON eval_datasets USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
CREATE POLICY tenant_isolation ON eval_cases    USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
