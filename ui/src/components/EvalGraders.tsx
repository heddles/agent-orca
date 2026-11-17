/** The graders of one dataset (checks on an agent's output), and a box to try them on pasted output. */
import { useCallback, useEffect, useState } from 'react'
import {
  deleteGrader, describeEvalError, EVAL_LIMITS, GRADER_KINDS, gradeOutput, listGraders, putGrader,
  type EvalGrader, type EvalResult, type GraderKind,
} from '../api/evals'
import { Dialog } from '../lib/dialog'
import { ds } from '../lib/designSystem'
import { ICON } from '../lib/icons'
import { EmptyState, Section } from '../lib/layout'
import { DSButton, DSChip } from '../lib/primitives'
import { configFromForm, emptyGraderForm, formFromConfig, KIND_INFO, NORMALIZERS, summarizeGrader, type GraderForm } from '../lib/graderForm'
import { DESIGN } from '../lib/designSystem'
import { Field, field } from './BuilderParts'
import { actionsRow, table, td, th } from './EvalDatasets'

const NAME_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/
const mono = { ...field.control, fontFamily: DESIGN.font.mono }

export function EvalGraders({ datasetId, writable, canGrade }: { datasetId: number; writable: boolean; canGrade: boolean }) {
  const [graders, setGraders] = useState<EvalGrader[] | null>(null)
  const [error, setError] = useState('')
  const [dialog, setDialog] = useState<{ edit: EvalGrader | null } | null>(null)
  const [removing, setRemoving] = useState<EvalGrader | null>(null)

  const load = useCallback(() => {
    let live = true
    listGraders(datasetId)
      .then((l) => { if (live) { setGraders(l); setError('') } })
      .catch((e: unknown) => { if (live) { setError(describeEvalError(e)); setGraders([]) } })
    return () => { live = false }
  }, [datasetId])
  useEffect(() => load(), [load])
  const changed = () => { setDialog(null); setRemoving(null); load() }

  return (
    <>
      <Section
        title={`Graders${graders ? ` (${graders.length})` : ''}`}
        actions={writable ? <DSButton size="sm" variant="primary" disabled={(graders?.length ?? 0) >= EVAL_LIMITS.maxGraders} onClick={() => setDialog({ edit: null })}>Add a grader</DSButton> : undefined}
      >
        <p style={{ margin: '0 0 8px', fontSize: 12, color: ds.textSecondary }}>
          A grader is a check on an agent's answer. No model is used: they cost nothing. A case passes when every required
          grader passes and the weighted average of the others is at least half.
        </p>
        {error && <div role="alert" style={{ color: ds.error, fontSize: 13, marginBottom: 8 }}>{error}</div>}
        {!graders ? (
          <div role="status" style={{ color: ds.textMuted, fontSize: 13 }}>Loading graders…</div>
        ) : graders.length === 0 ? (
          !error && (
            <EmptyState icon={ICON.check} title="No graders yet">
              {writable ? 'Add a grader to say what a good answer looks like, for example that it contains a word.' : 'A builder can add graders.'}
            </EmptyState>
          )
        ) : (
          <table style={table}>
            <thead>
              <tr>
                <th style={th}>Name</th><th style={th}>Check</th><th style={th}>Weight</th><th style={th}>Required</th>
                {writable && <th style={th}><span style={{ position: 'absolute', left: -9999 }}>Actions</span></th>}
              </tr>
            </thead>
            <tbody>
              {graders.map((g) => (
                <tr key={g.id}>
                  <td style={td}>{g.name}</td>
                  <td style={td}><strong>{KIND_INFO[g.kind]?.label ?? g.kind}</strong><div style={{ fontSize: 12, color: ds.textSecondary }}>{summarizeGrader(g.kind, g.config)}</div></td>
                  <td style={td}>{g.weight}</td>
                  <td style={td}>{g.required ? <DSChip size="sm" variant="warning">Required</DSChip> : 'No'}</td>
                  {writable && (
                    <td style={{ ...td, whiteSpace: 'nowrap' }}>
                      <DSButton size="sm" aria-label={`Edit grader ${g.name}`} onClick={() => setDialog({ edit: g })}>Edit</DSButton>{' '}
                      <DSButton size="sm" variant="danger" aria-label={`Delete grader ${g.name}`} onClick={() => setRemoving(g)}>Delete</DSButton>
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {dialog && <GraderDialog datasetId={datasetId} edit={dialog.edit} onClose={() => setDialog(null)} onSaved={changed} />}
        {removing && <RemoveGraderDialog datasetId={datasetId} grader={removing} onClose={() => setRemoving(null)} onRemoved={changed} />}
      </Section>
      <TryGrader datasetId={datasetId} canGrade={canGrade} graderCount={graders?.length ?? 0} />
    </>
  )
}

function GraderDialog({ datasetId, edit, onClose, onSaved }: { datasetId: number; edit: EvalGrader | null; onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState(edit?.name ?? '')
  const [kind, setKind] = useState<GraderKind>(edit?.kind ?? 'contains')
  const [form, setForm] = useState<GraderForm>(edit ? formFromConfig(edit.kind, edit.config) : emptyGraderForm())
  const [weight, setWeight] = useState(String(edit?.weight ?? 1))
  const [required, setRequired] = useState(edit?.required ?? false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const patch = (p: Partial<GraderForm>) => setForm((f) => ({ ...f, ...p }))

  const w = Number(weight)
  const nameProblem = name === '' ? '' : name.length > 63 || !NAME_RE.test(name) ? 'Use lower-case letters, numbers and dashes only, up to 63 characters.' : ''
  const weightProblem = !(w > 0) || !Number.isFinite(w) ? 'The weight must be a number above 0.' : ''
  const cfg = configFromForm(kind, form)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (name === '' || nameProblem || weightProblem || !cfg.ok) return
    setBusy(true)
    setError('')
    try {
      await putGrader(datasetId, name, { kind, config: cfg.config, weight: w, required })
      onSaved()
    } catch (err) {
      setError(describeEvalError(err))
      setBusy(false)
    }
  }
  // A config problem shows once the person has started on the kind's fields.
  const touched = JSON.stringify(form) !== JSON.stringify(emptyGraderForm())

  return (
    <Dialog title={edit ? `Edit grader ${edit.name}` : 'Add a grader'} onClose={onClose} width={560}>
      <form onSubmit={submit} style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <Field label="Name" help="Unique in the dataset." error={nameProblem}>
          {(p) => <input {...p} style={field.control} value={name} disabled={!!edit} onChange={(e) => setName(e.target.value)} />}
        </Field>
        <Field label="Kind" help={KIND_INFO[kind].help}>
          {(p) => (
            <select {...p} style={field.control} value={kind} onChange={(e) => { setKind(e.target.value as GraderKind); setForm(emptyGraderForm()) }}>
              {GRADER_KINDS.map((k) => <option key={k} value={k}>{KIND_INFO[k].label}</option>)}
            </select>
          )}
        </Field>
        <KindFields kind={kind} form={form} patch={patch} />
        {!cfg.ok && touched && <p role="alert" style={field.error}>{cfg.error}</p>}
        <Field label="Weight" help="How much this grader counts in the average." error={weightProblem}>
          {(p) => <input {...p} type="number" step="any" style={field.control} value={weight} onChange={(e) => setWeight(e.target.value)} />}
        </Field>
        <label style={{ display: 'flex', gap: 8, fontSize: 13, alignItems: 'center' }}>
          <input type="checkbox" checked={required} onChange={(e) => setRequired(e.target.checked)} />
          Required: the case fails when this grader fails
        </label>
        {error && <p role="alert" style={field.error}>Could not save the grader: {error}</p>}
        <div style={actionsRow}>
          <DSButton type="button" onClick={onClose}>Cancel</DSButton>
          <DSButton type="submit" variant="primary" disabled={busy || name === '' || !!nameProblem || !!weightProblem || !cfg.ok}>Save grader</DSButton>
        </div>
      </form>
    </Dialog>
  )
}

function KindFields({ kind, form, patch }: { kind: GraderKind; form: GraderForm; patch: (p: Partial<GraderForm>) => void }) {
  switch (kind) {
    case 'exact':
      return (
        <>
          <Field label="Value (optional)" help="Leave empty to compare with the expected answer of each case.">
            {(p) => <input {...p} style={field.control} value={form.value} onChange={(e) => patch({ value: e.target.value })} />}
          </Field>
          <fieldset style={{ border: 'none', padding: 0, margin: 0, display: 'flex', flexDirection: 'column', gap: 4 }}>
            <legend style={field.label}>Clean up both sides first</legend>
            {NORMALIZERS.map((n) => (
              <label key={n.value} style={{ display: 'flex', gap: 8, fontSize: 13 }}>
                <input
                  type="checkbox"
                  checked={form.normalize.includes(n.value)}
                  onChange={(e) => patch({ normalize: e.target.checked ? [...form.normalize, n.value] : form.normalize.filter((x) => x !== n.value) })}
                />
                {n.label}
              </label>
            ))}
          </fieldset>
        </>
      )
    case 'contains':
    case 'not_contains':
      return (
        <>
          <Field label="Text">
            {(p) => <input {...p} style={field.control} value={form.value} onChange={(e) => patch({ value: e.target.value })} />}
          </Field>
          <label style={{ display: 'flex', gap: 8, fontSize: 13 }}>
            <input type="checkbox" checked={form.caseInsensitive} onChange={(e) => patch({ caseInsensitive: e.target.checked })} />
            Ignore upper and lower case
          </label>
        </>
      )
    case 'regex':
      return (
        <>
          <Field label="Pattern" help="RE2 syntax: no lookahead and no backreferences.">
            {(p) => <input {...p} style={mono} value={form.pattern} onChange={(e) => patch({ pattern: e.target.value })} />}
          </Field>
          <label style={{ display: 'flex', gap: 8, fontSize: 13 }}>
            <input type="checkbox" checked={form.mustMatch} onChange={(e) => patch({ mustMatch: e.target.checked })} />
            The output must match (clear it to require that it does not)
          </label>
        </>
      )
    case 'json_schema':
      return (
        <Field label="JSON schema">
          {(p) => <textarea {...p} rows={8} style={mono} value={form.schema} onChange={(e) => patch({ schema: e.target.value })} />}
        </Field>
      )
    case 'json_field_equals':
      return (
        <>
          <Field label="Field path" help="For example status, or customer.name.">
            {(p) => <input {...p} style={mono} value={form.path} onChange={(e) => patch({ path: e.target.value })} />}
          </Field>
          <Field label="Expected value" help={'As JSON: "open" with quotes for text, or 42, true, null.'}>
            {(p) => <input {...p} style={mono} value={form.jsonValue} onChange={(e) => patch({ jsonValue: e.target.value })} />}
          </Field>
        </>
      )
    case 'tool_called':
    case 'tool_not_called':
      return (
        <Field label="Tool name">
          {(p) => <input {...p} style={field.control} value={form.tool} onChange={(e) => patch({ tool: e.target.value })} />}
        </Field>
      )
    case 'max_cost':
      return (
        <Field label="Most the run may cost (US dollars)">
          {(p) => <input {...p} type="number" step="any" style={field.control} value={form.usd} onChange={(e) => patch({ usd: e.target.value })} />}
        </Field>
      )
    case 'max_latency':
      return (
        <Field label="Longest the run may take (milliseconds)">
          {(p) => <input {...p} type="number" step="1" style={field.control} value={form.ms} onChange={(e) => patch({ ms: e.target.value })} />}
        </Field>
      )
  }
}

function RemoveGraderDialog({ datasetId, grader, onClose, onRemoved }: { datasetId: number; grader: EvalGrader; onClose: () => void; onRemoved: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const run = async () => {
    setBusy(true)
    setError('')
    try { await deleteGrader(datasetId, grader.name); onRemoved() } catch (e) { setError(describeEvalError(e)); setBusy(false) }
  }
  return (
    <Dialog title={`Delete grader ${grader.name}?`} onClose={onClose}>
      <p style={{ margin: 0, fontSize: 13 }}>This also deletes the grades it gave in kept results.</p>
      {error && <p role="alert" style={field.error}>Could not delete: {error}</p>}
      <div style={actionsRow}>
        <DSButton onClick={onClose}>Cancel</DSButton>
        <DSButton variant="danger" disabled={busy} onClick={run}>Delete grader</DSButton>
      </div>
    </Dialog>
  )
}

/** Grades pasted output with every grader of the dataset, to see how they behave. */
function TryGrader({ datasetId, canGrade, graderCount }: { datasetId: number; canGrade: boolean; graderCount: number }) {
  const [output, setOutput] = useState('')
  const [reference, setReference] = useState('')
  const [tools, setTools] = useState('')
  const [cost, setCost] = useState('')
  const [latency, setLatency] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [result, setResult] = useState<EvalResult | null>(null)

  const run = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      const r = await gradeOutput(datasetId, {
        output,
        ...(reference !== '' ? { reference } : {}),
        toolCalls: tools.split(',').map((t) => t.trim()).filter(Boolean),
        costUsd: cost === '' ? 0 : Number(cost),
        latencyMs: latency === '' ? 0 : Math.round(Number(latency)),
      })
      setResult(r)
    } catch (err) {
      setResult(null)
      setError(describeEvalError(err))
    } finally { setBusy(false) }
  }
  const numBad = (v: string) => v !== '' && !(Number(v) >= 0)
  const tooBig = new TextEncoder().encode(output).length > EVAL_LIMITS.maxOutputBytes

  return (
    <Section title="Try the graders">
      <form onSubmit={run} style={{ display: 'flex', flexDirection: 'column', gap: 10, maxWidth: 640 }}>
        <p style={{ margin: 0, fontSize: 12, color: ds.textSecondary }}>
          Paste an answer to see what each grader says. All graders of the dataset run, and the result is kept with the dataset
          (everyone in your organisation can read it), so paste only text you would keep.
        </p>
        <Field label="Agent output" error={tooBig ? `The output is over ${EVAL_LIMITS.maxOutputBytes} bytes.` : ''}>
          {(p) => <textarea {...p} rows={4} style={field.control} value={output} onChange={(e) => setOutput(e.target.value)} />}
        </Field>
        <Field label="Expected answer (optional)" help="Used by graders that compare with the expected answer.">
          {(p) => <input {...p} style={field.control} value={reference} onChange={(e) => setReference(e.target.value)} />}
        </Field>
        <Field label="Tools called (optional)" help="Names separated by commas.">
          {(p) => <input {...p} style={field.control} value={tools} onChange={(e) => setTools(e.target.value)} />}
        </Field>
        <div style={{ display: 'flex', gap: 12 }}>
          <Field label="Cost in US dollars (optional)" error={numBad(cost) ? 'Enter a number, 0 or more.' : ''}>
            {(p) => <input {...p} type="number" step="any" style={field.control} value={cost} onChange={(e) => setCost(e.target.value)} />}
          </Field>
          <Field label="Time in milliseconds (optional)" error={numBad(latency) ? 'Enter a number, 0 or more.' : ''}>
            {(p) => <input {...p} type="number" step="1" style={field.control} value={latency} onChange={(e) => setLatency(e.target.value)} />}
          </Field>
        </div>
        {!canGrade && <p style={field.help}>Grading needs the runner role.</p>}
        {graderCount === 0 && <p style={field.help}>Add a grader first: there is nothing to try yet.</p>}
        <div>
          <DSButton type="submit" variant="primary" disabled={busy || !canGrade || graderCount === 0 || output === '' || tooBig || numBad(cost) || numBad(latency)}>
            {busy ? 'Grading…' : 'Grade this output'}
          </DSButton>
        </div>
        {error && <p role="alert" style={field.error}>Could not grade: {error}</p>}
      </form>
      {result && (
        <div style={{ marginTop: 12 }} aria-label="Grading result">
          <p style={{ margin: '0 0 8px', fontSize: 14 }}>
            Overall: <DSChip variant={result.casePass ? 'success' : 'error'}>{result.casePass ? 'Passes' : 'Fails'}</DSChip>
          </p>
          <table style={table}>
            <thead><tr><th style={th}>Grader</th><th style={th}>Result</th><th style={th}>Score</th><th style={th}>Detail</th></tr></thead>
            <tbody>
              {result.grades.map((g) => (
                <tr key={g.graderId}>
                  <td style={td}>{g.grader}</td>
                  <td style={td}>
                    {g.notGraded
                      ? <DSChip size="sm" variant="warning">Not graded</DSChip>
                      : <DSChip size="sm" variant={g.pass ? 'success' : 'error'}>{g.pass ? 'Pass' : 'Fail'}</DSChip>}
                  </td>
                  <td style={td}>{g.score}</td>
                  <td style={td}>{g.detail}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Section>
  )
}
