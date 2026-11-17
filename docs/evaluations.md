# Evaluation datasets

A dataset is a named set of **cases** (an input, an optional reference answer, tags) that belongs to one workspace. Datasets are the first piece of the evaluation work in the evaluations proposal (`docs/proposals/evaluations.md`, not merged yet); experiments, the judge and the publish gate come later. This page covers what exists now: datasets, cases, "add to dataset" from a run, redaction, and deterministic graders.

Datasets live in PostgreSQL, in the tables `eval_datasets` and `eval_cases` (migration `0013`). They carry `tenant_id` with row-level security like every other tenant table (see [tenant isolation](tenant-isolation.md)). They are not CRDs and have no GitOps form; the API is the interface. **Without the PostgreSQL store every endpoint answers 503** with `evaluations need the PostgreSQL store`. A caller with no tenant ID (authentication off) cannot create datasets: a dataset always belongs to a tenant.

## Roles and tenancy

| Action | Role |
|---|---|
| List and read datasets and cases | `viewer` |
| Create, change, delete datasets; add, edit, remove cases; add a run to a dataset; create, replace, delete graders | `builder` |
| Grade a supplied result | `runner` |

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

The byte limit can be lowered, not raised: the table refuses more than 65536 bytes, so a higher value is capped to it. Going over a size limit is a **413**; going over the case limit is a **409** and adds nothing; removed cases do not count towards it. Retention of results (outputs and grades) arrives with the experiment runner and is not part of datasets: cases stay until deleted.

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

A dataset can hold up to 50 **graders**: deterministic checks, run in the operator process on an agent's output and on a summary of its trace. They call no model, cost nothing and touch no network. (The LLM judge and human review are later stages.) The code is `internal/evals/graders`: pure functions, each returning `{pass, score, detail}`. `score` is 0 or 1; `detail` says why, in at most 1 KiB, and never repeats the output.

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

**Not graded.** A grader that needs something the input lacks (only `exact` today: a reference) reports `notGraded: true`, `pass: false`. It is never counted as a pass.

**Case result.** A result passes when every `required` grader passes, and the weighted mean score of the other graders (leaving out those not graded) is at least 0.5. A result with nothing graded does not pass.

### Grader and result endpoints

| Endpoint | Role | Audited |
|---|---|---|
| `GET /api/evals/datasets/{id}/graders`, `GET .../graders/{name}` | viewer | no |
| `PUT /api/evals/datasets/{id}/graders/{name}` `{kind, config, weight?, required?}` (creates, or replaces) | builder | yes |
| `DELETE /api/evals/datasets/{id}/graders/{name}` (also deletes its grades) | builder | yes |
| `POST /api/evals/datasets/{id}/grade` `{caseId?, reference?, arm?, output, toolCalls?, costUsd?, latencyMs?}` | runner | yes |
| `GET /api/evals/datasets/{id}/results` (`?caseId=`, `?limit=` 1 to 200, `?cursor=`), `GET /api/evals/results/{id}` | viewer | no |

`POST .../grade` runs all the dataset's graders on the supplied output and stores the result and one grade per grader (tables `eval_results` and `eval_grades`). With `caseId` (a live case of that dataset) the case's reference is used; otherwise pass `reference`, not both. It answers 409 when the dataset has no graders or already keeps 5000 results, and 413 for an output over 256 KiB. Grader names are DNS labels, unique in the dataset. Changing graders does not change the dataset's version, which is about its cases. A result's `experimentId` column exists for the experiment runner, which does not exist yet.

Tables `eval_graders`, `eval_results` and `eval_grades` (migration `0014`) carry `tenant_id` with the same forced row-level security as the dataset tables, and each child references its parent through a key that includes `tenant_id`, so the database refuses a grader, result or grade under another tenant than its parent's. Another tenant's dataset, grader or result, and anything in a workspace the caller may not use, is a 404. Audit rows use kind `EvalGrader` (name `dataset/grader`) and `EvalResult` and hold names and counts, never an output. Stored outputs are readable by every viewer in the tenant, as cases are; grade only text you would keep in a dataset.

## Operator settings

Helm values under `operator.evals` (environment variables on the operator):

| Value | Variable | Default |
|---|---|---|
| `maxCasesPerDataset` | `EVALS_MAX_CASES_PER_DATASET` | 2000 |
| `maxCaseBytes` | `EVALS_MAX_CASE_BYTES` | 65536 |
| `allowUnredactedCases` | `EVALS_ALLOW_UNREDACTED_CASES` | true |

## Not built yet

Experiments, the LLM judge and human review, the publish gate and the Evaluate screens, and JSONL export of a dataset. They are in the proposal as stages E3 to E6.
