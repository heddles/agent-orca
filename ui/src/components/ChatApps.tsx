/**
 * ChatApps — Settings → Chat apps (administrators): connect a chat app once, check that it
 * works, and decide which agent answers where by default.
 */
import { useCallback, useEffect, useState, type CSSProperties } from 'react'
import { createChatApp, listChatApps, type ChatApp } from '../api/chatApps'
import {
  deleteInbound, describeInboundError, listInboundKinds, refusalText, setInboundEnabled, testInbound,
  type InboundKind, type InboundTestReport,
} from '../api/inbound'
import { DESIGN, ds } from '../lib/designSystem'
import { Dialog } from '../lib/dialog'
import { DSButton, DSChip } from '../lib/primitives'
import { Field, field } from './BuilderParts'
import { ChannelSetup } from './ChannelSetup'
import { WhoAnswers } from './WhoAnswers'

/** The platforms with a guided setup. Others keep the per-agent channel. */
const GUIDED = ['slack']

export function ChatApps() {
  const [apps, setApps] = useState<ChatApp[] | null>(null)
  const [kinds, setKinds] = useState<InboundKind[]>([])
  const [error, setError] = useState('')
  const [adding, setAdding] = useState<InboundKind | null>(null)
  const [fresh, setFresh] = useState('')

  const load = useCallback(() => {
    listChatApps()
      .then((a) => { setApps(a); setError('') })
      .catch((e: unknown) => { setError(describeInboundError(e)); setApps((old) => old ?? []) })
  }, [])
  useEffect(() => { load() }, [load])
  useEffect(() => { listInboundKinds().then(setKinds).catch(() => setKinds([])) }, [])
  const guided = kinds.filter((k) => GUIDED.includes(k.kind))

  return (
    <section aria-label="Chat apps" style={s.wrap}>
      <p style={s.lead}>Connect a chat workspace once. Then each agent chooses where it answers, from its Channels tab.</p>
      {error && <p role="alert" style={field.error}>{error}</p>}
      {apps === null && <p role="status" style={s.quiet}>Loading chat apps…</p>}
      {apps?.length === 0 && !error && <p style={s.quiet}>No chat app is connected yet.</p>}
      <ul style={s.list}>
        {(apps ?? []).map((a) => (
          <AppCard key={a.id} app={a} kind={kinds.find((k) => k.kind === a.kind)} openSetup={a.id === fresh} onChanged={load} />
        ))}
      </ul>
      <div style={s.row}>
        {guided.map((k) => <DSButton key={k.kind} variant="primary" onClick={() => setAdding(k)}>Connect {k.descriptor.title}</DSButton>)}
      </div>
      <p style={s.quiet}>
        Mattermost and Microsoft Teams get a shared setup next. Until then, add them from an agent’s Channels tab.
      </p>
      {adding && <AddApp kind={adding} onClose={() => setAdding(null)} onAdded={(id) => { setAdding(null); setFresh(id); load() }} />}
    </section>
  )
}

function AddApp({ kind, onClose, onAdded }: { kind: InboundKind; onClose: () => void; onAdded: (id: string) => void }) {
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!name.trim()) { setError('Give it a name.'); return }
    setBusy(true)
    setError('')
    try { onAdded((await createChatApp(name.trim(), kind.kind)).id) }
    catch (err) { setError(describeInboundError(err)); setBusy(false) }
  }
  return (
    <Dialog title={`Connect ${kind.descriptor.title}`} onClose={onClose} width={480}>
      <form onSubmit={submit} noValidate style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <Field label="Name" help="What people here call the workspace. It also names the bot, for example “Home”.">
          {(p) => <input {...p} style={field.control} value={name} maxLength={35} onChange={(e) => setName(e.target.value)} />}
        </Field>
        {error && <p role="alert" style={field.error}>{error}</p>}
        <div style={s.row}>
          <DSButton type="submit" variant="primary" disabled={busy}>{busy ? 'Starting…' : 'Start setup'}</DSButton>
          <DSButton type="button" variant="ghost" onClick={onClose}>Cancel</DSButton>
        </div>
      </form>
    </Dialog>
  )
}

