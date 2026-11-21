/**
 * "Check with my cases": runs the draft on screen, saved or not, against a dataset of test cases and shows
 * pass or fail, score, cost and time for each case, with a summary against the published version when there
 * is one. It saves and publishes nothing: the server runs the draft as an arm of a check experiment, with a
 * spending limit, a cap on cases and a cap on runs at once.
 */
import { useEffect, useRef, useState } from 'react'
import { Link } from 'react-router'
import { ApiError, type KubeObject } from '../api/builder'
import {
  ACTIVE_STATUSES, checkDraft, getExperiment, getExperimentSummary, listCases, listDatasets, listExperimentResults, describeEvalError,
  type ArmSummary, type CheckResponse, type EvalDataset, type Experiment, type ExperimentResult, type ExperimentSummary,
} from '../api/evals'
import { canWriteEvals, useRoles } from '../lib/access'
import { ds } from '../lib/designSystem'
import { ms, pct, usd } from '../lib/evalFormat'
import { DSButton, DSChip } from '../lib/primitives'
import { ROUTES } from '../lib/routes'
import { Field, field } from './BuilderParts'

interface Props {
  namespace: string
  name: string
  /** The agent as it is on screen. */
  draft: KubeObject
  /** Why a check cannot start yet (for example the YAML does not parse); empty when it can. */
  blocked?: string
}

const DEFAULT_BUDGET = 0.25
const POLL_MS = 2000

/** The mean score of the grades that counted, or undefined when none did. */
export function scoreOf(r: ExperimentResult): number | undefined {
  const g = (r.grades ?? []).filter((x) => !x.notGraded && x.status !== 'error' && x.status !== 'pending_review')
  if (g.length === 0) return undefined
  return g.reduce((a, x) => a + x.score, 0) / g.length
}

function armRow(summary: ExperimentSummary | undefined, label: string): ArmSummary | undefined {
  return summary?.comparison?.arms.find((a) => a.label === label)
}

