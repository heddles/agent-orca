# GitOps and the UI: one writer per object

The UI and GitOps tools (Argo CD, Flux, Helm) can both write agent-orca resources. They never share one object. Every object has one owner, and the other side is blocked from changing it.

| Owner | How it is decided | UI | GitOps |
|---|---|---|---|
| **Git** | Tracking metadata from a GitOps tool, or `agentorca.io/owner: git` | Read-only. View YAML, copy YAML, **Duplicate** | Unchanged |
| **UI** | `agentorca.io/owner: ui`, set when an object is created through the UI API | Editable, deletable, exportable | **Denied** by the ownership webhook |
| **Unknown** | Neither (made by `kubectl`, say) | Read-only, **Duplicate** | Unchanged: GitOps may take it over |

There is no warn-only mode, no Git write-back (the UI never opens a pull request or edits a repository), and no shared-field mode (Git owning some fields while the UI edits others). Moving work between the two is an export or a duplicate, done by a person.

The kinds covered are the ones the UI can write: `Agent`, `Tool`, `MCPServer`, `KnowledgeBase`, `ModelSelector`, `ModelProvider`, `AgentDeployment` and `AgentWorkflow`. `AgentRun` and the rest are not.

## How ownership is detected

An object is **Git-owned** if any of these is present. They are checked in this order, and the first match gives the tool and application shown in the UI:

