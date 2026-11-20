/**
 * AgentTriggers — webhook triggers of a published agent: create one, see its
 * secret once, copy its URL, switch it off or delete it, and when it last
 * fired. Anyone who can see the agent can read the list; builders change it.
 */
import { useCallback, useEffect, useState, type CSSProperties } from 'react'
import {
  createTrigger, deleteTrigger, describeTriggerError, hookUrl, listTriggers, setTriggerEnabled,
  type CreatedTrigger, type Trigger,
} from '../api/integrations'
import { canWriteEvals, useRoles } from '../lib/access'
import { ds, DESIGN } from '../lib/designSystem'
import { Dialog } from '../lib/dialog'
import { DSButton, DSChip } from '../lib/primitives'
import { Field, field } from './BuilderParts'

export function AgentTriggers({ namespace, name }: { namespace: string; name: string }) {
  const builder = canWriteEvals(useRoles())
  const [triggers, setTriggers] = useState<Trigger[] | null>(null)
  const [error, setError] = useState('')
  const [creating, setCreating] = useState(false)
  const [created, setCreated] = useState<CreatedTrigger | null>(null)
  const [removing, setRemoving] = useState<Trigger | null>(null)

  const load = useCallback(() => {
    let live = true
    listTriggers(namespace, name)
      .then((t) => { if (live) { setTriggers(t); setError('') } })
      .catch((e: unknown) => { if (live) { setError(describeTriggerError(e)); setTriggers((old) => old ?? []) } })
    return () => { live = false }
  }, [namespace, name])
  useEffect(() => load(), [load])

  const toggle = async (t: Trigger) => {
    try {
      await setTriggerEnabled(t.id, !t.enabled)
      load()
    } catch (e) { setError(describeTriggerError(e)) }
  }
  const remove = async (t: Trigger) => {
    try {
      await deleteTrigger(t.id)
      setRemoving(null)
      load()
    } catch (e) { setRemoving(null); setError(describeTriggerError(e)) }
  }

  if (triggers === null) return <div role="status" style={{ padding: 24, color: ds.textMuted, fontSize: 13 }}>Loading triggers…</div>

  return (
    <section aria-label="Triggers" style={s.wrap}>
      <p style={s.lead}>
        A trigger gives another system, such as a build server, a form or a ticket tool, a web address it can send a signed
        request to. Each request starts one run of this agent.
      </p>
      {error && <p role="alert" style={field.error}>{error}</p>}
      {triggers.length === 0 && !error && <p style={s.muted}>No triggers yet.</p>}
      <ul style={s.list}>
        {triggers.map((t) => (
          <li key={t.id} style={s.card} aria-label={`Trigger ${t.name}`}>
            <div style={s.row}>
              <strong style={{ fontSize: 13 }}>{t.name}</strong>
              <DSChip size="sm" variant={t.enabled ? 'success' : 'neutral'} dot>{t.enabled ? 'On' : 'Off'}</DSChip>
              <span style={s.muted}>{t.lastFiredAt ? `Last fired ${new Date(t.lastFiredAt).toLocaleString()}` : 'Never fired'}</span>
              {t.fireCount > 0 && <span style={s.muted}>{t.fireCount} run{t.fireCount === 1 ? '' : 's'}</span>}
            </div>
            <div style={s.row}>
              <code style={s.code} aria-label={`URL of ${t.name}`}>{hookUrl(`/hooks/${t.id}`)}</code>
              <CopyButton text={hookUrl(`/hooks/${t.id}`)} label={`Copy URL of ${t.name}`} />
            </div>
            <p style={s.muted}>
              {t.inputPath ? `The run input is the field ${t.inputPath} of the JSON body.` : 'The run input is the whole body.'}
              {' '}Up to {t.rateLimitPerMin} requests a minute.
            </p>
            {builder && (
              <div style={s.row}>
                <DSButton size="sm" variant="secondary" onClick={() => toggle(t)}>{t.enabled ? 'Disable' : 'Enable'}</DSButton>
                <DSButton size="sm" variant="ghost" onClick={() => setRemoving(t)}>Delete</DSButton>
              </div>
            )}
          </li>
        ))}
      </ul>
      {builder && <div><DSButton variant="primary" onClick={() => setCreating(true)}>Add a trigger</DSButton></div>}

      {creating && (
        <CreateDialog
          namespace={namespace}
          agent={name}
          onClose={() => setCreating(false)}
          onCreated={(c) => { setCreating(false); setCreated(c); load() }}
        />
      )}
      {created && <SecretDialog created={created} onClose={() => setCreated(null)} />}
      {removing && (
        <Dialog title={`Delete ${removing.name}?`} onClose={() => setRemoving(null)}>
          <p style={s.lead}>Anything that still sends requests to this address will get “not found”. This cannot be undone.</p>
          <div style={s.row}>
            <DSButton variant="primary" onClick={() => remove(removing)}>Delete trigger</DSButton>
            <DSButton variant="ghost" onClick={() => setRemoving(null)}>Cancel</DSButton>
          </div>
        </Dialog>
      )}
    </section>
  )
}

