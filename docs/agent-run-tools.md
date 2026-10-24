# Agent-run tools

The model-router runs every tool the LLM calls: Tools, MCP servers and built-ins
(`_done`, `_spawn`, ...). Some tools can only run inside the agent process, e.g. a
shell, a compiler or a file editor in the agent's [sandbox](openshell-sandbox.md).
With `spec.runtime.localTools: true`, the agent declares those tools in its chat
request and runs their calls itself.

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: Agent
metadata:
  name: coder
spec:
  modelSelectorRef: default
  runtime:
    ociRef: ghcr.io/heddles/agent-orca/dev-sandbox:latest
    localTools: true
```

## Protocol

The agent uses the standard OpenAI tool-call flow against `OPENAI_BASE_URL`.

1. The agent sends `tools` with its own function tools. The router adds the Agent's
   router tools to what the LLM sees, as before. An agent tool may not reuse the name
   of a router tool or built-in (with or without the leading `_`); the request is
   rejected with 400.
2. When the LLM calls agent tools, the router first runs any router tools called in
   the same turn. It then streams the agent its calls only, indexed from 0, followed
   by `finish_reason: "tool_calls"` and `data: [DONE]`. The model's raw tool-call
   deltas are not forwarded, so an OpenAI client's tool-call accumulator sees exactly
   the calls it has to run. Text deltas stream as usual.
3. The agent runs the calls and sends its next request ending with one `role: "tool"`
   message per call (`tool_call_id` set). Only these trailing tool messages are read:
   the router keeps the conversation, so the agent may send just the results or its
   whole history. A request that leaves a call unanswered, answers an unknown ID or
   answers one twice is rejected with 400 and never reaches the LLM.
4. The turn continues: the router may run more of its own tools, return more agent
   calls, or end with a text reply.

Pending calls are derived from the stored conversation, so they survive a router
restart from a checkpoint.

## Governance

Agent tools get the same treatment as router tools:

- **Safeguards:** every call counts toward `maxRepeatedToolCalls` and
  `toolFrequencyCap`.
- **Guardrails:** input filters apply to tool results before they reach the LLM.
- **Result size:** results are capped at `maxToolResultTokens`, like router tool results.
- **Trace:** each call and result is a `toolCall` / `toolResult` trace event with
  `backendType: "agent"`.

Note that `maxRepeatedToolCalls` counts identical calls across the whole run. An
agent that legitimately reruns the same command (e.g. `go test ./...` after each
edit) needs a limit that allows for it.
