-- 0002: approvals. A request for a human (or an auto-approve rule) to allow
-- something an agent asked for, e.g. egress to a host its sandbox policy does
-- not list. Rows are the audit record: who decided, when and why.
CREATE TABLE approvals (
    id              TEXT PRIMARY KEY,
    tenant          TEXT,
    namespace       TEXT NOT NULL,
    run_name        TEXT NOT NULL,
    -- kind is the source: egress (OpenShell policy draft), later budget, merge, tool.
    kind            TEXT NOT NULL,
    -- source_key identifies the request at its source, e.g. <sandbox>/<chunk id>;
    -- a repeated request updates its row instead of adding one.
    source_key      TEXT NOT NULL,
    summary         TEXT NOT NULL,
    details         JSONB NOT NULL DEFAULT '{}',
    -- approver_groups may decide; empty means any user of the namespace.
    approver_groups JSONB NOT NULL DEFAULT '[]',
    -- pending, approved, rejected, expired (timed out, treated as rejected) or
    -- withdrawn (the run ended first).
    status          TEXT NOT NULL DEFAULT 'pending',
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL,
    decided_by      TEXT,
    decided_at      TIMESTAMPTZ,
    reason          TEXT,
    -- applied_at is when the decision reached the source.
    applied_at      TIMESTAMPTZ,
    UNIQUE (kind, source_key)
);

CREATE INDEX idx_approvals_pending ON approvals(namespace, status, requested_at DESC);
CREATE INDEX idx_approvals_run     ON approvals(namespace, run_name);
