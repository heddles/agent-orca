/**
 * RequiredConnections — above an agent's run form: when the agent uses a service
 * that needs the runner's own connected account and they have none, say so and
 * link to Connections. Renders nothing otherwise, and nothing when connections are
 * not set up on the platform.
 */
import { useEffect, useState } from 'react'
import { Link } from 'react-router'
import { getRequiredConnections, type RequiredServer } from '../api/connections'
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

export function RequiredConnections({ required }: { required: RequiredServer[] }) {
  if (required.length === 0) return null
  return (
    <p role="alert" style={{ margin: '0 0 12px', fontSize: 13, color: ds.textPrimary }}>
      Connect {required.map((r) => r.server).join(', ')} to run this agent.{' '}
      <Link to={ROUTES.myConnections} style={{ color: ds.accent }}>Open Connections</Link>
    </p>
  )
}
