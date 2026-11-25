/**
 * ChannelSetup — the guided setup of a chat channel: the address the platform
 * must reach, creating the app there, its credentials (administrators only,
 * stored encrypted and never shown again), and a test message carrying a
 * one-time code that adds the sender to the channel. Each step says what is
 * missing, and the test says which part is failing.
 */
import { useEffect, useState, type CSSProperties, type ReactNode } from 'react'
import {
  approvePairing, createPairing, describeInboundError, discardPairing, getPairing, inboundUrl, setInboundCredentials,
  setupSteps, type ApprovedPairing, type InboundConnection, type InboundKind, type InboundPairing,
} from '../api/inbound'
import { canAdminEvals, useRoles } from '../lib/access'
import { DESIGN, ds } from '../lib/designSystem'
import { DSButton } from '../lib/primitives'
import { CopyButton } from './AgentTriggers'
import { Field, field } from './BuilderParts'

const POLL_MS = 3000
/** How long to wait before saying what might be wrong. */
const PATIENCE_MS = 20000

interface Props {
  c: InboundConnection
  kind: InboundKind
  /** Reads the connection again: credentials and live status come from it. */
  onChanged: () => void
  onClose: () => void
  /** For tests: how often to ask, and how long before explaining a silence. */
  pollMs?: number
  patienceMs?: number
}

export function ChannelSetup({ c, kind, onChanged, onClose, pollMs = POLL_MS, patienceMs = PATIENCE_MS }: Props) {
  const title = kind.descriptor.title
  const url = c.url ?? inboundUrl(c.path)
  const [appMade, setAppMade] = useState(Boolean(c.credentials))
  const ready = Boolean(c.credentials)

  return (
    <div style={s.wrap} role="group" aria-label={`Set up ${c.name}`}>
      <div style={s.between}>
        <strong style={{ fontSize: 13 }}>Set up {title}</strong>
        <DSButton size="sm" variant="ghost" onClick={onClose}>Close</DSButton>
      </div>
      <ol style={s.steps}>
        <Step n={1} title="Check the public address" done={Boolean(c.url)}>
          <p style={s.text}>{title} must be able to reach this address.</p>
          <div style={s.row}><code style={s.code}>{url}</code><CopyButton text={url} label="Copy the receive address" /></div>
          {!c.url && (
            <p style={s.warn}>
              No public address is configured, so this is the one you are browsing. If {title} cannot reach it, ask an
              administrator to set <code>operator.inbound.publicURL</code>.
            </p>
          )}
        </Step>
        <Step n={2} title={`Create the ${title} app`} done={ready || appMade}>
          {c.setupLink ? (
            <>
              <p style={s.text}>
                This opens {title} with the app&rsquo;s permissions, events and address filled in. Choose the workspace, create
                the app, then install it. {title} may say the address is not verified yet; that is expected until the next
                step is saved.
              </p>
              <div style={s.row}>
                <a href={c.setupLink} target="_blank" rel="noreferrer" style={s.linkButton}>Open {title} with settings filled in</a>
              </div>
              <details>
                <summary style={s.summary}>Do it by hand instead</summary>
                <Steps kind={kind} url={url} />
              </details>
            </>
          ) : <Steps kind={kind} url={url} />}
          {!ready && !appMade && <div><DSButton size="sm" variant="primary" onClick={() => setAppMade(true)}>I created the app</DSButton></div>}
        </Step>
        <Step n={3} title="Add the credentials" done={ready} locked={!ready && !appMade}>
          <Credentials c={c} kind={kind} onSaved={onChanged} />
        </Step>
        <Step n={4} title="Send a test message" done={false} locked={!ready}>
          <Pairing c={c} title={title} onChanged={onChanged} pollMs={pollMs} patienceMs={patienceMs} />
        </Step>
      </ol>
    </div>
  )
}

function Step({ n, title, done, locked = false, children }: { n: number; title: string; done: boolean; locked?: boolean; children: ReactNode }) {
  return (
    <li style={{ ...s.step, ...(locked ? s.locked : {}) }} aria-label={`Step ${n}: ${title}`}>
      <div style={s.row}>
        <span style={{ ...s.num, ...(done ? s.numDone : {}) }} aria-hidden="true">{done ? '✓' : n}</span>
        <span style={s.stepTitle}>{title}{done && <span style={s.srOnly}> (done)</span>}</span>
      </div>
      {!locked && children}
    </li>
  )
}

function Steps({ kind, url }: { kind: InboundKind; url: string }) {
  return (
    <ol style={s.platformSteps} aria-label={`Steps on ${kind.descriptor.title}`}>
      {setupSteps(kind, url).map((t, i) => <li key={i}>{t}</li>)}
    </ol>
  )
}

