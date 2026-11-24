# Inbound channels

An inbound channel lets people (or systems) start a run of one **published** agent from somewhere other than the UI, and gets the result back in the same conversation. Email is the first channel (see [integrations.md](integrations.md#email)); chat platforms are meant to be the next ones, and the core is built so that none of them is a special case.

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

## Not built

- Chat platform adapters. The core is ready for them (push receivers, polling, replies with threads and size limits); none exists.
- Mapping a sender to a signed-in agent-orca user. Identities in the UI API do not carry an email address, so there is nothing to match against. The core has a `Message.UserID` field for it; it would be informational only, because a run from a channel is a trigger actor's run and never a person's, and it would never grant a per-user connection.
- IMAP IDLE (push). A poll runs every `pollSeconds` (30 to 3600, default 60).
- Several replicas of a webhook endpoint share no replay memory (as for the trigger webhooks).
- Streaming or partial answers, attachments in replies, and editing a reply.
- Rejection notices to senders. A refused message is recorded and silently dropped, on purpose: telling a spoofed address "you are not allowed" is itself a way to harass someone.
- A per-connection daily budget (the tenant's budget applies).
