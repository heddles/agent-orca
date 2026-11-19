import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

const mockGetMe = vi.fn()
vi.mock('../api/fleet', () => ({ getMe: () => mockGetMe() }))

import { AgentTriggers } from './AgentTriggers'

const me = (roles: string[]) => ({ userId: 'u', groups: [], tenant: { id: 't', name: 'T' }, workspaces: [{ name: 'A', namespace: 'team-a', primary: true }], roles })
const BUILDER = me(['viewer', 'runner', 'builder'])
const VIEWER = me(['viewer'])

const ID = 'trg_0123456789abcdef0123456789abcdef'
const TRIGGER = { id: ID, namespace: 'team-a', agent: 'helper', name: 'GitHub issues', inputPath: 'issue.title', rateLimitPerMin: 30, enabled: true, createdBy: 'u', createdAt: '2026-01-01T00:00:00Z', fireCount: 0 }

type Reply = { status: number; body?: unknown }
let routes: Record<string, Reply>
let current: unknown[]
const calls: { method: string; url: string; body: unknown }[] = []

beforeEach(() => {
  calls.length = 0
  current = [TRIGGER]
  routes = {}
  mockGetMe.mockResolvedValue(BUILDER)
  vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET'
    calls.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined })
    if (method === 'GET' && url.startsWith('/api/triggers')) return Promise.resolve(new Response(JSON.stringify(current), { status: 200 }))
    const key = Object.keys(routes).find((k) => { const [m, re] = k.split(' '); return m === method && new RegExp(re).test(url) })
    const r = key ? routes[key] : { status: 404, body: { error: 'not found' } }
    return Promise.resolve(new Response(r.body === undefined ? null : JSON.stringify(r.body), { status: r.status }))
  }))
})
afterEach(() => vi.unstubAllGlobals())

const setup = () => render(<AgentTriggers namespace="team-a" name="helper" />)

describe('AgentTriggers', () => {
  it('lists triggers with the URL, state and last fired', async () => {
    current = [{ ...TRIGGER, lastFiredAt: '2026-02-03T04:05:06Z', fireCount: 3 }]
    setup()
    const card = await screen.findByRole('listitem', { name: 'Trigger GitHub issues' })
    expect(within(card).getByText('On')).toBeInTheDocument()
    expect(within(card).getByLabelText('URL of GitHub issues')).toHaveTextContent(`/hooks/${ID}`)
    expect(within(card).getByText(/Last fired/)).toBeInTheDocument()
    expect(within(card).getByText('3 runs')).toBeInTheDocument()
    expect(within(card).getByText(/issue\.title of the JSON body/)).toBeInTheDocument()
    expect(calls[0].url).toBe('/api/triggers?namespace=team-a&agent=helper')
  })

  it('says a trigger never fired and when there are none', async () => {
    setup()
    expect(await screen.findByText('Never fired')).toBeInTheDocument()
  })

  it('shows the empty state', async () => {
    current = []
    setup()
    expect(await screen.findByText('No triggers yet.')).toBeInTheDocument()
  })

  it('shows the secret once after creating, then never again', async () => {
    routes['POST /api/triggers'] = { status: 201, body: { trigger: TRIGGER, secret: 'whsec_topsecret', hookPath: `/hooks/${ID}` } }
    const user = userEvent.setup()
    setup()
    await user.click(await screen.findByRole('button', { name: 'Add a trigger' }))
    await user.type(screen.getByLabelText('Name'), 'GitHub issues')
    await user.type(screen.getByLabelText(/Input field/), 'issue.title')
    await user.click(screen.getByRole('button', { name: 'Create trigger' }))

    expect(await screen.findByLabelText('Secret')).toHaveTextContent('whsec_topsecret')
    expect(screen.getByRole('button', { name: 'Copy URL' })).toBeInTheDocument()
    const post = calls.find((c) => c.method === 'POST')!
    expect(post.body).toEqual({ namespace: 'team-a', agent: 'helper', name: 'GitHub issues', inputPath: 'issue.title', rateLimitPerMin: 30 })

    await user.click(screen.getByRole('button', { name: 'I have saved the secret' }))
    expect(screen.queryByText('whsec_topsecret')).not.toBeInTheDocument()
    expect(document.body.textContent).not.toContain('whsec_topsecret')
  })

  it('copies the URL', async () => {
    const user = userEvent.setup()
    const writeText = vi.spyOn(navigator.clipboard, 'writeText')
    setup()
    await user.click(await screen.findByRole('button', { name: 'Copy URL of GitHub issues' }))
    expect(writeText).toHaveBeenCalledWith(`${window.location.origin}/hooks/${ID}`)
    expect(await screen.findByText('Copied')).toBeInTheDocument()
  })

  it('validates the form before sending', async () => {
    const user = userEvent.setup()
    setup()
    await user.click(await screen.findByRole('button', { name: 'Add a trigger' }))
    await user.click(screen.getByRole('button', { name: 'Create trigger' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Give the trigger a name.')
    await user.type(screen.getByLabelText('Name'), 'x')
    await user.clear(screen.getByLabelText('Requests per minute'))
    await user.type(screen.getByLabelText('Requests per minute'), '9999')
    await user.click(screen.getByRole('button', { name: 'Create trigger' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/from 1 to 600/)
    expect(calls.some((c) => c.method === 'POST')).toBe(false)
  })

  it('explains a refused create', async () => {
    routes['POST /api/triggers'] = { status: 409, body: { error: 'publish the agent before adding a trigger' } }
    const user = userEvent.setup()
    setup()
    await user.click(await screen.findByRole('button', { name: 'Add a trigger' }))
    await user.type(screen.getByLabelText('Name'), 'x')
    await user.click(screen.getByRole('button', { name: 'Create trigger' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('publish the agent before adding a trigger')
  })

  it('disables and deletes', async () => {
    routes[`PATCH /api/triggers/${ID}`] = { status: 200, body: { ...TRIGGER, enabled: false } }
    routes[`DELETE /api/triggers/${ID}`] = { status: 204 }
    const user = userEvent.setup()
    setup()
    await user.click(await screen.findByRole('button', { name: 'Disable' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ enabled: false }))

    await user.click(screen.getByRole('button', { name: 'Delete' }))
    await user.click(await screen.findByRole('button', { name: 'Delete trigger' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'DELETE' && c.url === `/api/triggers/${ID}`)).toBe(true))
  })

  it('lets a viewer read but not change', async () => {
    mockGetMe.mockResolvedValue(VIEWER)
    setup()
    await screen.findByRole('listitem', { name: 'Trigger GitHub issues' })
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Add a trigger' })).not.toBeInTheDocument())
    expect(screen.queryByRole('button', { name: 'Disable' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Delete' })).not.toBeInTheDocument()
  })

  it('explains a missing PostgreSQL store', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('{"error":"x"}', { status: 503 }))))
    setup()
    expect(await screen.findByRole('alert')).toHaveTextContent(/PostgreSQL/)
  })
})
