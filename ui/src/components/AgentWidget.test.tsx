import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

const mockGetMe = vi.fn()
vi.mock('../api/fleet', () => ({ getMe: () => mockGetMe() }))

import { AgentWidget } from './AgentWidget'

const me = (roles: string[]) => ({ userId: 'u', groups: [], tenant: { id: 't', name: 'T' }, workspaces: [{ name: 'A', namespace: 'team-a', primary: true }], roles })
const BUILDER = me(['viewer', 'runner', 'builder'])
const VIEWER = me(['viewer'])

const ID = 'wgt_0123456789abcdef0123456789abcdef'
const KEY = { id: ID, namespace: 'team-a', agent: 'helper', name: 'Help center', allowedOrigins: ['https://www.example.com'], rateLimitPerMin: 30, budgetUsdPerDay: 5, enabled: true, createdBy: 'u', createdAt: '2026-01-01T00:00:00Z' }

type Reply = { status: number; body?: unknown }
let routes: Record<string, Reply>
let current: unknown[]
const calls: { method: string; url: string; body: unknown }[] = []

beforeEach(() => {
  calls.length = 0
  current = [KEY]
  routes = {}
  mockGetMe.mockResolvedValue(BUILDER)
  vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET'
    calls.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined })
    if (method === 'GET' && url.startsWith('/api/widget-keys')) return Promise.resolve(new Response(JSON.stringify(current), { status: 200 }))
    const key = Object.keys(routes).find((k) => { const [m, re] = k.split(' '); return m === method && new RegExp(re).test(url) })
    const r = key ? routes[key] : { status: 404, body: { error: 'not found' } }
    return Promise.resolve(new Response(r.body === undefined ? null : JSON.stringify(r.body), { status: r.status }))
  }))
})
afterEach(() => vi.unstubAllGlobals())

const setup = () => render(<AgentWidget namespace="team-a" name="helper" />)

describe('AgentWidget', () => {
  it('lists keys with pages, limits and the embed snippet', async () => {
    setup()
    const card = await screen.findByRole('listitem', { name: 'Widget key Help center' })
    expect(within(card).getByText('Pages: https://www.example.com')).toBeInTheDocument()
    expect(within(card).getByText(/30 messages a minute and \$5 a day/)).toBeInTheDocument()
    expect(within(card).getByLabelText('Embed snippet of Help center')).toHaveTextContent(`<script src="${window.location.origin}/widget/v1.js" data-key="${ID}" async></script>`)
    expect(calls[0].url).toBe('/api/widget-keys?namespace=team-a&agent=helper')
  })

  it('copies the snippet', async () => {
    const user = userEvent.setup()
    const writeText = vi.spyOn(navigator.clipboard, 'writeText')
    setup()
    await user.click(await screen.findByRole('button', { name: 'Copy snippet of Help center' }))
    expect(writeText).toHaveBeenCalledWith(expect.stringContaining(`data-key="${ID}"`))
  })

  it('creates a key with the listed pages', async () => {
    current = []
    routes['POST /api/widget-keys'] = { status: 201, body: KEY }
    const user = userEvent.setup()
    setup()
    expect(await screen.findByText('No widget keys yet.')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Add a widget key' }))
    await user.type(screen.getByLabelText('Name'), 'Help center')
    await user.type(screen.getByLabelText(/Pages that may show the chat/), 'https://www.example.com{enter}https://shop.example.org')
    await user.click(screen.getByRole('button', { name: 'Create key' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'POST')?.body).toEqual({
      namespace: 'team-a', agent: 'helper', name: 'Help center',
      allowedOrigins: ['https://www.example.com', 'https://shop.example.org'], rateLimitPerMin: 30, budgetUsdPerDay: 5,
    }))
  })

  it('refuses wildcards, empty pages and bad limits before sending', async () => {
    const user = userEvent.setup()
    setup()
    await user.click(await screen.findByRole('button', { name: 'Add a widget key' }))
    await user.click(screen.getByRole('button', { name: 'Create key' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Give the key a name.')
    await user.type(screen.getByLabelText('Name'), 'x')
    await user.click(screen.getByRole('button', { name: 'Create key' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/at least one page address/)
    await user.type(screen.getByLabelText(/Pages that may show the chat/), 'https://*.example.com')
    await user.click(screen.getByRole('button', { name: 'Create key' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/Wildcards are not allowed/)
    await user.clear(screen.getByLabelText(/Pages that may show the chat/))
    await user.type(screen.getByLabelText(/Pages that may show the chat/), 'https://a.example')
    await user.clear(screen.getByLabelText('Daily budget (US dollars)'))
    await user.type(screen.getByLabelText('Daily budget (US dollars)'), '0')
    await user.click(screen.getByRole('button', { name: 'Create key' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/daily budget/)
    expect(calls.some((c) => c.method === 'POST')).toBe(false)
  })

  it('shows what the server refuses', async () => {
    routes['POST /api/widget-keys'] = { status: 409, body: { error: 'publish the agent before adding a widget key' } }
    const user = userEvent.setup()
    setup()
    await user.click(await screen.findByRole('button', { name: 'Add a widget key' }))
    await user.type(screen.getByLabelText('Name'), 'x')
    await user.type(screen.getByLabelText(/Pages that may show the chat/), 'https://a.example')
    await user.click(screen.getByRole('button', { name: 'Create key' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('publish the agent before adding a widget key')
  })

  it('edits the pages and limits, disables and deletes', async () => {
    routes[`PATCH /api/widget-keys/${ID}`] = { status: 200, body: KEY }
    routes[`DELETE /api/widget-keys/${ID}`] = { status: 204 }
    const user = userEvent.setup()
    setup()
    await user.click(await screen.findByRole('button', { name: 'Edit' }))
    const pages = screen.getByLabelText(/Pages that may show the chat/)
    await user.clear(pages)
    await user.type(pages, 'https://docs.example.com')
    await user.click(screen.getByRole('button', { name: 'Save changes' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ allowedOrigins: ['https://docs.example.com'], rateLimitPerMin: 30, budgetUsdPerDay: 5 }))

    await user.click(await screen.findByRole('button', { name: 'Disable' }))
    await waitFor(() => expect(calls.filter((c) => c.method === 'PATCH').slice(-1)[0]?.body).toEqual({ enabled: false }))

    await user.click(screen.getByRole('button', { name: 'Delete' }))
    await user.click(await screen.findByRole('button', { name: 'Delete key' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'DELETE' && c.url === `/api/widget-keys/${ID}`)).toBe(true))
  })

  it('lets a viewer read but not change', async () => {
    mockGetMe.mockResolvedValue(VIEWER)
    setup()
    await screen.findByRole('listitem', { name: 'Widget key Help center' })
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Add a widget key' })).not.toBeInTheDocument())
    expect(screen.queryByRole('button', { name: 'Edit' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Delete' })).not.toBeInTheDocument()
  })

  it('explains a missing PostgreSQL store', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('{"error":"x"}', { status: 503 }))))
    setup()
    expect(await screen.findByRole('alert')).toHaveTextContent(/PostgreSQL/)
  })
})
