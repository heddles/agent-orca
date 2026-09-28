# Senior Programming Agent Demo

A senior software engineer for the agent-orca project. It answers architecture
questions from a curated engineering KnowledgeBase first, then drills into live
source via a **read-only GitHub MCP server** (stdio sidecar) when RAG results are
insufficient — back-filling what it reads into the KB with `_rag_ingest` so the
vector store grows richer with each session. When a task completes, it can also
**notify the team Slack channel** via the `_webhook_notify` builtin tool (a Slack
**incoming webhook** — no per-user token, shared channel, no data-residency
concerns).

## What this demo shows

- **MCP stdio sidecar**: the official GitHub MCP server (`ghcr.io/github/github-mcp-server`)
  runs as a read-only stdio sidecar injected into the agent pod. The model-router
  mounts the image via an image volume and fork/execs the binary at `args[0]`;
  credentials (`GITHUB_PERSONAL_ACCESS_TOKEN`) are injected through `envFrom`
  (secretKeyRef), never via `mcpConfig.auth` (which the admission webhook rejects
  for stdio transport).
- **Slack webhook (team notification)**: the agent posts results to a shared Slack
  channel via the `_webhook_notify` builtin tool, which POSTs to a Slack **incoming
  webhook URL** mounted into the model-router sidecar from the `webhook-notify`
  Secret. No per-user OAuth, no browser, no data residency concerns: the webhook URL
  is the only credential and is scoped to posting in its channel. Used to announce
  completions, PR links, or status updates — never to read Slack history.
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
| `webhook-notify` | Secret | Slack incoming-webhook URL (posts only), from `webhookNotify.url` value |
| `programming-agent-handbook` | ConfigMap | Engineering handbook ingested into the KB |
| `github-mcp` | MCPServer | Read-only GitHub MCP stdio sidecar (get_file_contents, search_code, list_commits, get_commit, list_branches, search_commits) |
| `github-mcp-<tool>` | Tool (6) | Auto-created child Tool CRs for each declared MCP tool |
| `_webhook_notify` | Builtin tool | Posts a message to Slack via the incoming webhook (`WEBHOOK_URL`) |
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
5. **Slack webhook**: create an **incoming webhook** in your Slack workspace (or a
   workspace app with incoming-webhooks enabled) for the channel you want the agent
   to notify. Copy its URL:
   ```bash
   export AGENT_ORCA_WEBHOOK_URL=https://hooks.slack.com/services/T/B/X
   ```
   Under Skaffold it is read from `AGENT_ORCA_WEBHOOK_URL` (never committed) →
   the `webhook-notify` Secret; the model-router mounts it as `WEBHOOK_URL` and
   uses it **only** for `_webhook_notify`. An incoming webhook can post to its channel
   but cannot read history, so there are no per-user tokens, no browser OAuth, and no
   data-residency concerns — any team member can read the posted messages.

## Deploy / Remove

```bash
# Skaffold (Slack webhook URL read from env var, never committed)
export AGENT_ORCA_GITHUB_TOKEN=ghp_YOUR_TOKEN
export AGENT_ORCA_WEBHOOK_URL=https://hooks.slack.com/services/T/B/X
skaffold run -p demo-programming-agent

# or Helm
helm install programming-agent charts/demos/programming-agent -n agent-orca-system \
  --set githubToken=ghp_YOUR_TOKEN \
  --set webhookNotify.url=https://hooks.slack.com/services/T/B/X

# Wait for the KB to ingest and the warm pod to come up:
kubectl get knowledgebase agent-orca-codebase -n agent-orca-system -w                       # → Ready
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

Expected: `list_commits` on `heddles/agent-orca`, then
`get_commit` for detail on interesting entries.

**Scenario 4 — Notify the team**

> `Post a one-line summary of this repo's architecture to Slack.`

Expected: `_rag_search` (handbook) + `search_code` for context, then `_webhook_notify`
to POST the summary to the configured Slack channel via the incoming webhook. No
per-user token is involved — the webhook URL only allows posting to its channel.

## Audit trail

```bash
# Each chat turn = one AgentRun; inspect tool calls and spend
kubectl get agentrun -n agent-orca-system \
  -l agentorca.io/deployment=senior-programmer-chat \
  --sort-by=.metadata.creationTimestamp

# KB ingestion status
kubectl describe knowledgebase agent-orca-codebase -n agent-orca-system

# MCP server tool readiness
kubectl get mcpserver github-mcp -n agent-orca-system -o wide
kubectl get tool -n agent-orca-system -l agentorca.io/managed-by=mcpserver
```

## Notes

- The agent image is the persona-free reference
  `ghcr.io/agentorca/agent-orca/openai-reference:latest`; the model-router injects the
  system prompt, tools, history, and built-in tool resolution (`_done`, `_fail`,
  `_clarify`, `_spawn`, `_rag_search`, `_rag_ingest`, `_webhook_notify`, …), so the
  image only ships `input → model-router → output`.
- The GitHub MCP runs **read-only** (`--read-only --tools <allow-list>`), so even
  though the `repos` toolset includes write tools, they cannot mutate anything.
- Slack is used **outbound only** via the incoming webhook (`_webhook_notify`), mounted
  as `WEBHOOK_URL` into the model-router sidecar from the `webhook-notify`
  Secret. The webhook URL can post to its channel but cannot read Slack history, so
  there are no per-user tokens and no data-residency concerns (any team member can
  read the posted messages).
- To point the agent at a different GitHub repo, edit the system prompt's repo
  (`heddles/agent-orca`) and the GitHub PAT's permissions accordingly.
- To point the agent at a different Slack channel, create another incoming webhook in
  that channel and update `webhookNotify.url`.
