/** Who answers where in a shared chat app, and (for administrators) who answers by default. */
import { useState, type CSSProperties } from 'react'
import { placesOf, setDefaultAgent, type ChatApp } from '../api/chatApps'
import { describeInboundError } from '../api/inbound'
import { canAdminEvals, useRoles } from '../lib/access'
import { ds } from '../lib/designSystem'
import { field } from './BuilderParts'

export function WhoAnswers({ app, onChanged }: { app: ChatApp; onChanged: () => void }) {
  const admin = canAdminEvals(useRoles())
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const places = placesOf(app)
  const key = (a: { namespace: string; agent: string }) => `${a.namespace}/${a.agent}`

  const change = async (place: string, value: string) => {
    const [ns, agent] = value.split('/')
    setBusy(true)
    setError('')
    try { await setDefaultAgent(app.id, place, ns, agent); onChanged() }
    catch (e) { setError(describeInboundError(e)) }
    finally { setBusy(false) }
  }

  if (places.length === 0) {
    return <p style={s.quiet}>No agent answers here yet. Add one from the Channels tab of a published agent.</p>
  }
  return (
    <div style={{ overflowX: 'auto' }}>
      <table style={s.table} aria-label={`Who answers where in ${app.name}`}>
        <thead><tr><th style={s.th}>Place</th><th style={s.th}>Answers by default</th><th style={s.th}>Also answers when named</th></tr></thead>
        <tbody>
          {places.map((p) => {
            const def = p.agents.find((a) => a.default)
            const others = p.agents.filter((a) => a !== def)
            return (
              <tr key={p.id}>
                <td style={s.td}>{p.label}</td>
                <td style={s.td}>
                  {admin && p.agents.length > 1 ? (
                    <select aria-label={`Default agent for ${p.label}`} style={field.control} disabled={busy}
                      value={def ? key(def) : ''} onChange={(e) => change(p.id, e.target.value)}>
                      {!def && <option value="">Choose…</option>}
                      {p.agents.map((a) => <option key={key(a)} value={key(a)}>{a.agent}</option>)}
                    </select>
                  ) : (def?.agent ?? 'Nobody')}
                </td>
                <td style={s.td}>{others.map((a) => a.agent).join(', ') || '—'}</td>
              </tr>
            )
          })}
        </tbody>
      </table>
      <p style={s.quiet}>
        To pick an agent, start the message with its name, for example “{places[0].agents[0].agent} what changed?”. A reply
        in a thread goes to the agent that answered there last; direct messages count as one thread. Messages anywhere
        else are not answered.
        {!admin && ' Only an administrator can change who answers by default.'}
      </p>
      {error && <p role="alert" style={field.error}>{error}</p>}
    </div>
  )
}

const s: Record<string, CSSProperties> = {
  table: { borderCollapse: 'collapse', width: '100%', fontSize: 13 },
  th: { textAlign: 'left', padding: '6px 10px 6px 0', borderBottom: '1px solid var(--ds-border)', fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.06em', color: ds.textMuted },
  td: { padding: '6px 10px 6px 0', borderBottom: '1px solid var(--ds-border)', color: ds.textPrimary, verticalAlign: 'middle' },
  quiet: { margin: '8px 0 0', fontSize: 12, color: ds.textMuted },
}
