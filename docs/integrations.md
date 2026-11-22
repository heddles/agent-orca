# Agents where people already work

Three ways to start an agent from somewhere other than the UI: a **webhook trigger** (another system posts a signed request), an embeddable **chat widget** (a script a web page loads) and an **email** channel (write to an address, get the answer by mail). All create ordinary runs under the tenant that owns the agent, so quotas, budgets, the audit trail and tenant isolation apply as they do to a run started in the UI.

All need the PostgreSQL store (the tables `triggers`, migration `0018`, `widget_keys`, migration `0019`, and `inbound_connections` and `inbound_messages`, migration `0024`, carry `tenant_id` with row-level security, see [tenant isolation](tenant-isolation.md)). Without it the management endpoints answer 503 and the public endpoints answer 404.

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

The UI proxy forwards `/hooks/*`, `/inbound/*` and `/widget/*` without a session. Everything else under it still needs one.

## Email

A business user, or a system, writes to an address; an agent runs on the message; the answer is mailed back to the sender in the same conversation. It is the first adapter of the [inbound channel core](inbound-channels.md), which describes the shared behaviour (de-duplication, allow-list, limits, threading, delivery) and the security model. The run is a trigger actor's run, never a person's: invoker-mode MCP servers fail closed for it.

A builder adds an email channel to a **published** agent on the agent page, tab **Channels**, or with the API:

| Call | Role | Audited |
|---|---|---|
| `POST /api/inbound-connections` `{"namespace","agent","name","kind":"email","secretRef","config":{...}}` | builder | yes (kind `InboundConnection`) |
| `GET /api/inbound-connections?namespace=&agent=` | viewer | no |
| `PATCH /api/inbound-connections/{id}` (`enabled`, `config`, `secretRef`) | builder | yes |
| `DELETE /api/inbound-connections/{id}` | builder | yes |
| `POST /api/inbound-connections/{id}/test` | builder | no |
| `POST /inbound/email/{id}` | signed, no session | no |
| `GET /api/inbound-kinds` | viewer | no |
| `POST /inbound/{slack,mattermost}/{id}` | verified by the platform's signature or token, no session | no |

`test` logs in and reports the folders' message counts (IMAP) or checks the configuration (webhook). It reads no message, changes no flag and sends nothing. The list shows the status: last poll, last error (never a secret), and the received, answered and refused counts. `operator.inbound.enabled` (Helm; env `INBOUND_CHANNELS_DISABLED=true`) turns the feature off; it is on by default and does nothing until a connection exists.

### 1. The Secret (an administrator)

The mailbox login is never typed into the UI. Create a Secret in the agent's namespace and label it for channels:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: support-mailbox
  namespace: team-a
  labels:
    agentorca.io/inbound-secret: "true"
stringData:
  from: support@agents.example          # the mailbox: replies are sent From it, and it is never answered
  smtpHost: smtp.agents.example         # replies go out over SMTP (STARTTLS required)
  smtpPort: "587"
  smtpUsername: support@agents.example
  smtpPassword: <app password>
  # IMAP mode:
  imapHost: imap.agents.example
  imapPort: "993"                       # default 993 (tls) or 143 (starttls)
  imapSecurity: tls                     # tls (default) | starttls | none (needs insecure: "true")
  username: support@agents.example
  password: <app password>              # or accessToken: <OAuth bearer>, never both
  # Webhook mode:
  signingSecret: <long random string>   # the relay signs with hex(sha256(signingSecret))
  # Rarely: insecure: "true" (no TLS), allowPrivateAddresses: "true" (an on-premises server)
