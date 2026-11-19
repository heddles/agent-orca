/**
 * AgentWidget — chat widget keys of a published agent: create one with the pages
 * allowed to embed it, copy the embed snippet, change the limits, switch it off
 * or delete it. Anyone who can see the agent can read the list; builders change it.
 */
import { useCallback, useEffect, useState, type CSSProperties } from 'react'
import {
  createWidgetKey, deleteWidgetKey, describeTriggerError, embedSnippet, listWidgetKeys, parseOrigins, updateWidgetKey,
  type WidgetKey,
} from '../api/integrations'
import { canWriteEvals, useRoles } from '../lib/access'
import { ds, DESIGN } from '../lib/designSystem'
import { Dialog } from '../lib/dialog'
import { DSButton, DSChip } from '../lib/primitives'
import { Field, field } from './BuilderParts'
import { CopyButton } from './AgentTriggers'

export function AgentWidget({ namespace, name }: { namespace: string; name: string }) {
  const builder = canWriteEvals(useRoles())
  const [keys, setKeys] = useState<WidgetKey[] | null>(null)
  const [error, setError] = useState('')
  const [editing, setEditing] = useState<WidgetKey | 'new' | null>(null)
  const [removing, setRemoving] = useState<WidgetKey | null>(null)

  const load = useCallback(() => {
    let live = true
    listWidgetKeys(namespace, name)
      .then((k) => { if (live) { setKeys(k); setError('') } })
      .catch((e: unknown) => { if (live) { setError(describeTriggerError(e)); setKeys((old) => old ?? []) } })
    return () => { live = false }
  }, [namespace, name])
  useEffect(() => load(), [load])

  const toggle = async (k: WidgetKey) => {
    try { await updateWidgetKey(k.id, { enabled: !k.enabled }); load() } catch (e) { setError(describeTriggerError(e)) }
  }
  const remove = async (k: WidgetKey) => {
    try { await deleteWidgetKey(k.id); setRemoving(null); load() } catch (e) { setRemoving(null); setError(describeTriggerError(e)) }
  }

  if (keys === null) return <div role="status" style={{ padding: 24, color: ds.textMuted, fontSize: 13 }}>Loading widget keys…</div>

  return (
    <section aria-label="Chat widget" style={s.wrap}>
      <p style={s.lead}>
        Put this agent’s chat on a web page. Each key works only on the pages you list, has its own limit on messages and a daily
        spending cap, and can only talk to this agent. The key is not a password: anyone can see it in the page.
      </p>
      {error && <p role="alert" style={field.error}>{error}</p>}
      {keys.length === 0 && !error && <p style={s.muted}>No widget keys yet.</p>}
      <ul style={s.list}>
        {keys.map((k) => (
          <li key={k.id} style={s.card} aria-label={`Widget key ${k.name}`}>
            <div style={s.row}>
              <strong style={{ fontSize: 13 }}>{k.name}</strong>
              <DSChip size="sm" variant={k.enabled ? 'success' : 'neutral'} dot>{k.enabled ? 'On' : 'Off'}</DSChip>
            </div>
            <p style={s.muted}>Pages: {k.allowedOrigins.join(', ')}</p>
            <p style={s.muted}>Up to {k.rateLimitPerMin} messages a minute and ${k.budgetUsdPerDay} a day across all visitors.</p>
            <p style={s.muted}>Paste this on those pages:</p>
            <div style={s.row}>
              <code style={s.code} aria-label={`Embed snippet of ${k.name}`}>{embedSnippet(k.id)}</code>
              <CopyButton text={embedSnippet(k.id)} label={`Copy snippet of ${k.name}`} />
            </div>
            {builder && (
              <div style={s.row}>
                <DSButton size="sm" variant="secondary" onClick={() => setEditing(k)}>Edit</DSButton>
                <DSButton size="sm" variant="secondary" onClick={() => toggle(k)}>{k.enabled ? 'Disable' : 'Enable'}</DSButton>
                <DSButton size="sm" variant="ghost" onClick={() => setRemoving(k)}>Delete</DSButton>
              </div>
            )}
          </li>
        ))}
      </ul>
      {builder && <div><DSButton variant="primary" onClick={() => setEditing('new')}>Add a widget key</DSButton></div>}

      {editing && (
        <KeyDialog
          namespace={namespace}
          agent={name}
          existing={editing === 'new' ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={() => { setEditing(null); load() }}
        />
      )}
      {removing && (
        <Dialog title={`Delete ${removing.name}?`} onClose={() => setRemoving(null)}>
          <p style={s.lead}>Pages that still embed this key will show “not available”. This cannot be undone.</p>
          <div style={s.row}>
            <DSButton variant="primary" onClick={() => remove(removing)}>Delete key</DSButton>
            <DSButton variant="ghost" onClick={() => setRemoving(null)}>Cancel</DSButton>
          </div>
        </Dialog>
      )}
    </section>
  )
}

function KeyDialog({ namespace, agent, existing, onClose, onSaved }: {
  namespace: string; agent: string; existing: WidgetKey | null; onClose: () => void; onSaved: () => void
}) {
  const [name, setName] = useState(existing?.name ?? '')
  const [origins, setOrigins] = useState(existing?.allowedOrigins.join('\n') ?? '')
  const [rate, setRate] = useState(String(existing?.rateLimitPerMin ?? 30))
  const [budget, setBudget] = useState(String(existing?.budgetUsdPerDay ?? 5))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const list = parseOrigins(origins)
    const perMin = Number(rate)
    const perDay = Number(budget)
    if (!existing && !name.trim()) { setError('Give the key a name.'); return }
    if (list.length === 0) { setError('List at least one page address, such as https://www.example.com.'); return }
    if (list.some((o) => o.includes('*'))) { setError('Wildcards are not allowed: list each address.'); return }
    if (!Number.isInteger(perMin) || perMin < 1 || perMin > 600) { setError('Messages per minute is a whole number from 1 to 600.'); return }
    if (!(perDay > 0) || perDay > 10000) { setError('The daily budget is above 0 and at most 10000 dollars.'); return }
    setBusy(true)
    setError('')
    try {
      if (existing) await updateWidgetKey(existing.id, { allowedOrigins: list, rateLimitPerMin: perMin, budgetUsdPerDay: perDay })
      else await createWidgetKey({ namespace, agent, name: name.trim(), allowedOrigins: list, rateLimitPerMin: perMin, budgetUsdPerDay: perDay })
      onSaved()
    } catch (err) {
      setError(describeTriggerError(err))
      setBusy(false)
    }
  }

  return (
    <Dialog title={existing ? `Edit ${existing.name}` : 'Add a widget key'} onClose={onClose} width={520}>
      <form onSubmit={submit} noValidate style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        {!existing && (
          <Field label="Name" help="Where it is used, for example “Help center”.">
            {(p) => <input {...p} style={field.control} value={name} maxLength={80} onChange={(e) => setName(e.target.value)} />}
          </Field>
        )}
        <Field label="Pages that may show the chat" help="One address per line, exactly as the browser shows it: https://www.example.com (http only for localhost).">
          {(p) => <textarea {...p} style={{ ...field.control, minHeight: 80 }} value={origins} onChange={(e) => setOrigins(e.target.value)} />}
        </Field>
        <Field label="Messages per minute" help="Across all visitors of this key.">
          {(p) => <input {...p} style={field.control} inputMode="numeric" value={rate} onChange={(e) => setRate(e.target.value)} />}
        </Field>
        <Field label="Daily budget (US dollars)" help="When the runs of this key have cost this much today, the chat says it is unavailable until tomorrow.">
          {(p) => <input {...p} style={field.control} inputMode="decimal" value={budget} onChange={(e) => setBudget(e.target.value)} />}
        </Field>
        {error && <p role="alert" style={field.error}>{error}</p>}
        <div style={s.row}>
          <DSButton type="submit" variant="primary" disabled={busy}>{busy ? 'Saving…' : existing ? 'Save changes' : 'Create key'}</DSButton>
          <DSButton type="button" variant="ghost" onClick={onClose}>Cancel</DSButton>
        </div>
      </form>
    </Dialog>
  )
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
