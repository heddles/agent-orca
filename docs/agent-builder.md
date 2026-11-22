# Agent builder API

The endpoints behind the guided agent builder in the UI. All of them are
tenant-scoped (an object outside the caller's tenant is a 404), gated by the
roles in `docs/tenant-isolation.md`, and the writes are audited. The machine
readable description is `internal/apiserver/schemas/openapi-ui.yaml`.

| Endpoint | Role | Audited |
|---|---|---|
| `GET /api/builder/options` | viewer | no |
| `POST /api/resources/{kind}?dryRun=true`, `PUT /api/resources/{kind}/{ns}/{name}?dryRun=true` | as the real write | no (nothing is written) |
| `POST /api/templates/{ns}/{name}/use` | builder | yes, one row per object |
| `POST /api/builder/check` | builder | yes, one row per check |
| `POST /api/builder/draft` | builder | yes, one row (sizes and cost, never the text) |
| `GET /api/knowledge/{ns}/{name}/documents` | viewer | no |
| `PUT`, `DELETE /api/knowledge/{ns}/{name}/documents/{doc}` | builder | yes, one row each (file name and size, never the text) |
| `POST /api/agents/{ns}/{name}/publish` | builder | yes |
| `DELETE /api/agents/{ns}/{name}/publish` | builder | yes |

## Check a draft: dry run

Add `?dryRun=true` to a create or an update. The request goes through the same
code as a real write: tenant checks, roles, ownership rules, label stamping,
server-side apply and the validating and mutating webhooks, with the API
server's `dryRun=All`. Nothing is stored, no audit row is written and no
`last-modified-*` annotation lands anywhere.

```
200 {"valid": true, "object": { ...the object as it would be stored... }}
409 {"valid": false, "errors": ["Agent shared/bot already exists"]}
```

A failure has the status a real write would give (400, 403, 404, 409, 428, ...)
and `errors` lists the messages. A webhook denial is reduced to its own message.
A stale `resourceVersion` also returns `currentResourceVersion`.

## Check with my cases

`POST /api/builder/check` runs the draft that is on screen, saved or not, against a dataset of test cases
(`docs/evaluations.md`), so a person sees pass or fail, a score, the cost and the time of each case before they
save or publish. It builds nothing second: it queues an experiment of purpose `check` on the existing runner, graders
and budget caps.

```
{"agent": {...Agent object...}, "datasetId": 7, "budgetUsd": 0.25, "compareWithPublished": true}
201 {"valid": true, "experiment": {...}, "comparedWithPublished": true, "capped": false}
```

- **Nothing is saved or published.** The draft goes through the same dry-run write as above (CRD schema, webhooks,
  ownership, tenant stamping); a draft that fails answers like a failed dry run (`400 {"valid": false, "errors": [...]}`)
  and nothing is queued. A valid draft travels as the inline `spec` of one experiment arm (`draft`); the runner makes
  the usual hidden `eval-<id>-draft` Agent from it, which is deleted with the experiment's other objects. An Agent of
  that name is never created. A Git-owned agent is a 409, as it cannot be saved here; an agent in another tenant is a 404.
- **Against the published version.** When the agent has an AgentDeployment, the live agent runs as the first arm
  (`published`) and the draft as the second, so the paired summary says whether the draft is better or worse. Send
  `compareWithPublished: false` to skip it (it doubles the cost).
- **Caps, all enforced by the server** (`operator.builder.*`): the budget (default `$0.25`, at most `$2`; over it is a 400),
  the cases (the first 25 of a larger dataset, shown as `capped`), the runs in flight (2), the per-run budget (at most
  `$0.50`), and 12 checks per user an hour (`429` with `Retry-After`). The tenant's active-experiment cap and daily budget
  apply as for any experiment.
- **Visibility.** A check is not in `GET /api/evals/experiments`. It is read with the experiment endpoints by id
  (`/results`, `/summary`), by anyone with the viewer role in the tenant. It can never be the evidence of a publish gate.
- **Audit.** One row (`create`, `EvalExperiment`) with the dataset name, the number of arms and the budget. No case, draft
  or output text.
- Tools the draft uses are really called, as in any experiment, so a draft with a tool that changes things changes them
  once per case. Test it on a safe workspace.

## Describe what you want

`POST /api/builder/draft` with `{"description": "...", "namespace": "optional"}` writes a first draft of the form from plain
words, for a person to review. It never writes an object: the answer is only fields.

```
200 {"name": "refund-helper", "namespace": "team-a", "description": "...", "instructions": "...",
     "exampleInputs": ["..."], "connections": ["search", "tickets"], "knowledgeBases": ["policy"], "guardrail": "pii",
     "suggestedBudgetUsd": 0.4, "aiFields": ["connections", "description", ...], "notes": ["..."], "valid": true}
```

- **The model.** `operator.builder.draftProvider` names a ModelProvider. It is found the way an evaluation judge's is
  (`judge.KubeResolver`): in the caller's workspace first, then in the platform catalog, subject to the tenant access check, so
  a tenant that defines a provider of that name uses its own. The call goes through the same `router.CompleteOnce` path with
  the provider's own prices. There is no other provider code. With no provider set, or one the workspace cannot use, the
  answer is `409 {"code": "draft_unavailable", "error": "..."}` and `GET /api/builder/options` has
  `"draft": {"available": false, "reason": "..."}`; the UI then disables the box and shows the reason.
- **The reply is untrusted.** The description and the list of the workspace's tool, MCP server, knowledge base and guardrail
  names reach the model as data in blocks with a random nonce in their tags. Its reply must be one JSON object (one code
  fence allowed, prose around it is refused, as for a judge's verdict). Only the known fields are read, anything else (a
  `runtime`, an image, a namespace) is dropped; each field is type-checked, stripped of control characters and cut to its cap
  (instructions 6000 bytes, description 240, five examples of 200, twelve references). A tool, MCP server, knowledge base or
  guardrail policy survives only if the workspace has an object of that name that the caller may read (another tenant's, one in
  another workspace, or an invented one is dropped, with a note). Text goes through the evaluation redactor, so keys, tokens,
  e-mail addresses and card numbers do not reach the form. The name is made a DNS label and a number is added when it is
  taken. The agent the form would send is checked by the dry-run write; `valid` and `errors` are its verdict.
  The model never chooses the image, the model selector or the namespace.
- **Limits** (`operator.builder.draft*`, per user, in memory on each replica): the description is 10 to 2000 characters (413
  above); 10 drafts an hour (`429`, `Retry-After`); a call whose estimated cost, from the provider's prices, is over `$0.05` is
  refused before it is made; and `$1` a day (a call is let in while what was spent plus its estimate fits, so the allowance can
  be overrun by one call). The reply is capped at 1500 tokens.
- **Privacy.** The description is redacted before it is sent, and never logged or audited: logs and the audit row hold its
  size, the tenant, the user and the cost.

## Documents for knowledge

How knowledge is ingested today ([rag.md](rag.md)): a KnowledgeBase lists sources in `spec.ingestion` (ConfigMaps, URLs, S3, MCP
servers); an `mcp-ingester` Job reads them, extracts text (PDF, Office, HTML and plain text), chunks, embeds and upserts into the
knowledge base's own Qdrant. The platform has no object store of its own (S3 is only a source an administrator points at a
bucket they run) and the ingester has no upload endpoint, so a browser upload has nowhere to go without new infrastructure.

