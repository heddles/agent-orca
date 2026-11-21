/**
 * RequiredConnections — above an agent's run form: when the agent uses a service
 * that needs the runner's own connected account and they have none, say so and
 * link to Connections. Renders nothing otherwise, and nothing when connections are
 * not set up on the platform.
 */
import { useEffect, useState } from 'react'
import { Link } from 'react-router'
import { getRequiredConnections, listConnections, type RequiredServer } from '../api/connections'
import { ds } from '../lib/designSystem'
import { ROUTES } from '../lib/routes'

/** The servers still to connect; empty while loading and on any failure. */
export function useRequiredConnections(namespace: string, agent: string): RequiredServer[] {
  const [required, setRequired] = useState<RequiredServer[]>([])
  useEffect(() => {
    let live = true
    getRequiredConnections(namespace, agent).then((r) => { if (live) setRequired(r.required) }).catch(() => { if (live) setRequired([]) })
    return () => { live = false }
  }, [namespace, agent])
  return required
}

/** "github as alice (invoker)" for each invoker-mode server the caller is connected to. Empty on any failure. */
export function useInvokerIdentities(namespace: string, agent: string): string[] {
  const [labels, setLabels] = useState<string[]>([])
  useEffect(() => {
    let live = true
    getRequiredConnections(namespace, agent)
      .then(async (r) => {
        const mine = r.servers.filter((s) => s.mode === 'invoker' && s.connected)
        if (mine.length === 0) return []
        const conns = await listConnections()
        return mine.map((s) => {
          const c = conns.find((x) => x.namespace === s.namespace && x.server === s.server)
          return `${s.server} as ${c?.accountLabel || 'you'} (invoker)`
        })
      })
      .then((l) => { if (live) setLabels(l) })
      .catch(() => { if (live) setLabels([]) })
    return () => { live = false }
  }, [namespace, agent])
  return labels
}

export function RequiredConnections({ required, identities = [] }: { required: RequiredServer[]; identities?: string[] }) {
  if (required.length === 0 && identities.length === 0) return null
  if (required.length === 0) {
    return <p style={{ margin: '0 0 12px', fontSize: 12, color: ds.textMuted }}>Runs use your account: {identities.join(', ')}.</p>
  }
  return (
    <p role="alert" style={{ margin: '0 0 12px', fontSize: 13, color: ds.textPrimary }}>
      Connect {required.map((r) => r.server).join(', ')} to run this agent.{' '}
      <Link to={ROUTES.myConnections} style={{ color: ds.accent }}>Open Connections</Link>
    </p>
  )
}
