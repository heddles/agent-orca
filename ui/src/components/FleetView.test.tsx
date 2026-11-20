import { describe, expect, it, vi, beforeEach } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import type { Fleet, FleetRow } from '../api/fleet'
import { NamespaceProvider } from '../lib/namespace'

const mockFleet = vi.fn()
const mockSchedules = vi.fn()
vi.mock('../api/schedules', () => ({ listSchedules: (...a: unknown[]) => mockSchedules(...a) }))
vi.mock('../api/fleet', () => ({ getFleet: (...a: unknown[]) => mockFleet(...a) }))

import { FleetView, filterFleet, sortFleet, NO_FLEET_FILTERS, formatDuration, SpendMeter } from './FleetView'

function row(over: Partial<FleetRow>): FleetRow {
  return {
    name: 'helper', namespace: 'team-a', maintainer: 'Support', published: false, owner: 'ui',
    health: { status: 'healthy', reason: 'ready' }, runCount: 0, errorRate: null, p95DurationSeconds: null,
    spend: { usd: 0, agentBudgetUSD: null, tenantBudgetUSD: null }, pendingApprovals: 0, lastEvalScore: null, ...over,
  }
}

const rows = [
  row({ name: 'triage', published: true, owner: 'git', runCount: 10, errorRate: 0.1, p95DurationSeconds: 40, spend: { usd: 1.5, agentBudgetUSD: 2, tenantBudgetUSD: null }, pendingApprovals: 2 }),
  row({ name: 'analyst', maintainer: 'Finance', health: { status: 'failing', reason: 'Ready is false: no model' }, runCount: 3, errorRate: 0.5, p95DurationSeconds: 90 }),
  row({ name: 'coder', maintainer: 'Platform', health: { status: 'degraded', reason: 'paused' }, runCount: 7 }),
]

const names = (rs: FleetRow[]) => rs.map((r) => r.name)

describe('filterFleet', () => {
  it('searches name and maintainer, ignoring case', () => {
    expect(names(filterFleet(rows, { ...NO_FLEET_FILTERS, query: 'FINANCE' }))).toEqual(['analyst'])
    expect(names(filterFleet(rows, { ...NO_FLEET_FILTERS, query: 'tri' }))).toEqual(['triage'])
  })
  it('filters by owner, health and published state', () => {
    expect(names(filterFleet(rows, { ...NO_FLEET_FILTERS, owner: 'git' }))).toEqual(['triage'])
    expect(names(filterFleet(rows, { ...NO_FLEET_FILTERS, health: 'degraded' }))).toEqual(['coder'])
    expect(names(filterFleet(rows, { ...NO_FLEET_FILTERS, published: 'unpublished' }))).toEqual(['analyst', 'coder'])
  })
})

describe('sortFleet', () => {
  it('puts the worst health first', () => {
    expect(names(sortFleet(rows, 'health', 'asc'))).toEqual(['analyst', 'coder', 'triage'])
  })
  it('sorts numbers both ways and keeps missing values last', () => {
    expect(names(sortFleet(rows, 'p95', 'desc'))).toEqual(['analyst', 'triage', 'coder'])
    expect(names(sortFleet(rows, 'p95', 'asc'))).toEqual(['triage', 'analyst', 'coder'])
  })
})

describe('formatting', () => {
  it('formats durations', () => {
    expect(formatDuration(null)).toBe('—')
    expect(formatDuration(42)).toBe('42 s')
    expect(formatDuration(150)).toBe('2 min 30 s')
  })
})

describe('SpendMeter', () => {
  it('states the figures and exposes the ratio to assistive technology', () => {
    render(<SpendMeter usd={1.5} budget={2} scope="agent" />)
    const meter = screen.getByRole('meter')
    expect(meter).toHaveAccessibleName('$1.50 of $2.00 agent budget, 75% used')
    expect(meter).toHaveAttribute('aria-valuenow', '1.5')
    expect(screen.getByText(/\$1\.50 of \$2\.00/)).toBeInTheDocument()
  })
  it('says so in words when the budget is exceeded', () => {
    render(<SpendMeter usd={3} budget={2} scope="agent" />)
    expect(screen.getByText('over')).toBeInTheDocument()
  })
  it('shows only the amount without a budget', () => {
    render(<SpendMeter usd={0.4} budget={null} scope="agent" />)
    expect(screen.queryByRole('meter')).not.toBeInTheDocument()
    expect(screen.getByText('$0.40')).toBeInTheDocument()
  })
})

