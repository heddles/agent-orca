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
  /** Whether "Describe what you want" can be offered, and why not when it cannot. */
  draft: { available: boolean; reason?: string }
}

/** A draft of the form's fields, written by a model from a description. Nothing is saved. */
export interface AgentDraft {
  name: string
  namespace: string
  description: string
  instructions: string
  exampleInputs: string[]
  /** Tool and MCP server names. */
  connections: string[]
  knowledgeBases: string[]
  guardrail: string
  /** Advice for a run budget in USD; the form has no field for it. */
  suggestedBudgetUsd?: number
  /** The form fields the model filled. */
  aiFields: string[]
  /** What was dropped or changed, in plain words. */
  notes: string[]
  /** The verdict of the dry-run write on the assembled agent. */
  valid: boolean
  errors?: string[]
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
  /** The parsed JSON body of the failed response, when it had one (a 409 from publish carries `gate`). */
  body?: Record<string, unknown>
  constructor(status: number, message: string, errors: string[] = [], currentResourceVersion?: string, missing?: MissingRef[], body?: Record<string, unknown>) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.errors = errors
    this.currentResourceVersion = currentResourceVersion
    this.missing = missing
    this.body = body
  }
}

export async function failure(res: Response): Promise<ApiError> {
  let message = ''
  let errors: string[] = []
  let current: string | undefined
  let missing: MissingRef[] | undefined
  let parsed: Record<string, unknown> | undefined
  const text = (await res.text().catch(() => '')).trim()
  try {
    const body = JSON.parse(text) as { message?: string; error?: string; errors?: string[]; currentResourceVersion?: string; missing?: MissingRef[] }
    if (body && typeof body === 'object' && !Array.isArray(body)) parsed = body as Record<string, unknown>
    errors = Array.isArray(body.errors) ? body.errors : []
    current = body.currentResourceVersion
    missing = Array.isArray(body.missing) ? body.missing.map((m) => ({ ...m, alternatives: m.alternatives ?? [] })) : undefined
    // Never show raw JSON: prefer the message, then the listed errors.
    message = body.message || body.error || errors.join('; ')
  } catch {
    // Not JSON: use the text as is.
    message = text
  }
  return new ApiError(res.status, message || `HTTP ${res.status}`, errors, current, missing, parsed)
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
export async function publishAgent(namespace: string, name: string, req: { name?: string; chat?: boolean; overrideGate?: { reason: string } }): Promise<{ deployment: KubeObject }> {
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
    draft: body.draft ?? { available: false },
  }
}

/**
 * Asks the workspace's model for a draft from a plain-English description. 409 (code draft_unavailable) when no
 * model is set up, 429 when the user asked too often, 413 for a long description, 502 when the model's answer was
 * not usable. The draft is not stored: the caller shows it for review.
 */
export async function draftFromDescription(description: string, namespace?: string): Promise<AgentDraft> {
  const res = await apiFetch('/api/builder/draft', {
    method: 'POST', headers: JSON_HEADERS, body: JSON.stringify({ description, ...(namespace ? { namespace } : {}) }),
  })
  if (!res.ok) throw await failure(res)
  const body = (await res.json()) as AgentDraft
  return {
    ...body,
    exampleInputs: body.exampleInputs ?? [], connections: body.connections ?? [], knowledgeBases: body.knowledgeBases ?? [],
    aiFields: body.aiFields ?? [], notes: body.notes ?? [],
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

export interface KnowledgeDoc { name: string; bytes: number }

export interface KnowledgeDocs {
  documents: KnowledgeDoc[]
  totalBytes: number
  limits: { maxDocBytes: number; maxDocs: number; maxTotalBytes: number; types: string[] }
  /** True when the caller may add and remove documents: the builder role on a UI-owned knowledge base. */
  editable: boolean
  /** Why not, in plain words, when editable is false. */
  reason?: string
  /** A caution about what the change did not do. */
  warning?: string
}

const DEFAULT_DOC_LIMITS = { maxDocBytes: 262144, maxDocs: 50, maxTotalBytes: 921600, types: ['.txt', '.md', '.markdown', '.csv'] }

function normalizeDocs(b: Partial<KnowledgeDocs>): KnowledgeDocs {
  return { documents: b.documents ?? [], totalBytes: b.totalBytes ?? 0, limits: { ...DEFAULT_DOC_LIMITS, ...(b.limits ?? {}) }, editable: !!b.editable, reason: b.reason, warning: b.warning }
}

const knowledgeUrl = (ns: string, kb: string, doc?: string) =>
  `/api/knowledge/${enc(ns)}/${enc(kb)}/documents${doc ? `/${enc(doc)}` : ''}`

/** The documents of a knowledge base added here: names and sizes, never the text. */
export async function listKnowledgeDocs(namespace: string, kb: string): Promise<KnowledgeDocs> {
  const res = await apiFetch(knowledgeUrl(namespace, kb))
  if (!res.ok) throw await failure(res)
  return normalizeDocs((await res.json()) as Partial<KnowledgeDocs>)
}

/**
 * Adds or replaces a text, markdown or csv document. 400 for a bad file name, 409 when the knowledge base is
 * managed in Git or full, 413 over a size limit, 415 when the content is not text.
 */
export async function putKnowledgeDoc(namespace: string, kb: string, name: string, content: string): Promise<KnowledgeDocs> {
  const res = await apiFetch(knowledgeUrl(namespace, kb, name), { method: 'PUT', headers: { 'Content-Type': 'text/plain; charset=utf-8' }, body: content })
  if (!res.ok) throw await failure(res)
  return normalizeDocs((await res.json()) as Partial<KnowledgeDocs>)
}

export async function deleteKnowledgeDoc(namespace: string, kb: string, name: string): Promise<KnowledgeDocs> {
  const res = await apiFetch(knowledgeUrl(namespace, kb, name), { method: 'DELETE' })
  if (!res.ok) throw await failure(res)
  return normalizeDocs((await res.json()) as Partial<KnowledgeDocs>)
}
