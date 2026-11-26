/**
 * AgentChannels — inbound channels of a published agent (docs/inbound-channels.md): a mailbox
 * and chat platforms. People write where they already are, the agent runs on the message and
 * the answer comes back in the same conversation. Email has its own form; every other kind is
 * drawn from what its adapter describes (GET /api/inbound-kinds). Anyone who can see the agent can read the list; builders change it. The
 * channel can be saved without credentials, so that its web address exists while the app is being
 * set up. Chat channels then have a guided setup (ChannelSetup); a Secret an administrator
 * prepared works too.
 */
import { useCallback, useEffect, useState, type CSSProperties } from 'react'
import {
  createInbound, deleteInbound, describeInboundError, inboundUrl, listInbound, listInboundKinds, parseList, refusalText,
  setInboundEnabled, setInboundSecret, setupSteps, testInbound,
  type EmailAdapterConfig, type InboundConnection, type InboundKind, type InboundTestReport,
} from '../api/inbound'
import { canWriteEvals, useRoles } from '../lib/access'
import { ds, DESIGN } from '../lib/designSystem'
import { Dialog } from '../lib/dialog'
import { DSButton, DSChip } from '../lib/primitives'
import { Field, field } from './BuilderParts'
import { CopyButton } from './AgentTriggers'
import { ChannelSetup } from './ChannelSetup'

export function AgentChannels({ namespace, name }: { namespace: string; name: string }) {
  const builder = canWriteEvals(useRoles())
  const [items, setItems] = useState<InboundConnection[] | null>(null)
  const [error, setError] = useState('')
  const [creating, setCreating] = useState<string | null>(null)
  // The chat channel just made, whose setup opens at once.
  const [fresh, setFresh] = useState('')
  const [kinds, setKinds] = useState<InboundKind[]>([])
  const [removing, setRemoving] = useState<InboundConnection | null>(null)
  const [reports, setReports] = useState<Record<string, InboundTestReport | 'testing'>>({})

  const load = useCallback(() => {
    let live = true
    listInbound(namespace, name)
      .then((l) => { if (live) { setItems(l); setError('') } })
      .catch((e: unknown) => { if (live) { setError(describeInboundError(e)); setItems((old) => old ?? []) } })
    return () => { live = false }
  }, [namespace, name])
  useEffect(() => load(), [load])
  // The kinds are what the platform offers; if the list cannot be read the tab still works for email.
  useEffect(() => { listInboundKinds().then(setKinds).catch(() => setKinds([])) }, [])
  const chatKinds = kinds.filter((k) => k.kind !== 'email')

  const toggle = async (c: InboundConnection) => {
    try { await setInboundEnabled(c.id, !c.enabled); load() } catch (e) { setError(describeInboundError(e)) }
  }
  const remove = async (c: InboundConnection) => {
    try { await deleteInbound(c.id); setRemoving(null); load() } catch (e) { setRemoving(null); setError(describeInboundError(e)) }
  }
  const test = async (c: InboundConnection) => {
    setReports((r) => ({ ...r, [c.id]: 'testing' }))
    try {
      const rep = await testInbound(c.id)
      setReports((r) => ({ ...r, [c.id]: rep }))
    } catch (e) {
      setReports((r) => ({ ...r, [c.id]: { ok: false, error: describeInboundError(e) } }))
    }
  }

  if (items === null) return <div role="status" style={{ padding: 24, color: ds.textMuted, fontSize: 13 }}>Loading channels…</div>

  return (
    <section aria-label="Channels" style={s.wrap}>
      <p style={s.lead}>
        A channel lets people write to this agent where they already are. For email, people write to an address and the agent
        answers in the same conversation. Only the senders you list can start a run, and a message is only ever the agent’s
        input, never its instructions.
      </p>
      {error && <p role="alert" style={field.error}>{error}</p>}
      {items.length === 0 && !error && <p style={s.muted}>No channels yet.</p>}
      <ul style={s.list}>
        {items.map((c) => <ChannelCard key={c.id} c={c} kind={kinds.find((k) => k.kind === c.kind)} builder={builder} report={reports[c.id]} onToggle={toggle} onTest={test} onDelete={setRemoving} onChanged={load} openSetup={c.id === fresh} />)}
      </ul>
      {builder && <div><DSButton variant="primary" onClick={() => setCreating(chatKinds.length ? '' : 'email')}>Add a channel</DSButton></div>}

      {creating === '' && (
        <Dialog title="Add a channel" onClose={() => setCreating(null)}>
          <p style={s.lead}>Where should people reach this agent?</p>
          <div style={s.row}>
            <DSButton variant="secondary" onClick={() => setCreating('email')}>Email</DSButton>
            {chatKinds.map((k) => (
              <DSButton key={k.kind} variant="secondary" onClick={() => setCreating(k.kind)}>{k.descriptor.title}</DSButton>
            ))}
          </div>
        </Dialog>
      )}
      {creating === 'email' && (
        <CreateDialog namespace={namespace} agent={name} onClose={() => setCreating(null)} onCreated={() => { setCreating(null); load() }} />
      )}
      {creating && creating !== 'email' && kinds.find((k) => k.kind === creating) && (
        <GenericCreateDialog
          namespace={namespace}
          agent={name}
          kind={kinds.find((k) => k.kind === creating)!}
          onClose={() => setCreating(null)}
          onCreated={(id) => { setCreating(null); setFresh(id); load() }}
        />
      )}
      {removing && (
        <Dialog title={`Delete ${removing.name}?`} onClose={() => setRemoving(null)}>
          <p style={s.lead}>The agent stops answering this channel. Mail already in the mailbox is not touched. This cannot be undone.</p>
          <div style={s.row}>
            <DSButton variant="primary" onClick={() => remove(removing)}>Delete channel</DSButton>
            <DSButton variant="ghost" onClick={() => setRemoving(null)}>Cancel</DSButton>
          </div>
        </Dialog>
      )}
    </section>
  )
}

