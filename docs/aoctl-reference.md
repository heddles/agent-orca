# aoctl CLI Reference

`aoctl` is the command-line client for agent-orca. It covers three surfaces:
authentication/login, task management (External Task API), agent discovery
(ACP API), and admin tenant lifecycle management.

## Installation

```bash
make aoctl                   # builds bin/aoctl
go install ./cmd/aoctl       # or install from any checked-out tree
```

## Global flags

These flags are available on every subcommand:

| Flag | Env var | Default | Description |
|---|---|---|---|
| `--endpoint` | `AOCTL_ENDPOINT` | `http://localhost:8084` | External Task API URL |
| `--acp-endpoint` | `AOCTL_ACP_ENDPOINT` | `http://localhost:8000` | ACP API URL |
| `--token` | — | saved config | Bearer token (overrides cached token) |
| `--timeout` | — | `30s` | HTTP timeout |
| `--insecure` | — | `false` | Skip TLS verification (local dev only) |
| `--config-dir` | `AOCTL_CONFIG_DIR` | `~/.aoctl` | Override config directory |

`aoctl` caches your token (and, for OIDC, the refresh material) in
`~/.aoctl/config.json` (mode `0600`). Subsequent commands pick it up
automatically.

## Commands

### `aoctl login`

Authenticate and cache a bearer token. Two auth methods are supported:

| Method | Flag | How it works |
|---|---|---|
| `oauth` | `--auth-method oauth` | OAuth2 `client_credentials` exchange at `POST /oauth/token` (agent-orca-issued tenant JWT). Needs `--client-id` + `--client-secret`. |
| `oidc`  | `--auth-method oidc`  | OIDC authorization-code flow: `aoctl` opens your browser at the IdP, catches the loopback redirect on `--redirect-uri`, exchanges the code for an id_token (PKCE), and uses the id_token directly as the bearer token. The session is refreshed automatically using the refresh token. Needs `--issuer-url` + `--client-id`; `--client-secret` is optional (public clients). |

`aoctl login` (with no method and no credentials) presents an **interactive
selection menu** — the CLI analogue of the UI's `/oauth/login` picker — so you
can choose OAuth or OIDC and be prompted for the fields you haven't supplied.

```bash
# OAuth (non-interactive)
aoctl login \
  --endpoint http://localhost:8084 \
  --client-id acme-client \
  --client-secret secret123

# OIDC (non-interactive; opens your browser)
aoctl login \
  --endpoint http://localhost:8084 \
  --auth-method oidc \
  --issuer-url https://accounts.google.com \
  --client-id <your-oauth-web-client-id> \
  --client-secret <your-oauth-client-secret>   # omit for a public/PKCE client

# Interactive picker (no flags)
aoctl login
```

The id_token obtained via OIDC is accepted directly by the External/ACP APIs as a
federated bearer token (verified via the issuer's JWKS by
`TenantConfig.spec.federated`). This requires a `federated` `TenantConfig` whose
`issuerURL` + `clientID` match the IdP you sign in with — see
[OIDC Login](oauth-login.md). The OIDC config (`issuerURL`, `clientID`,
`clientSecret`, `redirectURI`) is cached alongside the token so sessions refresh
without re-prompting.

> **--no-browser** prints the authorization URL instead of opening a browser
> (useful for SSH/headless sessions). **--redirect-uri** defaults to
> `http://127.0.0.1:8765/callback` (loopback only) — register the same value
> with your IdP.

For cluster-internal use, you can pass a Kubernetes ServiceAccount token
directly with `--token` instead of running `login`.

### `aoctl tasks`

Manage tasks via the External Task API (port 8084).

| Command | Description |
|---|---|
| `aoctl tasks submit --agent <name> --input <text> [--stream] [--timeout 5m]` | Submit a task (`POST /v1/tasks`); optionally stream SSE events |
| `aoctl tasks ls [--agent <name>] [--status <status>]` | List tasks (`GET /v1/tasks`) |
| `aoctl tasks get <id>` | Get a task by ID (`GET /v1/tasks/{id}`) |
| `aoctl tasks cancel <id>` | Cancel a task (`POST /v1/tasks/{id}/cancel`) |
| `aoctl tasks wait <id>` | Stream a task to completion (SSE) |

```bash
# Submit + watch
aoctl tasks submit --agent support-bot --input "How do I reset my password?" --stream

# Or poll
TID=$(aoctl tasks submit --agent support-bot --input "hi" | jq -r .id)
aoctl tasks wait $TID
```

### `aoctl agents`

Discover agents and launch runs via the ACP API (port 8000).

| Command | Description |
|---|---|
| `aoctl agents list` | List agents available to your tenant (`GET /agents`) |
| `aoctl agents describe <name>` | Show the full manifest for an agent (`GET /agents/{name}`) |
| `aoctl agents run <name> --input <text> [--session-id <sid>]` | Launch a run (`POST /agents/{name}/run`) |

```bash
aoctl agents list
aoctl agents describe support-bot
aoctl agents run support-bot --input "How do I reset my password?"
```

The `--session-id` flag enables conversation continuity — pass the same session
ID across multiple `run` calls to chain conversation context.

### `aoctl admin tenants`

Manage tenants via the Admin API (`POST /admin/*` on port 8084). **Requires a
Kubernetes ServiceAccount bearer token** — not an OAuth2 client_credentials
JWT. Obtain one via:

```bash
kubectl create token agentorca-admin -n agent-orca-system
```

| Command | Description |
|---|---|
| `aoctl admin tenants list` | List all tenants (`GET /admin/tenants`) |
| `aoctl admin tenants get <name>` | Get a tenant by name (`GET /admin/tenants/{name}`) |
| `aoctl admin tenants create <name> --namespace <ns> --client-id <id> [--allowed-agents a,b] [--rpm 60] [--concurrent 10] [--budget 100.00]` | Create a tenant (`POST /admin/tenants`) |
| `aoctl admin tenants rotate-secret <name>` | Rotate a tenant's client secret (`POST /admin/tenants/{name}/rotate-secret`) |
| `aoctl admin tenants delete <name>` | Delete a tenant (`DELETE /admin/tenants/{name}`) |

```bash
# Create a tenant with rate limits and budget
aoctl admin tenants create acme \
  --namespace tenant-acme \
  --client-id acme-client \
  --rpm 60 \
  --concurrent 10 \
  --budget 100.00

# Rotate the client secret
aoctl admin tenants rotate-secret acme

# Delete a tenant
aoctl admin tenants delete acme
```

When creating a tenant, the response includes the one-time `clientSecret` —
save it immediately; it is not retrievable afterwards.

## Using aoctl with Terraform

For infrastructure-as-code tenant provisioning, you can use either `aoctl admin
tenants create` or the [Terraform example](../examples/terraform/agentorca_tenant.tf).
Both produce identical Kubernetes resources (`TenantConfig` + client-secret
`Secret`).