export function BuilderCheckPanel({ namespace, name, draft, blocked }: Props) {
  const roles = useRoles()
  const [datasets, setDatasets] = useState<EvalDataset[] | null>(null)
  const [datasetId, setDatasetId] = useState<number | ''>('')
  const [budget, setBudget] = useState(String(DEFAULT_BUDGET))
  const [busy, setBusy] = useState(false)
  const [errors, setErrors] = useState<string[]>([])
  const [started, setStarted] = useState<CheckResponse | null>(null)
  const [exp, setExp] = useState<Experiment | null>(null)
  const [results, setResults] = useState<ExperimentResult[]>([])
  const [summary, setSummary] = useState<ExperimentSummary | undefined>()
  const [inputs, setInputs] = useState<Record<number, string>>({})
  const runId = useRef(0)

  useEffect(() => {
    if (!namespace) return
    let live = true
    listDatasets({ namespace })
      .then((list) => {
        if (!live) return
        setDatasets(list)
        const mine = list.find((d) => d.agent && d.agent === name && d.caseCount > 0) ?? list.find((d) => d.caseCount > 0)
        if (mine) setDatasetId((cur) => cur || mine.id)
      })
      .catch(() => { if (live) setDatasets([]) })
    return () => { live = false }
  }, [namespace, name])

  // Follow the experiment until it ends, then read its results and summary.
  useEffect(() => {
    if (!started) return
    const id = started.experiment.id
    const mine = ++runId.current
    let timer: ReturnType<typeof setTimeout> | undefined
    const load = async () => {
      try {
        const e = await getExperiment(id)
        if (mine !== runId.current) return
        setExp(e)
        if (ACTIVE_STATUSES.includes(e.status) && e.status !== 'awaiting_review') {
          timer = setTimeout(() => { void load() }, POLL_MS)
          return
        }
        const [res, sum] = await Promise.all([
          listExperimentResults(id, { limit: 200 }),
          getExperimentSummary(id).catch(() => null),
        ])
        if (mine !== runId.current) return
        setResults(res.results)
        setSummary(sum?.summary ?? e.summary)
        listCases(e.datasetId, { limit: 200 })
          .then((p) => { if (mine === runId.current) setInputs(Object.fromEntries(p.cases.map((c) => [c.id, c.input]))) })
          .catch(() => {})
      } catch (e) {
        if (mine === runId.current) setErrors([describeEvalError(e)])
      }
    }
    void load()
    return () => { runId.current++; if (timer) clearTimeout(timer) }
  }, [started])

  const roleOk = canWriteEvals(roles)
  const budgetNum = Number(budget)
  const budgetOk = budget.trim() !== '' && Number.isFinite(budgetNum) && budgetNum > 0
  const ready = datasets?.some((d) => d.caseCount > 0) ?? false

  const run = async () => {
    if (datasetId === '') return
    setBusy(true)
    setErrors([])
    setStarted(null)
    setExp(null)
    setResults([])
    setSummary(undefined)
    try {
      setStarted(await checkDraft({ agent: draft, datasetId, budgetUsd: budgetNum }))
    } catch (e) {
      if (e instanceof ApiError && e.status === 400 && e.errors.length > 0) setErrors(['The platform would not accept this draft yet:', ...e.errors])
      else if (e instanceof ApiError && e.status === 429) setErrors([e.message])
      else setErrors([describeEvalError(e)])
    } finally {
      setBusy(false)
    }
  }

  const finished = !!exp && !ACTIVE_STATUSES.includes(exp.status)
  const withPublished = !!started?.comparedWithPublished
  const draftRows = results.filter((r) => (r.arm ?? 'draft') === 'draft')
  const liveByCase = new Map(results.filter((r) => r.arm === 'published').map((r) => [r.caseId, r]))
  const d = armRow(summary, 'draft')
  const p = armRow(summary, 'published')
  const cmp = summary?.comparison?.comparisons?.[0]

  return (
    <section aria-labelledby="check-heading" style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
      <h2 id="check-heading" style={{ margin: 0, fontSize: 15, fontWeight: 600, color: ds.textPrimary }}>Check with my cases</h2>
      <p style={field.help}>
        Runs this draft, as it is on the screen, on your saved test cases. Nothing is saved or published. Each case is a real run, so it
        uses the budget below; tools the draft uses are really called.
      </p>
      {!roleOk ? (
        <p role="status" style={field.help}>Checking a draft needs the builder role.</p>
      ) : datasets === null ? (
        <p role="status" style={field.help}>Looking for your test cases…</p>
      ) : !ready ? (
        <p role="status" style={field.help}>
          You have no test cases in this workspace yet. <Link to={ROUTES.evaluate} style={{ color: ds.accent }}>Add some</Link> to check drafts against them.
        </p>
      ) : (
        <>
          <Field label="Test cases">
            {(a) => (
              <select {...a} style={field.control} value={datasetId} onChange={(e) => setDatasetId(Number(e.target.value))}>
                {datasets.filter((x) => x.caseCount > 0).map((x) => <option key={x.id} value={x.id}>{x.name} ({x.caseCount} cases)</option>)}
              </select>
            )}
          </Field>
          <Field label="Spending limit (USD)" help="The check stops when it has spent this much. The platform sets the highest value you may use.">
            {(a) => <input {...a} style={field.control} inputMode="decimal" value={budget} onChange={(e) => setBudget(e.target.value)} />}
          </Field>
          <div>
            <DSButton type="button" variant="secondary" onClick={() => { void run() }} disabled={busy || !budgetOk || datasetId === '' || !!blocked || (!!started && !!exp && !finished)} title={blocked || undefined}>
              {busy ? 'Starting…' : 'Run check'}
            </DSButton>
          </div>
          {blocked && <p style={field.help}>{blocked}</p>}
        </>
      )}
      <div role="status" aria-live="polite">
        {errors.length > 0 && (
          <div role="alert">
            {errors.map((m) => <p key={m} style={field.error}>{m}</p>)}
          </div>
        )}
        {started && exp && !finished && <p style={field.help}>Running: {exp.resultsDone} of {exp.resultsTotal} done…</p>}
        {started?.capped && exp && <p style={field.help}>Only the first {exp.cases} cases are run in a check.</p>}
      </div>
      {finished && exp && (
        <div aria-label="Check result" style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
          {exp.status === 'failed' || exp.status === 'cancelled' ? (
            <p role="alert" style={field.error}>The check {exp.status === 'failed' ? 'failed' : 'was cancelled'}{exp.error ? `: ${exp.error}` : '.'}</p>
          ) : (
            <p style={{ margin: 0, fontSize: 13, color: ds.textPrimary }}>
              {d ? <>The draft passed <strong>{d.passed} of {d.cases}</strong> cases ({pct(d.passRate)}), cost {usd(d.costTotalUsd)}{d.hasLatency ? `, typical time ${ms(d.latencyP50Ms)}` : ''}.</> : 'The check finished without a summary.'}
              {exp.status === 'awaiting_review' && ' Some checks wait for a person to review them.'}
            </p>
          )}
          {withPublished && p && d && (
            <p style={{ margin: 0, fontSize: 12, color: ds.textSecondary }}>
              The published version passed {p.passed} of {p.cases} ({pct(p.passRate)}), cost {usd(p.costTotalUsd)}.
              {cmp?.verdict ? ` Compared with it, the draft is: ${cmp.verdict.verdict}.` : ''}
              {cmp && (cmp.regressedCases?.length ?? 0) > 0 ? ` ${cmp.regressedCases.length} case(s) got worse.` : ''}
            </p>
          )}
          {exp.warnings?.map((w) => <p key={w} style={field.help}>{w}</p>)}
          <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12 }}>
            <caption style={{ position: 'absolute', left: -9999 }}>Result of each case</caption>
            <thead>
              <tr style={{ textAlign: 'left', color: ds.textSecondary }}>
                <th scope="col">Case</th><th scope="col">Result</th><th scope="col">Score</th><th scope="col">Cost</th><th scope="col">Time</th>
                {withPublished && <th scope="col">Published</th>}
              </tr>
            </thead>
            <tbody>
              {draftRows.map((r) => {
                const sc = scoreOf(r)
                const live = liveByCase.get(r.caseId)
                const text = r.caseId !== undefined ? inputs[r.caseId] : undefined
                return (
                  <tr key={r.id} style={{ borderTop: '1px solid var(--ds-border)' }}>
                    <td title={text} style={{ maxWidth: 140, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{text ? text : `Case ${r.caseId ?? ''}`}</td>
                    <td><DSChip size="sm" variant={r.phase === 'Skipped' ? 'neutral' : r.casePass ? 'success' : 'error'}>{r.phase === 'Skipped' ? 'Skipped' : r.casePass ? 'Pass' : 'Fail'}</DSChip></td>
                    <td>{sc === undefined ? '-' : pct(sc)}</td>
                    <td>{usd(r.costUsd)}</td>
                    <td>{ms(r.latencyMs)}</td>
                    {withPublished && <td>{live ? (live.casePass ? 'Pass' : 'Fail') : '-'}</td>}
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