function renderFleet() {
  return render(<MemoryRouter><NamespaceProvider><FleetView /></NamespaceProvider></MemoryRouter>)
}

const bodyNames = () => within(screen.getAllByRole('rowgroup')[1]).getAllByRole('row').map((r) => within(r).getAllByRole('cell')[0].textContent?.split(/Support|Finance|Platform/)[0])

describe('FleetView', () => {
  beforeEach(() => {
    mockFleet.mockReset()
    mockSchedules.mockReset()
    mockSchedules.mockResolvedValue([])
    mockFleet.mockResolvedValue({ window: '24h', since: '', tenantSpendUSD: 0, tenantBudgetUSD: null, agents: rows } satisfies Fleet)
  })

  it('lists agents worst first, with health badges and links', async () => {
    renderFleet()
    await screen.findByRole('table')
    expect(bodyNames()).toEqual(['analyst', 'coder', 'triage'])
    expect(within(screen.getByRole('table')).getByText('Failing')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'triage' })).toHaveAttribute('href', '/agents/details/team-a/triage')
    expect(screen.getByRole('link', { name: '10 runs of triage' })).toHaveAttribute('href', '/runs?agent=triage')
    expect(screen.getByRole('link', { name: '2 pending approvals for triage' })).toHaveAttribute('href', '/approvals')
  })

  it('badges an agent that has a schedule and filters to scheduled agents', async () => {
    mockSchedules.mockResolvedValue([{
      name: 'nightly', namespace: 'team-a', target: 'coder', targetKind: 'agent', timeZone: 'UTC', suspended: false, state: 'active',
      nextRun: '2026-11-16T07:00:00Z', lastResult: { status: 'failed', at: '2026-11-15T07:00:00Z' },
    }])
    renderFleet()
    const badge = await screen.findByRole('link', { name: /Scheduled: nightly, next .*Last run failed/ })
    expect(badge).toHaveAttribute('href', '/operate/schedules')
    await userEvent.selectOptions(screen.getByLabelText('Filter by schedule'), 'scheduled')
    expect(bodyNames()).toEqual(['coder'])
  })

  it('sorts when a column header is clicked and marks the sort', async () => {
    renderFleet()
    await screen.findByRole('table')
    await userEvent.click(screen.getByRole('button', { name: 'Runs' }))
    expect(screen.getByRole('columnheader', { name: 'Runs ▲' })).toHaveAttribute('aria-sort', 'ascending')
    expect(bodyNames()).toEqual(['analyst', 'coder', 'triage'])
    await userEvent.click(screen.getByRole('button', { name: /^Runs/ }))
    expect(bodyNames()).toEqual(['triage', 'coder', 'analyst'])
  })

  it('filters by health and search, and clears the filters', async () => {
    renderFleet()
    await screen.findByRole('table')
    await userEvent.selectOptions(screen.getByLabelText('Filter by health'), 'degraded')
    expect(bodyNames()).toEqual(['coder'])
    await userEvent.selectOptions(screen.getByLabelText('Filter by health'), '')
    await userEvent.type(screen.getByLabelText('Search agents'), 'nothing-like-this')
    expect(screen.getByRole('status')).toHaveTextContent('No agents match these filters')
    await userEvent.click(screen.getByRole('button', { name: 'Clear filters' }))
    expect(bodyNames()).toHaveLength(3)
  })

  it('reloads for the other time window', async () => {
    renderFleet()
    await screen.findByRole('table')
    expect(mockFleet).toHaveBeenLastCalledWith('24h', '')
    await userEvent.click(screen.getByRole('button', { name: '7 days' }))
    expect(mockFleet).toHaveBeenLastCalledWith('7d', '')
    expect(screen.getByRole('button', { name: '7 days' })).toHaveAttribute('aria-pressed', 'true')
  })

  it('reports a load failure', async () => {
    mockFleet.mockRejectedValue(new Error('HTTP 500'))
    renderFleet()
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not load the fleet: HTTP 500')
  })
})
