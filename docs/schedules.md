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
