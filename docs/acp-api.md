# ACP API

The ACP (Agent Communication Protocol) API is an ACP-compatible surface on port
**8000** that provides agent discovery and self-service run execution. It lets
callers discover which agents are available to their tenant, inspect each
agent's input/output schema and tools, and launch runs — all without writing
YAML or touching `kubectl`.

## Authentication

The ACP API uses the same bearer token as the External Task API. Obtain a JWT
via the OAuth2 `client_credentials` grant:

```bash
TOKEN=$(curl -s -X POST http://localhost:8084/oauth/token \
  -d "grant_type=client_credentials&client_id=acme-client&client_secret=secret123" \
  | jq -r .access_token)
```

All ACP endpoints except `/openapi.json`, `/healthz`, `/readyz`, `/version`,
`/metrics`, and `/ping` require a valid `Authorization: Bearer <token>` header.

## Endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/agents` | List agents available to the authenticated tenant |
| `GET` | `/agents/{name}` | Get an agent's full manifest |
| `POST` | `/agents/{name}/run` | Launch a run for an agent (schema-validated) |
| `POST` | `/runs` | Create a run directly (lower-level ACP endpoint) |
| `GET` | `/runs/{run_id}` | Get run status |
| `POST` | `/runs/{run_id}` | Resume a run (send additional input) |
| `POST` | `/runs/{run_id}/cancel` | Cancel a run |
| `GET` | `/runs/{run_id}/events` | List run events |
| `GET` | `/session/{session_id}` | Get session state |

## GET /agents

List all agents available to the authenticated tenant.

**Response:**

```json
{
  "agents": [
    {
      "name": "support-bot",
      "description": "Customer support agent",
      "input_content_types": ["text/plain"],
      "output_content_types": ["text/plain"],
      "input_schema": { ... },
      "output_schema": { ... },
      "allowed_tools": [ ... ],
      "knowledge_bases": ["docs"],
      "guardrail_policy": "phi-redact",
      "clarify_available": true
    }
  ]
}
```

## GET /agents/{name}

Get the full manifest for a specific agent. The manifest includes:

| Field | Type | Description |
|---|---|---|
| `name` | string | Agent name |
| `description` | string | Human-readable description |
| `input_content_types` | []string | Accepted input content types (e.g. `text/plain`) |
| `output_content_types` | []string | Produced output content types |
| `input_schema` | object | JSON Schema for input — includes `tool_names` list for dynamic tool selection |
| `output_schema` | object | JSON Schema for output |
| `allowed_tools` | []object | Tools available to this agent (built-in + configured), each with `name`, `description`, `input_schema` |
| `knowledge_bases` | []string | KnowledgeBase names available to this agent |
| `guardrail_policy` | string | GuardrailPolicy name applied to this agent |
| `clarify_available` | bool | Whether `_clarify` is available (false if `disableClarify: true`) |

**Built-in tools** always available (unless disabled):

| Tool | Description |
|---|---|
| `_clarify` | Ask the human a clarifying question |
| `_rag_search` | Search the agent's KnowledgeBases |
| `_rag_ingest` | Ingest documents into a KnowledgeBase |

**Response example:**

```json
{
  "name": "support-bot",
  "description": "Customer support agent",
  "input_schema": {
    "type": "object",
    "properties": {
      "input": { "type": "array", "items": { "$ref": "#/components/schemas/ACPMessage" } },
      "tool_names": { "type": "array", "items": { "type": "string" } }
    },
    "required": ["input"]
  },
  "output_schema": {
    "type": "object",
    "properties": {
      "output": { "type": "array", "items": { "$ref": "#/components/schemas/ACPMessage" } }
    }
  },
  "allowed_tools": [
    {"name": "_clarify", "description": "Ask the human a clarifying question"},
    {"name": "_rag_search", "description": "Search knowledge bases"},
    {"name": "web-search", "description": "Search the web", "input_schema": {"type": "object", "properties": {"query": {"type": "string"}}}},
    {"name": "get-customer-info", "description": "Look up customer data", "input_schema": {"type": "object", "properties": {"customer_id": {"type": "string"}}}}
  ],
  "knowledge_bases": ["support-docs"],
  "guardrail_policy": "phi-redact",
  "clarify_available": true
}
```

## POST /agents/{name}/run

Launch a new run for an agent. This is a schema-validated, UX-friendly wrapper
around `POST /runs` that accepts ACP-format messages.

**Request body:**

| Field | Type | Required | Description |
|---|---|---|---|
| `input` | []ACPMessage | yes | Array of input messages |
| `session_id` | string | no | Session ID for conversation continuity |

**ACPMessage format:**

```json
{
  "role": "user",
  "parts": [
    {
      "content_type": "text/plain",
      "content": "How do I reset my password?"
    }
  ]
}
```

| Field | Description |
|---|---|
| `role` | Message role: `user`, `assistant`, `tool` |
| `parts` | Array of message parts |
| `parts[].content_type` | MIME type: `text/plain`, `text/html`, `application/json`, etc. |
| `parts[].content` | The message content (string) |
| `parts[].content_url` | URL to external content (alternative to inline `content`) |

**Request example:**

```bash
curl -X POST http://localhost:8000/agents/support-bot/run \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "input": [
      {
        "role": "user",
        "parts": [
          {"content_type": "text/plain", "content": "How do I reset my password?"}
        ]
      }
    ]
  }'
```

**Response (201):**

```json
{
  "agent_name": "support-bot",
  "run_id": "acp-support-bot-9flnj",
  "status": "created",
  "created_at": "2024-01-01T12:00:00Z"
}
```

### Run statuses

| Status | Description |
|---|---|
| `created` | Run has been created, not yet started |
| `in-progress` | Run is actively executing |
| `awaiting` | Run is waiting for human input (e.g. `_clarify`) |
| `cancelling` | Run is being cancelled |
| `cancelled` | Run was cancelled |
| `completed` | Run finished successfully |
| `failed` | Run failed |

## POST /runs

The lower-level ACP endpoint for creating runs. Accepts `agent_name`, `input`,
`session_id`, `session`, and `mode` fields. The `POST /agents/{name}/run`
endpoint is the recommended UX-friendly alternative.

## GET /runs/{run_id}

Get the status and output of a run.

**Response:**

```json
{
  "agent_name": "support-bot",
  "run_id": "acp-support-bot-9flnj",
  "status": "completed",
  "output": [
    {
      "role": "assistant",
      "parts": [
        {"content_type": "text/plain", "content": "Go to Settings > Security > Reset Password."}
      ]
    }
  ],
  "created_at": "2024-01-01T12:00:00Z",
  "finished_at": "2024-01-01T12:00:05Z"
}
```

## GET /runs/{run_id}/events

List events for a run (ACP event stream).

## POST /runs/{run_id}/cancel

Cancel a running task.

## GET /session/{session_id}

Retrieve session state for conversation continuity.

## Using aoctl

All ACP endpoints are available via `aoctl agents`:

```bash
aoctl agents list
aoctl agents describe support-bot
aoctl agents run support-bot --input "How do I reset my password?"
```

See [aoctl-reference.md](aoctl-reference.md) for the full CLI reference.

## Rate limiting & budget

ACP endpoints are subject to the same per-tenant rate limits and budget
enforcement as the External Task API. Exceeding `requestsPerMinute` or
`concurrentRuns` returns `429 Too Many Requests` with a `Retry-After` header.
Exceeding `budgetPerDayUSD` returns `402 Payment Required`. See
[Rate Limiting & Budget Enforcement](rate-limiting.md) for details.
