/**
 * The Quality section of an agent: its publish gate (the bar a new version must clear before Publish),
 * where the gate stands, and "Evaluate now". Admins edit the gate; builders run the evaluation.
 * GateFailure is what the publish flow shows when the gate blocks a publish.
 */
import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router'
import { ApiError } from '../api/builder'
import {
  deleteGate, describeEvalError, evaluateAgent, getGate, listDatasets, OVERRIDE_REASON, putGate,
  type EvalDataset, type Experiment, type GateDecision, type GateInput, type GateState,
} from '../api/evals'
import { listRevisions, type RevisionSummary } from '../api/revisions'
import { canAdminEvals, canWriteEvals, useRoles } from '../lib/access'
import { ds } from '../lib/designSystem'
import { GATE_VARIANT, GATE_WORDS, pct, points } from '../lib/evalFormat'
import { DSButton, DSChip } from '../lib/primitives'
import { datasetPath, experimentPath } from '../lib/routes'
import { Dialog } from '../lib/dialog'
import { Field, field } from './BuilderParts'
import { MissingRefs } from './EvalExperiments'
import { actionsRow } from './EvalDatasets'

const card = { border: '1px solid var(--ds-border)', borderRadius: 8, padding: 14, display: 'flex', flexDirection: 'column', gap: 10 } as const

export function AgentQuality({ namespace, name, saved = true, onGateChange }: { namespace: string; name: string; saved?: boolean; onGateChange?: (s: GateState) => void }) {
  const roles = useRoles()
  const admin = canAdminEvals(roles)
  const builder = canWriteEvals(roles)
  const [state, setState] = useState<GateState | null>(null)
  const [error, setError] = useState('')
  const [editing, setEditing] = useState(false)
  const [removing, setRemoving] = useState(false)
  const [busy, setBusy] = useState(false)
  const [started, setStarted] = useState<Experiment | null>(null)
  const [evalError, setEvalError] = useState('')
  const [evalMissing, setEvalMissing] = useState<ApiError['missing']>()

  const load = useCallback(() => {
    let live = true
    getGate(namespace, name)
      .then((s) => { if (live) { setState(s); setError(''); onGateChange?.(s) } })
      .catch((e: unknown) => { if (live) { setError(describeEvalError(e)); setState((old) => old ?? { gate: null, overrideAllowed: false }) } })
    return () => { live = false }
  }, [namespace, name, onGateChange])
  useEffect(() => load(), [load])

  const evaluate = async () => {
    setBusy(true)
    setEvalError('')
    setEvalMissing(undefined)
    try {
      setStarted(await evaluateAgent(namespace, name))
      load()
    } catch (e) {
      const status = (e as { status?: number }).status
      if (status === 422 && e instanceof ApiError && e.missing?.length) { setEvalMissing(e.missing); setEvalError('The baseline revision, or something an arm needs, is missing.') }
      else if (status === 409) setEvalError((e as Error).message || 'This agent has no gate, or too many experiments are already queued or running.')
      else setEvalError(describeEvalError(e))
    } finally {
      setBusy(false)
    }
  }

  const gate = state?.gate ?? null
  return (
    <section aria-labelledby="quality-heading" style={card}>
      <h2 id="quality-heading" style={{ margin: 0, fontSize: 15 }}>Quality</h2>
      <p style={field.help}>A gate makes a new version of this agent prove itself on a dataset before it can be published.</p>
      {error && <p role="alert" style={field.error}>Could not load the gate: {error}</p>}
      {!state ? (
        !error && <span role="status" style={field.help}>Loading…</span>
      ) : !gate ? (
        <p style={field.help}>
          No gate is set, so publishing is not checked. {admin ? 'Set one to require an evaluation before Publish.' : 'An administrator can set one.'}
        </p>
      ) : (
        <>
          <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
            <strong style={{ fontSize: 13 }}>Gate status</strong>
            {!gate.enabled ? <DSChip size="sm" variant="neutral">Switched off</DSChip>
              : state.decision ? <DSChip size="sm" variant={GATE_VARIANT[state.decision.status]} dot>{GATE_WORDS[state.decision.status]}</DSChip> : null}
          </div>
          {gate.enabled && state.decision && <DecisionDetail d={state.decision} />}
          <GateSummary gate={gate} />
        </>
      )}
      {started && (
        <p role="status" style={field.help}>
          Evaluation queued. <Link to={experimentPath(started.id)} style={{ color: ds.accent }}>Follow experiment {started.id}</Link>.
          {' '}Refresh the status when it ends.
        </p>
      )}
      {evalError && <p role="alert" style={field.error}>Could not start the evaluation: {evalError}</p>}
      {evalMissing && evalMissing.length > 0 && <MissingRefs missing={evalMissing} />}
      <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
        {builder && gate && gate.enabled && (
          <DSButton variant="primary" disabled={busy || !saved} title={!saved ? 'Save the agent first' : undefined} onClick={evaluate}>Evaluate now</DSButton>
        )}
        {gate && <DSButton variant="secondary" onClick={load}>Refresh status</DSButton>}
        {admin && state && <DSButton variant="secondary" onClick={() => setEditing(true)}>{gate ? 'Edit gate' : 'Set up a gate'}</DSButton>}
        {admin && gate && <DSButton variant="ghost" onClick={() => setRemoving(true)}>Remove gate</DSButton>}
      </div>
      {gate && builder && !saved && <p style={field.help}>Save the agent before evaluating it: the evaluation runs the saved agent.</p>}
      {!admin && gate && <p style={field.help}>Only an administrator can change the gate.</p>}
      {editing && <GateDialog namespace={namespace} agent={name} gate={gate} onClose={() => setEditing(false)} onSaved={() => { setEditing(false); load() }} />}
      {removing && <RemoveDialog namespace={namespace} agent={name} onClose={() => setRemoving(false)} onRemoved={() => { setRemoving(false); setStarted(null); load() }} />}
    </section>
  )
}

