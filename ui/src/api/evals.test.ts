import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  addCase, createDataset, deleteCase, deleteDataset, deleteGrader, describeEvalError, gradeOutput, listCases,
  listDatasets, listGraders, putGrader, updateCase,
} from './evals'

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
