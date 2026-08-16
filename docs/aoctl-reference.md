# aoctl CLI Reference

`aoctl` is the command-line client for agent-orc. It covers three surfaces:
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

`aoctl` caches your token in `~/.aoctl/config.json`. Subsequent commands pick
it up automatically.

## Commands

### `aoctl login`

Authenticate and cache a bearer token.

```bash
aoctl login \
  --endpoint http://localhost:8084 \
  --client-id acme-client \
  --client-secret secret123
```

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
kubectl create token agentorc-admin -n agent-orc-system
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
tenants create` or the [Terraform example](../examples/terraform/agentorc_tenant.tf).
Both produce identical Kubernetes resources (`TenantConfig` + client-secret
`Secret`).
