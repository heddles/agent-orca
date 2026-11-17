/**
 * Client for evaluation datasets, cases and graders (docs/evaluations.md).
 * Reading needs the viewer role, writing the builder role, grading the runner
 * role. Every call answers 503 when the platform has no PostgreSQL store.
 */
import { failure } from './builder'
import { apiFetch } from './sse'

export type ReferenceKind = 'none' | 'exact' | 'example' | 'rubric'

export interface EvalDataset {
  id: number
  namespace: string
  name: string
  description?: string
  agent?: string
  version: number
  caseCount: number
  createdBy: string
  createdAt: string
  updatedAt: string
}

export interface EvalCase {
  id: number
  datasetId: number
  addedIn: number
  input: string
  reference?: string
  referenceKind: ReferenceKind
  tags: string[]
  weight: number
  sourceRun?: string
  sourceRunNamespace?: string
  redacted: boolean
  createdAt: string
}

export interface CasePage {
  cases: EvalCase[]
  /** Pass as `cursor` for the next page; empty on the last. */
  nextCursor: string
  datasetVersion: number
}

export interface CaseInput {
  input: string
  reference?: string
  referenceKind?: ReferenceKind
  tags?: string[]
  weight?: number
}

export interface CasePatch {
  input?: string
  reference?: string
  referenceKind?: ReferenceKind
  tags?: string[]
  weight?: number
}

export const GRADER_KINDS = [
  'exact', 'contains', 'not_contains', 'regex', 'json_schema', 'json_field_equals',
  'tool_called', 'tool_not_called', 'max_cost', 'max_latency',
] as const
export type GraderKind = (typeof GRADER_KINDS)[number]

export interface EvalGrader {
  id: number
  datasetId: number
  name: string
  kind: GraderKind
  config: Record<string, unknown>
  weight: number
  required: boolean
  createdBy: string
  createdAt: string
  updatedAt: string
}

export interface GraderInput {
  kind: GraderKind
  config: Record<string, unknown>
  weight?: number
  required?: boolean
}

export interface GradeRequest {
  caseId?: number
  reference?: string
  arm?: string
  output: string
  toolCalls?: string[]
  costUsd?: number
  latencyMs?: number
}

export interface EvalGrade {
  graderId: number
  grader: string
  kind?: string
  score: number
  pass: boolean
  notGraded?: boolean
  detail: string
}

export interface EvalResult {
  id: number
  datasetId: number
  caseId?: number
  output: string
  toolCalls: string[]
  costUsd: number
  latencyMs: number
  casePass: boolean
  createdAt: string
  grades: EvalGrade[]
}

/** Limits the server enforces, shown so people are not surprised. The operator may lower the byte limit. */
export const EVAL_LIMITS = { maxCaseBytes: 65536, maxCasesPerDataset: 2000, maxTags: 20, maxGraders: 50, maxOutputBytes: 262144 }

const enc = encodeURIComponent
const JSON_HEADERS = { 'Content-Type': 'application/json' }
const root = '/api/evals'

async function send<T>(url: string, method: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method }
  if (body !== undefined) { init.headers = JSON_HEADERS; init.body = JSON.stringify(body) }
  const res = await apiFetch(url, init)
  if (!res.ok) throw await failure(res)
  if (res.status === 204) return undefined as T
  return res.json()
}

export async function listDatasets(opts: { namespace?: string; agent?: string } = {}): Promise<EvalDataset[]> {
  const q = new URLSearchParams()
  if (opts.namespace) q.set('namespace', opts.namespace)
  if (opts.agent) q.set('agent', opts.agent)
  const qs = q.toString()
  const body = await send<{ datasets?: EvalDataset[] }>(`${root}/datasets${qs ? `?${qs}` : ''}`, 'GET')
  return body.datasets ?? []
}

export const getDataset = (id: number) => send<EvalDataset>(`${root}/datasets/${id}`, 'GET')

/** Creates a dataset. 409 when the name exists in the workspace. */
export const createDataset = (req: { namespace?: string; name: string; description?: string; agent?: string }) =>
  send<EvalDataset>(`${root}/datasets`, 'POST', req)

export const patchDataset = (id: number, patch: { description?: string; agent?: string }) =>
  send<EvalDataset>(`${root}/datasets/${id}`, 'PATCH', patch)

export const deleteDataset = (id: number) => send<void>(`${root}/datasets/${id}`, 'DELETE')

export async function listCases(datasetId: number, opts: { tag?: string; limit?: number; cursor?: string } = {}): Promise<CasePage> {
  const q = new URLSearchParams()
  if (opts.tag) q.set('tag', opts.tag)
  if (opts.limit) q.set('limit', String(opts.limit))
  if (opts.cursor) q.set('cursor', opts.cursor)
  const qs = q.toString()
  const body = await send<Partial<CasePage>>(`${root}/datasets/${datasetId}/cases${qs ? `?${qs}` : ''}`, 'GET')
  return {
    cases: (body.cases ?? []).map((c) => ({ ...c, tags: c.tags ?? [] })),
    nextCursor: body.nextCursor ?? '',
    datasetVersion: body.datasetVersion ?? 0,
  }
}

/** Adds one case. 409 when the dataset is full, 413 over the size limit. */
export const addCase = (datasetId: number, c: CaseInput) =>
  send<{ added: number; datasetVersion: number; cases: EvalCase[] }>(`${root}/datasets/${datasetId}/cases`, 'POST', c)

export const updateCase = (id: number, patch: CasePatch) =>
  send<{ case: EvalCase; datasetVersion: number }>(`${root}/cases/${id}`, 'PATCH', patch)

export const deleteCase = (id: number) => send<{ datasetVersion: number }>(`${root}/cases/${id}`, 'DELETE')

export async function listGraders(datasetId: number): Promise<EvalGrader[]> {
  const body = await send<{ graders?: EvalGrader[] }>(`${root}/datasets/${datasetId}/graders`, 'GET')
  return body.graders ?? []
}

/** Creates a grader, or replaces the one with that name. */
export const putGrader = (datasetId: number, name: string, g: GraderInput) =>
  send<EvalGrader>(`${root}/datasets/${datasetId}/graders/${enc(name)}`, 'PUT', g)

export const deleteGrader = (datasetId: number, name: string) =>
  send<void>(`${root}/datasets/${datasetId}/graders/${enc(name)}`, 'DELETE')

/** Grades a supplied output with every grader of the dataset and keeps the result. 409 when there are no graders. */
export async function gradeOutput(datasetId: number, req: GradeRequest): Promise<EvalResult> {
  const r = await send<EvalResult>(`${root}/datasets/${datasetId}/grade`, 'POST', req)
  return { ...r, toolCalls: r.toolCalls ?? [], grades: r.grades ?? [] }
}

/** A readable sentence for a failed call, by status. */
export function describeEvalError(e: unknown): string {
  if (!(e instanceof Error)) return String(e)
  const status = (e as { status?: number }).status
  const msg = e.message
  switch (status) {
    case 403: return `You are not allowed to do this. Changing datasets needs the builder role, and grading the runner role. ${msg}`.trim()
    case 404: return 'That item was not found. It may have been deleted, or it is in a workspace you cannot use.'
    case 409: return msg || 'That conflicts with what is already saved.'
    case 413: return msg || 'That text is over the size limit.'
    case 400:
    case 422: return msg || 'The values were not accepted.'
    case 503: return 'Evaluations need the platform PostgreSQL store, which is not set up.'
    default: return msg
  }
}
