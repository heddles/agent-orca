import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'

const mockGetMe = vi.fn()
vi.mock('../api/fleet', () => ({ getMe: () => mockGetMe() }))

import { AgentQuality, GateFailure } from './AgentQuality'

const me = (roles: string[]) => ({ userId: 'u', groups: [], tenant: { id: 't', name: 'T' }, workspaces: [{ name: 'A', namespace: 'team-a', primary: true }], roles })
const ADMIN = me(['viewer', 'runner', 'builder', 'admin'])
const BUILDER = me(['viewer', 'runner', 'builder'])
const VIEWER = me(['viewer'])

const GATE = { id: 1, namespace: 'team-a', agent: 'helper', datasetId: 7, baselineRevision: 2, margin: 0.05, minCases: 30, budgetUsd: 5, enabled: true, updatedBy: 'u' }
const decision = (status: string, reason = 'because') => ({ status, reason, experimentId: 42, required: { margin: 0.05, minCases: 30 }, best: { cases: 40, passRate: 0.9, baselinePassRate: 0.88, diffLo: -0.03, regressions: 1 } })

type Reply = { status: number; body?: unknown }
let routes: Record<string, Reply>
const calls: { method: string; url: string; body: unknown }[] = []

beforeEach(() => {
  calls.length = 0
  routes = {}
  mockGetMe.mockResolvedValue(ADMIN)
  vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET'
    calls.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined })
    const key = Object.keys(routes).find((k) => { const [m, re] = k.split(' '); return m === method && new RegExp(re).test(url) })
    const r = key ? routes[key] : { status: 404, body: { error: 'not found' } }
    return Promise.resolve(new Response(r.body === undefined ? null : JSON.stringify(r.body), { status: r.status }))
  }))
})
afterEach(() => vi.unstubAllGlobals())

const setup = (saved = true) => render(<MemoryRouter><AgentQuality namespace="team-a" name="helper" saved={saved} /></MemoryRouter>)
const withGate = (status: string, reason?: string) => {
  routes['GET /api/evals/gates/team-a/helper'] = { status: 200, body: { gate: GATE, decision: decision(status, reason), overrideAllowed: true } }
}

describe('AgentQuality: gate status', () => {
  for (const [status, word] of [['passed', 'Passed'], ['failed', 'Failed'], ['missing', 'Not evaluated'], ['stale', 'Out of date']] as const) {
    it(`shows ${status} with its reason and the numbers`, async () => {
      withGate(status, `reason for ${status}`)
      setup()
      expect(await screen.findByText(word)).toBeInTheDocument()
      expect(screen.getByText(`reason for ${status}`)).toBeInTheDocument()
      expect(screen.getByText(/40 cases, new version 90% against 88%/)).toBeInTheDocument()
      expect(screen.getByRole('link', { name: 'Open experiment 42' })).toHaveAttribute('href', '/evaluate/experiments/42')
    })
  }

  it('says no gate is set', async () => {
    routes['GET /api/evals/gates/team-a/helper'] = { status: 200, body: { gate: null, overrideAllowed: true } }
    setup()
    expect(await screen.findByText(/No gate is set, so publishing is not checked/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Evaluate now' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Set up a gate' })).toBeInTheDocument()
  })

  it('explains 503 and 404', async () => {
    routes['GET /api/evals/gates/team-a/helper'] = { status: 503, body: { error: 'x' } }
    const { unmount } = setup()
    expect(await screen.findByRole('alert')).toHaveTextContent(/PostgreSQL/)
    unmount()
    routes['GET /api/evals/gates/team-a/helper'] = { status: 404, body: { error: 'x' } }
    setup()
    expect(await screen.findByRole('alert')).toHaveTextContent(/not found/)
  })

  it('lets a viewer read only, and a builder evaluate but not edit', async () => {
    withGate('failed')
    mockGetMe.mockResolvedValue(VIEWER)
    const { unmount } = setup()
    await screen.findByText('Failed')
    await waitFor(() => expect(screen.getByText(/Only an administrator can change the gate/)).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: 'Evaluate now' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Edit gate' })).not.toBeInTheDocument()
    unmount()
    mockGetMe.mockResolvedValue(BUILDER)
    setup()
    await screen.findByText('Failed')
    expect(await screen.findByRole('button', { name: 'Evaluate now' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Edit gate' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Remove gate' })).not.toBeInTheDocument()
  })
})

describe('AgentQuality: evaluate now', () => {
  it('queues the evaluation and links to the experiment', async () => {
    const user = userEvent.setup()
    withGate('missing')
    routes['POST /api/agents/team-a/helper/evaluate'] = { status: 201, body: { id: 55, status: 'queued' } }
    setup()
    await user.click(await screen.findByRole('button', { name: 'Evaluate now' }))
    expect(await screen.findByRole('link', { name: 'Follow experiment 55' })).toHaveAttribute('href', '/evaluate/experiments/55')
  })

  it('shows 409 and 422 in words', async () => {
    const user = userEvent.setup()
    withGate('missing')
    routes['POST /api/agents/team-a/helper/evaluate'] = { status: 409, body: { error: 'too many experiments are queued or running' } }
    const { unmount } = setup()
    await user.click(await screen.findByRole('button', { name: 'Evaluate now' }))
    expect(await screen.findByText(/too many experiments are queued or running/)).toBeInTheDocument()
    unmount()
    routes['POST /api/agents/team-a/helper/evaluate'] = { status: 422, body: { error: 'missing', missing: [{ arm: 'baseline', kind: 'Revision', name: '2', namespace: 'team-a', alternatives: [] }] } }
    setup()
    await user.click(await screen.findByRole('button', { name: 'Evaluate now' }))
    expect(await screen.findByRole('group', { name: 'Missing from the workspace' })).toHaveTextContent('Version baseline needs Revision 2')
  })

  it('is off until the agent is saved', async () => {
    withGate('missing')
    setup(false)
    expect(await screen.findByRole('button', { name: 'Evaluate now' })).toBeDisabled()
  })
})