function GateSummary({ gate }: { gate: NonNullable<GateState['gate']> }) {
  return (
    <p style={{ ...field.help, color: ds.textSecondary }}>
      The new version must be no worse than revision {gate.baselineRevision} by more than {pct(gate.margin)} on{' '}
      <Link to={datasetPath(gate.datasetId)} style={{ color: ds.accent }}>dataset {gate.datasetId}</Link>, over at least {gate.minCases} cases
      {gate.minPassRate !== undefined ? `, with a pass rate of at least ${pct(gate.minPassRate)}` : ''}
      {gate.maxRegressions !== undefined ? `, and at most ${gate.maxRegressions} cases that went from pass to fail` : ''}.
      Each evaluation may spend up to ${gate.budgetUsd}.
    </p>
  )
}

/** Why the gate says what it says: the reason, and the numbers the latest evaluation found. */
export function DecisionDetail({ d }: { d: GateDecision }) {
  const b = d.best
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
      {d.reason && <p style={{ margin: 0, fontSize: 13 }}>{d.reason}</p>}
      {b && (b.cases !== undefined || b.passRate !== undefined) && (
        <p style={{ margin: 0, fontSize: 12, color: ds.textSecondary }}>
          Latest evaluation: {b.cases ?? 0} cases, new version {pct(b.passRate)} against {pct(b.baselinePassRate)} before
          {b.diffLo !== undefined ? ` (the low end of the difference is ${points(b.diffLo)})` : ''}
          {b.regressions !== undefined ? `, ${b.regressions} regressed` : ''}
          {b.unreviewed ? `, ${b.unreviewed} still waiting for review` : ''}.
        </p>
      )}
      {d.experimentId ? <Link to={experimentPath(d.experimentId)} style={{ color: ds.accent, fontSize: 12 }}>Open experiment {d.experimentId}</Link> : null}
    </div>
  )
}

