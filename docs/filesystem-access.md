# Filesystem Access via the ACP Bridge (`fs/*` relay)

Agent pods can read and edit the user's **real local workspace** through the
same ACP channel editors like Zed already use. This is the "pod drives the
workstation" architecture ([issue #34](https://github.com/heddles/agent-orca/issues/34)).

## Architecture

ACP v1 defines agent→client filesystem methods (`fs/read_text_file`,
`fs/write_text_file`). Zed and other ACP clients already answer them — that is
how Claude Code and Gemini CLI edit a Zed workspace. agent-orca relays those
calls from a remote pod to the user's machine:

```
┌────────────────────────── workstation ──────────────────────────┐
│  Zed (ACP client)  ◄──fs/* over stdio JSON-RPC──►  aoctl acp    │
│                                                    serve bridge │
└──────────────────────────────────▲──────────────────────────────┘
                                   │ GET /runs/{id}/fs/pending
                                   │ POST /runs/{id}/fs/pending/{req}/response
┌──────────────────────────────────┴──────────────────────────────┐
│  agent-orca ACP API (port 8000)  ──►  fs relay (redis)          │
│  operator internal API (port 8082) ──► same relay               │
└─────────────────────────────────────────────▲───────────────────┘
                                              │ POST enqueue / GET take
                                     ┌────────┴─────────┐
                                     │ model-router     │ (agent pod)
                                     │ fs_read_text_file│
                                     │ fs_write_text_…  │
                                     └──────────────────┘
```

1. The **model-router** executes the built-in tools as normal tool calls
   inside the LLM loop. Instead of touching a file, it POSTs the ACP call to
   the operator's internal API and long-polls for the bridge's answer.
2. The **bridge** (`aoctl acp serve`), already running on the user's machine
   for the Zed session, polls `GET /runs/{run_id}/fs/pending` while a turn is
   active, validates the request, and forwards it to Zed over the stdio
   JSON-RPC channel Zed already speaks.
3. Zed answers, the bridge posts the response back, the router returns it as
   the tool result, and the run continues.

Edits stay live against the user's actual files: no workspace copies, no sync
drift, no volume mounts — and the editor remains the arbiter of access.

## Enabling it

### 1. Agent CRD policy

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: Agent
metadata:
  name: senior-programmer
  namespace: default
spec:
  # ...
  filesystem:
    mode: readWrite      # readOnly | readWrite (default readOnly)
    writeApproval: true  # ask the user before every write (default true)
