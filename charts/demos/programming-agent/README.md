# Senior Programming Agent Demo

A senior software engineer for the agent-orca project. It answers architecture
questions from a curated engineering KnowledgeBase first, then drills into live
source via a **read-only GitHub MCP server** (stdio sidecar) when RAG results are
insufficient — back-filling what it reads into the KB with `_rag_ingest` so the
vector store grows richer with each session. It can also consult team Slack history
(messages, channels, users) via the **Slack MCP server** (`https://mcp.slack.com/mcp`,
Streamable HTTP) before grounding its answers in live source.

## What this demo shows

- **MCP stdio sidecar**: the official GitHub MCP server (`ghcr.io/github/github-mcp-server`)
  runs as a read-only stdio sidecar injected into the agent pod. The model-router
  mounts the image via an image volume and fork/execs the binary at `args[0]`;
  credentials (`GITHUB_PERSONAL_ACCESS_TOKEN`) are injected through `envFrom`
  (secretKeyRef), never via `mcpConfig.auth` (which the admission webhook rejects
  for stdio transport).
- **Slack MCP (HTTP + bearer token)**: Slack's publicly-hosted MCP server is
  reached over Streamable HTTP at `https://mcp.slack.com/mcp`. Unlike the GitHub
  stdio sidecar, no binary is injected — the model-router connects directly and
  authenticates with a Slack user token (xoxp-) mounted from the `slack-mcp-token`
  Secret. The token is sent as `Authorization: Bearer <xoxp-...>` (the `bearerToken`
  auth), resolved to a file and injected as a header on every request. A read-
  only scope set is recommended so the agent can search but not post back.
- **RAG + live code search**: the agent starts with `_rag_search` against the
  `agent-orca-codebase` KnowledgeBase (handbook ingested from a ConfigMap), then
  falls back to `search_code` / `get_file_contents` for fresh source, then
  `_rag_ingest`s the read content back into the vector store.
- **Warm-pool chat agent**: `senior-programmer-chat` AgentDeployment with
  `warmPoolSize: 1`, `inputMode: http`, and session checkpointing/resume.
- **Scoped persona**: the system prompt enforces an agent-orca focus and a
  "handbook first → live code second → back-fill" workflow.

## Resources