The smallest safe path that needs none is a **ConfigMap source**: the Job reads the string data keys of the ConfigMaps in
`spec.ingestion.configMapRefs`, and the controller watches those ConfigMaps, so a change re-ingests. A document added here is a
key of one ConfigMap per knowledge base, `<kb>-docs`.

- `PUT /api/knowledge/{ns}/{name}/documents/{file}` with the document as the body adds or replaces it; `DELETE` removes it;
  `GET .../documents` lists names and sizes. The first document creates the ConfigMap (UI-owned, stamped with the tenant ID,
  labelled `agentorca.io/knowledge-docs: <kb>`) and adds it to the knowledge base's `configMapRefs`; nothing else on the
  KnowledgeBase changes. A ConfigMap of that name that is not ours is never touched (409).
- **Ownership.** Only a UI-owned KnowledgeBase of the caller's tenant: Git-owned (or owned by nobody the UI knows) is a 409,
  platform-shared a 403, another tenant's a 404, and one with a handoff to Git pending a 409. The list says
  `editable: false` and why, so the UI shows no upload controls.
- **Types and limits** (`operator.builder.knowledge*`): `.txt`, `.md`, `.markdown` and `.csv`, UTF-8 text without NUL bytes
  (415 otherwise). The file name is letters, digits, dots, dashes and underscores, at most 100 characters. At most 256 KiB a
  document, 50 documents and 900 KiB in all per knowledge base: a ConfigMap is an etcd object of at most 1 MiB, and the total
  is capped below that whatever is configured. **PDF and Office files are not supported**: the ingester reads a ConfigMap's
  string data only, so binary files cannot be carried; an administrator can add an S3 or URL source for those.
- **Untrusted, opaque content.** The text is stored as it came, never parsed or sanitised by this API, never returned by any
  endpoint (the list has names and sizes) and never rendered by the UI. It is not in logs or audit rows: a row says
  `added document returns.md (2048 bytes) in knowledge base handbook`. Nothing scans for viruses or for sensitive data: text
  files cannot carry executable content the platform runs, but what a document says can be quoted in an agent's answers, so
  the UI tells people not to add what the agent's users should not see. A deployment that needs scanning puts it in front
  of the API.
