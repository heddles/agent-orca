-- 0012: revision history of the objects the UI API writes. One row per saved
-- state of an object, numbered per (tenant, kind, namespace, name). The object
-- is stored as JSONB, as it was after the write, so a revision can be shown,
-- compared and restored even after the object is deleted. Row-level security is
-- the same as on the other tenant tables (see 0010): a row is visible and
-- writable only inside its tenant's scope or the platform scope. A
-- platform-scoped write with no tenant (authentication disabled) is stored
-- unattributed (tenant_id NULL), visible only to the platform.
CREATE TABLE IF NOT EXISTS revisions (
    id               BIGSERIAL PRIMARY KEY,
    tenant_id        TEXT,
    kind             TEXT NOT NULL,
    namespace        TEXT NOT NULL,
    name             TEXT NOT NULL,
    revision         INTEGER NOT NULL,
    action           TEXT NOT NULL,
    user_id          TEXT NOT NULL DEFAULT '',
    summary          TEXT NOT NULL DEFAULT '',
    resource_version TEXT NOT NULL DEFAULT '',
    restored_from    INTEGER,
    object           JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Also the guard against two writers taking the same number: the store holds an
-- advisory lock while it numbers, and this index refuses a duplicate if it ever
-- did not.
CREATE UNIQUE INDEX IF NOT EXISTS idx_revisions_object ON revisions(tenant_id, kind, namespace, name, revision DESC);

ALTER TABLE revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE revisions FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON revisions USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
