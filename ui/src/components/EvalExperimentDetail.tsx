/** One experiment: live progress, the comparison of its arms, and every result with its grades. */
import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router'
import {
  cancelExperiment, describeEvalError, getExperiment, getExperimentSummary, graderTallies, listExperimentResults,
  type ArmSummary, type Comparison, type Experiment, type ExperimentResult, type SummaryResponse,
} from '../api/evals'
import { canGradeEvals, canWriteEvals, useRoles } from '../lib/access'
import { Dialog } from '../lib/dialog'
import { ds } from '../lib/designSystem'
import {
  armLabel, armWords, interval, isActive, ms, pct, points, pValue, STATUS_VARIANT, STATUS_WORDS, usd, when,
} from '../lib/evalFormat'
import { ICON } from '../lib/icons'
import { EmptyState, Page, PageHeader, Section } from '../lib/layout'
import { DSButton, DSChip } from '../lib/primitives'
import { datasetPath, experimentsPath, reviewPath } from '../lib/routes'
import { field } from './BuilderParts'
import { actionsRow, table, td, th } from './EvalDatasets'

const POLL_MS = 4000

export function EvalExperimentDetail({ id }: { id: number }) {
  const roles = useRoles()
  const [exp, setExp] = useState<Experiment | null>(null)
  const [error, setError] = useState<{ status?: number; message: string } | null>(null)
  const [summary, setSummary] = useState<SummaryResponse | null>(null)
  const [summaryNote, setSummaryNote] = useState('')
  const [cancelling, setCancelling] = useState(false)

  const load = useCallback(() => {
    let live = true
    getExperiment(id)
      .then((e) => { if (live) { setExp(e); setError(null) } })
      .catch((e: unknown) => { if (live) setError({ status: (e as { status?: number }).status, message: describeEvalError(e) }) })
    getExperimentSummary(id)
      .then((s) => { if (live) { setSummary(s); setSummaryNote('') } })
      .catch((e: unknown) => {
        if (!live) return
        const status = (e as { status?: number }).status
        setSummaryNote(status === 409 ? 'There is no comparison yet: the experiment has not started, or it failed before it had results.' : `Could not load the comparison: ${describeEvalError(e)}`)
      })
    return () => { live = false }
  }, [id])
  useEffect(() => load(), [load])

  const active = !!exp && isActive(exp)
  useEffect(() => {
    if (!active) return
    const t = setInterval(() => { load() }, POLL_MS)
    return () => clearInterval(t)
  }, [active, load])

  const back = (
    <nav aria-label="Breadcrumb" style={{ padding: '10px 24px 0', fontSize: 12 }}>
      <Link to={experimentsPath} style={{ color: ds.accent, textDecoration: 'none' }}>Experiments</Link>
      <span style={{ color: ds.textMuted }}> / </span>
      <span style={{ color: ds.textSecondary }}>{exp?.name || `Experiment ${id}`}</span>
    </nav>
  )

  if (error && !exp) {
    return (
      <Page>
        {back}
        <EmptyState icon={ICON.warning} title={error.status === 404 ? 'Experiment not found' : 'Could not load the experiment'}>{error.message}</EmptyState>
      </Page>
    )
  }
  if (!exp) return <Page>{back}<div role="status" style={{ padding: '0 24px', color: ds.textMuted, fontSize: 13 }}>Loading the experiment…</div></Page>

  const canCancel = canWriteEvals(roles) && active && !exp.cancelRequested
  const reviewing = exp.status === 'awaiting_review' || (summary?.summary.reviewPending ?? 0) > 0
  const sum = summary?.summary
  const comp = sum?.comparison

  return (
    <Page>
      {back}
      <PageHeader
        title={exp.name || `Experiment ${exp.id}`}
        description={`Workspace ${exp.namespace} · dataset version ${exp.datasetVersion} · started ${when(exp.startedAt ?? exp.createdAt)}`}
        actions={
          <div style={{ display: 'flex', gap: 8 }}>
            <Link to={datasetPath(exp.datasetId)} style={linkButton}>Dataset</Link>
            {reviewing && <Link to={reviewPath(exp.id)} style={linkButton}>{canGradeEvals(roles) ? 'Review answers' : 'View review'}</Link>}
            {canCancel && <DSButton variant="danger" onClick={() => setCancelling(true)}>Cancel experiment</DSButton>}
          </div>
        }
      />
      <div style={{ padding: '0 24px 24px', display: 'flex', flexDirection: 'column', gap: 24 }}>
        {error && <div role="alert" style={{ color: ds.error, fontSize: 13 }}>Could not refresh: {error.message}</div>}
        <Progress exp={exp} summary={summary} />
        {exp.warnings && exp.warnings.length > 0 && (
          <ul style={{ margin: 0, paddingLeft: 18, fontSize: 12, color: ds.warning }}>{exp.warnings.map((w) => <li key={w}>{w}</li>)}</ul>
        )}
        <Section title="Comparison">
          {summaryNote && !comp && <p role="status" style={{ margin: 0, fontSize: 13, color: ds.textSecondary }}>{summaryNote}</p>}
          {summary && <Notices summary={summary} exp={exp} />}
          {comp && comp.arms.length > 0 && <ArmTable exp={exp} arms={comp.arms} />}
          {comp && comp.comparisons.map((c) => <ComparisonCard key={c.candidate} c={c} />)}
          {comp && graderTallies(sum).length > 0 && <GraderTable tallies={graderTallies(sum)} />}
        </Section>
        <ResultsTable exp={exp} />
      </div>
      {cancelling && <CancelDialog exp={exp} onClose={() => setCancelling(false)} onCancelled={() => { setCancelling(false); load() }} />}
    </Page>
  )
}

