# Evaluation datasets

A dataset is a named set of **cases** (an input, an optional reference answer, tags) that belongs to one workspace. Datasets are the first piece of the evaluation work in the evaluations proposal (`docs/proposals/evaluations.md`, not merged yet); experiments, the LLM judge and human review are described below; the publish gate and the screens come later. This page covers what exists now: datasets, cases, "add to dataset" from a run, redaction, deterministic graders, experiments, the judge and review.

Datasets live in PostgreSQL, in the tables `eval_datasets` and `eval_cases` (migration `0013`). They carry `tenant_id` with row-level security like every other tenant table (see [tenant isolation](tenant-isolation.md)). They are not CRDs and have no GitOps form; the API is the interface. **Without the PostgreSQL store every endpoint answers 503** with `evaluations need the PostgreSQL store`. A caller with no tenant ID (authentication off) cannot create datasets: a dataset always belongs to a tenant.

## Roles and tenancy

| Action | Role |
|---|---|
| List and read datasets and cases | `viewer` |
| Create, change, delete datasets; add, edit, remove cases; add a run to a dataset; create, replace, delete graders | `builder` |
| Grade a supplied result; label an item in a review queue | `runner` |
| Read a review queue, a calibration report | `viewer` |
| Close an experiment's review | `builder` |
| Run a gate's evaluation (`POST .../evaluate`) | `builder` |
| Set, change or delete a publish gate; publish with `overrideGate` | `admin` |

A dataset of another tenant is a 404, as is one in a workspace (namespace) the caller may not use. Creating a dataset in a workspace the caller may not use is a 403. Machine callers get `operator.machineDefaultRole` (runner by default) and so cannot write datasets.

Every write adds one row to the audit trail (`GET /api/audit`, admin): kind `EvalDataset`, the workspace and dataset name, the dataset version after the change, and a short summary such as `added 3 cases; version 4`. **Case text never appears in an audit row.** If the audit row cannot be written the response carries `X-Audit-Status: failed`, as for other UI writes.

## The dataset version

`version` starts at 1 and goes up by exactly one for each of these:

| Change | Bump |
|---|---|
| Add cases (one case, or a whole JSON-lines call) | +1 per call, however many cases |
| Edit a case, when at least one field really changes | +1 |
| Remove a case | +1 |

These do **not** change it: a refused request (bad body, over a limit, full dataset), an edit that leaves every field as it was, a change to the dataset's description or agent, and any read. The bump and the change happen in one transaction with the dataset row locked, so concurrent writers get distinct versions and cannot race past the case limit.

A case records `addedIn` (the version that added it) and, once removed, `removedIn`. Removal is soft: the row stays so that an experiment frozen at an older version can be read back. Lists and gets show only live cases. Edits change a case in place; they bump the version, which is what makes an earlier pass stale for the publish gate.

Removed cases keep their text until the dataset is deleted. Deleting a dataset deletes all of its cases, removed ones included.

## Endpoints

All are in `internal/apiserver/schemas/openapi-ui.yaml` (served at `/api/openapi.json`).

| Endpoint | Role |
|---|---|
| `GET /api/evals/datasets` (`?namespace=`, `?agent=`) | viewer |
| `POST /api/evals/datasets` `{namespace?, name, description?, agent?}` | builder |
| `GET /api/evals/datasets/{id}` | viewer |
| `PATCH /api/evals/datasets/{id}` `{description?, agent?}` | builder |
| `DELETE /api/evals/datasets/{id}` | builder |
| `GET /api/evals/datasets/{id}/cases` (`?tag=`, `?limit=` 1 to 200, `?cursor=`) | viewer |
| `POST /api/evals/datasets/{id}/cases` (one case, or JSON lines) | builder |
| `GET /api/evals/datasets/{id}/export` (the live cases as JSON lines; audited as `export`, with the count and no text) | builder |
| `GET /api/evals/cases/{id}` | viewer |
| `PATCH /api/evals/cases/{id}` | builder |
| `DELETE /api/evals/cases/{id}` | builder |
| `POST /api/runs/{namespace}/{name}/dataset` | builder |

A dataset's name is a DNS label, unique per workspace; the workspace and name cannot change. Omitting `namespace` on create uses the caller's first workspace.

### Adding cases and bulk import

`POST .../cases` takes one case as `application/json`:

```json
{"input": "How do I reset my password?", "expected": "Use the Forgot password link.", "tags": ["account"]}
```

or up to **200 cases** as JSON lines (`Content-Type: application/x-ndjson`, or `application/jsonl`), one object per line, blank lines skipped. The call is all or nothing: if line 7 is bad nothing is added and the error says `line 7: ...`. A whole call bumps the version once.

A case has `input` (required), an optional reference given as `expected` or `reference` (not both), `referenceKind`, `tags` and `weight`:

- `referenceKind` is `none`, `exact`, `example` or `rubric`. A reference with no kind is `exact`. A kind other than `none` needs a reference, and `none` allows none.
- `tags`: at most 20, each at most 64 bytes. They are trimmed and de-duplicated.
- `weight` is above 0 and defaults to 1.
- Unknown fields are rejected, so a typo is an error, not silently ignored.

