# Upgrading from v0.12.x to v0.13.0

v0.13.0 is the "UI adoption" release: tenant IDs and isolation, GitOps-safe ownership, the new UI, evaluations, triggers,
schedules, per-user MCP OAuth and inbound channels (see the [changelog](../CHANGELOG.md)). It changes the database, the CRDs
and the admission webhooks. Read this page before you upgrade a cluster that holds data you care about.

## Before you start

1. **Back up PostgreSQL.** The operator applies seventeen migrations (0008 to 0024) when it starts, including the migration that
   turns on row-level security. There is no automatic rollback. With CloudNativePG: take an on-demand `Backup` and check it
   completed. A cluster without a backup cannot be recovered if a migration fails halfway.
2. **Upgrade the CRDs by hand.** `helm upgrade` does not touch the `crds/` directory of a chart. Apply them before (or right
   after) the upgrade, with server-side apply because some CRDs are large:

   ```sh
   helm pull oci://ghcr.io/heddles/agent-orca/charts/agent-orca --version 0.13.0 --untar
   kubectl apply --server-side --force-conflicts -f agent-orca/crds/
   ```

   Changed: `agents`, `tools`, `mcpservers`, `tenantconfigs`, `routingbenchmarks`; new: `agentschedules`.
3. **Use `--reset-then-reuse-values`**, not `--reuse-values`: with plain `--reuse-values` a setting the new chart added is
   missing, and some render as 0 or empty (for example `operator.connections.idleExpiryDays` rendered `0`, which turns the job off).

   ```sh
   helm upgrade agent-orca oci://ghcr.io/heddles/agent-orca/charts/agent-orca --version 0.13.0 \
     -n agent-orca-system --reset-then-reuse-values
   ```

## What changes for existing objects

- **Tenants get an ID.** Every TenantConfig gets a generated, immutable `tnt_...` ID (`status.tenantID`). Existing objects in
  the tenant's namespaces are labelled `agentorca.io/tenant-id` and existing database rows are backfilled by migration. A namespace can belong to one tenant only, unless the Namespace carries the label `agentorca.io/namespace-mode=shared`.
- **Roles.** Inside a tenant there are four roles: viewer, runner, builder, admin. A federated user's role comes from
  `TenantConfig.spec.roles` (group mapping). Machine tokens get `MACHINE_DEFAULT_ROLE` (chart value `machineDefaultRole`,
  default `runner`): check what your automation needs.
- **Ownership: one writer per object.** Objects the UI creates carry the annotation `agentorca.io/owner: ui`. The validating webhook
  **denies** a write from Argo CD, Flux or Helm to a UI-owned object (see [gitops-coexistence.md](gitops-coexistence.md)); it has
  `failurePolicy: Fail`, so the operator and its webhook certificate must be healthy for the UI kinds to be writable at all.
  Objects you created with `kubectl` or GitOps are not UI-owned and are not affected.
- **Network policy.** A run may reach an MCP server, tool or sandbox only if that workload's pods carry
  `agentorca.io/component: mcp-server|tool|sandbox` and the tenant's `agentorca.io/tenant-id`. The operator sets both on
  workloads it creates. **A hand-written Deployment that serves an MCPServer needs the two labels** or the router discovers
  zero tools and the server never sees a request.
- **MCP over HTTP** now speaks Streamable HTTP (MCP 2025-03-26 and later), so servers built on the official SDKs work.

## New settings (all have defaults; nothing is required)

| Value | Default | What it does |
|---|---|---|
| `database.connectionKeySecret` | empty | Secret holding the key ring that encrypts per-user MCP OAuth tokens. Per-user connections stay off without it ([mcp-oauth.md](mcp-oauth.md)). |
| `operator.connections.redirectURI` | empty | The UI's public callback URL, `https://<ui>/api/connections/callback`. |
| `operator.connections.idleExpiryDays` | 90 | Idle connections are revoked and marked expired. 0 turns it off. |
| `database.archivedRunsRetentionDays` | 0 | After N days the output and trace of an archived run are emptied (the summary row stays). 0 keeps everything. |
| `operator.builder.*` | see `values.yaml` | Caps for "Check with my cases" and the knowledge upload; `draftProvider` names the ModelProvider for "Describe what you want" (without it that box is disabled). |
| `operator.inbound.enabled` | true | Inbound channels (email, Slack, Teams, ...). Nothing runs without a connection. |
| `approvals.uiURL` | empty | The UI's public URL; run links in schedule notifications are left out without it. |

## Operating notes

- **Inbound channels need a public HTTPS URL** for Slack, Teams, Google Chat and Webex (`/inbound/{kind}/{id}`). Expose only
  `/inbound`. With Tailscale Funnel: allow the `funnel` node attribute for the operator's proxy tag, and **do not give the
  Funnel proxy a ProxyClass that sets `TS_DEBUG_MTU` below 1280**: the proxy then has no IPv6 address, Funnel ingress nodes
  connect over IPv6, and every request is dropped with `no rules matched`.
- **Credentials for channels** live in a Kubernetes Secret that an administrator labels `agentorca.io/inbound-secret=true`; a
  builder can create a connection but cannot point it at another Secret.
- **Email, Teams, Google Chat, Webex and Mattermost are experimental**: tested only against fakes. See
  [KNOWN_ISSUES.md](KNOWN_ISSUES.md#known-gaps-in-v0130) for what is unverified and not built.
- **Local models and agent-as-tool:** raise `toolExecutionTimeoutSec` (default 60 s) when a sub-agent runs on a local model.

## Rolling back

Helm can roll the images back, but the migrations stay applied and the CRD changes are not undone. Restore the PostgreSQL
backup if you must return to v0.12.x.