function Progress({ exp, summary }: { exp: Experiment; summary: SummaryResponse | null }) {
  const done = exp.resultsTotal > 0 ? Math.min(1, exp.resultsDone / exp.resultsTotal) : 0
  const spentFrac = exp.budgetUsd > 0 ? Math.min(1, exp.spentUsd / exp.budgetUsd) : 0
  const ended = !isActive(exp)
  return (
    <section aria-label="Progress" style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
      <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
        <DSChip variant={STATUS_VARIANT[exp.status]} dot>{STATUS_WORDS[exp.status] ?? exp.status}</DSChip>
        {exp.cancelRequested && exp.status !== 'cancelled' && <DSChip variant="warning" size="sm">Cancelling</DSChip>}
        {summary?.live && <DSChip variant="info" size="sm">Live figures</DSChip>}
        {!ended && <span style={{ fontSize: 12, color: ds.textSecondary }}>Updates by itself.</span>}
      </div>
      {exp.status === 'failed' && exp.error && <p role="alert" style={{ margin: 0, fontSize: 13, color: ds.error }}>The experiment failed: {exp.error}</p>}
      {exp.status !== 'failed' && exp.error && <p style={{ margin: 0, fontSize: 12, color: ds.warning }}>{exp.error}</p>}
      <div>
        <div style={{ fontSize: 12, color: ds.textSecondary }}>Results: {exp.resultsDone} of {exp.resultsTotal}</div>
        <progress aria-label="Results done" value={done} max={1} style={{ width: '100%', height: 8 }} />
      </div>
      <div>
        <div style={{ fontSize: 12, color: ds.textSecondary }}>Budget used: {usd(exp.spentUsd)} of {usd(exp.budgetUsd)}</div>
        <progress aria-label="Budget used" value={spentFrac} max={1} style={{ width: '100%', height: 8 }} />
      </div>
      <ul style={{ margin: 0, padding: 0, listStyle: 'none', display: 'flex', gap: 16, flexWrap: 'wrap', fontSize: 12, color: ds.textSecondary }}>
        {exp.arms.map((a, i) => <li key={i}><strong>{armLabel(a, i)}</strong>: {armWords(a)}</li>)}
        <li>{exp.repeats} {exp.repeats === 1 ? 'repeat' : 'repeats'}</li>
        {exp.concurrency ? <li>{exp.concurrency} at a time</li> : null}
      </ul>
    </section>
  )
}

const STOPPED: Record<string, string> = {
  budget: 'It stopped because the budget was spent.',
  'daily-budget': 'It stopped because the daily budget of the workspace ran low.',
  cancelled: 'It was cancelled.',
}

function Notices({ summary, exp }: { summary: SummaryResponse; exp: Experiment }) {
  const s = summary.summary
  const excErr = s.comparison?.excludedError ?? 0
  const excPend = s.comparison?.excludedPending ?? 0
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
      {summary.partial && (
        <p role="status" style={{ margin: 0, fontSize: 13, color: ds.warning }}>
          Partial: these figures cover only the cases finished so far
          {s.stoppedBy && STOPPED[s.stoppedBy] ? `. ${STOPPED[s.stoppedBy]}` : summary.live ? ' and will change as more finish.' : '.'}
        </p>
      )}
      {(excErr > 0 || excPend > 0 || (s.skipped ?? 0) > 0) && (
        <p style={{ margin: 0, fontSize: 12, color: ds.textSecondary }}>
          Left out of the figures:
          {excErr > 0 && ` ${excErr} ${excErr === 1 ? 'result' : 'results'} a check could not grade (error);`}
          {excPend > 0 && ` ${excPend} waiting for a person to review;`}
          {(s.skipped ?? 0) > 0 && ` ${s.skipped} skipped (the platform failed the run).`}
          {' '}Nothing is counted as a pass by guessing.
          {exp.status === 'awaiting_review' && ' The experiment is waiting for those reviews.'}
        </p>
      )}
    </div>
  )
}