The response is `{added, datasetVersion, cases}`. A manually added case has no source run and `redacted: false`: nothing is redacted from text a person types.

### Limits

| Limit | Default | Setting |
|---|---|---|
| Live cases per dataset | 2000 | `operator.evals.maxCasesPerDataset` |
| Size of a case's `input`, and of its `reference`, each | 65536 bytes | `operator.evals.maxCaseBytes` |
| Cases per call | 200 | fixed |
| Tags per case | 20 | fixed |

The byte limit can be lowered, not raised: the table refuses more than 65536 bytes, so a higher value is capped to it. Going over a size limit is a **413**; going over the case limit is a **409** and adds nothing; removed cases do not count towards it. Cases stay until deleted. Retention applies to results (outputs and grades), see operator settings.

## Add to dataset, from a run

`POST /api/runs/{namespace}/{name}/dataset` copies a run's input into a dataset. The run may be live or archived. Body:

```json
{"datasetId": 7, "useOutput": false, "reference": "", "referenceKind": "", "redact": true, "tags": ["refund"], "preview": false}
```

- It is a **copy**. The case keeps the run's namespace and name (`sourceRunNamespace`, `sourceRun`) as a pointer and the user who captured it (`sourceUser`). The trace and tool results are never copied.
- The **reference is empty by default.** `useOutput: true` takes the run's output as the reference with kind `example`, because a person said it was good; it is refused (409) for a run with no output yet. `reference` (with `referenceKind` `exact` by default, or `example` or `rubric`) writes the expected answer. The two cannot be combined. Taking the output is never automatic, so a bad run cannot become ground truth by accident.
- A run whose input or reference is over the size limit is refused with 413 and a message.
- `preview: true` returns what would be stored, with the original text under `original`, and stores nothing and audits nothing. A client can use it to show a diff.
- The response is `{case, datasetVersion, redaction}`. `redaction` reports, for `input` and `reference`, the number of replacements and the rule names that matched, never the text.
- A run of another tenant, one in a workspace the caller may not use, and one that does not exist are all a **404**, so a caller cannot probe for other tenants' runs.
- The audit summary names the run and the replacement count, for example `added case 12 from run team-a/run-1; version 5; redacted (2 replacements)`.

## Redaction

Cases outlive the runs they came from and are readable by the whole tenant, and run inputs can hold secrets and personal data. So a captured case is redacted by default, and marked `redacted: true`.

What is removed, in this order (the package is `internal/evals/redact`, pure functions with table tests):

| Rule | Replacement |
|---|---|
| Private key blocks (`-----BEGIN ... PRIVATE KEY-----`) | `[REDACTED_PRIVATE_KEY]` |
| JSON Web Tokens | `[REDACTED_TOKEN]` |
| `Bearer <token>` | `Bearer [REDACTED_TOKEN]` |
| API keys: `sk-...` (OpenAI, Anthropic), GitHub tokens, AWS access key IDs, Google `AIza...`, Slack `xox...`, Stripe `sk_live_...`, Hugging Face `hf_...` | `[REDACTED_API_KEY]` |
| Credentials in a URL (`scheme://user:pass@host`) | `[REDACTED_CREDENTIALS]` |
| `password=...`, `api_key: ...`, `token=...`, `secret=...` and similar (value of 6 or more characters) | the value becomes `[REDACTED_SECRET]` |
| E-mail addresses | `[REDACTED_EMAIL]` |
| Card numbers (13 to 19 digits that pass the Luhn check) | `[REDACTED_CARD]` |
| US social security numbers | `[REDACTED_SSN]` |
| The agent's guardrail patterns (below) | the pattern's replacement, else `[REDACTED]` |

**Guardrail patterns.** If the run's Agent names a `GuardrailPolicy`, its regex patterns and inline keywords are applied too: the **input filters** to the case input, the **output filters** to the reference. This holds whatever the filter's action (`redact`, `block` or `warn`): text a policy would block or flag should not be kept in a dataset either. A pattern that is invalid, empty, or over 1024 bytes is skipped (and logged); at most 100 apply. All patterns are RE2, so a hostile pattern cannot cause catastrophic backtracking. If the agent or its policy cannot be read, the built-in rules still apply.

Redaction is **best effort**. It recognises well-known shapes. It does not recognise names, addresses, internal project names that no pattern covers, or a secret written in free prose. People should still read a case before relying on it, and a UI should say so.

Editing a case's input or reference clears its `redacted` flag: the text is no longer what redaction produced.

### Keeping the original text

`redact: false` keeps the text as is. It needs the same `builder` role, is written to the audit trail as `kept unredacted`, and the case shows `redacted: false`. An operator can forbid it:

```yaml
operator:
  evals:
    allowUnredactedCases: false   # redact:false is then a 403
```

## Graders