```

Hosts on loopback, link-local, private and other internal addresses are refused unless `allowPrivateAddresses` is `"true"`, as for notification channels (an on-premises Exchange or Dovecot needs it). The cluster's NetworkPolicy must let the operator reach the mail host: IMAP on 993 or 143, SMTP on 587 or 465.

Exchange Online and Gmail: use an app password, or an OAuth access token in `accessToken` (SASL XOAUTH2). **The operator does not refresh tokens**: use a token that outlives your rotation (a service principal's token refreshed by your secret tooling into this Secret), or an app password.

### 2. The connection (a builder)

```json
{
  "namespace": "team-a", "agent": "helper", "name": "Support mailbox", "kind": "email", "secretRef": "support-mailbox",
  "config": {
    "allowSenders": ["alice@example.com"],
    "allowGroups": ["corp.example"],
    "ratePerMin": 30, "senderPerHour": 10, "pollSeconds": 60,
    "adapter": {
      "mode": "imap",
      "folder": "INBOX", "processedFolder": "Processed",
      "requireAuthResults": true, "trustedAuthservId": "mx.agents.example",
      "acceptTextAttachments": false
    }
  }
}
```

- `allowSenders` / `allowGroups`: **required**, exact addresses and exact domains (`corp.example` does not admit `sub.corp.example`). Nothing else may start a run, and a refused message is dropped silently.
- `mode`: `imap` (the operator polls the mailbox, at least every 30 seconds) or `webhook` (a provider sends each message to `/inbound/email/{id}`).
- `processedFolder` (IMAP): where a handled message is moved (created if missing). Empty marks it read (`\Seen`) instead. A message is marked **only after it was safely recorded**; a failure leaves it unmarked and the next poll tries again. Use a mailbox of its own: a message someone reads by hand is marked read and skipped.
- `requireAuthResults` (default **true**) and `trustedAuthservId`: see the trust model below.
- `acceptTextAttachments` (default false).

### Who is trusted: Authentication-Results

Anyone can write `From: ceo@example.com`. Mail is not authenticated by its `From` line; the receiving mail server checks SPF, DKIM and DMARC and records the verdict in an **`Authentication-Results`** header (RFC 8601). With `requireAuthResults` on (the default) a message is taken only if that header shows the sender's domain authenticated: `dmarc=pass`, or `dkim=pass` for a domain that matches the `From` domain, or `spf=pass` for a matching envelope domain (a parent domain matches, a child or a sibling does not). A `dmarc=fail` anywhere in the header refuses it.

The header is text in the message, so a sender can write one too. The trust model is therefore: **only the header added by your own receiving server counts.**

- Set **`trustedAuthservId`** to the host name your server writes as the *authserv-id* (the first word of the header, for example `mx.agents.example`). Only headers with that id are read. A compliant server removes or renames an incoming header that claims its identity (RFC 8601 section 5), so a sender cannot forge one that carries it. Look at a real message to find the id.
- Without it the **topmost** header is used (the last server to touch the message puts its header on top). That is sound only if your server adds one to every message; if it does not, the sender's own header would be on top. The Test button warns when the id is not set.
- Some servers write no authserv-id (Exchange Online's header starts with `spf=pass ...`). For those leave `trustedAuthservId` empty and make sure the mailbox only receives mail that went through that server.
- In webhook mode the relay can put its own verdict in an `authenticationResults` field; it replaces whatever the message says about itself, and `trustedAuthservId` then has to appear in it.

`requireAuthResults: false` is for a closed system where the mailbox only receives mail from servers you control. Then the allow-list is only as strong as the `From` line.

### What the agent receives

Only text, in labelled fields, as the run input: the sender's address, the subject, the new text of the message, and the name, type and size of each attachment. The body is the text part (or the visible text of the HTML part); **only the new text** is kept (quoted history, `On ... wrote:`, `-----Original Message-----`, Outlook header blocks and signatures are cut) and capped at 16,000 characters. HTML is reduced to text without a browser: scripts, styles, embedded objects and anything marked hidden are dropped, a link contributes its text and never its address, and nothing is fetched. Control, invisible and direction-changing characters are removed. The text is untrusted user input; it is never the agent's instructions.

Attachments are **not** passed on by default; the agent sees names and sizes. With `acceptTextAttachments`, up to three `text/plain`, `text/csv` or `text/markdown` files of at most 100 KiB, with a text file name and valid text content, are added to the input. Executables, scripts, HTML and archives are never passed, whatever their declared type.

### What is never answered

A message is dropped (recorded as `ignored`, no reply) when it is automatic or could start a loop: `Auto-Submitted` other than `no`; `Precedence: bulk`, `junk`, `list` or `auto_reply`; `X-Autoreply`, `X-Autorespond`, `X-Auto-Response-Suppress`, `List-Id`, `List-Unsubscribe` or `X-Loop` present; an empty `Return-Path` (a bounce); a delivery report; a sender such as `mailer-daemon`, `postmaster`, `noreply`, `no-reply`, `donotreply`, `bounces`; or the mailbox's own address. The answers sent carry `Auto-Submitted: auto-replied`, so another agent-orca mailbox ignores them. On top of that, one sender can start at most `senderPerHour` runs per connection per hour, counted in the database. A message with more than one `From` address is refused. A message over 1 MiB is refused without being downloaded (IMAP) or read.

### The answer

The answer is mailed to the `From` address of the message (never a `Reply-To` or an address in the text), with subject `Re: <their subject>`, `In-Reply-To` and `References` so the mail client keeps the thread, `Auto-Submitted: auto-replied`, and the mailbox's address as `From`. It is plain text plus an HTML twin in which every character the agent wrote is escaped: no markup, no links, no styles. Header values cannot carry a line break. It is cut at 20,000 characters, and credentials of the connection and anything registered with the redaction registry are masked. A run that fails is answered "The agent could not complete this request.", without its error.

A reply in the same thread continues the same conversation (the answer's `Message-ID` encodes the thread, so even a client that sends only `In-Reply-To` is matched). A new thread starts a new run.

### Webhook mode: wiring a provider

`POST /inbound/email/{id}` accepts the message in one of three shapes, whichever the provider can produce:

- `Content-Type: message/rfc822`: the raw message, headers included (best: it carries the original headers).
- `application/json`: `{"from","subject","text","html","messageId","inReplyTo","references","headers","authenticationResults","attachments":[{"name","contentType","content":"<base64>"}]}`. The field names of Postmark (`From`, `TextBody`, `HtmlBody`, `MessageID`, `Headers` as a list of `{Name, Value}`) and Mailgun (`body-plain`, `body-html`, `message-headers` as pairs) are accepted too. `headers` may be an object, a list, or a raw header block.
- `multipart/form-data`: the same field names, files as attachments, or one field `email` (or `raw`) holding the raw message.

The request is signed like a [trigger webhook](#webhook-triggers): `X-AgentOrca-Timestamp` (Unix seconds, within 5 minutes) and `X-AgentOrca-Signature: sha256=<hex HMAC-SHA256 of "<timestamp>.<body>">`, with the key `hex(sha256(signingSecret))` and the `signingSecret` of the Secret. Only this scheme is implemented. **The providers do not sign this way**, so put a small relay between the provider and agent-orca: a function or a gateway rule that (1) verifies the provider's own signature or token, (2) copies the message and the provider's SPF/DKIM verdict into one of the shapes above (`authenticationResults`), and (3) signs and posts it. Common cases:

| Provider | Configure | The relay |
|---|---|---|
| SendGrid Inbound Parse | "POST the raw, full MIME message" to the relay | the relay checks the basic-auth or IP you set, forwards the `email` field as `message/rfc822` and builds `authenticationResults` from the `dkim` and `SPF` fields |
| Mailgun Routes | `forward()` to the relay | the relay verifies Mailgun's `timestamp`/`token`/`signature`, posts `body-plain`, `from`, `subject`, `message-headers` |
| Postmark Inbound | the inbound webhook URL is the relay | the relay checks its basic-auth, posts Postmark's JSON as it is, and adds `authenticationResults` from the `X-Spam-*` or `Received-SPF` headers it trusts |
| Amazon SES | receipt rule to SNS (with the content) | the relay confirms the SNS signature, posts the `content` field as `message/rfc822` and the `receipt` verdicts as `authenticationResults` |

None of this was tried against a real provider. The shapes are covered by tests with the providers' field names.

The endpoint answers 202 for every message it recorded (`{"status":"started"|"duplicate"|"rejected"|"ignored"|"throttled"}`), so a provider does not retry what policy refused; 401 for a missing or wrong timestamp or signature or a replay; 404 for an unknown, disabled or mismatching connection or an IMAP one; 503 when the message could not be recorded (the provider should retry). The replay memory is per operator process.

### What is stored and logged

Message bodies and subjects are not stored, not logged and not audited. The ledger (`inbound_messages`) holds digests, sizes, a status and the run's name; the sender's address is kept, only while an answer is pending, in the reply target. The audit trail records connection changes (id, name, kind), not messages. The status counters on the connection are counts. Logs name the connection id and kind, never an address or text.

## Chat platforms

Slack and Mattermost are adapters of the same [inbound channel core](inbound-channels.md): people mention the bot, message it or use a slash command, and the agent answers in the thread. Setup, scopes, Secret keys and behaviour are in [chat-channels.md](chat-channels.md); the contract for further platforms is in [inbound-channels.md](inbound-channels.md#chat-contract). `GET /api/inbound-kinds` lists the registered kinds with their settings, Secret keys and setup steps.

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


- Per-trigger input templates beyond one JSON field path.
- A shared (Redis) replay memory and rate limiter for several replicas. The widget's limits are per process too, so with several replicas the effective rate is the limit times the replicas (the budget cap and the active-run cap read the cluster, so they hold).
- Streaming answers in the widget: it polls once a second and shows the whole answer.
- Widget theming (colors, logo), file upload and markdown rendering. Answers are plain text.
- Per-visitor limits: all visitors of a key share its limits, and a visitor can use up the key's share.
- An iframe variant of the widget, for pages that cannot allow `connect-src`.
- A per-session message cap.