function ChannelCard({ c, kind, builder, report, onToggle, onTest, onDelete, onChanged, openSetup }: {
  c: InboundConnection; kind?: InboundKind; builder: boolean; report?: InboundTestReport | 'testing'
  onToggle: (c: InboundConnection) => void; onTest: (c: InboundConnection) => void; onDelete: (c: InboundConnection) => void
  onChanged: () => void
  openSetup: boolean
}) {
  const mode = c.config.adapter?.mode
  const url = c.url ?? inboundUrl(c.path)
  const chat = c.kind !== 'email'
  const steps = setupSteps(kind, url)
  // Without credentials a channel cannot check or answer anything.
  const draft = !c.credentials && !c.secretRef
  const [guided, setGuided] = useState(openSetup)
  // Until someone is allowed, nothing the channel receives is answered.
  const nobody = chat && !c.config.allowSenders?.length && !c.config.allowGroups?.length && !c.config.allowChannels?.length
  const refused = Object.entries(c.refused ?? {}).sort((a, b) => b[1] - a[1])
  return (
    <li style={s.card} aria-label={`Channel ${c.name}`}>
      <div style={s.row}>
        <strong style={{ fontSize: 13 }}>{c.name}</strong>
        <DSChip size="sm">{c.kind === 'email' ? 'Email' : (kind?.descriptor.title ?? c.kind)}{c.kind === 'email' && mode ? ` (${mode === 'imap' ? 'mailbox' : 'provider webhook'})` : ''}</DSChip>
        {draft || nobody
          ? <DSChip size="sm" variant="warning" dot>Not ready</DSChip>
          : <DSChip size="sm" variant={c.enabled ? 'success' : 'neutral'} dot>{c.enabled ? 'On' : 'Off'}</DSChip>}
      </div>
      <p style={s.muted}>
        {c.receivedCount} received, {c.repliedCount} answered, {c.rejectedCount} refused.
        {mode === 'imap' && (c.lastPollAt ? ` Last checked ${new Date(c.lastPollAt).toLocaleString()}.` : ' Not checked yet.')}
      </p>
      {c.lastError && <p role="alert" style={field.error}>Last problem: {c.lastError}</p>}
      <p style={s.muted}>
        May write: {[...(c.config.allowSenders ?? []), ...(c.config.allowGroups ?? []).map((g) => (chat ? `anyone in ${g}` : `anyone at ${g}`))].join(', ') || (c.config.allowChannels?.length ? 'anyone in the listed channels' : 'nobody')}
        {chat && c.config.allowChannels?.length ? `. Only in: ${c.config.allowChannels.join(', ')}` : ''}
        {chat && c.config.modes?.length ? `. Answers: ${c.config.modes.join(', ')}` : ''}
      </p>
      {(mode === 'webhook' || chat) && (
        <div style={s.row}>
          <code style={s.code} aria-label={`URL of ${c.name}`}>{url}</code>
          <CopyButton text={url} label={`Copy URL of ${c.name}`} />
        </div>
      )}
      {(mode === 'webhook' || chat) && !c.url && (
        <p style={s.muted}>
          This is the address you are using now. If the other service reaches agent-orca at a different one, use that
          instead. An administrator can set it as <code>operator.inbound.publicURL</code>.
        </p>
      )}
      {chat && kind && builder && !guided && (
        <div>
          <DSButton size="sm" variant={draft || nobody ? 'primary' : 'secondary'} onClick={() => setGuided(true)}>
            {draft || nobody ? 'Continue setup' : 'Setup and pairing'}
          </DSButton>
        </div>
      )}
      {guided && kind && <ChannelSetup c={c} kind={kind} onChanged={onChanged} onClose={() => setGuided(false)} />}
      {draft && builder && !guided && (
        <details>
          <summary style={{ fontSize: 12, cursor: 'pointer' }}>Use a Kubernetes Secret instead</summary>
          <AddSecret c={c} kind={kind} onAdded={onChanged} />
        </details>
      )}
      {refused.length > 0 && (
        <div style={s.muted}>
          Refused in the last 24 hours:
          <ul aria-label="Messages refused in the last 24 hours" style={{ margin: '4px 0 0', paddingLeft: 18 }}>
            {refused.map(([code, n]) => <li key={code}>{n}: {refusalText(code)}</li>)}
          </ul>
        </div>
      )}
      {chat && steps.length > 0 && !guided && (
        <details>
          <summary style={{ fontSize: 12, cursor: 'pointer' }}>Setup steps</summary>
          <ol aria-label={`Setup steps of ${c.name}`} style={{ margin: '6px 0 0', paddingLeft: 18, fontSize: 12, color: ds.textSecondary }}>
            {steps.map((t, i) => <li key={i}>{t}</li>)}
          </ol>
        </details>
      )}
      {report === 'testing' && <p role="status" style={s.muted}>Testing…</p>}
      {report && report !== 'testing' && (
        <div role="status" style={s.muted}>
          <strong>{report.ok ? 'The connection works.' : 'The test failed.'}</strong>{report.error ? ` ${report.error}` : ''}
          {report.details && (
            <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>
              {Object.entries(report.details).map(([k, v]) => <li key={k}>{k}: {v}</li>)}
            </ul>
          )}
        </div>
      )}
      {builder && (
        <div style={s.row}>
          <DSButton size="sm" variant="secondary" onClick={() => onToggle(c)}>{c.enabled ? 'Disable' : 'Enable'}</DSButton>
          {!draft && <DSButton size="sm" variant="secondary" onClick={() => onTest(c)}>Test connection</DSButton>}
          <DSButton size="sm" variant="ghost" onClick={() => onDelete(c)}>Delete</DSButton>
        </div>
      )}
    </li>
  )
}

