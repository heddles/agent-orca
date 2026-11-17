-- 0010: row-level security as the backstop under the tenant_id filters every
-- query already applies. The store sets two transaction-local settings:
--
--   agentorca.tenant_id  the caller's tenant ID
--   agentorca.platform   'on' for operator-internal and platform-admin paths
--
-- A row is visible and writable when its tenant_id equals agentorca.tenant_id,
-- or when agentorca.platform is 'on'. When neither setting is set,
-- current_setting(..., true) returns NULL (or '' once a prior transaction has
-- touched it), the comparison is not true, and NO rows match: an unset setting
-- fails closed. NULLIF keeps '' from matching a row whose tenant_id is ''.
--
-- FORCE applies the policies to the table owner too. The operator connects as
-- the owner, so without FORCE the policies would never run.
CREATE FUNCTION agentorca_tenant_visible(row_tenant TEXT) RETURNS BOOLEAN
LANGUAGE sql STABLE AS $$
    SELECT COALESCE(current_setting('agentorca.platform', true), '') = 'on'
        OR (row_tenant IS NOT NULL
            AND row_tenant = NULLIF(current_setting('agentorca.tenant_id', true), ''))
$$;

ALTER TABLE archived_runs    ENABLE ROW LEVEL SECURITY;
ALTER TABLE archived_runs    FORCE ROW LEVEL SECURITY;
ALTER TABLE approvals        ENABLE ROW LEVEL SECURITY;
ALTER TABLE approvals        FORCE ROW LEVEL SECURITY;
ALTER TABLE routing_events   ENABLE ROW LEVEL SECURITY;
ALTER TABLE routing_events   FORCE ROW LEVEL SECURITY;
ALTER TABLE run_outcomes     ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_outcomes     FORCE ROW LEVEL SECURITY;
ALTER TABLE routing_feedback ENABLE ROW LEVEL SECURITY;
ALTER TABLE routing_feedback FORCE ROW LEVEL SECURITY;
ALTER TABLE shadow_results   ENABLE ROW LEVEL SECURITY;
ALTER TABLE shadow_results   FORCE ROW LEVEL SECURITY;
ALTER TABLE shadow_leases    ENABLE ROW LEVEL SECURITY;
ALTER TABLE shadow_leases    FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON archived_runs    USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
CREATE POLICY tenant_isolation ON approvals        USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
CREATE POLICY tenant_isolation ON routing_events   USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
CREATE POLICY tenant_isolation ON run_outcomes     USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
CREATE POLICY tenant_isolation ON routing_feedback USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
CREATE POLICY tenant_isolation ON shadow_results   USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
CREATE POLICY tenant_isolation ON shadow_leases    USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
