import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, renderHook, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'

const mockGetMe = vi.fn()
vi.mock('../api/fleet', () => ({ getMe: () => mockGetMe() }))

import { ConnectionsView } from './ConnectionsView'
import { McpOAuthAdmin } from './McpOAuthAdmin'
import { RequiredConnections, useInvokerIdentities } from './RequiredConnections'
import { needsConnection } from '../api/connections'

const me = (roles: string[]) => ({ userId: 'u', groups: [], tenant: { id: 't', name: 'T' }, workspaces: [{ name: 'A', namespace: 'team-a', primary: true }], roles })
const SERVER = { name: 'github', namespace: 'team-a', transport: 'http', toolCount: 3, ready: true, allowedAgents: [], allowApps: false, connectionMode: 'maintainer', appRef: 'gh' }
const SHARED = { ...SERVER, name: 'legacy', connectionMode: 'shared' }

type Reply = { status: number; body?: unknown }
let routes: Record<string, Reply>
const calls: { method: string; url: string; body: unknown }[] = []

beforeEach(() => {
  calls.length = 0
  routes = {}
  mockGetMe.mockResolvedValue(me(['viewer', 'runner']))
  vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET'
    calls.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined })
    const key = Object.keys(routes).find((k) => { const [m, re] = k.split(' '); return m === method && new RegExp(re).test(url) })
    const r = key ? routes[key] : { status: 404, body: { error: 'not found' } }
    return Promise.resolve(new Response(r.body === undefined ? null : JSON.stringify(r.body), { status: r.status }))
  }))
})
afterEach(() => vi.unstubAllGlobals())

const setup = (path = '/connections') => render(<MemoryRouter initialEntries={[path]}><ConnectionsView /></MemoryRouter>)

describe('ConnectionsView', () => {
  it('lists per-user servers only, with the connection state', async () => {
    routes['GET /api/mcpservers'] = { status: 200, body: [SERVER, SHARED] }
    routes['GET /api/connections'] = { status: 200, body: [{ namespace: 'team-a', server: 'github', scopes: ['repo:read'], accountLabel: 'alice', status: 'active', createdAt: '2026-01-01T00:00:00Z' }] }
    setup()
    const card = await screen.findByRole('listitem', { name: 'Connection github' })
    expect(within(card).getByText('Connected')).toBeInTheDocument()
    expect(within(card).getByText(/Connected as alice/)).toBeInTheDocument()
    expect(within(card).getByText(/acts as you for everyone/)).toBeInTheDocument()
    expect(screen.queryByRole('listitem', { name: 'Connection legacy' })).toBeNull()
  })

  it('Connect starts the flow and does not put anything but the URL in the page', async () => {
    routes['GET /api/mcpservers'] = { status: 200, body: [SERVER] }
    routes['GET /api/connections'] = { status: 200, body: [] }
    routes['POST /api/connections/team-a/github/start'] = { status: 200, body: { authorizeUrl: 'https://as.example/authorize?state=abc' } }
    const assign = vi.fn()
    vi.stubGlobal('location', { ...window.location, assign })
    setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Connect github' }))
    await waitFor(() => expect(assign).toHaveBeenCalledWith('https://as.example/authorize?state=abc'))
    expect(calls.find((c) => c.method === 'POST')?.url).toBe('/api/connections/team-a/github/start')
  })

  it('shows the callback outcome and a needs-renewing connection', async () => {
    routes['GET /api/mcpservers'] = { status: 200, body: [SERVER] }
    routes['GET /api/connections'] = { status: 200, body: [{ namespace: 'team-a', server: 'github', scopes: [], status: 'needs_reauth', createdAt: '2026-01-01T00:00:00Z' }] }
    setup('/connections?status=denied&server=team-a%2Fgithub')
    expect(await screen.findByRole('alert')).toHaveTextContent('did not grant access')
    expect(await screen.findByText('Needs renewing')).toBeInTheDocument()
  })

  it('disconnects', async () => {
    routes['GET /api/mcpservers'] = { status: 200, body: [SERVER] }
    routes['GET /api/connections'] = { status: 200, body: [{ namespace: 'team-a', server: 'github', scopes: [], status: 'active', createdAt: '2026-01-01T00:00:00Z' }] }
    routes['DELETE /api/connections/team-a/github'] = { status: 204 }
    setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Disconnect' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'DELETE' && c.url === '/api/connections/team-a/github')).toBe(true))
  })

  it('explains when connections are not set up', async () => {
    routes['GET /api/mcpservers'] = { status: 200, body: [SERVER] }
    routes['GET /api/connections'] = { status: 503, body: { error: 'x' } }
    setup()
    expect(await screen.findByRole('alert')).toHaveTextContent('not set up on this platform')
  })

  it('a viewer sees state but no Connect button', async () => {
    mockGetMe.mockResolvedValue(me(['viewer']))
    routes['GET /api/mcpservers'] = { status: 200, body: [SERVER] }
    routes['GET /api/connections'] = { status: 200, body: [] }
    setup()
    await screen.findByRole('listitem', { name: 'Connection github' })
    await waitFor(() => expect(screen.queryByRole('button', { name: /Connect/ })).toBeNull())
  })
})

