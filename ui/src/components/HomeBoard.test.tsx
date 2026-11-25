import { describe, expect, it, vi, beforeEach } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import type { ScheduleSummary } from '../api/schedules'

const mockMe = vi.fn()
const mockSchedules = vi.fn()
const mockRuns = vi.fn()
const mockGetRun = vi.fn()
const mockArchived = vi.fn()
vi.mock('../api/fleet', () => ({ getMe: () => mockMe(), getFleet: vi.fn(() => Promise.resolve({ agents: [] })) }))
vi.mock('../api/schedules', () => ({ listSchedules: () => mockSchedules() }))
vi.mock('../api/sse', async () => ({
  ...(await vi.importActual<typeof import('../api/sse')>('../api/sse')),
  listRuns: () => mockRuns(),
  getRun: (...a: unknown[]) => mockGetRun(...a),
  getRunHistoryDetail: (...a: unknown[]) => mockArchived(...a),
}))
// The user's own widgets have their own tests.
vi.mock('./MyWork', () => ({ NeedsYou: () => <p>needs-you body</p>, RecentRuns: () => <p>recent body</p> }))
vi.mock('../api/catalog', () => ({ getCatalog: () => Promise.resolve([]), getAgentManifest: vi.fn(), startAgentRun: vi.fn() }))

import { HomeBoard } from './HomeBoard'

function schedule(over: Partial<ScheduleSummary>): ScheduleSummary {
  return {
    name: 'morning', namespace: 'team-a', target: 'watcher', targetKind: 'agent', schedule: '0 8 * * *', timeZone: 'UTC',
    concurrencyPolicy: 'Forbid', suspended: false, state: 'active', lastResult: { status: 'none' }, consecutiveFailures: 0,
    active: [], recentSkips: [], ownership: { owner: 'git', editable: false, handoff: false } as unknown as ScheduleSummary['ownership'], ...over,
  }
}

const renderHome = () => render(<MemoryRouter><HomeBoard /></MemoryRouter>)
const titles = () => screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent)

beforeEach(() => {
  vi.clearAllMocks()
  localStorage.clear()
  mockMe.mockResolvedValue({ userId: 'ada', tenant: null, roles: [] })
  mockSchedules.mockResolvedValue([])
  mockRuns.mockResolvedValue([])
})