/** Shown by the publish flow when the gate answers 409: the reason, and an admin-only override that needs a reason. */
export function GateFailure({ gate, canOverride, overrideAllowed = true, busy, onOverride }: {
  gate: GateDecision; canOverride: boolean; overrideAllowed?: boolean; busy: boolean; onOverride: (reason: string) => void
}) {
  const [open, setOpen] = useState(false)
  const [reason, setReason] = useState('')
  const trimmed = reason.trim()
  const problem = trimmed.length < OVERRIDE_REASON.min ? `Give a reason of at least ${OVERRIDE_REASON.min} characters.`
    : trimmed.length > OVERRIDE_REASON.max ? `A reason is at most ${OVERRIDE_REASON.max} characters.` : ''
  const hint = gate.status === 'missing' ? 'Run "Evaluate now" in the Quality section.'
    : gate.status === 'stale' ? 'The agent or the dataset changed since the last evaluation. Run "Evaluate now" again.' : ''
  return (
    <div role="alert" style={{ border: '1px solid var(--ds-error)', borderRadius: 6, padding: 10, display: 'flex', flexDirection: 'column', gap: 6 }}>
      <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
        <strong style={{ fontSize: 13, color: ds.error }}>Publishing is blocked by the quality gate</strong>
        <DSChip size="sm" variant={GATE_VARIANT[gate.status] ?? 'neutral'}>{GATE_WORDS[gate.status] ?? gate.status}</DSChip>
      </div>
      <DecisionDetail d={gate} />
      {hint && <p style={field.help}>{hint}</p>}
      {canOverride && overrideAllowed ? (
        open ? (
          <form onSubmit={(e) => { e.preventDefault(); if (!problem) onOverride(trimmed) }} style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
            <Field label="Reason for publishing anyway" help={`${OVERRIDE_REASON.min} to ${OVERRIDE_REASON.max} characters. It is written to the audit trail.`} error={reason && problem ? problem : ''}>
              {(p) => <textarea {...p} rows={3} style={field.control} value={reason} onChange={(e) => setReason(e.target.value)} />}
            </Field>
            <div style={actionsRow}>
              <DSButton type="button" onClick={() => setOpen(false)}>Cancel</DSButton>
              <DSButton type="submit" variant="danger" disabled={busy || !!problem}>Publish anyway</DSButton>
            </div>
          </form>
        ) : <div><DSButton size="sm" variant="danger" onClick={() => setOpen(true)}>Override the gate…</DSButton></div>
      ) : (
        <p style={field.help}>
          {canOverride ? 'This platform does not allow publishing past a gate. Change the gate, or pass it.' : 'Only an administrator can publish past a gate.'}
        </p>
      )}
    </div>
  )
}

