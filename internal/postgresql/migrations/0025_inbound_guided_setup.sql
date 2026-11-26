-- 0025: guided channel setup (docs/inbound-channels.md).
--
-- credentials_enc holds channel credentials an administrator entered in the UI,
-- envelope encrypted under the connection keyring (internal/connections), bound
-- to the tenant and the connection. It is never returned by the API. A
-- connection uses either these or the Secret named by secret_ref.
--
-- last_request_at and last_auth_error say whether the channel is reaching us:
-- the time of the last request that passed the adapter's check, and why the last
-- one that did not was refused.
ALTER TABLE inbound_connections ADD COLUMN IF NOT EXISTS credentials_enc BYTEA;
ALTER TABLE inbound_connections ADD COLUMN IF NOT EXISTS credentials_key_id TEXT NOT NULL DEFAULT '';
ALTER TABLE inbound_connections ADD COLUMN IF NOT EXISTS last_request_at TIMESTAMPTZ;
ALTER TABLE inbound_connections ADD COLUMN IF NOT EXISTS last_auth_error TEXT NOT NULL DEFAULT '';
ALTER TABLE inbound_connections ADD COLUMN IF NOT EXISTS last_auth_error_at TIMESTAMPTZ;

-- A pairing lets a person add themselves to a channel by sending a one-time code
-- from the chat app. Only the code's digest is kept. The sender's id and the
-- reply target are filled when a message with the code arrives, and the row is
-- deleted when someone approves or discards it, or after it expires.
CREATE TABLE IF NOT EXISTS inbound_pairings (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    connection_id   TEXT NOT NULL,
    code_hash       TEXT NOT NULL,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    received_at     TIMESTAMPTZ,
    sender_id       TEXT NOT NULL DEFAULT '',
    sender_name     TEXT NOT NULL DEFAULT '',
    channel_id      TEXT NOT NULL DEFAULT '',
    mode            TEXT NOT NULL DEFAULT '',
    reply_target    JSONB
);

CREATE INDEX IF NOT EXISTS idx_inbound_pairings_connection ON inbound_pairings (tenant_id, connection_id);

ALTER TABLE inbound_pairings ENABLE ROW LEVEL SECURITY;
ALTER TABLE inbound_pairings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON inbound_pairings USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
