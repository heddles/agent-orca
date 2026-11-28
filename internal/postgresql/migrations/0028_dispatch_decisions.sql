-- 0028: agent dispatch decisions. One row for each time an agent was picked (or
-- could not be) for a request that named none: what was chosen, how, what else
-- was considered, and what a model call cost.
--
-- The request itself is never stored. candidates holds agent names and the
-- decider's probabilities. cost_usd is spend on no AgentRun, so the tenant's
-- daily budget check for further decision calls reads it from here.
CREATE TABLE IF NOT EXISTS dispatch_decisions (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    source          TEXT NOT NULL,
    method          TEXT NOT NULL CHECK (method IN ('only', 'decided', 'fallback', 'refused')),
    reason          TEXT NOT NULL DEFAULT '',
    agent_namespace TEXT NOT NULL DEFAULT '',
    agent           TEXT NOT NULL DEFAULT '',
    confidence      DOUBLE PRECISION NOT NULL DEFAULT 0,
    candidate_count INTEGER NOT NULL DEFAULT 0,
    candidates      JSONB NOT NULL DEFAULT '[]'::jsonb,
    provider        TEXT NOT NULL DEFAULT '',
    model           TEXT NOT NULL DEFAULT '',
    escalated       BOOLEAN NOT NULL DEFAULT FALSE,
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    cost_usd        DOUBLE PRECISION NOT NULL DEFAULT 0,
    latency_ms      INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_dispatch_decisions_tenant_time ON dispatch_decisions (tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_dispatch_decisions_time ON dispatch_decisions (created_at);

ALTER TABLE dispatch_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE dispatch_decisions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON dispatch_decisions USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