A dataset can hold up to 50 **graders**: deterministic checks, run in the operator process on an agent's output and on a summary of its trace. They call no model, cost nothing and touch no network. (The `llm` and `human` kinds, which do, are described under [the LLM judge](#the-llm-judge) and [human review](#human-review).) The code is `internal/evals/graders`: pure functions, each returning `{pass, score, detail}`. `score` is 0 or 1; `detail` says why, in at most 1 KiB, and never repeats the output.

| Kind | Config | Passes when |
|---|---|---|
| `exact` | `{normalize?, value?}`, `normalize` is a list of `trim`, `collapseWhitespace`, `lowercase` | the output equals the case reference (or `value`) after normalising. With no reference the grader is **not graded** (see below). |
| `contains` | `{value, caseInsensitive?}` | the output contains `value` |
| `not_contains` | `{value, caseInsensitive?}` | the output does not contain `value` |
| `regex` | `{pattern, mustMatch?}` (default true) | the pattern matches the output (or, with `mustMatch: false`, does not) |
| `json_schema` | `{schema}` | the output is JSON (or holds a fenced JSON block) that validates against the schema |
| `json_field_equals` | `{path, value}` | the JSON in the output has `value` at `path`, a dotted path of object keys and array indexes (`items.0.name`) |
| `tool_called` | `{tool}` | the run called the tool at least once |
| `tool_not_called` | `{tool}` | the run never called the tool |
| `max_cost` | `{usd}` | the run's cost is at most `usd` |
| `max_latency` | `{ms}` | the run's latency is at most `ms` |

**Bounds.** A regex is RE2 (Go `regexp`): matching time is linear in the input, there is no backtracking, no lookahead and no backreference. The pattern is at most 1 KiB. A schema is at most 64 KiB and cannot load anything: a `$ref` that points outside the schema (a URL or a file) is refused when the grader is saved. Output and reference text over 256 KiB is **not graded**: the grader fails with a detail saying so. It is not truncated, because a prefix could make a `not_contains` or `exact` grader pass wrongly. `contains` and `not_contains` values, JSON paths and JSON values are at most 4 KiB. A grader with an invalid config is refused (400) when saved; one that has somehow gone bad grades as a failure and never panics.

**Not graded.** A grader that needs something the input lacks (a reference for `exact`, or for an `llm` grader in the `reference` mode) reports `notGraded: true`, `pass: false`. It is never counted as a pass. The `llm` and `human` kinds are not run by `POST .../grade`: they come back not graded, because only an experiment has a judge and a review queue.

**Case result.** A result passes when every `required` grader passes, and the weighted mean score of the other graders (leaving out those not graded) is at least 0.5. A result with nothing graded does not pass. A grade that is not `ok` (below) counts as follows: `invalid` and `error` are failures with score 0, never passes; `pending_review` is not graded yet, which fails a required grader and is left out of the mean for the others.

**Grade status.** Every grade has a `status`: `ok` (it counts), `error` (the grader could not run: no judge, no budget, a trace that was never archived), `invalid` (a judge reply that was not usable) or `pending_review` (waiting for a person). In an experiment's comparison an `invalid` grade counts as a failed grade; an `error` or `pending_review` grade leaves the result out of the comparison (the summary says how many), so a result is judged on all of its graders or not at all.

### Grader and result endpoints

| Endpoint | Role | Audited |
|---|---|---|
| `GET /api/evals/datasets/{id}/graders`, `GET .../graders/{name}` | viewer | no |
| `PUT /api/evals/datasets/{id}/graders/{name}` `{kind, config, weight?, required?}` (creates, or replaces) | builder | yes |
| `DELETE /api/evals/datasets/{id}/graders/{name}` (also deletes its grades) | builder | yes |
| `POST /api/evals/datasets/{id}/grade` `{caseId?, reference?, arm?, output, toolCalls?, costUsd?, latencyMs?}` | runner | yes |
| `GET /api/evals/datasets/{id}/results` (`?caseId=`, `?limit=` 1 to 200, `?cursor=`), `GET /api/evals/results/{id}` | viewer | no |

`POST .../grade` runs all the dataset's graders on the supplied output and stores the result and one grade per grader (tables `eval_results` and `eval_grades`). With `caseId` (a live case of that dataset) the case's reference is used; otherwise pass `reference`, not both. It answers 409 when the dataset has no graders or already keeps 5000 results, and 413 for an output over 256 KiB. Grader names are DNS labels, unique in the dataset. Changing graders does not change the dataset's version, which is about its cases.

Tables `eval_graders`, `eval_results` and `eval_grades` (migration `0014`) carry `tenant_id` with the same forced row-level security as the dataset tables, and each child references its parent through a key that includes `tenant_id`, so the database refuses a grader, result or grade under another tenant than its parent's. Another tenant's dataset, grader or result, and anything in a workspace the caller may not use, is a 404. Audit rows use kind `EvalGrader` (name `dataset/grader`) and `EvalResult` and hold names and counts, never an output. Stored outputs are readable by every viewer in the tenant, as cases are; grade only text you would keep in a dataset.

## Experiments

An experiment runs 2 to 4 **arms** over a dataset at its current version (frozen when the experiment is created: later edits to the dataset do not change it), grades every result with the dataset's deterministic graders, and compares the arms. An arm is `{label, agent, revision?, hash?, modelProvider?}`:

