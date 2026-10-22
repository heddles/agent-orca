import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, within, act } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import componentSource from './CostDashboard.tsx?raw'
import { emptyCostData, type CostData } from '../api/sse'
import { PHASE_COLOR } from '../lib/phaseColors'

const mockGetCosts = vi.fn()
vi.mock('../api/sse', async () => {
  const actual = await vi.importActual<typeof import('../api/sse')>('../api/sse')
  return { ...actual, getCosts: (...args: unknown[]) => mockGetCosts(...args) }
})

import { CostDashboard } from './CostDashboard'

function costs(over: Partial<CostData> = {}): CostData {
  return {
    ...emptyCostData(),
    totalUSD: '10.0000',
    avgRunUSD: '0.5000',
    inFlightUSD: '0.2500',
    failedRunUSD: '1.0000',
    byAgent: { 'agent-a': '7.0000', 'agent-b': '3.0000' },
    byModel: { 'gpt-4o': '9.0000', unknown: '1.0000' },
    byOutcome: { Succeeded: '9.0000', Failed: '1.0000' },
    byDay: [
      { date: '2026-10-20', usd: '4.0000', byOutcome: { Succeeded: '4.0000' } },
      { date: '2026-10-21', usd: '6.0000', byOutcome: { Succeeded: '5.0000', Failed: '1.0000' } },
    ],
    ...over,
  }
}

