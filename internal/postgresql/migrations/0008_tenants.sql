-- 0008: the tenant registry. A tenant is identified by an opaque, never reused
-- ID ("tnt_" plus a UUIDv7), not by its TenantConfig name, which an operator
-- can delete and reuse. This table is the source of truth for which IDs exist;
-- TenantConfig.status.tenantID mirrors it. k8s_uid tells a recreated
-- TenantConfig apart from the one it replaced. Other tables gain tenant_id
-- columns in a later migration.
CREATE TABLE tenants (
    id            TEXT PRIMARY KEY CHECK (id ~ '^tnt_[0-9a-f-]{36}$'),
    name          TEXT NOT NULL,
    k8s_namespace TEXT NOT NULL,
    k8s_uid       TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'suspended', 'deleted')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ
);

CREATE INDEX tenants_live_name_idx ON tenants (k8s_namespace, name) WHERE state <> 'deleted';
