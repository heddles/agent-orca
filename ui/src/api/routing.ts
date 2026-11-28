/** Client for automatic agent routing: whether an agent can be picked, and running the pick. */
import { failure } from './builder'
import { apiFetch } from './sse'
import type { RunRouting } from './sse'

export interface Routing {
  auto: boolean
  /** Why not, when auto is false. */
  reason?: string
  /** The tenant's default agent, which an unclear request goes to. */
  defaultAgent?: string
  /** False when there is no default, or this person may not run it. */
  hasDefault: boolean
}

export interface RoutedRun {
  run_id: string
  agent_name: string
  namespace: string
  routing?: RunRouting
}

const query = (namespace: string) => (namespace ? `?namespace=${encodeURIComponent(namespace)}` : '')

export async function getRouting(namespace = ''): Promise<Routing> {
  const res = await apiFetch(`/api/routing${query(namespace)}`)
  if (!res.ok) throw await failure(res)
  return res.json()
}

/** Starts a run of the agent picked for `message`. */
export async function startRoutedRun(message: string, namespace = ''): Promise<RoutedRun> {
  const res = await apiFetch(`/api/routing/run${query(namespace)}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ input: [{ role: 'user', parts: [{ content_type: 'text/plain', content: message }] }] }),
  })
  if (!res.ok) throw await failure(res)
  return res.json()
}