function Credentials({ c, kind, onSaved }: { c: InboundConnection; kind: InboundKind; onSaved: () => void }) {
  const admin = canAdminEvals(useRoles())
  const keys = kind.descriptor.secretKeys ?? []
  const [replacing, setReplacing] = useState(false)
  const [values, setValues] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  if (c.credentials && !replacing) {
    return (
      <div style={s.row}>
        <p style={s.ok}>{c.credentials === 'stored' ? 'Stored encrypted. Nobody can read them back.' : `Read from the Secret ${c.secretRef}.`}</p>
        {admin && c.canStoreCredentials && <DSButton size="sm" variant="ghost" onClick={() => setReplacing(true)}>Replace credentials</DSButton>}
      </div>
    )
  }
  if (!admin) return <p style={s.text}>An administrator adds the credentials. Ask one to finish this step; this channel is saved.</p>
  if (!c.canStoreCredentials) {
    return (
      <p style={s.warn}>
        This installation cannot store credentials yet: its encryption key is not set up
        (<code>database.connectionKeySecret</code>). You can name a Kubernetes Secret on the channel instead.
      </p>
    )
  }
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const missing = keys.find((k) => k.required && !values[k.key]?.trim())
    if (missing) { setError(`${missing.key} is required.`); return }
    setBusy(true)
    setError('')
    try {
      await setInboundCredentials(c.id, Object.fromEntries(Object.entries(values).filter(([, v]) => v.trim())))
      setValues({})
      setReplacing(false)
      onSaved()
    } catch (err) {
      setError(describeInboundError(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form onSubmit={submit} noValidate style={s.form} aria-label="Channel credentials">
      {keys.map((k) => (
        <Field key={k.key} label={k.key + (k.required ? '' : ' (optional)')} help={k.help}>
          {(p) => (
            <input {...p} type="password" autoComplete="off" spellCheck={false} style={field.control}
              value={values[k.key] ?? ''} onChange={(e) => setValues((v) => ({ ...v, [k.key]: e.target.value }))} />
          )}
        </Field>
      ))}
      <p style={s.text}>Stored encrypted. They are checked with {kind.descriptor.title} where it allows, and never shown again.</p>
      {error && <p role="alert" style={field.error}>{error}</p>}
      <div style={s.row}>
        <DSButton type="submit" size="sm" variant="primary" disabled={busy}>{busy ? 'Checking…' : 'Save credentials'}</DSButton>
        {replacing && <DSButton type="button" size="sm" variant="ghost" onClick={() => { setReplacing(false); setValues({}); setError('') }}>Keep the current ones</DSButton>}
      </div>
    </form>
  )
}

/** Why a code has not arrived yet, from what the channel has seen since the code was made. */
function silence(c: InboundConnection, since: number, title: string): string {
  const at = (iso?: string) => (iso ? new Date(iso).getTime() : 0)
  // A little slack: the browser's clock and the server's need not agree.
  const from = since - 30000
  if (c.lastAuthError && at(c.lastAuthErrorAt) >= from) {
    return `A request arrived but was refused: ${c.lastAuthError}. Check that the credentials are this app's, then try again.`
  }
  if (at(c.lastRequestAt) >= from) {
    return `${title} is reaching agent-orca, but no message carried the code. Send exactly the line above, to the bot, from the workspace the credentials belong to.`
  }
  return `No request from ${title} has reached the address. Check that event delivery is on and the address is verified in ${title}, and that it can be reached from there.`
}

function Pairing({ c, title, onChanged, pollMs, patienceMs }: {
  c: InboundConnection; title: string; onChanged: () => void; pollMs: number; patienceMs: number
}) {
  const [pairing, setPairing] = useState<InboundPairing | null>(null)
  const [madeAt, setMadeAt] = useState(0)
  const [slow, setSlow] = useState(false)
  const [done, setDone] = useState<ApprovedPairing | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const waiting = pairing?.status === 'waiting'

  useEffect(() => {
    if (!pairing || !waiting) return
    let live = true
    const id = setInterval(() => {
      getPairing(c.id, pairing.id)
        .then((p) => { if (live) { setPairing((old) => (old ? { ...old, ...p, code: old.code } : old)); setError('') } })
        .catch((e: unknown) => {
          if (!live) return
          if ((e as { status?: number }).status === 404) { setPairing(null); setError('That code expired. Get a new one.') }
          else setError(describeInboundError(e))
        })
      if (Date.now() - madeAt >= patienceMs) { setSlow(true); onChanged() }
    }, pollMs)
    return () => { live = false; clearInterval(id) }
  }, [c.id, pairing?.id, waiting, madeAt, pollMs, patienceMs]) // eslint-disable-line react-hooks/exhaustive-deps

  const run = async (what: () => Promise<void>) => {
    setBusy(true)
    setError('')
    try { await what() } catch (e) { setError(describeInboundError(e)) } finally { setBusy(false) }
  }
  const start = () => run(async () => {
    setDone(null)
    setSlow(false)
    setPairing(await createPairing(c.id))
    setMadeAt(Date.now())
  })
  const approve = () => run(async () => {
    setDone(await approvePairing(c.id, pairing!.id))
    setPairing(null)
    onChanged()
  })
  const reject = () => run(async () => {
    await discardPairing(c.id, pairing!.id)
    setPairing(null)
  })
  const who = pairing?.senderName ? `${pairing.senderName} (${pairing.senderId})` : pairing?.senderId

  return (
    <div style={s.form}>
      <p style={s.text}>
        Sending a one-time code from {title} shows that messages arrive, and adds you to the people who may write.
      </p>
      {done && (
        <p role="status" style={done.replySent ? s.ok : s.warn}>
          {done.replySent
            ? 'Connected. The sender is allowed, and the bot replied to them.'
            : `The sender is allowed, but the bot could not reply: ${done.replyError ?? 'unknown error'}`}
        </p>
      )}
      {pairing && waiting && (
        <>
          <p style={s.text}>Send this to the bot, in a direct message or by mentioning it in a channel. It works once, for 10 minutes.</p>
          <div style={s.row}><code style={s.code} aria-label="Pairing code">pair {pairing.code}</code><CopyButton text={`pair ${pairing.code}`} label="Copy the pairing message" /></div>
          <p role="status" style={s.text}>Waiting for your message…</p>
          {slow && <p role="alert" style={s.warn}>{silence(c, madeAt, title)}</p>}
          <div><DSButton size="sm" variant="ghost" disabled={busy} onClick={reject}>Cancel</DSButton></div>
        </>
      )}
      {pairing && !waiting && (
        <div style={s.confirm}>
          <p style={s.text}>
            <strong>{who}</strong> sent the code {pairing.direct ? 'in a direct message' : `from channel ${pairing.channelId}`}. Allow
            this person?{!pairing.direct && ' If this channel limits where it answers, that channel is added too.'}
          </p>
          <div style={s.row}>
            <DSButton size="sm" variant="primary" disabled={busy} onClick={approve}>Allow this person</DSButton>
            <DSButton size="sm" variant="ghost" disabled={busy} onClick={reject}>Not who I expected</DSButton>
          </div>
        </div>
      )}
      {error && <p role="alert" style={field.error}>{error}</p>}
      {!pairing && <div><DSButton size="sm" variant={done ? 'secondary' : 'primary'} disabled={busy} onClick={start}>{done ? 'Add another person' : 'Get a code'}</DSButton></div>}
    </div>
  )
}

const s: Record<string, CSSProperties> = {
  wrap: { display: 'flex', flexDirection: 'column', gap: 10, borderTop: '1px solid var(--ds-border)', paddingTop: 10 },
  between: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 8 },
  steps: { listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 10 },
  step: { border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, padding: 12, display: 'flex', flexDirection: 'column', gap: 8, minWidth: 0 },
  locked: { opacity: 0.55 },
  row: { display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' },
  num: { width: 22, height: 22, borderRadius: '50%', border: '1px solid var(--ds-border)', display: 'inline-flex', alignItems: 'center', justifyContent: 'center', fontSize: 12, fontWeight: 700, flexShrink: 0 },
  numDone: { color: ds.success, border: `1px solid ${ds.success}` },
  stepTitle: { fontSize: 13, fontWeight: 600, color: ds.textPrimary },
  text: { margin: 0, fontSize: 12, color: ds.textSecondary },
  warn: { margin: 0, fontSize: 12, color: ds.warning },
  ok: { margin: 0, fontSize: 12, color: ds.success },
  summary: { fontSize: 12, cursor: 'pointer', color: ds.textSecondary },
  platformSteps: { margin: '6px 0 0', paddingLeft: 18, fontSize: 12, color: ds.textSecondary },
  code: { fontFamily: DESIGN.font.mono, fontSize: 12, background: 'var(--ds-muted-bg)', padding: '4px 8px', borderRadius: DESIGN.radii.md, wordBreak: 'break-all' },
  linkButton: { fontSize: 12, fontWeight: 600, color: '#fff', background: ds.accent, padding: '6px 12px', borderRadius: DESIGN.radii.md, textDecoration: 'none' },
  form: { display: 'flex', flexDirection: 'column', gap: 8 },
  confirm: { display: 'flex', flexDirection: 'column', gap: 8, border: `1px solid ${ds.warning}`, borderRadius: DESIGN.radii.md, padding: 10 },
  srOnly: { position: 'absolute', width: 1, height: 1, overflow: 'hidden', clip: 'rect(0 0 0 0)', whiteSpace: 'nowrap' },
}
