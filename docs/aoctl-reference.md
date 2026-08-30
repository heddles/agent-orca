# aoctl CLI Reference

`aoctl` is the command-line client for agent-orca. It covers five surfaces:
authentication/login, task management (External Task API), agent discovery
(ACP API), admin tenant lifecycle management, and the ACP stdio bridge (for
editor integration via Zed and other ACP-compatible editors).

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
| `--json` | `AOCTL_OUTPUT_FORMAT` | `text` | `json` for machine-readable output; `text` (default) for human-friendly tables |

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
| `aoctl tasks ls [--agent <name>] [--status <status>]` | List tasks (`GET /v1/tasks`); human table by default, `--json` for an array |
| `aoctl tasks get <id>` | Get a task by ID (`GET /v1/tasks/{id}`); JSON by default, `--json` is a no-op |
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
| `aoctl agents list` | List agents in a `NAME  NAMESPACE` table; `--json` for the full manifest array |
| `aoctl agents describe <name>` | Show a human-readable manifest (content types, tools, KBs, guardrails, schemas); `--json` for the full manifest |
| `aoctl agents run <name> --input <text> \| --file <path>` | Launch a run (`POST /agents/{name}/run`); see details below |

`aoctl agents list` shows one row per agent (`NAME` and `NAMESPACE`; the namespace
is the tenant's). `aoctl agents describe <name>` prints the same header line
followed by the agent's full details. Add `--json` (or `AOCTL_OUTPUT_FORMAT=json`)
to either command to emit the raw JSON instead of the formatted output.

```bash
aoctl agents list                       # human table
aoctl agents list --json                # machine-readable JSON
aoctl agents describe support-bot       # human-readable manifest
aoctl agents describe support-bot --json
```

#### `aoctl agents run`

Launches an ACP run against an agent. The agent name is the first positional
argument. Input can be provided three ways (checked in priority order):

1. **`--input <text>`** — inline text (most common for short prompts).
2. **`--file <path>`** — read input from a file (for long prompts or JSON).
3. **stdin** — when `--input` is omitted and stdin is not a terminal, content
   is read from the pipe. This lets you chain commands:

```bash
# Inline
aoctl agents run support-bot --input "How do I reset my password?"

# From a file
aoctl agents run support-bot --file prompt.txt

# Piped via stdin
echo "alert: 3000 db files exported to 100.72.1.228" \
  | aoctl agents run soc-enricher-agent

# Custom content type (e.g. for JSON input the agent expects)
aoctl agents run support-bot --input '{"q":"hello"}' --content-type application/json
```

**Flags:**

| Flag | Default | Description |
|---|---|---|
| `--input` | — | Task input as inline text |
| `--file` | — | Read task input from a file (mutually exclusive with `--input`) |
| `--session-id` | — | Session ID for conversation continuity — pass the same value across calls to chain context |
| `--content-type` | `text/plain` | MIME type for the message part (e.g. `text/plain`, `application/json`) |
| `--json` | off | Emit raw JSON response instead of a human-readable summary |

The `--input` and `--file` flags are **mutually exclusive**. If neither is
provided and stdin is a terminal, the command errors with a hint listing the
options.

The run is created asynchronously. By default the output is a human-readable
summary showing the run ID, status, and timestamps, plus suggestions for
polling. Add `--json` to get the raw `ACPRun` object. To follow output, poll
`GET /runs/{run_id}` on the ACP API (`--acp-endpoint`) or stream via
`GET /runs/{run_id}/events`.

The `--session-id` flag enables conversation continuity — pass the same session
ID across multiple `run` calls to chain conversation context:

```bash
SID=$(uuidgen)
aoctl agents run support-bot --input "Hello, I'm Matthew" --session-id "$SID"
aoctl agents run support-bot --input "Follow up on my previous question" --session-id "$SID"
```

### `aoctl admin tenants`

Manage tenants via the Admin API (`POST /admin/*` on port 8084). **Requires a
Kubernetes ServiceAccount bearer token** — not an OAuth2 client_credentials
JWT. Obtain one via:

```bash
kubectl create token agentorca-admin -n agent-orca-system
```

| Command | Description |
|---|---|
| `aoctl admin tenants list` | List all tenants in a `NAME  CLIENT ID  NAMESPACE` table; `--json` for the array |
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

### `aoctl acp`

The ACP bridge lets editors like Zed connect to your remotely-hosted
agent-orca agents. Zed cannot talk to agent-orca's HTTP ACP API directly, so
`aoctl` runs as a **stdio subprocess** that Zed launches and communicates with
over JSON-RPC 2.0. The bridge translates each JSON-RPC method into HTTP calls
against agent-orca's ACP API, then streams results back.

| Command | Description |
|---|---|
| `aoctl acp serve --agent <name>` | Run the stdio JSON-RPC bridge (invoked by the editor as a subprocess) |
| `aoctl acp setup --editor zed [--agent <name>]` | Write a Zed `agent_servers` entry that launches the bridge |

#### `aoctl acp serve`

`serve` is a long-running process. The editor (e.g. Zed) starts it as a
subprocess, sends JSON-RPC 2.0 requests on stdin, and reads responses +
server→client notifications on stdout. All logging goes to stderr so it never
corrupts the JSON-RPC stream. The process exits when stdin closes.

Because ACP v1 is single-agent-per-server, `--agent` pins which agent-orca
agent this bridge instance represents. The bearer token is read from the saved
config (`~/.aoctl/config.json`), so run `aoctl login` first.

```bash
# Foreground (for debugging or direct testing)
aoctl acp serve --agent support-bot

# Or let Zed manage the lifecycle by adding it to agent_servers (see below)
```

The bridge handles the core ACP methods: `initialize`, `session/new`,
`session/prompt` (streaming token chunks + plan/cancel), `session/cancel`, and
`session/close`. If the server has a state store, token chunks arrive via SSE;
otherwise the bridge transparently falls back to polling `GET /runs/{run_id}`.

#### `aoctl acp setup --editor zed`

`setup` writes a Zed `agent_servers` entry to `~/.config/zed/settings.json`
(or `$XDG_CONFIG_HOME/zed/settings.json` on Linux, `%APPDATA%\Zed\settings.json`
on Windows). If `--agent` is omitted, the available agents are listed and you
pick one interactively.

```bash
# Configure a specific agent
aoctl acp setup --editor zed --agent support-bot

# Interactive: lists agents and prompts
aoctl acp setup --editor zed
```

This adds an entry like:

```json
{
  "agent_servers": {
    "support-bot": {
      "type": "custom",
      "command": "aoctl",
      "args": ["acp", "serve", "--agent", "support-bot"],
      "env": {}
    }
  }
}
```

Zed auto-detects the settings change — no restart needed. Open the Agent Panel,
start a new thread, and select your agent from the list.

To configure a different ACP-compatible editor, check the ACP spec for the
subprocess launch protocol; extend `aoctl acp setup` with a new `--editor` case.

## Troubleshooting

### `Error: ... invalid character '<' looking for beginning of value`

This means `aoctl` got an **HTML** response where it expected JSON. It happens when
`--endpoint` points at the wrong surface — most commonly the **UI proxy on
`:8080`**, whose SPA returns `index.html` (HTML, `200`) for unknown paths like
`/v1/tasks`. The External Task API is on **`:8084`** and the ACP API on **`:8000`**.

aoctl now detects this and reports the actual status + `Content-Type` plus a hint:

```
Error: non-JSON response (HTTP 200, Content-Type "text/html; charset=utf-8") from
http://127.0.0.1:8080/v1/tasks; is --endpoint correct? the External Task API is
:8084 and the ACP API is :8000 (the UI proxy on :8080 serves HTML, not JSON)
```

Fix: point `--endpoint`/`--acp-endpoint` at the right port (or set
`AOCTL_ENDPOINT`/`AOCTL_ACP_ENDPOINT`). If you are using the committed `aoctl`
binary at the repo root, rebuild it (`make aoctl`) — it may predate the status +
content-type checks.

## Using aoctl with Terraform

For infrastructure-as-code tenant provisioning, you can use either `aoctl admin
tenants create` or the [Terraform example](../examples/terraform/agentorca_tenant.tf).
Both produce identical Kubernetes resources (`TenantConfig` + client-secret
`Secret`).