function AppCard({ app, kind, openSetup, onChanged }: { app: ChatApp; kind?: InboundKind; openSetup: boolean; onChanged: () => void }) {
  const [setupOpen, setSetupOpen] = useState(openSetup)
  const [report, setReport] = useState<InboundTestReport | 'testing' | null>(null)
  const [removing, setRemoving] = useState(false)
  const [error, setError] = useState('')
  const title = kind?.descriptor.title ?? app.kind
  // Until someone has paired, nothing shows that messages arrive.
  const unfinished = !app.credentials || app.people.length === 0
  const refused = Object.entries(app.refused ?? {}).sort((a, b) => b[1] - a[1])
  const state = !app.enabled ? 'Off' : unfinished ? 'Setup not finished' : app.lastAuthError ? 'Needs attention' : 'On'

  const act = async (what: () => Promise<unknown>) => {
    setError('')
    try { await what(); onChanged() } catch (e) { setError(describeInboundError(e)) }
  }
  const test = async () => {
    setReport('testing')
    try { setReport(await testInbound(app.id)) } catch (e) { setReport({ ok: false, error: describeInboundError(e) }) }
  }

  return (
    <li style={s.card} aria-label={`Chat app ${app.name}`}>
      <div style={s.row}>
        <strong style={{ fontSize: 14 }}>{app.name}</strong>
        <DSChip size="sm">{title}</DSChip>
        <DSChip size="sm" dot variant={state === 'On' ? 'success' : state === 'Off' ? 'neutral' : 'warning'}>{state}</DSChip>
      </div>
      <p style={s.quiet}>{app.receivedCount} received, {app.repliedCount} answered, {app.rejectedCount} refused.</p>
      {app.lastAuthError && <p role="alert" style={s.warn}>The last request refused: {app.lastAuthError}</p>}
      {refused.length > 0 && (
        <div style={s.quiet}>
          Refused in the last 24 hours:
          <ul aria-label="Messages refused in the last 24 hours" style={{ margin: '4px 0 0', paddingLeft: 18 }}>
            {refused.map(([code, n]) => <li key={code}>{n}: {refusalText(code)}</li>)}
          </ul>
        </div>
      )}
      <WhoAnswers app={app} onChanged={onChanged} />
      <div style={s.row}>
        {!setupOpen && <DSButton size="sm" variant={unfinished ? 'primary' : 'secondary'} onClick={() => setSetupOpen(true)}>{unfinished ? 'Continue setup' : 'Setup and pairing'}</DSButton>}
        {app.credentials && <DSButton size="sm" variant="secondary" onClick={test}>Test connection</DSButton>}
        <DSButton size="sm" variant="secondary" onClick={() => act(() => setInboundEnabled(app.id, !app.enabled))}>{app.enabled ? 'Disable' : 'Enable'}</DSButton>
        <DSButton size="sm" variant="ghost" onClick={() => setRemoving(true)}>Disconnect</DSButton>
      </div>
      {report === 'testing' && <p role="status" style={s.quiet}>Testing…</p>}
      {report && report !== 'testing' && (
        <p role="status" style={report.ok ? s.quiet : s.warn}>
          {report.ok ? 'The credentials work.' : `The test failed: ${report.error ?? ''}`}
          {Object.entries(report.details ?? {}).map(([k, v]) => ` ${k}: ${v}.`).join('')}
        </p>
      )}
      {error && <p role="alert" style={field.error}>{error}</p>}
      {setupOpen && kind && <ChannelSetup c={app} kind={kind} shared onChanged={onChanged} onClose={() => setSetupOpen(false)} />}
      {removing && (
        <Dialog title={`Disconnect ${app.name}?`} onClose={() => setRemoving(false)}>
          <p style={s.lead}>
            Every agent stops answering in it ({app.routes.length} now). The stored credentials are deleted. This cannot be undone.
          </p>
          <div style={s.row}>
            <DSButton variant="primary" onClick={() => { setRemoving(false); void act(() => deleteInbound(app.id)) }}>Disconnect</DSButton>
            <DSButton variant="ghost" onClick={() => setRemoving(false)}>Cancel</DSButton>
          </div>
        </Dialog>
      )}
    </li>
  )
}

const s: Record<string, CSSProperties> = {
  wrap: { flex: 1, overflowY: 'auto', padding: '16px 24px', display: 'flex', flexDirection: 'column', gap: 12, maxWidth: 820 },
  lead: { margin: 0, fontSize: 13, color: ds.textSecondary },
  quiet: { margin: 0, fontSize: 12, color: ds.textMuted },
  warn: { margin: 0, fontSize: 12, color: ds.warning },
  list: { listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 12 },
  card: { border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.xl, background: ds.surface, padding: 14, display: 'flex', flexDirection: 'column', gap: 10, minWidth: 0 },
  row: { display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' },
}
