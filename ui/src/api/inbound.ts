/**
 * Client for inbound channel connections (docs/inbound-channels.md): a mailbox, and later chat
 * platforms, that start a run of a published agent from a message and send the result back.
 * Listing needs the viewer role; creating, changing, testing, pairing and deleting the builder
 * role; entering credentials the admin role. No answer ever carries a credential.
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
  /** path on the configured public address; absent when none is configured. */
  url?: string
  /** Messages refused in the last 24 hours, by reason code. */
  refused?: Record<string, number>
  /** Where the credentials are: entered here ("stored"), a Secret, or none yet. Never their values. */
  credentials?: 'stored' | 'secret' | ''
  canStoreCredentials?: boolean
  /** Creates the platform's app with this channel's settings filled in. */
  setupLink?: string
  /** The last request that passed the adapter's check, and the last refusal. */
  lastRequestAt?: string
  lastAuthError?: string
  lastAuthErrorAt?: string
}

export interface InboundPairing {
  id: string
  /** Only in the answer that made it. */
  code?: string
  status: 'waiting' | 'received'
  expiresAt: string
  senderId?: string
  senderName?: string
  channelId?: string
  channelName?: string
  direct?: boolean
}

export interface ApprovedPairing {
  replySent: boolean
  replyError?: string
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
  /** Chat: the exact ids of the spaces a message may come from. */
  allowChannels?: string[]
  /** Chat: how the agent may be addressed (direct, mention, command, channel). */
  modes?: string[]
  ratePerMin?: number
  senderPerHour?: number
  pollSeconds?: number
  adapter?: Partial<EmailAdapterConfig> | Record<string, string | number | boolean>
}

/** One setting of an adapter, as the adapter describes it. */
export interface InboundField {
  key: string
  label: string
  type: 'string' | 'bool' | 'int' | 'enum'
  required?: boolean
  help?: string
  placeholder?: string
  options?: string[]
  default?: string | number | boolean
}

/** What a registered kind of channel offers: its settings form, Secret keys and setup steps. */
export interface InboundKind {
  kind: string
  descriptor: {
    title: string
    summary?: string
    fields?: InboundField[]
    secretKeys?: { key: string; required?: boolean; help?: string }[]
    allow?: { users?: string; groups?: string; channels?: string }
    /** Steps to do on the platform; {{url}} stands for the connection's receive URL. */
    setup?: string[]
  }
  capabilities: { threads: boolean; dialect?: string; modes?: string[]; ackFirst?: boolean; workingIndicator?: boolean }
  testable: boolean
  receives: boolean
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
export async function listInboundKinds(): Promise<InboundKind[]> {
  return (await send<InboundKind[] | null>('/api/inbound-kinds', 'GET')) ?? []
}
export const createInbound = (input: InboundInput) => send<InboundConnection>('/api/inbound-connections', 'POST', input)
export const setInboundEnabled = (id: string, enabled: boolean) =>
  send<InboundConnection>(`/api/inbound-connections/${enc(id)}`, 'PATCH', { enabled })
export const setInboundSecret = (id: string, secretRef: string) =>
  send<InboundConnection>(`/api/inbound-connections/${enc(id)}`, 'PATCH', { secretRef })
export const setInboundCredentials = (id: string, values: Record<string, string>) =>
  send<InboundConnection>(`/api/inbound-connections/${enc(id)}/credentials`, 'PUT', { values })
const pairings = (id: string) => `/api/inbound-connections/${enc(id)}/pairings`
/** route: on a shared chat app, the agent the person is for; without it, the app itself (admin). */
export const createPairing = (id: string, route?: { namespace: string; agent: string }) => send<InboundPairing>(pairings(id), 'POST', route)
export const getPairing = (id: string, pairing: string) => send<InboundPairing>(`${pairings(id)}/${enc(pairing)}`, 'GET')
export const approvePairing = (id: string, pairing: string) => send<ApprovedPairing>(`${pairings(id)}/${enc(pairing)}/approve`, 'POST')
export const discardPairing = (id: string, pairing: string) => send<void>(`${pairings(id)}/${enc(pairing)}`, 'DELETE')
export const deleteInbound = (id: string) => send<void>(`/api/inbound-connections/${enc(id)}`, 'DELETE')
export const testInbound = (id: string) => send<InboundTestReport>(`/api/inbound-connections/${enc(id)}/test`, 'POST')

/** The full URL a provider's relay posts to, on the host the UI is served from. */
export function inboundUrl(path: string, origin: string = window.location.origin): string {
  return `${origin}${path}`
}

/** The setup steps of a kind with the connection's receive URL filled in. */
export function setupSteps(kind: InboundKind | undefined, url: string): string[] {
  return (kind?.descriptor.setup ?? []).map((t) => t.split('{{url}}').join(url))
}

const REFUSALS: Record<string, string> = {
  sender_not_allowed: 'the sender is not allowed',
  channel_not_allowed: 'that channel is not on the list',
  mode_not_enabled: 'the agent does not answer messages sent that way',
  connection_rate: 'too many messages to this channel at once',
  sender_rate: 'one sender reached the hourly limit',
  workspace_mismatch: 'it came from a different workspace than this channel’s',
  no_agent_here: 'no agent answers in that place',
}

/** A refusal reason code in plain words; an unknown code is shown as it is. */
export const refusalText = (code: string) => REFUSALS[code] ?? code

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