function CreateDialog({ namespace, agent, onClose, onCreated }: {
  namespace: string; agent: string; onClose: () => void; onCreated: (c: CreatedTrigger) => void
}) {
  const [name, setName] = useState('')
  const [path, setPath] = useState('')
  const [rate, setRate] = useState('30')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const perMin = Number(rate)
    if (!name.trim()) { setError('Give the trigger a name.'); return }
    if (!Number.isInteger(perMin) || perMin < 1 || perMin > 600) { setError('Requests per minute is a whole number from 1 to 600.'); return }
    setBusy(true)
    setError('')
    try {
      onCreated(await createTrigger({ namespace, agent, name: name.trim(), inputPath: path.trim() || undefined, rateLimitPerMin: perMin }))
    } catch (err) {
      setError(describeTriggerError(err))
      setBusy(false)
    }
  }

  return (
    <Dialog title="Add a trigger" onClose={onClose}>
      <form onSubmit={submit} noValidate style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <Field label="Name" help="Where it is used, for example “GitHub issues”.">
          {(p) => <input {...p} style={field.control} value={name} maxLength={80} onChange={(e) => setName(e.target.value)} />}
        </Field>
        <Field label="Input field (optional)" help="A path into the JSON body, such as issue.title. Leave empty to send the whole body to the agent.">
          {(p) => <input {...p} style={field.control} value={path} onChange={(e) => setPath(e.target.value)} />}
        </Field>
        <Field label="Requests per minute">
          {(p) => <input {...p} style={field.control} inputMode="numeric" value={rate} onChange={(e) => setRate(e.target.value)} />}
        </Field>
        {error && <p role="alert" style={field.error}>{error}</p>}
        <div style={s.row}>
          <DSButton type="submit" variant="primary" disabled={busy}>{busy ? 'Creating…' : 'Create trigger'}</DSButton>
          <DSButton type="button" variant="ghost" onClick={onClose}>Cancel</DSButton>
        </div>
      </form>
    </Dialog>
  )
}

function SecretDialog({ created, onClose }: { created: CreatedTrigger; onClose: () => void }) {
  const url = hookUrl(created.hookPath)
  return (
    <Dialog title="Copy the secret now" onClose={onClose} width={560}>
      <p style={s.lead}>
        This is the only time the secret is shown. It is not stored, so it cannot be shown again; if it is lost, delete the trigger
        and make a new one.
      </p>
      <div style={s.row}>
        <code style={s.code} aria-label="Secret">{created.secret}</code>
        <CopyButton text={created.secret} label="Copy secret" />
      </div>
      <p style={s.muted}>Web address to send requests to:</p>
      <div style={s.row}>
        <code style={s.code}>{url}</code>
        <CopyButton text={url} label="Copy URL" />
      </div>
      <p style={s.muted}>How to sign a request is in docs/integrations.md.</p>
      <div style={s.row}><DSButton variant="primary" onClick={onClose}>I have saved the secret</DSButton></div>
    </Dialog>
  )
}

export function CopyButton({ text, label }: { text: string; label: string }) {
  const [done, setDone] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      setDone(true)
      setTimeout(() => setDone(false), 2000)
    } catch { /* the text is on screen to copy by hand */ }
  }
  return <DSButton size="sm" variant="secondary" aria-label={label} onClick={copy}>{done ? 'Copied' : 'Copy'}</DSButton>
}

const s: Record<string, CSSProperties> = {
  wrap: { padding: '12px 24px', display: 'flex', flexDirection: 'column', gap: 12, maxWidth: 760 },
  lead: { margin: 0, fontSize: 13, color: ds.textSecondary },
  muted: { margin: 0, fontSize: 12, color: ds.textMuted },
  list: { listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 10 },
  card: { border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, padding: 12, display: 'flex', flexDirection: 'column', gap: 8 },
  row: { display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' },
  code: { fontFamily: DESIGN.font.mono, fontSize: 12, background: 'var(--ds-muted-bg)', padding: '4px 8px', borderRadius: DESIGN.radii.md, wordBreak: 'break-all' },
}
