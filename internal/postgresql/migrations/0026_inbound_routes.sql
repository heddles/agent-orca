-- 0026: one chat app shared by several agents (docs/inbound-channels.md, "Shared
-- chat apps"). A shared connection has an empty namespace and agent. Routes say
-- which agents answer in it, for whom, and where.
CREATE TABLE IF NOT EXISTS inbound_routes (
    tenant_id       TEXT NOT NULL,
    connection_id   TEXT NOT NULL,
    namespace       TEXT NOT NULL,
    agent           TEXT NOT NULL,
    -- Exact sender ids who may ask this agent, and those of them who may approve
    -- its tool calls from the chat.
    allow_senders   JSONB NOT NULL DEFAULT '[]'::jsonb,
    approvers       JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, connection_id, namespace, agent)
);

-- The places an agent answers in. place is the platform's channel id, or '' for
-- direct messages. Several agents may share a place; one of them is its default,
-- answering messages that name no agent.
CREATE TABLE IF NOT EXISTS inbound_places (
    tenant_id       TEXT NOT NULL,
    connection_id   TEXT NOT NULL,
    place           TEXT NOT NULL,
    place_name      TEXT NOT NULL DEFAULT '',
    namespace       TEXT NOT NULL,
    agent           TEXT NOT NULL,
    is_default      BOOLEAN NOT NULL DEFAULT FALSE,
    added_at        TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id, connection_id, place, namespace, agent)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_inbound_places_default ON inbound_places (tenant_id, connection_id, place) WHERE is_default;

-- A pairing made from an agent's page adds the sender to that agent's route.
ALTER TABLE inbound_pairings ADD COLUMN IF NOT EXISTS namespace TEXT NOT NULL DEFAULT '';
ALTER TABLE inbound_pairings ADD COLUMN IF NOT EXISTS agent TEXT NOT NULL DEFAULT '';

ALTER TABLE inbound_routes ENABLE ROW LEVEL SECURITY;
ALTER TABLE inbound_routes FORCE ROW LEVEL SECURITY;
ALTER TABLE inbound_places ENABLE ROW LEVEL SECURITY;
ALTER TABLE inbound_places FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON inbound_routes USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
CREATE POLICY tenant_isolation ON inbound_places USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