function ArmTable({ exp, arms }: { exp: Experiment; arms: ArmSummary[] }) {
  return (
    <table style={table} aria-label="Pass rate by version">
      <thead>
        <tr>
          <th style={th}>Version</th><th style={th}>Pass rate</th><th style={th}>Cases</th><th style={th}>Average score</th>
          <th style={th}>Failed runs</th><th style={th}>Cost (total / median)</th><th style={th}>Time (median / slowest 5%)</th><th style={th}>Left out</th>
        </tr>
      </thead>
      <tbody>
        {arms.map((a, i) => {
          const arm = exp.arms.find((x, j) => armLabel(x, j) === a.label)
          return (
            <tr key={a.label}>
              <td style={td}>
                <strong>{a.label}</strong>{i === 0 && <DSChip size="sm" variant="neutral" style={{ marginLeft: 6 }}>baseline</DSChip>}
                {arm && <div style={{ fontSize: 12, color: ds.textSecondary }}>{armWords(arm)}</div>}
              </td>
              <td style={td}>
                <strong>{pct(a.passRate)}</strong> ({a.passed} of {a.cases})
                <div style={{ fontSize: 12, color: ds.textSecondary }} title="Wilson 95% interval">{interval(a.passRateCi?.lo, a.passRateCi?.hi)}</div>
              </td>
              <td style={td}>{a.cases}</td>
              <td style={td}>{a.meanScore.toFixed(2)}</td>
              <td style={td}>{pct(a.failureRate)}</td>
              <td style={td}>{usd(a.costTotalUsd)} / {usd(a.costMedianUsd)}{a.costIncomplete && <div style={{ fontSize: 12, color: ds.warning }}>a floor: some cost was not captured</div>}</td>
              <td style={td}>{a.hasLatency ? `${ms(a.latencyP50Ms)} / ${ms(a.latencyP95Ms)}` : '-'}</td>
              <td style={td}>{a.excludedError + a.excludedPending + a.skipped > 0 ? `${a.excludedError} error, ${a.excludedPending} pending, ${a.skipped} skipped` : '-'}</td>
            </tr>
          )
        })}
      </tbody>
    </table>
  )
}

const VERDICT_WORDS: Record<string, { text: string; variant: 'success' | 'error' | 'neutral' }> = {
  better: { text: 'Better', variant: 'success' },
  worse: { text: 'Worse', variant: 'error' },
  'no clear difference': { text: 'No clear difference', variant: 'neutral' },
  'not enough data': { text: 'Not enough data to tell', variant: 'neutral' },
}