| Signature | Tool | Application shown |
|---|---|---|
| Annotation `argocd.argoproj.io/tracking-id` (Argo CD's default tracking method) | `argocd` | the Application name, from the tracking ID |
| Labels `kustomize.toolkit.fluxcd.io/name` and `kustomize.toolkit.fluxcd.io/namespace` | `flux-kustomize` | `<namespace>/<name>` of the Kustomization |
| Label `helm.toolkit.fluxcd.io/name` | `flux-helm` | the HelmRelease |
| Label `app.kubernetes.io/managed-by: Helm` and annotation `meta.helm.sh/release-name` | `helm` | the release name |
| Argo CD **label tracking**, only when the operator is configured for it (below) | `argocd` | the label value |
| Annotation `agentorca.io/owner: git` | `git` | none. For CI pipelines that run `kubectl apply` |

Git tracking metadata wins over `agentorca.io/owner: ui`. An object a GitOps tool tracks is Git's, whatever else it says.

An object with `agentorca.io/owner: ui` and none of the above is **UI-owned**. An object with neither is **unknown**, and the UI treats it as not editable.

**The Argo CD tracking label is off by default.** If Argo CD is set to `application.resourceTrackingMethod: label` it marks resources with `app.kubernetes.io/instance`, a label Helm and many charts also set. Treating it as Argo by default would mark every Helm-installed object as Git-owned for the wrong reason. If you do track by label, tell the operator:

```yaml
ownership:
  argoLabelTracking: true
  argoTrackingLabel: app.kubernetes.io/instance   # or the key you set in argocd-cm (application.instanceLabelKey)
```

Every object the UI API returns carries the result:

```json
"ownership": {
  "owner": "git", "tool": "argocd", "app": "shop",
  "editable": false,
  "handoff": { "pending": false }
}
```

`editable` is true only for a UI-owned object of the caller's tenant when the caller is a builder (an admin, for `Tool`, `MCPServer`, `ModelProvider` and `ModelSelector`).

## Who counts as a GitOps writer

The webhook identifies the writer from the admission request, in this order:

1. **A configured GitOps writer.** The request's user is one of the configured service accounts, **or** the request's field manager is one of the configured field managers (`AdmissionRequest.Options.fieldManager`, set for create, update and patch). Either is enough.
2. **The operator.** The operator's service account (from `POD_NAMESPACE` and `POD_SERVICE_ACCOUNT`) with any field manager except `agent-orca-ui`. Status writes, finalizers, the tenant label stamping, the owner migration and the handoff controller are all this. They are never blocked.
3. **The UI API.** The field manager `agent-orca-ui`. The UI API writes as the operator's service account, so the field manager tells it apart from a controller.
4. **Anyone else**, a human with `kubectl`.

Defaults (Helm `ownership.gitopsWriters`):

| | Defaults |
|---|---|
| Service accounts | `argocd:argocd-application-controller`, `flux-system:kustomize-controller`, `flux-system:helm-controller` |
| Field managers | `argocd-controller`, `kustomize-controller`, `helm-controller` |

## The rules

**The guarantee does not depend on the names you list.** Argo CD and Flux stamp their tracking metadata (annotation, labels, Helm metadata) on every object they apply. So the webhook also looks at the incoming object: an update that gives a UI-owned object Git tracking metadata, or `agentorca.io/owner: git`, is denied no matter who sends it, unless a handoff is pending. This holds for a renamed service account or a custom field manager that is not in `ownership.gitopsWriters`. Configuring `gitopsWriters` is still recommended: it gives a precise message, and it also blocks a writer whose manifests carry no tracking metadata. A human who adds `owner: git` by hand is told to use Export in the UI, which sets the handoff. The operator's own service account is never blocked.

| Writer | Object | Result |
|---|---|---|
| GitOps writer | UI-owned, no handoff | **Denied**, an Event on the object |
| GitOps writer | UI-owned, handoff pending and unexpired | Allowed (adoption) |
| GitOps writer | Git-owned or unknown | Allowed |
| Operator or controller | any | Allowed |
| UI field manager (`agent-orca-ui`) | UI-owned or unknown | Allowed (the API has its own check) |
| UI field manager | Git-owned, or a write that would add Git tracking | **Denied**, a backstop to the API check |
| Human | UI-owned | Allowed, with an admission **warning** |
| Human | anything else | Allowed |

Humans are not blocked on purpose: an operator must still be able to fix a UI-owned object by hand in an emergency. The warning says the next UI edit may overwrite what they set.

A CI job that runs `kubectl apply` is a human to the webhook, so it gets the warning and is not blocked. To block it, add its service account to `ownership.gitopsWriters.serviceAccounts`.

## A blocked sync

Someone commits a manifest whose kind, namespace and name match an agent that was created in the UI. The tool's next sync tries to update the live object, and the webhook denies it.

The message is:

```
Agent "support-triage" in namespace "team-a" is owned by the agent-orca UI (tenant acme).
A write from service account system:serviceaccount:argocd:argocd-application-controller was blocked:
export it from the UI and mark it for handoff, or rename your manifest.
```

**Argo CD.** The Application shows `SyncFailed`, with the resource `Agent/support-triage` out of sync. `argocd app get shop` and the sync result show:

```
one or more objects failed to apply, reason: admission webhook "vownership.kb.io" denied the request: Agent "support-triage" in namespace "team-a" is owned by the agent-orca UI (tenant acme). ...
```

**Flux.** The Kustomization (or HelmRelease) is not ready. `flux get kustomizations` and `kubectl describe kustomization apps -n flux-system` show:

```
Agent/team-a/support-triage dry-run failed: admission webhook "vownership.kb.io" denied the request: Agent "support-triage" in namespace "team-a" is owned by the agent-orca UI (tenant acme). ...
```

Flux applies a Kustomization as one unit, so one blocked object fails the whole Kustomization until it is resolved. Argo CD syncs the other resources and reports this one.

**The Event.** The webhook also records a `GitOpsWriteBlocked` Event on the object, so people who look at the object, not at the Git tool, see it:

```
kubectl get events -n team-a --field-selector involvedObject.name=support-triage
```

### Resolving it

Pick one:

1. **Rename your manifest.** If the UI object and the Git object are two different agents, give the one in Git another name. Nothing else changes.
2. **Hand the object over to Git.** The agent should now live in Git. Use the export and handoff flow below.
3. **Delete the UI object**, if it is obsolete. A builder can delete it in the UI, and the next sync then creates the Git version.

## Moving an agent from the UI to Git

There is no write-back. A person commits the manifest.

1. **Export in the UI.** A builder clicks **Export to Git** on the agent. The API (`POST /api/resources/agents/team-a/support-triage/export`) returns a clean manifest:
   - no `status`, `managedFields`, `resourceVersion`, `uid`, `creationTimestamp`, `generation` or finalizers;
   - none of the owner, last-modified-by/at and handoff annotations;
   - `agentorca.io/owner: git` set;
   - the `agentorca.io/tenant-id` label kept, and `agentorca.io/maintainer` and your own labels and annotations.

   The live object is annotated `agentorca.io/handoff: pending` with `agentorca.io/handoff-expires`, by default 7 days from now (Helm `ownership.handoffTTL`). Pass `?handoff=false` to export without opening a handoff, for example to just look at the YAML.

2. **Commit it** to the repository your tool watches, in the path the Argo CD Application or Flux Kustomization covers. This step is yours: the UI never touches the repository.

3. **Let the tool sync.**
   - *Argo CD:* the sync updates the existing object and Argo CD adds its `argocd.argoproj.io/tracking-id` annotation.
   - *Flux:* the Kustomization applies the manifest and adds the `kustomize.toolkit.fluxcd.io/name` and `/namespace` labels.

   While the handoff is pending and unexpired, the webhook lets that GitOps writer through. The webhook does not change ownership.

4. **Ownership flips.** The handoff controller sees the Git tracking metadata, removes `agentorca.io/owner: ui` (a `git` value from the manifest stays) and both handoff annotations, and records a `HandoffCompleted` Event. From then on the UI shows the agent as Git-owned and read-only, and the UI API's own writes are denied.

If the sync does not happen before `handoff-expires`, the controller clears the handoff annotations and records a `HandoffExpired` Event. The object stays UI-owned and GitOps writes are blocked again. Export again to start a new handoff. A builder can also cancel a pending handoff with `DELETE /api/resources/{kind}/{namespace}/{name}/handoff`. Editing the object in the UI while a handoff is pending keeps the handoff.

Check where it stands:

```
kubectl get agent support-triage -n team-a -o jsonpath='{.metadata.annotations}'
kubectl get events -n team-a --field-selector involvedObject.name=support-triage
```

## Moving an agent from Git to the UI

Git-owned objects are never edited in place from the UI. Use **Duplicate** (`POST /api/resources/{kind}/{namespace}/{name}/duplicate` with `{"name": "support-triage-copy"}`). It creates a UI-owned copy under the new name in your workspace namespace, stamped with your tenant, and drops the source's Git tracking, owner and handoff metadata. The source is untouched. Duplicate also works on unowned and platform-shared objects (templates).

## `failurePolicy: Fail`, and what happens if the webhook is down

The guarantee above needs the ownership webhook to be reachable. The chart sets its `failurePolicy` to `Fail`, separately from the other webhooks (it ignores `webhook.failurePolicy`), and `config/webhook` does the same.

With `Ignore`, an unavailable webhook would let a GitOps sync take over a UI-owned object without anyone noticing. With `Fail`, while the operator is down:

- creates and updates of the eight covered kinds are rejected by the API server for everyone, GitOps tools and humans alike, with `failed calling webhook "vownership.kb.io"`;
- Argo CD and Flux report the sync as failed and retry, so they catch up on their own once the operator is back;
- reads, deletes and status updates are not affected.

If you deploy the operator and its agent resources with the same GitOps tool, order them so the operator is up first (an Argo CD sync wave, or a Flux `dependsOn`). A development install that must run without the webhook can delete or edit the `ValidatingWebhookConfiguration` entry `vownership.kb.io`; ownership is then not enforced.

## Non-default installs: configuring writer identities

The defaults assume Argo CD in `argocd` and Flux in `flux-system`. If yours run elsewhere, or under other names, a collision from them will not be blocked until you tell the operator who they are.

1. **Find the service account.** For Argo CD it is the application controller's:

   ```
   kubectl get pods -n <argocd namespace> -l app.kubernetes.io/name=argocd-application-controller -o jsonpath='{.items[0].spec.serviceAccountName}'
   ```

   For Flux, the `kustomize-controller` and `helm-controller` Deployments in the namespace you bootstrapped into. If a Flux Kustomization or HelmRelease uses `spec.serviceAccountName` to impersonate a tenant account, that account is the writer: list it too.

2. **Set the Helm values.** A list replaces the default, so keep the tools you still use. A service account is `namespace:name` or the full `system:serviceaccount:<namespace>:<name>`:

   ```yaml
   ownership:
     gitopsWriters:
       serviceAccounts:
         - gitops:argo-application-controller
         - flux-prod:kustomize-controller
         - flux-prod:helm-controller
         - ci:deployer                      # a pipeline that runs kubectl apply
       fieldManagers:
         - argocd-controller
         - kustomize-controller
         - helm-controller
         - my-deploy-tool
   ```

   They reach the operator as `OWNERSHIP_GITOPS_SERVICE_ACCOUNTS` and `OWNERSHIP_GITOPS_FIELD_MANAGERS` (comma-separated).

3. **Verify.** Create an agent in the UI, then apply a manifest with the same name from the tool. The apply should be denied. To see the identity the API server gives a request, add `--v=8` to `kubectl` or check the API server audit log for the `user.username` and the field manager of the sync.

Field managers matter when a tool applies through a shared service account (for example, Argo CD in a multi-cluster setup with a per-cluster account): the field manager still gives it away. Argo CD's is `argocd-controller` for both client-side and server-side apply.

## Helm values

| Value | Default | Meaning |
|---|---|---|
| `ownership.gitopsWriters.serviceAccounts` | Argo CD, Flux kustomize and helm controllers | Service accounts whose writes to UI-owned objects are denied |
| `ownership.gitopsWriters.fieldManagers` | `argocd-controller`, `kustomize-controller`, `helm-controller` | Field managers whose writes are denied |
| `ownership.argoLabelTracking` | `false` | Treat `ownership.argoTrackingLabel` as Argo CD tracking |
| `ownership.argoTrackingLabel` | `app.kubernetes.io/instance` | The label key Argo CD tracks by |
| `ownership.handoffTTL` | `168h` | How long an export waits for Git to adopt the object |

## Annotations

| Annotation | Set by | Meaning |
|---|---|---|
| `agentorca.io/owner` | UI API (`ui`), export (`git`) | The owner. A caller cannot set it through the UI API |
| `agentorca.io/last-modified-by`, `/last-modified-at` | UI API | The last UI write |
| `agentorca.io/handoff` | export | `pending` while Git may adopt the object |
| `agentorca.io/handoff-expires` | export | RFC 3339 expiry of the handoff |
| `agentorca.io/maintainer` | the catalog (the object's maintainer) | Kept on export |

## Testing it

`internal/controller/ownership_envtest_test.go` runs the real webhook and handoff controller against an API server and impersonates the Argo CD and Flux service accounts with their field managers: a collision is denied, a handoff is adopted and flipped, a handoff expires, a human edit warns, a UI write to a Git-owned object is denied.

There is no kind-based end-to-end test with Argo CD and Flux installed yet.
