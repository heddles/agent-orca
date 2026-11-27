# Scheduled runs

An `AgentSchedule` starts an agent on a clock: "every weekday at 8am". It works like a Kubernetes `CronJob`. On each tick the operator creates an ordinary `AgentRun` that the schedule owns, so everything that applies to a run applies here: the run budget, the egress sinks, the tenant labels and the ownership webhook.

Nobody is signed in at 8am. A scheduled run is attributed to the schedule, not to a person: its `agentorca.io/created-by` annotation is `schedule:<namespace>/<name>`, it has no invoker, and it does not appear under "My runs". This also means a scheduled run cannot use a per-user (`invoker`-mode) MCP connection.

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: AgentSchedule
metadata:
  name: weekday-digest
  namespace: team-a
spec:
  agentRef: digest-agent
  schedule: "0 8 * * 1-5"
  timeZone: Europe/Berlin
  input: "Write the team digest for {{schedule.date}}."
  budget:
    maxUSD: "1.00"
```

A complete example is in `config/samples/agentschedule.yaml`.

## The schedule

`schedule` is a five-field cron expression (minute, hour, day of month, month, day of week) or `@hourly`, `@daily`, `@weekly`, `@monthly`. Seconds, `@every` and a `TZ=` prefix are not accepted: the zone is `spec.timeZone`, an IANA name that defaults to `UTC`.

Daylight saving follows two rules:

- A wall-clock time that does not exist (the clocks jump forward over it) is skipped. `30 2 * * *` in `America/New_York` does not run on the second Sunday of March.
- A wall-clock time that occurs twice (the clocks go back) fires once, at its first occurrence. An hourly schedule therefore has no second run in the repeated hour.

Only `{{schedule.time}}` (RFC 3339, in the schedule's zone), `{{schedule.date}}` (`YYYY-MM-DD` in the schedule's zone) and `{{schedule.name}}` are substituted in `input`. Nothing else in it is interpreted, so there is no injection surface.

## What a tick does

1. The operator finds the most recent tick since the schedule's cursor. If several were missed it considers only the latest; there is never a catch-up burst.
2. A tick that is more than `startingDeadlineSeconds` late (default one hour) is recorded as a skip (`MissedSchedule`), not started.
3. `concurrencyPolicy: Forbid` (the default) skips the tick while an earlier run is active. `Allow` starts another run anyway.
4. The run is created with the **deterministic name** `<schedule>-<unix seconds of the tick>`. The name comes from the scheduled instant, not from the time of creation. A repeated reconcile, an operator restart, or a second replica creates the same name and the second create fails with `AlreadyExists`, so a tick cannot start two runs. Leader election (`operator.leaderElection`, on in the chart) keeps the work on one replica; the name is the actual safety net.
5. The run carries the labels `agentorca.io/schedule=<name>` and the schedule's `agentorca.io/tenant-id`, an owner reference to the schedule (deleting the schedule deletes its history) and the annotation `agentorca.io/scheduled-for`.

Editing the spec, including suspending and resuming, moves the cursor to now: nothing fires retroactively, and ticks skipped while suspended are not replayed.

A schedule name is at most 52 characters.

## Runs and history

`successfulRunsHistoryLimit` and `failedRunsHistoryLimit` (default 7 each) bound the finished runs kept; the oldest are deleted first. The schedule counts a finished run (failure streak, last success) before it can be pruned.

`status` shows `lastScheduleTime`, `nextScheduleTime`, the runs in `active`, `consecutiveFailures` and the last ten skipped ticks with the reason.

## Tenant isolation and quotas

A schedule belongs to one tenant. It gets `agentorca.io/tenant-id` from its namespace like every other kind, the label is immutable, and the isolation webhook rejects an `agentRef`, `workflowTemplateRef` or sink Secret that belongs to another tenant. The target must be in the schedule's own namespace; the validating webhook rejects one that is not found there.

Everything is checked again on **every tick**, because the world changes after a schedule is written:

- the agent (or each agent of the template's steps) must still exist and be in the tenant's `allowedAgents`; otherwise the tick is skipped, `TargetMissing` is set, and the UI says why;
- the tenant's `rateLimit.concurrentRuns` and `budgetPerDayUSD` apply (the same code as the External API, `internal/quota`). Scheduled runs count toward the concurrent-run limit. A refusal skips the tick with the reason `SkippedQuota` and starts nothing. `requestsPerMinute` does not apply: the minimum interval does that job;
- the schedule must still have a spending ceiling (below).

A run that was cancelled or refused never counts against the schedule's failure streak.

### A ceiling is required

An unattended run with no ceiling is the failure scheduling must not allow. A schedule is refused when it is created or edited unless it has at least one of: `spec.budget` (per run), `spec.spend` (per schedule), a workflow template with `budgetCap`, or a tenant with `budgetPerDayUSD`.

A scheduled run has nobody to approve a budget raise, so `budget.onExceeded` is `fail` unless set; `pause` is accepted with a warning.

### Spend caps

`spec.spend.maxUSDPerDay` and `maxUSDPerMonth` (UTC windows) cap the schedule as a whole. The schedule sums the cost of its runs into `status.spentTodayUSD` and `status.spentMonthUSD`; the cost of a finished run is kept in `status.ledger`, so pruning a run does not lose it. At a cap the condition `BudgetExhausted` is set, ticks are skipped with the reason `SkippedBudget`, and the schedule wakes when the window rolls over. It never rewrites `spec.suspend`, which belongs to the person. The overshoot is bounded by one run's budget. The moment a cap stops a schedule is written to the audit log as action `suspend` by `system:agentschedule`.

### Failure breaker

After `operator.schedule.maxConsecutiveFailures` (default 5) finished runs in a row that failed, the schedule becomes `Degraded` and stops ticking, so a broken schedule cannot spend every day on failures nobody saw. A success resets the count. Editing the schedule or resuming it clears the condition.

## Concurrency policy

- `Forbid` (default) skips a tick while a run of the schedule is active.
- `Allow` starts a run regardless.
- `Replace` cancels the active runs first, the way the UI's Stop does: the run is failed, the cancel signal reaches the state store so the agent stops and its sandbox is released, and the run is marked `agentorca.io/schedule-replaced` so the cancellation does not count as a failure. `Replace` is for `agentRef` schedules; a workflow cannot be cancelled, and the CRD refuses it with `workflowTemplateRef`.

## Workflow templates

`workflowTemplateRef` names an `AgentWorkflow` with `spec.mode: template` in the same namespace. Each tick creates an `AgentWorkflow` named `<schedule>-<unix time>` with `spec.fromTemplate` set to the template and `parameters` from the schedule (the three placeholders are substituted in the values). Timeout, budget cap and safeguards come from the template. The history limits prune these workflow instances, and their cost, from `status.totalSpendUSD`, counts toward the schedule's spend. A workflow's step runs are not counted toward the tenant's concurrent-run limit.

## Result notifications

The result of a run is always kept on the run. `spec.result.notify` also sends a short message to one of two destinations: a **channel** an administrator set up on the tenant, or a **Slack channel through a connected app**. Set exactly one of `channel` and `slack`.

**In the UI and its API, only an admin can change where messages go or what they hold.** Any change to `result.notify` needs the admin role: choosing, changing or removing the destination, the events, or `includeOutput`. A builder can still edit the rest of such a schedule, and duplicating a schedule copies its destination only for an admin. Changing the agent or the instruction changes what gets sent there and stays a builder's edit. A schedule managed in Git is changed in Git, with one exception: an admin can [choose another Slack channel in the UI](#changing-the-channel-of-a-schedule-managed-in-git). Writes made straight to Kubernetes are governed by Kubernetes RBAC.

```yaml
spec:
  result:
    notify:
      channel: ops          # a name from the tenant's TenantConfig
      on: [Failed, BreakerOpen]   # Succeeded | Failed | Skipped | BreakerOpen; default [Failed]
      includeOutput: false  # default false
