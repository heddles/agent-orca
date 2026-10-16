# agent-orca roadmap

Priorities for agent-orca, updated as major items are scoped and shipped.
Open items link to their tracking issue for discussion and progress.

## 1. ACP v2 dialect support (top priority)

**Tracking issue:** [#33](https://github.com/heddles/agent-orca/issues/33)

Adopt the Agent Client Protocol **v2** dialect in the `aoctl acp` bridge
(`cmd/aoctl/acp_bridge.go`, JSON-RPC over stdio via `cmd/aoctl/acp_jsonrpc.go`)
so long-running AgentRuns are first-class in ACP clients (editors).

- `session/prompt` returns immediately with `{messageId}` ("prompt inserted")
  instead of blocking until the turn completes.
- Turn state streams as `state_update` notifications:
  `running` / `idle` (with `stopReason`) / `requires_action`.
- `idle.stopReason` (`end_turn | max_tokens | max_turn_requests | refusal |
  cancelled | error`) maps 1:1 onto AgentRun completion phases.
- `requires_action` gives mid-run permission requests a first-class
  representation, mapping onto our elicitation flow.

**Why this is the top item:** agent-orca's execution model is already async
(AgentRun CRD → warm-pod claim → model-router sidecar), but the current v1
bridge blocks `session/prompt` until the turn completes, so editors see no
live run state. This is the one capability users cannot get today.

**Status:** not started — ~4–6 days (server mapping ~1.5d, bridge dialect
negotiation ~1.5d, conformance tests ~2d, docs + manual verification ~0.5d).

**Risk & mitigation:** the v2 surface is still `unstable-v2` upstream
(`agentclientprotocol/agent-client-protocol`, `schema/v2/schema.json`; v2
scaffolded v0.13.0 #1099, draft schema v0.13.5 #1306, prompt-insertion
messageId #2175, foreground state transitions #2411/#2387). Gate v2 behind
`acpDialect: v2` and keep v1 as the default. Session notices (#2336) and
compaction (#2334) are already stable in v1 and can ship independently.

## 2. Further out

_(fill in as items are scoped)_

---

*Last updated: 2026-10-16*