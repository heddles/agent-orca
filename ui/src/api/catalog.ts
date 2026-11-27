/** Client for the agent catalog, agent manifests, starting runs and "my runs". */
import { apiFetch } from './sse'
import type { JSONSchema } from '../lib/schemaForm'
import type { Ownership } from './builder'

export interface CatalogMember {
  kind: string
  name: string
}

export interface CatalogEntry {
  name: string
  namespace: string
  description: string
  exampleInputs: string[]
  maintainer: string
  /** True when the agent may be started: it has a deployment, or it is not built in the UI and its spec can run. */
  published: boolean
  notPublishedReason?: string
  deployment?: string
  /** True when the agent has a deployment, which chat and the chat widget need. */
  conversational: boolean
  template: boolean
  templateGroup?: string
  tools: string[]
  modelSelector: string
  runCount: number
  /** Median cost of the agent's finished runs, in USD. Null when unknown. */
  typicalCostUSD: number | null
  members?: CatalogMember[]
  /** Who manages the agent: this UI, Git, or unknown. */
  ownership?: Ownership
}

export interface AgentManifest {
  name: string
  namespace?: string
  description: string
  example_inputs?: string[]
  input_schema?: JSONSchema
  allowed_tools?: Array<{ name: string; description?: string }>
  knowledge_bases?: string[]
  clarify_available?: boolean
}

export interface MyRun {
  name: string
  namespace: string
  agentRef: string
  phase: string
  spendUSD: string
  input: string
  output?: string
  clarifyQuestion?: string
  createdBy?: string
  createdAt: string
  completionTime?: string
}

export interface MyRuns {
  /** "user": runs the signed-in user started. "tenant": the tenant's recent runs. */
  scope: 'user' | 'tenant'
  userId: string
  runs: MyRun[]
}

async function failure(res: Response): Promise<Error> {
  let text = ''
  try {
    text = (await res.text()).trim()
    const body = JSON.parse(text) as { message?: string; error?: string }
    text = body.message || body.error || text
  } catch {
    // Not JSON: use the text as is.
  }
  return new Error(text || `HTTP ${res.status}`)
}

export async function getCatalog(namespace = ''): Promise<CatalogEntry[]> {
  const res = await apiFetch(`/api/catalog?namespace=${encodeURIComponent(namespace)}`)
  if (!res.ok) throw await failure(res)
  return res.json()
}

export async function getAgentManifest(namespace: string, name: string): Promise<AgentManifest> {
  const res = await apiFetch(`/api/agents/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/manifest`)
  if (!res.ok) throw await failure(res)
  return res.json()
}

/** Starts a run of an agent. `message` is the text the agent receives. */
export async function startAgentRun(namespace: string, name: string, message: string): Promise<{ run_id: string }> {
  const res = await apiFetch(`/api/agents/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/run`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ input: [{ role: 'user', parts: [{ content_type: 'text/plain', content: message }] }] }),
  })
  if (!res.ok) throw await failure(res)
  return res.json()
}

export async function getMyRuns(namespace = ''): Promise<MyRuns> {
  const res = await apiFetch(`/api/my/runs?namespace=${encodeURIComponent(namespace)}`)
  if (!res.ok) throw await failure(res)
  return res.json()
}
