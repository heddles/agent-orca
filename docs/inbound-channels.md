# Inbound channels

An inbound channel lets people (or systems) start a run of one **published** agent from somewhere other than the UI, and gets the result back in the same conversation. Email is the first channel (see [integrations.md](integrations.md#email)); chat platforms are the second kind (Slack and Mattermost are built, see [chat-channels.md](chat-channels.md)), and the core is built so that none of them is a special case.

This page is the contract for an adapter, the security model, and what is not built.

## Shape

```
  channel ──► Adapter ──► Message ──► Pipeline ──► AgentRun (as a trigger actor)
                ▲                         │
                └──── Reply(Result) ◄─────┘   (when the run has finished)
```

- **Adapter** (`internal/inbound`, one package per channel under it, `internal/inbound/email` today): speaks the channel's protocol and turns what arrives into a normalised `Message`. It also delivers the answer. It knows nothing about tenants, quotas or runs.
- **Pipeline** (`internal/inbound/pipeline.go`): everything that is the same for every channel. De-duplication, the sender allow-list, rate limits, threading, mapping to an `AgentRun`, delivery of the answer, redaction, failure handling.
- **Runner** (`internal/inbound/runner.go`): a leader-elected manager runnable. One poller goroutine per enabled connection of a polling adapter (bounded concurrency, backoff up to 15 minutes, graceful shutdown), plus the loop that delivers answers of finished runs and the pruning of the ledger.
- **Bridge** (`internal/apiserver/inbound_receive.go`): the only code that touches the cluster. It starts the run (with the checks of the webhook trigger), reads runs and reads Secrets.

A test (`TestCoreNamesNoChannelVendor`) fails if a channel vendor's name appears in the core. Vendors are named only inside their adapter's package.

## The adapter contract

```go
type Adapter interface {
    Kind() string                        // "email", ...; what a connection stores
    Capabilities() Capabilities          // Threads, Attachments, Markdown, MaxReplyChars
    ValidateConfig(json.RawMessage) error // the adapter's own settings, checked when a connection is saved
    Reply(ctx, Connection, ReplyTarget, Result) error
}
type Receiver interface { Adapter; Receive(ctx, Connection, *http.Request) (Message, error) } // the channel calls us
type Poller   interface { Adapter; PollOnce(ctx, Connection, Handler) error }                 // we call the channel
type Tester   interface { Adapter; Test(ctx, Connection) TestReport }                         // optional: "Test connection"
type PollSelector interface { Polls(adapterConfig json.RawMessage) bool }                     // optional: poll only for some settings
```

A **Receiver** verifies the request (for the signed channels `inbound.ReadSigned` does the HMAC, timestamp and replay checks of the trigger webhooks; return `*AuthError` for a request that fails them) and returns a `Message`. A **Poller** hands each message to the `Handler` and marks it done in the channel **only after the handler returned nil**: nil means the message is safely recorded, whatever became of it. A message the adapter refuses is returned with the identity fields filled (`MessageID`, `Sender.ID`, `Size`) and a `*Rejection{Reason, Ignore}`, so the refusal is recorded too and the channel does not offer it again.

`Message` carries the channel kind, the connection and tenant (set by the core, an adapter cannot choose them), the sender (`ID`, an optional `Group` such as an email domain or a workspace, and a display name), the text, a title, attachment descriptors, a thread id, the channel's message id (the idempotency key), the size, and an opaque `ReplyTarget` that only the adapter reads. `Result` is what is sent back: the text, whether the run failed, and for failures only a generic wording (the reason stays in the logs).

`Connection` is a stored connection with its Secret already read (`Secret` never prints) and the adapter's settings. An adapter does not read Kubernetes, the database or the run.

Register an adapter in `cmd/main.go` (`addInboundChannels`). The API and the UI list the registered kinds; a connection of an unregistered kind is refused.

## What the core does, in order

1. **Idempotency.** `inbound_messages` has a unique key `(tenant_id, connection_id, message_key)`. The key is a digest of the connection id and the channel's message id, so a redelivery, a retry by a provider or a flag that was lost in a mailbox starts nothing twice. A claim that never got as far as starting a run can be taken again after five minutes (a process that died in between).
2. **Allow-list.** `allowSenders` (exact ids) and `allowGroups` (exact group names). **No list means nobody**: saving a connection without one is a 400. There are no wildcards, and a group is not its sub-domains.
3. **Limits.** `ratePerMin` per connection (default 30), `senderPerHour` per sender per connection (default 10, counted in the database so it holds across restarts and replicas: this is what stops two automated senders answering each other), and the tenant's own request rate, concurrent-run cap and daily budget (`enforceQuotas`, the code the webhook triggers use).
4. **Threading.** If the channel has threads and the connection has not opted out (`newThreadPerMessage`), a message in a thread that already has a run continues it: `AgentRun.spec.priorRunRef` points at the thread's latest run, if that run is this connection's, of the same agent and tenant, and succeeded. Otherwise a new conversation starts. Whether the agent can carry the earlier conversation forward is the existing `priorRunRef` mechanism (checkpoints in the state store); without it the run simply starts without history.
5. **The run.** An ordinary `AgentRun`, labelled with the tenant and `agentorca.io/trigger: <connection id>` and `agentorca.io/inbound: <kind>`, with `agentorca.io/created-by: trigger:<connection id>`. **It is not a person's run**: no user is attached, so an MCP server in invoker mode has nobody to act for and fails closed, exactly as for a webhook trigger. A test asserts it (`invokerOf` returns no user). The sender's identity is the channel's claim and is used only for the allow-list and as a label in the input.
6. **The input.** Labelled fields (channel, sender, subject, attachment names) and then the text, which is introduced as the sender's words. The fields are cleaned to one line, so a sender cannot forge a field. The text is untrusted input to the run, never instructions and never a system prompt.
7. **The answer.** When the run has finished the worker builds the `Result`, masks secrets (the connection's own and everything registered with `internal/redact`), cuts it to the adapter's `MaxReplyChars` (a note says so) and calls `Reply`. Delivery is retried with a growing wait and given up after five attempts (status `failed`, reason `reply_failed`). A failed run is answered with a general sentence, never its error text. A run that never finishes is given up after two hours. Delivery is at least once: a crash between sending and recording can send an answer twice.

## Persistence

Migration `0024_inbound.sql`, both tables with the tenant row-level security of `0018`/`0019` (`tenant_id` and the `tenant_isolation` policy).

- `inbound_connections`: `tenant_id`, `id` (`inb_` and 32 hex), `kind`, `name`, agent namespace and name, `enabled`, `config` (jsonb, **non-secret** settings), `secret_ref` (the name of a Secret), `created_by`, timestamps and status (`last_poll_at`, `last_error` without secrets, received, replied and refused counts). At most 10 per agent.
- `inbound_messages`: the ledger. Digests of the message id, thread id and sender; the size; the status (`received`, `running`, `replied`, `rejected`, `ignored`, `failed`) and a short reason code; the run's name. **No body, no subject, no address.** The one exception is `reply_target`: while an answer is pending it holds what the adapter needs to send it (for email, the sender's address and the thread's ids) and it is set to NULL when the answer is sent or given up. Rows older than 30 days (and not pending) are pruned hourly; deleting a connection deletes its rows.

The audit trail (`audit_events`, kind `InboundConnection`) records who created, changed or deleted a connection, with the connection id and name. It records no message, no sender and no allow-list.

## Where secrets live

Credentials are **never** in `inbound_connections` and never go through the API. A connection holds `secretRef`, the name of a Kubernetes Secret in the agent's namespace, and the operator reads it when it needs it. The Secret must carry the label `agentorca.io/inbound-secret: "true"`.

Why a Secret and not the envelope-encrypted store of [per-user connections](mcp-oauth.md): that store holds tokens a *person* obtained through a consent flow in the UI, one per user. A mailbox login is a credential an *administrator* provisions for the platform, and this is how notification channels already work (`TenantConfig.spec.notifications`: the destination is in a Secret the UI cannot write). Using Secrets means the platform's secret tooling (rotation, External Secrets, sealed secrets, RBAC) applies, nothing needs a new key ring, and a database dump holds no mail credential.

The label is the guard against a confused deputy. The operator can read most Secrets; a builder can create a connection but cannot create a Secret or put a label on one. So a connection can only use a Secret an administrator made *for channels*. The mailbox host, port and login, the address replies come from, and the `insecure` and `allowPrivateAddresses` switches are all in the Secret rather than in the connection, so a builder cannot aim the operator at a host, pick the From address, or turn TLS off.

## Chat contract

Chat platforms differ from mail in who the sender is, where the conversation lives, how fast the platform wants an answer and how replies are drawn. The core covers that with a handful of generic pieces; an adapter fills them in and names no vendor outside its own package (`TestCoreNamesNoChannelVendor` also scans `internal/inbound/chat` and `internal/inbound/verify`).

**What a chat adapter sets on the `Message`**

| Field | Meaning |
|---|---|
| `Sender.ID`, `Sender.Group`, `Sender.Display` | the user id, the workspace / tenant / organisation id, the display name |
| `Channel` | the space (channel, room, space, conversation) id |
| `ThreadID` | built by the adapter from workspace, space and the platform's thread, so two spaces never share a conversation |
| `Mode` | `direct`, `mention`, `command` or `channel` (how the agent was addressed) |
| `FromBot` | the author is another bot, an app, an edit or our own bot: the core records the message as ignored (`bot_message`) and never answers it |
| `MessageID` | the idempotency key: a stable id of the *message*, not of the delivery, so a retry and a second event for the same message agree |

**Capabilities** an adapter declares: `Threads`, `Dialect` (`""` none, `mrkdwn`, `markdown`, `html`), `MaxReplyChars` (per message) and `MaxReplyParts`, `Modes`, `AckFirst`, `WorkingIndicator`.

**Allow-lists** (all exact, no wildcards, case-insensitive): `allowSenders` (user ids), `allowGroups` (workspace ids), `allowChannels` (space ids). The sender lists are an OR (a listed user, or a member of a listed workspace); the channel list is an AND on top (a message from any other space is refused, whoever wrote it). A connection needs at least one of the three, and with only channels it admits whoever can write in those spaces. Which ids an adapter calls what is in its descriptor. Reasons in the ledger: `sender_not_allowed`, `channel_not_allowed`, `mode_not_enabled`.

**Modes.** `modes` on a connection lists what the agent answers. Empty means the adapter's modes except `channel`. `channel` (every message of a space, no mention needed) must be named and needs `allowChannels`; saving it without is a 400. A mode the adapter cannot deliver is a 400.

**Ack-first.** A platform wants a 200 within about three seconds and retries on silence. An adapter with `AckFirst` gets this from the receive endpoint: it verifies the request (`Receive`), then runs `Enrich` (optional lookups), the pipeline and the run launch in the background with their own context, and waits at most two seconds for them. If they finish, the answer carries the true outcome (and a failure to record the message is a 503 so the platform retries; the pipeline is idempotent). If not, the endpoint answers `200 {"status":"accepted"}` and the work goes on, retried three times on error. The one thing this can lose is a message whose recording fails after the acknowledgement, only when the database and the cluster both stay down; it is logged. At most 64 background handlings are in flight; beyond that the endpoint answers 503. The agent run itself never holds the request: the reply is sent later by the delivery loop, as for email. A direct answer to the platform is possible with `*inbound.Response` (returned as the error of `Receive`: a verification challenge) or `Acknowledger.AckResponse` (a slash command's immediate line, never a refusal notice).

**Working indicator.** `Indicator.Working` is called once after the run started, best effort, five seconds at most.

**Threading.** `ThreadID` maps to a conversation as for email: the next message in the thread continues the thread's latest run (`priorRunRef`).

**Replies.** `chat.Render(text, caps)` (`internal/inbound/chat`) converts the agent's Markdown to the dialect and splits long text at paragraph, line or word boundaries (a code fence cut in two is closed and reopened) into at most `MaxReplyParts` messages of at most `MaxReplyChars`; beyond that the last one says it was shortened. The core cuts the text at `MaxReplyChars` times `MaxReplyParts` before the adapter sees it. Every mention is neutralised, in code spans excepted where platforms do not interpret them: `@name`, `@channel`, `@here`, `@everyone`, `@all`, `<@id>`, `<!here>`, `<!subteam^..>`, `<users/..>`, `<at>` get a zero-width space or are escaped, and links are kept only for `http` and `https`. So an agent can never mass-ping, whatever it writes. A delivery that fails half way is retried whole by the core (at least once): a part can be posted twice.

**Outbound calls.** `chat.Client`: the SSRF-guarded dialer (loopback, private, link-local and metadata addresses refused after DNS unless the Secret says `allowPrivateAddresses`), no proxy, no redirects, a timeout per attempt, three attempts for transport errors, 5xx and 429, waiting for `Retry-After` when it is at most 20 seconds and otherwise returning the 429 to the core's delivery retry. Errors name the method and the platform's error code, never a body, a header, a URL or a token.

**Identity.** A chat user is not an agent-orca user. The run is a trigger actor's run (`created-by: trigger:<connection id>`) with no invoker, so invoker-mode MCP servers fail closed; a test asserts it for a chat kind. The platform ids are the platform's claim, verified by its signature or token, and are used only for the allow-lists and as a label in the input. Linking a chat user to an agent-orca user (`requireLinkedUser`) is future work.

**Descriptor.** An adapter implements `Describer`: title, summary, its own settings (key, label, type `string|bool|int|enum`, required, help, options), the Secret keys (with `required`), help texts for the three allow-lists, and setup steps in which `{{url}}` stands for the connection's receive URL. `GET /api/inbound-kinds` returns them with the capabilities; the Channels tab draws its form and per-connection setup instructions from them, and creating a connection checks that the Secret has the required keys (names only). `Tester.Test` is the Test button: for chat an `auth.test`-like call showing the bot and the workspace, never a secret.

**Verification toolkit** (`internal/inbound/verify`), for receivers whose platform signs differently from the trigger webhooks:

- `VerifyHMAC(Sig{Secret, Base, Got, Prefix, Encoding})`: HMAC-SHA256 over a base string the adapter builds (`v0:<ts>:<body>`, a bare body, ...), hex or base64, constant time; `Timestamp(ts, now, window)`.
- `TokenEqual`, `TokenAny`, `SplitTokens`: constant-time comparison of shared tokens (both sides are hashed first, so length does not leak).
- `JWTVerifier` (`NewJWTVerifier(JWTConfig)`, `Verify(ctx, token)`, `BearerToken(header)`): verifies a bearer JWT against a JWKS URL. Keys are cached (default one hour) and fetched through the SSRF-guarded dialer over https, without redirects. An unknown `kid` triggers one refetch, at most once per minute (key rotation without hammering the endpoint). Strict checks: the algorithm must be in the configured list (default `RS256`; `none` and HMAC algorithms are refused at construction), the key must be for that algorithm and a signing key, `exp` is required, `nbf` is honoured, `iss` must equal one of the configured issuers and `aud` contain one of the configured audiences (both required), one minute of leeway. Errors never contain the token.

**Adding an adapter** needs: a package under `internal/inbound/<name>` with `Kind`, `Capabilities`, `ValidateConfig`, `Describe`, `Receive` (verify with the toolkit, return a `Message`, `*Response` or `*Rejection`), `Reply` (use `chat.Render` and `chat.Client`), `Test`, optionally `Enrich`, `AckResponse`, `Working`; one line in `addInboundChannels` (`cmd/main.go`); tests against `httptest` fakes of the platform. Nothing in the core, the API or the UI changes.

## Not built

- Further chat adapters (Microsoft Teams, Google Chat, Webex): the chat contract below is meant to make each one small; none exists yet.
- A per-connection `requireLinkedUser` (a chat sender must be a linked agent-orca user): future work. Today a chat user is never an agent-orca user and a run never has an invoker.
- Socket Mode (Slack) and the bot-account websocket (Mattermost direct messages).
- Mapping a sender to a signed-in agent-orca user. Identities in the UI API do not carry an email address, so there is nothing to match against. The core has a `Message.UserID` field for it; it would be informational only, because a run from a channel is a trigger actor's run and never a person's, and it would never grant a per-user connection.
- IMAP IDLE (push). A poll runs every `pollSeconds` (30 to 3600, default 60).
- Several replicas of a webhook endpoint share no replay memory (as for the trigger webhooks).
- Streaming or partial answers, attachments in replies, and editing a reply.
- Rejection notices to senders. A refused message is recorded and silently dropped, on purpose: telling a spoofed address "you are not allowed" is itself a way to harass someone.
- A per-connection daily budget (the tenant's budget applies).
