# Agents where people already work

Two ways to start an agent from somewhere other than the UI: a **webhook trigger** (another system posts a signed request) and, in the next section of this page once it ships, an embeddable **chat widget**. Both create ordinary runs under the tenant that owns the agent, so quotas, budgets, the audit trail and tenant isolation apply as they do to a run started in the UI.

Both need the PostgreSQL store (the tables carry `tenant_id` with row-level security, see [tenant isolation](tenant-isolation.md)). Without it the management endpoints answer 503 and the public endpoints answer 404.

## Webhook triggers

A builder adds a trigger to a **published** agent (an agent with an AgentDeployment) on the agent page, tab **Triggers**, or with the API:

| Call | Role | Audited |
|---|---|---|
| `POST /api/triggers` `{"namespace","agent","name","inputPath"?,"rateLimitPerMin"?}` | builder | yes (kind `Trigger`) |
| `GET /api/triggers?namespace=&agent=` | viewer | no |
| `PATCH /api/triggers/{id}` `{"enabled": bool}` | builder | yes |
| `DELETE /api/triggers/{id}` | builder | yes |

Creating answers once with the **secret** (`whsec_...`) and the hook path `/hooks/{id}`. The secret is never shown again. A trigger belongs to the caller's tenant: another tenant sees a 404 for its id, and the database refuses a row under the wrong tenant. An agent may have at most 20 triggers, with names unique per agent. Templates and unpublished agents are refused (409).

### Sending a request

```
POST https://<host>/hooks/trg_0123...
X-AgentOrca-Timestamp: 1790000000
X-AgentOrca-Signature: sha256=<hex>
Content-Type: application/json

{"issue": {"title": "Printer on fire"}}
```

The signature is the hex HMAC-SHA256, with this key and message:

- **key**: `hex(sha256(secret))`, the lowercase hex of the SHA-256 of the secret string, used as the text of the key;
- **message**: the timestamp, a dot, and the raw request body: `"<timestamp>.<body>"`.

```sh
secret=whsec_...; ts=$(date +%s); body='{"issue":{"title":"Printer on fire"}}'
key=$(printf %s "$secret" | openssl dgst -sha256 -hex | sed 's/^.* //')
sig=$(printf %s "$ts.$body" | openssl dgst -sha256 -hmac "$key" -hex | sed 's/^.* //')
curl -X POST https://<host>/hooks/<id> -H "X-AgentOrca-Timestamp: $ts" -H "X-AgentOrca-Signature: sha256=$sig" -d "$body"
```

The timestamp is part of the signed message (a signature over the body alone could be replayed forever). Why a derived key: the operator stores `hex(sha256(secret))`, not the secret. A read of the table cannot recover the secret, but it can verify a signature, so treat the table like any credential store.

### What the endpoint checks, in order

| Check | Answer |
|---|---|
| Unknown, malformed, **disabled** or deleted trigger (indistinguishable, so ids cannot be probed) | 404 |
| Body over 64 KiB | 413 |
| Timestamp missing, or more than 5 minutes from the operator's clock | 401 |
| Signature wrong (constant-time comparison) | 401 |
| The same signature already accepted inside the window (replay) | 401 |
| The trigger's own rate limit (default 30 per minute, 1 to 600; token bucket) | 429 with `Retry-After` |
| The tenant is suspended or deleted, the agent is gone or not allowed to the tenant | 404 |
| The tenant's own quotas (rate, concurrent runs, daily budget) | 429 or 402 |
| `inputPath` set and the body is not JSON or lacks the field | 422 |

Unsigned or badly signed requests never count against the rate limit, so a flood without the secret cannot lock the real sender out. Success is `202` with `{"runName","namespace"}`.

### The run

The input is the raw body text, or, when the trigger has an **input field** such as `issue.title` (dotted path; array positions as numbers), that field of the JSON body (a non-string value is passed as JSON). The run is an ordinary `AgentRun` in the agent's namespace labelled with the tenant ID and `agentorca.io/trigger: <id>`, with the annotation `agentorca.io/created-by: trigger:<id>`, so it shows in the run list and the audit of who started it is the trigger. The run is stamped with the tenant ID of the trigger's row, never anything from the request.

The replay memory and the rate limiter are per operator process. With several operator replicas behind one address a replay can be accepted once per replica inside the 5 minute window.

The UI proxy forwards `/hooks/*` without a session. Everything else under it still needs one.

## Not built yet

- Slack and Teams apps, and email triggers: they need third-party app registration (and for email an inbound mail domain), which is not set up. They are out of scope for now.
- Per-trigger input templates beyond one JSON field path.
- A shared (Redis) replay memory and rate limiter for several replicas.