function AddSecret({ c, kind, onAdded }: { c: InboundConnection; kind?: InboundKind; onAdded: () => void }) {
  const [secretRef, setSecretRef] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!secretRef.trim()) { setError('Name the Secret your administrator prepared.'); return }
    setBusy(true)
    setError('')
    try {
      await setInboundSecret(c.id, secretRef.trim())
      onAdded()
    } catch (err) {
      setError(describeInboundError(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form onSubmit={submit} noValidate style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
      <Field label="Secret" help={secretHelp(kind)}>
        {(p) => <input {...p} style={field.control} value={secretRef} onChange={(e) => setSecretRef(e.target.value)} />}
      </Field>
      {error && <p role="alert" style={field.error}>{error}</p>}
      <div><DSButton type="submit" size="sm" variant="primary" disabled={busy}>{busy ? 'Checking…' : 'Add credentials'}</DSButton></div>
    </form>
  )
}

function secretHelp(kind: InboundKind | undefined): string {
  const keys = kind?.descriptor.secretKeys
  const named = keys?.length ? ` with the keys ${keys.map((k) => k.key + (k.required ? '' : ' (optional)')).join(', ')}` : ''
  return `The name of a Secret in this workspace that holds the credentials. Your administrator creates it${named}; no token is typed here.`
}

function CreateDialog({ namespace, agent, onClose, onCreated }: {
  namespace: string; agent: string; onClose: () => void; onCreated: () => void
}) {
  const [name, setName] = useState('')
  const [mode, setMode] = useState<'imap' | 'webhook'>('imap')
  const [secretRef, setSecretRef] = useState('')
  const [senders, setSenders] = useState('')
  const [domains, setDomains] = useState('')
  const [requireAuth, setRequireAuth] = useState(true)
  const [authserv, setAuthserv] = useState('')
  const [folder, setFolder] = useState('')
  const [processed, setProcessed] = useState('')
  const [textFiles, setTextFiles] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const allowSenders = parseList(senders)
    const allowGroups = parseList(domains)
    if (!name.trim()) { setError('Give the channel a name.'); return }
    if (!secretRef.trim()) { setError('Name the Secret your administrator prepared for this mailbox.'); return }
    if (allowSenders.length + allowGroups.length === 0) { setError('List who may write in: addresses, or domains. Nobody is allowed by default.'); return }
    const adapter: EmailAdapterConfig = { mode, requireAuthResults: requireAuth, acceptTextAttachments: textFiles }
    if (authserv.trim()) adapter.trustedAuthservId = authserv.trim()
    if (mode === 'imap') {
      if (folder.trim()) adapter.folder = folder.trim()
      if (processed.trim()) adapter.processedFolder = processed.trim()
    }
    setBusy(true)
    setError('')
    try {
      await createInbound({
        namespace, agent, name: name.trim(), kind: 'email', secretRef: secretRef.trim(),
        config: { allowSenders, allowGroups, adapter },
      })
      onCreated()
    } catch (err) {
      setError(describeInboundError(err))
      setBusy(false)
    }
  }

  return (
    <Dialog title="Add an email channel" onClose={onClose} width={560}>
      <form onSubmit={submit} noValidate style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <Field label="Name" help="For example “Support mailbox”.">
          {(p) => <input {...p} style={field.control} value={name} maxLength={80} onChange={(e) => setName(e.target.value)} />}
        </Field>
        <Field label="How mail arrives">
          {(p) => (
            <select {...p} style={field.control} value={mode} onChange={(e) => setMode(e.target.value as 'imap' | 'webhook')}>
              <option value="imap">The agent checks a mailbox (IMAP)</option>
              <option value="webhook">My mail provider sends each message to a web address</option>
            </select>
          )}
        </Field>
        <Field label="Secret" help="The name of a Secret in this workspace that holds the mailbox login and where replies are sent from. Your administrator creates it; no password is typed here.">
          {(p) => <input {...p} style={field.control} value={secretRef} onChange={(e) => setSecretRef(e.target.value)} />}
        </Field>
        <Field label="Who may write (addresses)" help="One per line. Exact addresses only.">
          {(p) => <textarea {...p} style={{ ...field.control, minHeight: 56 }} value={senders} onChange={(e) => setSenders(e.target.value)} />}
        </Field>
        <Field label="Who may write (whole domains)" help="One per line, for example example.com. Subdomains are not included.">
          {(p) => <textarea {...p} style={{ ...field.control, minHeight: 40 }} value={domains} onChange={(e) => setDomains(e.target.value)} />}
        </Field>
        <label style={{ display: 'flex', gap: 8, alignItems: 'center', fontSize: 13 }}>
          <input type="checkbox" checked={requireAuth} onChange={(e) => setRequireAuth(e.target.checked)} />
          Require proof of the sender’s domain from our mail server (recommended)
        </label>
        <Field label="Mail server name (optional)" help="The host name your own mail server writes in its Authentication-Results header. Strongly recommended.">
          {(p) => <input {...p} style={field.control} value={authserv} onChange={(e) => setAuthserv(e.target.value)} />}
        </Field>
        {mode === 'imap' && (
          <>
            <Field label="Folder (optional)" help="Default INBOX.">
              {(p) => <input {...p} style={field.control} value={folder} onChange={(e) => setFolder(e.target.value)} />}
            </Field>
            <Field label="Move handled mail to (optional)" help="A folder. Empty marks handled mail as read instead.">
              {(p) => <input {...p} style={field.control} value={processed} onChange={(e) => setProcessed(e.target.value)} />}
            </Field>
          </>
        )}
        <label style={{ display: 'flex', gap: 8, alignItems: 'center', fontSize: 13 }}>
          <input type="checkbox" checked={textFiles} onChange={(e) => setTextFiles(e.target.checked)} />
          Pass small text attachments (.txt, .csv, .md) to the agent
        </label>
        {error && <p role="alert" style={field.error}>{error}</p>}
        <div style={s.row}>
          <DSButton type="submit" variant="primary" disabled={busy}>{busy ? 'Creating…' : 'Create channel'}</DSButton>
          <DSButton type="button" variant="ghost" onClick={onClose}>Cancel</DSButton>
        </div>
      </form>
    </Dialog>
  )
}

