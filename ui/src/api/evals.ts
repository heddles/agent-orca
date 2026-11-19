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
  /** ok, error or pending_review. */
  status?: string
  gradedBy?: string
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

/** Downloads the dataset's live cases as JSON lines (builder role; the export is audited). */
export async function exportDataset(id: number): Promise<{ blob: Blob; filename: string }> {
  const res = await apiFetch(`${root}/datasets/${id}/export`, { method: 'GET' })
  if (!res.ok) throw await failure(res)
  const m = /filename="([^"]+)"/.exec(res.headers.get('Content-Disposition') ?? '')
  return { blob: await res.blob(), filename: m?.[1] ?? `dataset-${id}.jsonl` }
}

export interface CaptureRequest {
  datasetId: number
  /** The expected answer the person wrote. Leave out for a case with no reference. */
  reference?: string
  referenceKind?: ReferenceKind
  /** Take the run's own output as the reference example. Never the default. */
  useOutput?: boolean
  tags?: string[]
  /** Return what would be stored (redacted) and store nothing. */
  preview?: boolean
}

export interface CaptureResult {
  case: EvalCase
  datasetVersion?: number
  redaction: Record<string, { replacements: number; rules?: string[] }>
  preview?: boolean
  original?: Record<string, string>
}

/** Copies a run into a dataset as a case: an explicit step, never automatic. Needs the builder role. */
export const captureRun = (namespace: string, run: string, req: CaptureRequest) =>
  send<CaptureResult>(`/api/runs/${enc(namespace)}/${enc(run)}/dataset`, 'POST', req)

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

// ── Experiments ──────────────────────────────────────────────────────────────

export type ExperimentStatus = 'queued' | 'running' | 'grading' | 'awaiting_review' | 'complete' | 'cancelled' | 'failed'
/** Statuses in which the experiment still changes, so a screen should keep polling. */
export const ACTIVE_STATUSES: ExperimentStatus[] = ['queued', 'running', 'grading', 'awaiting_review']

export interface ExperimentArm {
  label?: string
  agent: string
  /** A saved revision number; absent runs the live agent. */
  revision?: number
  /** Filled in when the experiment starts. */
  hash?: string
  modelProvider?: string
}

export interface Interval { lo: number; hi: number }

export interface ArmSummary {
  label: string
  cases: number
  skipped: number
  passed: number
  excludedError: number
  excludedPending: number
  passRate: number
  passRateCi: Interval
  meanScore: number
  failureRate: number
  costTotalUsd: number
  costMedianUsd: number
  costIncomplete: boolean
  hasLatency: boolean
  latencyP50Ms: number
  latencyP95Ms: number
  repeatedCases: number
  flakyCases: number
  stability: number
}

export interface MeanDiff { n: number; mean: number; hasCi: boolean; ci: Interval; approximate?: boolean }
export interface MedianDiff { n: number; median: number; baselineMedian: number; candidateMedian: number; rel: number; hasRel: boolean }
export interface PassRateDiff {
  counts: { bothPass: number; improved: number; regressed: number; bothFail: number }
  n: number
  baselineRate: number
  candidateRate: number
  diff: number
  ci: Interval
  mcnemar: { improved: number; regressed: number; exact: boolean; statistic: number; p: number }
}
export interface Verdict {
  verdict: 'better' | 'worse' | 'no clear difference' | 'not enough data'
  conclusive: boolean
  nonInferior: boolean
  margin: number
  minCases: number
  reason: string
}
export interface Comparison {
  baseline: string
  candidate: string
  pairs: number
  skipped: number
  passRate?: PassRateDiff
  score?: MeanDiff
  cost?: MeanDiff
  latency?: MeanDiff
  costMedian?: MedianDiff
  latencyMedian?: MedianDiff
  verdict: Verdict
  regressedCases: number[]
  improvedCases: number[]
  skippedCases: number[]
}
export interface GraderTally {
  graderId?: number
  name?: string
  graded: number
  passed: number
  notGraded?: number
  invalid?: number
  errors?: number
  pendingReview?: number
  inconsistent?: number
  judgeCostUsd?: number
}
export interface ExperimentSummary {
  stoppedBy?: string
  partial?: boolean
  results?: number
  skipped?: number
  spentUsd?: number
  judgeSpentUsd?: number
  reviewPending?: number
  graders?: GraderTally[] | Record<string, GraderTally>
  comparison?: {
    baseline: string
    arms: ArmSummary[]
    comparisons: Comparison[]
    excludedError: number
    excludedPending: number
  }
}

export interface Experiment {
  id: number
  namespace: string
  datasetId: number
  datasetVersion: number
  name?: string
  status: ExperimentStatus
  arms: ExperimentArm[]
  repeats: number
  budgetUsd: number
  spentUsd: number
  runBudgetUsd?: number
  timeoutSeconds?: number
  concurrency?: number
  cancelRequested?: boolean
  cases?: number
  resultsDone: number
  resultsTotal: number
  summary?: ExperimentSummary
  warnings?: string[]
  error?: string
  createdBy?: string
  createdAt?: string
  startedAt?: string
  finishedAt?: string
}