- *revision comparison*: the same agent, two saved revisions (a number from the agent's revision history). An arm with no revision runs the live Agent. The runner hashes the spec each arm runs (SHA-256 of its canonical JSON) when the experiment starts and stores the hash in the arm, so a result names a precise artifact. A revision arm is read from the tenant's revision history; if the arm carries a `hash` that the stored revision no longer has, the request is a 422.
- *model comparison*: the same agent on a different `modelProvider`. The runner makes a one-provider ModelSelector for it.
- The first arm is the baseline.

The dataset needs at least one grader (409 otherwise). Experiments need PostgreSQL and `operator.evals.enabled`; otherwise the endpoints answer 503.

| Endpoint | Role |
|---|---|
| `POST /api/evals/experiments` `{datasetId, name?, arms, repeats?, budgetUsd, runBudgetUsd?, timeoutSeconds?, concurrency?}` | builder, audited |
| `GET /api/evals/experiments` (`?namespace=`, `?datasetId=`, `?status=`, `?limit=`, `?cursor=`) | viewer |
| `GET /api/evals/experiments/{id}` | viewer |
| `GET /api/evals/experiments/{id}/results` (`?arm=`, `?caseId=`, `?limit=`, `?cursor=`) | viewer |
| `GET /api/evals/experiments/{id}/summary` | viewer |
| `POST /api/evals/experiments/{id}/cancel` | builder, audited |

Another tenant's experiment, or one in a workspace the caller may not use, is a 404. Audit rows (kind `EvalExperiment`) hold the experiment ID, dataset name, version and counts, never case or output text. Defaults: 1 repeat (at most 5), run timeout 600 s, concurrency 4 (at most 16), per-run cap `min($1, budget)`.

### Before it runs

The create call checks what each arm's agent names (its ModelSelector, Tools, KnowledgeBases, GuardrailPolicy, and a model arm's ModelProvider) with the same lookup as "use a template". A gap is a **422** with `{"error", "missing": [{arm, kind, name, namespace, alternatives}]}`. The runner repeats the check when it starts the experiment, because the cluster may have changed; a gap then fails the experiment with the message in `error`.

### How it runs

A leader-only runner in the operator polls `eval_experiments` (`FOR UPDATE SKIP LOCKED`, with a lease, so two operators never run one experiment). For each experiment it:

