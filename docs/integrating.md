# Integrating with agent-orca

This guide is for developers and enterprise engineers who want to send tasks to
agent-orca from external software. Choose your level:

| Goal | Use |
|------|-----|
| Quick scripted integration | `aoctl` CLI |
| Embed in an application | Python SDK (`pip install agentorca`) or Go client |
| Full control / custom language | The External Task API over HTTP (contract: `GET /<endpoint>/openapi.json`) |
| Discover agents & self-service run | ACP API (`GET /agents`, `GET /agents/{name}`, `POST /agents/{name}/run`) |

## 1. Pick a surface

agent-orca exposes three HTTP surfaces (see [docs/auth.md](auth.md)):

- **External Task API** on port **8084** — `POST /v1/tasks`, streaming, webhooks.
  This is what integrations use. See [docs/enterprise-integration.md](enterprise-integration.md).
- **ACP API** on port **8000** — agent discovery (`GET /agents`) and ACP-style runs.
  See [docs/acp-api.md](acp-api.md) for the full reference.
- **UI API** on port 8080 — the local dev chat proxy / React SPA only.

All three share the same authentication: an OAuth2 token from `POST /oauth/token`
(`client_credentials` grant), a federated OIDC JWT, or an in-cluster Kubernetes
ServiceAccount token. Tenants, rate limits, and budgets are defined by
`TenantConfig` CRDs — see [docs/enterprise-integration.md](enterprise-integration.md)
for the full example.

## 2. Use the CLI

```bash
# Install (from the repo root)
make aoctl          # builds bin/aoctl
go install ./cmd/aoctl   # or install from any checked-out tree

# Authenticate
aoctl login \
  --endpoint http://localhost:8084 \
  --client-id acme-client \
  --client-secret secret123

# Submit + watch a task
aoctl tasks submit --agent support-bot --input "How do I reset my password?" --stream

# Or poll
TID=$(aoctl tasks submit --agent support-bot --input "hi" | jq -r .id)
aoctl tasks wait $TID
aoctl tasks ls
aoctl tasks get $TID
```

`aoctl` caches your token in `~/.aoctl/config.json` (override with
`AOCTL_CONFIG_DIR`). Subsequent commands pick it up automatically. Point
`AOCTL_ENDPOINT` / `AOCTL_ACP_ENDPOINT` at an in-cluster service if you prefer
not to pass `--endpoint` each time.

## 3. Use the Python SDK

```bash
pip install ./pkg/python/agentorca   # or: pip install agentorca
```

```python
from agentorca import AgentOrca

ao = AgentOrca(endpoint="https://agent-orca.acme-internal.com:8084")
ao.login(client_id="acme-client", client_secret="secret123")

run = ao.submit_task(agent="support-bot", input="How do I reset my password?")
print("task:", run["id"], run["status"])

# Stream tokens to completion
for ev in ao.stream_task(run["id"]):
    if ev["event"] == "token":
        print(ev["data"], end="")
print()

# Or block until done
final = ao.wait_task(run["id"])
print("final:", final["status"], final.get("output"))
```

## 4. Write an agent (SDK)

agent-orca ships SDKs that let you write agents targeting the **Tier 1 `openai-compatible`** framework tier. The SDK handles the OpenAI-compatible HTTP protocol against the model-router sidecar (default `http://localhost:8080`), checkpoint save/restore, and built-in lifecycle tools.

### Python SDK

```bash
pip install ./pkg/python/agentorca
```

```python
from agentorca import Agent

agent = Agent()  # reads OPENAI_BASE_URL, AGENTORC_INPUT, etc. from env

@agent.tool
def search(query: str) -> str:
    """Search the web for current information."""
    ...

result = agent.run(input="Find the latest news about LLMs")
# result.status is "succeeded" or "failed"
# result.output is the final text
```

**Lifecycle methods:**

