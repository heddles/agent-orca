/**
 * ConnectionsView — "Connect your account": the MCP servers that act as a person
 * (connection mode maintainer), whether you are connected to each, and Connect
 * and Disconnect. The provider sends the browser back to /connections?status=…
 * after the flow; the page shows the outcome. Tokens never reach the browser.
 */
import { useCallback, useEffect, useState } from 'react'
import { useSearchParams } from 'react-router'
import {
  CONNECT_OUTCOMES, describeConnectionError, disconnect, listConnections, startConnection,
  type Connection,
} from '../api/connections'
import { listMCPServers, type MCPServerSummary } from '../api/sse'
import { canSee, useRoles } from '../lib/access'
import { ds } from '../lib/designSystem'
import { DSButton, DSChip } from '../lib/primitives'
import { EmptyState, Page, PageHeader } from '../lib/layout'
import { ICON } from '../lib/icons'

const key = (ns: string, name: string) => `${ns}/${name}`

export function ConnectionsView() {
  const roles = useRoles()
  const runner = roles === null || roles.includes('runner')
  const admin = canSee(roles, 'settings')
  const [params, setParams] = useSearchParams()
  const [servers, setServers] = useState<MCPServerSummary[] | null>(null)
  const [conns, setConns] = useState<Connection[]>([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState('')

  const load = useCallback(() => {
    let live = true
    Promise.all([listMCPServers(''), listConnections()])
      .then(([s, c]) => { if (live) { setServers(s.filter((m) => m.connectionMode === 'maintainer')); setConns(c); setError('') } })
      .catch((e: unknown) => { if (live) { setError(describeConnectionError(e)); setServers((old) => old ?? []) } })
    return () => { live = false }
  }, [])
  useEffect(() => load(), [load])

  const outcome = CONNECT_OUTCOMES[params.get('status') ?? '']
  const connect = async (m: MCPServerSummary) => {
    setBusy(key(m.namespace, m.name)); setError('')
    try {
      const { authorizeUrl } = await startConnection(m.namespace, m.name)
      window.location.assign(authorizeUrl)
    } catch (e) { setError(describeConnectionError(e)); setBusy('') }
  }
  const remove = async (m: MCPServerSummary) => {
    setBusy(key(m.namespace, m.name)); setError('')
    try { await disconnect(m.namespace, m.name); load() } catch (e) { setError(describeConnectionError(e)) }
    setBusy('')
  }

  return (
    <Page>
      <PageHeader title="Connections" description="Let an agent act on your own account at another service, such as your files or your repositories." />
      {outcome && (
        <p role={outcome.ok ? 'status' : 'alert'} style={{ margin: '0 24px 12px', fontSize: 13, color: outcome.ok ? 'var(--ds-success)' : 'var(--ds-error, #b00020)' }}>
          {outcome.text}{' '}
          <button type="button" style={linkBtn} onClick={() => setParams({}, { replace: true })}>Dismiss</button>
        </p>
      )}
      {error && <p role="alert" style={{ margin: '0 24px 12px', fontSize: 13, color: 'var(--ds-error, #b00020)' }}>{error}</p>}
      {servers === null && <div role="status" style={{ padding: 24, color: ds.textMuted, fontSize: 13 }}>Loading connections…</div>}
      {servers?.length === 0 && !error && (
        <EmptyState icon={ICON.tool} title="No services to connect yet">
          {admin ? 'Set a server to connection mode maintainer under Settings, Connections, and register its OAuth app.' : 'An administrator has not set up any service that uses your own account.'}
        </EmptyState>
      )}
      <ul style={{ listStyle: 'none', margin: 0, padding: '0 24px', display: 'flex', flexDirection: 'column', gap: 10, maxWidth: 760 }}>
        {servers?.map((m) => {
          const c = conns.find((x) => x.namespace === m.namespace && x.server === m.name)
          return (
            <li key={key(m.namespace, m.name)} aria-label={`Connection ${m.name}`} style={card}>
              <div style={row}>
                <strong style={{ fontSize: 13 }}>{m.name}</strong>
                {c ? <ConnectionChip status={c.status} /> : <DSChip size="sm" variant="neutral">Not connected</DSChip>}
              </div>
              {c && <p style={muted}>Connected as {c.accountLabel || 'you'}. Access: {c.scopes.length ? c.scopes.join(', ') : 'as granted by the service'}.{c.lastUsedAt ? ` Last used ${new Date(c.lastUsedAt).toLocaleString()}.` : ''}</p>}
              {c && c.status !== 'active' && <p style={muted}>This connection needs to be renewed. Connect again to continue.</p>}
              <p style={muted}>An agent you publish that uses this service acts as you for everyone who runs it.</p>
              {runner && (
                <div style={row}>
                  <DSButton size="sm" variant="primary" disabled={busy === key(m.namespace, m.name)} onClick={() => connect(m)}>
                    {c ? 'Connect again' : `Connect ${m.name}`}
                  </DSButton>
                  {c && <DSButton size="sm" variant="ghost" disabled={busy === key(m.namespace, m.name)} onClick={() => remove(m)}>Disconnect</DSButton>}
                </div>
              )}
            </li>
          )
        })}
      </ul>
    </Page>
  )
}

export function ConnectionChip({ status }: { status: string }) {
  if (status === 'active') return <DSChip size="sm" variant="success" dot>Connected</DSChip>
  if (status === 'needs_reauth') return <DSChip size="sm" variant="warning" dot>Needs renewing</DSChip>
  return <DSChip size="sm" variant="neutral" dot>Revoked</DSChip>
}

const row = { display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' } as const
const muted = { margin: 0, fontSize: 12, color: ds.textMuted } as const
const card = { border: '1px solid var(--ds-border)', borderRadius: 8, padding: 12, display: 'flex', flexDirection: 'column', gap: 8 } as const
const linkBtn = { background: 'none', border: 'none', color: ds.accent, cursor: 'pointer', fontSize: 12 } as const