- **Removing does not forget.** Ingestion upserts. Removing or replacing a document stops it being read next time, but the
  chunks already embedded stay in the knowledge base's vector store until it is rebuilt. The answers say so, and so does the UI.

**What real file upload (PDF, Office, large files) would need:** an object store the platform owns (or a presigned-upload flow
into the S3 source an administrator configures) with per-tenant prefixes and quotas; the API issuing short-lived upload URLs
and recording each object; the ingester reading those prefixes (it already reads S3); malware scanning of binaries before
ingestion; and deletion of a removed document's chunks from Qdrant by document ID.

## Use a template

`POST /api/templates/{ns}/{name}/use` with `{"name": "my-agent", "namespace": "optional"}`.

The source is an Agent labelled `agentorca.io/template: "true"` and
`agentorca.io/scope: platform` in a platform namespace. The agent and everything
that shares its `agentorca.io/template-group` label (the AgentDeployment, Tools
and GuardrailPolicy) are copied into the caller's workspace: the `namespace`
from the body when the tenant may use it, otherwise the tenant's first allowed
namespace.

- The agent gets exactly the requested name (a DNS label of at most 40
  characters). The others get a suffix: `-deploy`, `-tool`, `-guardrail`, with a
  number when a group has several.
- References between the copies are rewritten (the agent's `tools` and
  `guardrailPolicyRef`, the deployment's `agentRef`, a tool's `agentRef`). References
  to anything else, such as a shared ModelSelector, are kept.
- The copies are UI-owned and stamped with the caller's tenant ID. The template,
  scope and template-group labels and the owner, handoff and last-modified
  annotations are removed. `agentorca.io/maintainer`, the description and the
  example inputs are kept.
- The copies are created with the atomic create path of the UI write; if one
  fails, the ones already created are deleted. An existing name is a 409.

**The target namespace needs what the template names.** References are
same-namespace names, so a copy works only if the workspace has the template's
ModelSelector (every sample template uses `default`) and any KnowledgeBase,
GuardrailPolicy or Tool the template names outside its group. Before creating
anything the API checks each reference of every copy: it must be another copy,
or an object in the target namespace that the caller may read (their own
tenant's, or platform-scoped and visible there). If not, nothing is created and
the answer is

```
422 {"message": "This template needs 1 thing that is not set up in workspace team-a.",
      "errors": ["ModelSelector \"default\" does not exist in namespace team-a: ask an administrator to add it, or use one of its own (team-sel) by sending {\"replace\":{\"ModelSelector/default\":\"team-sel\"}}"],
      "missing": [{"kind": "ModelSelector", "name": "default", "namespace": "team-a",
                   "alternatives": ["team-sel"], "replaceKey": "ModelSelector/default"}]}
```

`errors` has one plain-language message per missing reference. `missing` is the
same as data for a picker: `alternatives` are the sorted names of readable
objects of that kind in the namespace (`[]` when there are none) and
`replaceKey` is the exact key to send in `replace`.

To use a different object, add `replace` to the body, keyed `Kind/name`:
`{"name": "my-agent", "replace": {"ModelSelector/default": "team-sel"}}`. It is
applied to the copies before the check, so the replacement must exist too.

```
201 {"agent": {...}, "created": [{"kind": "Tool", "namespace": "team-a", "name": "my-agent-tool"}, ...]}
```

## Publish

`POST /api/agents/{ns}/{name}/publish` with an optional `{"name": "...", "chat": true}`.

The agent must be UI-owned and in the caller's tenant (a Git-owned agent is a
409, a platform-shared one a 403). The call creates the agent's AgentDeployment
(`201`), or updates the one that already runs it (`200`):

```
{"deployment": {...}}
```

The deployment is minimal: `agentRef`, chat input and one replica; everything
else comes from the agent and its model selector. It is labelled and annotated like any other
UI-created object. `name` defaults to the agent's existing deployment, else the
agent's name. A Git-owned existing deployment is a 409.

`DELETE /api/agents/{ns}/{name}/publish` unpublishes by deleting that deployment
(`?deployment=` is needed only when several run the agent; without it that is a 409). It has the same ownership
guard as `DELETE /api/deployments/{ns}/{name}`: a deployment the UI does not own
is a 403.

## Options for the form

`GET /api/builder/options` lists what an Agent can reference, from the caller's
namespaces plus platform-scoped objects:

```
{"modelSelectors": [{"name": "default", "namespace": "agent-orca-system", "description": "..."}],
 "tools": [...], "mcpServers": [...], "knowledgeBases": [...], "guardrailPolicies": [...]}
```

`description` is the object's description where it has one (a knowledge base,
a tool's schema description) or its `agentorca.io/description` annotation.

## Revision history

Every UI save keeps a revision that can be listed, read and restored; see
[revisions.md](revisions.md).
