# MCP Server Access Control

MCP servers in agent-orca have two complementary security layers:

| Layer | Where enforced | Mechanism |
|---|---|---|
| **Authorization** | Operator, at AgentRun creation time | `MCPServer.spec.allowedAgents` |
| **Identity verification** | MCP server, at request time | Kubernetes SA JWT via TokenReview |

Both layers work together: the operator prevents unauthorized runs from starting, and the MCP server can independently verify every inbound request came from a legitimate agent pod.

---

## Auto tool discovery & name-based filtering

`spec.discoverability` controls whether the model-router auto-discovers every tool
the MCP server exposes (via `tools/list` at runtime) and surfaces them to the LLM.

- **`enabled` (default)** — all tools are discovered automatically. `spec.tools` is
  **optional**: when omitted, the controller creates a single connector `Tool` CR named
  after the MCPServer, and the agent references that one name in `spec.tools` instead of
  enumerating every tool. Declared `spec.tools` entries are still honored when present.
- **`disabled`** — opt back into the explicit model: `spec.tools` is **required** (≥1
  entry), each becoming its own `Tool` CR, and the agent references each one by name.

Name-based filtering narrows which *discovered* tools the LLM can actually call, without
listing them. These are ordinary glob patterns (the same convention used by
[KnowledgeBase ingestion](https://kubernetes.io/docs/concepts/cluster-administration/networking/#kubectl)):

- `includePatterns` — when non-empty, only tools whose name matches at least one pattern
  are exposed (`*` and `?` wildcards; `**` for recursive matching).
- `excludePatterns` — tools matching any pattern are always dropped, even if they matched
  an include pattern (exclude wins).
- Empty (the default) ⇒ all discovered tools are exposed (backward compatible).

These mirror the `includePatterns`/`excludePatterns` glob convention already used by
[KnowledgeBase ingestion](mcp-apps.md) and the `mcp-ingester`.

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: MCPServer
metadata:
  name: github-mcp
  namespace: default
spec:
  transport: http
  url: http://github-mcp-svc.default:8080
  allowedAgents:
    - analyst
  # Tools are auto-discovered — no spec.tools needed. The agent references
  # "github-mcp" (the connector Tool CR) in agent.spec.tools.
  includePatterns:
    - "list_*"
    - "get_*"
  excludePatterns:
    - "internal_*"
```

> Discovery happens at runtime in the model-router sidecar, which connects to every MCP
> server referenced by the agent and merges the discovered schemas into the LLM's tool
> set. Filters are applied at that point. The connector `Tool` CR exists only so the
> agent (and the podbuilder's stdio sidecar image-volume wiring, which is keyed on Tool CR
> names) can reference the server by name.

---

## Authorization — `allowedAgents`

Access to an MCP server is **denied by default**. An agent must be explicitly listed in `MCPServer.spec.allowedAgents` for any AgentRun that uses it to be allowed to start.

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: MCPServer
metadata:
  name: code-tools
  namespace: default
spec:
  transport: http
  url: http://code-tools-svc:8080
  allowedAgents:
    - analyst
    - report-builder
  tools:
    - name: run-query
      description: Execute a SQL query
```

### Enforcement

When an AgentRun transitions from `Pending` to `Running`, the controller resolves every MCP tool the agent declares. For each tool that is owned by an `MCPServer` (identified by the `agentorca.io/mcpserver` label on the child Tool CR), it checks whether the agent name appears in `MCPServer.spec.allowedAgents`.

If the check fails, the run transitions immediately to `Failed` with a reason like:

```
agent "data-pipeline" is not in the allowedAgents list for MCPServer "code-tools"
```

The pod is never scheduled. No credentials, network access, or compute are allocated.

### What counts as "an agent"

The name in `allowedAgents` must match `Agent.metadata.name` in the same namespace. Cross-namespace access is not supported — each namespace is an independent trust boundary.

### Granting access

Edit the MCPServer and add the agent name:

```bash
kubectl patch mcpserver code-tools --type=json \
  -p='[{"op":"add","path":"/spec/allowedAgents/-","value":"new-agent"}]'
```

Or apply a full update:

```yaml
spec:
  allowedAgents:
    - analyst
    - report-builder
    - new-agent
```

Changes take effect on the next AgentRun — running pods are not affected.

---

## Identity Verification — SA JWT

For HTTP and SSE transport MCP servers, the model-router sidecar sends a short-lived Kubernetes ServiceAccount JWT on every request. This lets the MCP server cryptographically verify the caller's identity without trusting the network.

### How it works

The operator projects a second SA token into each agent pod at creation time:

```yaml
volumes:
  - name: agentorca-mcp-token
    projected:
      sources:
        - serviceAccountToken:
            audience: agentorca/mcp
            expirationSeconds: 900
            path: token
```

This token is mounted exclusively in the model-router sidecar at `/var/run/secrets/agentorca-mcp/token`. The kubelet rotates it automatically before it expires.

On every HTTP/SSE request to an MCP server, the model-router reads the current token from disk and adds:

```
X-Agentorc-Identity: Bearer <sa-jwt>
```

This header is separate from any service-level auth credentials configured in `MCPConfig.auth` (bearer tokens, API keys, etc.) — both can coexist.

### Validating the token in an MCP server

An MCP server that wants to verify caller identity makes a TokenReview call to the Kubernetes API:

```http
POST /apis/authentication.k8s.io/v1/tokenreviews
Authorization: Bearer <mcp-server's own SA token>

{
  "apiVersion": "authentication.k8s.io/v1",
  "kind": "TokenReview",
  "spec": {
    "token": "<value from X-Agentorc-Identity, strip 'Bearer ' prefix>",
    "audiences": ["agentorca/mcp"]
  }
}
```

A successful response confirms:

| Field | Expected value |
|---|---|
| `status.authenticated` | `true` |
| `status.audiences` | `["agentorca/mcp"]` |
| `status.user.username` | `system:serviceaccount:<namespace>:agentorca-agent-<agentName>` |

The username encodes the agent identity. If you want to double-check the specific agent, parse `agentorca-agent-<agentName>` from the username and cross-reference against your own allowlist.

For the TokenReview call to work, the MCP server's ServiceAccount needs permission to create TokenReviews:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: code-tools-token-reviewer
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:auth-delegator
subjects:
  - kind: ServiceAccount
    name: code-tools
    namespace: default
```

### `stdio` transport

Stdio MCP servers run as a subprocess **inside** the model-router sidecar. There is no network boundary, so the SA JWT header is not sent and identity verification via TokenReview does not apply. The authorization check (`allowedAgents`) still runs at AgentRun creation time.

---

## Trust model summary

```
AgentRun created
       │
       ▼
Controller checks MCPServer.spec.allowedAgents
       │
  agent listed? ──No──▶ Run FAILED (pod never scheduled)
       │
      Yes
       │
       ▼
Pod scheduled — model-router sidecar connects to MCP server
       │
       │  HTTP request:
       │    Authorization: Bearer <service-level secret>   (optional, from MCPConfig.auth)
       │    X-Agentorc-Identity: Bearer <sa-jwt, aud: agentorca/mcp>
       ▼
MCP server (optional) calls k8s TokenReview
       │
  authenticated? ──No──▶ return 401
       │
      Yes
       │
  username matches expected agent? ──No──▶ return 403
       │
      Yes
       ▼
   Handle request
```

The two layers are independent: the operator check happens before the pod starts, and the TokenReview check happens at request time for defense in depth.
