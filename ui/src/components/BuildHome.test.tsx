import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import type { CatalogEntry } from '../api/catalog'

const mockCatalog = vi.fn()
vi.mock('../api/catalog', () => ({ getCatalog: (...a: unknown[]) => mockCatalog(...a) }))

import { BuildHome } from './BuildHome'

function entry(over: Partial<CatalogEntry>): CatalogEntry {
  return {
    name: 'x', namespace: 'team-a', description: '', exampleInputs: [], maintainer: '', published: false,
    conversational: false, template: false, tools: [], modelSelector: 'default', runCount: 0, typicalCostUSD: null, ...over,
  }
}
const ui = { owner: 'ui' as const, editable: true, handoff: { pending: false } }
const git = { owner: 'git' as const, editable: false, handoff: { pending: false } }

describe('BuildHome', () => {
  it('lists only UI-owned agents and offers Create and Use a template', async () => {
    mockCatalog.mockResolvedValue([
      entry({ name: 'mine', description: 'My agent', ownership: ui, published: true }),
      entry({ name: 'gitops', ownership: git }),
      entry({ name: 'tpl', ownership: ui, template: true }),
    ])
    render(<MemoryRouter><BuildHome /></MemoryRouter>)
    const link = await screen.findByRole('link', { name: /mine/ })
    expect(link).toHaveAttribute('href', '/build/team-a/mine')
    expect(screen.queryByText('gitops')).not.toBeInTheDocument()
    expect(screen.queryByText('tpl')).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Create an agent' })).toHaveAttribute('href', '/build/new')
    expect(screen.getByRole('link', { name: 'Use a template' })).toHaveAttribute('href', '/agents/templates')
  })

  it('explains an empty list', async () => {
    mockCatalog.mockResolvedValue([])
    render(<MemoryRouter><BuildHome /></MemoryRouter>)
    expect(await screen.findByText('You have not built an agent yet')).toBeInTheDocument()
  })
})