describe('McpOAuthAdmin', () => {
  const APP = { namespace: 'team-a', appRef: 'gh', registered: true, clientId: 'cid', hasClientSecret: true, redirectUri: 'https://ui.example/api/connections/callback', scopesAllowed: ['repo:read'], updatedBy: 'root' }

  it('shows the app without a secret, saves with a write-only secret and lists connected users', async () => {
    routes['GET /api/mcp-oauth-apps/team-a/github'] = { status: 200, body: APP }
    routes['GET /api/mcpservers/team-a/github/connections'] = { status: 200, body: { connections: [{ userId: 'bob', status: 'active', scopes: [], createdAt: '2026-01-01T00:00:00Z' }], counts: { active: 1 } } }
    routes['PUT /api/mcp-oauth-apps/team-a/github'] = { status: 200, body: APP }
    routes['DELETE /api/connections/team-a/github'] = { status: 204 }
    render(<McpOAuthAdmin namespace="team-a" name="github" />)
    expect(await screen.findByText('https://ui.example/api/connections/callback')).toBeInTheDocument()
    expect(screen.getByText(/never shown/)).toBeInTheDocument()
    const secret = screen.getByLabelText('Client secret') as HTMLInputElement
    expect(secret.type).toBe('password')
    expect(secret.value).toBe('')
    await userEvent.type(secret, 's3cret')
    await userEvent.click(screen.getByRole('button', { name: 'Update app' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true))
    expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ clientId: 'cid', clientSecret: 's3cret', scopesAllowed: ['repo:read'] })
    await waitFor(() => expect((screen.getByLabelText('Client secret') as HTMLInputElement).value).toBe(''))
    await userEvent.click(within(screen.getByLabelText('Connection of bob')).getByRole('button', { name: 'Revoke' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'DELETE' && c.url.endsWith('?user=bob'))).toBe(true))
  })
})

describe('RequiredConnections', () => {
  it('renders nothing when nothing is required and a link when it is', () => {
    const { container, rerender } = render(<MemoryRouter><RequiredConnections required={[]} /></MemoryRouter>)
    expect(container).toBeEmptyDOMElement()
    rerender(<MemoryRouter><RequiredConnections required={[{ namespace: 'n', server: 'github', mode: 'invoker', connected: false }]} /></MemoryRouter>)
    expect(screen.getByRole('alert')).toHaveTextContent('Connect github to run this agent')
    expect(screen.getByRole('link', { name: 'Open Connections' })).toHaveAttribute('href', '/connections')
  })
})

describe('invoker connections', () => {
  it('lists invoker servers on Connections with the invoker wording', async () => {
    routes['GET /api/mcpservers'] = { status: 200, body: [{ ...SERVER, name: 'drive', connectionMode: 'invoker' }, SHARED] }
    routes['GET /api/connections'] = { status: 200, body: [] }
    setup()
    const item = await screen.findByLabelText('Connection drive')
    expect(item).toHaveTextContent('acts as whoever starts the run')
    expect(item).toHaveTextContent('Scheduled and automatic runs')
    expect(screen.queryByLabelText('Connection legacy')).toBeNull()
  })

  it('labels the identity of connected invoker servers as "<server> as <user> (invoker)"', async () => {
    routes['GET /api/me/connections/required'] = { status: 200, body: { servers: [
      { namespace: 'team-a', server: 'github', mode: 'invoker', connected: true },
      { namespace: 'team-a', server: 'other', mode: 'maintainer', connected: true },
    ], required: [] } }
    routes['GET /api/connections'] = { status: 200, body: [{ namespace: 'team-a', server: 'github', scopes: [], accountLabel: 'alice', status: 'active', createdAt: '2026-01-01T00:00:00Z' }] }
    const { result } = renderHook(() => useInvokerIdentities('team-a', 'reporter'))
    await waitFor(() => expect(result.current).toEqual(['github as alice (invoker)']))
    render(<MemoryRouter><RequiredConnections required={[]} identities={result.current} /></MemoryRouter>)
    expect(screen.getByText(/github as alice \(invoker\)/)).toBeInTheDocument()
  })

  it('knows the connect question the router asks', () => {
    expect(needsConnection('This agent needs your github account. Connect it under Connections, then answer here to continue.')).toBe(true)
    expect(needsConnection('Which branch?')).toBe(false)
    expect(needsConnection(undefined)).toBe(false)
  })
})
