-- 0020: per-user OAuth for MCP servers (docs/mcp-oauth.md).
--
-- mcp_connections holds one person's connection to one MCP server: the refresh
-- and access tokens as envelope-encrypted blobs (internal/connections/crypto.go),
-- never plaintext. mcp_oauth_apps holds the OAuth app an admin registered for a
-- server (the client secret is encrypted too). mcp_oauth_flows holds the pending
-- authorization requests between "Connect" and the callback, so any operator
-- replica can finish a flow another one started.
--
-- Every table carries tenant_id with the row-level security of 0010; the store
-- also filters on user_id, because RLS isolates tenants and the predicate
-- isolates users inside one.
CREATE TABLE IF NOT EXISTS mcp_connections (
    id                BIGSERIAL PRIMARY KEY,
    tenant_id         TEXT NOT NULL,
    user_id           TEXT NOT NULL,
    server_ns         TEXT NOT NULL,
    server_name       TEXT NOT NULL,
    -- the MCPServer's UID: a server deleted and recreated under the same name
    -- does not inherit the credential.
    server_uid        TEXT NOT NULL,
    scopes            TEXT[] NOT NULL DEFAULT '{}',
    account_label     TEXT NOT NULL DEFAULT '',
    refresh_token_enc BYTEA NOT NULL,
    access_token_enc  BYTEA,
    access_expires_at TIMESTAMPTZ,
    key_id            TEXT NOT NULL,
    status            TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'needs_reauth', 'revoked')),
    last_used_at      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, user_id, server_ns, server_name)
);

CREATE INDEX IF NOT EXISTS idx_mcp_connections_server ON mcp_connections (tenant_id, server_ns, server_name);

CREATE TABLE IF NOT EXISTS mcp_oauth_apps (
    id                BIGSERIAL PRIMARY KEY,
    tenant_id         TEXT NOT NULL,
    server_ns         TEXT NOT NULL,
    app_ref           TEXT NOT NULL,
    client_id         TEXT NOT NULL,
    client_secret_enc BYTEA,
    key_id            TEXT NOT NULL,
    redirect_uri      TEXT NOT NULL,
    scopes_allowed    TEXT[] NOT NULL DEFAULT '{}',
    updated_by        TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, server_ns, app_ref)
);

CREATE TABLE IF NOT EXISTS mcp_oauth_flows (
    id               BIGSERIAL PRIMARY KEY,
    -- SHA-256 of the state value; the state itself is never stored.
    state_hash       TEXT NOT NULL UNIQUE,
    tenant_id        TEXT NOT NULL,
    user_id          TEXT NOT NULL,
    server_ns        TEXT NOT NULL,
    server_name      TEXT NOT NULL,
    server_uid       TEXT NOT NULL,
    verifier_enc     BYTEA NOT NULL,
    key_id           TEXT NOT NULL,
    requested_scopes TEXT[] NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at       TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_mcp_oauth_flows_expiry ON mcp_oauth_flows (expires_at);

ALTER TABLE mcp_connections ENABLE ROW LEVEL SECURITY;
ALTER TABLE mcp_connections FORCE ROW LEVEL SECURITY;
ALTER TABLE mcp_oauth_apps  ENABLE ROW LEVEL SECURITY;
ALTER TABLE mcp_oauth_apps  FORCE ROW LEVEL SECURITY;
ALTER TABLE mcp_oauth_flows ENABLE ROW LEVEL SECURITY;
ALTER TABLE mcp_oauth_flows FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON mcp_connections USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
CREATE POLICY tenant_isolation ON mcp_oauth_apps  USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
CREATE POLICY tenant_isolation ON mcp_oauth_flows USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
