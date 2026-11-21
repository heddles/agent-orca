# UI adoption overhaul — agents for everyone, GitOps-safe

## Status (2026-11-20)

Everything below is merged to `main` (head dd192e4) unless marked otherwise. The stage checklists
further down were written as the plan and are not ticked item by item; this table is the current
state. Per-stage detail is in the PRs (#199-#234) and in `docs/`.

| Stage | State | Notes |
|---|---|---|
| U0 tenant identity and isolation | Merged | Generated `tnt_` IDs, JWT `tid`, label stamping, Postgres RLS, NetworkPolicies by tenant ID. Items not individually re-checked for this update: Redis, object store, vector DB and Hindsight scoping, audit-then-enforce rollout. |
| U1 safe write path and roles | Merged | viewer/runner/builder/admin, federated roles via `TenantConfig.spec.roles`, server-side apply, 409 on conflict, audit. |
| U2 ownership and GitOps coexistence | Merged, e2e open | Ownership webhook blocks GitOps writes to UI-owned objects (`failurePolicy: Fail`), handoff, export, `docs/gitops-coexistence.md`. **Not done:** the e2e with real Argo CD and Flux. Argo CD (v3.6.1, Helm release `argocd`) is installed on mlx-k3s, so the Argo half can run there; Flux is not installed. |
| U3 shell and IA | Merged | |
| U4 catalog, templates, My work | Merged | |
| U5 builder | Mostly merged | Guided builder, test panel, form/YAML, publish. **Not built:** document upload for knowledge, draft-from-description, "Check with my cases". |
| U6 versions, review, export | Merged | Revisions, diff, rollback, export. |
| U7 evaluations | Merged | Datasets, graders (incl. untrusted-judge publish gate), experiments, gates, retention, JSONL export. |
| U8 fleet overview | Merged | |
| U9 where people work | Partly merged | Webhook triggers and the embeddable chat widget are merged. **Not built:** Slack/Teams app, email trigger. |
| Scheduled runs (S1-S4) | Merged | `AgentSchedule` CRD, controller, API, ownership, UI. **Not built:** result notifications (S5). |
| Per-user MCP OAuth C1-C3 | Merged | Connect flow, encrypted store, `maintainer` mode, redaction, publish gate. |
| C4 `invoker` mode | Merged (#232) | Fails closed for schedule, trigger, widget, machine, child and workflow runs; pause with a connect card. |
| C5 lifecycle | Merged (#233) | Idle expiry (90 days), key-rotation job, bulk offboarding, metrics, migration 0021. |

### Verified on the mlx-k3s cluster
Routing, the live tenant page sweep, triggers, widget, schedules, and per-user OAuth `maintainer` mode
against a fake provider. **Not verified live:** C4 `invoker` pause and resume, C5 lifecycle jobs, the
warm-pod claim path for per-user servers (unit tests only; needs an http-input agent),  mlx-k3s was upgraded to `e2e-dd192e46` (operator, UI, router; main dd192e4, Helm revision 11) on 2026-11-20 with migration 0021 and the new CRD enum applied and the 90-day idle expiry on; the live checks above are still to do. Note: `helm upgrade` does not update CRDs, apply `charts/agent-orca/crds` with server-side apply.

### Remaining work, in order
1. Schedule result notifications (S5).
2. Slack, Teams and email triggers (U9 rest).
3. Builder gaps: document upload, draft-from-description, "Check with my cases".
4. `routing_events` filtering; audit of chat, answer and feedback.
5. `archived_runs` retention caveats.
6. Argo CD / Flux kind e2e (U2), last.

### Known issues and decisions pending
- Persistent `agentorca-deploy-*` pods have a placeholder run name and still fail the per-user token fetch;
  unknown whether anything uses them.
- `invoker` approval default is coarse: MCP discovery drops side-effect classes, so most invoker tools
  default to requiring approval until a policy sets the class.
- A scheduled or trigger run with no invoker connection gets no tools from that server instead of a
  `connection_required` error.
- Two `-race` test flakes exist on `main`: `TestRoutingRequestLogCarriesTheReason` and
  `TestClaimRun_RestartKeepsPriorRunSearchable`.
- The OIDC login keeps pending-login state in process (`oidc_handlers.go`), so a multi-replica operator can
  fail the callback.
- Test rows from the live e2e remain in cluster Postgres; cleanup must be scoped by `tenant_id`.
- The connections redirect URI on the cluster is a localhost placeholder.

## Goal

Anyone (business users, AI consumers) can find an agent, put it to work, build
their own, and see what it did, without kubectl or YAML. Engineers keep
deploying with CRD YAML and GitOps (Argo CD, Flux, Helm). Neither path may
silently overwrite the other, and no tenant can see or reach another tenant's
agents, data or runtime.

## Decisions (2026-11-13)

1. **When GitOps collides with a UI-owned object, block it.** There is no
   warn-only mode.
2. **No Git write-back.** agent-orca is deployed in too many environments
   (different Git hosts, repo layouts, Helm/Kustomize rendering) to edit
   repositories safely. Moving work between the UI and Git is done with YAML
   export and copy, by a human.
3. **No shared-field mode** (Git owns the object while the UI edits some
   fields). Not planned.
4. **Tenants get a generated, immutable tenant ID.** Namespace isolation stays,
   but as one layer of defense in depth, not the boundary.

## Where we are

Compared against Microsoft Foundry, LangSmith (Studio / Deployment / Fleet) and
Google Gemini Enterprise Agent Platform:

- The UI reads resources but cannot write them. Only chat, cancel, retry,
  approvals and feedback write anything.
- There is no playground or schema-driven run form. The UI never calls
  `GET /agents/{name}/manifest` or `POST /agents/{name}/run` (Stage 7).
- There are no evaluations, and the Marketplace tab says "Coming soon".
- 18 top-level nav items are named after CRD kinds. Costs hide behind an icon.
  "Runs" and "History" are two separate views of runs.
- No URL routing: navigation lives in `sessionStorage` (`ui/src/main.tsx:36`).
- No user menu or workspace switcher: namespace is hardcoded to `''`
  (`ui/src/main.tsx:292`).

### Backend gaps this plan has to fix first

**Generic write API** (`/api/resources`, `internal/apiserver/uiapi.go:2404-2585`):

1. `getResource`, `updateResource` and `deleteResource` do not check the
   tenant's namespaces. Only `createResource` forces the namespace.
2. `updateResource` stamps `app.kubernetes.io/managed-by: agent-orca` on any
   object that lacks it. That includes Argo- and Flux-applied manifests. After
   one UI edit, `deleteResource`'s guard then allows the UI to delete a
   Git-owned resource.
3. Controllers use the same label for objects they create, so "made in the UI"
   cannot be told apart from "made by a controller".
4. Writes use a full `Update` as the operator's service account. Nothing
   records which user made the change, nothing checks roles within a tenant,
   and there is no conflict detection.

**Tenant identity:**

5. A tenant is known only by its `TenantConfig` **name**. That name is copied
   into the `agentorca.io/tenant` label (runs only, for example
   `external_api.go:371`) and into Postgres `tenant` columns (approvals,
   feedback, routing events).
6. Every data query is scoped by **namespace**, for example
   `ApprovalQuery.Namespaces` (`internal/postgresql/approvals.go:161`). As a
   result:
   - Two tenants that list the same namespace see each other's agents, runs,
     approvals and feedback.
   - A tenant deleted and recreated with the same name inherits the old
     tenant's data, and old session JWTs still work.
   - `TenantConfig` is namespaced, so two tenants in different namespaces can
     share a name.
7. Agents, Tools, KnowledgeBases, MCPServers, Deployments and the pods,
   Secrets and PVCs that controllers create carry no tenant marker. Nothing
   stops an Agent from referencing another tenant's Tool, KnowledgeBase or
   Secret if it can name it.

---

## Design 1: tenant identity and isolation

### The tenant ID

- **Format:** `tnt_` plus a UUIDv7, for example `tnt_01935c7e-...`. The ID is
  opaque, never reused, and never derived from the name.
- **Generated when the tenant is created.** The operator's TenantConfig
  reconciler, or a mutating webhook on create, assigns it. It is recorded in
  two places:
  - **Postgres `tenants` registry** (source of truth): `id`, `name`,
    `k8s_namespace`, `k8s_uid`, `state` (active / suspended / deleted),
    `created_at`, `deleted_at`.
  - **`TenantConfig.status.tenantID`**, a mirror so controllers and kubectl
    can see it without a database round trip.
- **Optional pinning:** `spec.tenantID`. Engineers who want a deterministic ID
  in GitOps, or need disaster recovery, can set it. The webhook enforces that
  a pinned ID is well-formed, immutable once set, and not bound to another
  live TenantConfig.
- **Recreating a tenant never silently inherits data.** A TenantConfig
  recreated with the same name gets a **new** ID. To get the old data back,
  set `spec.tenantID` to the deleted tenant's ID. That is an explicit restore,
  and only a platform admin may do it: the webhook checks the requester's
  group. Use the same path to rebind after deleting a TenantConfig by mistake.
- **Rebuilding the registry:** if Postgres is lost (as on pnw-dev in October),
  the registry can be rebuilt from `TenantConfig.status.tenantID`,
  `spec.tenantID` and object labels. `aoctl admin tenants rebuild-registry`
  does this. Pinning `spec.tenantID` in Git is the recommended DR posture.

### Defense in depth

Each layer checks the tenant ID on its own, so one bug or misconfiguration does
not open access across tenants.

| Layer | Enforcement |
|---|---|
| **Identity** | Session, issued and federated JWTs carry a `tid` claim. Each request resolves `tid` against the registry: unknown, suspended or deleted tenants get `401`, which ends their sessions and blocks a same-name recreate from using old tokens. `TenantIdentity` gains `TenantID`. The name is for display only. |
| **API** | Every list, get, write and delete requires both conditions: the object's `agentorca.io/tenant-id` label matches `tid` (or the object is platform-shared, read-only), **and** its namespace is in `allowedNamespaces`. Failing either returns `404`, not `403`, so existence does not leak. |
| **Kubernetes objects** | Every tenant-owned object carries `agentorca.io/tenant-id`, including Agents, Tools, MCPServers, KnowledgeBases, Deployments, Workflows, AgentRuns, and the pods, Secrets, PVCs, ConfigMaps and Services that controllers create. API writes are stamped by the API. GitOps-applied objects get the label from their manifest, or from the namespace's tenant binding (below). Once set, the label is **immutable** (validating webhook). |
| **References** | The webhook and controllers reject any spec reference (Agent → Tool / MCPServer / KnowledgeBase / ModelSelector / Secret, Deployment → Agent, Workflow → Agent) whose target belongs to a different tenant. Only same-tenant targets or `agentorca.io/scope: platform` targets are allowed. This is the check that actually holds when namespaces are shared or misconfigured. |
| **Namespaces** | A namespace is claimed by at most one tenant by default: the webhook rejects a second TenantConfig listing it. Platform admins can mark a namespace `agentorca.io/namespace-mode: shared`. In that mode the label and reference checks above are the only boundary, and the UI says so on the tenant's settings page. Namespaces with one owning tenant get the label `agentorca.io/tenant-id`, which is also how GitOps-applied objects without the label are stamped. |
| **Network** | Generated NetworkPolicies select by `agentorca.io/tenant-id`. A tenant's agent pods can reach only its own MCP servers, tools and sandboxes, plus platform services. This extends `internal/security/network_policy.go`. |
| **Data stores** | Postgres: a `tenant_id` column on every tenant table (approvals, feedback, routing events, archived runs, audit, revisions, datasets), backfilled from the current name→tenant mapping. Every query filters on it, and **row-level security** policies keyed on `current_setting('agentorca.tenant_id')` act as the backstop. Redis: keys prefixed `t:{tid}:`. Checkpoints in S3/GCS: prefix `{tid}/`. Vector DB: a collection per tenant, or a mandatory payload filter, chosen per backend. Hindsight: a bank per tenant. |
| **Spend and limits** | Budgets, rate limits and concurrency are accounted by `tid`, not name, so two same-name tenants never share counters. |
| **Audit** | Every audit row records `tenant_id`, `user_id` and the action. Tenant admins can see their own audit trail in the UI. |

**Platform-shared resources** (ModelProviders, curated Templates, shared Tools)
carry `agentorca.io/scope: platform`. Tenants can reference and read them, but
only platform admins can change them.

**Migration:** assign IDs to existing TenantConfigs. Backfill the
`agentorca.io/tenant-id` label from the namespace bindings and the existing
`agentorca.io/tenant` label, and backfill `tenant_id` in Postgres. Run in
**audit mode** first: log violations without blocking. Then switch to enforce.
Release notes must spell out the namespace-exclusivity change, because some
setups share a namespace between tenants today.

---

## Design 2: one source of truth per resource (UI vs GitOps)

| Owner | How it's decided | UI behaviour | GitOps behaviour |
|---|---|---|---|
| **Git** | Detected from GitOps tracking metadata, or `agentorca.io/owner: git` | Read-only, with a "Managed in Git" badge naming the Argo app / Flux Kustomization / Helm release. Actions: **View YAML**, **Copy as YAML**, **Duplicate** (a UI-owned copy) | Unchanged |
| **UI** | Created through the UI API. Annotated `agentorca.io/owner: ui` | Editable, versioned, can be rolled back, can be exported | Writes from GitOps controllers are **denied** by the webhook |

### Detecting Git ownership

An object is Git-owned if any of these are present:

- Argo CD: the annotation `argocd.argoproj.io/tracking-id`, or the configured
  label tracking key (default `app.kubernetes.io/instance`).
- Flux: `kustomize.toolkit.fluxcd.io/name` and `/namespace`, or
  `helm.toolkit.fluxcd.io/name`.
- Helm: `app.kubernetes.io/managed-by: Helm` plus `meta.helm.sh/release-name`.
- An explicit `agentorca.io/owner: git`, for CI pipelines that run
  `kubectl apply`.

### Blocking collisions

Argo CD and Flux only prune objects they track, so they never delete UI-owned
objects. The real risk is a later commit with the same kind, namespace and name
taking the object over on the next sync. The validating webhook **denies**
writes to UI-owned objects from configured GitOps writers. These are listed in
Helm as `ownership.gitopsWriters`: service accounts (for example
`argocd-application-controller`, `kustomize-controller`, `helm-controller`) and
field managers. The denial message names the problem and the fix:

> `Agent support-triage` is owned by the agent-orca UI (tenant acme). Export it
> from the UI and mark it for handoff, or rename your manifest.

The webhook also records a Kubernetes Event on the object. The UI shows "A Git
sync tried to change this agent" on the agent page. Human `kubectl` edits to
UI-owned objects get an admission **warning**, not a denial, so operators can
still fix things by hand.

For the reverse case, the webhook also denies the UI API's own service account
when it writes Git-owned objects, as a backstop to the API check.

**This guarantee needs `failurePolicy: Fail`** on the ownership webhook. With
`Ignore`, which is how dev runs today, an unavailable webhook means GitOps wins
silently. The Helm chart sets the ownership webhook to `Fail` independently of
the other webhooks.

### Moving an agent from the UI to Git (manual, no write-back)

1. In the UI, a builder clicks **Export to Git**. They get a clean manifest:
   no status, managedFields, UI annotations, revision data or tenant-specific
   runtime fields. It has `agentorca.io/owner: git` and keeps
   `agentorca.io/tenant-id`. The live object is annotated
   `agentorca.io/handoff: pending` with an expiry (default 7 days).
2. An engineer commits the manifest to their own repository however they
   normally do.
3. While the handoff is pending, the webhook lets a GitOps writer adopt the
   object. When tracking metadata appears, ownership flips to Git and the UI's
   revision history is frozen read-only. If the handoff expires, it is
   cleared, and later GitOps writes are blocked again.

### Moving a Git-owned agent into the UI

Use **Duplicate**: it creates a UI-owned copy under a new name. Git-owned
objects can never be edited in place from the UI.

---

## Stages

### Stage U0: Tenant identity and isolation (backend, prerequisite)

Implements Design 1, in order:

- [ ] Tenant registry table plus `status.tenantID` / `spec.tenantID`
  (immutable, pinnable, restore restricted to admins). Webhook enforcement.
- [ ] `tid` in all JWT types. Resolve against the registry on each request
  (cached, invalidated when tenant state changes). Add
  `TenantIdentity.TenantID`.
- [ ] Namespace exclusivity, plus `namespace-mode: shared` as the exception.
- [ ] Stamp `agentorca.io/tenant-id` on API writes and controller-created
  children, keep it immutable, and backfill existing objects.
- [ ] Tenant check on cross-references, in the webhook and controllers.
- [ ] Filter every API surface by label **and** namespace: UI API, ACP,
  External Task, Admin.
- [ ] Postgres `tenant_id` columns, backfill, row-level security. Redis, object
  store, vector DB and Hindsight scoping.
- [ ] NetworkPolicies by tenant ID.
- [ ] Audit mode, then enforce. Update the `aoctl admin tenants` commands.
- [ ] Tests: a table test per layer; e2e with two tenants sharing a namespace,
  and a same-name recreate.

### Stage U1: Make the write path safe (backend)

- [ ] Delete the `managed-by` stamping in `updateResource` and the delete
  guard that relies on it. Use `agentorca.io/owner` instead, and migrate
  existing UI-created objects.
- [ ] Roles inside a tenant: `viewer`, `runner`, `builder`, `admin`, mapped
  from OIDC groups through `TenantConfig.spec.roles`.
  `TenantIdentity.Groups` is already populated.
- [ ] Server-side apply with field manager `agent-orca-ui`. Require
  `resourceVersion` and return `409` on conflict.
- [ ] Record the actor: annotate `agentorca.io/last-modified-by` and write an
  audit row with `tenant_id`.
- [ ] Update the UI OpenAPI (`internal/apiserver/schemas/openapi-ui.yaml`).

### Stage U2: Ownership and GitOps coexistence (backend)

- [ ] Ownership detection library, shared by the API and the webhook, with a
  table test for each GitOps signature.
- [ ] The ownership webhook: deny GitOps writers on UI-owned objects, deny the
  UI service account on Git-owned objects, handoff with expiry, Events, and
  `failurePolicy: Fail`.
- [ ] The API returns
  `ownership { owner, source: {tool, app}, editable, handoff }` for each
  object.
- [ ] The Export endpoint, which produces the clean manifest and starts the
  handoff.
- [ ] `docs/gitops-coexistence.md` with Argo CD and Flux walkthroughs.
- [ ] e2e test: Argo CD and Flux in kind, covering collision denied, handoff
  adopted, handoff expired, and no prune of UI objects.

### Stage U3: App shell and information architecture (UI)

- [ ] A router with a URL for every run, agent, revision and trace. Delete the
  `sessionStorage` navigation.
- [ ] A left sidebar grouped by task:
  - **Home**
  - **Agents**
  - **Runs** (Runs and History merged)
  - **Build**
  - **Evaluate**
  - **Operate** (fleet, costs, routing, status), builders and admins only
  - **Settings** (providers, model selectors, connections, members, audit),
    admins only
- [ ] User menu (identity, tenant, role), workspace switcher, theme toggle.
- [ ] Remove the Marketplace placeholder.
- [ ] Use business vocabulary: *published agent*, *Model: Auto*,
  *Connections*, *Knowledge*. CRD names appear only in the YAML view and
  Settings.
- [ ] First-run empty states. Setup banners for admins only, linking to the
  fix.
- [ ] Shared layout primitives instead of inline style maps per file.

Starts in parallel with U0 to U2, because it only reads.

### Stage U4: Catalog, templates and running agents (consumer path)

- [ ] Agent catalog with cards: what the agent does, example inputs, owner
  badge, cost per run.
- [ ] Run form generated from the manifest input schema, plus chat for
  conversational agents.
- [ ] **My work** inbox: live status, outputs, inline approvals and
  clarifications, feedback.
- [ ] **Templates**: Git-owned agents with `agentorca.io/scope: platform` and
  `agentorca.io/template: "true"`, curated by the platform team in Git. "Use
  template" duplicates one into the tenant's workspace as UI-owned. Ship 5–8
  templates built from the existing demos.

Needs U0 (tenant filtering) and U3.

### Stage U5: Agent builder (UI-owned create/edit)

- [ ] A guided builder: goal, instructions, model (Auto), connections,
  knowledge (upload/link), guardrails and budget, how the agent runs.
- [ ] Optional: draft the builder fields from a plain-English description, for
  the user to review.
- [ ] A test panel: run the draft against sample inputs and see the trace
  alongside it.
- [ ] Form ↔ YAML toggle. Validate with the CRD schema and a `dryRun=All`
  webhook check before saving.
- [ ] Save creates a revision. Publish creates or updates the AgentDeployment.
- [ ] Git-owned agents open read-only, with Copy YAML and Duplicate.
- [ ] **Backend gaps that each need a short spec first:**
  - Scheduled/cron triggers (none on Agent, AgentDeployment or AgentWorkflow).
  - A per-user OAuth "Connect" for MCPServers.

Needs U1 and U2.

### Stage U6: Versions, review, export

- [ ] A revision for every UI save (Postgres, `tenant_id`): list, diff, roll
  back.
- [ ] Optional "require review before publish" per workspace.
- [ ] The Export to Git flow and handoff status in the UI.
- [ ] Workflows: the visual DAG (exists today) plus step-by-step editing as a
  form.

### Stage U7: Evaluations

- [ ] "Add to dataset" from any run or trace. Datasets are scoped to each
  tenant.
- [ ] Graders: an LLM judge (Switchyard sample), exact/regex/JSON-schema
  checks, human review.
- [ ] Experiments comparing revisions or models on quality, cost and latency.
  Feed the results into routing benchmarks.
- [ ] Optional: a pass threshold on a dataset gates Publish.

### Stage U8: Fleet overview and operations

- [ ] One table with a row per agent: owner, health, runs, error rate, p95,
  spend vs budget, eval score, pending approvals.
- [ ] Cost dashboard, status page and routing insights become tabs under
  Operate.

### Stage U9: Agents where people already work (later)

- [ ] An embeddable chat widget, a Slack/Teams app, email and webhook triggers,
  each using the External Task API with tenant credentials.

## Dependency order

```
U0 (tenant ID) ── U1 (safe writes) ── U2 (ownership) ── U5 ── U6 ── U7
      │
      └──────────── U4 (catalog/run) ── U9
U3 (shell/IA) ─────┘                     U8 (after U3)
```

## Not planned

- Git write-back (PRs from the UI). Revisit only as an opt-in integration if a
  specific deployment needs it.
- Shared Git/UI field ownership.
- A warn-only collision mode.