```

### Slack, through a connected app

```yaml
spec:
  result:
    notify:
      slack:
        app: orca-bot           # the name of a shared app (Settings, Chat apps)
        channelID: C0123456789  # Slack's id of the channel, from the channel's details
        channelName: "#reports" # optional; a label for the UI, not used to send
      on: [Succeeded, Failed]
      includeOutput: true
```

This uses a [shared chat app](inbound-channels.md) already connected to the tenant, so no webhook is needed for each channel. Notes:

- The app must be **invited to the channel**. It needs the `chat:write` scope; picking channels by name in the UI also needs `channels:read` and `groups:read`. An app created from the UI's setup link has all three.
- No agent has to answer in the channel, and the app's lists of people and places do not limit reports.
- Only apps of the schedule's own tenant are looked up. Anyone who can write schedules straight to Kubernetes can name any of that tenant's apps and any channel the app is in.
- Renaming or deleting the app breaks the reference; so does turning it off. The schedule then has `NotifyReady: False` with the reason.
- `NotifyReady` does not call Slack. A channel the app was not invited to shows up when a message is sent, as a `NotifyFailed` event on the schedule.
- The message looks like the incoming-webhook one, and output cannot mention anyone or make a link. Requests are retried as chat replies are, so a lost connection can post a message twice.
- It needs the database, as chat channels do.

### Changing the channel of a schedule managed in Git

On **Operate > Schedules**, an admin can press **Change channel** on an agent schedule that Git (or `kubectl`) manages and that already has `spec.result.notify`, pick a Slack app and channel, and save. This is the one thing the UI changes about such a schedule.

- **Only the destination is replaced.** `on` and `includeOutput` stay what Git says, and so does everything else. A schedule with no `notify` in Git sends nothing, and the UI cannot add it.
- **The schedule object is not written.** The choice is kept in PostgreSQL (`schedule_destinations`), so a Git sync neither sees nor undoes it. The card says who changed it and what Git says; **Reset to Git** removes it. Both are audited.
- **Git alone no longer says where reports go.** Rebuilding the cluster from Git without the database goes back to Git's destination, so back the database up.
- **If the choice cannot be read, nothing is sent.** The operator does not fall back to Git's destination, which may be one the report was moved away from. A report due then is not retried: the schedule gets a `NotifyFailed` event, and `NotifyReady` is `False` with reason `DestinationUnreadable` until the database answers.
- **Rolling back to a release without this feature** sends reports to Git's destination again, because older operators do not read the table.
- **Deleting the schedule forgets the choice,** so a later schedule with the same name does not inherit it. That includes a delete and re-create by a GitOps tool. The operator does this when it sees the deletion; if a schedule is deleted while the operator is down, the choice stays and applies to a later schedule of that name.
- It does not apply to a schedule the UI owns, which is edited directly. A copy made with **Duplicate** takes Git's destination, not the chosen one.
- `kubectl` shows Git's destination in the spec. `NotifyReady` describes the destination in force and follows a change within about a minute.

### Channels (administrators)

Channels are in the tenant's `TenantConfig`, which only administrators and Git write: the UI can list their names and types and nothing else.

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: TenantConfig
spec:
  notifications:
    channels:
      - name: ops
        type: slack            # webhook | slack | smtp
        secretRef: { name: ops-slack }      # namespace defaults to the TenantConfig's
        # insecure: false                   # allow http:// and SMTP without STARTTLS
        # allowPrivateAddresses: false      # allow a destination on an internal address
```

