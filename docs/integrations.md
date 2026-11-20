# Agents where people already work

Two ways to start an agent from somewhere other than the UI: a **webhook trigger** (another system posts a signed request) and an embeddable **chat widget** (a script a web page loads). Both create ordinary runs under the tenant that owns the agent, so quotas, budgets, the audit trail and tenant isolation apply as they do to a run started in the UI.

Both need the PostgreSQL store (the tables `triggers`, migration `0018`, and `widget_keys`, migration `0019`, carry `tenant_id` with row-level security, see [tenant isolation](tenant-isolation.md)). Without it the management endpoints answer 503 and the public endpoints answer 404.

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

The UI proxy forwards `/hooks/*` and `/widget/*` without a session. Everything else under it still needs one.

## Chat widget

A builder adds a **widget key** to a **published** conversational agent (tab **Chat widget** on the agent page, or the API) and pastes the snippet it shows on the pages that may use it:

```html
<script src="https://<host>/widget/v1.js" data-key="wgt_..." async></script>
```

Optional attributes: `data-title="Ask us"` (header text), `data-open="true"` (start open). The script mounts a chat button in the corner of the page, inside a shadow root so the page's styles and the widget's do not touch. All text, from the visitor and from the agent, is put in the page as text, never as HTML. The page's Content-Security-Policy must allow the script and `connect-src` to `https://<host>`.

| Call | Role | Audited |
|---|---|---|
| `POST /api/widget-keys` `{"namespace","agent","name","allowedOrigins":[...],"rateLimitPerMin"?,"budgetUsdPerDay"?}` | builder | yes (kind `WidgetKey`) |
| `GET /api/widget-keys?namespace=&agent=` | viewer | no |
| `PATCH /api/widget-keys/{id}` (`enabled`, `allowedOrigins`, `rateLimitPerMin`, `budgetUsdPerDay`) | builder | yes |
| `DELETE /api/widget-keys/{id}` | builder | yes |

### What protects the agent

The key is public: it is in the page's HTML. It is an address, not a credential. No credential of the tenant is ever in the page.

- **One agent.** The key names one agent in one tenant. The visitor sends only a message; the agent, deployment and namespace come from the key's row on the server. The request body is checked strictly, so a body that names anything else is refused (400). The tenant, its allowed agents and its quotas are looked up from the row's tenant ID on every message; a suspended or deleted tenant, a deleted agent or an unpublished one stops the widget (404).
- **Allowed origins.** `POST /widget/v1/{key}/sessions` and every later call require an `Origin` header that exactly matches one of the key's origins (`https://host[:port]`; `http` only for `localhost` and `127.0.0.1`; no wildcards, paths or credentials). Otherwise 403, with no CORS headers. CORS preflights get the same check, and the response allows only the requesting origin. Removing an origin applies at once, to live sessions too. A browser sets `Origin` and a page cannot change it, so another site cannot use your key from a visitor's browser. A script outside a browser can send any `Origin`, so the origin rule does not stop it: the limits below do.
- **Short-lived sessions.** A session is an HMAC token (`X-Widget-Session`, 30 minutes) bound to the key, a random session ID and the origin. It is signed with a key derived from the operator's persisted signing key under its own label, so all replicas accept it, and it is worthless anywhere else (it is not a tenant token; the UI API rejects it). A session is one conversation: the chat history lives in the same session checkpoint as the UI's chat, scoped by tenant. When the token expires the visitor starts a new conversation.
- **Rate limit.** `rateLimitPerMin` (default 30, 1 to 600) is shared by all visitors of the key, for messages, and separately for session creation. Polling has its own larger allowance. Requests from a denied origin or with a bad session never reach the limiter.
- **Budget cap.** `budgetUsdPerDay` (default 5): the spend of the runs labelled with the key since 00:00 UTC. When it is reached, messages get 402 and the widget says the chat is unavailable until tomorrow. At most 5 runs of a key may be unfinished at once (429), because a run's cost is known only when it ends. The tenant's own quotas (requests, concurrent runs, daily budget) apply as well.
- **Visibility.** A session can read only the runs it started (anything else is a 404 as if absent). A failed run shows a generic message, never its output.

### How it chats

The widget uses the chat path of the UI: `startChatTurn` records the message in the session checkpoint and creates an `AgentRun` chained to the session's previous run through the agent's AgentDeployment (labels `agentorca.io/deployment`, `agentorca.io/session`, `agentorca.io/source: chat`, plus `agentorca.io/widget-key: <id>`, annotation `agentorca.io/created-by: widget:<id>`, tenant ID from the key). The widget polls `GET /widget/v1/{key}/runs/{run}`; when it finds the run finished, the server appends the answer to the session once with the same code as the UI's "complete" call.

## Not built yet

- Slack and Teams apps, and email triggers: they need third-party app registration (and for email an inbound mail domain), which is not set up. They are out of scope for now.
- Per-trigger input templates beyond one JSON field path.
- A shared (Redis) replay memory and rate limiter for several replicas. The widget's limits are per process too, so with several replicas the effective rate is the limit times the replicas (the budget cap and the active-run cap read the cluster, so they hold).
- Streaming answers in the widget: it polls once a second and shows the whole answer.
- Widget theming (colors, logo), file upload and markdown rendering. Answers are plain text.
- Per-visitor limits: all visitors of a key share its limits, and a visitor can use up the key's share.
- An iframe variant of the widget, for pages that cannot allow `connect-src`.
- A per-session message cap.
