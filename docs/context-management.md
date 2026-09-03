# Context Management

This guide covers how agent-orca manages LLM context windows across turns and runs — including context compaction, episodic summaries, warm-pool session chaining, and MCP auth.

## Overview

agent-orca enforces a hard cap on the conversation sent to each LLM request, measured in estimated tokens. The cap is derived from the largest configured `ModelProvider.contextWindow`. At 80% of that window, the router triggers **proactive async compaction**: it drops the oldest turns and replaces them with a condensed summary message.

| Layer | Description |
|-------|-------------|
| **Context window ceiling** | 80% of the largest provider's `contextWindow` (see `ContextWindowReserve`) |
| **Compaction target** | Configurable fraction (`contextCompactionRatio`) of the context window to compact **down to** |
| **Episodic summary** | A `[Compacted: Nmsg/~Xk]` system message replacing dropped turns |
| **Token streaming cap** | Redis stream auto-trims at 100k entries (was 10k) to avoid losing history on replay |

## Configuring the compaction target

By default, compaction reduces the conversation to **50%** of the context window. You can tune this to aggressively reclaim context — for example, to compact down to ~10% of the window, set `contextCompactionRatio: 0.1`.

### Helm values

```yaml
gateway:
  modelRouter:
    contextCompactionRatio: 0.1  # default: 0.5
```

### Environment variable (operator level)

```yaml
env:
  - name: CONTEXT_COMPACTION_RATIO
    value: "0.1"
```

### AgentDeployment Spec

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: AgentDeployment
spec:
  contextCompactionRatio: 0.1  # optional; overrides operator/global default
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

## Warm-pool session chaining

Long-running `AgentDeployment`s with `inputSource.type: chat` use **warm pods** — idle agent pods that are reused across chat turns instead of being shut down and recreated. For the agent to *remember* prior turns across `execute` calls:

1. **Checkpoint on completion** — When a chat turn finishes (`POST …/complete`), the checkpoint Store persists a `Checkpoint{LastRunRef, ConversationHistory, TotalCostUSD}` entry keyed by `sessionId`. Prior to a recent fix (#55), this only persisted every `CheckpointEvery` turns; now it persists on every `/complete` call for chat runs.
2. **Claim on next turn** — The next `execute` with the same `sessionId` triggers `ClaimRun`, which looks up the prior `LastRunRef` and loads the prior conversation from the model-router's `agentorca/runs/<run>/state` Redis key.
3. **Stale-claim reclaim** — If a pod was claimed by a run that already terminated, `claimWarmPod` reclaims it before spawning a fresh pod.

> **Note:** Warm pods must complete the prior turn (`handlePodSuccess`) and return to idle in the same reconcile cycle that observes completion; otherwise the next turn's claim spins up a fresh pod and loses context. This is handled by `checkProgress → reconcileRunPodOnTerminal` in the AgentDeployment reconciler.

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

## MCP auth for HTTP servers

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

The operator propagates `auth` to child `Tool` CRs and renders the token as a file mounted into the agent pod. This is **mutually exclusive** with `stdio` transport (which runs locally as a sidecar and needs no credentials).

## Tool result truncation & loop guards

To prevent large tool results from blowing the context window, the model-router auto-sizes `MaxToolResultTokens` per deployment: ~10% of the model `ContextWindow`, floored at 8k, capped at 64k, bounded by half of `MaxRequestTokens`. Example: a 128k-window model gets 12,800 char truncation (was a fixed 4,000).

Flow/Displacement: tool calls are monitored with conservative loop guards:

- `MaxRepeatedToolCalls`: 50 (identical tool + args)
- `MaxConsecutiveNoopTurns`: 15
- `ToolFrequencyCap`: off by default (opt-in)

When a tool result is actually truncated, a trace event is emitted (`tool_result_truncated`).

## Debug checklist

| Symptom | Check |
|--------|-------|
| Agent "forgets" prior chat turns after warm-pod reuse | `kubectl logs deploy/<agent-deployment> -c model-router \| grep ClaimRun` — confirm `PriorRunRef` is non-empty |
| High latency on a single turn | Look for `truncating conversation history` log lines — indicates sync pre-send truncation may be running |
| Tool results getting cut off | Check `MaxToolResultTokens` in the generated ConfigMap; ensure it's ≥ 10% of CW |
| Episodic summary not firing | Verify `EpisodicMemory.SummaryEvery` and that a provider is configured |