The destination is in the Secret, never in the spec, because a Slack webhook URL is itself a credential:

| Type | Secret keys | What is sent |
|---|---|---|
| `slack` | `url` (an incoming-webhook URL) | `{"text": ...}` |
| `webhook` | `url`, `signingKey` | A JSON body signed like the [trigger webhooks](integrations.md): `X-AgentOrca-Timestamp` is the Unix time and `X-AgentOrca-Signature` is `sha256=` and the hex HMAC-SHA256 of `<timestamp>.<body>` with `signingKey` |
| `smtp` | `host`, `port` (default 587), `from`, `to` (comma separated), optionally `username` and `password` | A plain-text email. STARTTLS is required (port 465 uses implicit TLS) |

The webhook body has `schedule`, `namespace`, `agent`, `outcome`, `run`, `durationSeconds`, `costUSD`, `link`, `reason`, `time` and, only when asked for, `output`. Every message names the schedule, the agent, the outcome, the run, its duration and cost, and a link to the run in the UI. The link is built from `approvals.uiURL` (the UI address the approval messages already use) and is left out when that is empty.

### When it is sent

| `on` | Sent |
|---|---|
| `Succeeded`, `Failed` | When a run finishes, from the schedule controller. A run the `Replace` policy cancelled is not announced |
| `BreakerOpen` | Once, when the failure breaker stops the schedule |
| `Skipped` | Once, when the schedule's spend cap first stops it, not for every skipped tick. Quota, concurrency and missed ticks are not announced |

