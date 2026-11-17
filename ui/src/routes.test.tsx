import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, within, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, useLocation } from 'react-router'

// The views have their own tests; here they are markers so the tests cover the
// shell: routing, redirects, sidebar state and what each URL renders.
vi.mock('./api/sse', () => ({
  getSystemStatus: vi.fn(() => Promise.resolve({ stateConfigured: true })),
  listApprovals: vi.fn(() => Promise.resolve([{ id: 'a' }, { id: 'b' }])),
}))
const mockGetMe = vi.fn()
vi.mock('./api/fleet', () => ({ getMe: () => mockGetMe() }))
vi.mock('./components/FleetView', () => ({ FleetView: () => <div>view:fleet</div> }))
vi.mock('./components/SystemDashboard', () => ({ SystemDashboard: () => <div>view:overview</div> }))
vi.mock('./components/MyWork', () => ({ MyWork: () => <div>view:home</div> }))
vi.mock('./components/AgentCatalog', () => ({ AgentCatalog: () => <div>view:catalog</div> }))
vi.mock('./components/AgentPage', () => ({
  AgentPage: (p: { namespace: string; name: string; tab: string }) => <div>view:agent {p.namespace}/{p.name} tab:{p.tab}</div>,
}))
vi.mock('./components/TemplatesGallery', () => ({
  TemplatesGallery: () => <div>view:templates</div>,
  TemplateDetail: (p: { namespace: string; name: string }) => <div>view:template {p.namespace}/{p.name}</div>,
}))
vi.mock('./components/BuildHome', () => ({ BuildHome: () => <div>view:build-home</div> }))
vi.mock('./components/AgentBuilder', () => ({
  AgentBuilder: (p: { namespace?: string; name?: string }) => <div>view:builder {p.namespace ? `${p.namespace}/${p.name}` : 'new'}</div>,
}))
vi.mock('./components/ApprovalsView', () => ({ ApprovalsView: () => <div>view:approvals</div> }))
vi.mock('./components/CostDashboard', () => ({ CostDashboard: () => <div>view:costs</div> }))
vi.mock('./components/RoutingInsightsView', () => ({ RoutingInsightsView: () => <div>view:routing</div> }))
vi.mock('./components/SystemStatusPage', () => ({ SystemStatusPage: () => <div>view:status</div> }))
vi.mock('./components/RunsView', () => ({ RunsView: () => <div>view:runs</div> }))
vi.mock('./components/RunView', () => ({ RunView: (p: { runId: string; namespace: string }) => <div>view:live-run {p.namespace}/{p.runId}</div> }))
vi.mock('./components/RunHistoryDetailView', () => ({ RunHistoryDetailView: (p: { runId: string; namespace: string }) => <div>view:archived-run {p.namespace}/{p.runId}</div> }))
vi.mock('./components/DeploymentView', () => ({ DeploymentView: (p: { name: string }) => <div>view:published {p.name}</div> }))
vi.mock('./components/WorkflowView', () => ({ WorkflowView: (p: { name: string }) => <div>view:workflow {p.name}</div> }))
vi.mock('./components/ConfigDetailView', () => ({ ConfigDetailView: (p: { selection: { kind: string; name: string } }) => <div>view:config {p.selection.kind} {p.selection.name}</div> }))
vi.mock('./components/ResourceList', () => ({
  ResourceList: (p: { tab: string; selection: { name: string } | null }) => <div>list:{p.tab} selected:{p.selection?.name ?? 'none'}</div>,
}))
vi.mock('./components/ConfigList', () => ({
  ConfigList: (p: { tab: string; selection: { name: string } | null }) => <div>list:{p.tab} selected:{p.selection?.name ?? 'none'}</div>,
}))

import { AppRoutes } from './routes'
import { NamespaceProvider } from './lib/namespace'

function Where() {
  const l = useLocation()
  return <div data-testid="where">{l.pathname}{l.search}</div>
}

function renderAt(url: string) {
  return render(
    <MemoryRouter initialEntries={[url]}>
      <NamespaceProvider>
        <AppRoutes />
        <Where />
      </NamespaceProvider>
    </MemoryRouter>,
  )
}

const where = () => screen.getByTestId('where').textContent

const meWith = (roles: string[]) => ({ userId: 'ada', groups: [], tenant: { id: 'tnt_a', name: 'a' }, workspaces: [], roles })

beforeEach(() => {
  vi.clearAllMocks()
  // No tenant: authentication is off, so every section shows.
  mockGetMe.mockResolvedValue({ userId: '', groups: [], tenant: null, workspaces: [], roles: [] })
})

