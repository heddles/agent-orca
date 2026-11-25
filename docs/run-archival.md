# Run archival

A finished run's answer and its trace are kept in Redis until the run is archived to PostgreSQL. This page says when that happens, what happens when it fails, and what to watch.

Archiving needs the database (`database.enabled`). Without it nothing is archived, a run's answer stays in Redis for 30 days, and a run is deleted as soon as it is asked to be.

## What is stored where

| Data | Where | How long |
|---|---|---|
| The answer of a run | Redis (`run-output` KV, tenant-scoped, compressed, up to 1 MiB) | 30 days |
| The execution trace | Redis token stream (the run's and its children's) | 24 hours |
| The run summary, answer and trace | PostgreSQL `archived_runs` | until [archived-run retention](../charts/agent-orca/values.yaml) (`database.archivedRunsRetentionDays`, off by default) |
| The AgentRun | Kubernetes (etcd). `status.output` is empty for new runs, so an answer is never written to etcd. | until deleted |

If Redis cannot take an answer, a copy clipped to 10 KB stays in `status.output`. Nothing is lost on that path, but a long answer is cut.

## When a run is archived

The reconcile of a finished run (Succeeded, Failed or HandedOff) is the only archiver.

1. It waits until the run's spend is known, for at most ten minutes, so one archive row carries the final cost.
2. It reads the answer and trace from Redis and writes the row (an idempotent upsert keyed by namespace and name).
3. It sets the `Archived` condition on the run: one status write per run.

If the write fails, the reconcile returns an error and controller-runtime retries it with exponential backoff (a few milliseconds up to about 16 minutes between attempts) until PostgreSQL is back. A `Warning` event `ArchiveFailed` is recorded on the run. Nothing is written to the run until the archive succeeds.

A run that has the `Archived` condition is not archived again. A run from before this was added has no condition and is archived once, after an upgrade; the upsert keeps what the archive already holds.

Set the annotation `agentorca.io/skip-archive: "true"` on a run to keep it out of the archive and out of the deletion wait.

## Deleting a run

A finished run keeps its finalizer until it is archived. When PostgreSQL is down, the deletion waits and tries again every minute (event `ArchiveBlocksDeletion`).

It waits for `database.archiveDeleteGrace` (default `24h`, measured from the delete request). After that the run is deleted without an archive: its answer and trace are gone, the event `ArchiveAbandoned` is recorded, and `agentorca_run_archive_total{result="abandoned"}` goes up. Schedules delete finished runs through their history limits, so this wait applies to them too.

## What can still be lost

- **An answer that expires before the run is archived.** Answers last 30 days, traces 24 hours. If PostgreSQL is down for more than 24 hours, runs archived afterwards have no trace. If it is down for more than 30 days, they have no answer: the archive row then says "No answer is stored for this run" (and `agentorca_run_archive_output_missing_total` counts it).
- **Redis loses data.** The bundled Redis writes an append-only file to a persistent volume, so a restart keeps its data. `redis.maxmemory` (default `384mb`, about 75% of the 512 Mi limit) with `volatile-ttl` makes Redis evict the keys closest to expiry, traces first, instead of being OOM-killed. Raise both together if you raise the limit.
- **A deletion past the grace period** (above).

## Metrics and alerts

| Metric | Meaning |
|---|---|
| `agentorca_run_archive_total{result}` | `archived`, `failed` (will be retried), `abandoned`, `skipped` |
| `agentorca_run_archive_seconds` | time to archive one run |
| `agentorca_run_archive_pending` | finished runs not archived ten minutes after they finished |
| `agentorca_run_archive_oldest_pending_seconds` | age of the oldest of those |
| `agentorca_run_archive_output_missing_total` | succeeded runs archived with no stored answer |

`metrics.prometheusRule.enabled=true` installs two alerts (Prometheus Operator): `AgentOrcaRunsNotArchived` (pending runs for 15 minutes) and `AgentOrcaRunArchiveAbandoned` (a run was deleted without an archive).
