# Testing MCP and Tool Calls

This guide covers how to run the tool integration tests, which exercise:

- **MCP tools** — stdio-transport calculator (add, multiply)
- **Agent-as-tool** — sub-agent exposed as a callable tool (summarizer)

## Prerequisites

A running cluster with the operator installed. The testdata manifests use the `default` namespace.

## Step 1: Create API key secrets

Skip if the secrets already exist in the target namespace.

```bash
kubectl create secret generic anthropic-dev-key --from-literal=api-key=<YOUR_ANTHROPIC_KEY>
kubectl create secret generic openai-dev-key   --from-literal=api-key=<YOUR_OPENAI_KEY>
```

## Step 2: Apply shared infrastructure

`shared.yaml` creates the `ModelProvider` and `ModelSelector` (`test-router`) that all tool test agents reference.

```bash
kubectl apply -f testdata/agents/shared.yaml
```

Verify:

```bash
kubectl get modelprovider,modelselector -n default
```

## Step 3: Apply tool test manifests

```bash
kubectl apply -f testdata/agents/tools.yaml
```

This creates:

| Resource | Kind | Purpose |
|---|---|---|
| `test-mcp-calculator` | Tool | MCP server (inline Python, stdio transport) exposing `add` and `multiply` |
| `test-summarizer-agent` | Agent | Sub-agent that summarizes text in one sentence |
| `test-summarizer-tool` | Tool | Wraps `test-summarizer-agent` as a callable tool |
| `test-tools-orchestrator` | Agent | Orchestrator with both tools registered |
| `test-mcp-run` | AgentRun | Exercises the MCP calculator — expected output contains `294` (42 × 7) |
| `test-agent-tool-run` | AgentRun | Exercises the agent-as-tool — expected output is a one-sentence summary |

## Step 4: Watch the runs

```bash
kubectl get agentrun -n default -w
```

Both runs should transition: `Pending` → `Running` → `Succeeded`.

## Step 5: Stream output in real time

Use `stream-run.sh` to see tokens, model selection, and tool results as they arrive:

```bash
# Terminal 1 — start the stream watcher before creating runs
./hack/stream-run.sh

# Terminal 2 — trigger a new run (or re-apply tools.yaml)
kubectl apply -f testdata/agents/tools.yaml
```

The script port-forwards to the operator's internal API and prints:

```
[model: gpt-4.1]
The result of 42 multiplied by 7 is 294.

=== done ===
```

## Checking results

```bash
# View final output of a completed run
kubectl get agentrun test-mcp-run -n default -o jsonpath='{.status.output}'

kubectl get agentrun test-agent-tool-run -n default -o jsonpath='{.status.output}'
```

Expected outputs:

- `test-mcp-run` — contains `294`
- `test-agent-tool-run` — one-sentence summary of the Kubernetes operator passage

## Troubleshooting

**`ModelSelector "test-router" not found`**
You applied `tools.yaml` without applying `shared.yaml` first. Run Step 2.

**Run stuck in `Pending`**
Check operator logs and ensure the MCP sidecar image (`python:3.12-slim`) can be pulled:

```bash
kubectl logs -n agent-orc-system deploy/agent-orc-controller-manager
kubectl describe agentrun test-mcp-run -n default
```

**`stream-run.sh` shows no output**
Ensure the operator's internal API service exists:

```bash
kubectl get svc -n agent-orc-system agent-orc-internal-api
```

## Cleanup

```bash
kubectl delete -f testdata/agents/tools.yaml
kubectl delete -f testdata/agents/shared.yaml
```
