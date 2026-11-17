# Evaluation datasets

A dataset is a named set of **cases** (an input, an optional reference answer, tags) that belongs to one workspace. Datasets are the first piece of the evaluation work in the evaluations proposal (`docs/proposals/evaluations.md`, not merged yet); graders, experiments and the publish gate come later. This page covers what exists now: datasets, cases, "add to dataset" from a run, and redaction.

Datasets live in PostgreSQL, in the tables `eval_datasets` and `eval_cases` (migration `0013`). They carry `tenant_id` with row-level security like every other tenant table (see [tenant isolation](tenant-isolation.md)). They are not CRDs and have no GitOps form; the API is the interface. **Without the PostgreSQL store every endpoint answers 503** with `evaluations need the PostgreSQL store`. A caller with no tenant ID (authentication off) cannot create datasets: a dataset always belongs to a tenant.

## Roles and tenancy

| Action | Role |
|---|---|
| List and read datasets and cases | `viewer` |
| Create, change, delete datasets; add, edit, remove cases; add a run to a dataset | `builder` |

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

## Operator settings

Helm values under `operator.evals` (environment variables on the operator):

| Value | Variable | Default |
|---|---|---|
| `maxCasesPerDataset` | `EVALS_MAX_CASES_PER_DATASET` | 2000 |
| `maxCaseBytes` | `EVALS_MAX_CASE_BYTES` | 65536 |
| `allowUnredactedCases` | `EVALS_ALLOW_UNREDACTED_CASES` | true |

## Not built yet

Graders, experiments, the judge, the publish gate and the Evaluate screens, and JSONL export of a dataset. They are in the proposal as stages E2 to E6.
