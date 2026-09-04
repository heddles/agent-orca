# Context Management

This guide covers how agent-orca manages LLM context windows across turns and runs — including context compaction, episodic summaries, warm-pool session chaining, tool-call-loop buffer capping, and MCP auth.

## Overview

agent-orca enforces a hard cap on the conversation sent to each LLM request, measured in estimated tokens. The cap is derived from the largest configured `ModelProvider.contextWindow`. At 80% of that window, the router triggers **proactive async compaction**: it drops the oldest turns and replaces them with a condensed summary message.

| Layer | Description |
|-------|-------------|
| **Context window ceiling** | 80% of the largest provider's `contextWindow` (see `ContextWindowReserve`) |
| **Compaction target** | Configurable fraction (`contextCompactionRatio`) of the context window to compact **down to** |
| **Proactive threshold** | Fraction of the ceiling at which truncation is triggered proactively (strategy-based: 0.6 for `balanced`, 0.4 for `aggressive`, 0.0 for `max-fidelity`) |
| **Episodic summary** | A `[Compacted: Nmsg/~Xk]` system message replacing dropped turns |
| **Token streaming cap** | Redis stream auto-trims at 100k entries (was 10k) to avoid losing history on replay |
| **Tool-loop cap** | Mid tool-call loop, the live buffer is capped via `capLiveBufferDuringToolLoop()` so it doesn't grow unboundedly during multi-step tool chains |

## Configuring the compaction target

By default, compaction reduces the conversation to **30%** of the context window (set via the Helm chart). The code-level default (when unset) is 50%. You can tune this to aggressively reclaim context — for example, to compact down to ~10% of the window, set `contextCompactionRatio: 0.1`.

### Helm values

```yaml
modelRouter:
  contextCompactionRatio: 0.1  # default: 0.3
```

### Environment variable (operator level)

```yaml
env:
  - name: CONTEXT_COMPACTION_RATIO
    value: "0.1"
```

> **Note:** `contextCompactionRatio` is **not** configurable per-AgentDeployment. It is an operator-level setting (chart value or env var) that applies to all deployments managed by the operator. To use different ratios per deployment, run separate operator instances.

## Context management strategies

The `contextManagementStrategy` field controls how aggressively the router manages context. Set via the Helm value `modelRouter.contextManagementStrategy` or the `CONTEXT_MANAGEMENT_STRATEGY` env var.

| Strategy | Proactive threshold | Behavior |
|----------|-------------------|----------|
| `balanced` (default) | 60% of budget | Triggers truncation at 60% of the checkpoint budget; suitable for most workloads |
| `aggressive` | 40% of budget | Triggers truncation earlier; reduces memory and latency spikes at the cost of more frequent compaction |
| `max-fidelity` | 0.0 (disabled) | Only truncates when the buffer exceeds the 80% ceiling; preserves maximum context |

### Explicit proactive threshold override

You can override the strategy-based proactive threshold with an explicit value:

```yaml
modelRouter:
  contextManagementStrategy: balanced
  proactiveTruncationThreshold: 0.5  # triggers at 50% of budget
```

## How compaction works

When `priorMessages + messages` exceeds the context window ceiling during `trimLiveBuffer` or `concludeTurn`, the router:

1. Delegates truncation to a **background goroutine** (`spawnCompaction`) so the request thread is not blocked.
2. Calls `truncateHistory` with the **compaction target** (not just the ceiling). The target is `max(min(CW * ratio, checkpointBudget), 2000)`.
3. Drops the oldest **safe** message boundaries (never splitting a `tool_use`/`tool_result` pair) until the sum fits the target.
4. Replaces the dropped messages with a single **`[Compacted: Nmsg/~Xk]`** system message containing:
     - The first user intent (truncated to 75 chars)
     - The 2 most recent user follow-ups (50 chars each)
     - Sorted list of tool calls made + count of unique vs. total calls
5. Preserves all leading system messages (system prompt, builtin tool hints, RAG trust boundary).

The compaction summary itself is ~100–200 tokens, so the effective residual context lands near the requested ratio, not the 80% ceiling.

### Tool-call loop buffer capping

The top-level request path caps the buffer via `trimLiveBuffer()` at the start of each new top-level request and via `concludeTurn()` at the terminal text turn. However, the tool-call loop recurses through `HandleChatCompletions` with a continuation context, for which **both cap points are gated off**. As a result, the live buffer and its incremental token cache (`liveBufferTokens`) only **grow** during the tool-call loop — they are never capped until the loop resolves into a terminal text turn. This caused buffers to stay at ~92% of the window during multi-step tool chains, with checkpoints persisting at the 80% ceiling on every tool-call iteration.

