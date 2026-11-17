/**
 * Client for the agent builder: reading and writing resources through the safe
 * write path, dry runs, templates, publishing, ownership handoff and the
 * builder's option lists. See the U5 contract in plans/ui-adoption-overhaul.md.
 */
import { apiFetch } from './sse'

export type Owner = 'ui' | 'git' | 'unknown'

export interface Ownership {
  owner: Owner
  /** The GitOps tool that manages the object, for example "Argo CD". */
  tool?: string
  /** The application in that tool. */
  app?: string
  editable: boolean
  handoff: { pending: boolean; expires?: string }
}

export interface KubeMetadata {
  name?: string
  namespace?: string
  resourceVersion?: string
  labels?: Record<string, string>
  annotations?: Record<string, string>
  [key: string]: unknown
}

/** A Kubernetes object as the UI API returns it. */
export interface KubeObject {
  apiVersion?: string
  kind?: string
  metadata: KubeMetadata
  spec?: Record<string, unknown>
  status?: unknown
  ownership?: Ownership
  [key: string]: unknown
}

export interface ObjectRef { kind: string; namespace: string; name: string }

export interface OptionItem { name: string; namespace: string; description?: string }

export interface BuilderOptions {
  modelSelectors: OptionItem[]
  tools: OptionItem[]
  mcpServers: OptionItem[]
  knowledgeBases: OptionItem[]
  guardrailPolicies: OptionItem[]
}

/** A reference a template needs that the target workspace lacks, with what could stand in for it. */
export interface MissingRef {
  kind: string
  name: string
  namespace: string
  alternatives: string[]
  /** The key to use in a `replace` map, for example "ModelSelector/default". */
  replaceKey: string
}

/** A failed API call. `status` is the HTTP status; the other fields come from the body when present. */
export class ApiError extends Error {
  status: number
  errors: string[]
  currentResourceVersion?: string
  /** References a template names that do not exist in the target workspace (422 from using a template). */
  missing?: MissingRef[]
  constructor(status: number, message: string, errors: string[] = [], currentResourceVersion?: string, missing?: MissingRef[]) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.errors = errors
    this.currentResourceVersion = currentResourceVersion
    this.missing = missing
  }
}

async function failure(res: Response): Promise<ApiError> {
  let message = ''
  let errors: string[] = []
  let current: string | undefined
  let missing: MissingRef[] | undefined
  const text = (await res.text().catch(() => '')).trim()
  try {
    const body = JSON.parse(text) as { message?: string; error?: string; errors?: string[]; currentResourceVersion?: string; missing?: MissingRef[] }
    errors = Array.isArray(body.errors) ? body.errors : []
    current = body.currentResourceVersion
    missing = Array.isArray(body.missing) ? body.missing.map((m) => ({ ...m, alternatives: m.alternatives ?? [] })) : undefined
    // Never show raw JSON: prefer the message, then the listed errors.
    message = body.message || body.error || errors.join('; ')
  } catch {
    // Not JSON: use the text as is.
    message = text
  }
  return new ApiError(res.status, message || `HTTP ${res.status}`, errors, current, missing)
}

const enc = encodeURIComponent
const JSON_HEADERS = { 'Content-Type': 'application/json' }

/** The object without fields the server owns, ready to send as a write body. */
export function writeBody(obj: KubeObject): KubeObject {
  const copy = { ...obj }
  delete copy.ownership
  delete copy.status
  return copy
}

export async function getResource(kind: string, namespace: string, name: string): Promise<KubeObject> {
  const res = await apiFetch(`/api/resources/${enc(kind)}/${enc(namespace)}/${enc(name)}`)
  if (!res.ok) throw await failure(res)
  return res.json()
}

/** Creates an object. Throws ApiError (409 when the name exists). */
export async function createResource(kind: string, obj: KubeObject): Promise<KubeObject> {
  const res = await apiFetch(`/api/resources/${enc(kind)}`, { method: 'POST', headers: JSON_HEADERS, body: JSON.stringify(writeBody(obj)) })
  if (!res.ok) throw await failure(res)
  return res.json()
}

/** Updates an object. `obj.metadata.resourceVersion` is required: a stale one gives 409, a missing one 428. */
export async function updateResource(kind: string, namespace: string, name: string, obj: KubeObject): Promise<KubeObject> {
  const res = await apiFetch(`/api/resources/${enc(kind)}/${enc(namespace)}/${enc(name)}`, {
    method: 'PUT', headers: JSON_HEADERS, body: JSON.stringify(writeBody(obj)),
  })
  if (!res.ok) throw await failure(res)
  return res.json()
}