beforeEach(() => {
  mockGetCosts.mockReset()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('CostDashboard', () => {
  it('shows the KPI row from the API data', async () => {
    mockGetCosts.mockResolvedValue(costs())
    render(<CostDashboard />)

    await waitFor(() => expect(screen.getByTestId('kpi-Total spend')).toHaveTextContent('$10.0000'))
    expect(screen.getByTestId('kpi-In-flight spend')).toHaveTextContent('$0.2500')
    expect(screen.getByTestId('kpi-Avg cost / run')).toHaveTextContent('$0.5000')
    expect(screen.getByTestId('kpi-Failed-run spend')).toHaveTextContent('$1.0000')
    expect(screen.getByText('10% of total')).toBeInTheDocument()
  })

  it('highlights failed-run spend in the error color only above the threshold', async () => {
    mockGetCosts.mockResolvedValue(costs({ failedRunUSD: '1.0000' })) // 10%
    const { unmount } = render(<CostDashboard />)
    await waitFor(() => screen.getByTestId('kpi-Failed-run spend'))
    expect(screen.getByTestId('kpi-Failed-run spend').style.color).toBe('var(--ds-text-primary)')
    unmount()

    mockGetCosts.mockResolvedValue(costs({ failedRunUSD: '5.0000' })) // 50%
    render(<CostDashboard />)
    await waitFor(() => expect(screen.getByTestId('kpi-Failed-run spend').style.color).toBe('var(--ds-error)'))
  })

  it('renders the untracked banner only when untrackedCount > 0', async () => {
    mockGetCosts.mockResolvedValue(costs({ untrackedCount: 0 }))
    const { unmount } = render(<CostDashboard />)
    await waitFor(() => screen.getByTestId('kpi-Total spend'))
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    expect(screen.queryByText(/no recorded cost/)).not.toBeInTheDocument()
    expect(screen.queryByText('Untracked runs')).not.toBeInTheDocument()
    unmount()

    mockGetCosts.mockResolvedValue(costs({ untrackedCount: 3, untrackedSample: ['ns/a', 'ns/b'] }))
    render(<CostDashboard />)
    const banner = await screen.findByRole('status')
    expect(banner).toHaveTextContent('3 runs have no recorded cost')
    expect(banner).toHaveTextContent('rather than counted as $0')
    expect(screen.getByText('Untracked runs')).toBeInTheDocument()
  })

  it('uses singular wording for one untracked run', async () => {
    mockGetCosts.mockResolvedValue(costs({ untrackedCount: 1, untrackedSample: ['ns/a'] }))
    render(<CostDashboard />)
    expect(await screen.findByRole('status')).toHaveTextContent('1 run has no recorded cost')
  })

  it('"View runs" opens the history and untracked runs open their detail', async () => {
    const user = userEvent.setup()
    const onViewRuns = vi.fn()
    const onNavigateToRun = vi.fn()
    mockGetCosts.mockResolvedValue(costs({ untrackedCount: 2, untrackedSample: ['tenant-a/run-1', 'run-2'] }))
    render(<CostDashboard namespace="fallback-ns" onViewRuns={onViewRuns} onNavigateToRun={onNavigateToRun} />)

    const banner = await screen.findByRole('status')
    await user.click(within(banner).getByRole('button', { name: 'View runs' }))
    expect(onViewRuns).toHaveBeenCalledTimes(1)

    await user.click(within(banner).getByRole('button', { name: 'tenant-a/run-1' }))
    expect(onNavigateToRun).toHaveBeenCalledWith('run-1', 'tenant-a')
    await user.click(within(banner).getByRole('button', { name: 'run-2' }))
    expect(onNavigateToRun).toHaveBeenLastCalledWith('run-2', 'fallback-ns')
  })

  it('does not offer "View runs" without a handler', async () => {
    mockGetCosts.mockResolvedValue(costs({ untrackedCount: 1, untrackedSample: ['ns/a'] }))
    render(<CostDashboard />)
    await screen.findByRole('status')
    expect(screen.queryByRole('button', { name: 'View runs' })).not.toBeInTheDocument()
  })

  it('warns when the run archive is unavailable', async () => {
    mockGetCosts.mockResolvedValue(costs({ archiveUnavailable: true }))
    render(<CostDashboard />)
    expect(await screen.findByText(/Run archive unavailable/)).toBeInTheDocument()
  })

  it('stacks daily bars by outcome using the shared phase colors', async () => {
    mockGetCosts.mockResolvedValue(costs())
    const { container } = render(<CostDashboard />)
    await waitFor(() => screen.getByRole('img'))

    const succeeded = container.querySelectorAll<HTMLElement>('[data-outcome="Succeeded"]')
    const failed = container.querySelectorAll<HTMLElement>('[data-outcome="Failed"]')
    expect(succeeded).toHaveLength(2) // both days
    expect(failed).toHaveLength(1) // only the second day
    expect(succeeded[0].style.background).toBe(hexToRgb(PHASE_COLOR.Succeeded))
    expect(failed[0].style.background).toBe(hexToRgb(PHASE_COLOR.Failed))
  })

  it('gives the chart an accessible description and a legend with icon + color cues', async () => {
    mockGetCosts.mockResolvedValue(costs())
    render(<CostDashboard />)
    const chart = await screen.findByRole('img')
    expect(chart.getAttribute('aria-label')).toContain('2026-10-21 $6.0000 (Succeeded $5.0000, Failed $1.0000)')

    const legend = screen.getByRole('list', { name: 'Chart legend' })
    expect(within(legend).getByText('Succeeded')).toBeInTheDocument()
    expect(within(legend).getByText('Failed')).toBeInTheDocument()
    // Each legend entry carries an icon (svg) in addition to the colored swatch.
    for (const item of within(legend).getAllByRole('listitem')) {
      expect(item.querySelector('svg')).not.toBeNull()
    }
  })

  it('switches the breakdown with the tab bar', async () => {
    const user = userEvent.setup()
    mockGetCosts.mockResolvedValue(costs())
    render(<CostDashboard />)

    const panel = await screen.findByRole('tabpanel')
    expect(within(panel).getByText('agent-a')).toBeInTheDocument()
    expect(within(panel).queryByText('gpt-4o')).not.toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: 'By Model' }))
    expect(within(screen.getByRole('tabpanel')).getByText('gpt-4o')).toBeInTheDocument()

    await user.click(screen.getByRole('tab', { name: 'By Outcome' }))
    const outcomePanel = screen.getByRole('tabpanel')
    expect(within(outcomePanel).getByText('Succeeded')).toBeInTheDocument()
    expect(within(outcomePanel).getByText('$9.0000')).toBeInTheDocument()
  })

  it('lists breakdown rows largest first', async () => {
    mockGetCosts.mockResolvedValue(costs({ byAgent: { small: '1.0000', big: '9.0000' } }))
    render(<CostDashboard />)
    const panel = await screen.findByRole('tabpanel')
    const names = within(panel).getAllByText(/^(small|big)$/).map((e) => e.textContent)
    expect(names).toEqual(['big', 'small'])
  })

  it('renders empty states without data', async () => {
    mockGetCosts.mockResolvedValue(emptyCostData())
    render(<CostDashboard />)
    await waitFor(() => screen.getByTestId('kpi-Total spend'))
    expect(screen.getAllByText('No data').length).toBeGreaterThanOrEqual(2)
    expect(screen.getByTestId('kpi-Total spend')).toHaveTextContent('$0.0000')
    expect(screen.getByText('0% of total')).toBeInTheDocument()
  })

  it('shows an error when the request fails', async () => {
    mockGetCosts.mockRejectedValue(new Error('HTTP 500'))
    render(<CostDashboard />)
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to load costs: HTTP 500')
  })

  it('keeps polling every 30s', async () => {
    vi.useFakeTimers()
    mockGetCosts.mockResolvedValue(costs())
    render(<CostDashboard />)
    await act(async () => { await Promise.resolve() })
    expect(mockGetCosts).toHaveBeenCalledTimes(1)

    await act(async () => { await vi.advanceTimersByTimeAsync(30_000) })
    expect(mockGetCosts).toHaveBeenCalledTimes(2)
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000) })
    expect(mockGetCosts).toHaveBeenCalledTimes(3)
  })

  it('scopes to the selected run and toggles back to all', async () => {
    const user = userEvent.setup()
    mockGetCosts.mockResolvedValue(costs())
    render(<CostDashboard selection={{ kind: 'run', name: 'run-9' }} />)

    await waitFor(() => expect(mockGetCosts).toHaveBeenCalledWith('default', { run: 'run-9' }))
    expect(screen.getByText('Run: run-9')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'This run' })).toHaveAttribute('aria-pressed', 'true')

    await user.click(screen.getByRole('button', { name: 'All' }))
    await waitFor(() => expect(mockGetCosts).toHaveBeenLastCalledWith('default', undefined))
    expect(screen.getByRole('button', { name: 'All' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByText('All runs')).toBeInTheDocument()
  })

  it('scopes to a deployment in its own namespace', async () => {
    mockGetCosts.mockResolvedValue(costs())
    render(<CostDashboard selection={{ kind: 'deployment', name: 'chat', namespace: 'tenant-a' }} />)
    await waitFor(() => expect(mockGetCosts).toHaveBeenCalledWith('tenant-a', { deployment: 'chat' }))
  })
})

describe('CostDashboard source conventions', () => {
  const src: string = componentSource

  it('has no raw hex colors', () => {
    expect(src.match(/#[0-9a-fA-F]{3,8}\b/g)).toBeNull()
  })

  it('has no emoji', () => {
    expect(src).not.toMatch(/\p{Extended_Pictographic}/u)
  })

  it('uses only var(--ds-*) custom properties', () => {
    const vars = src.match(/var\((--[a-z-]+)/g) ?? []
    expect(vars.length).toBeGreaterThan(0)
    for (const v of vars) expect(v).toMatch(/^var\(--ds-/)
  })

  it('does not hand-roll buttons', () => {
    expect(src).not.toMatch(/<button\b/)
  })
})

/** jsdom normalises inline hex colors to rgb(). */
function hexToRgb(hex: string): string {
  const n = parseInt(hex.slice(1), 16)
  return `rgb(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255})`
}
