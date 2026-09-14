# Reference agent image (`ghcr.io/agentorca/agent-orca/openai-reference`)

A minimal, **generic** OpenAI-compatible agent image for agent-orca. It is a thin
transport that streams a chat request to the model-router and returns the tokens —
the model-router injects the system prompt, tools, conversation history, guardrails,
built-in tool resolution (`_done`, `_fail`, `_clarify`, `_handoff`, `_spawn`,
`_rag_search`, …), spend accounting, and checkpointing. See
[docs/agent-images.md](../docs/agent-images.md) for the full contract.

This is the image published alongside releases as
`ghcr.io/agentorca/agent-orca/openai-reference:<version>` (plus `:latest`).

## Input modes

- **env** (default): set `AGENTORC_INPUT` (the operator does this for `inputMode: env`);
  the image runs one turn and exits.
- **http**: when `AGENTORC_INPUT` is unset (i.e. `inputMode: http`), the image serves:
  - `GET  /healthz` → `200 ok`
  - `POST /invoke` `{"input": "..."}` → `200 {"output": "..."}`
  on `PORT` (default `8000`).

## Quick start (local)

```bash
docker build -t ghcr.io/agentorca/agent-orca/openai-reference:latest -f examples/agent-sdk-template/Dockerfile .
```

Deploy it:

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: Agent
metadata:
  name: my-agent
spec:
  modelSelectorRef: default
  systemPrompt: "You are a helpful assistant."
  runtime:
    ociRef: ghcr.io/agentorca/agent-orca/openai-reference:latest
    framework: openai-compatible
```

The image has no baked persona or custom tools — set `systemPrompt` and `tools` on the
`Agent` CR; the model-router injects them into the request it serves.

## Custom persona / custom tools

The `agentorca` Python SDK (installed in this image) lets you write a richer agent with a
custom system prompt, `@agent.tool` registration, and lifecycle helpers. Replace
`agent.py` with your own and rebuild:

```python
from agentorca import Agent

agent = Agent(system_prompt="You are a domain expert. ...")

@agent.tool
def lookup_order(order_id: str) -> str:
    ...

result = agent.run(input=os.environ["AGENTORC_INPUT"])
print(result.output)
```

For the complete integration path (CLI, SDKs, ACP API), see
[docs/integrating.md](../docs/integrating.md).
