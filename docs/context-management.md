# Context Management

This guide covers how agent-orca manages LLM context windows across turns and runs — including context compaction, warm-pool session chaining, and MCP auth.

## Overview

The model-router stores each run's conversation (the history before the current turn plus the turn in progress) and builds every LLM request from it. Context it injects per request — the agent's system prompt, built-in tool hints, long-term memory and hindsight recall — is rebuilt for each request and never stored, so it cannot pile up in the history.

When a request would carry more than **80%** of the selected provider's `contextWindow` (minus the tool definitions, which count toward the window on every request), the router **compacts the stored conversation once**, before sending, and keeps the result. Later turns build on the compacted history until it grows past the threshold again.

| Setting | Default | Description |
|---------|---------|-------------|
| `contextCompactionThreshold` | `0.8` | Fraction of the selected provider's window at which the conversation is compacted |
| `contextCompactionMode` | `summary` | `summary`: the model replaces the history with a structured summary. `truncate`: rollback switch, drops the oldest messages |
| `contextCompactionRatio` | `0.3` (chart), `0.5` (code) | **Deprecated.** The fraction of the window drop-oldest truncation cuts down to: `truncate` mode and the fallback when a summary fails |

## Configuration

These are operator-level settings (chart values or env vars) that apply to every deployment the operator manages.

```yaml
modelRouter:
  contextCompactionThreshold: 0.8
  contextCompactionMode: summary   # or: truncate
  contextCompactionRatio: 0.3      # deprecated; truncate mode and fallback only
```

```yaml
env:
  - name: CONTEXT_COMPACTION_THRESHOLD
    value: "0.8"
  - name: CONTEXT_COMPACTION_MODE
    value: "summary"
```

## How summary compaction works

