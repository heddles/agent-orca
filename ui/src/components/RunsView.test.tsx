import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

const listRuns = vi.fn()
const listRunHistory = vi.fn()
vi.mock('../api/sse', () => ({
  listRuns: (...a: unknown[]) => listRuns(...a),
  listRunHistory: (...a: unknown[]) => listRunHistory(...a),
}))

import { RunsView } from './RunsView'

const run = (name: string, phase: string, extra: object = {}) => ({
  name, namespace: 'team-a', agentRef: 'coder', phase, spendUSD: '0.10', restartCount: 0, ...extra,
})

beforeEach(() => {
  vi.clearAllMocks()
  listRuns.mockResolvedValue([run('live-1', 'Running'), run('both', 'Succeeded')])
  listRunHistory.mockResolvedValue({
    runs: [run('both', 'Succeeded'), run('old-1', 'Failed')], total: 2, limit: 50, offset: 0,
  })
})

describe('RunsView', () => {
  it('merges live and archived runs and shows a run present in both once', async () => {
    render(<RunsView onNavigateToRun={vi.fn()} />)
    expect(await screen.findByRole('button', { name: 'View run live-1' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'View run old-1' })).toBeInTheDocument()
    expect(screen.getAllByRole('button', { name: 'View run both' })).toHaveLength(1)
    expect(screen.getByText('3 runs found')).toBeInTheDocument()
  })

  it('opens the live view for a live run and the archived view for a past one', async () => {
    const onOpen = vi.fn()
    render(<RunsView onNavigateToRun={onOpen} />)
    await userEvent.click(await screen.findByRole('button', { name: 'View run live-1' }))
    expect(onOpen).toHaveBeenLastCalledWith('live-1', 'team-a', 'live')
    await userEvent.click(screen.getByRole('button', { name: 'View run old-1' }))
    expect(onOpen).toHaveBeenLastCalledWith('old-1', 'team-a', 'archived')
  })

  it('filters by source', async () => {
    render(<RunsView onNavigateToRun={vi.fn()} />)
    await screen.findByRole('button', { name: 'View run live-1' })
    await userEvent.selectOptions(screen.getByLabelText('Filter by source'), 'archived')
    expect(screen.queryByRole('button', { name: 'View run live-1' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'View run old-1' })).toBeInTheDocument()
  })

  it('separates what schedules started from what people started', async () => {
    listRuns.mockResolvedValue([run('mine', 'Running'), run('tick-1', 'Running', { schedule: 'digest', trigger: 'schedule' }), run('manual-1', 'Running', { schedule: 'digest', trigger: 'manual' })])
    render(<RunsView onNavigateToRun={vi.fn()} />)
    await screen.findByRole('button', { name: 'View run tick-1' })
    await userEvent.selectOptions(screen.getByLabelText('Filter by who started the run'), 'schedule')
    expect(screen.getByRole('button', { name: 'View run tick-1' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'View run mine' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'View run old-1' })).not.toBeInTheDocument()
    await userEvent.selectOptions(screen.getByLabelText('Filter by who started the run'), 'people')
    expect(screen.queryByRole('button', { name: 'View run tick-1' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'View run mine' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'View run manual-1' })).toBeInTheDocument()
  })

  it('still lists live runs when run history is not configured', async () => {
    listRunHistory.mockRejectedValue(new Error('HTTP 503'))
    render(<RunsView onNavigateToRun={vi.fn()} />)
    expect(await screen.findByRole('button', { name: 'View run live-1' })).toBeInTheDocument()
    expect(screen.getByRole('note')).toHaveTextContent(/Past runs are not being kept/)
  })

  it('tells a new user what to do when there are no runs', async () => {
    listRuns.mockResolvedValue([])
    listRunHistory.mockResolvedValue({ runs: null, total: 0, limit: 50, offset: 0 })
    render(<RunsView onNavigateToRun={vi.fn()} />)
    const title = await screen.findByText('No runs yet')
    expect(within(title.parentElement!).getByText(/send it a message/i)).toBeInTheDocument()
  })
})
