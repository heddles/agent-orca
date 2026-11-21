# Per-user OAuth for MCP servers ("Connect your account")

A business user wants an agent to read **their own** GitHub, Google Drive or Slack. Until now the only
way was one shared token in a Secret: one identity for everyone who uses the agent. This page describes
the connection model that lets a person connect their own account.

Status: the `maintainer` and `invoker` modes are built. The lifecycle jobs (idle expiry, offboarding hook,
key rotation job, metrics) are not built yet; see
[What is not built](#what-is-not-built). The design is in `docs/proposals/mcp-user-oauth.md` on the
`docs/proposals-triggers-oauth` branch.

## The model

`MCPServer.spec.auth.oauth.connectionMode` says whose credential a run uses.

| Mode | Credential | Who connects | Notes |
|---|---|---|---|
| `shared` (default) | One credential in a Secret (`credentials`) | An admin, once | Today's behaviour. Existing servers stay `shared`; nothing changes for them. |
| `maintainer` | The account of the user who **last published** the agent | The maintainer, from the UI | A delegation: the agent acts as its maintainer for everyone who runs it. The run page and the Connections page say so. |

| `invoker` | The account of the user who **started the run** (the run's `agentorca.io/created-by`) | Each user, for themselves, from the UI | The agent acts as whoever runs it. A run with no person behind it has no account: it fails closed (see [Invoker mode](#invoker-mode)). |

A server the UI creates with an `auth.oauth` block and no `credentials` Secret becomes `maintainer`
by default. A server with `credentials`, and every server that already exists, stays `shared`.

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: MCPServer
metadata: {name: github, namespace: team-a}
spec:
  transport: http
  url: https://mcp.github.example/mcp
  allowedAgents: [reporter]
  auth:
    oauth:
      connectionMode: maintainer
      appRef: github            # the OAuth app an admin registered for this server
      userScopes: ["repo:read"] # the most a user may grant; a connection with more is refused
      # revokeOnDisconnect: true (default): ask the provider to revoke on disconnect (RFC 7009)
```

CEL rules: `shared` requires `credentials`; a per-user mode requires `appRef` and takes no `credentials`,
`refreshTokenSecretRef` or `accessTokenSecretRef`. OAuth needs `http` or `sse` transport.

## What is stored, and where

Tokens live **encrypted in PostgreSQL**, not in Kubernetes Secrets, not in the CRD, not in a pod.
The feature needs `database.dsn` and a key; without either the endpoints answer 503 and the UI says so.

* `mcp_connections`: one row per tenant, user and server (migration `0020`): the refresh token and the
  latest access token as ciphertext, the granted scopes, the account label, a status
  (`active`, `needs_reauth`, `revoked`), the server UID and `last_used_at`.
* `mcp_oauth_apps`: the OAuth app an admin registered, per tenant: client id, **encrypted** client secret,
  allowed scopes.
* `mcp_oauth_flows`: a pending authorization request for 5 minutes, keyed by the SHA-256 of the state.
  It is in PostgreSQL so any operator replica can finish a flow another one started.

All three tables carry `tenant_id` with the forced row-level security of `0010`, and every query on a
connection also filters on `user_id`: RLS isolates tenants, the predicate isolates users inside one. A
tenant admin can see that a user is connected, never the token. The connection is bound to the MCPServer
**UID**: a server deleted and recreated under the same name does not inherit credentials.

### Encryption

Envelope encryption (`internal/connections/crypto.go`): each value gets a fresh AES-256-GCM data key,
wrapped by a key-encryption key (KEK). The additional authenticated data binds the ciphertext to its purpose
and its owner (tenant, user, server UID; for an app, tenant, namespace and appRef), so a row copied to
another tenant or user does not decrypt. The row stores the `key_id` of the KEK that wrapped it, so two keys
are valid while a rotation is in progress (`Keyring.Rewrap` re-wraps a value; the background job that runs
it is not built yet).

The KEK comes from a Kubernetes Secret you create, named in the chart value
`database.connectionKeySecret`:

```bash
kubectl -n agent-orca-system create secret generic mcp-connection-keys \
  --from-literal=2026-11="$(openssl rand -base64 32)" \
  --from-literal=active=2026-11
helm upgrade ... --set database.connectionKeySecret=mcp-connection-keys \
  --set operator.connections.redirectURI=https://agents.example.com/api/connections/callback
```

Each entry is a key: the entry name is the key id, the value is 32 random bytes as base64 or hex. The entry
`active` names the key that encrypts new data (optional with one key). To rotate: add a second key, point
`active` at it, re-wrap, then remove the old one. The Secret is mounted into the operator only, read-only at
`/etc/agentorca/connection-keys`.

**A stolen database dump is ciphertext only if the KEK is kept apart. Database access plus Secret access is
a full compromise.** A KMS-held key is preferable; the `Keyring` has a small interface for it, but only the
Secret-backed keyring is implemented.

## Invoker mode

With `connectionMode: invoker` the run acts as the person who started it. The operator reads that person
from the **AgentRun object** (`agentorca.io/created-by`), never from the request body, and uses only that
user's connection. There is no fallback to the maintainer, to a shared credential or to anyone else.

The token endpoint answers `409 connection_required` (and uses no connection at all) when the run has no
person behind it:

| Run | Why it has no invoker |
|---|---|
| `created-by` empty | Nothing recorded (an unattributed run). |
| `schedule:` (a schedule), `trigger:` (an external trigger), `widget:` (a chat widget key), `machine:` or `anonymous` | Not a person. A service account or client-credentials caller records no user. |
| External API task, workflow step, evaluation run (label `agentorca.io/external-task`, `agentorca.io/workflow`, `agentorca.io/eval-arm`) | Started by a system, even if a user is named on it. |
| A child run (`parentRunRef`) with no propagation | See below. |

**Agent as tool.** A child run keeps the ORIGINAL invoker. When a pod creates a child run
(`POST /agentrun/{ns}`) the operator drops any `created-by` the body carried and, only when the caller is the
ServiceAccount of the parent run's own agent and the parent has an invoker, copies the parent's `created-by`
and sets `agentorca.io/invoker-from-run` to the parent's name. The token endpoint accepts a child only when
that annotation names its `parentRunRef`. The callee's own `allowedAgents`, "agent uses the server" and tenant
checks still apply. A child whose parent has no invoker (a scheduled run) has none either.

**Pausing for a connection.** If a tool call is refused with `connection_required` (for example the user
disconnected during the run), the model gets a tool error it can relay ("the github account is not
connected ... do not retry") and the run moves to `WaitingForInput` with the card "This agent needs your
github account. Connect it under Connections, then answer here to continue." The run page and My work show
a Connect link next to it. Answering resumes the run through the ordinary clarification path; the
continuation keeps the original `created-by`. A child run cannot ask a human, so it returns the error to its
caller instead. Before a run starts, `GET /api/me/connections/required?agent=ns/name` lists the invoker
servers the caller has not connected, and the run form shows "Connect X to run this agent" and stays blocked.

**Approval.** A tool of an invoker server needs an approver unless the agent's GuardrailPolicy says what the
tool's class needs. MCP discovery carries no side-effect class, so most such tools are unclassified and are
gated; set `toolApproval.unclassified: none` (or `write: none` for tools classified `write`) to let them run.
`read` tools are never gated by this default. See [tool-side-effects.md](tool-side-effects.md).

**Publishing.** An agent that uses an invoker server needs no publisher connection and records no
`publishedBy`. The run form says `github as alice (invoker)` for a connected user, and the run's trace has an
`mcpConnection` event with the server and identity label (once per server per run). The token endpoint logs
the run, server, mode and identity, never the token.

**Warm pods.** A warm pod has no run when it starts, so it connects only its non-per-user servers then; the
per-user servers (maintainer and invoker) connect when the pod is claimed, with the claimed run's token.

## Setting it up (admin)

1. Set the chart values above (`database.connectionKeySecret`, `operator.connections.redirectURI`). The
   redirect URI is one fixed URL per deployment, the UI's public origin plus `/api/connections/callback`; it
   is never taken from a request.
2. Create the MCPServer with `connectionMode: maintainer` and an `appRef`.
3. At the provider, create an OAuth app and give it that redirect URI. **Dynamic client registration
   (RFC 7591) is not used**: apps are registered by hand.
4. In the UI, Settings, Connections, open the server, and enter the client id, the client secret
   (write-only: it is never shown again, and an empty field on a later update keeps the stored one) and the
   scopes users may grant. Or `PUT /api/mcp-oauth-apps/{ns}/{server}`.

The server's authorization service is found through the server's own metadata (RFC 9728 and RFC 8414,
`internal/oauth`). The operator **refuses** a server that does not support PKCE `S256`, and refuses
authorization or token endpoints that are not `https` (a dev-only value
`operator.connections.allowInsecureOAuth` lifts the second check).

The **resource indicator** (RFC 8707) is sent on the authorization, code and refresh requests: the
canonical URL of the MCP server. The MCP authorization specification requires it; a provider that rejects the
parameter cannot be used with this mode until a per-app switch exists (not built).

## Connecting (user)

Connections in the sidebar lists the servers that act as a person, with Connect and Disconnect. The flow is
authorization code with PKCE:

1. `POST /api/connections/{ns}/{server}/start` checks the role, that the server is the caller's tenant's (or
   platform-shared) and that the app is registered, discovers the server, creates a 256-bit `state`, a PKCE
   verifier and challenge, stores the pending flow for 5 minutes, and sets the state in a
   `__Host-` cookie (`HttpOnly; Secure; SameSite=Lax`; the prefix is dropped on plain-HTTP dev).
2. The provider redirects to `GET /api/connections/callback`. The operator, in order: reads the pending flow
   by the state's hash and **deletes it in the same statement** (single use; a state that fails any later
   check is burned); requires the cookie to equal the state; requires the **signed-in session's** tenant
   and user to equal the stored ones (a state minted for Alice is rejected for Bob's session); checks the
   server UID; checks the RFC 9207 `iss` (and requires it when the server announces support); exchanges the
   code; refuses a grant with no refresh token or with scopes beyond `userScopes`/the app's list (and asks
   the provider to revoke what it refused).
3. It redirects to `/connections?status=connected|denied|expired|scopes|failed`. No code, state or token is in
   that URL, the response carries `Referrer-Policy: no-referrer`, and the provider's own error text is never
   echoed.

`account_label` shows the signed-in agent-orca user, not a lookup of the provider account.

## Publishing

* A builder who publishes an agent that uses a `maintainer` server **must have an active connection of
  their own** to it. Otherwise publish answers 409 with the servers to connect.
* On publish, the publisher is copied into `Agent.status.publishedBy` (and `publishedAt`). It is not looked
  up at runtime. Republishing as someone else needs their connection and makes them the maintainer; a refused
  republish changes nothing.
* A draft agent that nobody has published has no maintainer, so its runs cannot use a `maintainer` server
  (`connection_unavailable`). Publish it first.

## How the token reaches a tool call

The model chooses a tool; the router makes the MCP request. The model sees tool names, arguments and
results, never headers.

1. The pod has **no credential for a per-user server**: the controllers mount no Secret for it
   (`podbuilder.ResolveToolSecretVolumes`, test `PerUserOAuthMountsNothing`) and put no token in the router
   config, only `userConnection.server`.
2. When the router needs the server (connect, tools/list, every tool call) it calls the operator's internal
   API: `POST /agentrun/{ns}/{run}/mcp-token {"server": "<MCPServer>"}`. It authenticates exactly like the
   budget and approval calls: the pod's projected ServiceAccount token, audience `agentorca/model-router`,
   checked with TokenReview. On top of the namespace check those use, the caller must be the
   ServiceAccount **of the run's own agent**.
3. The operator resolves the identity from the objects, not the request. The body names only the server.
   It loads the AgentRun (must not be in a terminal phase) and its Agent, takes the **maintainer from
   `Agent.status.publishedBy`** and the tenant from the Agent's tenant label (the run's and the server's
   tenant labels must agree), requires the server to be in `maintainer` mode, to list the agent in
   `allowedAgents`, and to be used by one of the agent's tools. The store call is scoped to that tenant, so
   row-level security enforces it a second time.
4. It locks the connection row (`SELECT ... FOR UPDATE`), refreshes if the access token expires within 60
   seconds (so two runs never both spend a rotating refresh token), stores the new tokens encrypted, and
   returns **only** `{accessToken, expiresAt, identity}`. The refresh token never leaves the operator.
5. The router keeps the token in memory until 30 seconds before its expiry, puts it in
   `Authorization: Bearer ...` on the HTTP request, drops it on a 401, and asks again on expiry.

Nothing else mints or carries the token. The existing `shared` path (mounted Secret, in-memory refresh) is
unchanged.

## Redaction

There is no single writer of traces, tool results and logs, so the router registers each token it receives
with `internal/redact` and masks it (exact, JSON-escaped and URL-escaped forms) at every point text leaves
the process:

| Writer | Where |
|---|---|
| Tool results and tool errors to the model (and so to traces, which are built from them) | `mcp.Client.Call`, `FetchResource` |
| Every stored trace event | `Router.emitTraceEvent` |
| Every SSE token and trace event streamed or replayed | `TokenBroadcaster.Send` |
| Router logs (`slog`) and the standard `log` package | `redact.Handler`, `redact.Writer` in `cmd/model-router` |
| Errors from the token call | the router relays only a code and a fixed message |

Tests: `internal/redact`, and `internal/router/mcp_user_token_test.go` (token in the request header on every
request; absent from the tool result of a server that echoes it, from stored trace events, from SSE
tokens and from logs). `Secret` in `internal/connections` redacts itself under `%v`, JSON and `slog`.
Residual risk: a token split across two writes is not caught, and a registered token shorter than 8
bytes is ignored. An MCP server that echoes its credential is a trust issue for the admin who registered it.

## Roles

| Action | Role |
|---|---|
| List your own connections | viewer |
| Connect and disconnect your own account, "what does this agent need" | runner |
| Register or rotate the OAuth app, set `connectionMode` and `userScopes`, see who is connected, revoke anyone's connection | admin (MCPServer writes are admin already) |
| Publish an agent that uses a maintainer server | builder, with an active connection of their own |
| Run an agent that uses an invoker server | runner, with an active connection of their own |

A request with no signed-in **user** (a service account, client credentials, authentication disabled) cannot
connect: it answers 403.

## Audit

Rows in `audit_events`, kind `MCPConnection`, name = the server, summary has the account label and scopes,
never a token: `connect`, `disconnect` (and whether the provider revoked), `revoke` (an admin removing
another user's), `reauth_required` (a refresh failed with `invalid_grant`; written as
`system:mcp-connections`), `app_register`, `app_rotate`. A change of `connectionMode` is an ordinary
MCPServer `update` row. Use is not audited per call: `last_used_at` is updated at most every 5 minutes.

## Failure behaviour

| Situation | What happens |
|---|---|
| The maintainer never connected, or the server was recreated | The tool call fails with `connection_required`. |
| A refresh fails with `invalid_grant` | The status becomes `needs_reauth` and the call fails with a message the model can relay without secrets ("the connection needs to be renewed by its owner"). The owner sees "Needs renewing" on Connections. |
| The maintainer disconnected | `connection_required`. The run never falls back to `shared` or to another user. |
| Invoker mode, the user has not connected, or the run has no person behind it | `connection_required`; see [Invoker mode](#invoker-mode) for the pause and the connect card. |
| The provider is down | The call fails with `connection_unavailable`; the status does not change. |

When no token can be obtained while the run's router starts, that server's tools are not discovered (the
router logs a warning with the code, no secret), so the model sees no tools from it rather than failing calls.

## API

See `internal/apiserver/schemas/openapi-ui.yaml`, tag `connections`: `GET /api/connections`,
`POST /api/connections/{ns}/{server}/start`, `GET /api/connections/callback`,
`DELETE /api/connections/{ns}/{server}[?user=]`, `GET /api/mcpservers/{ns}/{name}/connections`,
`GET|PUT /api/mcp-oauth-apps/{ns}/{server}`, `GET /api/me/connections/required?agent={ns}/{name}`, and the
internal `POST /agentrun/{ns}/{run}/mcp-token`. `GET /api/mcpservers` now reports `connectionMode` and
`appRef`.

## What is not built

* **Invoker mode, cut parts** (C4): a tool call can only be refused with `connection_required` for tools the
  router already discovered. When the invoker has no connection (or the run is scheduled) when the run starts,
  that server's tools are not discovered, so the model sees no tools from it instead of a `connection_required`
  error; the run form blocks a user who has not connected, but a scheduled or trigger run simply lacks the
  tools. The MCP tool's side-effect class is not carried through discovery, so the approval default
  applies to the whole server rather than to its write tools only.
* **Lifecycle** (C5): the 90-day idle expiry, an offboarding hook, the key-rotation job, the
  `agentorca_mcp_connections` and refresh-failure metrics. There is no user-lifecycle signal in the operator
  today, so offboarding is manual: an admin revokes the user's connections.
* A KMS-backed key, dynamic client registration, a per-app switch for the resource indicator, a provider
  account lookup for the account label, `MCPOAuthApp` as a CRD for GitOps.
