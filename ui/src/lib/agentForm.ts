/**
 * The builder's form model and its mapping to an Agent object. The form uses
 * plain words; this file is the one place that knows the CRD field names. It
 * keeps every field the form does not show (runtime, resources, ...) so that
 * editing in the form never drops what was set in the YAML view.
 */
import type { KubeObject } from '../api/builder'

export const AGENT_API_VERSION = 'agentorca.agentorca.io/v1alpha1'
/** The ModelSelector that "Auto (recommended)" stands for, offered only where it exists. */
export const AUTO_MODEL_SELECTOR = 'default'
/** The agent image a new agent runs on, the same as the platform's templates. */
export const DEFAULT_RUNTIME = { ociRef: 'ghcr.io/heddles/agent-orca/openai-reference:latest', framework: 'openai-compatible' }

/**
 * Whether "Auto (recommended)" can be offered: only when the options list a
 * ModelSelector called `default` in the namespace the agent will be saved in
 * (an item without a namespace is taken to be already scoped by the server).
 */
export function autoAvailable(selectors: { name: string; namespace: string }[] | undefined, namespace: string): boolean {
  return (selectors ?? []).some((s) => s.name === AUTO_MODEL_SELECTOR && (!s.namespace || s.namespace === namespace))
}

export const ANNOTATION = {
  description: 'agentorca.io/description',
  exampleInputs: 'agentorca.io/example-inputs',
  maintainer: 'agentorca.io/maintainer',
} as const

export interface AgentForm {
  name: string
  namespace: string
  description: string
  maintainer: string
  /** One example per line. */
  exampleInputs: string
  instructions: string
  /** The ModelSelector the agent references, as written. The value of AUTO_MODEL_SELECTOR is shown as Auto where it exists. */
  modelSelector: string
  /** Tool and MCP server names; both are entries of the agent's tool list. */
  connections: string[]
  knowledgeBases: string[]
  guardrail: string
}

export type FormErrors = Partial<Record<keyof AgentForm, string>>

export function emptyForm(namespace = ''): AgentForm {
  return {
    name: '', namespace, description: '', maintainer: '', exampleInputs: '', instructions: '',
    modelSelector: '', connections: [], knowledgeBases: [], guardrail: '',
  }
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : ''
}

function strings(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : []
}

function parseExamples(raw: string | undefined): string[] {
  if (!raw) return []
  try {
    const v = JSON.parse(raw)
    return strings(v)
  } catch {
    return []
  }
}

/** Reads the form's values out of an Agent object. */
export function formFromObject(obj: KubeObject): AgentForm {
  const spec = (obj.spec ?? {}) as Record<string, unknown>
  const ann = obj.metadata?.annotations ?? {}
  const refs = Array.isArray(spec.knowledgeBaseRefs)
    ? (spec.knowledgeBaseRefs as Array<{ name?: unknown }>).map((r) => str(r?.name)).filter(Boolean)
    : null
  const selector = str(spec.modelSelectorRef)
  return {
    name: str(obj.metadata?.name),
    namespace: str(obj.metadata?.namespace),
    description: ann[ANNOTATION.description] ?? '',
    maintainer: ann[ANNOTATION.maintainer] ?? '',
    exampleInputs: parseExamples(ann[ANNOTATION.exampleInputs]).join('\n'),
    instructions: str(spec.systemPrompt),
    modelSelector: selector,
    connections: strings(spec.tools),
    knowledgeBases: refs ?? strings(spec.knowledgeBases),
    guardrail: str(spec.guardrailPolicyRef),
  }
}

