/**
 * McpOAuthAdmin — for an admin, on a per-user MCP server (connection mode
 * maintainer): register or rotate the OAuth app (the client secret is write-only
 * and never shown again), see the callback URL to register at the provider, and
 * see who is connected and revoke a connection. Never shows a token.
 */
import { useCallback, useEffect, useState } from 'react'
import {
  describeConnectionError, disconnect, getOAuthApp, listServerConnections, saveOAuthApp,
  type OAuthApp, type ServerConnection,
} from '../api/connections'
import { ds } from '../lib/designSystem'
import { DSButton } from '../lib/primitives'
import { Field, field } from './BuilderParts'
import { ConnectionChip } from './ConnectionsView'

export function McpOAuthAdmin({ namespace, name }: { namespace: string; name: string }) {
  const [app, setApp] = useState<OAuthApp | null>(null)
  const [people, setPeople] = useState<ServerConnection[]>([])
  const [error, setError] = useState('')
  const [clientId, setClientId] = useState('')
  const [secret, setSecret] = useState('')
  const [scopes, setScopes] = useState('')
  const [saving, setSaving] = useState(false)

  const load = useCallback(() => {
    let live = true
    Promise.all([getOAuthApp(namespace, name), listServerConnections(namespace, name)])
      .then(([a, c]) => {
        if (!live) return
        setApp(a); setPeople(c.connections); setError('')
        setClientId(a.clientId ?? ''); setScopes(a.scopesAllowed.join(' '))
      })
      .catch((e: unknown) => { if (live) setError(describeConnectionError(e)) })
    return () => { live = false }
  }, [namespace, name])
  useEffect(() => load(), [load])

  const save = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!clientId.trim()) { setError('Enter the client ID from the provider.'); return }
    setSaving(true); setError('')
    try {
      const a = await saveOAuthApp(namespace, name, { clientId: clientId.trim(), clientSecret: secret || undefined, scopesAllowed: scopes.split(/[\s,]+/).filter(Boolean) })
      setApp(a); setSecret('')
    } catch (err) { setError(describeConnectionError(err)) }
    setSaving(false)
  }
  const revoke = async (p: ServerConnection) => {
    try { await disconnect(namespace, name, p.userId); load() } catch (err) { setError(describeConnectionError(err)) }
  }

  return (
    <section aria-label="OAuth app and connections" style={{ display: 'flex', flexDirection: 'column', gap: 10, marginTop: 12 }}>
      <h3 style={{ margin: 0, fontSize: 13 }}>OAuth app</h3>
      {error && <p role="alert" style={field.error}>{error}</p>}
      {app && (
        <p style={{ margin: 0, fontSize: 12, color: ds.textSecondary }}>
          Create an OAuth app at the provider and give it this redirect URL: <code>{app.redirectUri}</code>.{' '}
          {app.registered ? `Registered${app.updatedBy ? ` by ${app.updatedBy}` : ''}. The client secret is ${app.hasClientSecret ? 'stored' : 'not set'} and is never shown.` : 'Not registered yet: nobody can connect.'}
        </p>
      )}
      <form onSubmit={save} noValidate style={{ display: 'flex', flexDirection: 'column', gap: 8, maxWidth: 480 }}>
        <Field label="Client ID">{(p) => <input {...p} style={field.control} value={clientId} onChange={(e) => setClientId(e.target.value)} />}</Field>
        <Field label="Client secret" help="Write-only. Leave empty to keep the stored secret.">
          {(p) => <input {...p} type="password" autoComplete="off" style={field.control} value={secret} onChange={(e) => setSecret(e.target.value)} />}
        </Field>
        <Field label="Scopes users may grant" help="Space separated. Empty allows what the server's userScopes allows.">
          {(p) => <input {...p} style={field.control} value={scopes} onChange={(e) => setScopes(e.target.value)} />}
        </Field>
        <div><DSButton type="submit" variant="primary" disabled={saving}>{saving ? 'Saving…' : app?.registered ? 'Update app' : 'Register app'}</DSButton></div>
      </form>
      <h3 style={{ margin: '8px 0 0', fontSize: 13 }}>Connected users</h3>
      {people.length === 0 && <p style={{ margin: 0, fontSize: 12, color: ds.textMuted }}>Nobody is connected.</p>}
      <ul style={{ listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 6 }}>
        {people.map((p) => (
          <li key={p.userId} style={{ display: 'flex', gap: 8, alignItems: 'center', fontSize: 12 }} aria-label={`Connection of ${p.userId}`}>
            <span>{p.userId}</span><ConnectionChip status={p.status} />
            <span style={{ color: ds.textMuted }}>{p.lastUsedAt ? `used ${new Date(p.lastUsedAt).toLocaleDateString()}` : 'never used'}</span>
            <DSButton size="sm" variant="ghost" onClick={() => revoke(p)}>Revoke</DSButton>
          </li>
        ))}
      </ul>
    </section>
  )
}
