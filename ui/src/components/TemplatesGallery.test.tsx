import { describe, expect, it, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router'
import type { CatalogEntry } from '../api/catalog'

const mockCatalog = vi.fn()
vi.mock('../api/catalog', async () => {
  const actual = await vi.importActual<typeof import('../api/catalog')>('../api/catalog')
  return { ...actual, getCatalog: (...a: unknown[]) => mockCatalog(...a) }
})

import { TemplateDetail, TemplatesGallery } from './TemplatesGallery'

function entry(over: Partial<CatalogEntry>): CatalogEntry {
  return {
    name: 'x', namespace: 'platform', description: 'd', exampleInputs: [], maintainer: 'Platform', published: false,
    conversational: false, template: true, tools: [], modelSelector: 'auto', runCount: 0, typicalCostUSD: null, ...over,
  }
}

const list = [
  entry({
    name: 'financial-analysis', description: 'Analyses company filings', tools: ['sec-search'], templateGroup: 'financial-analysis',
    exampleInputs: ['Compare ACME and Globex'], members: [{ kind: 'AgentDeployment', name: 'financial-analysis-chat' }, { kind: 'Tool', name: 'sec-search' }],
  }),
  entry({ name: 'coder', description: 'Writes code' }),
  entry({ name: 'ordinary', template: false }),
]

beforeEach(() => { mockCatalog.mockReset() })

describe('TemplatesGallery', () => {
  it('lists only templates, each linking to its details', async () => {
    mockCatalog.mockResolvedValue(list)
    render(<MemoryRouter><TemplatesGallery /></MemoryRouter>)
    const items = await screen.findAllByRole('link')
    expect(items).toHaveLength(2)
    expect(screen.getByRole('link', { name: /financial-analysis/ })).toHaveAttribute('href', '/agents/templates/platform/financial-analysis')
    expect(screen.queryByText('ordinary')).not.toBeInTheDocument()
  })

  it('explains what to do when there are none', async () => {
    mockCatalog.mockResolvedValue([entry({ template: false })])
    render(<MemoryRouter><TemplatesGallery /></MemoryRouter>)
    expect(await screen.findByText('No templates yet')).toBeInTheDocument()
    expect(screen.getByText(/Ask your platform team/)).toBeInTheDocument()
  })
})

describe('TemplateDetail', () => {
  const renderDetail = (name: string) => render(
    <MemoryRouter initialEntries={[`/agents/templates/platform/${name}`]}>
      <Routes><Route path="/agents/templates/:ns/:name" element={<TemplateDetail namespace="platform" name={name} />} /></Routes>
    </MemoryRouter>,
  )

  it('shows what is included and a disabled Use template button that says why', async () => {
    mockCatalog.mockResolvedValue(list)
    renderDetail('financial-analysis')
    expect(await screen.findByRole('heading', { name: 'financial-analysis' })).toBeInTheDocument()
    expect(screen.getByText('financial-analysis-chat')).toBeInTheDocument()
    expect(screen.getByText('Compare ACME and Globex')).toBeInTheDocument()
    const btn = screen.getByRole('button', { name: 'Use template' })
    expect(btn).toBeDisabled()
    expect(btn).toHaveAccessibleDescription('Using a template arrives with the builder.')
  })

  it('handles a missing template', async () => {
    mockCatalog.mockResolvedValue(list)
    renderDetail('gone')
    expect(await screen.findByText('Template not found')).toBeInTheDocument()
  })
})