```

- `nil` (unset) — **no filesystem tools at all** (default; fail closed).
- `mode: readOnly` — only `fs_read_text_file` is exposed. Write calls are
  refused inside the pod (defense in depth) and rejected again by the bridge.
- `mode: readWrite` — both tools are exposed; the bridge can still be told to
  require approval per write (below).
- `writeApproval` is advisory for the bridge: it is surfaced in the ACP agent
  manifest so `aoctl acp serve` can elicit an explicit user approval for every
  write. The workstation flag `--fs-require-approval` can force approvals on
  regardless of this knob; it can never turn the *agent's* requirement off.

A sample Agent CR lives in [`testdata/agents/filesystem.yaml`](../testdata/agents/filesystem.yaml).

### 2. Operator deployment

The relay is backed by the shared state store (Redis). `/acp/runs/{id}/fs/*`
and the internal-API fs endpoints return `501` when the operator runs without
one — with a clear error the tenant can act on. Chart values:
`redis.enabled=true` (the chart default) is all it takes.

### 3. Workstation bridge

```bash
aoctl acp setup --editor zed --agent senior-programmer
```

`aoctl acp serve` extra flags:

| Flag | Default | Meaning |
|---|---|---|
| `--fs-roots <dir>[,<dir>…]` | *(none)* | Extra directory trees the agent may touch, on top of the session's own workspace. Accepts `~`. |
| `--fs-require-approval` / `--fs-require-approval=false` | `true` | Force (or waive) the per-write approval elicitation. Waiving only works if the agent's own policy allows it. |
| `--fs-remember-approval` | `false` | After the first approved write, auto-allow subsequent writes in the same session without re-prompting. |

The bridge advertises exactly the capabilities the agent's manifest grants, so
a `readOnly` agent never leads the editor to believe it can write.

#### Approval-mode matrix

| You want | Agent CRD | Bridge flags |
|---|---|---|
| Approve every write (default) | `writeApproval: true` | `--fs-require-approval` (default) |
| Approve once, then auto-allow for the session | `writeApproval: true` | `--fs-require-approval --fs-remember-approval` |
| Never prompt (trusted agent / unattended) | `writeApproval: false` | `--fs-require-approval=false` |

Both knobs must agree to skip prompting: the bridge prompt fires when
`--fs-require-approval` is set **or** the agent policy asks for it, so either
side can always narrow (force prompts) but only both together can widen
(silence them). Prompts are also skipped entirely in readOnly mode — reads
never elicit.

## Security model

Three enforcement layers, each where the corresponding resource lives:

| Layer | Enforced by | Mechanism |
|---|---|---|
| Tool existence | model-router (in-cluster) | `Agent.spec.filesystem` decides which fs tools are injected; readOnly refuses writes before any traffic leaves the cluster |
| Workspace confinement | bridge (workstation) | every path is resolved against the session workspace (`session/new` `cwd`) + `--fs-roots`; lexical `..` escapes are rejected and the call never reaches the editor |
| Write approval | bridge (workstation) | ACP `elicitation/create` form per write ("Agent X wants to write N bytes to path") unless policy/flag waives it |
| Listing scope | bridge (workstation) | `fs_list_dir` is enumerated by the bridge itself (ACP v1 has no list method), confined to the same roots; names/sizes only, no content |

Additional properties:

- **Editor is the final arbiter.** The relay can never grant more than Zed is
  willing to do; the editor may still refuse (its own trust prompts, missing
  file, symlink policies). Symlink escape detection is delegated to the editor.
- **Run-scoped transport.** fs requests are keyed `<namespace>:<run>`; the
  bridge can only see requests of runs it created (tenant-owned). Pod-side
  enqueue is authenticated with the run's ServiceAccount (TokenReview, like
  every internal-API route) and scoped to the caller's namespace — the same
  trust model as the other internal-API actions (`clarify`, `done`, `handoff`).
  Relay entries self-expire.
- **Failure semantics.** If the bridge is disconnected the fs tool returns a
  structured error telling the agent to ask the user to reconnect
  (`aoctl acp serve`), rather than hanging; a request that expires unserved
  reports the same. Write approvals distinguish "the user declined"
  (`write declined by the user in the editor`) from "the approval prompt
  itself failed" (`write approval prompt failed: …`, e.g. an editor without
  elicitation support) — a failed prompt never proceeds unapproved, and an
  accepted form approves the write regardless of the form content shape.
- **No shell, no exec.** The surface is strictly list-dir, read-text and
  write-text; there is no command execution via `fs/*`.

## Tool reference (what the LLM sees)

- `fs_list_dir(path?)` → `{"path": "...", "entries": [{name, type, size}...]}` —
  lists a directory (defaults to the workspace root). ACP v1 defines no
  directory-listing method, so this one is answered **by the bridge itself**
  (confined to the same workspace roots, read-only metadata, no approval) —
  it is never forwarded to the editor.
- `fs_read_text_file(path, line?, limit?)` → `{"content": "..."}` — reads a
  text file from the workspace (paths may be workspace-relative or absolute).
- `fs_write_text_file(path, content)` → `{"written": true}` — creates or
  overwrites a file (only in `readWrite` mode). Writes are full-file
  replacements; the editor may surface them as a diff to the user.

## Observability

- fs tool calls appear in the run trace as `toolCall`/`toolResult` events
  (backendType `builtin`), like every other tool.
- The bridge logs rejected requests (unknown paths, declined approvals,
  policy mismatches) to stderr, visible in Zed's external-agent log panel.

## API surface (`docs/acp-api.md`)

| Endpoint | Caller | Purpose |
|---|---|---|
| `POST /agentrun/{ns}/{run}/fs` | model-router | enqueue an fs call |
| `GET /agentrun/{ns}/{run}/fs/{id}` | model-router | take the bridge's answer |
| `GET /runs/{run_id}/fs/pending` | bridge | list pending calls (long-poll) |
| `POST /runs/{run_id}/fs/pending/{id}/response` | bridge | answer one call |