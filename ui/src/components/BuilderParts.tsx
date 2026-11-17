/** Small pieces shared by the builder pages: a labelled field, styles, and the dialogs. */
import { useEffect, useId, useState, type CSSProperties, type ReactNode } from 'react'
import { Link, useNavigate } from 'react-router'
import { ApiError, createFromTemplate, type MissingRef, duplicateResource, exportResource } from '../api/builder'
import { getMe, type Me } from '../api/fleet'
import { Dialog } from '../lib/dialog'
import { nameProblem } from '../lib/agentForm'
import { DESIGN, ds } from '../lib/designSystem'
import { DSButton } from '../lib/primitives'
import { buildPath, ROUTES } from '../lib/routes'

export const field: Record<string, CSSProperties> = {
  wrap: { display: 'flex', flexDirection: 'column', gap: 4 },
  label: { fontSize: 12, fontWeight: 600, color: ds.textPrimary },
  help: { margin: 0, fontSize: 12, color: ds.textSecondary },
  error: { margin: 0, fontSize: 12, color: ds.error },
  control: {
    background: ds.surface, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md,
    padding: '8px 10px', color: ds.textPrimary, fontSize: 13, fontFamily: DESIGN.font.family,
    width: '100%', boxSizing: 'border-box',
  },
}

interface FieldProps {
  label: string
  help?: string
  error?: string
  children: (props: { id: string; 'aria-describedby'?: string; 'aria-invalid'?: true }) => ReactNode
}

/** A labelled field. The label, help text and error are tied to the control for screen readers. */
export function Field({ label, help, error, children }: FieldProps) {
  const id = useId()
  const describedBy = [help ? `${id}-help` : '', error ? `${id}-error` : ''].filter(Boolean).join(' ') || undefined
  return (
    <div style={field.wrap}>
      <label htmlFor={id} style={field.label}>{label}</label>
      {help && <p id={`${id}-help`} style={field.help}>{help}</p>}
      {children({ id, 'aria-describedby': describedBy, 'aria-invalid': error ? true : undefined })}
      {error && <p id={`${id}-error`} role="alert" style={field.error}>{error}</p>}
    </div>
  )
}

