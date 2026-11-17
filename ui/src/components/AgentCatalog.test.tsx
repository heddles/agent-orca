import { describe, expect, it, vi, beforeEach } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import type { CatalogEntry } from '../api/catalog'

const mockCatalog = vi.fn()
vi.mock('../api/catalog', async () => {
  const actual = await vi.importActual<typeof import('../api/catalog')>('../api/catalog')
  return { ...actual, getCatalog: (...a: unknown[]) => mockCatalog(...a) }
})

import { AgentCatalog, filterCatalog, NO_FILTERS, formatCost } from './AgentCatalog'

function entry(over: Partial<CatalogEntry>): CatalogEntry {
  return {
    name: 'helper', namespace: 'team-a', description: 'Helps', exampleInputs: [], maintainer: 'Support',
    published: false, conversational: false, template: false, tools: [], modelSelector: 'auto',
    runCount: 0, typicalCostUSD: null, ...over,
  }
}

const entries = [
  entry({ name: 'triage', description: 'Sorts incoming tickets', maintainer: 'Support', published: true, exampleInputs: ['Triage ticket 42'], typicalCostUSD: 0.2, runCount: 3 }),
  entry({ name: 'analyst', description: 'Reads financial filings', maintainer: 'Finance', tools: ['sec-search'] }),
  entry({ name: 'coder', description: 'Writes code', maintainer: 'Platform', template: true }),
]

describe('filterCatalog', () => {
  it('searches names, descriptions, examples and tools, ignoring case', () => {
    expect(filterCatalog(entries, { ...NO_FILTERS, query: 'FILINGS' }).map((e) => e.name)).toEqual(['analyst'])
    expect(filterCatalog(entries, { ...NO_FILTERS, query: 'ticket 42' }).map((e) => e.name)).toEqual(['triage'])
    expect(filterCatalog(entries, { ...NO_FILTERS, query: 'sec-search' }).map((e) => e.name)).toEqual(['analyst'])
  })
  it('filters by published state and maintainer', () => {
    expect(filterCatalog(entries, { ...NO_FILTERS, published: 'published' }).map((e) => e.name)).toEqual(['triage'])
    expect(filterCatalog(entries, { ...NO_FILTERS, published: 'unpublished' }).map((e) => e.name)).toEqual(['analyst', 'coder'])
    expect(filterCatalog(entries, { ...NO_FILTERS, maintainer: 'Finance' }).map((e) => e.name)).toEqual(['analyst'])
  })
  it('formats cost', () => {
    expect(formatCost(0.2)).toBe('$0.20')
    expect(formatCost(0.004)).toBe('under $0.01')
  })
})

function renderCatalog() {
  return render(<MemoryRouter><AgentCatalog /></MemoryRouter>)
}

describe('AgentCatalog', () => {
  beforeEach(() => { mockCatalog.mockReset() })

  it('shows a card per agent with description, maintainer, state and cost, and leaves templates out', async () => {
    mockCatalog.mockResolvedValue(entries)
    renderCatalog()
    const list = await screen.findByRole('list', { name: 'Agents' })
    const cards = within(list).getAllByRole('link')
    expect(cards).toHaveLength(2)
    const triage = screen.getByRole('link', { name: 'triage, published' })
    expect(triage).toHaveAttribute('href', '/agents/details/team-a/triage')
    expect(within(triage).getByText('Sorts incoming tickets')).toBeInTheDocument()
    expect(within(triage).getByText('Triage ticket 42')).toBeInTheDocument()
    expect(within(triage).getByText('Maintained by: Support')).toBeInTheDocument()
    expect(within(triage).getByText('Typically $0.20 per run')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'analyst, not published' })).toHaveTextContent('No cost data yet')
    expect(screen.queryByText('coder')).not.toBeInTheDocument()
  })

  it('narrows the list by search and filters, and offers a way back', async () => {
    mockCatalog.mockResolvedValue(entries)
    renderCatalog()
    await screen.findByRole('list', { name: 'Agents' })
    await userEvent.type(screen.getByLabelText('Search'), 'financial')
    expect(screen.getAllByRole('link', { name: /,/ })).toHaveLength(1)
    expect(screen.getByRole('status', { name: '' })).toHaveTextContent('1 of 2 agents')

    await userEvent.selectOptions(screen.getByLabelText('Status'), 'published')
    expect(await screen.findByText('No agents match')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Clear search and filters' }))
    expect(screen.getAllByRole('link', { name: /,/ })).toHaveLength(2)
  })

  it('tells the user what to do when there are no agents', async () => {
    mockCatalog.mockResolvedValue([entry({ template: true })])
    renderCatalog()
    expect(await screen.findByText('No agents in this workspace yet')).toBeInTheDocument()
    expect(screen.getByText(/Templates tab/)).toBeInTheDocument()
  })

  it('shows a load failure', async () => {
    mockCatalog.mockImplementation(() => Promise.reject(new Error('HTTP 500')))
    renderCatalog()
    expect(await screen.findByRole('alert')).toHaveTextContent('HTTP 500')
  })
})
