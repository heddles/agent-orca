import { describe, expect, it, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router'
import type { CatalogEntry } from '../api/catalog'

const mockCatalog = vi.fn()
vi.mock('../api/catalog', async () => {
  const actual = await vi.importActual<typeof import('../api/catalog')>('../api/catalog')
  return { ...actual, getCatalog: (...a: unknown[]) => mockCatalog(...a) }
})

const mockUse = vi.fn()
vi.mock('../api/builder', async () => {
  const actual = await vi.importActual<typeof import('../api/builder')>('../api/builder')
  return { ...actual, createFromTemplate: (...a: unknown[]) => mockUse(...a) }
})
vi.mock('../api/fleet', () => ({
  getMe: () => Promise.resolve({ userId: 'u', groups: [], tenant: { id: 't', name: 'T' }, roles: ['builder'], workspaces: [{ name: 'Mine', namespace: 'team-a', primary: true }] }),
}))

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

beforeEach(() => { mockCatalog.mockReset(); mockUse.mockReset() })

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

  it('shows what is included and an enabled Use template button', async () => {
    mockCatalog.mockResolvedValue(list)
    renderDetail('financial-analysis')
    expect(await screen.findByRole('heading', { name: 'financial-analysis' })).toBeInTheDocument()
    expect(screen.getByText('financial-analysis-chat')).toBeInTheDocument()
    expect(screen.getByText('Compare ACME and Globex')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Use template' })).toBeEnabled()
  })

  it('makes a copy and opens it in the builder', async () => {
    mockCatalog.mockResolvedValue(list)
    mockUse.mockResolvedValue({ agent: { metadata: { name: 'my-fin', namespace: 'team-a' } }, created: [] })
    const user = userEvent.setup()
    render(
      <MemoryRouter initialEntries={['/agents/templates/platform/financial-analysis']}>
        <Routes>
          <Route path="/agents/templates/:ns/:name" element={<TemplateDetail namespace="platform" name="financial-analysis" />} />
          <Route path="/build/:ns/:name" element={<div>builder opened</div>} />
        </Routes>
      </MemoryRouter>,
    )
    await user.click(await screen.findByRole('button', { name: 'Use template' }))
    const name = screen.getByLabelText('Name for your agent')
    await user.clear(name)
    await user.type(name, 'my-fin')
    await user.click(screen.getByRole('button', { name: 'Make my copy' }))
    await waitFor(() => expect(mockUse).toHaveBeenCalledWith('platform', 'financial-analysis', { name: 'my-fin', namespace: 'team-a' }))
    expect(await screen.findByText('builder opened')).toBeInTheDocument()
  })

  it('says so when the name is taken, and rejects a bad name before calling', async () => {
    mockCatalog.mockResolvedValue(list)
    const { ApiError } = await import('../api/builder')
    mockUse.mockRejectedValue(new ApiError(409, 'exists'))
    const user = userEvent.setup()
    renderDetail('financial-analysis')
    await user.click(await screen.findByRole('button', { name: 'Use template' }))
    const name = screen.getByLabelText('Name for your agent')
    await user.clear(name)
    await user.type(name, 'Bad Name')
    await user.click(screen.getByRole('button', { name: 'Make my copy' }))
    expect(await screen.findByText(/lower-case letters/, { selector: '[role=alert]' })).toBeInTheDocument()
    expect(mockUse).not.toHaveBeenCalled()
    await user.clear(name)
    await user.type(name, 'taken')
    await user.click(screen.getByRole('button', { name: 'Make my copy' }))
    expect(await screen.findByText(/already exists/)).toBeInTheDocument()
  })

  it('handles a missing template', async () => {
    mockCatalog.mockResolvedValue(list)
    renderDetail('gone')
    expect(await screen.findByText('Template not found')).toBeInTheDocument()
  })
})
