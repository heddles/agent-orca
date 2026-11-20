/**
 * Client for webhook triggers and chat widget keys (docs/integrations.md). Listing needs the viewer
 * role, creating, enabling, disabling and deleting the builder role. Every call
 * answers 503 when the platform has no PostgreSQL store.
 */
import { failure } from './builder'
import { apiFetch } from './sse'

export interface Trigger {
  id: string
  namespace: string
  agent: string
  name: string
  /** Dotted path of a JSON field used as the input; empty means the whole body. */
  inputPath: string
  rateLimitPerMin: number
  enabled: boolean
  createdBy: string
  createdAt: string
  lastFiredAt?: string
  fireCount: number
}

/** The answer to creating a trigger. The secret is in it and nowhere else, ever. */
export interface CreatedTrigger {
  trigger: Trigger
  secret: string
  hookPath: string
}

export interface TriggerInput {
  namespace: string
  agent: string
  name: string
  inputPath?: string
  rateLimitPerMin?: number
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

export async function listTriggers(namespace: string, agent: string): Promise<Trigger[]> {
  return (await send<Trigger[] | null>(`/api/triggers?namespace=${enc(namespace)}&agent=${enc(agent)}`, 'GET')) ?? []
}
export const createTrigger = (input: TriggerInput) => send<CreatedTrigger>('/api/triggers', 'POST', input)
export const setTriggerEnabled = (id: string, enabled: boolean) => send<Trigger>(`/api/triggers/${enc(id)}`, 'PATCH', { enabled })
export const deleteTrigger = (id: string) => send<void>(`/api/triggers/${enc(id)}`, 'DELETE')

/** The full URL a sender posts to, on the host the UI is served from. */
export function hookUrl(hookPath: string, origin: string = window.location.origin): string {
  return `${origin}${hookPath}`
}

/** A readable sentence for a failed call, by status. */
export function describeTriggerError(e: unknown): string {
  if (!(e instanceof Error)) return String(e)
  const status = (e as { status?: number }).status
  switch (status) {
    case 403: return `You are not allowed to do this. Managing triggers and widget keys needs the builder role. ${e.message}`.trim()
    case 404: return 'That item or agent was not found. It may have been deleted, or it is in a workspace you cannot use.'
    case 409: return e.message || 'That conflicts with what is already saved.'
    case 503: return 'This needs the platform PostgreSQL store, which is not set up.'
    default: return e.message
  }
}

// ── Chat widget keys ────────────────────────────────────────────────────────

export interface WidgetKey {
  /** The public key in the embed snippet. It is not a secret. */
  id: string
  namespace: string
  agent: string
  name: string
  allowedOrigins: string[]
  rateLimitPerMin: number
  budgetUsdPerDay: number
  enabled: boolean
  createdBy: string
  createdAt: string
}

export interface WidgetKeyInput {
  namespace: string
  agent: string
  name: string
  allowedOrigins: string[]
  rateLimitPerMin?: number
  budgetUsdPerDay?: number
}

export interface WidgetKeyPatch {
  enabled?: boolean
  allowedOrigins?: string[]
  rateLimitPerMin?: number
  budgetUsdPerDay?: number
}

export async function listWidgetKeys(namespace: string, agent: string): Promise<WidgetKey[]> {
  return (await send<WidgetKey[] | null>(`/api/widget-keys?namespace=${enc(namespace)}&agent=${enc(agent)}`, 'GET')) ?? []
}
export const createWidgetKey = (input: WidgetKeyInput) => send<WidgetKey>('/api/widget-keys', 'POST', input)
export const updateWidgetKey = (id: string, patch: WidgetKeyPatch) => send<WidgetKey>(`/api/widget-keys/${enc(id)}`, 'PATCH', patch)
export const deleteWidgetKey = (id: string) => send<void>(`/api/widget-keys/${enc(id)}`, 'DELETE')

/** The snippet a page embeds: the script from this host, with the key. */
export function embedSnippet(keyId: string, origin: string = window.location.origin): string {
  return `<script src="${origin}/widget/v1.js" data-key="${keyId}" async></script>`
}

/** Splits a list typed one per line or comma separated into trimmed, non-empty origins. */
export function parseOrigins(text: string): string[] {
  return text.split(/[\s,]+/).map((o) => o.trim()).filter(Boolean)
}
