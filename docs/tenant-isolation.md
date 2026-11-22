# Tenant isolation

Every tenant-owned object and every row of tenant data carries the tenant's ID. Each layer below compares the ID, never the tenant name. The ID is assigned when a `TenantConfig` is first reconciled (see [TenantConfig](crds.md#11-tenantconfig)) and shows up as `status.tenantID`.

## The layers

| Layer | What isolates | Where |
|---|---|---|
| Identity | Session and issued tokens carry a `tid` claim. Kubernetes ServiceAccount tokens resolve to the `TenantConfig` that claims the ServiceAccount's namespace, and carry its ID. | `internal/apiserver/auth.go` |
| Labels and webhooks | `agentorca.io/tenant-id` on every tenant-owned object. Webhooks default it from the namespace, make it immutable and reject references to another tenant's objects. `AgentSchedule` is covered like the other kinds: it can reference only an agent or template of its own namespace and tenant. Objects shared by the platform carry `agentorca.io/scope=platform` instead. | `internal/webhook`, `internal/tenancy` |
| API filtering | The UI, external and ACP APIs hide objects of other tenants. `/api/me` reports the caller's tenant. | `internal/apiserver` |
| PostgreSQL | A `tenant_id` column on every tenant table, with row-level security (RLS) as a backstop under the query filters. | `internal/postgresql` |
| Redis | A run's keys are prefixed `t:<tenant ID>:`. The operator, the APIs and the model-router all build keys the same way. | `internal/state/tenant.go` |
| Qdrant | The default collection name includes the tenant ID. Point IDs are derived from the tenant ID, document ID and chunk index. Every read filters on the `tenant_id` payload field, and every write refuses to overwrite a point that another tenant owns. | `internal/rag` |
| Hindsight | One memory bank per tenant. | `internal/router` |
| Network | Run pods may reach only Redis, the operator API, and the tenant's own (or platform) tool and MCP pods. A per-namespace ingress policy closes the tenant's tool, MCP and sandbox pods to other tenants. | `internal/security/network_policy*.go` |

### Qdrant details

Collections that already exist keep the name recorded in `status.collectionName`, so no data moves. Only new KnowledgeBases get `<tenant ID>-<name>`. An explicit `spec.vectorStore.collectionName` always wins.

On each reconcile of a KnowledgeBase the operator stamps `tenant_id` on points that have none, then re-keys the tenant's points from the old ID scheme (document ID and chunk index only) to the tenant-derived one. This copies the vector and payload, so it needs no re-embedding, and it is safe to repeat. Points whose payload lacks `doc_id` or `chunk_index` stay under their old ID until their document is re-ingested. They are still filtered by tenant on read.

### Redis details

Reads try the prefixed key first and fall back to the old unprefixed key. Writes always go to the prefixed key, so a run that started before the upgrade keeps working and moves on its next write. Old keys expire with their TTLs. The prefix is on by default. Set `operator.stateTenantPrefix: false` (the `AGENTORCA_STATE_TENANT_PREFIX` variable) to turn it off for the operator, the APIs and the router together. Change it with no runs in flight.

### Network details

Port 443 and 6443 (provider APIs and the kube-apiserver) and tool or Hindsight endpoints outside the cluster stay port-only, because a NetworkPolicy cannot select them. If `OperatorNamespace` is unknown (`POD_NAMESPACE` unset), the Redis and operator-API rules also stay port-only. An unlabelled run keeps the old port-only policy until it is labelled.

The `TenantConfig` reconciler creates one `agentorca-tenant-<tenant ID>` policy in each namespace in `spec.allowedNamespaces`. It removes the policy when the namespace leaves the list or the tenant is deleted. A claimed namespace that does not exist yet gets its policy on a later reconcile.

## Rolling out: audit, then enforce

`operator.tenantIsolationMode` (`TENANT_ISOLATION_MODE`) is `audit` by default. In audit mode a label or cross-tenant reference violation is logged and counted in `agentorca_tenant_isolation_violations_total`, and the request goes through. In `enforce` mode it is denied, and objects without `agentorca.io/tenant-id` are hidden from the APIs.

1. **Deploy in audit mode.** At startup the operator backfills `tenant_id` on existing PostgreSQL rows and labels existing objects in namespaces claimed by a single tenant. Check the logs for the backfill result.
2. **Read the audit warnings and the metric.** Each violation names the rule, kind, namespace and name. Watch `agentorca_tenant_isolation_violations_total` by rule.
3. **Label what the backfill could not attribute.** Secrets and Tools in a shared namespace (several tenants claim it) or an unclaimed one have no single owner. Set `agentorca.io/tenant-id=<id>` on each, or `agentorca.io/scope=platform` for something every tenant may use. Never set both.
4. **Switch to `enforce`** once the metric stays at zero.

## Row-level security in PostgreSQL

Every store call runs in a transaction that sets two settings: `agentorca.tenant_id` and `agentorca.platform`. A row is visible and writable when its `tenant_id` matches the first, or when `agentorca.platform` is `on`. With neither set, no rows match. A call with no scope fails with `ErrNoTenantScope`, and the store never falls back to all rows.

**The platform bypass.** The platform scope sets `agentorca.platform=on`, which sees every tenant's rows. Only operator-internal paths (controllers, the router-facing internal API, retention) and platform admins use it. In the API, only a request with no identity at all (authentication disabled, or the UI proxy's service-account flow) maps to the platform scope. An identity without a tenant ID gets no scope, and the store refuses its calls.

**Use a role that RLS applies to.** The policies are `FORCE`d, so they apply to the table owner. They do not apply to a superuser or to any role with `BYPASSRLS`, which skip RLS silently. The operator's DSN (`database.dsn`) must therefore name a plain login role.

