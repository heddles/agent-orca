/**
 * AgentChatApps — on an agent's Channels tab: where it answers in each shared chat app, who may
 * ask it, and who may approve its actions from the chat. Several agents may share a place.
 */
import { useCallback, useEffect, useState, type CSSProperties } from 'react'
import {
  listChatApps, listPlatformPlaces, personLabel, placeLabel, removeAgentRoute, saveAgentRoute,
  type AppPlace, type AppRoute, type ChatApp, type PlatformPlace,
} from '../api/chatApps'
import { describeInboundError, type InboundKind } from '../api/inbound'
import { canWriteEvals, useRoles } from '../lib/access'
import { DESIGN, ds } from '../lib/designSystem'
import { DSButton, DSChip } from '../lib/primitives'
import { field } from './BuilderParts'
import { Pairing } from './ChannelSetup'
import { WhoAnswers } from './WhoAnswers'

export function AgentChatApps({ namespace, name, kinds }: { namespace: string; name: string; kinds: InboundKind[] }) {
  const [apps, setApps] = useState<ChatApp[]>([])
  const [error, setError] = useState('')
  const load = useCallback(() => {
    listChatApps()
      .then((a) => { setApps(a); setError('') })
      // Channels without shared apps still work, so only a real failure is shown.
      .catch((e: unknown) => { if ((e as { status?: number }).status !== 503) setError(describeInboundError(e)) })
  }, [])
  useEffect(() => { load() }, [load])

  if (apps.length === 0 && !error) return null
  return (
    <div style={s.wrap}>
      <h2 style={s.heading}>Shared chat apps</h2>
      {error && <p role="alert" style={field.error}>Could not load the chat apps: {error}</p>}
      {apps.map((a) => (
        <AppForAgent key={a.id} app={a} namespace={namespace} agent={name}
          title={kinds.find((k) => k.kind === a.kind)?.descriptor.title ?? a.kind} onChanged={load} />
      ))}
    </div>
  )
}

function AppForAgent({ app, namespace, agent, title, onChanged }: {
  app: ChatApp; namespace: string; agent: string; title: string; onChanged: () => void
}) {
  const builder = canWriteEvals(useRoles())
  const route = app.routes.find((r) => r.namespace === namespace && r.agent === agent)
  const [editing, setEditing] = useState(false)

  return (
    <section style={s.card} aria-label={`${agent} in ${app.name}`}>
      <div style={s.row}>
        <strong style={{ fontSize: 13 }}>{app.name}</strong>
        <DSChip size="sm">{title}</DSChip>
        <DSChip size="sm" dot variant={route?.places.length ? 'success' : 'neutral'}>{route?.places.length ? 'Answering' : 'Not answering'}</DSChip>
      </div>
      {!app.credentials && <p style={s.warn}>An administrator has not finished connecting this app.</p>}
      {route && !editing && (
        <>
          <p style={s.text}>
            Answers in: {route.places.map((p) => placeLabel(p) + (p.default ? '' : ' (when named)')).join(', ') || 'nowhere yet'}
          </p>
          <p style={s.text}>
            May ask: {route.allowSenders.map((id) => personLabel(app.people, id)).join(', ') || 'nobody yet'}. May approve in
            chat: {route.approvers.map((id) => personLabel(app.people, id)).join(', ') || 'nobody (approve in agent-orca)'}.
          </p>
        </>
      )}
      {builder && !editing && app.credentials && (
        <div><DSButton size="sm" variant={route ? 'secondary' : 'primary'} onClick={() => setEditing(true)}>{route ? 'Change' : `Answer in ${app.name}`}</DSButton></div>
      )}
      {editing && (
        <RouteEditor app={app} title={title} initial={route ?? { namespace, agent, places: [], allowSenders: [], approvers: [] }}
          exists={Boolean(route)} onDone={() => { setEditing(false); onChanged() }} onChanged={onChanged} />
      )}
      {app.routes.length > 0 && (
        <details>
          <summary style={s.summary}>Who answers where in {app.name}</summary>
          <WhoAnswers app={app} onChanged={onChanged} />
        </details>
      )}
    </section>
  )
}

