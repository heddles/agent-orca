# Local Model Selection Guide

This guide covers choosing a chat model for local development with `scripts/start-llama-native.sh`.

## Minimum viable model size

**Do not use models smaller than 7B parameters for any agent task that requires:**
- Multi-turn context tracking (remembering facts from earlier in the conversation)
- Reliable tool use (calling the right tool with the right arguments)
- Instruction following (respecting system prompt rules)
- Coherent RAG responses

Models in the 1B–4B range fail these tasks frequently and unpredictably. Observed failures on `qwen2.5-3b-instruct`:

- Forgot user-stated preferences within 2–3 turns ("What did I name you?" → wrong answer)
- Called `_rag_search` with an empty `query` field despite a non-empty schema constraint
- Used `code-runner` to print conversational responses
- Continued answering after being explicitly told to stop
- Misidentified who was being addressed in pronoun-heavy utterances

These are model capability failures, not bugs in the router or system prompt. Prompt engineering reduces but does not eliminate them at this size.

## Demos that require a frontier model

Some demos depend on strict multi-step tool-use compliance that local models fail at
regardless of prompt engineering.

| Demo | Minimum viable | Notes |
|------|---------------|-------|
| `demo-escalation-chain` | **Frontier model required** | See below |
| `demo-soc-triage` | Qwen 2.5 14B or larger | Occasionally skips tool calls at 14B |
| `demo-parallel-swarm` | Qwen 2.5 14B | Reliable at 14B |
| All others | Qwen 2.5 14B | Current dev default |

### demo-escalation-chain — known deficiencies with local models

The escalation chain demo requires two agents to follow strict behavioral contracts:

**`deployment-planner-agent` (child run)**

The planner must call `_fail` — not produce prose — when a compatibility constraint is
violated. With Qwen 2.5 14B the following failures are observed:

- **Bypasses `_fail` entirely**: The model answers the deployment request as a general
  assistant ("here is how you do a canary deployment") rather than calling `_fail` with
  the constraint violation. Root cause: the model interprets `_fail` as a failure signal
  and reasons that a conflict with available options is not an "unrecoverable failure",
  so it produces prose instead.
- **Produces JSON without calling `_done`**: When no constraint is violated, the model
  sometimes outputs the JSON plan as text instead of calling `_done(output=...)`. The
  executor receives the raw text as the child run's output, which the orchestrator then
  summarises as a successful deployment — even if nothing was actually deployed.

**`deploy-orchestrator-agent` (top-level run)**

The orchestrator must call `deploy-planner` before responding. With Qwen 2.5 14B:

- **Hallucinates a deployment summary**: When the orchestrator receives plausible-sounding
  text from the planner (whether via a genuine `_done` call or raw prose), it presents it
  as a confirmed deployment. If the planner skipped `_fail`, the user sees a claim that
  payments-api v2.1.3 was deployed successfully — which is false and misleading.
- **Skips the tool call on first turn**: The orchestrator occasionally answers the user's
  deployment request directly from training-data knowledge, bypassing `deploy-planner`
  entirely.

**Why prompt engineering alone does not fix this**

Both failures are rooted in the model's RLHF training: it is rewarded for producing
helpful, complete-sounding answers to operational questions. A system prompt saying
"call the tool first" competes with that reward signal. Larger local models
(32B, 72B Q4) improve compliance significantly but do not eliminate it.

**Recommended workaround**: use a frontier model (Claude Sonnet or GPT-4o) as the
`default` ModelSelector for this demo. These models reliably follow tool-use instructions
with complex multi-step constraints. See the deploy section in
[demo-escalation-chain.md](demo-escalation-chain.md).

## Recommended models by RAM

All sizes assume ~3–4 GB consumed by the kind cluster and OS overhead.

| Available RAM | Recommended model              | GGUF size | Notes |
|---------------|-------------------------------|-----------|-------|
| 16 GB         | Qwen 2.5 7B Q4\_K\_M          | ~4.5 GB   | Minimum for reliable tool use |
| 16 GB         | Llama 3.1 8B Q4\_K\_M         | ~5 GB     | Good alternative; strong instruction following |
| 24 GB         | **Qwen 2.5 14B Q4\_K\_M**     | ~9 GB     | Current dev default; reliable for all agent tasks |
| 24 GB         | Qwen 2.5 14B Q8\_0            | ~15 GB    | Noticeably sharper; fits with some headroom |
| 48 GB         | Qwen 2.5 32B Q4\_K\_M         | ~20 GB    | Strong reasoning and code; good for complex workflows |
| 64 GB+        | Qwen 2.5 72B Q4\_K\_M         | ~45 GB    | Near-frontier local quality |

## Changing the model

1. Update `CHAT_MODEL_FILE` and `CHAT_MODEL_URL` in `scripts/start-llama-native.sh`
2. Update `litellmModel` in `charts/agent-orc-resources/values-dev.yaml` under the chat model provider
3. Restart the llama server (`scripts/start-llama-native.sh`)
4. Redeploy resources (`skaffold dev -p dev`)

The embedding model (`nomic-embed-text-v1.5`) is independent of the chat model and does not need to change.

## llama-server flags

Key flags used in `start-llama-native.sh`:

| Flag | Value | Purpose |
|------|-------|---------|
| `--n-gpu-layers 99` | 99 (all) | Offload all layers to Metal GPU — required for good throughput |
| `--ctx-size` | 8192 | Context window; increase to 16384 if using long conversations or large RAG chunks |
| `--parallel 2` | 2 | Concurrent inference slots; supports the warm pod pool dispatching overlapping requests |