/** Writes the form's values into a copy of `base` (or a new Agent when null). */
export function objectFromForm(base: KubeObject | null, form: AgentForm): KubeObject {
  const obj: KubeObject = base
    ? (JSON.parse(JSON.stringify(base)) as KubeObject)
    : { apiVersion: AGENT_API_VERSION, kind: 'Agent', metadata: {}, spec: { runtime: { ...DEFAULT_RUNTIME } } }
  obj.apiVersion ||= AGENT_API_VERSION
  obj.kind ||= 'Agent'
  obj.metadata ||= {}
  obj.metadata.name = form.name
  if (form.namespace) obj.metadata.namespace = form.namespace
  else delete obj.metadata.namespace

  const annotations = { ...(obj.metadata.annotations ?? {}) }
  const setAnn = (key: string, value: string) => { if (value) annotations[key] = value; else delete annotations[key] }
  setAnn(ANNOTATION.description, form.description.trim())
  setAnn(ANNOTATION.maintainer, form.maintainer.trim())
  const examples = form.exampleInputs.split('\n').map((l) => l.trim()).filter(Boolean)
  setAnn(ANNOTATION.exampleInputs, examples.length > 0 ? JSON.stringify(examples) : '')
  if (Object.keys(annotations).length > 0) obj.metadata.annotations = annotations
  else delete obj.metadata.annotations

  const spec = (obj.spec ??= {})
  if (form.modelSelector) spec.modelSelectorRef = form.modelSelector; else delete spec.modelSelectorRef
  if (form.instructions) spec.systemPrompt = form.instructions; else delete spec.systemPrompt
  if (form.connections.length > 0) spec.tools = form.connections; else delete spec.tools
  if (Array.isArray(spec.knowledgeBaseRefs)) {
    const existing = spec.knowledgeBaseRefs as Array<{ name?: string }>
    const next = form.knowledgeBases.map((n) => existing.find((r) => r?.name === n) ?? { name: n })
    if (next.length > 0) spec.knowledgeBaseRefs = next; else delete spec.knowledgeBaseRefs
  } else if (form.knowledgeBases.length > 0) spec.knowledgeBases = form.knowledgeBases
  else delete spec.knowledgeBases
  if (form.guardrail) spec.guardrailPolicyRef = form.guardrail; else delete spec.guardrailPolicyRef
  return obj
}

const DNS_LABEL = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/

/** A name Kubernetes accepts: lower-case letters, digits and dashes. Empty string when fine. */
export function nameProblem(name: string): string {
  if (!name.trim()) return 'Give the agent a name.'
  if (name.length > 63) return 'Use 63 characters or fewer.'
  if (!DNS_LABEL.test(name)) return 'Use lower-case letters, numbers and dashes only, and start and end with a letter or number.'
  return ''
}

/** What is missing before the agent can be saved. */
export function validateForm(form: AgentForm, opts: { needsNamespace?: boolean } = {}): FormErrors {
  const errors: FormErrors = {}
  const nameErr = nameProblem(form.name)
  if (nameErr) errors.name = nameErr
  if (opts.needsNamespace && !form.namespace) errors.namespace = 'Choose a workspace.'
  if (!form.description.trim()) errors.description = 'Say in a sentence what this agent does, so people can find it.'
  if (!form.instructions.trim()) errors.instructions = 'Write the instructions the agent follows.'
  if (!form.modelSelector) errors.modelSelector = 'Choose a model.'
  return errors
}

/** Where a server error belongs, so it can be shown next to the field. Null when unclear. */
export function fieldForServerError(message: string): keyof AgentForm | null {
  const m = message.toLowerCase()
  if (m.includes('systemprompt')) return 'instructions'
  if (m.includes('modelselectorref')) return 'modelSelector'
  if (m.includes('guardrail')) return 'guardrail'
  if (m.includes('knowledgebase')) return 'knowledgeBases'
  if (m.includes('spec.tools')) return 'connections'
  if (m.includes('metadata.name')) return 'name'
  return null
}

/**
 * The form with an AI draft's fields filled in. The workspace, maintainer and model are the person's
 * and stay as they are. Returns the new form and the form fields the draft changed, to be marked.
 */
export function applyDraft(form: AgentForm, d: { name: string; description: string; instructions: string; exampleInputs: string[]; connections: string[]; knowledgeBases: string[]; guardrail: string }): { form: AgentForm; fields: Array<keyof AgentForm> } {
  const next: AgentForm = { ...form }
  const fields: Array<keyof AgentForm> = []
  const set = <K extends keyof AgentForm>(k: K, v: AgentForm[K], present: boolean) => { if (present) { next[k] = v; fields.push(k) } }
  set('name', d.name, d.name !== '')
  set('description', d.description, d.description !== '')
  set('instructions', d.instructions, d.instructions !== '')
  set('exampleInputs', d.exampleInputs.join('\n'), d.exampleInputs.length > 0)
  set('connections', d.connections, d.connections.length > 0)
  set('knowledgeBases', d.knowledgeBases, d.knowledgeBases.length > 0)
  set('guardrail', d.guardrail, d.guardrail !== '')
  return { form: next, fields }
}
