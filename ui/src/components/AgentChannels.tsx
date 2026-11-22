/**
 * AgentChannels — inbound channels of a published agent (docs/inbound-channels.md): today a
 * mailbox. People write to the address, the agent runs on the message and the answer is
 * mailed back. Anyone who can see the agent can read the list; builders change it. The
 * connection names a Secret an administrator prepared: no password is typed here.
 */
import { useCallback, useEffect, useState, type CSSProperties } from 'react'
import {
  createInbound, deleteInbound, describeInboundError, inboundUrl, listInbound, parseList, setInboundEnabled, testInbound,
  type EmailAdapterConfig, type InboundConnection, type InboundTestReport,
} from '../api/inbound'
import { canWriteEvals, useRoles } from '../lib/access'
import { ds, DESIGN } from '../lib/designSystem'
import { Dialog } from '../lib/dialog'
import { DSButton, DSChip } from '../lib/primitives'
import { Field, field } from './BuilderParts'
import { CopyButton } from './AgentTriggers'

export function AgentChannels({ namespace, name }: { namespace: string; name: string }) {
  const builder = canWriteEvals(useRoles())
  const [items, setItems] = useState<InboundConnection[] | null>(null)
  const [error, setError] = useState('')
  const [creating, setCreating] = useState(false)
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
        {items.map((c) => <ChannelCard key={c.id} c={c} builder={builder} report={reports[c.id]} onToggle={toggle} onTest={test} onDelete={setRemoving} />)}
      </ul>
      {builder && <div><DSButton variant="primary" onClick={() => setCreating(true)}>Add a channel</DSButton></div>}

      {creating && (
        <CreateDialog
          namespace={namespace}
          agent={name}
          onClose={() => setCreating(false)}
          onCreated={() => { setCreating(false); load() }}
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

function ChannelCard({ c, builder, report, onToggle, onTest, onDelete }: {
  c: InboundConnection; builder: boolean; report?: InboundTestReport | 'testing'
  onToggle: (c: InboundConnection) => void; onTest: (c: InboundConnection) => void; onDelete: (c: InboundConnection) => void
}) {
  const mode = c.config.adapter?.mode
  const url = inboundUrl(c.path)
  return (
    <li style={s.card} aria-label={`Channel ${c.name}`}>
      <div style={s.row}>
        <strong style={{ fontSize: 13 }}>{c.name}</strong>
        <DSChip size="sm">{c.kind === 'email' ? 'Email' : c.kind}{mode ? ` (${mode === 'imap' ? 'mailbox' : 'provider webhook'})` : ''}</DSChip>
        <DSChip size="sm" variant={c.enabled ? 'success' : 'neutral'} dot>{c.enabled ? 'On' : 'Off'}</DSChip>
      </div>
      <p style={s.muted}>
        {c.receivedCount} received, {c.repliedCount} answered, {c.rejectedCount} refused.
        {mode === 'imap' && (c.lastPollAt ? ` Last checked ${new Date(c.lastPollAt).toLocaleString()}.` : ' Not checked yet.')}
      </p>
      {c.lastError && <p role="alert" style={field.error}>Last problem: {c.lastError}</p>}
      <p style={s.muted}>
        May write: {[...(c.config.allowSenders ?? []), ...(c.config.allowGroups ?? []).map((g) => `anyone at ${g}`)].join(', ') || 'nobody'}
      </p>
      {mode === 'webhook' && (
        <div style={s.row}>
          <code style={s.code} aria-label={`URL of ${c.name}`}>{url}</code>
          <CopyButton text={url} label={`Copy URL of ${c.name}`} />
        </div>
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
          <DSButton size="sm" variant="secondary" onClick={() => onTest(c)}>Test connection</DSButton>
          <DSButton size="sm" variant="ghost" onClick={() => onDelete(c)}>Delete</DSButton>
        </div>
      )}
    </li>
  )
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

const s: Record<string, CSSProperties> = {
  wrap: { padding: '12px 24px', display: 'flex', flexDirection: 'column', gap: 12, maxWidth: 760 },
  lead: { margin: 0, fontSize: 13, color: ds.textSecondary },
  muted: { margin: 0, fontSize: 12, color: ds.textMuted },
  list: { listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 10 },
  card: { border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, padding: 12, display: 'flex', flexDirection: 'column', gap: 8 },
  row: { display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' },
  code: { fontFamily: DESIGN.font.mono, fontSize: 12, background: 'var(--ds-muted-bg)', padding: '4px 8px', borderRadius: DESIGN.radii.md, wordBreak: 'break-all' },
}