function GateDialog({ namespace, agent, gate, onClose, onSaved }: { namespace: string; agent: string; gate: GateState['gate']; onClose: () => void; onSaved: () => void }) {
  const [datasets, setDatasets] = useState<EvalDataset[] | null>(null)
  const [revisions, setRevisions] = useState<RevisionSummary[]>([])
  const [datasetId, setDatasetId] = useState(gate ? String(gate.datasetId) : '')
  const [baseline, setBaseline] = useState(gate ? String(gate.baselineRevision) : '')
  const [margin, setMargin] = useState(gate ? String(gate.margin) : '0.05')
  const [minCases, setMinCases] = useState(gate ? String(gate.minCases) : '30')
  const [minPassRate, setMinPassRate] = useState(gate?.minPassRate !== undefined ? String(gate.minPassRate) : '')
  const [maxRegressions, setMaxRegressions] = useState(gate?.maxRegressions !== undefined ? String(gate.maxRegressions) : '')
  const [budget, setBudget] = useState(gate ? String(gate.budgetUsd) : '5')
  const [enabled, setEnabled] = useState(gate ? gate.enabled : true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    let live = true
    listDatasets({ namespace }).then((d) => { if (live) setDatasets(d) }).catch((e: unknown) => { if (live) { setError(describeEvalError(e)); setDatasets([]) } })
    listRevisions('agents', namespace, agent, { limit: 50 }).then((p) => { if (live) setRevisions(p.revisions) }).catch(() => {})
    return () => { live = false }
  }, [namespace, agent])

  const m = Number(margin)
  const problem =
    !datasetId ? 'Choose a dataset.'
      : !(Number.isInteger(Number(baseline)) && Number(baseline) >= 1) ? 'The baseline is a saved revision number, 1 or more.'
      : !(m > 0 && m < 1) ? 'The allowed drop is more than 0 and less than 1 (0.05 is 5 points).'
      : !(Number.isInteger(Number(minCases)) && Number(minCases) >= 1 && Number(minCases) <= 2000) ? 'The fewest cases is a whole number from 1 to 2000.'
      : minPassRate && !(Number(minPassRate) >= 0 && Number(minPassRate) <= 1) ? 'The lowest pass rate is from 0 to 1.'
      : maxRegressions && !(Number.isInteger(Number(maxRegressions)) && Number(maxRegressions) >= 0) ? 'The most regressions is a whole number, 0 or more.'
      : !(Number(budget) > 0) ? 'The budget must be more than zero.' : ''

  const save = async (e: React.FormEvent) => {
    e.preventDefault()
    if (problem) return
    const body: GateInput = {
      datasetId: Number(datasetId), baselineRevision: Number(baseline), margin: m, minCases: Number(minCases),
      budgetUsd: Number(budget), enabled,
      ...(minPassRate ? { minPassRate: Number(minPassRate) } : {}),
      ...(maxRegressions ? { maxRegressions: Number(maxRegressions) } : {}),
    }
    setBusy(true)
    setError('')
    try {
      await putGate(namespace, agent, body)
      onSaved()
    } catch (err) {
      const status = (err as { status?: number }).status
      setError(status === 409 ? `${(err as Error).message || 'The dataset has only judge (llm) checks.'} A gate needs a deterministic or human check on the dataset.` : describeEvalError(err))
      setBusy(false)
    }
  }

  return (
    <Dialog title={gate ? 'Edit the gate' : 'Set up a gate'} onClose={onClose} width={560}>
      <form onSubmit={save} style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <Field label="Dataset" help="The cases the evaluation runs. It needs a deterministic or human check.">
          {(p) => datasets === null ? <span role="status" style={field.help}>Loading…</span> : (
            <select {...p} style={field.control} value={datasetId} onChange={(e) => setDatasetId(e.target.value)}>
              <option value="">Choose a dataset…</option>
              {datasets.map((d) => <option key={d.id} value={d.id}>{d.name} (version {d.version})</option>)}
            </select>
          )}
        </Field>
        <Field label="Baseline revision" help="The saved revision of this agent the new version is compared with: the one that is live.">
          {(p) => (
            <>
              <input {...p} type="number" min="1" list="gate-revisions" style={field.control} value={baseline} onChange={(e) => setBaseline(e.target.value)} />
              <datalist id="gate-revisions">{revisions.map((r) => <option key={r.revision} value={r.revision}>{r.summary ?? ''}</option>)}</datalist>
            </>
          )}
        </Field>
        <Field label="Allowed drop in pass rate" help="0.05 means the new version may be up to 5 points worse and still pass.">
          {(p) => <input {...p} type="number" min="0" max="1" step="0.01" style={field.control} value={margin} onChange={(e) => setMargin(e.target.value)} />}
        </Field>
        <Field label="Fewest cases" help="An evaluation over fewer paired cases does not count.">
          {(p) => <input {...p} type="number" min="1" max="2000" style={field.control} value={minCases} onChange={(e) => setMinCases(e.target.value)} />}
        </Field>
        <Field label="Lowest pass rate (optional)" help="From 0 to 1: a floor on the new version's own pass rate.">
          {(p) => <input {...p} type="number" min="0" max="1" step="0.01" style={field.control} value={minPassRate} onChange={(e) => setMinPassRate(e.target.value)} />}
        </Field>
        <Field label="Most regressions (optional)" help="The most cases that may go from pass to fail.">
          {(p) => <input {...p} type="number" min="0" style={field.control} value={maxRegressions} onChange={(e) => setMaxRegressions(e.target.value)} />}
        </Field>
        <Field label="Budget per evaluation (USD)">
          {(p) => <input {...p} type="number" min="0" step="0.5" style={field.control} value={budget} onChange={(e) => setBudget(e.target.value)} />}
        </Field>
        <label style={{ fontSize: 13, display: 'flex', gap: 6, alignItems: 'center' }}>
          <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} /> Gate is on
        </label>
        {problem && <p style={field.help}>{problem}</p>}
        {error && <p role="alert" style={field.error}>Could not save the gate: {error}</p>}
        <div style={actionsRow}>
          <DSButton type="button" onClick={onClose}>Cancel</DSButton>
          <DSButton type="submit" variant="primary" disabled={busy || !!problem}>Save gate</DSButton>
        </div>
      </form>
    </Dialog>
  )
}

function RemoveDialog({ namespace, agent, onClose, onRemoved }: { namespace: string; agent: string; onClose: () => void; onRemoved: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const run = async () => {
    setBusy(true)
    setError('')
    try { await deleteGate(namespace, agent); onRemoved() } catch (e) { setError(describeEvalError(e)); setBusy(false) }
  }
  return (
    <Dialog title="Remove the gate?" onClose={onClose}>
      <p style={{ margin: 0, fontSize: 13 }}>Publishing this agent will no longer need an evaluation. The change is recorded in the audit trail.</p>
      {error && <p role="alert" style={field.error}>Could not remove: {error}</p>}
      <div style={actionsRow}>
        <DSButton onClick={onClose}>Keep the gate</DSButton>
        <DSButton variant="danger" disabled={busy} onClick={run}>Remove gate</DSButton>
      </div>
    </Dialog>
  )
}