function ComparisonCard({ c }: { c: Comparison }) {
  const v = VERDICT_WORDS[c.verdict.verdict] ?? { text: c.verdict.verdict, variant: 'neutral' as const }
  const pr = c.passRate
  return (
    <section aria-label={`${c.candidate} against ${c.baseline}`} style={{ border: '1px solid var(--ds-border)', borderRadius: 8, padding: 14, display: 'flex', flexDirection: 'column', gap: 8 }}>
      <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
        <strong style={{ fontSize: 14 }}>{c.candidate} against {c.baseline}</strong>
        <DSChip variant={v.variant}>{v.text}</DSChip>
        {c.verdict.nonInferior && <DSChip size="sm" variant="info">No worse than baseline by {pct(c.verdict.margin)}</DSChip>}
      </div>
      <p style={{ margin: 0, fontSize: 13 }}>{c.verdict.reason}</p>
      <dl style={{ margin: 0, display: 'grid', gridTemplateColumns: 'max-content 1fr', gap: '4px 16px', fontSize: 13 }}>
        <dt style={dt}>Paired cases</dt><dd style={dd}>{c.pairs}{c.skipped > 0 ? ` (${c.skipped} left out)` : ''}</dd>
        {pr && (
          <>
            <dt style={dt}>Pass rate change</dt>
            <dd style={dd}>{points(pr.diff)} <span style={{ color: ds.textSecondary }}>(95% interval {points(pr.ci.lo)} to {points(pr.ci.hi)})</span></dd>
            <dt style={dt}>Case by case</dt>
            <dd style={dd}>{pr.counts.improved} improved, {pr.counts.regressed} regressed, {pr.counts.bothPass} passed in both, {pr.counts.bothFail} failed in both</dd>
            <dt style={dt}>McNemar test</dt>
            <dd style={dd}>{pValue(pr.mcnemar.p)}{pr.mcnemar.exact ? ' (exact)' : ''} <span style={{ color: ds.textSecondary }}>on the {pr.mcnemar.improved + pr.mcnemar.regressed} cases that differ</span></dd>
          </>
        )}
        {c.cost && (
          <>
            <dt style={dt}>Cost change per case</dt>
            <dd style={dd}>
              {usd(c.cost.mean, true)}{c.cost.hasCi ? ` (interval ${usd(c.cost.ci.lo, true)} to ${usd(c.cost.ci.hi, true)})` : ''}
              {c.costMedian?.hasRel ? `, median ${points(c.costMedian.rel)}` : ''}
            </dd>
          </>
        )}
        {c.latency && (
          <>
            <dt style={dt}>Time change per case</dt>
            <dd style={dd}>
              {ms(c.latency.mean, true)}{c.latency.hasCi ? ` (interval ${ms(c.latency.ci.lo, true)} to ${ms(c.latency.ci.hi, true)})` : ''}
              {c.latencyMedian?.hasRel ? `, median ${points(c.latencyMedian.rel)}` : ''}
            </dd>
          </>
        )}
        {c.score && <><dt style={dt}>Score change</dt><dd style={dd}>{c.score.mean >= 0 ? '+' : ''}{c.score.mean.toFixed(2)}</dd></>}
        {(c.regressedCases?.length ?? 0) > 0 && <><dt style={dt}>Regressed cases</dt><dd style={dd}>{c.regressedCases.join(', ')}</dd></>}
      </dl>
    </section>
  )
}

const dt = { color: ds.textSecondary, margin: 0 } as const
const dd = { margin: 0 } as const

