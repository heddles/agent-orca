/** Evaluate, experiments: the list, and the wizard that queues a new one. */
import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router'
import { ApiError, type MissingRef } from '../api/builder'
import { getCatalog, type CatalogEntry } from '../api/catalog'
import {
  createExperiment, describeEvalError, listDatasets, listExperiments,
  type EvalDataset, type Experiment, type ExperimentArm,
} from '../api/evals'
import { listRevisions, type RevisionSummary } from '../api/revisions'
import { listModelProviders, type ModelProviderSummary } from '../api/sse'
import { canWriteEvals, useRoles } from '../lib/access'
import { Dialog } from '../lib/dialog'
import { ds } from '../lib/designSystem'
import { armWords, isActive, STATUS_VARIANT, STATUS_WORDS, usd, when } from '../lib/evalFormat'
import { ICON } from '../lib/icons'
import { EmptyState, Page } from '../lib/layout'
import { useNamespace } from '../lib/namespace'
import { DSButton, DSChip } from '../lib/primitives'
import { experimentPath } from '../lib/routes'
import { Field, field } from './BuilderParts'
import { actionsRow, table, td, th } from './EvalDatasets'

const POLL_MS = 5000

export function EvalExperiments() {
  const { namespace } = useNamespace()
  const roles = useRoles()
  const writable = canWriteEvals(roles)
  const [experiments, setExperiments] = useState<Experiment[] | null>(null)
  const [error, setError] = useState('')
  const [creating, setCreating] = useState(false)

  const load = useCallback(() => {
    let live = true
    listExperiments({ namespace: namespace || undefined, limit: 100 })
      .then((r) => { if (live) { setExperiments(r.experiments); setError('') } })
      .catch((e: unknown) => { if (live) { setError(describeEvalError(e)); setExperiments((old) => old ?? []) } })
    return () => { live = false }
  }, [namespace])
  useEffect(() => load(), [load])

  const anyActive = !!experiments?.some(isActive)
  useEffect(() => {
    if (!anyActive) return
    const t = setInterval(() => { load() }, POLL_MS)
    return () => clearInterval(t)
  }, [anyActive, load])

  return (
    <div style={{ padding: '0 24px 24px' }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12, marginBottom: 12 }}>
        <p style={{ margin: 0, fontSize: 13, color: ds.textSecondary }}>
          An experiment runs two to four versions of an agent over a dataset and compares how often each gives a good answer.
        </p>
        {writable && <DSButton variant="primary" onClick={() => setCreating(true)}>New experiment</DSButton>}
      </div>
      {error && <div role="alert" style={{ color: ds.error, fontSize: 13, marginBottom: 12 }}>Could not load experiments: {error}</div>}
      {!experiments ? (
        <div role="status" style={{ color: ds.textMuted, fontSize: 13 }}>Loading experiments…</div>
      ) : experiments.length === 0 ? (
        !error && (
          <EmptyState icon={ICON.chart} title="No experiments yet">
            {writable
              ? 'Start one to compare a new version of an agent, or a different model, with the current one on your dataset.'
              : 'No experiments have been run in this workspace. A builder can start one.'}
          </EmptyState>
        )
      ) : (
        <table style={table}>
          <thead>
            <tr>
              <th style={th}>Experiment</th><th style={th}>Status</th><th style={th}>Versions compared</th>
              <th style={th}>Progress</th><th style={th}>Spent</th><th style={th}>Started</th>
            </tr>
          </thead>
          <tbody>
            {experiments.map((e) => (
              <tr key={e.id}>
                <td style={td}>
                  <Link to={experimentPath(e.id)} style={{ color: ds.accent, fontWeight: 600, textDecoration: 'none' }}>{e.name || `Experiment ${e.id}`}</Link>
                  <div style={{ fontSize: 12, color: ds.textSecondary }}>#{e.id} · {e.namespace}</div>
                </td>
                <td style={td}><DSChip size="sm" variant={STATUS_VARIANT[e.status]}>{STATUS_WORDS[e.status] ?? e.status}</DSChip></td>
                <td style={td}>{e.arms.map((a, i) => <div key={i}>{armWords(a)}</div>)}</td>
                <td style={td}>{e.resultsDone} of {e.resultsTotal}</td>
                <td style={td}>{usd(e.spentUsd)} of {usd(e.budgetUsd)}</td>
                <td style={td}>{when(e.startedAt ?? e.createdAt)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {creating && <NewExperimentDialog defaultNamespace={namespace} onClose={() => setCreating(false)} onCreated={() => { setCreating(false); load() }} />}
    </div>
  )
}

type ArmKind = 'live' | 'revision' | 'model'
interface ArmDraft { label: string; kind: ArmKind; agent: string; revision: string; modelProvider: string }
const newArm = (kind: ArmKind = 'live', agent = ''): ArmDraft => ({ label: '', kind, agent, revision: '', modelProvider: '' })

export function armFromDraft(a: ArmDraft): ExperimentArm {
  const arm: ExperimentArm = { agent: a.agent }
  if (a.label.trim()) arm.label = a.label.trim()
  if (a.kind === 'revision' && a.revision) arm.revision = Number(a.revision)
  if (a.kind === 'model' && a.modelProvider) arm.modelProvider = a.modelProvider
  return arm
}

const LABEL_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/

function armProblem(a: ArmDraft): string {
  if (!a.agent) return 'Choose an agent.'
  if (a.kind === 'revision' && !a.revision) return 'Choose a saved version.'
  if (a.kind === 'model' && !a.modelProvider) return 'Choose a model.'
  if (a.label && (a.label.length > 20 || !LABEL_RE.test(a.label))) return 'A label is lower case letters, digits and dashes, at most 20 characters.'
  return ''
}

const STEPS = ['What to run', 'Versions to compare', 'Limits'] as const

function NewExperimentDialog({ defaultNamespace, onClose, onCreated }: { defaultNamespace: string; onClose: () => void; onCreated: () => void }) {
  const [step, setStep] = useState(0)
  const [datasets, setDatasets] = useState<EvalDataset[] | null>(null)
  const [agents, setAgents] = useState<CatalogEntry[]>([])
  const [providers, setProviders] = useState<ModelProviderSummary[]>([])
  const [loadError, setLoadError] = useState('')
  const [datasetId, setDatasetId] = useState('')
  const [name, setName] = useState('')
  const [arms, setArms] = useState<ArmDraft[]>([newArm('live'), newArm('revision')])
  const [repeats, setRepeats] = useState('1')
  const [budget, setBudget] = useState('5')
  const [runBudget, setRunBudget] = useState('')
  const [concurrency, setConcurrency] = useState('4')
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState('')
  const [missing, setMissing] = useState<(MissingRef & { arm?: string })[]>([])
  const [warnings, setWarnings] = useState<string[]>([])

  useEffect(() => {
    let live = true
    listDatasets({ namespace: defaultNamespace || undefined })
      .then((d) => { if (live) setDatasets(d) })
      .catch((e: unknown) => { if (live) { setLoadError(describeEvalError(e)); setDatasets([]) } })
    return () => { live = false }
  }, [defaultNamespace])

  const dataset = datasets?.find((d) => String(d.id) === datasetId)
  // The arms run in the dataset's workspace, so its agents and models are the choices.
  const armNamespace = dataset?.namespace ?? defaultNamespace
  useEffect(() => {
    let live = true
    getCatalog(armNamespace).then((c) => { if (live) setAgents(c.filter((a) => !a.template)) }).catch(() => {})
    listModelProviders(armNamespace).then((p) => { if (live) setProviders(p) }).catch(() => {})
    return () => { live = false }
  }, [armNamespace])
  const setArm = (i: number, patch: Partial<ArmDraft>) => setArms((old) => old.map((a, j) => (j === i ? { ...a, ...patch } : a)))

  // A dataset made for one agent suggests it for the arms that have none.
  const pickDataset = (id: string) => {
    setDatasetId(id)
    const d = datasets?.find((x) => String(x.id) === id)
    if (d?.agent) setArms((old) => old.map((a) => (a.agent ? a : { ...a, agent: d.agent! })))
  }

  const budgetNum = Number(budget)
  const limitsProblem =
    !(budgetNum > 0) ? 'The budget must be more than zero.'
      : runBudget && !(Number(runBudget) > 0) ? 'The cap for one run must be more than zero.'
      : !(Number.isInteger(Number(repeats)) && Number(repeats) >= 1 && Number(repeats) <= 5) ? 'Repeats is a whole number from 1 to 5.'
      : !(Number.isInteger(Number(concurrency)) && Number(concurrency) >= 1 && Number(concurrency) <= 16) ? 'Concurrency is a whole number from 1 to 16.'
      : ''
  const stepProblem = [
    datasetId ? '' : 'Choose a dataset.',
    arms.map(armProblem).find(Boolean) ?? '',
    limitsProblem,
  ]

  const submit = async () => {
    setBusy(true)
    setFailure('')
    setMissing([])
    try {
      const e = await createExperiment({
        datasetId: Number(datasetId),
        name: name.trim() || undefined,
        arms: arms.map(armFromDraft),
        repeats: Number(repeats),
        budgetUsd: budgetNum,
        runBudgetUsd: runBudget ? Number(runBudget) : undefined,
        concurrency: Number(concurrency),
      })
      if (e.warnings && e.warnings.length > 0) { setWarnings(e.warnings); setBusy(false); return }
      onCreated()
    } catch (err) {
      if (err instanceof ApiError && err.status === 422 && err.missing && err.missing.length > 0) {
        setMissing(err.missing)
        setFailure('Something the versions need is missing from the workspace.')
      } else if (err instanceof ApiError && err.status === 409) {
        setFailure(err.message || 'The dataset has no checks, or too many experiments are already queued or running. Wait for one to finish.')
      } else {
        setFailure(describeEvalError(err))
      }
      setBusy(false)
    }
  }

  if (warnings.length > 0) {
    return (
      <Dialog title="Experiment queued" onClose={onCreated}>
        <p style={{ margin: 0, fontSize: 13 }}>The experiment is queued. Please note:</p>
        <ul style={{ margin: 0, paddingLeft: 18, fontSize: 13, color: ds.warning }}>{warnings.map((w) => <li key={w}>{w}</li>)}</ul>
        <div style={actionsRow}><DSButton variant="primary" onClick={onCreated}>Done</DSButton></div>
      </Dialog>
    )
  }

  const last = step === STEPS.length - 1
  return (
    <Dialog title="New experiment" onClose={onClose} width={640}>
      <p style={{ margin: 0, fontSize: 12, color: ds.textSecondary }} aria-live="polite">Step {step + 1} of {STEPS.length}: {STEPS[step]}</p>
      {loadError && <p role="alert" style={field.error}>Could not load datasets: {loadError}</p>}
      <form onSubmit={(e) => { e.preventDefault(); if (last) { if (!stepProblem.some(Boolean)) void submit() } else if (!stepProblem[step]) setStep(step + 1) }} style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        {step === 0 && (
          <>
            <Field label="Dataset" help="The cases to run. It is frozen at its current version when you start.">
              {(p) => datasets === null ? <span role="status" style={field.help}>Loading datasets…</span> : (
                <select {...p} style={field.control} value={datasetId} onChange={(e) => pickDataset(e.target.value)}>
                  <option value="">Choose a dataset…</option>
                  {datasets.map((d) => <option key={d.id} value={d.id}>{d.name} (version {d.version}, {d.caseCount} cases)</option>)}
                </select>
              )}
            </Field>
            {datasets !== null && datasets.length === 0 && !loadError && <p style={field.help}>There are no datasets yet. Create one under Datasets first.</p>}
            {dataset && dataset.caseCount === 0 && <p style={field.error}>This dataset has no cases yet.</p>}
            <Field label="Name (optional)">
              {(p) => <input {...p} style={field.control} value={name} onChange={(e) => setName(e.target.value)} />}
            </Field>
          </>
        )}
        {step === 1 && (
          <>
            <p style={field.help}>The first version is the baseline: the others are compared with it. Use 2 to 4.</p>
            {arms.map((a, i) => (
              <ArmEditor key={i} index={i} arm={a} agents={agents} providers={providers}
                onChange={(patch) => setArm(i, patch)}
                onRemove={arms.length > 2 ? () => setArms((old) => old.filter((_, j) => j !== i)) : undefined} />
            ))}
            {arms.length < 4 && <DSButton type="button" size="sm" onClick={() => setArms((old) => [...old, newArm('model', old[0]?.agent ?? '')])}>Add a version</DSButton>}
          </>
        )}
        {step === 2 && (
          <>
            <Field label="Budget (USD)" help="A hard cap on what the runs and the judge may cost. The experiment stops when it is spent.">
              {(p) => <input {...p} type="number" min="0" step="0.5" style={field.control} value={budget} onChange={(e) => setBudget(e.target.value)} />}
            </Field>
            <Field label="Cap for one run (USD, optional)">
              {(p) => <input {...p} type="number" min="0" step="0.1" style={field.control} value={runBudget} onChange={(e) => setRunBudget(e.target.value)} />}
            </Field>
            <Field label="Repeats" help="Runs of each case on each version, 1 to 5. More repeats show how steady an answer is.">
              {(p) => <input {...p} type="number" min="1" max="5" style={field.control} value={repeats} onChange={(e) => setRepeats(e.target.value)} />}
            </Field>
            <Field label="Concurrency" help="Runs at the same time, 1 to 16.">
              {(p) => <input {...p} type="number" min="1" max="16" style={field.control} value={concurrency} onChange={(e) => setConcurrency(e.target.value)} />}
            </Field>
          </>
        )}
        {stepProblem[step] && <p style={field.help}>{stepProblem[step]}</p>}
        {failure && <p role="alert" style={field.error}>Could not start the experiment: {failure}</p>}
        {missing.length > 0 && <MissingRefs missing={missing} />}
        <div style={actionsRow}>
          <DSButton type="button" onClick={onClose}>Cancel</DSButton>
          {step > 0 && <DSButton type="button" onClick={() => setStep(step - 1)}>Back</DSButton>}
          {last
            ? <DSButton type="submit" variant="primary" disabled={busy || stepProblem.some(Boolean)}>Start experiment</DSButton>
            : <DSButton type="submit" variant="primary" disabled={!!stepProblem[step]}>Next</DSButton>}
        </div>
      </form>
    </Dialog>
  )
}

/** The 422 payload of a create call, readable: what each version needs that the workspace lacks. */
export function MissingRefs({ missing }: { missing: (MissingRef & { arm?: string })[] }) {
  return (
    <div role="group" aria-label="Missing from the workspace" style={{ border: '1px solid var(--ds-border)', borderRadius: 6, padding: 10 }}>
      <strong style={{ fontSize: 12 }}>Missing from the workspace</strong>
      <ul style={{ margin: '6px 0 0', paddingLeft: 18, fontSize: 12 }}>
        {missing.map((m, i) => (
          <li key={`${m.arm ?? ''}${m.kind}${m.name}${i}`}>
            {m.arm ? <>Version <strong>{m.arm}</strong> needs </> : 'Needs '}
            {m.kind} <strong>{m.name}</strong>
            {m.namespace ? ` in ${m.namespace}` : ''}.{' '}
            {m.alternatives.length > 0 ? `Available instead: ${m.alternatives.join(', ')}.` : 'Nothing similar is available; an administrator needs to add it.'}
          </li>
        ))}
      </ul>
    </div>
  )
}

function ArmEditor({ index, arm, agents, providers, onChange, onRemove }: {
  index: number; arm: ArmDraft; agents: CatalogEntry[]; providers: ModelProviderSummary[]
  onChange: (p: Partial<ArmDraft>) => void; onRemove?: () => void
}) {
  const [revisions, setRevisions] = useState<RevisionSummary[]>([])
  const [revError, setRevError] = useState('')
  const entry = agents.find((a) => a.name === arm.agent)
  const ns = entry?.namespace
  useEffect(() => {
    if (arm.kind !== 'revision' || !arm.agent || !ns) return
    let live = true
    listRevisions('agents', ns, arm.agent, { limit: 50 })
      .then((p) => { if (live) { setRevisions(p.revisions); setRevError('') } })
      .catch((e: unknown) => { if (live) { setRevisions([]); setRevError(e instanceof Error ? e.message : String(e)) } })
    return () => { live = false }
  }, [arm.kind, arm.agent, ns])
  const problem = armProblem(arm)
  const n = index + 1
  return (
    <fieldset style={{ border: '1px solid var(--ds-border)', borderRadius: 6, padding: 10, display: 'flex', flexDirection: 'column', gap: 8, margin: 0 }}>
      <legend style={{ fontSize: 12, fontWeight: 600 }}>{index === 0 ? 'Baseline (version 1)' : `Version ${n}`}</legend>
      <Field label={`Version ${n} agent`}>
        {(p) => (
          <select {...p} style={field.control} value={arm.agent} onChange={(e) => onChange({ agent: e.target.value, revision: '' })}>
            <option value="">Choose an agent…</option>
            {agents.map((a) => <option key={`${a.namespace}/${a.name}`} value={a.name}>{a.name}</option>)}
          </select>
        )}
      </Field>
      <Field label={`Version ${n} runs`}>
        {(p) => (
          <select {...p} style={field.control} value={arm.kind} onChange={(e) => onChange({ kind: e.target.value as ArmKind })}>
            <option value="live">The agent as it is now</option>
            <option value="revision">A saved version of the agent</option>
            <option value="model">The agent on a different model</option>
          </select>
        )}
      </Field>
      {arm.kind === 'revision' && (
        <Field label={`Version ${n} saved version`} error={revError ? `Could not load saved versions: ${revError}` : ''}>
          {(p) => (
            <select {...p} style={field.control} value={arm.revision} onChange={(e) => onChange({ revision: e.target.value })}>
              <option value="">Choose a saved version…</option>
              {revisions.map((r) => <option key={r.revision} value={r.revision}>Revision {r.revision}{r.summary ? `: ${r.summary}` : ''}</option>)}
            </select>
          )}
        </Field>
      )}
      {arm.kind === 'model' && (
        <Field label={`Version ${n} model`}>
          {(p) => (
            <select {...p} style={field.control} value={arm.modelProvider} onChange={(e) => onChange({ modelProvider: e.target.value })}>
              <option value="">Choose a model…</option>
              {providers.map((m) => <option key={`${m.namespace}/${m.name}`} value={m.name}>{m.name} ({m.litellmModel})</option>)}
            </select>
          )}
        </Field>
      )}
      <Field label={`Version ${n} label (optional)`} help="Shown in the results. Defaults to arm1, arm2.">
        {(p) => <input {...p} style={field.control} value={arm.label} onChange={(e) => onChange({ label: e.target.value })} />}
      </Field>
      {problem && arm.agent && <p style={field.help}>{problem}</p>}
      {onRemove && <div><DSButton type="button" size="sm" variant="ghost" onClick={onRemove}>Remove version {n}</DSButton></div>}
    </fieldset>
  )
}