describe('deep links', () => {
  it.each([
    ['/', 'view:home'],
    ['/runs', 'view:runs'],
    ['/runs/live/team-a/run-1', 'view:live-run team-a/run-1'],
    ['/runs/history/team-a/run-2', 'view:archived-run team-a/run-2'],
    ['/agents', 'view:catalog'],
    ['/agents/templates', 'view:templates'],
    ['/agents/templates/team-a/coder', 'view:template team-a/coder'],
    ['/agents/details/team-a/helper', 'view:agent team-a/helper tab:run'],
    ['/agents/details/team-a/helper?tab=chat', 'view:agent team-a/helper tab:chat'],
    ['/agents/published/team-a/helper', 'view:published helper'],
    ['/agents/definitions/team-a/coder', 'view:config agent coder'],
    ['/build', 'view:build-home'],
    ['/build/new', 'view:builder new'],
    ['/build/team-a/helper', 'view:builder team-a/helper'],
    ['/approvals', 'view:approvals'],
    ['/operate/fleet', 'view:fleet'],
    ['/operate/costs', 'view:costs'],
    ['/operate/routing', 'view:routing'],
    ['/operate/status', 'view:status'],
    ['/operate/workflows/team-a/flow', 'view:workflow flow'],
    ['/settings/providers/team-a/anthropic', 'view:config modelprovider anthropic'],
    ['/settings/models/team-a/auto', 'view:config modelselector auto'],
    ['/settings/connections/mcp/team-a/github', 'view:config mcpserver github'],
    ['/settings/connections/tools/team-a/calc', 'view:config tool calc'],
    ['/settings/knowledge/team-a/docs', 'view:config knowledgebase docs'],
  ])('%s renders %s', async (url, text) => {
    renderAt(url)
    expect(await screen.findByText(text)).toBeInTheDocument()
    expect(where()).toBe(url)
  })

  it('decodes names with unusual characters', async () => {
    renderAt('/runs/live/team-a/run%2F1')
    expect(await screen.findByText('view:live-run team-a/run/1')).toBeInTheDocument()
  })

  it('highlights the open resource in its list', async () => {
    renderAt('/agents/published/team-a/helper')
    expect(await screen.findByText('list:deployments selected:helper')).toBeInTheDocument()
    expect(screen.getByText('list:agents selected:helper')).toBeInTheDocument()
  })

  it('offers Catalog and Templates tabs on the agents page', async () => {
    renderAt('/agents')
    const sub = await screen.findByRole('navigation', { name: 'Agents' })
    expect(within(sub).getByRole('link', { name: 'Catalog' })).toHaveAttribute('aria-current', 'page')
    await userEvent.click(within(sub).getByRole('link', { name: 'Templates' }))
    expect(await screen.findByText('view:templates')).toBeInTheDocument()
    expect(where()).toBe('/agents/templates')
  })

  it('keeps the platform overview closed until asked for', async () => {
    renderAt('/')
    await screen.findByText('view:home')
    expect(screen.queryByText('view:overview')).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Show' }))
    expect(await screen.findByText('view:overview')).toBeInTheDocument()
  })

  it('keeps Build away from viewers, with a plain explanation', async () => {
    mockGetMe.mockResolvedValue(meWith(['viewer']))
    renderAt('/build/new')
    expect(await screen.findByText('Building agents needs the builder role')).toBeInTheDocument()
    expect(screen.queryByText('view:builder new')).not.toBeInTheDocument()
  })

  it('sends unknown paths home', async () => {
    renderAt('/marketplace')
    expect(await screen.findByText('view:home')).toBeInTheDocument()
    expect(where()).toBe('/')
  })

  it('has no Marketplace entry', async () => {
    renderAt('/')
    await screen.findByText('view:home')
    expect(screen.queryByText(/marketplace/i)).not.toBeInTheDocument()
  })
})

describe('index redirects', () => {
  it('/operate goes to the fleet overview', async () => {
    renderAt('/operate')
    expect(await screen.findByText('view:fleet')).toBeInTheDocument()
    expect(where()).toBe('/operate/fleet')
  })

  it('puts Costs, Model routing and System status next to Fleet', async () => {
    renderAt('/operate')
    const sub = await screen.findByRole('navigation', { name: 'Operate' })
    expect(within(sub).getAllByRole('link').map((l) => l.textContent)).toEqual(
      ['Fleet', 'Costs', 'Model routing', 'System status', 'Workflows'])
  })

  it('/settings goes to providers', async () => {
    renderAt('/settings')
    expect(await screen.findByText('list:modelproviders selected:none')).toBeInTheDocument()
    expect(where()).toBe('/settings/providers')
  })
})

