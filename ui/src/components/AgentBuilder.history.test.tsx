import { describe, expect, it, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes, useParams } from 'react-router'
import type { KubeObject, Ownership } from '../api/builder'

const m = {
  getResource: vi.fn(), updateResource: vi.fn(), dryRunResource: vi.fn(), getBuilderOptions: vi.fn(), getCatalog: vi.fn(),
}
const r = { listRevisions: vi.fn(), getRevision: vi.fn(), restoreRevision: vi.fn() }

vi.mock('../api/builder', async () => {
  const actual = await vi.importActual<typeof import('../api/builder')>('../api/builder')
  return {
    ...actual,
    getResource: (...a: unknown[]) => m.getResource(...a),
    updateResource: (...a: unknown[]) => m.updateResource(...a),
    dryRunResource: (...a: unknown[]) => m.dryRunResource(...a),
    getBuilderOptions: () => m.getBuilderOptions(),
  }
})
vi.mock('../api/catalog', async () => {
  const actual = await vi.importActual<typeof import('../api/catalog')>('../api/catalog')
  return { ...actual, getCatalog: (...a: unknown[]) => m.getCatalog(...a) }
})
vi.mock('../api/revisions', () => ({
  listRevisions: (...a: unknown[]) => r.listRevisions(...a),
  getRevision: (...a: unknown[]) => r.getRevision(...a),
  restoreRevision: (...a: unknown[]) => r.restoreRevision(...a),
}))
vi.mock('../api/fleet', () => ({
  getMe: () => Promise.resolve({
    userId: 'u', groups: [], tenant: { id: 't', name: 'T' }, roles: ['builder'],
    workspaces: [{ name: 'Mine', namespace: 'team-a', primary: true }],
  }),
}))
vi.mock('./BuilderTestPanel', () => ({ BuilderTestPanel: () => <div>test panel</div> }))

import { ApiError } from '../api/builder'
import { AgentBuilder } from './AgentBuilder'

const UI_OWNED: Ownership = { owner: 'ui', editable: true, handoff: { pending: false } }

function agent(prompt = 'Be helpful.', rv = '5', ownership: Ownership = UI_OWNED): KubeObject {
  return {
    apiVersion: 'agentorca.agentorca.io/v1alpha1',
    kind: 'Agent',
    metadata: { name: 'helper', namespace: 'team-a', resourceVersion: rv, annotations: { 'agentorca.io/description': 'Helps' } },
    spec: { modelSelectorRef: 'default', systemPrompt: prompt, tools: [], runtime: { ociRef: 'img:1', framework: 'openai-compatible' } },
    ownership,
  }
}

const OPTIONS = {
  modelSelectors: [{ name: 'default', namespace: 'team-a' }],
  tools: [], mcpServers: [], knowledgeBases: [], guardrailPolicies: [],
}

function RouteBuilder() {
  const { ns = '', name = '' } = useParams()
  return <AgentBuilder key={`${ns}/${name}`} namespace={ns} name={name} />
}

function renderBuilder(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes><Route path="/build/:ns/:name" element={<RouteBuilder />} /></Routes>
    </MemoryRouter>,
  )
}

const REV = (n: number) => ({ revision: n, action: 'update', userId: 'Ada', createdAt: new Date().toISOString(), summary: `Change ${n}` })

beforeEach(() => {
  Object.values(m).forEach((f) => f.mockReset())
  Object.values(r).forEach((f) => f.mockReset())
  m.getBuilderOptions.mockResolvedValue(OPTIONS)
  m.getCatalog.mockResolvedValue([])
  m.dryRunResource.mockResolvedValue({ valid: true, errors: [] })
  m.getResource.mockResolvedValue(agent())
  r.listRevisions.mockResolvedValue({ revisions: [REV(5), REV(4)], next: '' })
  r.getRevision.mockImplementation((_k: string, _ns: string, _n: string, n: number) =>
    Promise.resolve({ ...REV(n), object: agent(`Old ${n}`) }))
})