function GraderTable({ tallies }: { tallies: ReturnType<typeof graderTallies> }) {
  return (
    <table style={table} aria-label="Checks">
      <thead>
        <tr><th style={th}>Check</th><th style={th}>Passed</th><th style={th}>Not graded</th><th style={th}>Errors</th><th style={th}>Waiting for review</th><th style={th}>Judge cost</th></tr>
      </thead>
      <tbody>
        {tallies.map((g, i) => (
          <tr key={g.name ?? i}>
            <td style={td}>{g.name ?? `Check ${i + 1}`}</td>
            <td style={td}>{g.passed} of {g.graded}</td>
            <td style={td}>{g.notGraded ?? 0}</td>
            <td style={td}>{(g.errors ?? 0) + (g.invalid ?? 0)}{(g.invalid ?? 0) > 0 ? ` (${g.invalid} unusable judge replies)` : ''}</td>
            <td style={td}>{g.pendingReview ?? 0}</td>
            <td style={td}>{g.judgeCostUsd ? usd(g.judgeCostUsd) : '-'}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function ResultsTable({ exp }: { exp: Experiment }) {
  const [arm, setArm] = useState('')
  const [rows, setRows] = useState<ExperimentResult[] | null>(null)
  const [cursor, setCursor] = useState('')
  const [error, setError] = useState('')
  const [more, setMore] = useState(false)
  const active = isActive(exp)

  const load = useCallback((reset: boolean, from = '') => {
    let live = true
    listExperimentResults(exp.id, { arm: arm || undefined, limit: 50, cursor: from || undefined })
      .then((p) => {
        if (!live) return
        setRows((old) => (reset || !old ? p.results : [...old, ...p.results]))
        setCursor(p.nextCursor)
        setError('')
        setMore(false)
      })
      .catch((e: unknown) => { if (live) { setError(describeEvalError(e)); setRows((old) => old ?? []); setMore(false) } })
    return () => { live = false }
  }, [exp.id, arm])
  useEffect(() => load(true), [load, exp.resultsDone, exp.status])

  return (
    <Section title="Results">
      <div style={{ display: 'flex', gap: 8, alignItems: 'center', marginBottom: 8 }}>
        <label htmlFor="results-arm" style={field.label}>Show</label>
        <select id="results-arm" style={{ ...field.control, width: 'auto' }} value={arm} onChange={(e) => { setRows(null); setArm(e.target.value) }}>
          <option value="">All versions</option>
          {exp.arms.map((a, i) => <option key={i} value={armLabel(a, i)}>{armLabel(a, i)}</option>)}
        </select>
      </div>
      {error && <p role="alert" style={{ color: ds.error, fontSize: 13 }}>Could not load results: {error}</p>}
      {!rows ? (
        <div role="status" style={{ color: ds.textMuted, fontSize: 13 }}>Loading results…</div>
      ) : rows.length === 0 ? (
        !error && <p style={{ margin: 0, fontSize: 13, color: ds.textSecondary }}>{active ? 'No results yet. They appear here as runs finish.' : 'This experiment has no results.'}</p>
      ) : (
        <table style={table} aria-label="Results by case">
          <thead>
            <tr><th style={th}>Case</th><th style={th}>Version</th><th style={th}>Repeat</th><th style={th}>Outcome</th><th style={th}>Cost</th><th style={th}>Time</th><th style={th}>Answer and checks</th></tr>
          </thead>
          <tbody>
            {rows.map((r) => <ResultRow key={r.id} r={r} />)}
          </tbody>
        </table>
      )}
      {cursor && <div style={{ marginTop: 8 }}><DSButton size="sm" disabled={more} onClick={() => { setMore(true); load(false, cursor) }}>Load more</DSButton></div>}
    </Section>
  )
}

function ResultRow({ r }: { r: ExperimentResult }) {
  const pending = r.grades.some((g) => g.status === 'pending_review')
  const failedRun = r.phase && r.phase !== 'Succeeded'
  return (
    <tr>
      <td style={td}>{r.caseId ?? '-'}</td>
      <td style={td}>{r.arm || '-'}</td>
      <td style={td}>{(r.repeat ?? 0) + 1}</td>
      <td style={td}>
        {failedRun ? <DSChip size="sm" variant="error">{r.phase}</DSChip>
          : pending ? <DSChip size="sm" variant="warning">Waiting for review</DSChip>
          : <DSChip size="sm" variant={r.casePass ? 'success' : 'error'}>{r.casePass ? 'Passed' : 'Failed'}</DSChip>}
      </td>
      <td style={td}>{usd(r.costUsd)}{r.costIncomplete ? ' +' : ''}</td>
      <td style={td}>{ms(r.latencyMs)}</td>
      <td style={td}>
        <details>
          <summary style={{ cursor: 'pointer', fontSize: 12 }}>Show answer and {r.grades.length} {r.grades.length === 1 ? 'check' : 'checks'}</summary>
          <pre style={{ whiteSpace: 'pre-wrap', margin: '6px 0', fontSize: 12, maxHeight: 240, overflow: 'auto' }}>{r.output || '(no output)'}</pre>
          {r.toolCalls.length > 0 && <div style={{ fontSize: 12, color: ds.textSecondary }}>Tools called: {r.toolCalls.join(', ')}</div>}
          <ul style={{ margin: '6px 0 0', paddingLeft: 18, fontSize: 12 }}>
            {r.grades.map((g, i) => (
              <li key={`${g.graderId}-${i}`}>
                <strong>{g.grader}</strong>: {g.status === 'pending_review' ? 'waiting for review' : g.status === 'error' ? 'could not be graded' : g.notGraded ? 'not graded' : g.pass ? 'pass' : 'fail'} ({g.score.toFixed(2)})
                {g.detail ? <> {'–'} {g.detail}</> : null}
              </li>
            ))}
          </ul>
        </details>
      </td>
    </tr>
  )
}

function CancelDialog({ exp, onClose, onCancelled }: { exp: Experiment; onClose: () => void; onCancelled: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const run = async () => {
    setBusy(true)
    setError('')
    try {
      await cancelExperiment(exp.id)
      onCancelled()
    } catch (e) {
      const status = (e as { status?: number }).status
      setError(status === 409 ? 'The experiment has already ended.' : describeEvalError(e))
      setBusy(false)
    }
  }
  return (
    <Dialog title="Cancel this experiment?" onClose={onClose}>
      <p style={{ margin: 0, fontSize: 13 }}>
        Runs in progress are stopped and what has finished is kept. The comparison then covers the cases that finished for every version.
      </p>
      {error && <p role="alert" style={field.error}>Could not cancel: {error}</p>}
      <div style={actionsRow}>
        <DSButton onClick={onClose}>Keep running</DSButton>
        <DSButton variant="danger" disabled={busy} onClick={run}>Cancel experiment</DSButton>
      </div>
    </Dialog>
  )
}

const linkButton = {
  display: 'inline-flex', alignItems: 'center', padding: '6px 12px', fontSize: 13, borderRadius: 6,
  border: '1px solid var(--ds-border)', color: ds.textPrimary, textDecoration: 'none',
} as const