describe('legacy ?tab= links', () => {
  it.each([
    ['approvals', '/approvals', 'view:approvals'],
    ['history', '/runs', 'view:runs'],
    ['runs', '/runs', 'view:runs'],
    ['status', '/operate/status', 'view:status'],
    ['routing', '/operate/routing', 'view:routing'],
    ['workflows', '/operate/workflows', 'list:workflows selected:none'],
    ['deployments', '/agents', 'view:catalog'],
    ['modelselectors', '/settings/models', 'list:modelselectors selected:none'],
    ['marketplace', '/', 'view:home'],
    ['nonsense', '/', 'view:home'],
  ])('/?tab=%s lands on %s', async (tab, path, text) => {
    renderAt(`/?tab=${tab}`)
    expect(await screen.findByText(text)).toBeInTheDocument()
    expect(where()).toBe(path)
  })
})

describe('sidebar', () => {
  it('groups sections by task and marks the current one', async () => {
    renderAt('/runs/live/team-a/run-1')
    const nav = await screen.findByRole('navigation', { name: 'Main' })
    for (const label of ['Home', 'Agents', 'Runs', 'Build', 'Evaluate', 'Operate', 'Settings']) {
      expect(within(nav).getByRole('link', { name: new RegExp(`^${label}`) })).toBeInTheDocument()
    }
    expect(within(nav).getByRole('link', { name: 'Runs' })).toHaveAttribute('aria-current', 'page')
    expect(within(nav).getByRole('link', { name: 'Agents' })).not.toHaveAttribute('aria-current')
    expect(within(nav).getByRole('link', { name: 'Home' })).not.toHaveAttribute('aria-current')
  })

  it.each([
    ['viewer', ['Home', 'Agents', 'Runs', 'Approvals'], ['Build', 'Evaluate', 'Operate', 'Settings']],
    ['runner', ['Home', 'Agents', 'Runs', 'Approvals'], ['Build', 'Evaluate', 'Operate', 'Settings']],
    ['builder', ['Home', 'Agents', 'Runs', 'Approvals', 'Build', 'Evaluate', 'Operate'], ['Settings']],
    ['admin', ['Home', 'Agents', 'Runs', 'Approvals', 'Build', 'Evaluate', 'Operate', 'Settings'], []],
  ])('shows a %s the sections their role allows', async (role, shown, hidden) => {
    const order = ['viewer', 'runner', 'builder', 'admin']
    mockGetMe.mockResolvedValue(meWith(order.slice(0, order.indexOf(role) + 1)))
    renderAt('/')
    const nav = await screen.findByRole('navigation', { name: 'Main' })
    await waitFor(() => {
      for (const label of hidden) expect(within(nav).queryByRole('link', { name: new RegExp(`^${label}`) })).not.toBeInTheDocument()
    })
    for (const label of shown) expect(within(nav).getByRole('link', { name: new RegExp(`^${label}`) })).toBeInTheDocument()
  })

  it('shows everything when /api/me fails', async () => {
    mockGetMe.mockRejectedValue(new Error('HTTP 500'))
    renderAt('/')
    const nav = await screen.findByRole('navigation', { name: 'Main' })
    for (const label of ['Build', 'Evaluate', 'Operate', 'Settings']) {
      expect(within(nav).getByRole('link', { name: new RegExp(`^${label}`) })).toBeInTheDocument()
    }
  })

  it('keeps Operate active on its sub-pages', async () => {
    renderAt('/operate/status')
    const nav = await screen.findByRole('navigation', { name: 'Main' })
    expect(within(nav).getByRole('link', { name: 'Operate' })).toHaveAttribute('aria-current', 'page')
  })

  it('shows the pending approval count on Approvals', async () => {
    renderAt('/')
    expect(await screen.findByRole('link', { name: 'Approvals, 2 pending' })).toBeInTheDocument()
  })

  it('navigates without a page load when a section is clicked', async () => {
    renderAt('/')
    await userEvent.click(await screen.findByRole('link', { name: 'Runs' }))
    expect(await screen.findByText('view:runs')).toBeInTheDocument()
    expect(where()).toBe('/runs')
  })

  it('does not persist navigation in sessionStorage', async () => {
    renderAt('/runs')
    await screen.findByText('view:runs')
    expect(sessionStorage.getItem('agentorca-nav-state')).toBeNull()
  })
})

describe('top bar', () => {
  it('offers a workspace switcher, theme toggle and an account menu with sign out', async () => {
    renderAt('/')
    expect(await screen.findByRole('combobox', { name: 'Workspace' })).toHaveValue('')
    expect(screen.getByRole('button', { name: /switch to (light|dark) theme/i })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Account menu' }))
    expect(screen.getByRole('menuitem', { name: 'Sign out' })).toHaveAttribute('href', '/oauth/logout')
    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('menuitem', { name: 'Sign out' })).not.toBeInTheDocument())
  })

  it('has a skip link to the main content', async () => {
    renderAt('/')
    expect(await screen.findByRole('link', { name: 'Skip to content' })).toHaveAttribute('href', '#main-content')
  })
})
