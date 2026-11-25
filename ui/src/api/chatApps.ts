/**
 * Client for shared chat apps (docs/inbound-channels.md, "Shared chat apps"): one bot, several
 * agents. Admins connect apps and decide who answers by default; builders say where their agents
 * answer, for whom, and who may approve. Credentials, testing and deleting an app use the channel
 * endpoints in inbound.ts.
 */
import { failure } from './builder'
import { apiFetch } from './sse'
import type { InboundConnection } from './inbound'

export interface AppPlace {
  /** The platform's channel id; '' is direct messages. */
  id: string
  name?: string
  /** This agent answers messages there that name no agent. */
  default?: boolean
}

export interface AppRoute {
  namespace: string
  agent: string
  places: AppPlace[]
  allowSenders: string[]
  approvers: string[]
}

export interface AppPerson { id: string; name?: string }

export interface ChatApp extends InboundConnection {
  routes: AppRoute[]
  /** Who has paired with the app, so they can be picked by name. */
  people: AppPerson[]
  canListPlaces: boolean
}

/** A channel as the platform lists it. */
export interface PlatformPlace { id: string; name: string; member: boolean }

const enc = encodeURIComponent

async function send<T>(url: string, method = 'GET', body?: unknown): Promise<T> {
  const init: RequestInit = { method }
  if (body !== undefined) { init.headers = { 'Content-Type': 'application/json' }; init.body = JSON.stringify(body) }
  const res = await apiFetch(url, init)
  if (!res.ok) throw await failure(res)
  return res.json()
}

const route = (app: string, ns: string, agent: string) => `/api/chat-apps/${enc(app)}/agents/${enc(ns)}/${enc(agent)}`

export const listChatApps = async () => (await send<ChatApp[] | null>('/api/chat-apps')) ?? []
export const createChatApp = (name: string, kind: string) => send<ChatApp>('/api/chat-apps', 'POST', { name, kind })
export const saveAgentRoute = (app: string, r: AppRoute) =>
  send<ChatApp>(route(app, r.namespace, r.agent), 'PUT', {
    places: r.places.map(({ id, name }) => ({ id, name })), allowSenders: r.allowSenders, approvers: r.approvers,
  })
export const removeAgentRoute = (app: string, ns: string, agent: string) => send<ChatApp>(route(app, ns, agent), 'DELETE')
export const setDefaultAgent = (app: string, place: string, namespace: string, agent: string) =>
  send<ChatApp>(`/api/chat-apps/${enc(app)}/default`, 'PUT', { place, namespace, agent })
export const listPlatformPlaces = (app: string) => send<PlatformPlace[]>(`/api/chat-apps/${enc(app)}/places`)

export const placeLabel = (p: AppPlace) => (p.id === '' ? 'Direct messages' : p.name || p.id)

export const personLabel = (people: AppPerson[], id: string) => {
  const name = people.find((p) => p.id.toLowerCase() === id.toLowerCase())?.name
  return name ? `${name} (${id})` : id
}

/** Every place with an agent in an app, each with its agents; direct messages last. */
export function placesOf(app: ChatApp): { id: string; label: string; agents: { namespace: string; agent: string; default: boolean }[] }[] {
  const byId = new Map<string, { id: string; label: string; agents: { namespace: string; agent: string; default: boolean }[] }>()
  for (const r of app.routes) {
    for (const p of r.places) {
      const at = byId.get(p.id) ?? { id: p.id, label: placeLabel(p), agents: [] }
      if (p.name) at.label = placeLabel(p)
      at.agents.push({ namespace: r.namespace, agent: r.agent, default: Boolean(p.default) })
      byId.set(p.id, at)
    }
  }
  return [...byId.values()].sort((a, b) => Number(a.id === '') - Number(b.id === '') || a.label.localeCompare(b.label))
}
