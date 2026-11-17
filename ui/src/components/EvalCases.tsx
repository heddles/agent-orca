/** The cases of one dataset: a table, with add, edit and delete for builders. */
import { useCallback, useEffect, useState } from 'react'
import {
  addCase, deleteCase, describeEvalError, EVAL_LIMITS, listCases, updateCase,
  type EvalCase, type ReferenceKind,
} from '../api/evals'
import { Dialog } from '../lib/dialog'
import { ds } from '../lib/designSystem'
import { ICON } from '../lib/icons'
import { EmptyState, Section } from '../lib/layout'
import { DSButton, DSChip } from '../lib/primitives'
import { Field, field } from './BuilderParts'
import { actionsRow, table, td, th } from './EvalDatasets'

const KIND_LABEL: Record<ReferenceKind, string> = { none: 'None', exact: 'Exact answer', example: 'Example answer', rubric: 'Rubric' }
const byteLength = (s: string) => new TextEncoder().encode(s).length

export const parseTags = (text: string): string[] => text.split(',').map((t) => t.trim()).filter(Boolean)

export function EvalCases({ datasetId, caseCount, writable, onChanged }: {
  datasetId: number
  caseCount: number
  writable: boolean
  /** Called after any change, so the dataset's counts and version reload. */
  onChanged: () => void
}) {
  const [cases, setCases] = useState<EvalCase[] | null>(null)
  const [next, setNext] = useState('')
  const [error, setError] = useState('')
  const [dialog, setDialog] = useState<{ edit: EvalCase | null } | null>(null)
  const [removing, setRemoving] = useState<EvalCase | null>(null)

  const load = useCallback(() => {
    let live = true
    listCases(datasetId, { limit: 50 })
      .then((p) => { if (live) { setCases(p.cases); setNext(p.nextCursor); setError('') } })
      .catch((e: unknown) => { if (live) { setError(describeEvalError(e)); setCases([]) } })
    return () => { live = false }
  }, [datasetId])
  useEffect(() => load(), [load])

  const more = async () => {
    try {
      const p = await listCases(datasetId, { limit: 50, cursor: next })
      setCases((cur) => [...(cur ?? []), ...p.cases])
      setNext(p.nextCursor)
    } catch (e) { setError(describeEvalError(e)) }
  }
  const changed = () => { setDialog(null); setRemoving(null); load(); onChanged() }

  return (
    <Section
      title={`Cases (${caseCount})`}
      actions={writable ? <DSButton size="sm" variant="primary" onClick={() => setDialog({ edit: null })}>Add a case</DSButton> : undefined}
    >
      <p style={{ margin: '0 0 8px', fontSize: 12, color: ds.textSecondary }}>
        Cases are readable by everyone in your organisation. Do not put secrets or personal data in them. Text typed here is
        not redacted; only cases copied from a run are. Each input and each expected answer can be up to
        {' '}{EVAL_LIMITS.maxCaseBytes.toLocaleString()} bytes, and a dataset holds up to {EVAL_LIMITS.maxCasesPerDataset.toLocaleString()} cases.
      </p>
      {error && <div role="alert" style={{ color: ds.error, fontSize: 13, marginBottom: 8 }}>{error}</div>}
      {!cases ? (
        <div role="status" style={{ color: ds.textMuted, fontSize: 13 }}>Loading cases…</div>
      ) : cases.length === 0 ? (
        !error && (
          <EmptyState icon={ICON.check} title="No cases yet">
            {writable ? 'Add a case: something a person would ask, and the answer you expect.' : 'A builder can add cases.'}
          </EmptyState>
        )
      ) : (
        <table style={table}>
          <thead>
            <tr>
              <th style={th}>Input</th><th style={th}>Expected</th><th style={th}>Tags</th><th style={th}>Weight</th>
              {writable && <th style={th}><span style={{ position: 'absolute', left: -9999 }}>Actions</span></th>}
            </tr>
          </thead>
          <tbody>
            {cases.map((c) => (
              <tr key={c.id}>
                <td style={td}>{c.input}{c.redacted && <> <DSChip size="sm" variant="info">Redacted</DSChip></>}</td>
                <td style={td}>
                  {c.reference ? <>{c.reference} <DSChip size="sm" variant="neutral">{KIND_LABEL[c.referenceKind] ?? c.referenceKind}</DSChip></> : '-'}
                </td>
                <td style={td}>{c.tags.length ? c.tags.map((t) => <DSChip key={t} size="sm" variant="neutral">{t}</DSChip>) : '-'}</td>
                <td style={td}>{c.weight}</td>
                {writable && (
                  <td style={{ ...td, whiteSpace: 'nowrap' }}>
                    <DSButton size="sm" aria-label={`Edit case ${c.id}`} onClick={() => setDialog({ edit: c })}>Edit</DSButton>{' '}
                    <DSButton size="sm" variant="danger" aria-label={`Delete case ${c.id}`} onClick={() => setRemoving(c)}>Delete</DSButton>
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {next && <div style={{ marginTop: 8 }}><DSButton size="sm" onClick={more}>Show more cases</DSButton></div>}
      {dialog && <CaseDialog datasetId={datasetId} edit={dialog.edit} onClose={() => setDialog(null)} onSaved={changed} />}
      {removing && <RemoveCaseDialog item={removing} onClose={() => setRemoving(null)} onRemoved={changed} />}
    </Section>
  )
}

function CaseDialog({ datasetId, edit, onClose, onSaved }: { datasetId: number; edit: EvalCase | null; onClose: () => void; onSaved: () => void }) {
  const [input, setInput] = useState(edit?.input ?? '')
  const [reference, setReference] = useState(edit?.reference ?? '')
  const [kind, setKind] = useState<ReferenceKind>(edit?.referenceKind && edit.referenceKind !== 'none' ? edit.referenceKind : 'exact')
  const [tags, setTags] = useState(edit?.tags.join(', ') ?? '')
  const [weight, setWeight] = useState(String(edit?.weight ?? 1))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const w = Number(weight)
  const problems = {
    input: input.trim() === '' ? 'Enter the input.' : byteLength(input) > EVAL_LIMITS.maxCaseBytes ? `The input is over ${EVAL_LIMITS.maxCaseBytes} bytes.` : '',
    reference: byteLength(reference) > EVAL_LIMITS.maxCaseBytes ? `The expected answer is over ${EVAL_LIMITS.maxCaseBytes} bytes.` : '',
    tags: parseTags(tags).length > EVAL_LIMITS.maxTags ? `Use at most ${EVAL_LIMITS.maxTags} tags.` : '',
    weight: !(w > 0) || !Number.isFinite(w) ? 'The weight must be a number above 0.' : '',
  }
  const invalid = Object.values(problems).some(Boolean)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (invalid) return
    setBusy(true)
    setError('')
    const refKind: ReferenceKind = reference === '' ? 'none' : kind
    try {
      if (edit) {
        await updateCase(edit.id, { input, reference, referenceKind: refKind, tags: parseTags(tags), weight: w })
      } else {
        await addCase(datasetId, {
          input, tags: parseTags(tags), weight: w,
          ...(reference !== '' ? { reference, referenceKind: refKind } : {}),
        })
      }
      onSaved()
    } catch (err) {
      setError(describeEvalError(err))
      setBusy(false)
    }
  }

  return (
    <Dialog title={edit ? `Edit case ${edit.id}` : 'Add a case'} onClose={onClose} width={560}>
      <form onSubmit={submit} style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <Field label="Input" help="What is sent to the agent." error={input === '' ? '' : problems.input}>
          {(p) => <textarea {...p} rows={4} style={field.control} value={input} onChange={(e) => setInput(e.target.value)} />}
        </Field>
        <Field label="Expected answer (optional)" help="Checks such as Exact match compare the agent's answer with this." error={problems.reference}>
          {(p) => <textarea {...p} rows={3} style={field.control} value={reference} onChange={(e) => setReference(e.target.value)} />}
        </Field>
        {reference !== '' && (
          <Field label="What the expected answer is">
            {(p) => (
              <select {...p} style={field.control} value={kind} onChange={(e) => setKind(e.target.value as ReferenceKind)}>
                <option value="exact">The exact answer</option>
                <option value="example">An example of a good answer</option>
                <option value="rubric">A rubric to judge by</option>
              </select>
            )}
          </Field>
        )}
        <Field label="Tags (optional)" help="Separate with commas." error={problems.tags}>
          {(p) => <input {...p} style={field.control} value={tags} onChange={(e) => setTags(e.target.value)} />}
        </Field>
        <Field label="Weight" error={problems.weight}>
          {(p) => <input {...p} type="number" step="any" style={field.control} value={weight} onChange={(e) => setWeight(e.target.value)} />}
        </Field>
        <p style={field.help}>Text typed here is stored as written. It is not redacted, and everyone in your organisation can read it.</p>
        {error && <p role="alert" style={field.error}>Could not save the case: {error}</p>}
        <div style={actionsRow}>
          <DSButton type="button" onClick={onClose}>Cancel</DSButton>
          <DSButton type="submit" variant="primary" disabled={busy || invalid}>{edit ? 'Save case' : 'Add case'}</DSButton>
        </div>
      </form>
    </Dialog>
  )
}

function RemoveCaseDialog({ item, onClose, onRemoved }: { item: EvalCase; onClose: () => void; onRemoved: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const run = async () => {
    setBusy(true)
    setError('')
    try { await deleteCase(item.id); onRemoved() } catch (e) { setError(describeEvalError(e)); setBusy(false) }
  }
  return (
    <Dialog title={`Delete case ${item.id}?`} onClose={onClose}>
      <p style={{ margin: 0, fontSize: 13 }}>The case leaves the dataset, and the dataset moves to a new version.</p>
      {error && <p role="alert" style={field.error}>Could not delete: {error}</p>}
      <div style={actionsRow}>
        <DSButton onClick={onClose}>Cancel</DSButton>
        <DSButton variant="danger" disabled={busy} onClick={run}>Delete case</DSButton>
      </div>
    </Dialog>
  )
}