`capLiveBufferDuringToolLoop()` (in `internal/router/router.go`) fixes this: it synchronously compacts the combined `priorMessages + messages` buffer down to `compactionTarget()` when the proactive threshold is exceeded, persists the result back into `r.messages` (clearing `priorMessages`), and resyncs `liveBufferTokens`. The pre-send truncation remains as a safety net for the outgoing payload. It is wired into both:

- Non-streaming tool dispatch: `router.go` → `handleToolCalls`
- Streaming tool dispatch: `stream.go` → `handleStreamingResponse`

## Warm-pool session chaining

Long-running `AgentDeployment`s with `inputSource.type: chat` use **warm pods** — idle agent pods that are reused across chat turns instead of being shut down and recreated. For the agent to *remember* prior turns across `execute` calls:

1. **Checkpoint on completion** — When a chat turn finishes (`POST …/complete`), the checkpoint Store persists a `Checkpoint{LastRunRef, ConversationHistory, TotalCostUSD}` entry keyed by `sessionId`. Prior to a recent fix (#55), this only persisted every `CheckpointEvery` turns; now it persists on every `/complete` call for chat runs.
2. **Claim on next turn** — The next `execute` with the same `sessionId` triggers `ClaimRun`, which looks up the prior `LastRunRef` and loads the prior conversation from the model-router's `agentorca/runs/<run>/state` Redis key.
3. **Stale-claim reclaim** — If a pod was claimed by a run that already terminated, `claimWarmPod` reclaims it before spawning a fresh pod.

> **Note:** Warm pods must complete the prior turn (`handlePodSuccess`) and return to idle in the same reconcile cycle that observes completion; otherwise the next turn's claim spins up a fresh pod and loses context. This is handled by `checkProgress → reconcileRunPodOnTerminal` in the AgentDeployment reconciler.

### Warm-pod token management

Each warm pod receives a **freshly minted SA token Secret** (not the shared deployment-level token) at creation time. The token expiry is derived from `WarmPodMaxAge` so it always outlives the pod. When age-based recycling is disabled (the default — `WarmPodMaxAge` not set), the token is minted with the maximum Kubernetes-allowed lifetime (8760h = 1 year) so an indefinitely-lived pod can keep authenticating `claim-run` POSTs. This prevents the warm pod from becoming unclaimable due to a stale deployment-level token secret (which has a 15-minute expiry).

## Episodic summaries

For very long runs (or runs with `CheckpointEvery` spilling across many turns), the router can generate an **episodic summary** via `maybeRunEpisodicSummary`. Triggered every `episodicMemory.summaryEvery` turns (default: 2), it:

- Reads from both `priorMessages` and `messages` (after fold)
- Sends the conversation to a configured provider (`summaryProviderName`)
- Persists the summary to the KV key `episodic:<run>:<n>`
- Replaces the summarized turns in the in-memory buffer

```yaml
apiVersion: v1
kind: Config
stringData:
  config.json:
    EpisodicMemory:
      SummaryEvery: 2
      SummaryProviderName: openai-gpt4o-mini
      SummaryModel: gpt-4o-mini
```

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

Flow/Displacement: tool calls are monitored with conservative loop guards:

- `MaxRepeatedToolCalls`: 50 (identical tool + args)
- `MaxConsecutiveNoopTurns`: 15
- `ToolFrequencyCap`: off by default (opt-in)

When a tool result is actually truncated, a trace event is emitted (`toolResultTruncated`).

## Debug checklist

| Symptom | Check |
|--------|-------|
| Agent "forgets" prior chat turns after warm-pod reuse | `kubectl logs deploy/<agent-deployment> -c model-router \| grep ClaimRun` — confirm `PriorRunRef` is non-empty |
| High latency on a single turn | Look for `truncating conversation history` log lines — indicates sync pre-send truncation may be running |
| Buffer only grows during long tool-call chains | Look for `compacted live buffer during tool-call loop` debug log lines — confirms `capLiveBufferDuringToolLoop` is firing |
| Tool results getting cut off | Check `MaxToolResultTokens` in the generated ConfigMap; ensure it's ≥ 10% of CW |
| Episodic summary not firing | Verify `EpisodicMemory.SummaryEvery` and that a provider is configured |
| Warm pod can't be claimed (401/403) | Verify the per-pod SA token Secret expiry (`warmPodTokenExpirySeconds` derives from `WarmPodMaxAge`) |
