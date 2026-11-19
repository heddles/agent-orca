import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  addCase, createDataset, deleteCase, deleteDataset, deleteGrader, describeEvalError, gradeOutput, listCases,
  listDatasets, listGraders, putGrader, updateCase,
  cancelExperiment, closeReview, createExperiment, deleteGate, evaluateAgent, gateFromPublishError, getCalibration,
  getExperiment, getExperimentSummary, getGate, getReviewQueue, graderTallies, labelReviewItem, listExperimentResults,
  listExperiments, putGate,
} from './evals'
import { ApiError, failure } from './builder'

const fetchMock = vi.fn()
const reply = (status: number, body?: unknown) =>
  fetchMock.mockResolvedValueOnce(new Response(body === undefined ? null : JSON.stringify(body), { status }))
const last = () => {
  const [url, init] = fetchMock.mock.calls[fetchMock.mock.calls.length - 1] as [string, RequestInit | undefined]
  return { url, method: init?.method, body: init?.body ? JSON.parse(String(init.body)) : undefined }
}

beforeEach(() => { fetchMock.mockReset(); vi.stubGlobal('fetch', fetchMock) })
afterEach(() => vi.unstubAllGlobals())

describe('datasets', () => {
  it('lists with filters and tolerates an empty body', async () => {
    reply(200, {})
    expect(await listDatasets({ namespace: 'team-a', agent: 'helper' })).toEqual([])
    expect(last()).toMatchObject({ url: '/api/evals/datasets?namespace=team-a&agent=helper', method: 'GET' })
  })
  it('creates and deletes', async () => {
    reply(201, { id: 3, name: 'qa' })
    await createDataset({ name: 'qa', namespace: 'team-a' })
    expect(last()).toMatchObject({ url: '/api/evals/datasets', method: 'POST', body: { name: 'qa', namespace: 'team-a' } })
    reply(204)
    await deleteDataset(3)
    expect(last()).toMatchObject({ url: '/api/evals/datasets/3', method: 'DELETE' })
  })
  it('throws an ApiError carrying the status and message', async () => {
    reply(409, { error: 'dataset already exists' })
    await expect(createDataset({ name: 'qa' })).rejects.toMatchObject({ status: 409, message: 'dataset already exists' })
  })
})

describe('cases', () => {
  it('lists a page with a cursor and tag, defaulting missing fields', async () => {
    reply(200, { cases: [{ id: 1, input: 'hi' }], datasetVersion: 4 })
    const page = await listCases(7, { tag: 'refund', limit: 50, cursor: '9' })
    expect(last().url).toBe('/api/evals/datasets/7/cases?tag=refund&limit=50&cursor=9')
    expect(page.nextCursor).toBe('')
    expect(page.cases[0].tags).toEqual([])
  })
  it('adds, edits and removes a case', async () => {
    reply(201, { added: 1, datasetVersion: 2, cases: [] })
    await addCase(7, { input: 'q', reference: 'a', referenceKind: 'exact' })
    expect(last()).toMatchObject({ url: '/api/evals/datasets/7/cases', method: 'POST', body: { input: 'q', reference: 'a', referenceKind: 'exact' } })
    reply(200, { case: {}, datasetVersion: 3 })
    await updateCase(5, { tags: ['x'] })
    expect(last()).toMatchObject({ url: '/api/evals/cases/5', method: 'PATCH', body: { tags: ['x'] } })
    reply(200, { datasetVersion: 4 })
    expect(await deleteCase(5)).toEqual({ datasetVersion: 4 })
    expect(last().method).toBe('DELETE')
  })
})