export interface ExperimentRequest {
  datasetId: number
  name?: string
  arms: ExperimentArm[]
  repeats?: number
  budgetUsd: number
  runBudgetUsd?: number
  timeoutSeconds?: number
  concurrency?: number
}

export interface ExperimentResult extends EvalResult {
  arm?: string
  experimentId?: number
  repeat?: number
  phase?: string
  costIncomplete?: boolean
  runName?: string
}

export interface SummaryResponse {
  experimentId: number
  status: ExperimentStatus
  live: boolean
  partial: boolean
  summary: ExperimentSummary
}

function normalizeExperiment(e: Experiment): Experiment {
  return { ...e, arms: e.arms ?? [], warnings: e.warnings ?? [] }
}

export async function listExperiments(opts: { namespace?: string; datasetId?: number; status?: string; limit?: number; cursor?: string } = {}): Promise<{ experiments: Experiment[]; nextCursor: string }> {
  const q = new URLSearchParams()
  if (opts.namespace) q.set('namespace', opts.namespace)
  if (opts.datasetId) q.set('datasetId', String(opts.datasetId))
  if (opts.status) q.set('status', opts.status)
  if (opts.limit) q.set('limit', String(opts.limit))
  if (opts.cursor) q.set('cursor', opts.cursor)
  const qs = q.toString()
  const body = await send<{ experiments?: Experiment[]; nextCursor?: string }>(`${root}/experiments${qs ? `?${qs}` : ''}`, 'GET')
  return { experiments: (body.experiments ?? []).map(normalizeExperiment), nextCursor: body.nextCursor ?? '' }
}

export async function getExperiment(id: number): Promise<Experiment> {
  return normalizeExperiment(await send<Experiment>(`${root}/experiments/${id}`, 'GET'))
}

/** Queues an experiment. 409 without graders or with too many active experiments; 422 with `missing` when an arm names something absent. */
export async function createExperiment(req: ExperimentRequest): Promise<Experiment> {
  return normalizeExperiment(await send<Experiment>(`${root}/experiments`, 'POST', req))
}

export async function cancelExperiment(id: number): Promise<Experiment> {
  return normalizeExperiment(await send<Experiment>(`${root}/experiments/${id}/cancel`, 'POST'))
}

export async function listExperimentResults(id: number, opts: { arm?: string; caseId?: number; limit?: number; cursor?: string } = {}): Promise<{ results: ExperimentResult[]; nextCursor: string }> {
  const q = new URLSearchParams()
  if (opts.arm) q.set('arm', opts.arm)
  if (opts.caseId) q.set('caseId', String(opts.caseId))
  if (opts.limit) q.set('limit', String(opts.limit))
  if (opts.cursor) q.set('cursor', opts.cursor)
  const qs = q.toString()
  const body = await send<{ results?: ExperimentResult[]; nextCursor?: string }>(`${root}/experiments/${id}/results${qs ? `?${qs}` : ''}`, 'GET')
  return {
    results: (body.results ?? []).map((r) => ({ ...r, toolCalls: r.toolCalls ?? [], grades: r.grades ?? [] })),
    nextCursor: body.nextCursor ?? '',
  }
}

/** The comparison. 409 while the experiment is queued or when it failed before it had a summary. */
export const getExperimentSummary = (id: number) => send<SummaryResponse>(`${root}/experiments/${id}/summary`, 'GET')

/** The per-grader tallies of a summary as a list, whichever shape the server used. */
export function graderTallies(s: ExperimentSummary | undefined): GraderTally[] {
  const g = s?.graders
  if (!g) return []
  if (Array.isArray(g)) return g
  return Object.entries(g).map(([name, t]) => ({ ...t, name: t.name ?? name }))
}

// ── Human review and calibration ─────────────────────────────────────────────

export type ReviewStatus = 'pending' | 'done' | 'closed'

/** One thing to label. Blind: nothing says which arm, repeat or run it came from. */
export interface ReviewItem {
  id: number
  experimentId: number
  caseId: number
  grader: string
  scale: 'passfail' | '1to5'
  instructions?: string
  passAt?: number
  input: string
  reference?: string
  output: string
  status: ReviewStatus
  reviewer?: string
  labelledAt?: string
  score?: number
  pass?: boolean
  comment?: string
}

export interface ReviewQueue {
  status: string
  progress: { pending: number; done: number; closed: number }
  items: ReviewItem[]
}

export interface ReviewLabel { pass?: boolean; stars?: number; comment?: string }

export async function getReviewQueue(experimentId: number, opts: { status?: ReviewStatus; limit?: number } = {}): Promise<ReviewQueue> {
  const q = new URLSearchParams()
  if (opts.status) q.set('status', opts.status)
  if (opts.limit) q.set('limit', String(opts.limit))
  const qs = q.toString()
  const body = await send<Partial<ReviewQueue>>(`${root}/experiments/${experimentId}/review${qs ? `?${qs}` : ''}`, 'GET')
  return {
    status: body.status ?? '',
    progress: { pending: 0, done: 0, closed: 0, ...body.progress },
    items: body.items ?? [],
  }
}