describe('AgentBuilder: history tab', () => {
  it('opens on History from the URL, shows the version in the header, and Edit is one click away', async () => {
    const user = userEvent.setup()
    renderBuilder('/build/team-a/helper?tab=history')
    expect(await screen.findByRole('tab', { name: 'History', selected: true })).toBeInTheDocument()
    expect((await screen.findAllByText('Version 5')).length).toBe(2) // list row and header chip
    expect(await screen.findByRole('list', { name: /Versions/ })).toBeVisible()
    expect(screen.getByLabelText('Name')).not.toBeVisible()
    await user.click(screen.getByRole('tab', { name: 'Edit' }))
    expect(screen.getByLabelText('Name')).toBeVisible()
  })

  it('refreshes the history after Save', async () => {
    m.updateResource.mockResolvedValue(agent('Be helpful.', '6'))
    const user = userEvent.setup()
    renderBuilder('/build/team-a/helper')
    await screen.findByLabelText('Name')
    await waitFor(() => expect(r.listRevisions).toHaveBeenCalledTimes(1))
    r.listRevisions.mockResolvedValue({ revisions: [REV(6), REV(5)], next: '' })
    await user.type(screen.getByLabelText('Description'), ' more')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(r.listRevisions).toHaveBeenCalledTimes(2))
    expect((await screen.findAllByText('Version 6')).length).toBe(2)
  })

  it('restores a version, reloads the form with it and refreshes the history', async () => {
    r.restoreRevision.mockResolvedValue(agent('Old 4', '7'))
    const user = userEvent.setup()
    renderBuilder('/build/team-a/helper?tab=history')
    await user.click(await screen.findByRole('button', { name: /Version 4/ }))
    await user.click(await screen.findByRole('button', { name: 'Restore this version' }))
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Restore version 4' }))
    await waitFor(() => expect(r.restoreRevision).toHaveBeenCalledWith('agents', 'team-a', 'helper', 4, '5'))
    await waitFor(() => expect(r.listRevisions).toHaveBeenCalledTimes(2))
    expect(await screen.findByText(/Restored version 4/)).toBeInTheDocument()
    await user.click(screen.getByRole('tab', { name: 'Edit' }))
    expect(screen.getByLabelText('Instructions', { selector: 'textarea' })).toHaveValue('Old 4')
  })

  it.each([
    ['a Git-owned agent', { owner: 'git', tool: 'Argo CD', editable: false, handoff: { pending: false } } as Ownership, /history is read-only/],
    ['an agent with an unknown owner', { owner: 'unknown', editable: false, handoff: { pending: false } } as Ownership, /history is read-only/],
    ['an agent with a pending handoff', { owner: 'ui', editable: true, handoff: { pending: true } } as Ownership, /versions cannot be restored until the handoff ends/],
  ])('shows history for %s without a Restore button', async (_n, own, note) => {
    m.getResource.mockResolvedValue(agent('Be helpful.', '5', own))
    const user = userEvent.setup()
    renderBuilder('/build/team-a/helper?tab=history')
    await user.click(await screen.findByRole('button', { name: /Version 4/ }))
    await screen.findByRole('button', { name: 'Compare with the current version' })
    expect(screen.queryByRole('button', { name: 'Restore this version' })).not.toBeInTheDocument()
    expect(screen.getAllByText(note).length).toBeGreaterThan(0)
  })

  it('shows the PostgreSQL message on 501 and no version in the header', async () => {
    r.listRevisions.mockRejectedValue(new ApiError(501, 'revision history needs PostgreSQL'))
    renderBuilder('/build/team-a/helper?tab=history')
    expect(await screen.findByText('Version history is not turned on for this installation. It needs PostgreSQL.')).toBeInTheDocument()
    expect(screen.queryByText(/^Version \d+$/)).not.toBeInTheDocument()
  })
})
