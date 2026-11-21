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
