/** How a run's agent was picked, when the request named none, and asking another agent instead. */
import { useEffect, useState, type CSSProperties } from 'react'
import { getCatalog, startAgentRun, type CatalogEntry } from '../api/catalog'
import type { RunRouting } from '../api/sse'
import { DESIGN, ds } from '../lib/designSystem'
import { DSButton } from '../lib/primitives'
import { parseRunInput } from '../lib/runText'
import { field } from './BuilderParts'

const percent = (p: number) => `${Math.round(p * 100)}%`

/** One sentence on why this agent has the run. */
export function routingSentence(agent: string, r: RunRouting): string {
  const sure = r.confidence ? ` (${percent(r.confidence)} sure)` : ''
  switch (r.method) {
    case 'fallback':
      return `No agent clearly fitted the request, so it went to the default agent, ${agent}.`
    case 'only':
      return `${agent} was the only agent there was to pick.`
    case 'sticky':
      return `${agent} already had this conversation.`
    default:
      return r.reason === 'no_default'
        ? `Routed to ${agent}${sure}. There is no default agent for unclear requests.`
        : `Routed to ${agent}${sure}.`
  }
}

interface Props {
  agent: string
  namespace: string
  routing: RunRouting
  input: string
  onNavigateToRun?: (runName: string, namespace: string) => void
}

export function RoutedNote({ agent, namespace, routing, input, onNavigateToRun }: Props) {
  const others = (routing.considered ?? []).filter((c) => c.agent !== agent).slice(0, 3)
  // A chat run's input carries the channel's wrapper, which is not a request to send again.
  const canAskAnother = Boolean(onNavigateToRun && input && !parseRunInput(input).channel)
  const [open, setOpen] = useState(false)

  return (
    <section aria-label="How the agent was picked" style={s.panel}>
      <p style={s.text}>
        {routingSentence(agent, routing)}
        {others.length > 0 && ` Also considered: ${others.map((c) => `${c.agent} ${percent(c.probability)}`).join(', ')}.`}
      </p>
      {canAskAnother && !open && <DSButton size="sm" variant="ghost" onClick={() => setOpen(true)}>Ask another agent</DSButton>}
      {open && (
        <AskAnother agent={agent} namespace={namespace} preferred={others.map((c) => c.agent)} input={input}
          onStarted={onNavigateToRun!} />
      )}
    </section>
  )
}

function AskAnother({ agent, namespace, preferred, input, onStarted }: {
  agent: string
  namespace: string
  preferred: string[]
  input: string
  onStarted: (runName: string, namespace: string) => void
}) {
  const [agents, setAgents] = useState<CatalogEntry[] | null>(null)
  const [chosen, setChosen] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const key = (a: CatalogEntry) => `${a.namespace}/${a.name}`

  useEffect(() => {
    let live = true
    const rank = (a: CatalogEntry) => { const i = preferred.indexOf(a.name); return i < 0 ? preferred.length : i }
    getCatalog('')
      .then((list) => {
        if (live) {
          setAgents(list.filter((a) => !a.template && !(a.name === agent && a.namespace === namespace))
            .sort((a, b) => rank(a) - rank(b) || a.name.localeCompare(b.name)))
        }
      })
      .catch((e: unknown) => { if (live) setError(e instanceof Error ? e.message : String(e)) })
    return () => { live = false }
    // The list is read once, when it is opened.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agent, namespace])

  if (error && !agents) return <p role="alert" style={field.error}>{error}</p>
  if (!agents) return <p role="status" style={s.text}>Loading agents…</p>
  const target = agents.find((a) => key(a) === chosen) ?? agents[0]
  if (!target) return <p style={s.text}>There is no other agent to ask.</p>

  const send = async () => {
    setBusy(true)
    setError('')
    try { onStarted((await startAgentRun(target.namespace, target.name, input)).run_id, target.namespace) }
    catch (e) { setError(e instanceof Error ? e.message : String(e)); setBusy(false) }
  }

  return (
    <div style={s.row}>
      <select aria-label="Agent to ask instead" style={{ ...field.control, width: 'auto' }} value={key(target)}
        onChange={(e) => setChosen(e.target.value)}>
        {agents.map((a) => <option key={key(a)} value={key(a)}>{a.name}</option>)}
      </select>
      <DSButton size="sm" variant="primary" disabled={busy} onClick={send}>{busy ? 'Starting…' : 'Send the same request'}</DSButton>
      {error && <p role="alert" style={field.error}>{error}</p>}
    </div>
  )
}

const s: Record<string, CSSProperties> = {
  panel: {
    background: ds.surface, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.lg, padding: '10px 16px',
    display: 'flex', gap: 10, alignItems: 'center', flexWrap: 'wrap', flexShrink: 0,
  },
  text: { margin: 0, fontSize: 13, color: ds.textSecondary },
  row: { display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' },
}
