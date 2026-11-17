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
const mockWorkspaces = vi.hoisted(() => ({ current: [{ name: 'Mine', namespace: 'team-a', primary: true }] }))
vi.mock('../api/fleet', () => ({
  getMe: () => Promise.resolve({ userId: 'u', groups: [], tenant: { id: 't', name: 'T' }, roles: ['builder'], workspaces: mockWorkspaces.current }),
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

beforeEach(() => {
  mockCatalog.mockReset(); mockUse.mockReset()
  mockWorkspaces.current = [{ name: 'Mine', namespace: 'team-a', primary: true }]
})

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

  describe('when the template needs something the workspace lacks', () => {
    const missingModel = { kind: 'ModelSelector', name: 'default', namespace: 'shared', alternatives: ['team-sel', 'other-sel'], replaceKey: 'ModelSelector/default' }
    const renderWithBuilder = () => render(
      <MemoryRouter initialEntries={['/agents/templates/platform/financial-analysis']}>
        <Routes>
          <Route path="/agents/templates/:ns/:name" element={<TemplateDetail namespace="platform" name="financial-analysis" />} />
          <Route path="/build/:ns/:name" element={<div>builder opened</div>} />
          <Route path="/settings/models" element={<div>models page</div>} />
        </Routes>
      </MemoryRouter>,
    )
    const open = async (user: ReturnType<typeof userEvent.setup>) => {
      await user.click(await screen.findByRole('button', { name: 'Use template' }))
      await user.click(screen.getByRole('button', { name: 'Make my copy' }))
    }

    it('lets the user pick a stand-in, then retries with replace and opens the copy', async () => {
      mockCatalog.mockResolvedValue(list)
      const { ApiError } = await import('../api/builder')
      mockUse.mockRejectedValueOnce(new ApiError(422, 'needs a model', [], undefined, [missingModel]))
      mockUse.mockResolvedValueOnce({ agent: { metadata: { name: 'financial-analysis', namespace: 'team-a' } }, created: [] })
      const user = userEvent.setup()
      renderWithBuilder()
      await open(user)
      expect(await screen.findByText(/needs a model that isn.t set up in Mine yet/)).toBeInTheDocument()
      const retry = screen.getByRole('button', { name: 'Try again' })
      expect(retry).toBeDisabled()
      await user.selectOptions(screen.getByLabelText(/Model to use instead of/), 'team-sel')
      expect(retry).toBeEnabled()
      await user.click(retry)
      await waitFor(() => expect(mockUse).toHaveBeenLastCalledWith('platform', 'financial-analysis', {
        name: 'financial-analysis', namespace: 'team-a', replace: { 'ModelSelector/default': 'team-sel' },
      }))
      expect(await screen.findByText('builder opened')).toBeInTheDocument()
    })

    it('says an administrator must add one when there is no alternative, and keeps Try again off', async () => {
      mockCatalog.mockResolvedValue(list)
      const { ApiError } = await import('../api/builder')
      mockUse.mockRejectedValue(new ApiError(422, 'x', [], undefined, [{ ...missingModel, alternatives: [] }]))
      const user = userEvent.setup()
      renderWithBuilder()
      await open(user)
      expect(await screen.findByText(/An administrator needs to add one/)).toBeInTheDocument()
      expect(screen.getByRole('link', { name: 'Settings → Models' })).toHaveAttribute('href', '/settings/models')
      expect(screen.getByRole('button', { name: 'Try again' })).toBeDisabled()
      expect(screen.queryByRole('combobox', { name: /instead of/ })).not.toBeInTheDocument()
    })

    it('shows a readable message for a 422 without missing, never raw JSON', async () => {
      mockCatalog.mockResolvedValue(list)
      const { ApiError } = await import('../api/builder')
      mockUse.mockRejectedValue(new ApiError(422, 'spec.tools[0]: tool not found; spec.x: bad'))
      const user = userEvent.setup()
      renderWithBuilder()
      await open(user)
      expect(await screen.findByText('spec.tools[0]: tool not found; spec.x: bad')).toBeInTheDocument()
      expect(screen.queryByText(/\{/)).not.toBeInTheDocument()
    })

    it('clears the state when the workspace changes', async () => {
      mockCatalog.mockResolvedValue(list)
      mockWorkspaces.current = [{ name: 'Mine', namespace: 'team-a', primary: true }, { name: 'Other', namespace: 'team-b', primary: false }]
      const { ApiError } = await import('../api/builder')
      mockUse.mockRejectedValue(new ApiError(422, 'x', [], undefined, [missingModel]))
      const user = userEvent.setup()
      renderWithBuilder()
      await user.click(await screen.findByRole('button', { name: 'Use template' }))
      await user.click(await screen.findByRole('option', { name: 'Other' }).then(() => screen.getByRole('button', { name: 'Make my copy' })))
      await screen.findByText(/set up in Mine yet/)
      await user.selectOptions(screen.getByLabelText('Workspace'), 'team-b')
      expect(screen.queryByText(/set up in/)).not.toBeInTheDocument()
      expect(screen.getByRole('button', { name: 'Make my copy' })).toBeEnabled()
    })
  })

  it('turns a failed JSON body into a readable message in the API client', async () => {
    const { createFromTemplate } = await vi.importActual<typeof import('../api/builder')>('../api/builder')
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(
      JSON.stringify({ errors: ['a is missing', 'b is wrong'], missing: [{ kind: 'Tool', name: 't', namespace: 'n', replaceKey: 'Tool/t' }] }),
      { status: 422 },
    ))))
    const err = await createFromTemplate('p', 't', { name: 'x' }).catch((e: unknown) => e) as import('../api/builder').ApiError
    expect(err.message).toBe('a is missing; b is wrong')
    expect(err.missing?.[0].alternatives).toEqual([])
    vi.unstubAllGlobals()
  })

  it('handles a missing template', async () => {
    mockCatalog.mockResolvedValue(list)
    renderDetail('gone')
    expect(await screen.findByText('Template not found')).toBeInTheDocument()
  })
})
