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

## Local terminal

- [`local_exec`](local-terminal.md) is tested with a simulated editor that starts
  commands the way Zed's source does. It has not been tried in a real editor.
- If the editor or `aoctl` goes away during a turn, the agent learns only when
  the call's time runs out (5 minutes plus the command's timeout).
- No background or interactive commands.
- It relies on ACP version 1; the version 2 preview removes client terminals.

## Known gaps in v0.13.0

Recorded at the v0.13.0 release (2026-11-23). These are not GitHub issues yet: promote one to an issue (and a row in the table above) when someone picks it up. "Severity" is the effect on a user, not the effort to fix.

### Not verified against the real service

| Area | What is unverified | Severity |
|---|---|---|
| Inbound channels: email (IMAP, signed webhook, SMTP), Microsoft Teams, Google Chat, Webex, Mattermost | Tested only against fakes of the service and its identity provider. Protocol details the vendors' documents did not settle are listed per platform in [chat-channels.md](chat-channels.md) (each platform has a "Not verified against the real service" paragraph). **Treat these five as experimental.** Slack inbound and the eero path were exercised live. | high |
| Per-user MCP OAuth ([mcp-oauth.md](mcp-oauth.md)) | Exercised on a cluster only against a fake OAuth and MCP provider. No real provider (GitHub, Google, Slack) has been tried. | high |
| Schedule result notifications (webhook, Slack, SMTP) | Delivery was checked against fake receivers. | medium |
| GitOps coexistence with Argo CD and Flux ([gitops-coexistence.md](gitops-coexistence.md)) | The ownership webhook is covered by unit and envtest tests. The end-to-end test with a real Argo CD or Flux (collision denied, handoff adopted, handoff expired, no prune) has not run. | high |

### Behaviour that surprises

| Area | Gap | Severity |
|---|---|---|
| Agent-as-tool and local models | A tool call is cut off after 60 seconds by default. A sub-agent on a local model can need several minutes, so the caller retries and starts duplicate sub-agent runs. Set `toolExecutionTimeoutSec` (AgentSchedule `safeguards`, AgentDeployment, AgentRun) to 600-900. The default is not raised by the platform. | medium |
| Reasoning models | A model can end a run with an empty answer (the text stays in its reasoning). The caller then repeats the work. The workaround is a prompt rule: end with the message content or the `_done` output. The platform does not treat an empty final answer as a failure. | medium |
| `_create_workflow` tool | A call that cannot finish ran for 300 seconds although the timeout message said 60. The timeout does not fire when it should. Not investigated. | medium |
| Invoker-mode MCP servers | Discovery drops each tool's side-effect class, so the "write tools need approval" default gates almost every invoker tool until a GuardrailPolicy sets the class. | medium |
| Invoker-mode MCP servers | A scheduled, trigger or widget run has no invoker. The server's tools are absent from that run instead of failing with `connection_required`. | low |
| Persistent deployment pods (`agentorca-deploy-*`) | They carry a placeholder run name and still fail the per-user MCP token fetch. Warm pods and per-run pods work. | medium |
| Scheduled runs and tailnet model hosts | Two scheduled runs failed within 5 seconds with `no such host` for a `.ts.net` provider host that resolved minutes later. Cause not found. A provider reachable only through the tailnet makes unattended runs depend on cluster DNS reaching it. | medium |
| Per-user MCP tokens | The router caches an access token until 30 seconds before it expires, so disconnecting a connection takes effect on a running run only when that token expires. | low |

### Not built

| Area | Gap |
|---|---|
| Schedule notifications | They cannot alert only when something is wrong: `on` selects the run's outcome (Succeeded, Failed, Skipped, BreakerOpen), not what the agent found. A report is sent on every run. |
| Knowledge upload | Text, markdown and CSV only, through a ConfigMap (256 KiB a document, 900 KiB in all). No PDF or Office files. Removing a document does not remove text already embedded. A `.pdf` name answers 400, not 415. |
| Slack | Socket Mode is not built: the Events API needs a public HTTPS URL. |
| Mattermost | No direct messages (they need the bot websocket). |
| Chat progress | Reactions are shown on Slack and Mattermost only. A delegated run's approval shows as waiting but is not announced in the thread. |
| Webex | If the message fetch after the acknowledgement fails, the message is recorded as ignored and lost: the enrichment step cannot ask for a retry. |
| Email | No IMAP IDLE, no OAuth token refresh for the mailbox, no mapping of a sender to a UI user (UI identities carry no email address), no rejection notice to a sender. |
| Chat users | They are not agent-orca users: a chat run has no invoker, so invoker-mode servers fail closed for it. Linking a chat user to a UI user is future work. |
| Metrics | `agentorca_mcp_token_refresh_failures_total`, `agentorca_mcp_connections_expired_total` and `agentorca_mcp_key_rotation_rows_total` appear only after their first event, which makes `rate()` alerts awkward. |

### Test flakes on `main`

`TestRoutingRequestLogCarriesTheReason` and `TestClaimRun_RestartKeepsPriorRunSearchable` fail intermittently under `-race`. `TestEmbedShrinksBatchesAfter429` (`internal/rag`) is timing-dependent and failed once in CI. A re-run passes.

---

## Adding a Known Issue

To register a new known issue:

1. Create a GitHub issue with the `known-issue` and `status:confirmed` labels.
2. Add a row to the table above.
3. Link relevant code paths in the issue body using file paths from the repository.

## Resolved Known Issues

Resolved issues are moved to the [closed issues list](https://github.com/heddles/agent-orc/issues?q=label%3Aknown-issue+is%3Aclosed).
