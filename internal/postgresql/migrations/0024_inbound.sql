-- 0024: inbound conversation channels (docs/inbound-channels.md).
--
-- An inbound connection lets people (or systems) start a run of one published
-- agent from a channel outside agent-orca, for example a mailbox, and gets the
-- result back in the same conversation. The core is vendor neutral: kind names
-- the adapter that speaks the channel's protocol, config holds that adapter's
-- NON-secret settings (folders, allow-list, limits).
--
-- Secrets (mail passwords, tokens, webhook signing secrets) are never stored
-- here. secret_ref names a Kubernetes Secret, labelled by an administrator, in
-- the connection's namespace, exactly as notification channels do
-- (TenantConfig spec.notifications): a builder who creates a connection cannot
-- read or write Secrets and so cannot point a connection at someone else's.
--
-- inbound_messages is the idempotency and status ledger. It never holds a
-- message body, subject or sender address: message_key, thread_key and
-- sender_hash are SHA-256 digests, so a read of the table cannot recover who
-- wrote what. reply_target is the one exception: while a reply is pending it
-- holds what the adapter needs to answer (an address, a message id), and it is
-- set to NULL the moment the reply is sent or given up.
--
-- Both tables carry tenant_id with the row-level security of 0010.
CREATE TABLE IF NOT EXISTS inbound_connections (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    kind            TEXT NOT NULL,
    name            TEXT NOT NULL,
    namespace       TEXT NOT NULL,
    agent           TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    config          JSONB NOT NULL DEFAULT '{}'::jsonb,
    secret_ref      TEXT NOT NULL DEFAULT '',
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_poll_at    TIMESTAMPTZ,
    last_error      TEXT NOT NULL DEFAULT '',
    received_count  BIGINT NOT NULL DEFAULT 0,
    replied_count   BIGINT NOT NULL DEFAULT 0,
    rejected_count  BIGINT NOT NULL DEFAULT 0,
    UNIQUE (id, tenant_id),
    UNIQUE (tenant_id, namespace, agent, name)
);

CREATE INDEX IF NOT EXISTS idx_inbound_connections_agent ON inbound_connections (tenant_id, namespace, agent);

CREATE TABLE IF NOT EXISTS inbound_messages (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    connection_id   TEXT NOT NULL,
    message_key     TEXT NOT NULL,
    thread_key      TEXT NOT NULL DEFAULT '',
    sender_hash     TEXT NOT NULL DEFAULT '',
    size_bytes      INTEGER NOT NULL DEFAULT 0,
    status          TEXT NOT NULL CHECK (status IN ('received', 'running', 'replied', 'rejected', 'ignored', 'failed')),
    reason          TEXT NOT NULL DEFAULT '',
    run_namespace   TEXT NOT NULL DEFAULT '',
    run_name        TEXT NOT NULL DEFAULT '',
    reply_target    JSONB,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, connection_id, message_key)
);

CREATE INDEX IF NOT EXISTS idx_inbound_messages_pending ON inbound_messages (status) WHERE status IN ('received', 'running');
CREATE INDEX IF NOT EXISTS idx_inbound_messages_thread ON inbound_messages (tenant_id, connection_id, thread_key) WHERE thread_key <> '';
CREATE INDEX IF NOT EXISTS idx_inbound_messages_sender ON inbound_messages (tenant_id, connection_id, sender_hash, received_at);
CREATE INDEX IF NOT EXISTS idx_inbound_messages_age ON inbound_messages (received_at);

ALTER TABLE inbound_connections ENABLE ROW LEVEL SECURITY;
ALTER TABLE inbound_connections FORCE ROW LEVEL SECURITY;
ALTER TABLE inbound_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE inbound_messages FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON inbound_connections USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
CREATE POLICY tenant_isolation ON inbound_messages USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