| Method | Description |
|---|---|
| `agent.run(input, maxTurns=50)` | Run the agent loop until done/fail |
| `agent.done(output)` | Signal successful completion |
| `agent.fail(reason)` | Signal failure |
| `agent.ask(question)` | Ask the human a clarifying question |
| `agent.handoff(agentRef, input)` | Delegate to another agent |
| `agent.spawn(agentRef, input)` | Spawn a sub-agent tool call |

**Checkpoint helpers:**

| Method | Description |
|---|---|
| `agent.save_checkpoint(data)` | Persist arbitrary state |
| `agent.load_checkpoint()` | Restore state from last checkpoint |

See [examples/agent-sdk-template/agent.py](../examples/agent-sdk-template/agent.py) for a full reference implementation.

### Go SDK

```go
import "github.com/floppyfish14/agent-orca/pkg/agent"

func main() {
    agent := agent.New()
    agent.Tool("search", "Search the web", searchHandler)
    result := agent.Run("Find the latest news about LLMs")
    fmt.Println(result.Status, result.Output)
}
```

## 5. Discover agents & self-service run

The ACP API (port 8000) lets any authenticated caller discover which agents
are available to their tenant, inspect each agent's input/output schema and
tools, and launch runs — without writing YAML or touching `kubectl`.

```bash
# 1. Discover available agents
curl -s http://localhost:8000/agents \
  -H "Authorization: Bearer $TOKEN"

# 2. Inspect an agent's manifest (schema, tools, KBs, guardrails, clarify)
curl -s http://localhost:8000/agents/support-bot \
  -H "Authorization: Bearer $TOKEN"

# 3. Launch a run with schema-validated input
curl -s -X POST http://localhost:8000/agents/support-bot/run \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{ "input": [{"role":"user","parts":[{"content_type":"text/plain","content":"How do I reset my password?"}]}] }'
```

Or use the CLI:

```bash
aoctl agents list
aoctl agents describe support-bot
aoctl agents run support-bot --input "How do I reset my password?"
```

**Manifest fields:**

| Field | Description |
|---|---|
| `input_schema` | JSON Schema for the agent's input (includes `tool_names` list) |
| `output_schema` | JSON Schema for the agent's output |
| `allowed_tools` | List of tools with names, descriptions, and input schemas |
| `knowledge_bases` | KnowledgeBase names available to this agent |
| `guardrail_policy` | GuardrailPolicy name applied to this agent |
| `clarify_available` | Whether the `_clarify` built-in tool is available (false if `disableClarify: true`) |

## 6. Use the HTTP API directly

The contract is machine-readable. Fetch it from a running operator:

```bash
# External Task API (port 8084)
curl http://localhost:8084/openapi.json

# ACP API (port 8000)
curl http://localhost:8000/openapi.json
```

Or view the specs in-repo at `internal/apiserver/schemas/openapi-external.yaml`
and `internal/apiserver/schemas/openapi-acp.yaml` (`make openapi` copies them to
`./openapi/`). See [docs/openapi-spec.md](openapi-spec.md) for the full spec
reference. Generate a client with any OpenAPI generator:

```bash
npx --package=@openapitools/openapi-generator-cli openapi-generator-cli generate \
  -i internal/apiserver/schemas/openapi-external.yaml \
  -g python -o /tmp/agentorca-client
```

## 7. Observability

Probe the external API and scrape metrics:

```bash
curl http://localhost:8084/healthz   # liveness
curl http://localhost:8084/readyz    # readiness (checks K8s API + Redis if configured)
curl http://localhost:8084/version
curl http://localhost:8084/metrics   # Prometheus metrics
```

Every authenticated external request is also written to the operator log as an
audit line (`external_api_request`) with the tenant, method, path, status, and
latency. See [docs/observability.md](observability.md) for the full metrics
reference and Prometheus scrape config.

## 8. Securing inbound callbacks

If you configure `callback.url` on a task, verify the delivery with HMAC-SHA256
— see [Webhook signature verification](enterprise-integration.md#webhook-signature-verification-hmac)
in the enterprise guide.