Each run is announced **at most once**. The schedule controller claims the run in the same write that counts it (annotation `agentorca.io/schedule-notified`, whose value is the channel, or `slack:<app>/<channelID>`), before anything is sent, so a repeated reconcile, a restart or a second replica does not send it again. The cost is that a message whose delivery fails for good is not retried later.

Delivery is best effort, and never touches the run. It happens in the background with a 10 second timeout per attempt and three attempts with a doubling wait; a client error (4xx other than 408 and 429), a refused address and a bad configuration are final. A failure is an event on the schedule (`NotifyFailed`) with no destination in it.

If the channel is not in the tenant's `TenantConfig`, or its Secret is missing or invalid, the schedule gets the condition `NotifyReady: False` with the reason, and runs go on. No tenant, no channel.

### What a message may contain

- The output is added only when `includeOutput: true`. It is cut to 2000 characters. Anything that matches a secret of the channel (its URL, signing key, password, host) or a credential registered with `internal/redact` is masked first, then the text is cut. This is exact-match masking: it cannot know a secret the operator was never given, so anyone who can read the channel can read the output. Leave `includeOutput` off for private results.
- The failure reason is treated the same way.
- Slack text is escaped (`&`, `<`, `>`), so output cannot make a mention or a link; email headers are single-line.
- The Secret's contents are never in a log line, an event, the status, a condition, an annotation or a message. Errors name a key, never its value.

### Network safety

- `https` is required for `slack` and `webhook` URLs, and STARTTLS for `smtp`, unless the channel sets `insecure: true`.
- The address is checked when the connection is made, after DNS, so a name that resolves to an internal address (or changes after a check) is refused. Loopback, link-local (including the cloud metadata addresses), private (RFC 1918 and IPv6 ULA), carrier-grade NAT, unspecified and multicast addresses are blocked unless the channel sets `allowPrivateAddresses: true`, which an in-cluster mail relay or receiver needs.
- Redirects are not followed, proxies are not used, and a response body is read only to a few kilobytes and never logged.

## Configuration

| Helm value | Default | Meaning |
|---|---|---|
| `operator.schedule.minInterval` | `5m` | The shortest gap between ticks. Checked against the closest pair of firings in the coming year, so `* * * * *` and `0,1 * * * *` are both refused |
| `operator.schedule.maxConsecutiveFailures` | `5` | The failure breaker |
| `approvals.uiURL` | empty | The UI's public address. Also used for the run link in result notifications; empty leaves the link out |
| `operator.leaderElection` | `true` | Keeps the controller on one replica. The deterministic run name is the safety net if it is ever off |

## API and ownership

`AgentSchedule` is a UI-writable kind. Create, update and delete go through the generic `/api/resources/agentschedules` endpoints, so the rules of [GitOps and the UI](gitops-coexistence.md) apply unchanged: a UI-created schedule carries `agentorca.io/owner: ui`, edits are server-side applies with field manager `agent-orca-ui` and a required `resourceVersion` (409 when stale), a GitOps writer is denied on a UI-owned schedule, and a Git-owned schedule is read-only in the UI (Copy YAML, Duplicate). The controller's status writes are never blocked.

