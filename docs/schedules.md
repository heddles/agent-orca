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

## Configuration

| Helm value | Default | Meaning |
|---|---|---|
| `operator.schedule.minInterval` | `5m` | The shortest gap between ticks. Checked against the closest pair of firings in the coming year, so `* * * * *` and `0,1 * * * *` are both refused |
| `operator.schedule.maxConsecutiveFailures` | `5` | The failure breaker |
| `operator.leaderElection` | `true` | Keeps the controller on one replica. The deterministic run name is the safety net if it is ever off |
