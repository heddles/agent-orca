/**
 * "Add to dataset" for a run the person rated thumbs-up: an explicit, two-step
 * copy of the run into an evaluation dataset (preview what will be stored, then
 * add). It is never automatic, and the run's own output becomes the reference
 * only when the person ticks the box (docs/proposals/evaluations.md, "Thumbs
 * bridge"). Shown to builders; the API enforces the role.
 */
import { useEffect, useState } from 'react'
import {
  captureRun, describeEvalError, listDatasets, type CaptureResult, type EvalDataset, type ReferenceKind,
} from '../api/evals'
import { canWriteEvals, useRoles } from '../lib/access'
import { Dialog } from '../lib/dialog'
import { ds } from '../lib/designSystem'
import { DSButton } from '../lib/primitives'
import { Field, field } from './BuilderParts'
import { actionsRow } from './EvalDatasets'

const parseTags = (text: string): string[] => text.split(',').map((t) => t.trim()).filter(Boolean)

export function AddToDataset({ namespace, runName }: { namespace: string; runName: string }) {
  const roles = useRoles()
  const [open, setOpen] = useState(false)
  const [added, setAdded] = useState<{ dataset: string; version?: number } | null>(null)
  if (!canWriteEvals(roles)) return null
  return (
    <span style={{ marginLeft: 12 }}>
      <button
        type="button"
        onClick={() => setOpen(true)}
        style={{ background: 'none', border: 'none', color: ds.accent, cursor: 'pointer', fontSize: 12, padding: 0 }}
      >
        Add to dataset…
      </button>
      {added && <span role="status" style={{ marginLeft: 8, fontSize: 11, color: ds.textSecondary }}>Added to {added.dataset}.</span>}
      {open && (
        <AddDialog
          namespace={namespace}
          runName={runName}
          onClose={() => setOpen(false)}
          onAdded={(dataset, version) => { setOpen(false); setAdded({ dataset, version }) }}
        />
      )}
    </span>
  )
}

function AddDialog({ namespace, runName, onClose, onAdded }: {
  namespace: string
  runName: string
  onClose: () => void
  onAdded: (dataset: string, version?: number) => void
}) {
  const [datasets, setDatasets] = useState<EvalDataset[] | null>(null)
  const [datasetId, setDatasetId] = useState(0)
  const [reference, setReference] = useState('')
  const [kind, setKind] = useState<ReferenceKind>('exact')
  const [useOutput, setUseOutput] = useState(false)
  const [tags, setTags] = useState('')
  const [preview, setPreview] = useState<CaptureResult | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    let live = true
    listDatasets({ namespace })
      .then((l) => { if (live) { setDatasets(l); if (l.length > 0) setDatasetId(l[0].id) } })
      .catch((e: unknown) => { if (live) { setError(describeEvalError(e)); setDatasets([]) } })
    return () => { live = false }
  }, [namespace])

  const request = (previewOnly: boolean) => ({
    datasetId,
    tags: parseTags(tags),
    preview: previewOnly,
    ...(useOutput ? { useOutput: true } : reference.trim() !== '' ? { reference, referenceKind: kind } : {}),
  })

  const run = async (previewOnly: boolean) => {
    setBusy(true)
    setError('')
    try {
      const res = await captureRun(namespace, runName, request(previewOnly))
      if (previewOnly) { setPreview(res); setBusy(false); return }
      onAdded(datasets?.find((d) => d.id === datasetId)?.name ?? 'the dataset', res.datasetVersion)
    } catch (e) {
      setError(describeEvalError(e))
      setBusy(false)
    }
  }

  return (
    <Dialog title="Add this run to a dataset" onClose={onClose} width={560}>
      <form onSubmit={(e) => { e.preventDefault(); void run(preview === null) }} style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        {datasets !== null && datasets.length === 0 && !error && (
          <p style={{ margin: 0, fontSize: 13 }}>There is no dataset in this workspace yet. Create one under Evaluate first.</p>
        )}
        <Field label="Dataset">
          {(p) => (
            <select {...p} style={field.control} value={datasetId} disabled={preview !== null} onChange={(e) => setDatasetId(Number(e.target.value))}>
              {(datasets ?? []).map((d) => <option key={d.id} value={d.id}>{d.name}</option>)}
            </select>
          )}
        </Field>
        <Field label="Expected answer (optional)" help="What a good answer looks like. Leave empty for a case with no reference.">
          {(p) => <textarea {...p} rows={3} style={field.control} value={reference} disabled={useOutput || preview !== null} onChange={(e) => setReference(e.target.value)} />}
        </Field>
        {reference.trim() !== '' && !useOutput && (
          <Field label="What the expected answer is">
            {(p) => (
              <select {...p} style={field.control} value={kind} disabled={preview !== null} onChange={(e) => setKind(e.target.value as ReferenceKind)}>
                <option value="exact">The exact answer</option>
                <option value="example">An example of a good answer</option>
                <option value="rubric">A rubric to judge by</option>
              </select>
            )}
          </Field>
        )}
        <label style={{ display: 'flex', gap: 8, fontSize: 12, alignItems: 'flex-start' }}>
          <input type="checkbox" checked={useOutput} disabled={preview !== null} onChange={(e) => setUseOutput(e.target.checked)} />
          <span>Use this run&apos;s output as an example answer. Only tick this if you checked the output is right; a thumbs-up on the model choice does not say so.</span>
        </label>
        <Field label="Tags (optional)" help="Separate with commas.">
          {(p) => <input {...p} style={field.control} value={tags} disabled={preview !== null} onChange={(e) => setTags(e.target.value)} />}
        </Field>
        {preview && (
          <div role="region" aria-label="What will be stored" style={{ fontSize: 12, display: 'flex', flexDirection: 'column', gap: 6 }}>
            <strong>This will be stored (secrets and personal data are redacted on a best effort basis):</strong>
            <pre style={{ margin: 0, whiteSpace: 'pre-wrap', background: 'var(--ds-surface-2, transparent)', border: '1px solid var(--ds-border)', padding: 8 }}>{preview.case.input}</pre>
            {preview.case.reference && <pre style={{ margin: 0, whiteSpace: 'pre-wrap', border: '1px solid var(--ds-border)', padding: 8 }}>{preview.case.reference}</pre>}
            {Object.entries(preview.redaction ?? {}).filter(([, r]) => r.replacements > 0).map(([f, r]) => (
              <span key={f} style={{ color: ds.textSecondary }}>{r.replacements} redacted in the {f}.</span>
            ))}
          </div>
        )}
        {error && <p role="alert" style={field.error}>Could not add the run: {error}</p>}
        <div style={actionsRow}>
          <DSButton type="button" onClick={onClose}>Cancel</DSButton>
          {preview && <DSButton type="button" disabled={busy} onClick={() => setPreview(null)}>Back</DSButton>}
          <DSButton type="submit" variant="primary" disabled={busy || datasetId === 0}>{preview ? 'Add case' : 'Preview'}</DSButton>
        </div>
      </form>
    </Dialog>
  )
}
