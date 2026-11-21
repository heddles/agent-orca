-- 0021: lifecycle of per-user MCP connections (docs/mcp-oauth.md).
--
-- 'expired' is a connection that went unused for the idle period: its tokens are
-- wiped (and revoked at the provider, best effort) and the row stays so its owner
-- sees why, and can reconnect. last_used_at already exists (0020); the index lets
-- the idle sweep and the key-rotation job find their rows without a table scan.
ALTER TABLE mcp_connections DROP CONSTRAINT IF EXISTS mcp_connections_status_check;
ALTER TABLE mcp_connections ADD CONSTRAINT mcp_connections_status_check
    CHECK (status IN ('active', 'needs_reauth', 'revoked', 'expired'));

CREATE INDEX IF NOT EXISTS idx_mcp_connections_idle
    ON mcp_connections ((COALESCE(last_used_at, created_at)))
    WHERE status IN ('active', 'needs_reauth');

CREATE INDEX IF NOT EXISTS idx_mcp_connections_key
    ON mcp_connections (key_id, id)
    WHERE status IN ('active', 'needs_reauth');