describe('graders and grading', () => {
  it('lists, puts and deletes', async () => {
    reply(200, { graders: [{ name: 'g' }] })
    expect(await listGraders(7)).toHaveLength(1)
    reply(201, {})
    await putGrader(7, 'has-word', { kind: 'contains', config: { value: 'x' }, weight: 2, required: true })
    expect(last()).toMatchObject({ url: '/api/evals/datasets/7/graders/has-word', method: 'PUT', body: { kind: 'contains', config: { value: 'x' }, weight: 2, required: true } })
    reply(204)
    await deleteGrader(7, 'has-word')
    expect(last().method).toBe('DELETE')
  })
  it('grades and fills defaults', async () => {
    reply(201, { id: 1, casePass: true, grades: [{ graderId: 1, grader: 'g', score: 1, pass: true, detail: 'ok' }] })
    const r = await gradeOutput(7, { output: 'hello', toolCalls: ['search'] })
    expect(last()).toMatchObject({ url: '/api/evals/datasets/7/grade', method: 'POST', body: { output: 'hello', toolCalls: ['search'] } })
    expect(r.toolCalls).toEqual([])
    expect(r.grades[0].pass).toBe(true)
  })
})

describe('describeEvalError', () => {
  const err = (status: number, message = '') => Object.assign(new Error(message), { status })
  it('explains each status in words', () => {
    expect(describeEvalError(err(403))).toMatch(/builder role/)
    expect(describeEvalError(err(404))).toMatch(/not found/)
    expect(describeEvalError(err(409, 'the dataset is full'))).toBe('the dataset is full')
    expect(describeEvalError(err(422, 'bad regex'))).toBe('bad regex')
    expect(describeEvalError(err(413))).toMatch(/size limit/)
    expect(describeEvalError(err(503))).toMatch(/PostgreSQL/)
  })
})

describe('experiments', () => {
  it('lists with filters and defaults missing arrays', async () => {
    reply(200, { experiments: [{ id: 1, status: 'running' }] })
    const r = await listExperiments({ namespace: 'team-a', datasetId: 7, status: 'running', limit: 20, cursor: 'c' })
    expect(last()).toMatchObject({ url: '/api/evals/experiments?namespace=team-a&datasetId=7&status=running&limit=20&cursor=c', method: 'GET' })
    expect(r.experiments[0].arms).toEqual([])
    expect(r.nextCursor).toBe('')
  })
  it('creates, reads and cancels', async () => {
    const req = { datasetId: 7, arms: [{ agent: 'a' }, { agent: 'a', revision: 2 }], budgetUsd: 5, repeats: 2 }
    reply(201, { id: 9, warnings: ['judge is an arm'] })
    const e = await createExperiment(req)
    expect(last()).toMatchObject({ url: '/api/evals/experiments', method: 'POST', body: req })
    expect(e.warnings).toEqual(['judge is an arm'])
    reply(200, { id: 9, arms: [] })
    await getExperiment(9)
    expect(last().url).toBe('/api/evals/experiments/9')
    reply(200, { id: 9 })
    await cancelExperiment(9)
    expect(last()).toMatchObject({ url: '/api/evals/experiments/9/cancel', method: 'POST' })
  })
  it('carries the 422 missing list on the error', async () => {
    reply(422, { error: 'missing', missing: [{ arm: 'arm2', kind: 'ModelProvider', name: 'gpt', namespace: 'team-a', alternatives: null }] })
    const err = await createExperiment({ datasetId: 1, arms: [], budgetUsd: 1 }).catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err.status).toBe(422)
    expect(err.missing[0]).toMatchObject({ arm: 'arm2', name: 'gpt', alternatives: [] })
  })
  it('reads results with filters and the summary', async () => {
    reply(200, { results: [{ id: 1, output: 'x' }], nextCursor: '5' })
    const r = await listExperimentResults(9, { arm: 'arm2', caseId: 3, limit: 50, cursor: '2' })
    expect(last().url).toBe('/api/evals/experiments/9/results?arm=arm2&caseId=3&limit=50&cursor=2')
    expect(r.results[0].grades).toEqual([])
    expect(r.nextCursor).toBe('5')
    reply(200, { experimentId: 9, status: 'running', live: true, partial: true, summary: {} })
    expect((await getExperimentSummary(9)).live).toBe(true)
    reply(409, { error: 'queued' })
    await expect(getExperimentSummary(9)).rejects.toMatchObject({ status: 409 })
  })
  it('lists grader tallies from either shape', () => {
    expect(graderTallies(undefined)).toEqual([])
    expect(graderTallies({ graders: [{ name: 'a', graded: 1, passed: 1 }] })).toHaveLength(1)
    expect(graderTallies({ graders: { polite: { graded: 4, passed: 3 } } })[0]).toMatchObject({ name: 'polite', graded: 4 })
  })
})

