# agent-orca roadmap

Priorities for agent-orca, updated as major items are scoped and shipped.
Open items link to their tracking issue for discussion and progress.

## 1. Native local filesystem access via ACP fs/* (top priority)

**Tracking issue:** [#34](https://github.com/heddles/agent-orca/issues/34)

Give agent pods native read/write access to the user's local workspace via
ACP's agent→client filesystem methods, `fs/read_text_file` and
`fs/write_text_file` — this is a **v1** surface (upstream v2 removed its
client filesystem surface, #1346), so it does not depend on #33 and can
land first.

**Why this is the top item:** this is the most valuable missing capability
— it's how Claude Code and Gemini CLI already edit a Zed workspace today.
Zed (and other ACP clients) already answer `fs/*` requests; our bridge
(`cmd/aoctl/acp_bridge.go`, JSON-RPC over stdio via `cmd/aoctl/acp_jsonrpc.go`)
just never sends them.

**Architecture (from the design discussion):** *pod drives the workstation*
— add a reverse proxy in the bridge so the agent pod forwards `fs/*`
requests to the client (Zed), which answers them as it already does. Edits
stay live against the user's actual files: no workspace copies, no sync
drift, no volume mounts, and the editor remains the arbiter of access.
Rejected alternative: shipping workspace files into the pod (volume mounts
/ copy-in) — diverges from live editor state, needs its own sync machinery,
and is not how ACP-native agents operate.

**Status:** not started — ~4–5 days (bridge reverse proxy ~1.5d, capability
negotiation on `readTextFile`/`writeTextFile` ~0.5d, security/policy:
workspace-root confinement, gated writes, Agent CRD policy knobs ~1.5d,
tests + docs ~1d).

**Security notes:** confine paths to workspace roots the client exposes;
gate writes behind approval/policy, mirroring `docs/mcp-access-control.md`
patterns.

## 2. ACP v2 dialect support

**Tracking issue:** [#33](https://github.com/heddles/agent-orca/issues/33)

Adopt the Agent Client Protocol **v2** dialect in the `aoctl acp` bridge so
long-running AgentRuns are first-class in ACP clients (editors).

- `session/prompt` returns immediately with `{messageId}` ("prompt inserted")
  instead of blocking until the turn completes.
- Turn state streams as `state_update` notifications:
  `running` / `idle` (with `stopReason`) / `requires_action`.
- `idle.stopReason` (`end_turn | max_tokens | max_turn_requests | refusal |
  cancelled | error`) maps 1:1 onto AgentRun completion phases.
- `requires_action` gives mid-run permission requests a first-class
  representation, mapping onto our elicitation flow.

**Status:** not started — ~4–6 days (server mapping ~1.5d, bridge dialect
negotiation ~1.5d, conformance tests ~2d, docs + manual verification ~0.5d).

**Risk & mitigation:** the v2 surface is still `unstable-v2` upstream
(`agentclientprotocol/agent-client-protocol`, `schema/v2/schema.json`; v2
scaffolded v0.13.0 #1099, draft schema v0.13.5 #1306, prompt-insertion
messageId #2175, foreground state transitions #2411/#2387). Gate v2 behind
`acpDialect: v2` and keep v1 as the default. Session notices (#2336) and
compaction (#2334) are already stable in v1 and can ship independently.

## 3. Further out

_(fill in as items are scoped)_

---

*Last updated: 2026-10-16*