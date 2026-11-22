import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'

const mockGetMe = vi.fn()
vi.mock('../api/fleet', () => ({ getMe: () => mockGetMe() }))

import { BuilderCheckPanel, scoreOf } from './BuilderCheckPanel'

const me = (roles: string[]) => ({ userId: 'u', groups: [], tenant: { id: 't', name: 'T' }, workspaces: [{ name: 'A', namespace: 'team-a', primary: true }], roles })
const draft = { apiVersion: 'x/v1', kind: 'Agent', metadata: { name: 'helper', namespace: 'team-a' }, spec: { systemPrompt: 'unsaved text' } }

type Reply = { status: number; body?: unknown }
let routes: Record<string, Reply>
const calls: { method: string; url: string; body: unknown }[] = []

const DATASET = { id: 7, namespace: 'team-a', name: 'support', agent: 'helper', version: 3, caseCount: 2 }
const exp = (status: string, extra: object = {}) => ({
  id: 5, namespace: 'team-a', datasetId: 7, datasetVersion: 3, status, arms: [], repeats: 1, budgetUsd: 0.25, spentUsd: 0.02,
  cases: 2, resultsDone: 4, resultsTotal: 4, purpose: 'check', ...extra,
})
const result = (id: number, arm: string, caseId: number, pass: boolean) => ({
  id, datasetId: 7, caseId, arm, output: 'x', toolCalls: [], costUsd: 0.004, latencyMs: 1200, casePass: pass, createdAt: '', phase: 'Succeeded',
  grades: [{ graderId: 1, grader: 'g', score: pass ? 1 : 0, pass, detail: '' }],
})
const arm = (label: string, passed: number) => ({ label, cases: 2, passed, passRate: passed / 2, costTotalUsd: 0.01, hasLatency: true, latencyP50Ms: 900 })

beforeEach(() => {
  calls.length = 0
  routes = { 'GET /api/evals/datasets(\\?.*)?$': { status: 200, body: { datasets: [DATASET] } } }
  mockGetMe.mockResolvedValue(me(['viewer', 'runner', 'builder']))
  vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET'
    calls.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined })
    const key = Object.keys(routes).find((k) => { const [m, re] = k.split(' '); return m === method && new RegExp(re).test(url) })
    const r = key ? routes[key] : { status: 404, body: { error: 'not found' } }
    return Promise.resolve(new Response(r.body === undefined ? null : JSON.stringify(r.body), { status: r.status }))
  }))
})
afterEach(() => vi.unstubAllGlobals())

const setup = (blocked = '') => render(<MemoryRouter><BuilderCheckPanel namespace="team-a" name="helper" draft={draft} blocked={blocked} /></MemoryRouter>)

describe('BuilderCheckPanel', () => {
  it('runs the draft as it is on screen and shows each case with a summary against the published version', async () => {
    routes['POST /api/builder/check'] = { status: 201, body: { valid: true, experiment: exp('complete'), comparedWithPublished: true, capped: false } }
    routes['GET /api/evals/experiments/5$'] = { status: 200, body: exp('complete') }
    routes['GET /api/evals/experiments/5/results'] = { status: 200, body: { results: [
      result(1, 'published', 11, true), result(2, 'draft', 11, true), result(3, 'published', 12, true), result(4, 'draft', 12, false),
    ] } }
    routes['GET /api/evals/experiments/5/summary'] = { status: 200, body: { experimentId: 5, status: 'complete', live: false, partial: false, summary: {
      comparison: { baseline: 'published', arms: [arm('published', 2), arm('draft', 1)], comparisons: [{ verdict: { verdict: 'worse' }, regressedCases: [12] }] },
    } } }
    routes['GET /api/evals/datasets/7/cases'] = { status: 200, body: { cases: [{ id: 11, input: 'How do I reset?' }, { id: 12, input: 'Cancel my order' }] } }
    setup()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Run check' }))

    const post = calls.find((c) => c.method === 'POST')
    expect(post?.body).toMatchObject({ datasetId: 7, budgetUsd: 0.25, agent: { spec: { systemPrompt: 'unsaved text' } } })
    const result_ = await screen.findByLabelText('Check result')
    expect(result_).toHaveTextContent('The draft passed 1 of 2 cases (50%)')
    expect(result_).toHaveTextContent('The published version passed 2 of 2')
    expect(result_).toHaveTextContent('the draft is: worse')
    expect(result_).toHaveTextContent('1 case(s) got worse')
    const rows = within(result_).getAllByRole('row')
    expect(rows).toHaveLength(3)
    await waitFor(() => expect(within(result_).getAllByRole('row')[1]).toHaveTextContent('How do I reset?'))
    expect(rows[1]).toHaveTextContent('Pass')
    expect(rows[2]).toHaveTextContent('Fail')
    expect(rows[2]).toHaveTextContent('$0.0040')
    expect(rows[2]).toHaveTextContent('1.2 s')
  })

  it('shows the validation errors instead of a result when the draft is invalid', async () => {
    routes['POST /api/builder/check'] = { status: 400, body: { valid: false, errors: ['spec.tools: must be an array'] } }
    setup()
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Run check' }))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('would not accept this draft')
    expect(alert).toHaveTextContent('spec.tools: must be an array')
    expect(screen.queryByLabelText('Check result')).not.toBeInTheDocument()
  })

  it('says when the user is checking too often', async () => {
    routes['POST /api/builder/check'] = { status: 429, body: { error: 'you can start 12 checks an hour; try again in 5 minutes' } }
    setup()
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Run check' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('12 checks an hour')
  })

  it('needs test cases, the builder role and a startable draft', async () => {
    routes['GET /api/evals/datasets(\\?.*)?$'] = { status: 200, body: { datasets: [{ ...DATASET, caseCount: 0 }] } }
    const { unmount } = setup()
    expect(await screen.findByText(/no test cases in this workspace yet/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Run check' })).not.toBeInTheDocument()
    unmount()

    routes['GET /api/evals/datasets(\\?.*)?$'] = { status: 200, body: { datasets: [DATASET] } }
    mockGetMe.mockResolvedValue(me(['viewer']))
    const v = setup()
    expect(await screen.findByText(/needs the builder role/)).toBeInTheDocument()
    v.unmount()

    mockGetMe.mockResolvedValue(me(['viewer', 'runner', 'builder']))
    setup('Fix the YAML first.')
    expect(await screen.findByRole('button', { name: 'Run check' })).toBeDisabled()
  })

  it('does not run with a bad spending limit', async () => {
    setup()
    const user = userEvent.setup()
    const box = await screen.findByLabelText('Spending limit (USD)')
    await user.clear(box)
    await user.type(box, 'abc')
    expect(screen.getByRole('button', { name: 'Run check' })).toBeDisabled()
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(false))
  })

  it('scores a result from the grades that counted', () => {
    expect(scoreOf({ grades: [{ score: 1 }, { score: 0 }, { score: 1, notGraded: true }] } as never)).toBe(0.5)
    expect(scoreOf({ grades: [] } as never)).toBeUndefined()
  })
})