The CloudNativePG chart in `charts/cluster` satisfies this by default. Its app user is the database owner (`cluster.initdb.owner`, which defaults to the database name). It is created without superuser rights, and its credentials go in the `<cluster>-app` Secret, which is the `postgres-app` Secret that `database.passwordSecret` points at. The chart also sets `cluster.enableSuperuserAccess: true`, which creates a separate `postgres` superuser and Secret. Do not put that user in the DSN. Set `enableSuperuserAccess: false` where nothing needs it. Roles you add under `cluster.roles` must not set `superuser: true` or `bypassrls: true` if the operator connects as them.

To check, run `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user;` as the operator's role. Both must be `f`.

## The legacy-token flag

Tokens minted before tenant IDs existed have no `tid` claim. By default (`operator.allowLegacyTenantTokens: true`, `ALLOW_LEGACY_TENANT_TOKENS`) they are accepted and mapped to a tenant by name. This is a one-release bridge. A legacy token of a tenant with no ID gets no PostgreSQL scope, so it cannot read tenant data. Set the flag to `false` once existing sessions have rolled over.

## Platform catalog namespaces

Normally an object is visible to a tenant only when its namespace is one the tenant may use. That would make shared objects, such as agent templates, visible to the one tenant that claims their namespace. Platform catalog namespaces fix this. In a namespace listed in `operator.platformNamespaces` (`PLATFORM_NAMESPACES`, default the operator's own namespace), an object labelled `agentorca.io/scope=platform` is readable by every tenant, whatever its allowed namespaces.

- Everything else in a platform catalog namespace stays hidden from tenants that do not claim it. A tenant-labelled object there is not listed and a get returns 404.
- Platform objects are read only for tenants. The UI API refuses to run a template agent (`agentorca.io/template: "true"`) with a 409, so no tenant starts runs inside the platform namespace. Using a template copies it into the tenant's own workspace.
- The UI and ACP lists include the platform-scoped objects of these namespaces next to the tenant's own.

## Helm values

| Value | Default | Effect |
|---|---|---|
| `operator.tenantIsolationMode` | `audit` | `audit` or `enforce` (see above) |
| `operator.platformNamespaces` | `[]` (the release namespace) | Platform catalog namespaces (see above) |
| `operator.stateTenantPrefix` | `true` | Prefix Redis keys with the tenant ID |
| `operator.allowLegacyTenantTokens` | `true` | Accept tokens with no `tid` claim |
| `operator.tenantAdminGroup` | `""` | Group whose members may pin the ID of a deleted `TenantConfig`, which restores that tenant's data |
| `database.dsn`, `database.passwordSecret` | see `values.yaml` | PostgreSQL connection. The role must not bypass RLS |

## Roles inside a tenant

`TenantConfig.spec.roles` maps OIDC groups to `viewer`, `runner`, `builder` and
`admin`. Roles are cumulative (admin includes builder includes runner includes
viewer) and are resolved on every request from the caller's groups and the live
TenantConfig, so a change reaches sessions already issued. `defaultRole` gives a
role to users in none of the listed groups.

```yaml
spec:
  roles:
    viewer: [everyone]
    builder: [devs]
    admin: [platform]
    defaultRole: viewer
```

**When `spec.roles` is unset every signed-in user is a `runner`**, which is what
tenants could do before roles existed. Once it is set, a user in none of the
groups and with no `defaultRole` has no access (`/api/me` still answers, with
empty roles). Callers with no user behind them (ServiceAccount tokens, issued
client-credentials tokens) get `operator.machineDefaultRole` (default `runner`).
A request with no identity (authentication disabled) is not subject to roles.
Federated bearer tokens are mapped like an interactive login, through
`spec.federated.claimMappings` (defaults `sub` and `groups`); a token with no user
claim is a machine caller. Reading your own tenant at `GET /admin/tenants/{name}`
with a federated token needs the admin role.

| Role | May |
|---|---|
| viewer | read everything in the tenant |
| runner | start, cancel and chat with runs, answer clarifications, send feedback |
| builder | create, update and delete resources in `/api/resources`, `/api/agents`, deployments; ask for a routing recommendation action |
| admin | write ModelProvider, ModelSelector, MCPServer and Tool; decide approvals; read `/api/audit` |

Approval decisions are admin, not builder: an approval is the control on what a
builder's agents may do, so the people who write agents should not also be the
ones who release them. A tenant that wants builders to approve can put the same
group in both lists.

## Ownership, writes and audit

Objects created through the UI API carry `agentorca.io/owner: ui`; the UI updates
and deletes only those. Updates are server-side apply (field manager
`agent-orca-ui`) and need `metadata.resourceVersion`: a stale one is a 409 that
names the current version. Each write sets `agentorca.io/last-modified-by` and
`-at` and adds a row to `audit_events` (row-level security like the other tenant
tables). If the audit row cannot be written after three tries the write still
succeeds, since it cannot be undone and a retried request would apply it twice;
the response carries `X-Audit-Status: failed`, the event is logged in full and
`agentorca_audit_write_failures_total` counts it. Besides writes, the trail records what a signed-in user says to an agent: `chat` (a turn sent to a deployment), `answer` (an answer to a run's clarification question), `feedback` (a rating of a run or one of its routing decisions) and the `approve` and `reject` decisions on approvals. Those rows hold the actor, tenant, namespace, target and ids and byte counts only, never the message, answer or comment text. `GET /api/audit?action=chat` filters them. Chat turns through the widget (anonymous visitors), the ACP API and the external task API are not in the trail yet. At startup the leader converts
objects the old UI API made (label `app.kubernetes.io/managed-by: agent-orca`, no
controller owner) to the annotation; ambiguous ones (workflows, Git-tracked,
platform-shared) are logged and left alone.