export interface DryRunResult { valid: boolean; errors: string[]; object?: KubeObject }

/**
 * Checks a write without saving it. `existing` is the saved object's namespace
 * and name for an update, or null for a create. A rejected object is a normal
 * result (valid: false); only a failure of the call itself throws.
 */
export async function dryRunResource(kind: string, obj: KubeObject, existing: { namespace: string; name: string } | null): Promise<DryRunResult> {
  const url = existing
    ? `/api/resources/${enc(kind)}/${enc(existing.namespace)}/${enc(existing.name)}?dryRun=true`
    : `/api/resources/${enc(kind)}?dryRun=true`
  const res = await apiFetch(url, { method: existing ? 'PUT' : 'POST', headers: JSON_HEADERS, body: JSON.stringify(writeBody(obj)) })
  let body: { valid?: boolean; errors?: string[]; object?: KubeObject; error?: string } | null = null
  try { body = await res.json() } catch { body = null }
  if (res.ok) return { valid: body?.valid !== false, errors: body?.errors ?? [], object: body?.object }
  if (body && (body.valid === false || Array.isArray(body.errors))) return { valid: false, errors: body.errors ?? [] }
  throw new ApiError(res.status, body?.error || `HTTP ${res.status}`)
}

export interface UsedTemplate { agent: KubeObject; created: ObjectRef[] }

/**
 * Copies a template into the caller's workspace as a UI-owned agent. 409 when the name exists; 422 with
 * `missing` when the template names references the workspace lacks (answer with `replace`).
 */
export async function createFromTemplate(namespace: string, name: string, req: { name: string; namespace?: string; replace?: Record<string, string> }): Promise<UsedTemplate> {
  const res = await apiFetch(`/api/templates/${enc(namespace)}/${enc(name)}/use`, {
    method: 'POST', headers: JSON_HEADERS, body: JSON.stringify(req),
  })
  if (!res.ok) throw await failure(res)
  return res.json()
}

/** Creates or updates the AgentDeployment of an agent. */
export async function publishAgent(namespace: string, name: string, req: { name?: string; chat?: boolean }): Promise<{ deployment: KubeObject }> {
  const res = await apiFetch(`/api/agents/${enc(namespace)}/${enc(name)}/publish`, {
    method: 'POST', headers: JSON_HEADERS, body: JSON.stringify(req),
  })
  if (!res.ok) throw await failure(res)
  return res.json()
}

export async function unpublishAgent(namespace: string, name: string): Promise<void> {
  const res = await apiFetch(`/api/agents/${enc(namespace)}/${enc(name)}/publish`, { method: 'DELETE' })
  if (!res.ok) throw await failure(res)
}

export async function getBuilderOptions(): Promise<BuilderOptions> {
  const res = await apiFetch('/api/builder/options')
  if (!res.ok) throw await failure(res)
  const body = (await res.json()) as Partial<BuilderOptions>
  return {
    modelSelectors: body.modelSelectors ?? [],
    tools: body.tools ?? [],
    mcpServers: body.mcpServers ?? [],
    knowledgeBases: body.knowledgeBases ?? [],
    guardrailPolicies: body.guardrailPolicies ?? [],
  }
}

/** Copies an object under a new name, as a UI-owned object. Returns the copy. */
export async function duplicateResource(kind: string, namespace: string, name: string, newName: string): Promise<KubeObject> {
  const res = await apiFetch(`/api/resources/${enc(kind)}/${enc(namespace)}/${enc(name)}/duplicate`, {
    method: 'POST', headers: JSON_HEADERS, body: JSON.stringify({ name: newName }),
  })
  if (!res.ok) throw await failure(res)
  return res.json()
}

/** Returns the object as a YAML manifest for a Git repository and starts the handoff. */
export async function exportResource(kind: string, namespace: string, name: string): Promise<string> {
  const res = await apiFetch(`/api/resources/${enc(kind)}/${enc(namespace)}/${enc(name)}/export`, { method: 'POST' })
  if (!res.ok) throw await failure(res)
  return res.text()
}

export async function cancelHandoff(kind: string, namespace: string, name: string): Promise<void> {
  const res = await apiFetch(`/api/resources/${enc(kind)}/${enc(namespace)}/${enc(name)}/handoff`, { method: 'DELETE' })
  if (!res.ok) throw await failure(res)
}
