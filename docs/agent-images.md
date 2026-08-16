# Agent Images

An **agent image** is an OCI container whose entrypoint runs an [agent loop](#the-core-contract)
against the **model-router sidecar**. The operator launches one pod per `AgentRun`
(or one replica set for an `AgentDeployment`) and injects the configuration it needs
to reach the model-router — so a compliant image typically doesn't need any of its own
secrets, endpoints, or restart/checkpointing logic.

> **Why ship a reference image?** Most teams don't have an agent image lying around
> that correctly implements the lifecycle contract (`_done`/`_fail`/`_clarify`/`_handoff`,
> multi-turn resume, pooling). The contract is small, so a minimal reference image is
> low-maintenance and removes the largest onboarding friction.

---

## The core contract (what an agent image must do)

1. **Speak OpenAI chat completions.** Send `POST /v1/chat/completions` to the model-router
   (the base URL is injected via `OPENAI_BASE_URL`, see below). The request is a standard
   OpenAI `ChatCompletionRequest` (model, messages, tools, stream, …) and the response is a
   standard OpenAI `ChatCompletionResponse`.
2. **Run to a terminal state.** The model-router resolves all tool calls server-side
   (custom tools, MCP tools, and the built-in lifecycle/RAG tools — see
   [Built-in tools](#built-in-tools-are-injected-and-intercepted)) and applies guardrails
   before returning. The run terminates when the conversation reaches a built-in
   terminal signal (`_done`, `_fail`, `_clarify`, `_handoff`); the agent image just needs to
   exit cleanly once it has surfaced the final answer, or respond to an injected input.
3. **Read its input from the operator.** In `env` mode the input text is passed via the
   `AGENTORC_INPUT` env var; in `http` mode the operator POSTs the input to the agent's
   HTTP endpoint (default port `8000`, configurable via `inputPort`).
4. **Use the injected env vars and secrets** rather than hardcoding endpoints or keys.

That's it. Because the model-router owns provider selection, tool execution, guardrails,
token streaming, spend accounting, and checkpointing, an `openai-compatible` agent image is
a thin client — any OpenAI-compatible SDK (the `openai` package, LangChain, LangGraph,
CrewAI, ADK `LiteLlmModel`, …) works unmodified.

---

## Runtime frameworks and env-injection tiers

Set `spec.runtime.framework` on the `Agent` CR. The operator injects configuration by tier:

| Framework | Tier | What gets injected | Covers |
|---|---|---|---|
| `openai-compatible` *(default)* | 1 | `OPENAI_BASE_URL` (`<router>/v1`) + `OPENAI_API_KEY` (empty) | OpenAI SDK, LangChain, LangGraph, CrewAI, ADK (`LiteLlmModel`) |
| `autogen` | 2 | An `OAI_CONFIG_LIST.json` init container + `OAI_CONFIG_LIST` env | AutoGen v0.4+ |
| `semantic-kernel` | 2 | An `appsettings.json` init container + `APPSETTINGS_PATH`, `AZURE_OPENAI_ENDPOINT`, `OPENAI_ENDPOINT` | Semantic Kernel (C#/Python) |
| `langgraph` | 2 | `OPENAI_API_BASE` + optional `LANGGRAPH_CHECKPOINT_URL` | LangGraph (uses model-router checkpointing unless a Postgres checkpoint URL is set) |
| `shim` | 3 | A `/etc/hosts` redirect of `shimTarget` (default `api.openai.com`) → the router, plus `GOOGLE_API_BASE` | Frameworks that hardcode their provider URL (e.g. native Google ADK `generativelanguage.googleapis.com` → model-router Gemini-compatible surface on `:8082`) |

`openai-compatible` is the recommended default: it works with the widest range of existing
SDKs with zero framework-specific wiring.

---

## Environment variables injected into the agent container

| Variable | Value | Notes |
|---|---|---|
| `OPENAI_BASE_URL` | `http://<model-router>:8080/v1` | OpenAI-compatible endpoint; call `POST $OPENAI_BASE_URL/chat/completions` |
| `OPENAI_API_KEY` | *(empty)* | The model-router handles auth; the key is never read by the operator |
| `AGENTORC_INPUT` | run input text | `env` input mode (default) |
| `AGENTORC_INPUT_SOURCE` | the deployment's input-source type (`chat`, `queue`, `pubsub`, `loop`) | AgentDeployment pods only |
| `AGENTORC_RUN_ID` | the `AgentRun` name | For runs |
| `AGENTORC_AGENT` | the `Agent` name | For deployments |
| `AGENTORC_ROUTER_CONFIG` | path to the mounted router config | Gives the image the resolved model-selector/providers for the run |

**Input modes** (`spec.runtime.inputMode`, default `env`):
- `env` — the operator sets `AGENTORC_INPUT` and the agent runs once with that input.
- `http` — the agent runs as a long-lived HTTP server and the operator POSTs each input
  message to it on `spec.runtime.inputPort` (default `8000`). Used by chat-style
  `AgentDeployment`s.

---

## The model-router surfaces

The model-router (in every agent pod, by default) exposes:

| Surface | Path | Format |
|---|---|---|
| Chat completions | `POST /v1/chat/completions` | OpenAI `ChatCompletionRequest`/`Response` |
| Health | `GET /healthz`, `GET /readyz` | JSON |
| (shim tier) | `http://<model-router>:8082` | Google/Gemini-compatible (for `shim` frameworks) |

When the model returns `tool_calls`, the model-router **executes them server-side**
(`router.go` → `handleToolCalls`) and streams incremental results back, so a plain
OpenAI client sees a single completed conversation. Token/streaming tokens are published
to a Redis stream `tokens:<namespace>:<run>` and replayed via `TailTokens` for a browser
refresh. Guardrail output filters are applied before any response leaves the router.

---

## Built-in tools are injected and intercepted

The model-router injects these built-in tools into the agent's tool list. They are
**resolved server-side** — an agent image must *not* reimplement them; it should just
surface the results it receives:

`_done` · `_fail` · `_clarify` · `_handoff` · `_spawn` · `_rag_search` · `_rag_ingest` ·
`_search_history` · `_memory_store` · `_list_state` · `_read_state` · `_write_state` ·
`_delete_state` · `_list_resources` · `_mcp_read_resource` · `_emit_event` ·
`_propose_step` · `_propose_fix` · `_confirm_fix` · `_create_workflow`

**Lifecycle / terminal states:** `_done` (succeed, returns final output),
`_fail` (fail the run), `_clarify` (ask the user a question, run pauses as
`WaitingForInput`), `_handoff` (transfer to another agent/deployment), `_spawn`
(create a child `AgentRun`).

> If the model emits a question as plain text instead of calling `_clarify`, the
> model-router **auto-triggers** `_clarify` (`shouldAutoTriggerClarify`), so a compliant
> agent doesn't strictly need to detect questions itself — but its first-turn-after-resume
> path is skipped to avoid re-triggering loops.

---

## Checkpoints, spend, and resume (the state the image relies on)

State is persisted to Redis under `agentorc/runs/<run>/state` by the model-router's
`state.Store` (see [README → How session state works](README.md#how-session-state-works-two-layers)).
The image doesn't manage this, but it's why images can be stateless/restartable:

- **Messages** are saved on a checkpoint cadence (`AgentRunCheckpointEvery`) and on
  terminal states; a restarted pod reloads them via `LoadMessages`.
- **Spend** is written after every LLM call (`SaveSpend` → `…/state:spend`); restored on
  cold start and on warm-pool reuse (`ClaimRun` → `PriorRunRef`).
- A resumed run chains context through `PriorRunRef`↔`LastRunRef`, which the model-router
  turns into `ResumeCheckpointKey = agentorc/runs/<prior-run>/state` to reload history.
- For chat deployments, the UI API also persists a session `Checkpoint{SessionID, Version,
  ConversationHistory, LastRunRef, Metadata{TotalCostUSD,…}}` via `internal/checkpoint`.
- The live conversation buffer is **hard-capped** to 80% of the largest provider
  `contextWindow` (`checkpointBudget()`); episodic summaries condense old turns so the
  buffer doesn't grow unbounded across a long session.

Images that want explicit state can use the SDK's `agent.save_checkpoint()` /
`agent.load_checkpoint()` helpers, and `agent.run(input=…)` handles the full loop.

---

## Minimal reference agent (Python, `openai-compatible`)

This is what `examples/agent-sdk-template/` ships: a persona-free transport built on `python:3.12-alpine`
that the `agentorc` SDK (`pkg/python/agentorc`, package `agentorc`) is also installed into for customization.
`skaffold dev` builds and kind-loads it as `:latest` automatically (see [development.md](development.md));
it is also published to GHCR on every release.

`examples/agent-sdk-template/Dockerfile`:
```dockerfile
FROM python:3.12-alpine
# Install the agent-orc Python SDK (available for custom agents; the default
# entrypoint below does not require it).
COPY pkg/python/agentorc/ /tmp/agentorc-sdk/
RUN pip install --no-cache-dir /tmp/agentorc-sdk/ && rm -rf /tmp/agentorc-sdk/
COPY examples/agent-sdk-template/agent.py /app/agent.py
USER 65534
ENTRYPOINT ["python", "/app/agent.py"]
```

`examples/agent-sdk-template/agent.py` is a generic transport — it just streams the run
input to `POST $OPENAI_BASE_URL/chat/completions` and prints the tokens (env mode) or serves
`GET /healthz` + `POST /invoke` (http mode). The model-router injects `systemPrompt`,
`tools`, prior context, and built-in tool resolution, so the image ships no persona.

Deploy it (no per-agent `ociRef` needed — the Agent inherits the image; set `systemPrompt`
and `tools` on the CR):

```yaml
apiVersion: agentorc.agentorc.io/v1alpha1
kind: Agent
metadata:
  name: my-agent
spec:
  modelSelectorRef: default
  systemPrompt: "You are a helpful assistant."
  runtime:
    ociRef: ghcr.io/agentorc/agent-orc/openai-reference:latest
    framework: openai-compatible
    inputMode: env
```

For a custom persona, replace `ociRef` with your own image and keep `systemPrompt` on the
CR (the reference image ignores a baked prompt — the model-router injects the CR's).

---

## Recommendations: should we ship reference images?

**Yes — ship a default-tier reference image, but keep it minimal.**

- The contract is small (an OpenAI chat-completion client), so a minimal reference image is
  low-maintenance and high-value for first runs. Without one, users must hand-roll a loop
  that handles `_done`/`_clarify`/`_handoff`, resume, and the injected env — a common source
  of "the run never terminates" or "context drifts" bugs.
- **`openai-compatible` is the one to ship first.** It's the default framework tier and the
  widest; it works with the `openai` SDK out of the box. That single image unblocks the
  Quick Start and `hack/test-agents.sh`.
- **"anthropic-compatible" is lower priority.** The model-router's native surface is OpenAI
  (`/v1/chat/completions`); `anthropicToOpenAI` only normalizes *internal* responses. An
  Anthropic-native image needs `framework: shim` or a manually-pointed base URL — not a
  first-class surface worth a dedicated image yet.
- Optionally ship two variants of the same image: `ghcr.io/agentorc/agent-orc/openai-reference:<version>`
  (minimal, `python:3.12-alpine`) and `:<version>-full` (SDK + common deps) for teams that
  want more out of box. Tag each per release; `:latest` tracks the newest for quick starts.

**Concrete next step (already staged):** `examples/agent-sdk-template/Dockerfile` builds the
reference image; `.github/workflows/build-reference-image.yml` publishes it on GitHub Release;
`.github/workflows/build-component-images.yml` publishes operator/model-router/mcp-ingester/ui-proxy;
`skaffold dev` builds+kind-loads `openai-reference:latest` via a non-fatal build hook; and the demos,
`hack/test-agents.sh`, and the Quick Start default to it. The component-image workflow uses
`docker/build-push-action` multi-arch and should be run/sign/SCA-scanned like the other releases.
