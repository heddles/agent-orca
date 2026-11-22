/**
 * Client for inbound channel connections (docs/inbound-channels.md): a mailbox, and later chat
 * platforms, that start a run of a published agent from a message and send the result back.
 * Listing needs the viewer role; creating, changing, testing and deleting the builder role. A
 * connection never carries a credential: it names a Secret an administrator prepared.
 */
import { failure } from './builder'
import { apiFetch } from './sse'

export interface InboundConnection {
  id: string
  namespace: string
  agent: string
  name: string
  kind: string
  enabled: boolean
  /** Non-secret settings: who may write in, limits, and the adapter's own settings. */
  config: InboundConfig
  secretRef: string
  createdBy: string
  createdAt: string
  lastPollAt?: string
  lastError?: string
  receivedCount: number
  repliedCount: number
  rejectedCount: number
  /** Where a channel that calls us posts, for the webhook mode. */
  path: string
}

export interface EmailAdapterConfig {
  mode: 'imap' | 'webhook'
  folder?: string
  processedFolder?: string
  requireAuthResults?: boolean
  trustedAuthservId?: string
  acceptTextAttachments?: boolean
}

export interface InboundConfig {
  allowSenders?: string[]
  allowGroups?: string[]
  ratePerMin?: number
  senderPerHour?: number
  pollSeconds?: number
  adapter?: EmailAdapterConfig
}

export interface InboundInput {
  namespace: string
  agent: string
  name: string
  kind: string
  secretRef: string
  config: InboundConfig
}

export interface InboundTestReport {
  ok: boolean
  details?: Record<string, string>
  error?: string
}

const enc = encodeURIComponent
const JSON_HEADERS = { 'Content-Type': 'application/json' }

async function send<T>(url: string, method: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method }
  if (body !== undefined) { init.headers = JSON_HEADERS; init.body = JSON.stringify(body) }
  const res = await apiFetch(url, init)
  if (!res.ok) throw await failure(res)
  if (res.status === 204) return undefined as T
  return res.json()
}

export async function listInbound(namespace: string, agent: string): Promise<InboundConnection[]> {
  return (await send<InboundConnection[] | null>(`/api/inbound-connections?namespace=${enc(namespace)}&agent=${enc(agent)}`, 'GET')) ?? []
}
export const createInbound = (input: InboundInput) => send<InboundConnection>('/api/inbound-connections', 'POST', input)
export const setInboundEnabled = (id: string, enabled: boolean) =>
  send<InboundConnection>(`/api/inbound-connections/${enc(id)}`, 'PATCH', { enabled })
export const deleteInbound = (id: string) => send<void>(`/api/inbound-connections/${enc(id)}`, 'DELETE')
export const testInbound = (id: string) => send<InboundTestReport>(`/api/inbound-connections/${enc(id)}/test`, 'POST')

/** The full URL a provider's relay posts to, on the host the UI is served from. */
export function inboundUrl(path: string, origin: string = window.location.origin): string {
  return `${origin}${path}`
}

/** Splits text typed one per line, or comma separated, into trimmed, non-empty entries. */
export function parseList(text: string): string[] {
  return text.split(/[\s,]+/).map((o) => o.trim()).filter(Boolean)
}

/** A readable sentence for a failed call, by status. */
export function describeInboundError(e: unknown): string {
  if (!(e instanceof Error)) return String(e)
  const status = (e as { status?: number }).status
  switch (status) {
    case 403: return `You are not allowed to do this. Managing channels needs the builder role. ${e.message}`.trim()
    case 404: return 'That channel or agent was not found. It may have been deleted, or it is in a workspace you cannot use.'
    case 409: return e.message || 'That conflicts with what is already saved.'
    case 503: return 'Channels need the platform PostgreSQL store, which is not set up, or they are turned off.'
    default: return e.message
  }
}
