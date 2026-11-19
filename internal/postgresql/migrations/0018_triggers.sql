-- 0018: webhook triggers (docs/integrations.md).
--
-- A trigger lets a system outside agent-orca start a run of one agent by
-- POSTing to /hooks/{id}. The row carries tenant_id with the row-level
-- security of 0010. The id is the public, unguessable part of the URL; the
-- secret itself is shown once at creation and never stored. secret_hash holds
-- hex(sha256(secret)), which is the key the sender signs with (docs/integrations.md),
-- so a read of this table cannot recover the secret, but it can verify a signature.
CREATE TABLE IF NOT EXISTS triggers (
    id                  TEXT PRIMARY KEY,
    tenant_id           TEXT NOT NULL,
    namespace           TEXT NOT NULL,
    agent               TEXT NOT NULL,
    name                TEXT NOT NULL,
    secret_hash         TEXT NOT NULL,
    input_path          TEXT NOT NULL DEFAULT '',
    rate_limit_per_min  INTEGER NOT NULL DEFAULT 30 CHECK (rate_limit_per_min BETWEEN 1 AND 600),
    enabled             BOOLEAN NOT NULL DEFAULT TRUE,
    created_by          TEXT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_fired_at       TIMESTAMPTZ,
    fire_count          BIGINT NOT NULL DEFAULT 0,
    UNIQUE (id, tenant_id),
    UNIQUE (tenant_id, namespace, agent, name)
);

CREATE INDEX IF NOT EXISTS idx_triggers_agent ON triggers (tenant_id, namespace, agent);

ALTER TABLE triggers ENABLE ROW LEVEL SECURITY;
ALTER TABLE triggers FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON triggers USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