function messageOf(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

/** The workspaces the caller can create agents in, and the primary one. */
export function useWorkspaces(): { workspaces: Me['workspaces']; primary: string; loaded: boolean } {
  const [me, setMe] = useState<Me | null>(null)
  const [loaded, setLoaded] = useState(false)
  useEffect(() => {
    let live = true
    getMe().then((m) => { if (live) setMe(m) }).catch(() => {}).finally(() => { if (live) setLoaded(true) })
    return () => { live = false }
  }, [])
  const workspaces = me?.workspaces ?? []
  const primary = (workspaces.find((w) => w.primary) ?? workspaces[0])?.namespace ?? ''
  return { workspaces, primary, loaded }
}

/** A kind of reference in words a business user knows. */
function kindWords(kind: string): string {
  switch (kind) {
    case 'ModelSelector': return 'Model'
    case 'Tool': return 'Tool'
    case 'MCPServer': return 'Connected system'
    case 'KnowledgeBase': return 'Knowledge base'
    case 'GuardrailPolicy': return 'Safety policy'
    default: return kind
  }
}

/** Where an administrator adds one, where such a page exists. */
function settingsFor(kind: string): { to: string; label: string } | null {
  switch (kind) {
    case 'ModelSelector': return { to: ROUTES.models, label: 'Settings → Models' }
    case 'Tool':
    case 'MCPServer': return { to: ROUTES.connections, label: 'Settings → Connections' }
    case 'KnowledgeBase': return { to: ROUTES.knowledge, label: 'Settings → Knowledge' }
    default: return null
  }
}

// ── Use a template ───────────────────────────────────────────────────────────

/** Asks for a name and workspace, copies the template, and opens the copy in the builder. */
export function UseTemplateDialog({ template, onClose }: { template: { namespace: string; name: string }; onClose: () => void }) {
  const navigate = useNavigate()
  const { workspaces, primary } = useWorkspaces()
  const [name, setName] = useState(template.name)
  const [workspace, setWorkspace] = useState('')
  const [problem, setProblem] = useState('')
  const [failure, setFailure] = useState('')
  const [busy, setBusy] = useState(false)
  const [missing, setMissing] = useState<MissingRef[]>([])
  const [picks, setPicks] = useState<Record<string, string>>({})
  const chosen = workspace || primary
  const workspaceLabel = workspaces.find((w) => w.namespace === chosen)?.name || chosen || 'your workspace'
  const allPicked = missing.length > 0 && missing.every((m) => picks[m.replaceKey])
  const blocked = missing.length > 0 && !allPicked

  const changeWorkspace = (ns: string) => {
    setWorkspace(ns)
    setMissing([])
    setPicks({})
    setFailure('')
  }

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const p = nameProblem(name)
    setProblem(p)
    setFailure('')
    if (p || blocked) return
    setBusy(true)
    try {
      const used = await createFromTemplate(template.namespace, template.name, {
        name,
        ...(chosen ? { namespace: chosen } : {}),
        ...(Object.keys(picks).length > 0 ? { replace: picks } : {}),
      })
      const meta = used.agent.metadata
      navigate(buildPath(meta.namespace ?? chosen, meta.name ?? name))
    } catch (err) {
      if (err instanceof ApiError && err.status === 422 && err.missing && err.missing.length > 0) {
        setMissing(err.missing)
        setPicks((old) => Object.fromEntries(err.missing!.filter((m) => old[m.replaceKey] && m.alternatives.includes(old[m.replaceKey])).map((m) => [m.replaceKey, old[m.replaceKey]])))
        setFailure('')
      } else {
        setFailure(err instanceof ApiError && err.status === 409
          ? `An agent called ${name} already exists in this workspace. Choose another name.`
          : messageOf(err))
      }
      setBusy(false)
    }
  }

  return (
    <Dialog title="Use this template" onClose={onClose}>
      <form onSubmit={submit} noValidate style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <p style={field.help}>This makes your own copy of {template.name}. You can change it freely; the template stays as it is.</p>
        <Field label="Name for your agent" help="Lower-case letters, numbers and dashes." error={problem}>
          {(p) => <input {...p} style={field.control} value={name} onChange={(e) => setName(e.target.value)} />}
        </Field>
        {workspaces.length > 1 && (
          <Field label="Workspace">
            {(p) => (
              <select {...p} style={field.control} value={chosen} onChange={(e) => changeWorkspace(e.target.value)}>
                {workspaces.map((w) => <option key={w.namespace} value={w.namespace}>{w.name || w.namespace}</option>)}
              </select>
            )}
          </Field>
        )}
        {missing.length > 0 && (
          <div role="group" aria-label="Missing from this workspace" style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
            <p role="alert" style={field.error}>
              This template needs {missing.length === 1 ? `a ${kindWords(missing[0].kind).toLowerCase()}` : 'things'} that
              {missing.length === 1 ? " isn't" : " aren't"} set up in {workspaceLabel} yet. Choose what to use instead.
            </p>
            {missing.map((m) => (
              m.alternatives.length > 0 ? (
                <Field key={m.replaceKey} label={`${kindWords(m.kind)} to use instead of “${m.name}”`}>
                  {(p) => (
                    <select {...p} style={field.control} value={picks[m.replaceKey] ?? ''} onChange={(e) => setPicks((old) => ({ ...old, [m.replaceKey]: e.target.value }))}>
                      <option value="">Choose…</option>
                      {m.alternatives.map((a) => <option key={a} value={a}>{a}</option>)}
                    </select>
                  )}
                </Field>
              ) : (
                <p key={m.replaceKey} style={field.error}>
                  There is no {kindWords(m.kind).toLowerCase()} in {workspaceLabel} to use instead of “{m.name}”. An administrator needs to add one
                  {settingsFor(m.kind) && <>{' '}under <Link to={settingsFor(m.kind)!.to} style={{ color: ds.accent }}>{settingsFor(m.kind)!.label}</Link></>}.
                </p>
              )
            ))}
          </div>
        )}
        <div role="alert" style={field.error}>{failure}</div>
        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
          <DSButton type="button" variant="ghost" onClick={onClose}>Cancel</DSButton>
          <DSButton type="submit" variant="primary" disabled={busy || blocked}>
            {busy ? 'Making a copy…' : missing.length > 0 ? 'Try again' : 'Make my copy'}
          </DSButton>
        </div>
      </form>
    </Dialog>
  )
}

