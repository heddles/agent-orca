# MCP Server Access Control

MCP servers in agent-orca have two complementary security layers:

| Layer | Where enforced | Mechanism |
|---|---|---|
| **Authorization** | Operator, at AgentRun creation time | `MCPServer.spec.allowedAgents` |
| **Identity verification** | MCP server, at request time | Kubernetes SA JWT via TokenReview |

Both layers work together: the operator prevents unauthorized runs from starting, and the MCP server can independently verify every inbound request came from a legitimate agent pod.

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
