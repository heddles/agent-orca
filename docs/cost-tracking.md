# Cost Tracking

This document describes how LLM spend is tracked through the agent orchestrator and how to verify it end-to-end.

## Architecture

Every agent pod runs a **model-router sidecar** that proxies LLM calls. The router maintains a running `spendUSD` total, calculated from token usage and per-model pricing defined in the ModelProvider CRD.

```
Agent Pod
├── agent container          (calls localhost:8080 for LLM)
├── model-router sidecar     (proxies to LLM provider, tracks spend)
└── tool-executor sidecar    (dispatches tool calls, including child agents)
```

### Cost flow

1. **LLM call completes** — the router parses `usage.prompt_tokens` and `usage.completion_tokens` from the provider response.
2. **`updateSpend()`** — calculates `(prompt × costPerInputToken) + (completion × costPerOutputToken)`, adds it to `r.spendUSD`, and persists to Redis via `store.SaveSpend()`.
3. **Run completes** — the controller reads spend from Redis via `loadSpendFromStore()` and writes it to `AgentRun.Status.SpendUSD`.
4. **Workflow aggregation** — `AgentWorkflow` sums `SpendUSD` across all steps for budget enforcement.

### Where costs are generated

| Source | Code path | Provider used |
|--------|-----------|---------------|
| Main LLM calls (non-streaming) | `router.go` → `forwardToProvider()` | Selected by rule/meta router |
| Main LLM calls (streaming) | `stream.go` → `handleStreamingResponse()` | Selected by rule/meta router |
| Meta-router decisions | `meta_router.go` → `callLLM()` | `metaRouterProviderName` |
| Episodic memory summaries | `router.go` → `maybeRunEpisodicSummary()` | `episodicMemory.summaryProviderName` |
| Child agents (`_spawn`) | `router.go` → `executeSpawn()` | Child's own providers |
| Child agents (agent-type tools) | `executor.go` → `executeAgentTool()` | Child's own providers |

### Cost aggregation for child agents

When a parent agent spawns a child (via `_spawn` or an agent-type tool), the child's `SpendUSD` is added to the parent's running total after the child completes. This cascades: if a child spawns a grandchild, the grandchild's cost flows into the child, which then flows into the parent.

### Spend persistence and recovery

Spend is persisted to Redis at key `agentorc/runs/<run>/state:spend` after every LLM call. This allows recovery in two scenarios:

- **Pod restart**: `New()` loads prior spend from the checkpoint key on startup.
- **Warm-mode continuation**: `handleWarmModeClaim()` loads spend from the prior run's key when a warm pod is reused for a new turn.

### Budget enforcement

`AgentWorkflow` supports a `budgetCap.total` field (USD). The workflow controller calls `budgetExceeded()` before launching each step, summing `SpendUSD` across all completed steps. Since each step's spend includes child agent costs, the budget cap covers the full execution tree.

## Key files

| File | Relevant functions |
|------|--------------------|
| `internal/router/router.go` | `updateSpend()`, `aggregateChildSpend()`, `SpendUSD()`, `executeSpawn()`, `executeToolBackend()`, `selectProvider()`, `maybeRunEpisodicSummary()`, `New()`, `handleWarmModeClaim()` |
| `internal/router/meta_router.go` | `Route()`, `callLLM()` |
| `internal/router/stream.go` | `handleStreamingResponse()` |
| `internal/executor/executor.go` | `executeAgentTool()`, `dispatch()`, `ToolExecuteResponse.ChildSpendUSD` |
| `internal/controller/agentrun_controller.go` | `loadSpendFromStore()`, `handlePodSuccess()` |
| `internal/controller/agentworkflow_controller.go` | `sumSpend()`, `budgetExceeded()` |
| `internal/apiserver/uiapi.go` | `handleSaveResponse()` (populates `CheckpointMetadata.TotalCostUSD`) |
| `internal/state/store.go` | `SaveSpend()`, `LoadSpend()` |
| `api/v1alpha1/modelprovider_types.go` | `CostPerMillionInputTokens`, `CostPerMillionOutputTokens` |

## Verification

### Prerequisites

- A cluster with the operator deployed (images rebuilt with current code)
- `kubectl` configured for the target cluster
- `redis-cli` access to the operator's Redis instance

### 1. Episodic summary costs

Verify that episodic memory summarization LLM calls are included in spend.

1. Create or update an Agent with episodic memory on a low trigger threshold:
   ```yaml
   spec:
     memory:
       episodicSummaryEvery: 2
       summaryModelSelectorRef: "<your-cheap-modelselector>"
   ```
