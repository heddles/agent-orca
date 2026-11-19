/**
 * The blind review queue of an experiment, and the calibration of an llm grader against a human one.
 * An item never says which version produced the answer.
 */
import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router'
import {
  closeReview, describeEvalError, getCalibration, getExperiment, getReviewQueue, labelReviewItem, listGraders,
  type Calibration, type EvalGrader, type Experiment, type ReviewItem, type ReviewQueue,
} from '../api/evals'
import { canGradeEvals, canWriteEvals, useRoles } from '../lib/access'
import { Dialog } from '../lib/dialog'
import { ds } from '../lib/designSystem'
import { pct } from '../lib/evalFormat'
import { ICON } from '../lib/icons'
import { EmptyState, Page, PageHeader, Section } from '../lib/layout'
import { DSButton, DSChip } from '../lib/primitives'
import { experimentPath, experimentsPath } from '../lib/routes'
import { Field, field } from './BuilderParts'
import { actionsRow } from './EvalDatasets'

const POLL_MS = 8000

export function EvalReview({ id }: { id: number }) {
  const roles = useRoles()
  const canLabel = canGradeEvals(roles)
  const [exp, setExp] = useState<Experiment | null>(null)
  const [queue, setQueue] = useState<ReviewQueue | null>(null)
  const [error, setError] = useState<{ status?: number; message: string } | null>(null)
  const [closing, setClosing] = useState(false)

  const load = useCallback(() => {
    let live = true
    getExperiment(id).then((e) => { if (live) setExp(e) }).catch((e: unknown) => { if (live) setError({ status: (e as { status?: number }).status, message: describeEvalError(e) }) })
    getReviewQueue(id, { status: 'pending', limit: 20 })
      .then((q) => { if (live) { setQueue(q); setError(null) } })
      .catch((e: unknown) => { if (live) setError({ status: (e as { status?: number }).status, message: describeEvalError(e) }) })
    return () => { live = false }
  }, [id])
  useEffect(() => load(), [load])

  const waiting = exp?.status === 'awaiting_review'
  useEffect(() => {
    if (!waiting) return
    const t = setInterval(() => { load() }, POLL_MS)
    return () => clearInterval(t)
  }, [waiting, load])

  const back = (
    <nav aria-label="Breadcrumb" style={{ padding: '10px 24px 0', fontSize: 12 }}>
      <Link to={experimentsPath} style={{ color: ds.accent, textDecoration: 'none' }}>Experiments</Link>
      <span style={{ color: ds.textMuted }}> / </span>
      <Link to={experimentPath(id)} style={{ color: ds.accent, textDecoration: 'none' }}>{exp?.name || `Experiment ${id}`}</Link>
      <span style={{ color: ds.textMuted }}> / </span>
      <span style={{ color: ds.textSecondary }}>Review</span>
    </nav>
  )

  if (error && !queue) {
    return (
      <Page>
        {back}
        <EmptyState icon={ICON.warning} title={error.status === 404 ? 'Experiment not found' : 'Could not load the review queue'}>{error.message}</EmptyState>
      </Page>
    )
  }
  if (!queue) return <Page>{back}<div role="status" style={{ padding: '0 24px', color: ds.textMuted, fontSize: 13 }}>Loading the review queue…</div></Page>

  const { pending, done, closed } = queue.progress
  const item = queue.items[0]
  return (
    <Page>
      {back}
      <PageHeader
        title="Review answers"
        description="Judge each answer on its own. You are not told which version of the agent wrote it."
        actions={canWriteEvals(roles) && waiting ? <DSButton variant="secondary" onClick={() => setClosing(true)}>Close review</DSButton> : undefined}
      />
      <div style={{ padding: '0 24px 24px', display: 'flex', flexDirection: 'column', gap: 24 }}>
        <p role="status" style={{ margin: 0, fontSize: 13, color: ds.textSecondary }}>
          {pending} waiting, {done} labelled{closed > 0 ? `, ${closed} closed without a label` : ''}.
        </p>
        {!canLabel && <p role="status" style={{ margin: 0, fontSize: 12, color: ds.textSecondary }}>You can read the queue. Labelling needs the runner role.</p>}
        {error && <div role="alert" style={{ color: ds.error, fontSize: 13 }}>{error.message}</div>}
        {!item ? (
          <EmptyState icon={ICON.check} title={pending === 0 && done + closed === 0 ? 'Nothing to review' : 'Nothing left to review'}>
            {done + closed === 0
              ? 'This experiment has no answers waiting for a person. Its dataset needs a check of the human kind.'
              : 'Every answer has been labelled or closed.'}
          </EmptyState>
        ) : (
          <ReviewCard key={item.id} item={item} index={done + 1} canLabel={canLabel} onDone={load} />
        )}
        {exp && <CalibrationPanel datasetId={exp.datasetId} experimentId={exp.id} />}
      </div>
      {closing && <CloseDialog id={id} pending={pending} onClose={() => setClosing(false)} onClosed={() => { setClosing(false); load() }} />}
    </Page>
  )
}