function GenericCreateDialog({ namespace, agent, kind, onClose, onCreated }: {
  namespace: string; agent: string; kind: InboundKind; onClose: () => void; onCreated: (id: string) => void
}) {
  const d = kind.descriptor
  const offered = kind.capabilities.modes ?? []
  const [name, setName] = useState('')
  const [secretRef, setSecretRef] = useState('')
  const [users, setUsers] = useState('')
  const [groups, setGroups] = useState('')
  const [channels, setChannels] = useState('')
  const [modes, setModes] = useState<string[]>(offered.filter((m) => m !== 'channel'))
  const [values, setValues] = useState<Record<string, string | boolean>>(() =>
    Object.fromEntries((d.fields ?? []).filter((f) => f.default !== undefined).map((f) => [f.key, f.default as string | boolean])))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const set = (k: string, v: string | boolean) => setValues((o) => ({ ...o, [k]: v }))

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const allowSenders = parseList(users)
    const allowGroups = parseList(groups)
    const allowChannels = parseList(channels)
    if (!name.trim()) { setError('Give the channel a name.'); return }
    if (offered.length > 0 && modes.length === 0) { setError('Choose at least one way of addressing the agent.'); return }
    if (modes.includes('channel') && allowChannels.length === 0) { setError('Answering every message of a channel needs that channel on the list.'); return }
    const adapter: Record<string, string | number | boolean> = {}
    for (const f of d.fields ?? []) {
      const v = values[f.key]
      if (f.required && (v === undefined || v === '')) { setError(`${f.label} is required.`); return }
      if (v === undefined || v === '') continue
      if (f.type === 'int') {
        const n = Number(v)
        if (!Number.isFinite(n)) { setError(`${f.label} must be a number.`); return }
        adapter[f.key] = n
      } else adapter[f.key] = typeof v === 'string' ? v.trim() : v
    }
    setBusy(true)
    setError('')
    try {
      const made = await createInbound({
        namespace, agent, name: name.trim(), kind: kind.kind, secretRef: secretRef.trim(),
        config: { allowSenders, allowGroups, allowChannels, ...(offered.length ? { modes } : {}), adapter },
      })
      onCreated(made?.id ?? '')
    } catch (err) {
      setError(describeInboundError(err))
      setBusy(false)
    }
  }

  return (
    <Dialog title={`Add a ${d.title} channel`} onClose={onClose} width={560}>
      <form onSubmit={submit} noValidate style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        {d.summary && <p style={s.lead}>{d.summary}</p>}
        {(d.setup?.length ?? 0) > 0 && (
          <details open>
            <summary style={{ fontSize: 13, cursor: 'pointer' }}>Before you start: what you will do</summary>
            <ol aria-label={`Steps on ${d.title}`} style={{ margin: '6px 0 0', paddingLeft: 18, fontSize: 12, color: ds.textSecondary, maxHeight: 180, overflowY: 'auto' }}>
              {setupSteps(kind, '(the address shown once the channel is created)').map((t, i) => <li key={i}>{t}</li>)}
            </ol>
          </details>
        )}
        <Field label="Name" help="For example “Support in chat”.">
          {(p) => <input {...p} style={field.control} value={name} maxLength={80} onChange={(e) => setName(e.target.value)} />}
        </Field>
        <Field label="Secret (can be added later)" help={`${secretHelp(kind)} Leave it empty if the app does not exist yet: creating the channel gives you the address to set it up with.`}>
          {(p) => <input {...p} style={field.control} value={secretRef} onChange={(e) => setSecretRef(e.target.value)} />}
        </Field>
        {(d.fields ?? []).map((f) => (
          f.type === 'bool' ? (
            <label key={f.key} style={{ display: 'flex', gap: 8, alignItems: 'center', fontSize: 13 }}>
              <input type="checkbox" checked={values[f.key] === true} onChange={(e) => set(f.key, e.target.checked)} />
              {f.label}
            </label>
          ) : (
            <Field key={f.key} label={f.label + (f.required ? '' : ' (optional)')} help={f.help}>
              {(p) => f.type === 'enum' ? (
                <select {...p} style={field.control} value={String(values[f.key] ?? '')} onChange={(e) => set(f.key, e.target.value)}>
                  <option value="">Choose…</option>
                  {(f.options ?? []).map((o) => <option key={o} value={o}>{o}</option>)}
                </select>
              ) : (
                <input {...p} style={field.control} value={String(values[f.key] ?? '')} placeholder={f.placeholder} onChange={(e) => set(f.key, e.target.value)} />
              )}
            </Field>
          )
        ))}
        <p style={s.muted}>
          Nobody may write until they are on the list. After creating the channel, send a one-time code from {d.title} to
          add yourself, or list ids here.
        </p>
        <details>
          <summary style={{ fontSize: 13, cursor: 'pointer' }}>List people and channels by id (optional)</summary>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 12, marginTop: 8 }}>
        {d.allow?.users && (
          <Field label="Who may write (user ids)" help={d.allow.users}>
            {(p) => <textarea {...p} style={{ ...field.control, minHeight: 48 }} value={users} onChange={(e) => setUsers(e.target.value)} />}
          </Field>
        )}
        {d.allow?.groups && (
          <Field label="Who may write (whole workspaces)" help={d.allow.groups}>
            {(p) => <textarea {...p} style={{ ...field.control, minHeight: 40 }} value={groups} onChange={(e) => setGroups(e.target.value)} />}
          </Field>
        )}
        {d.allow?.channels && (
          <Field label="Only in these channels (optional)" help={d.allow.channels}>
            {(p) => <textarea {...p} style={{ ...field.control, minHeight: 40 }} value={channels} onChange={(e) => setChannels(e.target.value)} />}
          </Field>
        )}
          </div>
        </details>
        {offered.length > 0 && (
          <fieldset style={{ border: 'none', padding: 0, margin: 0, display: 'flex', flexDirection: 'column', gap: 4 }}>
            <legend style={{ fontSize: 13, marginBottom: 4 }}>The agent answers</legend>
            {offered.map((m) => (
              <label key={m} style={{ display: 'flex', gap: 8, alignItems: 'center', fontSize: 13 }}>
                <input type="checkbox" checked={modes.includes(m)} onChange={(e) => setModes((cur) => (e.target.checked ? [...cur, m] : cur.filter((x) => x !== m)))} />
                {MODE_LABEL[m] ?? m}
              </label>
            ))}
          </fieldset>
        )}
        {error && <p role="alert" style={field.error}>{error}</p>}
        <div style={s.row}>
          <DSButton type="submit" variant="primary" disabled={busy}>{busy ? 'Creating…' : 'Create channel'}</DSButton>
          <DSButton type="button" variant="ghost" onClick={onClose}>Cancel</DSButton>
        </div>
      </form>
    </Dialog>
  )
}

const MODE_LABEL: Record<string, string> = {
  direct: 'Direct messages to the bot',
  mention: 'When the bot is mentioned in a channel',
  command: 'Slash commands',
  channel: 'Every message in the listed channels (no mention needed)',
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
