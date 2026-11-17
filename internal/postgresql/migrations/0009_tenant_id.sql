-- 0009: scope tenant data by the stable tenant ID. The existing "tenant"
-- columns hold a TenantConfig name, which an operator can delete and reuse, so
-- they cannot keep two same-name tenants apart. tenant_id holds the registry ID
-- (see 0008_tenants.sql). It stays NULL for a row that cannot be attributed;
-- such rows are visible only to the platform scope (see 0010) until
-- BackfillTenantIDs, which also knows the namespace bindings, claims them.
ALTER TABLE archived_runs   ADD COLUMN IF NOT EXISTS tenant_id TEXT;
ALTER TABLE approvals       ADD COLUMN IF NOT EXISTS tenant_id TEXT;
ALTER TABLE routing_events  ADD COLUMN IF NOT EXISTS tenant_id TEXT;
ALTER TABLE run_outcomes    ADD COLUMN IF NOT EXISTS tenant_id TEXT;
ALTER TABLE routing_feedback ADD COLUMN IF NOT EXISTS tenant_id TEXT;
ALTER TABLE shadow_results  ADD COLUMN IF NOT EXISTS tenant_id TEXT;
ALTER TABLE shadow_leases   ADD COLUMN IF NOT EXISTS tenant_id TEXT;

-- Backfill, pass 1: the tenant name on the row, when exactly one live tenant
-- in the registry has that name. Several (the same name in two namespaces) is
-- ambiguous and is left for the namespace pass.
UPDATE archived_runs x SET tenant_id = t.id FROM tenants t
 WHERE x.tenant_id IS NULL AND COALESCE(x.tenant, '') <> '' AND t.name = x.tenant AND t.state <> 'deleted'
   AND (SELECT count(*) FROM tenants u WHERE u.name = x.tenant AND u.state <> 'deleted') = 1;
UPDATE approvals x SET tenant_id = t.id FROM tenants t
 WHERE x.tenant_id IS NULL AND COALESCE(x.tenant, '') <> '' AND t.name = x.tenant AND t.state <> 'deleted'
   AND (SELECT count(*) FROM tenants u WHERE u.name = x.tenant AND u.state <> 'deleted') = 1;
UPDATE routing_events x SET tenant_id = t.id FROM tenants t
 WHERE x.tenant_id IS NULL AND COALESCE(x.tenant, '') <> '' AND t.name = x.tenant AND t.state <> 'deleted'
   AND (SELECT count(*) FROM tenants u WHERE u.name = x.tenant AND u.state <> 'deleted') = 1;
UPDATE run_outcomes x SET tenant_id = t.id FROM tenants t
 WHERE x.tenant_id IS NULL AND COALESCE(x.tenant, '') <> '' AND t.name = x.tenant AND t.state <> 'deleted'
   AND (SELECT count(*) FROM tenants u WHERE u.name = x.tenant AND u.state <> 'deleted') = 1;
UPDATE routing_feedback x SET tenant_id = t.id FROM tenants t
 WHERE x.tenant_id IS NULL AND COALESCE(x.tenant, '') <> '' AND t.name = x.tenant AND t.state <> 'deleted'
   AND (SELECT count(*) FROM tenants u WHERE u.name = x.tenant AND u.state <> 'deleted') = 1;
UPDATE shadow_results x SET tenant_id = t.id FROM tenants t
 WHERE x.tenant_id IS NULL AND COALESCE(x.tenant, '') <> '' AND t.name = x.tenant AND t.state <> 'deleted'
   AND (SELECT count(*) FROM tenants u WHERE u.name = x.tenant AND u.state <> 'deleted') = 1;

-- Pass 2: the row's namespace, when exactly one live tenant is registered in
-- it. shadow_leases has no tenant name, so only this pass reaches it.
UPDATE archived_runs x SET tenant_id = t.id FROM tenants t
 WHERE x.tenant_id IS NULL AND t.k8s_namespace = x.namespace AND t.state <> 'deleted'
   AND (SELECT count(*) FROM tenants u WHERE u.k8s_namespace = x.namespace AND u.state <> 'deleted') = 1;
UPDATE approvals x SET tenant_id = t.id FROM tenants t
 WHERE x.tenant_id IS NULL AND t.k8s_namespace = x.namespace AND t.state <> 'deleted'
   AND (SELECT count(*) FROM tenants u WHERE u.k8s_namespace = x.namespace AND u.state <> 'deleted') = 1;
UPDATE routing_events x SET tenant_id = t.id FROM tenants t
 WHERE x.tenant_id IS NULL AND t.k8s_namespace = x.namespace AND t.state <> 'deleted'
   AND (SELECT count(*) FROM tenants u WHERE u.k8s_namespace = x.namespace AND u.state <> 'deleted') = 1;
UPDATE run_outcomes x SET tenant_id = t.id FROM tenants t
 WHERE x.tenant_id IS NULL AND t.k8s_namespace = x.namespace AND t.state <> 'deleted'
   AND (SELECT count(*) FROM tenants u WHERE u.k8s_namespace = x.namespace AND u.state <> 'deleted') = 1;
UPDATE routing_feedback x SET tenant_id = t.id FROM tenants t
 WHERE x.tenant_id IS NULL AND t.k8s_namespace = x.namespace AND t.state <> 'deleted'
   AND (SELECT count(*) FROM tenants u WHERE u.k8s_namespace = x.namespace AND u.state <> 'deleted') = 1;
UPDATE shadow_results x SET tenant_id = t.id FROM tenants t
 WHERE x.tenant_id IS NULL AND t.k8s_namespace = x.namespace AND t.state <> 'deleted'
   AND (SELECT count(*) FROM tenants u WHERE u.k8s_namespace = x.namespace AND u.state <> 'deleted') = 1;
UPDATE shadow_leases x SET tenant_id = t.id FROM tenants t
 WHERE x.tenant_id IS NULL AND t.k8s_namespace = x.namespace AND t.state <> 'deleted'
   AND (SELECT count(*) FROM tenants u WHERE u.k8s_namespace = x.namespace AND u.state <> 'deleted') = 1;

CREATE INDEX IF NOT EXISTS idx_archived_runs_tenant_id    ON archived_runs(tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_approvals_tenant_id        ON approvals(tenant_id, status, requested_at DESC);
CREATE INDEX IF NOT EXISTS idx_routing_events_tenant_id   ON routing_events(tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_run_outcomes_tenant_id     ON run_outcomes(tenant_id);
CREATE INDEX IF NOT EXISTS idx_routing_feedback_tenant_id ON routing_feedback(tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_shadow_results_tenant_id   ON shadow_results(tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_shadow_leases_tenant_id    ON shadow_leases(tenant_id);

-- Report what could not be attributed. The operator repeats this after the
-- namespace-binding pass (BackfillTenantIDs) and logs it through slog.
DO $$
DECLARE
    t TEXT;
    n BIGINT;
BEGIN
    FOREACH t IN ARRAY ARRAY['archived_runs', 'approvals', 'routing_events', 'run_outcomes',
                             'routing_feedback', 'shadow_results', 'shadow_leases']
    LOOP
        EXECUTE format('SELECT count(*) FROM %I WHERE tenant_id IS NULL', t) INTO n;
        IF n > 0 THEN
            RAISE NOTICE 'tenant_id backfill: % rows in % have no tenant yet', n, t;
        END IF;
    END LOOP;
END $$;