2. Run a chat session with 3+ turns to trigger at least one summary.
3. Check the run's spend:
   ```bash
   kubectl get agentrun <run-name> -o jsonpath='{.status.spendUSD}'
   ```
4. **Pass criteria**: `SpendUSD` is slightly higher than the sum of just the main model calls. Confirm via the `"episodic summary stored"` log line from the router pod.

### 2. Spend survives pod restart

Verify that spend is recovered from Redis after the pod is recreated.

1. Start a long-running agent (one with tools that take time).
2. Mid-run, note the spend in Redis:
   ```bash
   redis-cli GET "agentorc/runs/<run-name>/state:spend"
   ```
3. Kill the pod:
   ```bash
   kubectl delete pod <pod-name>
   ```
4. After the controller restarts the run and it completes:
   ```bash
   kubectl get agentrun <run-name> -o jsonpath='{.status.spendUSD}'
   ```
5. **Pass criteria**: Final `SpendUSD` is greater than the value from step 2 (includes both pre- and post-restart costs).

### 3. Spend carries across warm-mode continuations

Verify that multi-turn chat sessions accumulate spend across turns.

1. Use an AgentDeployment with `warmPool` enabled.
2. Send message 1, wait for completion. Note the run's `SpendUSD`.
3. Send message 2 (creates a continuation run with `priorRunRef`).
4. After message 2 completes:
   ```bash
   redis-cli GET "agentorc/runs/<message2-run>/state:spend"
   ```
5. **Pass criteria**: The second run's spend includes the first run's accumulated spend.

### 4. Meta-router costs

Verify that meta-routing LLM calls are tracked.

1. Create an Agent with `strategy: hybrid` and at least 2 providers with distinct routing hints.
2. Send a request ambiguous enough to trigger the meta-router (rule-router confidence below `metaRouterThreshold`, default 0.6).
3. Confirm the meta-router fired via the `"meta-router LLM response"` log line.
4. Check spend:
   ```bash
   kubectl get agentrun <run-name> -o jsonpath='{.status.spendUSD}'
   ```
5. **Pass criteria**: `SpendUSD` includes a small additional amount from the meta-router call.

### 5. Child agent cost aggregation

Verify that parent runs include child agent costs.

1. Create a parent agent with a tool backed by a child agent (`backendType: agent`) or one whose system prompt causes it to use `_spawn`.
2. Run it with a task that triggers the child.
3. Compare parent and child spend:
   ```bash
   # Child runs
   kubectl get agentrun -l agentorc.io/parent-run=<parent-run> \
     -o custom-columns='NAME:.metadata.name,SPEND:.status.spendUSD'

   # Parent run (should include child spend)
   kubectl get agentrun <parent-run> -o jsonpath='{.status.spendUSD}'
   ```
4. **Pass criteria**: Parent's `SpendUSD` = its own direct LLM costs + sum of all children's `SpendUSD`.

For cascading (grandchild) tests, create a 3-level chain and verify the top-level parent includes all levels.

### 6. Checkpoint metadata

Verify that `CheckpointMetadata.TotalCostUSD` is populated.

1. Run a chat session via an AgentDeployment (at least 1 completed turn).
2. Inspect the checkpoint:
   ```bash
   redis-cli GET "checkpoint:<session-id>" | python3 -m json.tool | grep totalCostUSD
   ```
3. **Pass criteria**: `totalCostUSD` is a non-empty string (e.g., `"0.042000"`).

### 7. Workflow budget enforcement

Verify that budget caps account for child agent costs.

1. Create an AgentWorkflow with a low budget and a step that spawns children:
   ```yaml
   spec:
     budgetCap:
       total: "0.10"
     steps:
       - name: research
         agentRef: my-agent-with-child-tools
         input: "do something that spawns children"
   ```
2. Run the workflow.
3. **Pass criteria**: Workflow fails with `"workflow budget cap 0.1000 USD exceeded"` at a threshold that includes child spend.

### Smoke test (all-in-one)

Run a single chat session against an AgentDeployment with:
- `strategy: hybrid` (meta-router)
- `episodicMemory.summaryEvery: 2` (episodic summaries)
- A tool that calls a child agent (child aggregation)
- `warmPool` enabled, send 2+ messages (resume recovery)

After completion:
```bash
kubectl get agentrun -n <ns> -l agentorc.io/session=<session-id> \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,SPEND:.status.spendUSD'

redis-cli GET "checkpoint:<session-id>" | python3 -m json.tool | grep totalCostUSD
```

Parent run spend should account for child runs, meta-routing, and episodic summaries. The checkpoint should show cumulative cost.