| Endpoint | Role | What it does |
|---|---|---|
| `GET /api/schedules` | viewer | Summary list: state, next run (absolute and in the schedule's zone), last result, spend, ownership |
| `GET /api/schedules/preview?cron=&tz=&count=` | viewer | The next firings, and whether the expression is under the minimum interval |
| `GET /api/schedules/{ns}/{name}/runs` | viewer | History, newest first, paged with `limit` and `before` |
| `POST .../suspend`, `POST .../resume` | runner | Edit `spec.suspend` as the UI. Git-owned schedules answer 409: suspend them in Git. Resuming a `Degraded` schedule also clears the failure breaker |
| `POST .../run` | runner | Run now: a normal run started by the caller, passing the same quotas |
| `GET /api/runs?schedule=<name>` | viewer | Runs of a schedule; each run summary carries `schedule` and `trigger` |
| `GET /api/notification-channels` | builder | The names and types of the tenant's notification channels, for the builder's "Send the result to" choice. Read-only: the Secret is never returned and `TenantConfig` is not a UI-writable kind |
| `PUT`, `DELETE .../destination` | admin | Choose the Slack channel of a schedule not owned by the UI, or go back to Git's. `PUT` takes `{app, channelID, channelName}`. 409 for a UI-owned schedule or one without `result.notify`, 422 for an app that cannot be used, 503 without the database |
| create, update, delete | builder | Changing `result.notify` needs admin. A schedule whose `result.egress` has a `secretRef` needs admin, as MCPServer and Tool do (a builder may keep one an admin set) |

Run now creates the run with `agentorca.io/schedule=<name>` but no owner reference and `created-by` set to the person, so it shows in My runs and is not counted in the schedule's history limits, failure streak or spend caps. Scheduled ticks (`created-by: schedule:<ns>/<name>`) are left out of My runs.

Audit rows: `create`, `update`, `delete` on `AgentSchedule` as for other kinds, plus `suspend` and `resume`; Run now is `request` on `AgentRun`.

```
aoctl schedule list | get <name> | suspend <name> | resume <name> | run <name>
```

`aoctl schedule` talks to the UI API (`--ui-endpoint`, `AOCTL_UI_ENDPOINT`, default `http://localhost:8083`) with the same token as the other commands.

## In the UI

Schedules are under **Operate > Schedules** (builder role to create and edit; the API lets a runner pause, resume and run now).

- **The list** shows each schedule in words ("Every weekday at 08:00 (Europe/Berlin) runs the agent digest-agent"), its state with a plain reason (`On`, `Paused`, `Stopped after failures`, `Spend limit reached`, `Agent missing`), the next run in the schedule's zone and yours, the last result (succeeded, failed, or skipped and why), and spend today and this month. **History** lists the last runs, linking to the normal run page, and the skipped times.
- **The builder** asks "When should this run?" with presets (every weekday, every week, every day, every hour, or a custom cron expression) and a time zone picker that defaults to the browser's, with a live list of the next five runs from the preview endpoint. "What should it do?" is the instruction, with a button that inserts `{{schedule.date}}`. "If the last run is still going" chooses skip, run anyway, or stop the old one. "Send the result to" keeps the result on the run, and lets an admin also pick a tenant channel, or a Slack app and one of its channels (by name, or by ID when the app cannot list them), with the events to be told about (a run fails or succeeds, the spend limit stops the schedule, the schedule stops after repeated failures) and a checkbox to include the output. Everyone else sees the destination but cannot change it. "Spend limit" takes a per-run, per-day and per-month amount, and one is required.
- **Git-owned schedules** open read-only with Copy YAML and Duplicate, and are paused in Git. Their card says whether the destination comes from Git or was changed in the UI, and an admin has **Change channel** there; the editor shows what Git says.
- Each agent schedule's card says where it sends messages, and why when they cannot be sent.
- **Agents in the Fleet view** carry a clock badge with the next run and last result, and a Scheduled filter. A run started by a schedule shows a "Scheduled by" chip with the instant it was due and a **Run again now** button. Scheduled runs are left out of **Your recent runs** on Home (the **Latest reports** widget shows what they produced) and have a **Started by: Schedules** filter on the Runs page.
