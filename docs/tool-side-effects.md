# Tool side effects

A Tool can say what a call to it changes, and a GuardrailPolicy can require an approver before
calls of that kind run. This is how you let an agent use a tool that deploys, pays or commands a
device only after a person agrees.

## Classify the tool

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: Tool
metadata: {name: deploy, namespace: team-a}
spec:
  sideEffect: actuate   # read | write | actuate
  # ...
```

| Class | Meaning | Examples |
|---|---|---|
| `read` | Changes nothing | search, fetch a file, query a database |
| `write` | Changes state that can be undone or is reviewed later | commit to a branch, edit a ticket |
| `actuate` | Acts on the world and is hard to take back | deploy, send a payment, command a device |
| *(unset)* | Unclassified | |

For MCP servers each MCP tool is its own Tool resource, so each gets its own class.

## Say what each class needs

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: GuardrailPolicy
metadata: {name: factory, namespace: team-a}
spec:
  toolApproval:
    write: none
    actuate: approve
    unclassified: approve   # be strict about tools nobody classified
    timeout: 30m
    approverGroups: [release-managers]
```

| Requirement | Effect |
|---|---|
| `none` | The call runs. |
| `approve` | The call waits for an approver (below). |
| `deny` | The call is refused at once, without asking anyone. |

Defaults when a field is unset: `actuate` is **`approve`**, everything else is `none`. So a tool
you mark `actuate` is gated even if the agent has no GuardrailPolicy at all. A sample is in
`config/samples/tool-side-effects.yaml`.

## What happens to a gated call

1. The model-router holds the call and asks the operator to open an approval. It appears in the
   **Approvals** tab (with the tool, its class and its arguments), triggers the webhook
   notification if one is configured, and the run's `ToolApprovalPending` condition turns True.
2. An approver in `approverGroups` (or anyone with access to the namespace, if none are listed)
   approves or rejects it.
3. **Approved:** the tool runs. **Rejected, expired (no decision within `timeout`), or the run
   ended:** the tool does not run, and the model receives an error saying so and telling it not
   to repeat the call. The run carries on, so the agent can choose another approach.

The wait does not count against the tool's own execution timeout. Every step is recorded as a
`toolApproval` event in the run's trace (under the **Tools** filter), and the decision, with who
made it, is kept in PostgreSQL.

## Things to know

- **It fails closed.** If the approval service cannot be reached, or PostgreSQL is not
  configured, a call that needs an approver does not run.
- **Who may decide comes from the policy, not from the request.** The router sends only the tool,
  its class and its arguments; approver groups and the timeout are read from the GuardrailPolicy by
  the operator, and the tool must be one the agent has.
- **Only operator-defined tools are gated.** Built-in tools (such as `_done`) and tools an agent
  process runs itself (`localTools`, like the coder agent's shell) are not; those are governed by the
  sandbox's filesystem and network policy.
- **An approval covers one call.** Each call needs its own.