1. **Trigger.** Before each request (including tool-call continuations), the router estimates preamble + stored conversation + new input. Above the threshold it emits a `compaction.started` trace event and compacts synchronously; the turn waits for it.
2. **Summary.** The provider serving the request (or the Agent's `memory.summaryModelSelectorRef` provider, when that is one of the run's providers) is sent the current request's prefix plus a summarize instruction, so providers with prompt caching serve most of it from cache. The summary must contain these sections, and is capped at `min(12000, maxOutputTokens)` tokens:
   - Goal & original constraints
   - Decisions made (and why)
   - Files & resources touched (current state)
   - Work completed
   - Open todos / next step
   - Errors & dead ends (don't retry)
   - Key facts/identifiers
3. **What stays verbatim.**
   - The original task (the first user message), pinned as `[Original task — kept verbatim by compaction]`. Over ~8K tokens, its start and end are kept and the summary covers the middle.
   - The latest user message.
   - Mid tool loop, the last 3 complete tool-call groups. The oldest of them are dropped while they exceed 5% of the window.
4. **Rolling.** A later compaction summarizes the previous `[Conversation summary #n]` plus everything since. The pinned task is carried over unchanged.
5. **Fallback.** An error or a summary missing sections is retried once. After that, the oldest messages are dropped down to `contextCompactionRatio` of the window, and the trace event's mode is `degraded`. The turn always continues.
6. **Archive and memory.** The conversation as it was before compaction is saved to `agentorca/runs/<run>/archive/<unix-ms>` (zstd, checkpoint TTL), for a future recall tool (#48). When hindsight is enabled, the summary is also retained to the agent's bank in the background.

The completion trace event keeps the `truncation` type that the editor bridge and web UI already render, with a `mode` field: `summary`, `truncate`, `degraded` or `skipped`.

Checkpoints are saved whole and loaded whole: an oversized checkpoint is summarized on its first request instead of being cut on load.

### Pre-send safety net

`fitRequestToProvider` applies the same budget to the request actually sent. It catches fallback providers with smaller windows and new input that is too large by itself (the newest message is shortened, never dropped).

## Warm-pool session chaining

Long-running `AgentDeployment`s with `inputSource.type: chat` use **warm pods** — idle agent pods that are reused across chat turns instead of being shut down and recreated. For the agent to *remember* prior turns across `execute` calls:

1. **Checkpoint on completion** — When a chat turn finishes (`POST …/complete`), the checkpoint Store persists a `Checkpoint{LastRunRef, ConversationHistory, TotalCostUSD}` entry keyed by `sessionId`. Prior to a recent fix (#55), this only persisted every `CheckpointEvery` turns; now it persists on every `/complete` call for chat runs.
2. **Claim on next turn** — The next `execute` with the same `sessionId` triggers `ClaimRun`, which looks up the prior `LastRunRef` and loads the prior conversation from the model-router's `agentorca/runs/<run>/state` Redis key.
3. **Stale-claim reclaim** — If a pod was claimed by a run that already terminated, `claimWarmPod` reclaims it before spawning a fresh pod.

> **Note:** Warm pods must complete the prior turn (`handlePodSuccess`) and return to idle in the same reconcile cycle that observes completion; otherwise the next turn's claim spins up a fresh pod and loses context. This is handled by `checkProgress → reconcileRunPodOnTerminal` in the AgentDeployment reconciler.

### Warm-pod token management

Each warm pod receives a **freshly minted SA token Secret** (not the shared deployment-level token) at creation time. The token expiry is derived from `WarmPodMaxAge` so it always outlives the pod. When age-based recycling is disabled (the default — `WarmPodMaxAge` not set), the token is minted with the maximum Kubernetes-allowed lifetime (8760h = 1 year) so an indefinitely-lived pod can keep authenticating `claim-run` POSTs. This prevents the warm pod from becoming unclaimable due to a stale deployment-level token secret (which has a 15-minute expiry).

## Episodic summaries (deprecated)

`memory.episodicSummaryEvery` used to summarize the conversation every N turns. It is now ignored, with a warning: the conversation is summarized when it reaches the compaction threshold instead (see above). `memory.summaryModelSelectorRef` still selects the model that writes the summary.

## MCP auth for HTTP/SSE servers

HTTP/SSE MCP servers (e.g. Slack's public MCP) that require bearer-token auth need an `auth` block on the `MCPServer` CR:

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: MCPServer
metadata:
  name: slack-mcp
spec:
  transport: http
  url: https://mcp.slack.com/mcp
  auth:
    bearerToken:
      name: slack-mcp-token
      key: token
```

### OAuth for OAuth-only MCP servers

Some MCP servers (e.g. Slack's public MCP) require full OAuth 2.0 (not a static bearer token). For these, use the `auth.oauth` block:

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: MCPServer
metadata:
  name: slack-mcp
spec:
  transport: http
  url: https://mcp.slack.com/mcp
  auth:
    oauth:
      credentials:
        name: slack-oauth-creds
        key: client.json  # OAuth client ID/secret JSON
      scopes:
        - connections:write
        - chat:write
```

The model-router performs the OAuth token exchange in-memory (RFC 8414 metadata discovery + refresh_token grant) and keeps the bearer token refreshed. No access-token Secret is mounted — only the credentials Secret. This is mutually exclusive with `auth.bearerToken` and with `stdio` transport.

The operator propagates `auth` (both bearer and OAuth) to child `Tool` CRs and renders the appropriate credential files/volumes into the agent pod. See [docs/mcp-access-control.md](docs/mcp-access-control.md) for full details.

## Tool result truncation & loop guards

To prevent large tool results from blowing the context window, the model-router auto-sizes `MaxToolResultTokens` per deployment: ~10% of the model `ContextWindow`, floored at 8k, capped at 64k, bounded by half of `MaxRequestTokens`. Example: a 128k-window model gets ~12,800 tokens (not chars) of truncated tool-result capacity.

Flow/Displacement: the model-router enforces conservative loop guards on every AgentRun and AgentDeployment run:

- `MaxRepeatedToolCalls`: 50 (identical tool + args)
- `MaxConsecutiveNoopTurns`: 15 (consecutive text-only turns under `MinSubstantiveTokens`; not applied to chat runs)
- `ToolFrequencyCap`: off by default (opt-in)

Override them with `spec.safeguards`; a negative value disables a check. When one trips, the router stops the tool loop and ends the agent's response with `Run halted by safeguard: …`. It emits a `fail` trace event and answers later requests with 410. The operator then fails the run with `status.loopDetected` set, without retrying it.

When a tool result is actually truncated, a trace event is emitted (`toolResultTruncated`).

## Debug checklist

| Symptom | Check |
|--------|-------|
| Agent "forgets" prior chat turns after warm-pod reuse | `kubectl logs deploy/<agent-deployment> -c model-router \| grep ClaimRun` — confirm `PriorRunRef` is non-empty |
| A slow turn | Look for `compacted stored conversation` log lines: the summary call runs synchronously. The line includes `durationMs`, `mode` and a token `breakdown` by role and top tools |
| Compaction keeps falling back (`mode: degraded`) | The line's `reason` says why. Usually the model ignored the section headings, or the summary hit its output cap |
| Tool results getting cut off | Check `MaxToolResultTokens` in the generated ConfigMap; ensure it's ≥ 10% of CW |
| Warm pod can't be claimed (401/403) | Verify the per-pod SA token Secret expiry (`warmPodTokenExpirySeconds` derives from `WarmPodMaxAge`) |