/** Labels an item. 409 when it was labelled or closed already; 403 for the person who started the experiment when independent review is required. */
export const labelReviewItem = (id: number, label: ReviewLabel) => send<ReviewItem>(`${root}/review/${id}`, 'POST', label)

export const closeReview = (experimentId: number) => send<{ closed: number }>(`${root}/experiments/${experimentId}/review/close`, 'POST')

export interface Calibration {
  llmGrader: string
  humanGrader: string
  minPairs: number
  tooFewPairs: boolean
  report?: {
    pairs: number
    agreement: number
    kappa: number
    kappaDefined: boolean
    trustworthy: boolean
    confusion: { bothPass: number; bothFail: number; llmPassHumanFail: number; llmFailHumanPass: number }
    disagreements?: { resultId: number; caseId: number; llmScore: number; humanScore: number; llmPass: boolean; humanPass: boolean }[]
    disagreementsCut?: boolean
  }
}

export function getCalibration(datasetId: number, llm: string, human: string, experimentId?: number): Promise<Calibration> {
  const q = new URLSearchParams({ llm, human })
  if (experimentId) q.set('experimentId', String(experimentId))
  return send<Calibration>(`${root}/datasets/${datasetId}/calibration?${q}`, 'GET')
}

// ── Publish gate ─────────────────────────────────────────────────────────────

export interface EvalGate {
  id: number
  namespace: string
  agent: string
  datasetId: number
  baselineRevision: number
  margin: number
  minCases: number
  minPassRate?: number
  maxRegressions?: number
  budgetUsd: number
  enabled: boolean
  updatedBy: string
  createdAt?: string
  updatedAt?: string
}

export interface GateInput {
  datasetId: number
  baselineRevision: number
  margin?: number
  minCases?: number
  minPassRate?: number
  maxRegressions?: number
  budgetUsd?: number
  enabled?: boolean
}

export type GateStatus = 'passed' | 'failed' | 'missing' | 'stale'

export interface GateDecision {
  status: GateStatus
  reason: string
  experimentId?: number
  required?: { margin?: number; minCases?: number; minPassRate?: number; maxRegressions?: number }
  best?: { cases?: number; passRate?: number; baselinePassRate?: number; diffLo?: number; regressions?: number; nonInferior?: boolean; unreviewed?: number }
}

export interface GateState { gate: EvalGate | null; decision?: GateDecision; overrideAllowed: boolean }

const gateUrl = (ns: string, agent: string) => `${root}/gates/${enc(ns)}/${enc(agent)}`

export async function getGate(ns: string, agent: string): Promise<GateState> {
  const b = await send<Partial<GateState>>(gateUrl(ns, agent), 'GET')
  return { gate: b.gate ?? null, decision: b.decision, overrideAllowed: b.overrideAllowed ?? false }
}
/** Admin only. 409 when the dataset has only llm graders. */
export const putGate = (ns: string, agent: string, g: GateInput) => send<EvalGate>(gateUrl(ns, agent), 'PUT', g)
export const deleteGate = (ns: string, agent: string) => send<void>(gateUrl(ns, agent), 'DELETE')

/** Queues the gate's evaluation: the agent as it is now against the baseline revision. 409 when the agent has no gate. */
export async function evaluateAgent(ns: string, agent: string): Promise<Experiment> {
  return normalizeExperiment(await send<Experiment>(`/api/agents/${enc(ns)}/${enc(agent)}/evaluate`, 'POST'))
}

/** The gate payload of a 409 from publish, or null when the error is something else. */
export function gateFromPublishError(e: unknown): { error: string; gate: GateDecision } | null {
  const err = e as { status?: number; body?: { error?: string; gate?: GateDecision } }
  if (err?.status !== 409 || !err.body?.gate || typeof err.body.gate.status !== 'string') return null
  return { error: err.body.error ?? '', gate: err.body.gate }
}

export const OVERRIDE_REASON = { min: 10, max: 500 }

/** A readable sentence for a failed call, by status. */
export function describeEvalError(e: unknown): string {
  if (!(e instanceof Error)) return String(e)
  const status = (e as { status?: number }).status
  const msg = e.message
  switch (status) {
    case 403: return `You are not allowed to do this. Changing datasets, starting or cancelling experiments needs the builder role, reviewing the runner role, and gates the admin role. ${msg}`.trim()
    case 404: return 'That item was not found. It may have been deleted, or it is in a workspace you cannot use.'
    case 409: return msg || 'That conflicts with what is already saved.'
    case 413: return msg || 'That text is over the size limit.'
    case 400:
    case 422: return msg || 'The values were not accepted.'
    case 503: return 'Evaluations need the platform PostgreSQL store, which is not set up.'
    default: return msg
  }
}