function ReviewCard({ item, index, canLabel, onDone }: { item: ReviewItem; index: number; canLabel: boolean; onDone: () => void }) {
  const [stars, setStars] = useState(0)
  const [comment, setComment] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const submit = async (label: { pass?: boolean; stars?: number }) => {
    setBusy(true)
    setError('')
    try {
      await labelReviewItem(item.id, { ...label, comment: comment.trim() || undefined })
      onDone()
    } catch (e) {
      const status = (e as { status?: number }).status
      if (status === 409) { setError('Someone already labelled this answer, or the review was closed. Showing the next one.'); onDone() }
      else if (status === 403) setError(`You cannot label this. ${(e as Error).message || 'It needs the runner role, and the person who started the experiment may not review it.'}`)
      else setError(describeEvalError(e))
      setBusy(false)
    }
  }

  return (
    <section aria-label={`Answer ${index}`} style={{ border: '1px solid var(--ds-border)', borderRadius: 8, padding: 16, display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
        <strong style={{ fontSize: 14 }}>Answer {index}</strong>
        <DSChip size="sm" variant="neutral">{item.grader}</DSChip>
      </div>
      {item.instructions && <p style={{ margin: 0, fontSize: 13, color: ds.textSecondary }}>{item.instructions}</p>}
      <Block title="Question" text={item.input} />
      {item.reference && <Block title="Expected answer" text={item.reference} />}
      <Block title="Answer to judge" text={item.output || '(no output)'} />
      {canLabel && (
        <>
          {item.scale === '1to5' ? (
            <fieldset style={{ border: 0, padding: 0, margin: 0 }}>
              <legend style={field.label}>How good is the answer? (1 is poor, 5 is excellent)</legend>
              <div style={{ display: 'flex', gap: 12, marginTop: 4 }}>
                {[1, 2, 3, 4, 5].map((n) => (
                  <label key={n} style={{ fontSize: 13, display: 'flex', gap: 4, alignItems: 'center' }}>
                    <input type="radio" name={`stars-${item.id}`} value={n} checked={stars === n} onChange={() => setStars(n)} /> {n}
                  </label>
                ))}
              </div>
            </fieldset>
          ) : null}
          <Field label="Comment (optional)" help="At most 2048 characters. Shown to people who can read this review.">
            {(p) => <textarea {...p} rows={2} maxLength={2048} style={field.control} value={comment} onChange={(e) => setComment(e.target.value)} />}
          </Field>
          {error && <p role="alert" style={field.error}>{error}</p>}
          <div style={actionsRow}>
            {item.scale === '1to5'
              ? <DSButton variant="primary" disabled={busy || stars === 0} onClick={() => submit({ stars })}>Submit label</DSButton>
              : (
                <>
                  <DSButton variant="danger" disabled={busy} onClick={() => submit({ pass: false })}>Fail</DSButton>
                  <DSButton variant="primary" disabled={busy} onClick={() => submit({ pass: true })}>Pass</DSButton>
                </>
              )}
          </div>
        </>
      )}
    </section>
  )
}

function Block({ title, text }: { title: string; text: string }) {
  return (
    <div>
      <div style={{ fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.04em', color: ds.textSecondary }}>{title}</div>
      <pre style={{ margin: '4px 0 0', whiteSpace: 'pre-wrap', overflowWrap: 'anywhere', fontSize: 13, maxHeight: 280, overflow: 'auto', fontFamily: 'inherit' }}>{text}</pre>
    </div>
  )
}

function CloseDialog({ id, pending, onClose, onClosed }: { id: number; pending: number; onClose: () => void; onClosed: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const run = async () => {
    setBusy(true)
    setError('')
    try {
      await closeReview(id)
      onClosed()
    } catch (e) {
      const status = (e as { status?: number }).status
      setError(status === 409 ? 'The experiment is not waiting for review any more.' : describeEvalError(e))
      setBusy(false)
    }
  }
  return (
    <Dialog title="Close the review?" onClose={onClose}>
      <p style={{ margin: 0, fontSize: 13 }}>
        {pending} {pending === 1 ? 'answer is' : 'answers are'} still waiting. Closing leaves them unlabelled: they are left out of the comparison, never counted as passes. The experiment then finishes.
      </p>
      {error && <p role="alert" style={field.error}>Could not close: {error}</p>}
      <div style={actionsRow}>
        <DSButton onClick={onClose}>Keep reviewing</DSButton>
        <DSButton variant="danger" disabled={busy} onClick={run}>Close review</DSButton>
      </div>
    </Dialog>
  )
}

/** Compares an llm grader with a human grader of the dataset, so an author can tell whether to trust the judge. */
export function CalibrationPanel({ datasetId, experimentId }: { datasetId: number; experimentId?: number }) {
  const [graders, setGraders] = useState<EvalGrader[] | null>(null)
  const [llm, setLlm] = useState('')
  const [human, setHuman] = useState('')
  const [report, setReport] = useState<Calibration | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    let live = true
    listGraders(datasetId)
      .then((g) => {
        if (!live) return
        setGraders(g)
        const l = g.filter((x) => String(x.kind) === 'llm')
        const h = g.filter((x) => String(x.kind) === 'human')
        if (l.length === 1) setLlm(l[0].name)
        if (h.length === 1) setHuman(h[0].name)
      })
      .catch((e: unknown) => { if (live) { setError(describeEvalError(e)); setGraders([]) } })
    return () => { live = false }
  }, [datasetId])

  const llms = (graders ?? []).filter((g) => String(g.kind) === 'llm')
  const humans = (graders ?? []).filter((g) => String(g.kind) === 'human')

  const run = async () => {
    setBusy(true)
    setError('')
    try {
      setReport(await getCalibration(datasetId, llm, human, experimentId))
    } catch (e) {
      setReport(null)
      setError(describeEvalError(e))
    } finally {
      setBusy(false)
    }
  }

  const r = report?.report
  return (
    <Section title="How far to trust the judge">
      <p style={{ margin: '0 0 8px', fontSize: 13, color: ds.textSecondary }}>
        Compares a judge check (a model) with a human check on the answers both graded: how often they agree, and Cohen&apos;s kappa, which discounts agreement by chance.
      </p>
      {!graders ? (
        <div role="status" style={{ fontSize: 13, color: ds.textMuted }}>Loading checks…</div>
      ) : llms.length === 0 || humans.length === 0 ? (
        <p style={{ margin: 0, fontSize: 13, color: ds.textSecondary }}>
          Calibration needs a dataset with a judge check (kind llm) and a human check. {llms.length === 0 ? 'This dataset has no judge check.' : 'This dataset has no human check.'}
        </p>
      ) : (
        <div style={{ display: 'flex', gap: 12, alignItems: 'flex-end', flexWrap: 'wrap' }}>
          <Field label="Judge check">
            {(p) => <select {...p} style={field.control} value={llm} onChange={(e) => setLlm(e.target.value)}><option value="">Choose…</option>{llms.map((g) => <option key={g.id} value={g.name}>{g.name}</option>)}</select>}
          </Field>
          <Field label="Human check">
            {(p) => <select {...p} style={field.control} value={human} onChange={(e) => setHuman(e.target.value)}><option value="">Choose…</option>{humans.map((g) => <option key={g.id} value={g.name}>{g.name}</option>)}</select>}
          </Field>
          <DSButton disabled={busy || !llm || !human} onClick={run}>Check agreement</DSButton>
        </div>
      )}
      {error && <p role="alert" style={{ ...field.error, marginTop: 8 }}>Could not calibrate: {error}</p>}
      {report && report.tooFewPairs && (
        <p role="status" style={{ margin: '8px 0 0', fontSize: 13 }}>
          Not enough answers graded by both yet: at least {report.minPairs} are needed{r ? `, and there are ${r.pairs}` : ''}.
        </p>
      )}
      {report && !report.tooFewPairs && r && (
        <div style={{ marginTop: 12, display: 'flex', flexDirection: 'column', gap: 8 }}>
          {!r.trustworthy && (
            <p role="alert" style={{ margin: 0, fontSize: 13, color: ds.warning }}>
              {r.kappaDefined
                ? `Do not trust this judge yet. Its agreement with people is low (kappa ${r.kappa.toFixed(2)}, under 0.4). Improve its rubric before using it to decide anything.`
                : 'Do not trust this judge yet. One side gave the same verdict every time, so agreement says nothing about it. Add cases it should fail and pass.'}
            </p>
          )}
          {r.trustworthy && <p style={{ margin: 0, fontSize: 13, color: ds.success }}>The judge agrees with people well enough to be used, with a deterministic check beside it.</p>}
          <dl style={{ margin: 0, display: 'grid', gridTemplateColumns: 'max-content 1fr', gap: '4px 16px', fontSize: 13 }}>
            <dt style={{ color: ds.textSecondary }}>Answers graded by both</dt><dd style={{ margin: 0 }}>{r.pairs}</dd>
            <dt style={{ color: ds.textSecondary }}>Agreement</dt><dd style={{ margin: 0 }}>{pct(r.agreement)}</dd>
            <dt style={{ color: ds.textSecondary }}>Kappa</dt><dd style={{ margin: 0 }}>{r.kappaDefined ? r.kappa.toFixed(2) : 'not defined'}</dd>
            <dt style={{ color: ds.textSecondary }}>Both pass / both fail</dt><dd style={{ margin: 0 }}>{r.confusion.bothPass} / {r.confusion.bothFail}</dd>
            <dt style={{ color: ds.textSecondary }}>Judge pass, human fail</dt><dd style={{ margin: 0 }}>{r.confusion.llmPassHumanFail}</dd>
            <dt style={{ color: ds.textSecondary }}>Judge fail, human pass</dt><dd style={{ margin: 0 }}>{r.confusion.llmFailHumanPass}</dd>
          </dl>
          {(r.disagreements?.length ?? 0) > 0 && (
            <details>
              <summary style={{ cursor: 'pointer', fontSize: 13 }}>{r.disagreements!.length} disagreements{r.disagreementsCut ? ' (the biggest are shown)' : ''}</summary>
              <ul style={{ margin: '6px 0 0', paddingLeft: 18, fontSize: 12 }}>
                {r.disagreements!.map((d) => (
                  <li key={d.resultId}>Case {d.caseId}: judge {d.llmPass ? 'pass' : 'fail'} ({d.llmScore.toFixed(2)}), human {d.humanPass ? 'pass' : 'fail'} ({d.humanScore.toFixed(2)})</li>
                ))}
              </ul>
            </details>
          )}
        </div>
      )}
    </Section>
  )
}
