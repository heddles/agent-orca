# Tenant migration

A tenant's ID is generated when its `TenantConfig` is first reconciled (see [Tenant isolation](tenant-isolation.md)). Move agent-orca to another cluster, restore its PostgreSQL there, and apply the `TenantConfig` again, and the new `TenantConfig` is a new object. It gets a **new** ID. The old ID stays in the restored data, and every layer that compares IDs treats that data as belonging to a tenant that no longer exists.

This page covers how to keep the ID across a move, and how to re-key a tenant after the ID has already changed. The recovery steps were run for a k3s to Talos move and are marked where something was not covered.

## What you see when the ID changed

| Symptom | Why |
|---|---|
| ModelProviders, ModelSelectors and other objects you copied are missing from the UI, though `kubectl get` lists them. | They carry the old `agentorca.io/tenant-id` label. The UI API filters on the caller's tenant ID. |
| Run history, audit trail, routing events and Slack or webhook channels are empty. | Their rows carry the old `tenant_id`. |
| A Slack message to a channel fails with `agent_unavailable`. | The connection row still belongs to the old tenant, so its agent cannot be resolved under the new one. |
| `tenant isolation (audit mode, would be denied)` warnings naming the old ID. | A label points at a tenant no `TenantConfig` claims. |

Confirm with the registry. The old tenant is marked `deleted` at the moment the new one is created:

```sql
SELECT id, name, state, created_at, deleted_at FROM tenants WHERE name = '<TenantConfig name>' ORDER BY created_at;
```

`kubectl get tenantconfig <name> -o jsonpath='{.status.tenantID}'` is the ID in force now.

## Keep the ID (preferred)

Pin the old ID in the new cluster's `TenantConfig` before it is first reconciled:

```yaml
spec:
  tenantID: tnt_<old ID>
```

The admission webhook enforces these rules:

- The value is `tnt_` plus a UUIDv7, and it is immutable once set.
- It must not be bound to another live `TenantConfig`.
- A `TenantConfig` that already has a generated `status.tenantID` cannot be pinned to a different one. A live tenant is never rebound. Pinning the ID it already has is allowed, which is how GitOps adopts a generated ID.
- If the ID belongs to a tenant the registry marks `deleted` (the usual case after a restore), the request must come from a member of `operator.tenantAdminGroup` (`TENANT_ADMIN_GROUP`). With that value empty, nobody may restore a deleted ID. The check needs the PostgreSQL registry, so it applies only when `database` is configured.

Restore the database first, set `operator.tenantAdminGroup`, then apply the pinned `TenantConfig` as a user in that group. Labels, rows and Redis keys then already match and nothing below is needed. If the new `TenantConfig` has already reconciled, it cannot be pinned afterwards. Delete it, which marks its ID deleted, and apply it again with the pin.

When you copy manifests between clusters, drop `agentorca.io/tenant-id` from them. The webhook then stamps the new tenant's ID, instead of a copied label keeping the old one.

## Re-key to the new ID

Use this when the new ID is already in use and you want the old tenant's objects and data under it. Write `OLD` and `NEW` for the two IDs.

1. **Back up the database.**

   ```sh
   kubectl -n <ns> exec <postgres pod> -c postgres -- pg_dump -U postgres -d agentorca -Fc > pre-rekey.dump
   ```

2. **Relabel the objects.** Anything created by Argo CD or the UI API after the move already has `NEW`. Hand-applied or copied objects keep `OLD`. Find them:

   ```sh
   kubectl get modelproviders,modelselectors,agents,agentdeployments,agentschedules,agentworkflows,mcpservers,tools,knowledgebases \
     -A -l agentorca.io/tenant-id=OLD
   kubectl -n <ns> label <kind> <name> agentorca.io/tenant-id=NEW --overwrite
   ```

   The label is immutable once set. In `audit` mode the change goes through with a `label-immutable` warning. In `enforce` mode it is denied: delete the object and apply it again without the label.

3. **Re-key the rows.** Row-level security is `FORCE`d, so run this as the `postgres` superuser (`cluster.enableSuperuserAccess: true`). The operator's role sees no rows of another tenant. It runs in one transaction, so a failure leaves nothing half-moved.

   ```sql
   BEGIN;
   DO $$
   DECLARE r record; n bigint;
   BEGIN
     FOR r IN SELECT table_name FROM information_schema.columns
              WHERE table_schema = 'public' AND column_name = 'tenant_id' AND table_name <> 'tenants' ORDER BY 1 LOOP
       EXECUTE format('UPDATE public.%I SET tenant_id = %L WHERE tenant_id = %L', r.table_name, 'NEW', 'OLD');
       GET DIAGNOSTICS n = ROW_COUNT;
       IF n > 0 THEN RAISE NOTICE 'moved % rows in %', n, r.table_name; END IF;
     END LOOP;
   END $$;
   COMMIT;
   ```

   This covers runs, audit events, routing data, approvals, evaluations, MCP connections, inbound channels and their message records, triggers and widget keys. Move a channel's `inbound_messages` together with its `inbound_connections`. Those rows are the de-duplication state, and without them a connection can reply to old Slack messages again.

4. **Check.** No `tenant_id` column should still hold `OLD`:

   ```sql
   SELECT table_name, (xpath('/row/c/text()', query_to_xml(
     format('SELECT count(*) AS c FROM public.%I WHERE tenant_id = %L', table_name, 'OLD'), false, true, '')))[1]::text
   FROM information_schema.columns WHERE table_schema = 'public' AND column_name = 'tenant_id' AND table_name <> 'tenants' ORDER BY 1;
   ```

5. **Review what pointed at things that no longer exist.** Re-keying moves a row, it does not repair it. A trigger or inbound connection whose `agent` was deleted still has no agent to run.

## What re-keying does not touch

| Item | Detail |
|---|---|
| `tenants` registry | Leave the old row `deleted` and the new one `active`. |
| `archived_runs.labels` | A snapshot of the Kubernetes labels at archive time. It still names `OLD`. This is history, not ownership. |
| Redis | Run state keys are `t:<tenant ID>:...`. Reads fall back to the old *unprefixed* key, never to another tenant's prefix, so state under `t:OLD:` is not seen. It matters only for runs and conversations in flight. To keep it, rename: `redis-cli --scan --pattern 't:OLD:*'` piped to `RENAME` with the new prefix. Not covered by the migration this page was written from. |
| Qdrant | Existing collections keep the name in `status.collectionName`. Points carry a `tenant_id` payload that the KnowledgeBase reconciler stamps only where it is missing, so points already stamped `OLD` stay `OLD`. Not covered. |
| Hindsight | One memory bank per tenant. A bank created under `OLD` is not visible to `NEW`. In the migration this page came from it held no data. |

## Rolling back

Restore the dump from step 1 into an empty database (`pg_restore --clean --if-exists`) and relabel any objects you changed. The `tenants` registry is part of the dump, so it returns to its earlier state too.