describe('AgentQuality: editing the gate (admin)', () => {
  beforeEach(() => {
    routes['GET /api/evals/datasets'] = { status: 200, body: { datasets: [{ id: 7, namespace: 'team-a', name: 'refunds', version: 3, caseCount: 40 }, { id: 8, namespace: 'team-a', name: 'tone', version: 1, caseCount: 5 }] } }
    routes['GET /api/resources/agents/team-a/helper/revisions'] = { status: 200, body: { revisions: [{ revision: 2, action: 'update', userId: 'u', createdAt: '' }], next: '' } }
  })

  it('creates a gate', async () => {
    const user = userEvent.setup()
    routes['GET /api/evals/gates/team-a/helper'] = { status: 200, body: { gate: null, overrideAllowed: true } }
    routes['PUT /api/evals/gates/team-a/helper'] = { status: 200, body: GATE }
    setup()
    await user.click(await screen.findByRole('button', { name: 'Set up a gate' }))
    const dialog = await screen.findByRole('dialog')
    const save = within(dialog).getByRole('button', { name: 'Save gate' })
    expect(save).toBeDisabled()
    await waitFor(() => expect(within(dialog).getByRole('option', { name: /refunds/ })).toBeInTheDocument())
    await user.selectOptions(within(dialog).getByLabelText('Dataset'), '7')
    await user.type(within(dialog).getByLabelText('Baseline revision'), '2')
    await user.type(within(dialog).getByLabelText(/Lowest pass rate/), '0.8')
    expect(save).toBeEnabled()
    await user.click(save)
    await waitFor(() => expect(calls.find((c) => c.method === 'PUT')).toBeTruthy())
    expect(calls.find((c) => c.method === 'PUT')!.body).toEqual({
      datasetId: 7, baselineRevision: 2, margin: 0.05, minCases: 30, budgetUsd: 5, enabled: true, minPassRate: 0.8,
    })
  })

  it('shows the 409 for a dataset with only judge checks', async () => {
    const user = userEvent.setup()
    withGate('passed')
    routes['PUT /api/evals/gates/team-a/helper'] = { status: 409, body: { error: 'the dataset has only llm graders' } }
    setup()
    await user.click(await screen.findByRole('button', { name: 'Edit gate' }))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Save gate' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/only llm graders.*deterministic or human check/)
  })

  it('shows 403 when saving is refused, and removes a gate', async () => {
    const user = userEvent.setup()
    withGate('passed')
    routes['PUT /api/evals/gates/team-a/helper'] = { status: 403, body: { error: 'admin only' } }
    routes['DELETE /api/evals/gates/team-a/helper'] = { status: 204 }
    setup()
    await user.click(await screen.findByRole('button', { name: 'Edit gate' }))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Save gate' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/not allowed/)
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    await user.click(screen.getByRole('button', { name: 'Remove gate' }))
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove gate' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'DELETE')).toBe(true))
  })
})

describe('GateFailure', () => {
  it('gives an admin an override that needs 10 to 500 characters', async () => {
    const user = userEvent.setup()
    const onOverride = vi.fn()
    render(<MemoryRouter><GateFailure gate={decision('failed') as never} canOverride busy={false} onOverride={onOverride} /></MemoryRouter>)
    await user.click(screen.getByRole('button', { name: /Override the gate/ }))
    const box = screen.getByLabelText('Reason for publishing anyway')
    await user.type(box, 'x'.repeat(501))
    expect(screen.getByRole('button', { name: 'Publish anyway' })).toBeDisabled()
    expect(screen.getByText(/at most 500 characters/)).toBeInTheDocument()
    await user.clear(box)
    await user.type(box, '  needed for the launch  ')
    await user.click(screen.getByRole('button', { name: 'Publish anyway' }))
    expect(onOverride).toHaveBeenCalledWith('needed for the launch')
  })

  it('offers no override when the platform forbids it, and tells missing and stale what to do', () => {
    const { rerender } = render(<MemoryRouter><GateFailure gate={decision('missing') as never} canOverride overrideAllowed={false} busy={false} onOverride={() => {}} /></MemoryRouter>)
    expect(screen.queryByRole('button', { name: /Override the gate/ })).not.toBeInTheDocument()
    expect(screen.getByText(/does not allow publishing past a gate/)).toBeInTheDocument()
    expect(screen.getByText(/Run "Evaluate now"/)).toBeInTheDocument()
    rerender(<MemoryRouter><GateFailure gate={decision('stale') as never} canOverride={false} busy={false} onOverride={() => {}} /></MemoryRouter>)
    expect(screen.getByText(/changed since the last evaluation/)).toBeInTheDocument()
  })
})
