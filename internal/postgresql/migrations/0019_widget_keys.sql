-- 0019: chat widget keys (docs/integrations.md).
--
-- A widget key lets a web page embed the chat of one published agent. The id is
-- public (it is in the page's HTML), so it is not a credential: what protects
-- the agent is the list of origins the key answers to, the per-key request rate
-- and the per-key daily budget. The row carries tenant_id with the row-level
-- security of 0010.
CREATE TABLE IF NOT EXISTS widget_keys (
    id                  TEXT PRIMARY KEY,
    tenant_id           TEXT NOT NULL,
    namespace           TEXT NOT NULL,
    agent               TEXT NOT NULL,
    name                TEXT NOT NULL,
    allowed_origins     TEXT[] NOT NULL,
    rate_limit_per_min  INTEGER NOT NULL DEFAULT 30 CHECK (rate_limit_per_min BETWEEN 1 AND 600),
    budget_usd_per_day  NUMERIC(14,8) NOT NULL DEFAULT 5 CHECK (budget_usd_per_day > 0 AND budget_usd_per_day <= 10000),
    enabled             BOOLEAN NOT NULL DEFAULT TRUE,
    created_by          TEXT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, tenant_id),
    UNIQUE (tenant_id, namespace, agent, name)
);

CREATE INDEX IF NOT EXISTS idx_widget_keys_agent ON widget_keys (tenant_id, namespace, agent);

ALTER TABLE widget_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE widget_keys FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON widget_keys USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
