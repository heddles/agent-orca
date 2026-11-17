-- 0011: an audit trail of changes made through the UI API and of approval
-- decisions. One row per change, attributed to the tenant's stable ID and the
-- acting user. Row-level security is the same as on the other tenant tables
-- (see 0010): a row is visible and writable only inside its tenant's scope or
-- the platform scope. A platform-scoped write with no tenant (authentication
-- disabled) is stored unattributed (tenant_id NULL), visible only to the
-- platform.
CREATE TABLE IF NOT EXISTS audit_events (
    id               BIGSERIAL PRIMARY KEY,
    tenant_id        TEXT,
    user_id          TEXT NOT NULL DEFAULT '',
    action           TEXT NOT NULL,
    kind             TEXT NOT NULL,
    namespace        TEXT NOT NULL DEFAULT '',
    name             TEXT NOT NULL DEFAULT '',
    resource_version TEXT NOT NULL DEFAULT '',
    summary          TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_id ON audit_events(tenant_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_object    ON audit_events(tenant_id, kind, namespace, name, id DESC);

ALTER TABLE audit_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON audit_events USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