describe('HomeBoard', () => {
  it('shows the default widgets', async () => {
    renderHome()
    await screen.findByText('needs-you body')
    expect(titles()).toEqual(['Waiting for you', 'Latest reports', 'Problems', 'Ask an agent', 'Your recent runs'])
    expect(screen.queryByRole('button', { name: /^Remove/ })).not.toBeInTheDocument()
  })

  it('lets a person remove, add and reorder widgets, and remembers it', async () => {
    const first = renderHome()
    await screen.findByText('needs-you body')
    await userEvent.click(screen.getByRole('button', { name: 'Customize' }))
    await userEvent.click(screen.getByRole('button', { name: 'Remove Ask an agent' }))
    await userEvent.click(screen.getByRole('button', { name: 'Add Coming up' }))
    await userEvent.click(screen.getByRole('button', { name: 'Move Coming up earlier' }))
    expect(screen.queryByRole('button', { name: 'Remove Waiting for you' })).not.toBeInTheDocument()
    const arranged = ['Waiting for you', 'Latest reports', 'Problems', 'Coming up', 'Your recent runs']
    expect(titles()).toEqual(arranged)

    first.unmount()
    renderHome()
    await screen.findByText('needs-you body')
    expect(titles()).toEqual(arranged)

    await userEvent.click(screen.getByRole('button', { name: 'Customize' }))
    await userEvent.click(screen.getByRole('button', { name: 'Reset to default' }))
    expect(titles()).toContain('Ask an agent')
  })

  it('offers agent health only to someone who may see operations', async () => {
    mockMe.mockResolvedValue({ userId: 'bo', tenant: { id: 't', name: 't' }, roles: ['viewer'] })
    renderHome()
    await screen.findByText('needs-you body')
    await userEvent.click(screen.getByRole('button', { name: 'Customize' }))
    expect(screen.getByRole('button', { name: 'Add Coming up' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Add Agent health' })).not.toBeInTheDocument()
  })

  it('previews the latest scheduled report and opens it in place', async () => {
    mockSchedules.mockResolvedValue([schedule({ lastResult: { status: 'succeeded', at: new Date().toISOString(), run: 'r1' } })])
    mockGetRun.mockResolvedValue({ input: 'Daily report', output: '**All** quiet.' })
    renderHome()
    const list = await screen.findByRole('list', { name: 'Latest scheduled reports' })
    expect(await within(list).findByText('All quiet.')).toBeInTheDocument()
    expect(within(list).getByRole('link', { name: 'Open run' })).toHaveAttribute('href', '/runs/live/team-a/r1')
    await userEvent.click(within(list).getByRole('button', { name: 'Read report' }))
    expect(within(list).getByRole('button', { name: 'Copy output' })).toBeInTheDocument()
  })

  it('reads a report from the archive once the run has left the cluster', async () => {
    mockSchedules.mockResolvedValue([schedule({ lastResult: { status: 'succeeded', run: 'old' } })])
    mockGetRun.mockRejectedValue(new Error('404'))
    mockArchived.mockResolvedValue({ input: 'Daily report', output: 'From the archive.' })
    renderHome()
    expect(await screen.findByText('From the archive.')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Open run' })).toHaveAttribute('href', '/runs/history/team-a/old')
  })

  it('takes the answer from the archive when the live run no longer carries it', async () => {
    mockSchedules.mockResolvedValue([schedule({ lastResult: { status: 'succeeded', run: 'r2' } })])
    mockGetRun.mockResolvedValue({ input: 'Daily report', output: '' })
    mockArchived.mockResolvedValue({ input: 'Daily report', output: 'Kept in the archive.' })
    renderHome()
    expect(await screen.findByText('Kept in the archive.')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Open run' })).toHaveAttribute('href', '/runs/live/team-a/r2')
  })

  it('still links to a live run that has no answer anywhere', async () => {
    mockSchedules.mockResolvedValue([schedule({ lastResult: { status: 'failed', run: 'r3', reason: 'timed out' } })])
    mockGetRun.mockResolvedValue({ input: 'Daily report' })
    mockArchived.mockRejectedValue(new Error('503'))
    renderHome()
    expect(await screen.findByRole('link', { name: 'Open run' })).toHaveAttribute('href', '/runs/live/team-a/r3')
    expect(screen.getByText('timed out')).toBeInTheDocument()
  })

  it('lists the last day’s failed runs and broken schedules', async () => {
    const run = (name: string, hours: number) => ({
      name, namespace: 'team-a', agentRef: 'coder', phase: 'Failed', spendUSD: '', restartCount: 0,
      input: `ask ${name}`, startTime: new Date(Date.now() - hours * 3600000).toISOString(),
    })
    mockRuns.mockResolvedValue([run('fresh', 1), run('stale', 30)])
    mockSchedules.mockResolvedValue([schedule({ name: 'nightly', state: 'targetMissing' })])
    renderHome()
    const list = await screen.findByRole('list', { name: 'Problems' })
    expect(await within(list).findByRole('link', { name: 'ask fresh' })).toBeInTheDocument()
    expect(within(list).queryByRole('link', { name: 'ask stale' })).not.toBeInTheDocument()
    expect(within(list).getByRole('link', { name: /Schedule nightly points at something/ })).toBeInTheDocument()
  })

  it('does not claim all is well when the data could not be loaded', async () => {
    mockRuns.mockRejectedValue(new Error('HTTP 500'))
    renderHome()
    expect(await screen.findByText(/Could not load runs: HTTP 500/)).toBeInTheDocument()
    expect(screen.queryByText(/every schedule is in order/)).not.toBeInTheDocument()
  })
})
