/** Evaluate home: the evaluation datasets of the workspace, with create and delete. */
import { useCallback, useEffect, useState, type CSSProperties } from 'react'
import { Link } from 'react-router'
import { createDataset, deleteDataset, describeEvalError, listDatasets, type EvalDataset } from '../api/evals'
import { nameProblem } from '../lib/agentForm'
import { canWriteEvals, useRoles } from '../lib/access'
import { Dialog } from '../lib/dialog'
import { ds } from '../lib/designSystem'
import { ICON } from '../lib/icons'
import { EmptyState, Page, PageHeader } from '../lib/layout'
import { useNamespace } from '../lib/namespace'
import { DSButton } from '../lib/primitives'
import { datasetPath } from '../lib/routes'
import { Field, field, useWorkspaces } from './BuilderParts'

export function EvalDatasets() {
  const { namespace } = useNamespace()
  const roles = useRoles()
  const writable = canWriteEvals(roles)
  const [datasets, setDatasets] = useState<EvalDataset[] | null>(null)
  const [error, setError] = useState('')
  const [creating, setCreating] = useState(false)
  const [deleting, setDeleting] = useState<EvalDataset | null>(null)

  const load = useCallback(() => {
    let live = true
    listDatasets({ namespace: namespace || undefined })
      .then((list) => { if (live) { setDatasets(list); setError('') } })
      .catch((e: unknown) => { if (live) { setError(describeEvalError(e)); setDatasets([]) } })
    return () => { live = false }
  }, [namespace])
  useEffect(() => load(), [load])

  const actions = writable ? <DSButton variant="primary" onClick={() => setCreating(true)}>New dataset</DSButton> : undefined

  return (
    <Page>
      <PageHeader
        title="Evaluate"
        description="Datasets are sets of example cases, with checks that say whether an agent's answer is good."
        actions={actions}
      />
      <div style={{ padding: '0 24px 24px' }}>
        {error && <div role="alert" style={{ color: ds.error, fontSize: 13, marginBottom: 12 }}>Could not load datasets: {error}</div>}
        {!datasets ? (
          <div role="status" style={{ color: ds.textMuted, fontSize: 13 }}>Loading datasets…</div>
        ) : datasets.length === 0 ? (
          !error && (
            <EmptyState icon={ICON.check} title="No datasets yet">
              {writable
                ? 'Create a dataset, add a few example questions with the answers you expect, then add checks.'
                : 'No datasets have been created in this workspace. A builder can create one.'}
            </EmptyState>
          )
        ) : (
          <table style={table}>
            <thead>
              <tr>
                <th style={th}>Dataset</th><th style={th}>Workspace</th><th style={th}>Agent</th>
                <th style={th}>Cases</th><th style={th}>Version</th>
                {writable && <th style={th}><span style={{ position: 'absolute', left: -9999 }}>Actions</span></th>}
              </tr>
            </thead>
            <tbody>
              {datasets.map((d) => (
                <tr key={d.id}>
                  <td style={td}>
                    <Link to={datasetPath(d.id)} style={{ color: ds.accent, fontWeight: 600, textDecoration: 'none' }}>{d.name}</Link>
                    {d.description && <div style={{ fontSize: 12, color: ds.textSecondary }}>{d.description}</div>}
                  </td>
                  <td style={td}>{d.namespace}</td>
                  <td style={td}>{d.agent || '-'}</td>
                  <td style={td}>{d.caseCount}</td>
                  <td style={td}>{d.version}</td>
                  {writable && (
                    <td style={td}>
                      <DSButton size="sm" variant="danger" aria-label={`Delete ${d.name}`} onClick={() => setDeleting(d)}>Delete</DSButton>
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {creating && <CreateDatasetDialog defaultNamespace={namespace} onClose={() => setCreating(false)} onCreated={() => { setCreating(false); load() }} />}
      {deleting && <DeleteDatasetDialog dataset={deleting} onClose={() => setDeleting(null)} onDeleted={() => { setDeleting(null); load() }} />}
    </Page>
  )
}

function CreateDatasetDialog({ defaultNamespace, onClose, onCreated }: { defaultNamespace: string; onClose: () => void; onCreated: () => void }) {
  const { workspaces, primary } = useWorkspaces()
  const [name, setName] = useState('')
  const [ns, setNs] = useState(defaultNamespace)
  const [description, setDescription] = useState('')
  const [agent, setAgent] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const problem = name === '' ? '' : nameProblem(name)
  const effectiveNs = ns || primary

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (name === '' || problem) return
    setBusy(true)
    setError('')
    try {
      await createDataset({ name, namespace: effectiveNs || undefined, description: description.trim() || undefined, agent: agent.trim() || undefined })
      onCreated()
    } catch (err) {
      setError(describeEvalError(err))
      setBusy(false)
    }
  }

  return (
    <Dialog title="New dataset" onClose={onClose}>
      <form onSubmit={submit} style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <Field label="Name" help="Lower case letters, digits and dashes." error={problem}>
          {(p) => <input {...p} style={field.control} value={name} onChange={(e) => setName(e.target.value)} />}
        </Field>
        <Field label="Workspace">
          {(p) => workspaces.length > 0 ? (
            <select {...p} style={field.control} value={effectiveNs} onChange={(e) => setNs(e.target.value)}>
              {workspaces.map((w) => <option key={w.namespace} value={w.namespace}>{w.namespace}</option>)}
            </select>
          ) : (
            <input {...p} style={field.control} value={ns} placeholder="Your first workspace" onChange={(e) => setNs(e.target.value)} />
          )}
        </Field>
        <Field label="Description (optional)">
          {(p) => <textarea {...p} rows={2} style={field.control} value={description} onChange={(e) => setDescription(e.target.value)} />}
        </Field>
        <Field label="Agent (optional)" help="The name of the agent this dataset is meant for.">
          {(p) => <input {...p} style={field.control} value={agent} onChange={(e) => setAgent(e.target.value)} />}
        </Field>
        {error && <p role="alert" style={field.error}>Could not create the dataset: {error}</p>}
        <div style={actionsRow}>
          <DSButton type="button" onClick={onClose}>Cancel</DSButton>
          <DSButton type="submit" variant="primary" disabled={busy || name === '' || !!problem}>Create dataset</DSButton>
        </div>
      </form>
    </Dialog>
  )
}

function DeleteDatasetDialog({ dataset, onClose, onDeleted }: { dataset: EvalDataset; onClose: () => void; onDeleted: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const run = async () => {
    setBusy(true)
    setError('')
    try {
      await deleteDataset(dataset.id)
      onDeleted()
    } catch (e) {
      setError(describeEvalError(e))
      setBusy(false)
    }
  }
  return (
    <Dialog title={`Delete ${dataset.name}?`} onClose={onClose}>
      <p style={{ margin: 0, fontSize: 13 }}>
        This deletes the dataset with its {dataset.caseCount} {dataset.caseCount === 1 ? 'case' : 'cases'}, its checks and its kept results. It cannot be undone.
      </p>
      {error && <p role="alert" style={field.error}>Could not delete: {error}</p>}
      <div style={actionsRow}>
        <DSButton onClick={onClose}>Cancel</DSButton>
        <DSButton variant="danger" disabled={busy} onClick={run}>Delete dataset</DSButton>
      </div>
    </Dialog>
  )
}

export const actionsRow: CSSProperties = { display: 'flex', justifyContent: 'flex-end', gap: 8 }
export const table: CSSProperties = { width: '100%', borderCollapse: 'collapse', fontSize: 13 }
export const th: CSSProperties = { textAlign: 'left', padding: '8px 10px', fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.04em', color: ds.textSecondary, borderBottom: '1px solid var(--ds-border)' }
export const td: CSSProperties = { padding: '10px', borderBottom: '1px solid var(--ds-border)', verticalAlign: 'top', overflowWrap: 'anywhere' }