| Name | Kind | Purpose |
|---|---|---|
| `github-mcp-token` | Secret | GitHub PAT (repo read scope), from `githubToken` value |
| `slack-mcp-token` | Secret | Slack user token (xoxp-), read/search scopes, from `slackToken` value |
| `programming-agent-handbook` | ConfigMap | Engineering handbook ingested into the KB |
| `github-mcp` | MCPServer | Read-only GitHub MCP stdio sidecar (get_file_contents, search_code, list_commits, get_commit, list_branches, search_commits) |
| `github-mcp-<tool>` | Tool (6) | Auto-created child Tool CRs for each declared MCP tool |
| `slack-mcp` | MCPServer | Slack MCP over HTTP (https://mcp.slack.com/mcp), bearer-token auth; tools auto-discovered at runtime (search, read channels/threads, list users/channels, emoji, canvases, lists) |
| `agent-orca-codebase` | KnowledgeBase | Qdrant vector store of the handbook; embedded via `ollama-embed` |
| `senior-programmer` | Agent | Senior-engineer persona, openai-reference, http warm-pool |
| `senior-programmer-chat` | AgentDeployment | Chat endpoint, warm pool 1 |

## Prerequisites

1. The agent-orca operator + model-providers running (start the dev loop first):
   ```bash
   skaffold dev        # builds + kind-loads openai-reference:latest, deploys operator + model-providers + UI
   ```
2. The `default` ModelSelector is deployed by the dev profile.
3. **Embeddings**: the KnowledgeBase uses the `ollama-embed` ModelSelector, which
   requires Ollama in the cluster. Enable it:
   - In `charts/model-providers/values.yaml`, set `ollama-embed.enabled: true` (and
     `ollama-nomic-embed-text.enabled: true`).
   - Apply the Ollama cluster manifest from `charts/model-providers/` and run the
     `ollama-pull-model` Job so `nomic-embed-text` is pre-pulled.
   (Equivalent prerequisite shared with the `demo-llm-research`, `demo-financial-analysis`,
   and `demo-soc-triage` demos.)
4. **GitHub PAT**: a token with the `repo` (read) scope. Under Skaffold it is read
   from the `AGENT_ORCA_GITHUB_TOKEN` env var (never committed).
5. **Slack user token**: a `xoxp-...` token from a Slack app installed to your
   workspace, with the read/search scopes below (see `values.yaml`). Under Skaffold
   it is read from the `AGENT_ORCA_SLACK_TOKEN` env var (never committed). The token
   is sent as `Authorization: Bearer <token>` to the public Slack MCP endpoint.

   Required read scopes (safe for a demo — search + read + list; no posting):
   ```
   search:read.public search:read.private search:read.mpim search:read.im
   search:read.files files:read emoji:read search:read.users
   channels:history groups:history mpim:history im:history
   channels:read groups:read mpim:read im:read
   users:read users:read.email
   ```
   These map to the Slack MCP server's search/read/list tools (see
   <https://docs.slack.dev/ai/slack-mcp-server#oauth-scopes>). Add `chat:write` /
   `reactions:write` / `channels:write` / `im:write` / `mpim:write` only if you want
   the agent to post back to Slack.

## Deploy / Remove

```bash
# Skaffold (tokens read from env vars, never committed)
export AGENT_ORCA_GITHUB_TOKEN=ghp_YOUR_TOKEN
export AGENT_ORCA_SLACK_TOKEN=xoxp_YOUR_TOKEN
skaffold run -p demo-programming-agent

# or Helm
helm install programming-agent charts/demos/programming-agent -n agent-orca-system \
  --set githubToken=ghp_YOUR_TOKEN \
  --set slackToken=xoxp_YOUR_TOKEN

# Wait for the KnowledgeBase to ingest the handbook and the warm pod to come up:
kubectl get knowledgebase agent-orca-codebase -n agent-orca-system -w      # → Ready
kubectl get pods -n agent-orca-system -l agentorca.io/deployment=senior-programmer-chat -w

# Remove
skaffold delete -p demo-programming-agent      # or:  helm uninstall programming-agent -n agent-orca-system
```

## Running the demo

Chat endpoint:

```
POST /api/deployments/agent-orca-system/senior-programmer-chat/execute
```

**Scenario 1 — Architecture overview**

> `How does the model-router sidecar intercept tool calls?`

Expected: the agent calls `_rag_search` against the handbook (architecture.md),
cites `internal/controller/agentdeployment_controller.go` and the model-router
contract, then returns a concise explanation.

**Scenario 2 — Live code lookup**

> `Show me the MCPServer reconcile loop and list the industries it handles.`

Expected: `_rag_search` first (handbook), then `search_code` for
`internal/controller/mcpserver_controller.go`, then `get_file_contents`
for the file, then `_rag_ingest` to back-fill. If the local repo differs from the
indexed handbook, the live GitHub read takes precedence.

**Scenario 3 — Recent changes**

> `What changed in the last 5 commits to the agent-orca repo?`

Expected: `list_commits` on `floppyfish14/agent-orca`, then
`get_commit` for detail on interesting entries.

**Scenario 4 — Team Slack context**

> `What did we decide about the MCPServer auth field in the last discussions?`

Expected: `_rag_search` (handbook) + `search_code` for context, then the Slack MCP
(`slack-mcp`) to search recent channel history. The agent reads relevant
messages/threads and surfaces prior decisions, grounding its answer in the actual
discussion. The token's read/search scopes let it search and read but not post.

## Audit trail

```bash
# Each chat turn = one AgentRun; inspect tool calls and spend
kubectl get agentrun -n agent-orca-system \
  -l agentorca.io/deployment=senior-programmer-chat \
  --sort-by=.metadata.creationTimestamp

# KB ingestion status
kubectl describe knowledgebase agent-orca-codebase -n agent-orca-system

# MCP server tool readiness
kubectl get mcpserver github-mcp,slack-mcp -n agent-orca-system -o wide
kubectl get tool -n agent-orca-system -l agentorca.io/managed-by=mcpserver
```

## Notes

- The agent image is the persona-free reference
  `ghcr.io/agentorca/agent-orca/openai-reference:latest`; the model-router injects the
  system prompt, tools, history, and built-in tool resolution (`_done`, `_fail`,
  `_clarify`, `_spawn`, `_rag_search`, `_rag_ingest`, …), so the image only ships
  `input → model-router → output`.
- The GitHub MCP runs **read-only** (`--read-only --tools <allow-list>`), so even
  though the `repos` toolset includes write tools, they cannot mutate anything.
- The Slack MCP is exercised over the public endpoint `https://mcp.slack.com/mcp`.
  The token is mounted as a file into the model-router sidecar (via the MCPServer's
  `auth.bearerToken`) and sent as `Authorization: Bearer <token>` — it is never
  logged or baked into the chart. Restrict the token to read/search scopes so the
  agent cannot post to Slack even if the LLM tries.
- To point the agent at a different GitHub repo, edit the system prompt's repo
  (`floppyfish14/agent-orca`) and the GitHub PAT's permissions accordingly.
- To point the agent at a different Slack workspace/app, install the app to that
  workspace and swap the `slackToken` value (scopes will re-validate against the new
  workspace).