1. makes a hidden Agent `eval-<id>-<arm>` per arm from the resolved spec (and a ModelSelector of the same name for a model arm). They carry the tenant ID, `agentorca.io/owner: ui`, `agentorca.io/eval=true`, `agentorca.io/eval-experiment` and `agentorca.io/eval-arm`, and are left out of `GET /api/agents`, the catalog and the fleet. A user's own object of the same name is never overwritten: the experiment fails instead;
2. creates one `AgentRun` per (case, arm, repeat) named `eval-<id>-<arm index>-<case>-<repeat>` (a function of the slot, so a crash cannot make two), labelled `agentorca.io/eval=true`, `eval-experiment`, `eval-arm`, `eval-case`, `eval-repeat`, `tenant-id` and `tenant`. Runs carry **no callbacks and no egress**, so nothing reaches a sink. Work goes out by case, then repeat, then arm, so a stopped experiment still has paired data;
3. copies each finished run into `eval_results` (output, tool calls, spend, cost completeness, latency, phase), runs the graders (the judge included) and stores the grades. The result tables are written in the experiment's own tenant scope, never the platform scope;
4. ends when every slot has a result, the budget is spent, or a cancel arrives, grades the pairwise comparisons, writes the `summary`, and deletes the hidden Agents and ModelSelectors. A dataset with a `human` grader sends the experiment to `awaiting_review` first (see [human review](#human-review)).

**Tool calls.** The tool names a run called are read from the run's archived trace (the `toolCall` events the model-router writes; calls made inside a child run, an agent used as a tool, belong to the child and are not listed) and stored in the result's `toolCalls`, in order with repeats, at most 1000. The operator archives a run shortly after it ends, so when a dataset has a `tool_called` or `tool_not_called` grader the runner waits up to two minutes for the archive and records the run on a later tick; it does not start a retry run meanwhile. If the trace never arrives (the state store is off, or the stream expired) those graders get an `error` grade, never a pass, and the case does not pass. Without such graders no wait happens and `toolCalls` holds whatever the archive had.

**Budget.** `budgetUsd` is a hard cap on the experiment's runs. Each run gets `spec.budget.maxUSD = min(runBudgetUsd, remaining / (runs in flight + 1), remaining minus the caps already given to runs in flight)` with `onExceeded: fail` (never `pause`: nobody is there to approve). When nothing is left the experiment completes with `summary.stoppedBy = "budget"`. The router can overshoot a run's cap by one call, so the cap carries that slack. Experiment spend also counts against the tenant's `budgetPerDayUSD`: an experiment whose budget is more than what is left today does not start (status `failed`, with the reason), and a running one stops creating runs with `stoppedBy = "daily-budget"` when what is left falls below its remaining budget.

**Concurrency.** At most `concurrency` runs at once, never more than 16 eval runs for a tenant, and never more than the tenant's `rateLimit.concurrentRuns` when set. At most two experiments per tenant are queued or running (a third is a 409). The tenant's `requestsPerMinute` bucket is consulted once per run created.

**Failures.** A run that fails or times out is recorded with that phase and scores as a failed case: a failing agent is data. When the platform fails the run (pod not found, evicted, unschedulable, router or sandbox setup), the runner retries it once (`...-r1`) and then records the result as `Skipped`; the comparison drops that case from every arm, so it stays paired.

**Cancel.** `POST .../cancel` ends a queued experiment at once. For a running one the runner records finished runs, records the others as `Cancelled` (with the spend so far), deletes them, and ends the experiment `cancelled` with a partial summary of the paired data.

**Restart.** All state is in PostgreSQL and in the runs (found by label), so a restarted runner resumes: finished runs are recorded, in-flight ones are adopted, none is created twice. On start a sweep deletes the hidden objects, and any running AgentRun, of experiments that have ended or no longer exist.

### The summary

When an experiment ends `summary` holds `stoppedBy`, `partial`, `results`, `skipped`, `spentUsd` and the paired comparison of each arm against the first, from `internal/evals/compare`. It also holds `graders`, one entry per grader with the grades that counted (`graded`, `passed`), `notGraded`, `invalid`, `errors`, `pendingReview`, `inconsistent` (pairwise ties from a judge that disagreed with itself) and `judgeCostUsd`, plus `judgeSpentUsd` and `reviewPending`. While an experiment is `awaiting_review` the summary is an interim one that leaves out the results still pending; it is made again when the review ends.

`GET /api/evals/experiments/{id}/summary` returns it as `{experimentId, status, live, partial, summary}`. Once the experiment has ended it is the stored summary. Before that (`running`, `grading`, `awaiting_review` without one) it is worked out from the results so far, with `live: true` and `partial: true`; a queued experiment or one that failed before it had a summary is a 409. `partial` is also true for an ended experiment that stopped early (a cancel, the budget). What it holds, per `compare.Summary`:

- **Per arm:** `cases` with a usable result, `skipped`, `passed`, `passRate` with its Wilson interval (`passRateCi`), `meanScore`, `failureRate`, cost (`costTotalUsd`, `costMedianUsd`, `costIncomplete`), latency p50 and p95, and the repeat `stability`.
- **Per candidate arm against the first:** the paired `passRate` difference with counts (`improved`, `regressed`, both pass, both fail), the exact McNemar p-value on the discordant pairs, the Newcombe interval, mean paired differences with a bootstrap interval for `score`, `cost` and `latency`, the paired median difference and relative change for cost and latency, the verdict (`better`, `worse`, `no clear difference`, `not enough data`, with `nonInferior`) and the lists of regressed, improved and skipped case IDs.
- **Left out:** `excludedError` and `excludedPending`, on each arm and in total.

**How a case passes in the comparison.** It is the rule stored in `eval_results.case_pass` (`graders.CasePass`), applied to the grades: every `required` grader passes and the weighted mean score of the other graders is at least 0.5 (a weight of 0 counts as 1). So the comparison and the case verdict shown on a result agree. With repeats a case passes in an arm when more than half of its usable repeats pass. A run that failed or timed out is a failed case. An `invalid` judge reply is a failed grade with score 0.

**What is left out, and counted.** A succeeded result with a grade in `error` (the grader could not run) or `pending_review` (a person has not labelled it) is left out of every figure of its arm, and so is its case from every comparison that involves that arm, because a result is judged on all its graders or on none. Nothing is guessed for it. The summary counts them (`excludedError`, `excludedPending`) so a number is never read without knowing how many results it leaves out; `skipped` counts the cases this leaves with nothing usable. Cancelled and skipped runs are left out the same way.

### Left out of production figures

Runs labelled `agentorca.io/eval` are not production traffic. They are left out of: the cost view (`GET /api/costs`), the run list (`GET /api/runs`), run history (`GET /api/runs/history`), the fleet view, "my runs", and routing-benchmark shadow sampling (an eval run is never sampled, whatever a benchmark's `excludeRunSelector` says). `?includeEval=true` on the cost view, the run list and run history puts them back. What experiments cost is reported on its own: `GET /api/costs` has an `evaluations` line (`totalUSD`, `experiments`) that is not part of `totalUSD` or any other figure.

## The LLM judge

A grader of kind `llm` has a model grade each output. It is **not** the Switchyard routing classifier: it is a new judge, which follows the shadow judge's pattern (a named `ModelProvider`, a reply limit, a rubric) and calls the provider through the same code path (`router.CompleteOnce`, which uses the shadow judge's `forwardToProviderWithin`). No new network route is added: the operator makes the call, as the model-router does, with the provider's own prices. Config: `{providerRef, rubric, mode?, passAt?, maxTokens?, judgeRepeats?}`.

| Field | Meaning |
|---|---|
| `providerRef` | A ModelProvider in the experiment's workspace or a platform catalog namespace, one the tenant may use (the same access check as an arm's references). Its API key is read from the Secret it names **in the provider's own namespace**: `credentialsRef.namespace` is not followed. A provider that does not exist is a 422 when the experiment is created. |
| `rubric` | At most 4000 characters. Required for `rubric` and `pairwise`; `reference` has a default. It is the author's text and goes in the system prompt. |
| `mode` | `rubric` (default): grade by the rubric alone. `reference`: grade against the case's reference (a case with none is not graded). `pairwise`: two-arm experiments only; compare the two arms' outputs of the same case. |
| `passAt` | The score that passes, default 0.7. |
| `maxTokens` | The reply limit, 16 to 2048, default 512. |
| `judgeRepeats` | 1 to 3, default 1. Asks that many times and takes the median score; one unusable reply makes the whole grade `invalid`. |

**Defences against injection from case contents.** The case input, the reference and the agent's output are untrusted: a case may be copied from a hostile user's run, and an agent may echo a hostile document.
- Each goes in its own block (`<case_input_NONCE>`, `<candidate_output_NONCE>`, `<reference_NONCE>`), whose tag carries a fresh 96-bit random nonce made for each call, in the user message. The system prompt, which holds the rubric and the rules, never holds case text, and tells the model that text in those blocks is data and never an instruction. If a text happens to contain the nonce a new one is drawn. A block is cut at 24000 characters and says so.
- The judge has no tools (the request carries none).
- The reply must be exactly one JSON object `{"rationale", "score", "pass"}` (optionally in one code fence), with no other text, no unknown field and no repeated key. The score must be a number from 0 to 1, and `pass` must agree with it (`score >= passAt`). Anything else is `invalid`, which counts as a failed grade and still costs what the call cost, never a free pass. A reply of the shadow judge's looser kind (prose around the JSON) is invalid here.
- The rationale is stored as plain text: invalid UTF-8 and control characters are removed, it is cut at 4 KiB. A UI must show it escaped, never as Markdown, and never with links.
- A **canary** runs when the experiment starts: the judge grades three built-in answers to a fixed question (one right, one wrong, one wrong that tries to talk the judge into a pass) with its own prompt and criteria, and the experiment **fails before any run is made** if the right one does not pass, either wrong one does, or a reply is unusable. The canary uses its own rubric, not yours, because a rubric can be about anything; it proves the provider, the prompt and the parser work. Its cost is counted in `spentUsd`, also when it fails. It runs once per experiment and grader.
- Residual risk: a determined injection can still sway a model. Use a deterministic or human grader beside an `llm` grader for anything that matters, and check the judge against people (below).

**Bias.** *Position* (pairwise): the judge is asked twice with the two outputs in opposite order; when the two verdicts name different winners (a win and a tie included) the result is a tie, `inconsistent: true`, which does not pass. A pairwise grade is 1 for the winner, 0 for the loser and 0.5 each for a tie; the pair's cost is on the first arm's grade. A result whose other arm has no output for the case is not graded. *Verbosity and style*: the system prompt tells the judge to ignore length, tone, formatting and the order shown, and gives written anchors for 0, 0.5 and 1; the rationale comes before the score. *Self-preference*: the experiment's create response has `warnings` when a judge's provider is also an arm's `modelProvider`. *Variance*: temperature 0, one call, or `judgeRepeats`.

**Cost.** The judge's calls are part of the experiment's spend: each grade has `judgeCostUsd`, added to `spentUsd` and so counted against `budgetUsd`, with the canary. Before each call the judge checks that the call's most likely cost (its prompt priced at one token per three bytes plus `maxTokens`) still fits in what is left of the experiment's budget and of the tenant's `budgetPerDayUSD` (today's runs, today's other judge spend and the canaries all counted); if not, the grade is an `error` ("the budget has no room for the judge") and the model is not called. The judge never touches a run's own budget. The calls are made one after another by the runner, so a large dataset with a slow judge takes time.

## Human review

A grader of kind `human` has a person label each output: `{scale?, instructions?, passAt?, sample?}`. `scale` is `passfail` (default) or `1to5` (a label n is the score (n-1)/4 and passes at `passAt`, default 0.75, so 4 and 5). `sample` is how many **cases** to send to review, chosen at random (0 sends all); both arms' results of a sampled case are reviewed, and a result of a case left out simply has no grade from that grader.

When the runs are all in, an experiment whose dataset has a human grader moves to **`awaiting_review`**: its hidden objects are deleted, each human grade is `pending_review`, and one **assignment** per result and grader (table `eval_review_assignments`, migration `0016`) goes in the queue, ordered by a random key made when the item is created. The runner keeps the experiment until no item is pending, then ends it `complete` with the final summary. Pending results are **left out of the comparison and of the human grader's rate, never counted as passes**, and a result whose human grader is `required` does not pass until it is labelled.

| Endpoint | Role |
|---|---|
| `GET /api/evals/experiments/{id}/review` (`?status=pending\|done\|closed`, `?limit=` 1 to 50) | viewer |
| `GET /api/evals/review/{id}` | viewer |
| `POST /api/evals/review/{id}` `{pass?, stars?, comment?}` | runner, audited |
| `POST /api/evals/experiments/{id}/review/close` | builder, audited |

The queue is **blind**. An item holds the case input, the reference if there is one, the output, the grader's name, scale and instructions, and nothing that says which arm, repeat or run the output came from, or what it cost or called; its ID is the assignment's, not the result's, and the list is in the random order. The label is one per item (a second is a 409), the comment at most 2 KiB. A label turns the pending grade into an `ok` grade with `gradedBy` and works the result's case verdict out again. Closing the review marks every pending item `closed`, leaves their grades `pending_review`, and lets the runner end the experiment. Audit rows (kind `EvalReview`, name the experiment ID) hold the grader's name and counts, never the output or the comment.

What blinding does not cover: the output text can give an arm away ("As the cheap model..."), and anyone with the viewer role can still read the experiment's results, which name the arms. The queue hides the arm from the review screens; it is not an access control on the results. With `operator.evals.requireIndependentReview` the person who started the experiment cannot label its items (403).

### Calibration

`GET /api/evals/datasets/{id}/calibration?llm=<grader>&human=<grader>` (viewer; `&experimentId=` limits it to one experiment) compares an `llm` grader with a `human` grader over the results both graded with an `ok` grade: the share they agree on, **Cohen's kappa**, the 2x2 confusion table and the disagreements (the biggest score gaps first, at most 100), so an author can fix a rubric. It needs at least **20** such results (`tooFewPairs` otherwise). A kappa under **0.4** sets `trustworthy: false`; so does a kappa that is undefined because one side gave the same verdict throughout (agreement of 100% on a single class says nothing about the judge). The code is `internal/evals/calibrate`. The publish gate uses the same rule: a gate that passes on the numbers but rests on an `llm` grader with no human grader that agrees with it (kappa of at least 0.4 over at least 20 results) is `untrusted_judge`, a 409 that an admin may publish past with an override and a reason, which is audited.

## Feeding routing benchmarks

A model-comparison experiment is stronger evidence for a routing decision than shadow samples: whole tasks, a reference, no production traffic. `RoutingBenchmark.spec.evaluation.includeExperiments` (default `false`) lets the benchmark's findings read it. It is a read path, nothing is copied, and with the flag off the findings are exactly what they were.

With the flag on, the routing evaluation loads, besides the shadow samples, the **finished (`complete`) experiments in the benchmark's window** (by finish time) in the benchmark's workspace, and projects each into the shape the shadow samples have:

- Only an experiment whose **first (baseline) arm names a `modelProvider`** counts, for an Agent the benchmark covers. The baseline plays the *served* model; each other arm of the **same agent** with a different `modelProvider` is a *shadow* of it. An experiment whose baseline runs the live agent names no model that "served", and is left out.
- Each case that has a usable result in both arms is one pair. The category is the case's tag, `tag:<tag>` (one pair per tag; `tag:untagged` for a case with none). The winner is the arm that passed the case alone, a tie when both did the same; it uses the same case outcomes as the experiment's own comparison, so results waiting for review or in error are left out. The cost of each side is the case's mean cost in that arm.
- A category such as `tag:billing` is not a routing stage, so a finding built from it is reported and never proposes a patch to the selector (only `stage:` categories can be routed). The routing-score detectors (threshold, strategy switch) do not read experiment pairs, which have no routing score.
- A finding that rests on win-rate samples says how many of them came from experiments in `status.evidence.experimentSamples` (absent when none). A recommendation still never applies itself.

## The publish gate

A gate makes an agent pass an evaluation before **Publish** (`POST /api/agents/{ns}/{name}/publish`) creates or updates its AgentDeployment. It is **off unless an agent has an enabled gate**: a workspace that sets none publishes exactly as before. The gate is a row in `eval_gates` (migration `0017`, tenant row-level security like the other evaluation tables), not a field of the Agent, because anyone who can edit the Agent could then lower it.

**Configuration**, `PUT /api/evals/gates/{ns}/{agent}` (admin; `GET` is viewer, `DELETE` admin), audited as kind `EvalGate` with the old and the new numbers:

| Field | Meaning |
|---|---|
| `datasetId` | The dataset the evaluation runs on. It needs a deterministic or human grader: a dataset with only `llm` graders is a 409, because a judge can be swayed by the output it grades. |
| `baselineRevision` | The saved revision of the agent the candidate is compared with (the version that is live). Required. |
| `margin` | The non-inferiority margin on the pass rate, default 0.05: the low end of the interval for candidate minus baseline must be above minus the margin (the `nonInferior` of the verdict). |
| `minCases` | The fewest paired cases an evaluation needs, default 30, at most 2000. |
| `minPassRate`, `maxRegressions` | Optional extra bars: a floor on the candidate's own pass rate, and the most cases that may go from pass to fail. |
| `budgetUsd` | The budget of the experiment `evaluate` starts, default 5. |
| `enabled` | Default true. |

The dataset a gate rests on cannot be deleted (409) until the gate is changed or removed.

**Running the evaluation**, `POST /api/agents/{ns}/{name}/evaluate` (builder, audited): queues an experiment with `purpose: gate`, two arms: `baseline` (the gate's revision) and `candidate` (the agent as it is now, pinned to the hash of its spec, so an edit before the runner starts it refuses the experiment rather than evaluating something else), on the gate's dataset at its current version. It answers 201 with the experiment, and 409 when the agent has no gate.

**Enforcement.** Publish calls `gate.CheckGate` (`internal/evals/gate`) before it changes anything. The latest `complete` gate experiment of the agent decides, not the best one, so a pass cannot be had by rerunning until lucky. It counts only when:

1. its candidate arm ran the **hash of the spec being published**;
2. it ran on the gate's dataset at the dataset's **current version** (every edit of the cases raises the version);
3. the dataset's **graders** have not changed since (a grader added, edited or removed changes the bar, and does not change the dataset's version, so this is checked on its own);
4. no candidate result is still waiting for a review;
5. it compared at least `minCases` cases, the candidate is non-inferior to the baseline by `margin`, and the optional `minPassRate` and `maxRegressions` hold.

If so, publish goes on. If not, the answer is **409** `{"error", "gate": {"status", "reason", "required", "best", "experimentId"}}` with `status`:

- `missing`: no finished gate experiment;
- `stale`: the latest one no longer applies (another agent version, another dataset or dataset version, changed graders, the dataset is gone);
- `failed`: it applies and does not meet the bar (`best` has the cases, pass rates, the low end of the difference and the regressions).

`GET /api/evals/gates/{ns}/{agent}` returns the gate and the same decision, so a screen can say why Publish is blocked. If the gate or its evidence cannot be read, publish stops with a 503: the gate fails closed. Unpublish (`DELETE .../publish`) is never gated.

**Override.** `POST .../publish` with `{"overrideGate": {"reason": "..."}}` (10 to 500 characters) publishes past a failing gate. It needs the **admin** role: anyone else who sends `overrideGate` gets a 403, whatever the gate says. An override is written to the audit trail with the action `override` (kind `Agent`), and a summary such as `gate override (failed) on dataset support-core v7: needs a pass rate no more than 5 points below the baseline over 30 cases; experiment 42 had 81% against 90% over 60 cases; reason: ...`, within the 1 KiB audit limit; the normal publish row is written too. An admin who sends `overrideGate` for a gate that passes just publishes, with no override row. With `operator.evals.allowGateOverride: false` an override is a 403 and only changing the gate (itself audited) unblocks a publish.

**What the gate does not cover.** It gates the Publish action. It does not stop a direct `PUT /api/resources/Agent/...` or a `kubectl` edit of an Agent that an AgentDeployment already runs. "Require review before publish" and the gate are independent; both must pass.

## Operator settings

Helm values under `operator.evals` (environment variables on the operator):

| Value | Variable | Default |
|---|---|---|
| `maxCasesPerDataset` | `EVALS_MAX_CASES_PER_DATASET` | 2000 |
| `maxCaseBytes` | `EVALS_MAX_CASE_BYTES` | 65536 |
| `allowUnredactedCases` | `EVALS_ALLOW_UNREDACTED_CASES` | true |
| `enabled` | `EVALS_ENABLED` | true |
| `maxBudgetPerExperimentUSD` | `EVALS_MAX_BUDGET_PER_EXPERIMENT_USD` | 25 |
| `maxCasesPerExperiment` | `EVALS_MAX_CASES_PER_EXPERIMENT` | 500 |
| `maxActiveExperimentsPerTenant` | `EVALS_MAX_ACTIVE_EXPERIMENTS_PER_TENANT` | 2 |
| `runnerIntervalSeconds` | `EVALS_RUNNER_INTERVAL_SECONDS` | 5 |
| `requireIndependentReview` | `EVALS_REQUIRE_INDEPENDENT_REVIEW` | false |
| `allowGateOverride` | `EVALS_ALLOW_GATE_OVERRIDE` | true |
| `resultRetentionDays` | `EVALS_RESULT_RETENTION_DAYS` | 90 |

`resultRetentionDays`: the leader operator sweeps hourly and, for results older than this many days whose experiment is over, deletes their grades and empties their output and tool calls. The result row (cost, latency, pass flag) stays, and results of an experiment still running or awaiting review are never touched. `0` keeps everything. Datasets and cases are not affected.

## Not built yet

"Check with my cases" on a routing recommendation. Known limits of the runner: the review stage has no "disagreement only" sampling (`sample` picks cases at random); judge calls are made one at a time by the runner; a judge can overshoot the experiment budget by what the runs in flight already held; routing telemetry of eval runs (`routing_events`) is not yet filtered; and the form that makes a builder acknowledge an arm's write and actuate tools is not built. Known limits of the gate: it evaluates the agent as saved on the cluster, so an agent that a person has edited in the UI but not saved is not what is evaluated.