describe('review and calibration', () => {
  it('reads the queue and labels an item', async () => {
    reply(200, { status: 'awaiting_review', progress: { pending: 2 }, items: [{ id: 5 }] })
    const q = await getReviewQueue(9, { status: 'pending', limit: 20 })
    expect(last().url).toBe('/api/evals/experiments/9/review?status=pending&limit=20')
    expect(q.progress).toEqual({ pending: 2, done: 0, closed: 0 })
    reply(200, { id: 5 })
    await labelReviewItem(5, { stars: 4, comment: 'ok' })
    expect(last()).toMatchObject({ url: '/api/evals/review/5', method: 'POST', body: { stars: 4, comment: 'ok' } })
    reply(409, { error: 'already labelled' })
    await expect(labelReviewItem(5, { pass: true })).rejects.toMatchObject({ status: 409 })
    reply(200, { closed: 3 })
    expect(await closeReview(9)).toEqual({ closed: 3 })
    expect(last().url).toBe('/api/evals/experiments/9/review/close')
  })
  it('asks for a calibration', async () => {
    reply(200, { llmGrader: 'judge', humanGrader: 'person', minPairs: 20, tooFewPairs: true })
    await getCalibration(7, 'judge', 'person', 9)
    expect(last().url).toBe('/api/evals/datasets/7/calibration?llm=judge&human=person&experimentId=9')
  })
})

describe('publish gate', () => {
  it('reads a missing gate and a present one', async () => {
    reply(200, { gate: null, overrideAllowed: true })
    expect(await getGate('team-a', 'helper')).toEqual({ gate: null, decision: undefined, overrideAllowed: true })
    expect(last().url).toBe('/api/evals/gates/team-a/helper')
    reply(200, { gate: { id: 1, datasetId: 7 }, decision: { status: 'stale', reason: 'r' }, overrideAllowed: false })
    expect((await getGate('team-a', 'helper')).decision?.status).toBe('stale')
  })
  it('puts, deletes and evaluates', async () => {
    reply(200, { id: 1 })
    await putGate('team-a', 'helper', { datasetId: 7, baselineRevision: 2, margin: 0.1 })
    expect(last()).toMatchObject({ method: 'PUT', body: { datasetId: 7, baselineRevision: 2, margin: 0.1 } })
    reply(204)
    await deleteGate('team-a', 'helper')
    expect(last().method).toBe('DELETE')
    reply(201, { id: 12, status: 'queued' })
    expect((await evaluateAgent('team-a', 'helper')).id).toBe(12)
    expect(last()).toMatchObject({ url: '/api/agents/team-a/helper/evaluate', method: 'POST' })
    reply(409, { error: 'no gate' })
    await expect(evaluateAgent('team-a', 'helper')).rejects.toMatchObject({ status: 409, message: 'no gate' })
    reply(403, { error: 'admin' })
    await expect(putGate('team-a', 'helper', { datasetId: 1, baselineRevision: 1 })).rejects.toMatchObject({ status: 403 })
  })
  it('finds the gate payload on a publish 409', async () => {
    const res = new Response(JSON.stringify({ error: 'gate', gate: { status: 'failed', reason: 'too low', best: { cases: 40 } } }), { status: 409 })
    const err = await failure(res)
    expect(gateFromPublishError(err)).toMatchObject({ gate: { status: 'failed', reason: 'too low' } })
    expect(gateFromPublishError(new ApiError(409, 'name exists'))).toBeNull()
    expect(gateFromPublishError(new ApiError(500, 'x'))).toBeNull()
  })
  it('describes 503 in words', () => {
    expect(describeEvalError(new ApiError(503, 'x'))).toMatch(/PostgreSQL/)
  })
})