function RouteEditor({ app, title, initial, exists, onDone, onChanged }: {
  app: ChatApp; title: string; initial: AppRoute; exists: boolean; onDone: () => void; onChanged: () => void
}) {
  const [places, setPlaces] = useState<AppPlace[]>(initial.places)
  const [allow, setAllow] = useState<string[]>(initial.allowSenders)
  const [approvers, setApprovers] = useState<string[]>(initial.approvers)
  const [channels, setChannels] = useState<PlatformPlace[] | null>(null)
  const [listError, setListError] = useState('')
  const [pairingOpen, setPairingOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  // A pairing adds a person (and the place) on the server: take what it added
  // without dropping what is being edited here.
  const savedPlaces = JSON.stringify(initial.places)
  const savedAllow = JSON.stringify(initial.allowSenders)
  const [seen, setSeen] = useState({ places: savedPlaces, allow: savedAllow })
  if (seen.places !== savedPlaces || seen.allow !== savedAllow) {
    const before = { places: JSON.parse(seen.places) as AppPlace[], allow: JSON.parse(seen.allow) as string[] }
    setSeen({ places: savedPlaces, allow: savedAllow })
    setPlaces((cur) => [...cur, ...initial.places.filter((p) => !before.places.some((o) => o.id === p.id) && !cur.some((c) => c.id === p.id))])
    setAllow((cur) => [...cur, ...initial.allowSenders.filter((id) => !before.allow.includes(id) && !cur.includes(id))])
  }

  const has = (id: string) => places.some((p) => p.id === id)
  const people = [...app.people.map((p) => p.id), ...allow.filter((id) => !app.people.some((p) => p.id === id))]
  const toggle = (list: string[], id: string, on: boolean) => (on ? [...list, id] : list.filter((x) => x !== id))

  const loadChannels = async () => {
    setListError('')
    try { setChannels(await listPlatformPlaces(app.id)) } catch (e) { setListError(describeInboundError(e)) }
  }
  const run = async (what: () => Promise<unknown>) => {
    setBusy(true)
    setError('')
    try { await what(); onDone() } catch (e) { setError(describeInboundError(e)); setBusy(false) }
  }
  const save = () => run(() => saveAgentRoute(app.id, {
    ...initial, places, allowSenders: allow, approvers: approvers.filter((id) => allow.includes(id)),
  }))
  const otherDefault = (id: string) =>
    app.routes.find((r) => (r.agent !== initial.agent || r.namespace !== initial.namespace) && r.places.some((p) => p.id === id && p.default))?.agent

  return (
    <div style={s.editor} role="group" aria-label={`Where ${initial.agent} answers in ${app.name}`}>
      <fieldset style={s.fieldset}>
        <legend style={s.legend}>Channels</legend>
        <div style={s.row}>
          {places.filter((p) => p.id !== '').map((p) => (
            <span key={p.id} style={s.token}>
              {placeLabel(p)}
              <button type="button" style={s.remove} aria-label={`Remove ${placeLabel(p)}`} onClick={() => setPlaces(places.filter((x) => x.id !== p.id))}>×</button>
            </span>
          ))}
          {places.every((p) => p.id === '') && <span style={s.quiet}>None yet</span>}
        </div>
        {app.canListPlaces && (channels === null
          ? <div><DSButton size="sm" variant="secondary" onClick={loadChannels}>Pick a channel</DSButton></div>
          : (
            <select aria-label="Add a channel" style={field.control} value=""
              onChange={(e) => {
                const c = channels.find((x) => x.id === e.target.value)
                if (c) setPlaces([...places, { id: c.id, name: c.name }])
              }}>
              <option value="">Add a channel…</option>
              {channels.filter((c) => !has(c.id)).map((c) => (
                <option key={c.id} value={c.id}>{c.name}{c.member ? '' : ' (invite the bot first)'}</option>
              ))}
            </select>
          ))}
        {listError && <p role="alert" style={field.error}>{listError} You can still add a channel by sending a code.</p>}
        {places.filter((p) => p.id !== '' && otherDefault(p.id)).map((p) => (
          <p key={p.id} style={s.quiet}>{otherDefault(p.id)} answers {placeLabel(p)} by default. Start a message with “{initial.agent}” to ask this agent.</p>
        ))}
      </fieldset>

      <label style={s.check}>
        <input type="checkbox" checked={has('')} onChange={(e) => setPlaces(e.target.checked ? [...places, { id: '' }] : places.filter((p) => p.id !== ''))} />
        Answer direct messages
        {otherDefault('') && <span style={s.quiet}>{otherDefault('')} answers them by default; this agent answers those that start with its name.</span>}
      </label>

      <fieldset style={s.fieldset}>
        <legend style={s.legend}>Who may ask, and who may approve its actions</legend>
        {people.length === 0 && <p style={s.quiet}>Nobody has paired with this app yet. Add a person by sending a code.</p>}
        {people.map((id) => {
          const label = personLabel(app.people, id)
          return (
            <div key={id} style={s.person}>
              <label style={s.check}>
                <input type="checkbox" checked={allow.includes(id)}
                  onChange={(e) => { setAllow(toggle(allow, id, e.target.checked)); if (!e.target.checked) setApprovers(approvers.filter((x) => x !== id)) }} />
                {label}
              </label>
              <label style={s.check}>
                <input type="checkbox" aria-label={`${label} may approve`} disabled={!allow.includes(id)} checked={approvers.includes(id)}
                  onChange={(e) => setApprovers(toggle(approvers, id, e.target.checked))} />
                may approve
              </label>
            </div>
          )
        })}
      </fieldset>

      {pairingOpen
        ? (
          <div style={s.pairing}>
            <Pairing c={app} title={title} onChanged={onChanged} route={{ namespace: initial.namespace, agent: initial.agent }}
              intro="Sending a code from a channel adds both that channel and the person who sent it to this agent. A direct message adds the person and direct messages. It is saved as soon as you approve." />
          </div>
        )
        : <div><DSButton size="sm" variant="secondary" onClick={() => setPairingOpen(true)}>Add a channel or person by sending a code</DSButton></div>}

      {error && <p role="alert" style={field.error}>{error}</p>}
      <div style={s.row}>
        <DSButton size="sm" variant="primary" disabled={busy} onClick={save}>Save</DSButton>
        <DSButton size="sm" variant="ghost" onClick={onDone}>Cancel</DSButton>
        {exists && <DSButton size="sm" variant="ghost" disabled={busy} onClick={() => run(() => removeAgentRoute(app.id, initial.namespace, initial.agent))}>Remove from {app.name}</DSButton>}
      </div>
      {exists && <p style={s.quiet}>If this agent answers somewhere by default and leaves, the agent that has been there longest takes over.</p>}
    </div>
  )
}

const s: Record<string, CSSProperties> = {
  wrap: { padding: '12px 24px 0', display: 'flex', flexDirection: 'column', gap: 10, maxWidth: 760 },
  heading: { margin: 0, fontSize: 11, fontWeight: 600, textTransform: 'uppercase', letterSpacing: '0.08em', color: ds.textSecondary },
  card: { border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, padding: 12, display: 'flex', flexDirection: 'column', gap: 8, minWidth: 0 },
  row: { display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' },
  text: { margin: 0, fontSize: 12, color: ds.textSecondary },
  quiet: { margin: 0, fontSize: 12, color: ds.textMuted },
  warn: { margin: 0, fontSize: 12, color: ds.warning },
  summary: { fontSize: 12, cursor: 'pointer', color: ds.textSecondary },
  editor: { display: 'flex', flexDirection: 'column', gap: 12, borderTop: '1px solid var(--ds-border)', paddingTop: 10 },
  fieldset: { border: 'none', padding: 0, margin: 0, display: 'flex', flexDirection: 'column', gap: 6, minWidth: 0 },
  legend: { fontSize: 13, fontWeight: 600, marginBottom: 4, color: ds.textPrimary },
  token: { display: 'inline-flex', alignItems: 'center', gap: 6, fontSize: 12, padding: '3px 10px', borderRadius: 999, background: 'var(--ds-muted-bg)', color: ds.textPrimary },
  remove: { background: 'none', border: 'none', cursor: 'pointer', color: ds.textSecondary, padding: 0, fontSize: 14 },
  check: { display: 'flex', gap: 6, alignItems: 'center', flexWrap: 'wrap', fontSize: 13, color: ds.textPrimary },
  person: { display: 'flex', gap: 18, alignItems: 'center', flexWrap: 'wrap', justifyContent: 'space-between' },
  pairing: { border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, padding: 10 },
}