// ── Duplicate ────────────────────────────────────────────────────────────────

export function DuplicateDialog({ kind, namespace, name, onClose }: { kind: string; namespace: string; name: string; onClose: () => void }) {
  const navigate = useNavigate()
  const [newName, setNewName] = useState(`${name}-copy`)
  const [problem, setProblem] = useState('')
  const [failure, setFailure] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const p = nameProblem(newName)
    setProblem(p)
    setFailure('')
    if (p) return
    setBusy(true)
    try {
      const copy = await duplicateResource(kind, namespace, name, newName)
      navigate(buildPath(copy.metadata?.namespace ?? namespace, copy.metadata?.name ?? newName))
    } catch (err) {
      setFailure(err instanceof ApiError && err.status === 409 ? `An agent called ${newName} already exists. Choose another name.` : messageOf(err))
      setBusy(false)
    }
  }

  return (
    <Dialog title="Duplicate this agent" onClose={onClose}>
      <form onSubmit={submit} noValidate style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <p style={field.help}>The copy is yours to change in the builder. The original stays managed in Git.</p>
        <Field label="Name for the copy" error={problem}>
          {(p) => <input {...p} style={field.control} value={newName} onChange={(e) => setNewName(e.target.value)} />}
        </Field>
        <div role="alert" style={field.error}>{failure}</div>
        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
          <DSButton type="button" variant="ghost" onClick={onClose}>Cancel</DSButton>
          <DSButton type="submit" variant="primary" disabled={busy}>{busy ? 'Copying…' : 'Duplicate'}</DSButton>
        </div>
      </form>
    </Dialog>
  )
}

// ── Export to Git ────────────────────────────────────────────────────────────

/** Fetches the manifest on open and offers Copy and Download. */
export function ExportDialog({ kind, namespace, name, onClose, onExported }: {
  kind: string; namespace: string; name: string; onClose: () => void; onExported?: () => void
}) {
  const [manifest, setManifest] = useState<string | null>(null)
  const [failure, setFailure] = useState('')
  const [copied, setCopied] = useState(false)

  useEffect(() => {
    let live = true
    exportResource(kind, namespace, name)
      .then((text) => { if (live) { setManifest(text); onExported?.() } })
      .catch((e: unknown) => { if (live) setFailure(messageOf(e)) })
    return () => { live = false }
    // The export starts a handoff, so it runs once per opening.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [kind, namespace, name])

  const copy = () => {
    if (manifest === null) return
    navigator.clipboard.writeText(manifest).then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500) }).catch(() => {})
  }
  const download = () => {
    if (manifest === null) return
    const url = URL.createObjectURL(new Blob([manifest], { type: 'application/yaml' }))
    const a = document.createElement('a')
    a.href = url
    a.download = `${name}.yaml`
    a.click()
    URL.revokeObjectURL(url)
  }

  return (
    <Dialog title="Export to Git" onClose={onClose} width={640}>
      <p style={field.help}>
        Commit this file to the repository your platform team deploys from. Until it is deployed from Git,
        this agent can still be edited here, for a limited time.
      </p>
      <p style={field.help}>
        Once Git manages the agent, it becomes read-only here, and changes are made in Git. If you change
        your mind before then, cancel the handoff from the agent&apos;s page.
      </p>
      {failure && <p role="alert" style={field.error}>Could not export: {failure}</p>}
      {manifest === null && !failure && <p role="status" style={field.help}>Preparing the file…</p>}
      {manifest !== null && (
        <>
          <label htmlFor="export-manifest" style={field.label}>Manifest</label>
          <textarea
            id="export-manifest"
            readOnly
            value={manifest}
            style={{ ...field.control, fontFamily: DESIGN.font.mono, minHeight: 260, resize: 'vertical' }}
          />
        </>
      )}
      <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
        <DSButton type="button" variant="secondary" disabled={manifest === null} onClick={copy}>{copied ? 'Copied' : 'Copy'}</DSButton>
        <DSButton type="button" variant="secondary" disabled={manifest === null} onClick={download}>Download</DSButton>
        <DSButton type="button" variant="primary" onClick={onClose}>Done</DSButton>
      </div>
    </Dialog>
  )
}
