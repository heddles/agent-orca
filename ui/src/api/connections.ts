/**
 * Client for per-user OAuth connections to MCP servers (docs/mcp-oauth.md).
 * Listing needs the viewer role, connecting and disconnecting the runner role,
 * and registering the OAuth app and seeing who is connected the admin role.
 * Nothing here ever receives a token, a client secret or an authorization code:
 * the secret is sent once and is not returned.
 */
import { failure } from './builder'
import { apiFetch } from './sse'

const enc = encodeURIComponent
const JSON_HEADERS = { 'Content-Type': 'application/json' }

export type ConnectionStatus = 'active' | 'needs_reauth' | 'revoked' | 'expired'

export interface Connection {
  namespace: string
  server: string
  scopes: string[]
  accountLabel?: string
  status: ConnectionStatus
  lastUsedAt?: string
  createdAt: string
}

export interface OAuthApp {
  namespace: string
  appRef: string
  registered: boolean
  clientId?: string
  hasClientSecret: boolean
  /** The callback URL to register in the provider's app. */
  redirectUri: string
  scopesAllowed: string[]
  updatedBy?: string
  updatedAt?: string
}

export interface ServerConnection {
  userId: string
  accountLabel?: string
  status: ConnectionStatus
  scopes: string[]
  lastUsedAt?: string
  createdAt: string
}

export interface RequiredServer {
  namespace: string
  server: string
  mode: 'maintainer' | 'invoker' | string
  connected: boolean
}

async function send<T>(url: string, method: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method }
  if (body !== undefined) { init.headers = JSON_HEADERS; init.body = JSON.stringify(body) }
  const res = await apiFetch(url, init)
  if (!res.ok) throw await failure(res)
  if (res.status === 204) return undefined as T
  return res.json()
}

export async function listConnections(): Promise<Connection[]> {
  return (await send<Connection[] | null>('/api/connections', 'GET')) ?? []
}

/** Starts the flow; the caller sends the browser to authorizeUrl. */
export function startConnection(namespace: string, server: string, scopes?: string[]) {
  return send<{ authorizeUrl: string }>(`/api/connections/${enc(namespace)}/${enc(server)}/start`, 'POST', scopes?.length ? { scopes } : {})
}

export function disconnect(namespace: string, server: string, userId?: string) {
  const q = userId ? `?user=${enc(userId)}` : ''
  return send<void>(`/api/connections/${enc(namespace)}/${enc(server)}${q}`, 'DELETE')
}

export function getOAuthApp(namespace: string, server: string) {
  return send<OAuthApp>(`/api/mcp-oauth-apps/${enc(namespace)}/${enc(server)}`, 'GET')
}

export interface OAuthAppInput { clientId: string; clientSecret?: string; scopesAllowed: string[] }

export function saveOAuthApp(namespace: string, server: string, input: OAuthAppInput) {
  return send<OAuthApp>(`/api/mcp-oauth-apps/${enc(namespace)}/${enc(server)}`, 'PUT', input)
}

export async function listServerConnections(namespace: string, server: string) {
  const r = await send<{ connections: ServerConnection[] | null; counts: Record<string, number> | null }>(
    `/api/mcpservers/${enc(namespace)}/${enc(server)}/connections`, 'GET')
  return { connections: r.connections ?? [], counts: r.counts ?? {} }
}

export async function getRequiredConnections(namespace: string, agent: string) {
  const r = await send<{ servers: RequiredServer[] | null; required: RequiredServer[] | null; ownerNeedsReconnect?: RequiredServer[] | null }>(
    `/api/me/connections/required?agent=${enc(`${namespace}/${agent}`)}`, 'GET')
  return { servers: r.servers ?? [], required: r.required ?? [], ownerNeedsReconnect: r.ownerNeedsReconnect ?? [] }
}

/** A readable sentence for a failed call, by status. */
export function describeConnectionError(e: unknown): string {
  if (!(e instanceof Error)) return String(e)
  switch ((e as { status?: number }).status) {
    case 403: return `You are not allowed to do this, or you are not signed in as a user. ${e.message}`.trim()
    case 404: return 'That server was not found. It may have been deleted, or it is in a workspace you cannot use.'
    case 409: return e.message || 'That conflicts with how the server is set up.'
    case 502: return e.message || 'The provider could not be reached. Try again in a moment.'
    case 503: return 'Connecting accounts is not set up on this platform. It needs the PostgreSQL store and a connection key.'
    default: return e.message
  }
}

/** The sentences the callback page shows for ?status=. */
export const CONNECT_OUTCOMES: Record<string, { ok: boolean; text: string }> = {
  connected: { ok: true, text: 'Your account is connected.' },
  denied: { ok: false, text: 'The provider did not grant access, so nothing was connected.' },
  expired: { ok: false, text: 'That connection request was not found, expired or was already used. Start again.' },
  scopes: { ok: false, text: 'The provider granted more access than the administrator allows for this server, so the connection was refused.' },
  failed: { ok: false, text: 'The connection could not be completed. Start again, and ask an administrator if it keeps failing.' },
}

/** The question a run asks when a tool call needed the user's own connection (router: connectionRequired). */
export const CONNECTION_QUESTION_PREFIX = 'This agent needs your '
export function needsConnection(question?: string): boolean {
  return !!question && question.startsWith(CONNECTION_QUESTION_PREFIX)
}
