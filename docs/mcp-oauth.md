# Per-user OAuth for MCP servers ("Connect your account")

A business user wants an agent to read **their own** GitHub, Google Drive or Slack. Until now the only
way was one shared token in a Secret: one identity for everyone who uses the agent. This page describes
the connection model that lets a person connect their own account.

Status: the `maintainer` mode is built. The `invoker` mode (the account of whoever starts the run) and the
lifecycle jobs (idle expiry, offboarding hook, key rotation job, metrics) are not built yet; see
[What is not built](#what-is-not-built). The design is in `docs/proposals/mcp-user-oauth.md` on the
`docs/proposals-triggers-oauth` branch.

## The model

`MCPServer.spec.auth.oauth.connectionMode` says whose credential a run uses.

| Mode | Credential | Who connects | Notes |
|---|---|---|---|
| `shared` (default) | One credential in a Secret (`credentials`) | An admin, once | Today's behaviour. Existing servers stay `shared`; nothing changes for them. |
| `maintainer` | The account of the user who **last published** the agent | The maintainer, from the UI | A delegation: the agent acts as its maintainer for everyone who runs it. The run page and the Connections page say so. |

`invoker` (the user who starts the run) is deliberately not accepted by the CRD yet.

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

* **`invoker` mode** (C4): the run identity from `created-by`, fail-closed rules for scheduled, external,
  child and workflow runs, the WaitingForInput connect card, the approval default for writes.
  `GET /api/me/connections/required` already exists and reports `required: []` until then.
* **Lifecycle** (C5): the 90-day idle expiry, an offboarding hook, the key-rotation job, the
  `agentorca_mcp_connections` and refresh-failure metrics. There is no user-lifecycle signal in the operator
  today, so offboarding is manual: an admin revokes the user's connections.
* A KMS-backed key, dynamic client registration, a per-app switch for the resource indicator, a provider
  account lookup for the account label, `MCPOAuthApp` as a CRD for GitOps.
