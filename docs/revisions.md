# Revision history

Every object the UI API writes (agents, tools, MCP servers, model providers,
model selectors, knowledge bases, deployments, workflows) keeps a revision
history in PostgreSQL, so people can see what changed, read an earlier state and
restore it. The table is `revisions` (migration 0012), tenant-scoped with
row-level security like `audit_events`: a tenant sees only its own rows.

Without PostgreSQL there is no history. The endpoints answer `501
{"error":"revision history needs PostgreSQL"}` and writes work as before.

## What is recorded

After each successful UI create, update, restore and delete, including the
builder's use-template, publish, unpublish and duplicate writes, one revision
stores the object as it was after the write (as it was before, for a delete). It
leaves out status, managedFields, resourceVersion, uid, generation,
creationTimestamp and finalizers, and the owner, actor, handoff and
last-applied annotations, the same as an export. The tenant ID label stays.

- Dry runs record nothing.
- A save whose content equals the previous revision's adds nothing, so repeated
  saves do not flood the history. A delete is always recorded.
- Revisions are numbered per object (tenant, kind, namespace, name). Writers of
  one object take turns on a database lock, so numbers never repeat.
- An object is capped at 2 MB, the body limit of the API.
- The newest `revisions.keep` revisions per object are kept (Helm value, env
  `REVISIONS_KEEP`, default 50). Pruning runs in the transaction of the insert and
  never removes the revision the live object's content equals.
- Exporting to Git or cancelling a handoff changes only annotations that a revision
  leaves out, so it adds no revision.

## Failure policy

The same as the audit trail. The revision is written after the Kubernetes write,
tried three times, and if it still fails the request succeeds with the header
`X-Revision-Status: failed`, one log line records the failure, and the counter
`agentorca_revision_write_failures_total` goes up.

The log line carries the tenant ID, kind, namespace, name, action, user,
resourceVersion, attempts, the snapshot's size in bytes and its SHA-256. It
includes the snapshot itself only when it is at most 8 KB; a larger one is
reported as omitted, so a line never breaks log shipping or copies a whole agent
definition into the logs. For a large object the recovery is the Kubernetes
object itself, which is the source of truth, and the next successful write of the
object records a full revision.

## Endpoints

```
GET  /api/resources/{kind}/{ns}/{name}/revisions?limit=&before=
GET  /api/resources/{kind}/{ns}/{name}/revisions/{n}
POST /api/resources/{kind}/{ns}/{name}/revisions/{n}/restore    {"resourceVersion": "..."}
```

The list returns `{"revisions": [...], "next": "<cursor or empty>"}`, newest
first (`limit` 1 to 200, default 50; pass `next` as `before`). A revision is

```
{"revision": 3, "action": "update", "userId": "ada", "createdAt": "...",
 "summary": "...", "resourceVersion": "123", "restoredFrom": 2}
```

(`restoredFrom` only for a restore). The detail adds `"object": {...}`.

Access: list and detail need the viewer role and a live object the caller can
read. For a deleted object the caller must be a builder, and sees the history of
their own tenant only.

Restore needs the builder role, or the kind's write role (admin for tools, MCP
servers, model providers and model selectors). It applies revision n through the
same server-side apply, webhooks and audit as an update, records a revision with
action `restore` and `restoredFrom` n, and answers 200 with the object.

- `resourceVersion` is optional; a different one from the live object's is a 409.
- A Git-owned object cannot be restored (409, the same message as other UI writes).
- If the object was deleted and the name is free, restore creates it again with
  the atomic create. If the name was taken meanwhile, that is a 409.
- `?dryRun=true` validates without writing.
