# Known Issues

This is the consolidated tracker for known issues in agent-orca. Each entry links to its GitHub issue for discussion and tracking.

## How to Use

- Filter issues by the `known-issue` label: https://github.com/heddles/agent-orc/issues?q=label%3Aknown-issue
- Issues tagged `status:confirmed` have been verified as real problems with a clear root cause description.
- Issues tagged `status:untriaged` are reported but not yet confirmed.

---

| # | Issue | Component | Severity | Status |
|---|-------|-----------|----------|--------|
| [#70](https://github.com/heddles/agent-orc/issues/70) | Execution trace history shows the same exact time for every trace event | execution-trace, model-router | high | confirmed |
| [#71](https://github.com/heddles/agent-orc/issues/71) | `aoctl login` does not auto-refresh OIDC tokens | cli, authentication, oidc | medium | confirmed |
| [#72](https://github.com/heddles/agent-orc/issues/72) | Long running tasks capped at 30 minutes with no configurable override | controller, agent-crds | high | confirmed |
| [#73](https://github.com/heddles/agent-orc/issues/73) | Cost dashboard does not work | cost-tracking, dashboard | high | confirmed |
| [#74](https://github.com/heddles/agent-orc/issues/74) | Egress published, egress failed, and router tokens throughput stats don't work on status page | egress, observability, router | medium | confirmed |
| [#75](https://github.com/heddles/agent-orc/issues/75) | No way to easily pull down execution events history for a run | cli, execution-trace, api | medium | confirmed |
| [#76](https://github.com/heddles/agent-orc/issues/76) | Cost tracking needs validation for the continuation run path | cost-tracking, testing | high | confirmed |
| [#77](https://github.com/heddles/agent-orc/issues/77) | Multiple agent-orca operator pods may or may not work via Raft algorithm | controller, ha, raft | high | confirmed |
| [#78](https://github.com/heddles/agent-orc/issues/78) | Multiple agent warm pods may or may not work (untested since session history was implemented) | controller, warm-pods, session-history | high | confirmed |
| [#80](https://github.com/heddles/agent-orc/issues/80) | `aoctl` ACP sessions do not output execution traces and sometimes miss agent response output | acp, cli, execution-trace | high | confirmed |
| [#81](https://github.com/heddles/agent-orc/issues/81) | Execution trace UI reports "Stream connection lost" during long tool-call loops | ui, execution-trace, streaming | medium | confirmed |
| [#82](https://github.com/heddles/agent-orc/issues/82) | demo-htb-pwn MCP errors crash sessions, requiring manual pod restart and session recovery | demos, mcp, crash-recovery | high | confirmed |

---

## Adding a Known Issue

To register a new known issue:

1. Create a GitHub issue with the `known-issue` and `status:confirmed` labels.
2. Add a row to the table above.
3. Link relevant code paths in the issue body using file paths from the repository.

## Resolved Known Issues

Resolved issues are moved to the [closed issues list](https://github.com/heddles/agent-orc/issues?q=label%3Aknown-issue+is%3Aclosed).
