# Agent SDK Template

A reference agent that demonstrates the agent-orc Python SDK.

## What it shows

- **OpenAI-compatible integration**: The `Agent` class wraps an OpenAI-compatible
  client pointed at the model-router sidecar (`http://localhost:8080`).
- **Custom tools**: `@agent.tool` decorator registers tools for the LLM.
- **Built-in lifecycle tools**: `agent.done()`, `agent.fail()`, `agent.ask()`,
  `agent.handoff()`, `agent.spawn()` — Pythonic wrappers for the `_done`, `_fail`,
  `_clarify`, `_handoff`, `_spawn` built-in tools.
- **Checkpoint helpers**: `agent.load_checkpoint()` and `agent.save_checkpoint()`
  for explicit state management.

## Quick start

```bash
pip install agentorc
python examples/agent-sdk-template/agent.py
```

## Deploy on agent-orc

1. Build and push the agent image:

```bash
docker build -t ghcr.io/myorg/agent-sdk-template:latest \
  -f examples/agent-sdk-template/Dockerfile .
docker push ghcr.io/myorg/agent-sdk-template:latest
```

2. Create an `Agent` CR:

```yaml
apiVersion: agentorc.agentorc.io/v1alpha1
kind: Agent
metadata:
  name: sdk-template-agent
spec:
  modelSelectorRef: default
  tools: [web-search]
  systemPrompt: "You are a helpful research assistant."
  runtime:
    ociRef: ghcr.io/myorg/agent-sdk-template:latest
    framework: openai-compatible
  memory:
    checkpointEvery: 5
    resumeWindowSeconds: 3600
    archiveOnCompletion: true
```

3. Submit a task:

```bash
aoctl tasks submit --agent sdk-template-agent --input "What are the latest developments in LLMs?"
```

## How it works

The model-router sidecar injects the following environment variables into the
agent container:

| Variable | Value |
|---|---|
| `OPENAI_BASE_URL` | `http://localhost:8080` |
| `OPENAI_API_KEY` | (empty — model-router handles auth) |
| `AGENTORC_INPUT` | The run input text (env mode) |

The SDK's `Agent.run()` method handles the full chat completion loop:
1. Sends messages to `/v1/chat/completions` on the model-router
2. Executes any tool calls (custom or built-in)
3. Checks for terminal states (`_done`, `_fail`, `_clarify`, `_handoff`)
4. Returns a `RunResult` with the final output and phase
