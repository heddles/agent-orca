import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

const mockGetMe = vi.fn()
vi.mock('../api/fleet', () => ({ getMe: () => mockGetMe() }))

import { AgentChannels } from './AgentChannels'
import { parseList } from '../api/inbound'

const me = (roles: string[]) => ({ userId: 'u', groups: [], tenant: { id: 't', name: 'T' }, workspaces: [{ name: 'A', namespace: 'team-a', primary: true }], roles })
const BUILDER = me(['viewer', 'runner', 'builder'])
const VIEWER = me(['viewer'])

const ID = 'inb_0123456789abcdef0123456789abcdef'
const MAILBOX = {
  id: ID, namespace: 'team-a', agent: 'helper', name: 'Support mailbox', kind: 'email', enabled: true,
  config: { allowSenders: ['alice@example.com'], allowGroups: ['corp.example'], adapter: { mode: 'imap', requireAuthResults: true } },
  secretRef: 'mail-creds', createdBy: 'u', createdAt: '2026-01-01T00:00:00Z', receivedCount: 5, repliedCount: 4, rejectedCount: 1,
  lastPollAt: '2026-02-03T04:05:06Z', path: `/inbound/email/${ID}`,
}
const WEBHOOK = { ...MAILBOX, id: 'inb_ffffffffffffffffffffffffffffffff', name: 'Provider', config: { allowGroups: ['corp.example'], adapter: { mode: 'webhook' } }, path: '/inbound/email/inb_ffffffffffffffffffffffffffffffff' }

const CHAT_ID = 'inb_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
const SLACKISH = {
  kind: 'chatty',
  descriptor: {
    title: 'Chatty',
    summary: 'Ask the agent in a chat app.',
    fields: [
      { key: 'workspaceId', label: 'Workspace id', type: 'string', required: true, help: 'Find it in the app.' },
      { key: 'reaction', label: 'Show a reaction while working', type: 'bool' },
      { key: 'retries', label: 'Retries', type: 'int' },
    ],
    secretKeys: [{ key: 'botToken', required: true }, { key: 'signingSecret', required: true }],
    allow: { users: 'Member ids.', groups: 'Workspace ids.', channels: 'Channel ids.' },
    setup: ['Paste {{url}} into the app as the request URL.', 'Invite the bot to a channel.'],
  },
  capabilities: { threads: true, dialect: 'mrkdwn', modes: ['direct', 'mention', 'command', 'channel'], ackFirst: true },
  testable: true, receives: true,
}
const CHATCONN = {
  ...MAILBOX, id: CHAT_ID, name: 'Support chat', kind: 'chatty', path: `/inbound/chatty/${CHAT_ID}`,
  config: { allowSenders: ['U1'], allowChannels: ['C1'], modes: ['mention'], adapter: { workspaceId: 'T1' } },
}
let kinds: unknown[]

type Reply = { status: number; body?: unknown }
let routes: Record<string, Reply>
let current: unknown[]
const calls: { method: string; url: string; body: unknown }[] = []

beforeEach(() => {
  calls.length = 0
  current = [MAILBOX]
  kinds = []
  routes = {}
  mockGetMe.mockResolvedValue(BUILDER)
  vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET'
    calls.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined })
    if (method === 'GET' && url === '/api/inbound-kinds') return Promise.resolve(new Response(JSON.stringify(kinds), { status: 200 }))
    if (method === 'GET' && url.startsWith('/api/inbound-connections')) return Promise.resolve(new Response(JSON.stringify(current), { status: 200 }))
    const key = Object.keys(routes).find((k) => { const [m, re] = k.split(' '); return m === method && new RegExp(re).test(url) })
    const r = key ? routes[key] : { status: 404, body: { error: 'not found' } }
    return Promise.resolve(new Response(r.body === undefined ? null : JSON.stringify(r.body), { status: r.status }))
  }))
})
afterEach(() => vi.unstubAllGlobals())

const setup = () => render(<AgentChannels namespace="team-a" name="helper" />)

describe('AgentChannels', () => {
  it('lists channels with state, counts, last check and who may write', async () => {
    setup()
    const card = await screen.findByRole('listitem', { name: 'Channel Support mailbox' })
    expect(within(card).getByText('On')).toBeInTheDocument()
    expect(within(card).getByText('Email (mailbox)')).toBeInTheDocument()
    expect(within(card).getByText(/5 received, 4 answered, 1 refused/)).toBeInTheDocument()
    expect(within(card).getByText(/Last checked/)).toBeInTheDocument()
    expect(within(card).getByText(/alice@example.com, anyone at corp.example/)).toBeInTheDocument()
    expect(calls[0].url).toBe('/api/inbound-connections?namespace=team-a&agent=helper')
  })

  it('shows the empty state and the web address of a provider webhook', async () => {
    current = []
    const { unmount } = setup()
    expect(await screen.findByText('No channels yet.')).toBeInTheDocument()
    unmount()
    current = [WEBHOOK]
    setup()
    expect(await screen.findByLabelText('URL of Provider')).toHaveTextContent(`/inbound/email/${WEBHOOK.id}`)
  })

  it('shows the last problem without any secret', async () => {
    current = [{ ...MAILBOX, lastError: 'the mail server refused the login' }]
    setup()
    expect(await screen.findByRole('alert')).toHaveTextContent('Last problem: the mail server refused the login')
  })

  it('creates a channel with the allow-list and safe defaults', async () => {
    routes['POST /api/inbound-connections'] = { status: 201, body: MAILBOX }
    const user = userEvent.setup()
    setup()
    await user.click(await screen.findByRole('button', { name: 'Add a channel' }))
    await user.type(screen.getByLabelText('Name'), 'Support mailbox')
    await user.type(screen.getByLabelText('Secret'), 'mail-creds')
    await user.type(screen.getByLabelText(/Who may write \(addresses\)/), 'alice@example.com{enter}bob@example.com')
    await user.type(screen.getByLabelText(/Who may write \(whole domains\)/), 'corp.example')
    await user.type(screen.getByLabelText(/Mail server name/), 'mx.corp.example')
    await user.click(screen.getByRole('button', { name: 'Create channel' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    expect(calls.find((c) => c.method === 'POST')!.body).toEqual({
      namespace: 'team-a', agent: 'helper', name: 'Support mailbox', kind: 'email', secretRef: 'mail-creds',
      config: {
        allowSenders: ['alice@example.com', 'bob@example.com'], allowGroups: ['corp.example'],
        adapter: { mode: 'imap', requireAuthResults: true, acceptTextAttachments: false, trustedAuthservId: 'mx.corp.example' },
      },
    })
    // No password field exists anywhere in the form.
    expect(document.body.textContent).not.toMatch(/password/i)
  })

  it('refuses to send a channel that admits nobody', async () => {
    const user = userEvent.setup()
    setup()
    await user.click(await screen.findByRole('button', { name: 'Add a channel' }))
    await user.type(screen.getByLabelText('Name'), 'x')
    await user.type(screen.getByLabelText('Secret'), 's')
    await user.click(screen.getByRole('button', { name: 'Create channel' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/Nobody is allowed by default/)
    expect(calls.some((c) => c.method === 'POST')).toBe(false)
  })

  it('explains a refused create', async () => {
    routes['POST /api/inbound-connections'] = { status: 409, body: { error: 'publish the agent before adding a channel' } }
    const user = userEvent.setup()
    setup()
    await user.click(await screen.findByRole('button', { name: 'Add a channel' }))
    await user.type(screen.getByLabelText('Name'), 'x')
    await user.type(screen.getByLabelText('Secret'), 's')
    await user.type(screen.getByLabelText(/Who may write \(whole domains\)/), 'corp.example')
    await user.click(screen.getByRole('button', { name: 'Create channel' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('publish the agent before adding a channel')
  })

  it('tests a connection and shows what was found', async () => {
    routes[`POST /api/inbound-connections/${ID}/test`] = { status: 200, body: { ok: true, details: { folder: 'INBOX: 2 messages, 1 unseen' } } }
    const user = userEvent.setup()
    setup()
    await user.click(await screen.findByRole('button', { name: 'Test connection' }))
    expect(await screen.findByText('The connection works.')).toBeInTheDocument()
    expect(screen.getByText('folder: INBOX: 2 messages, 1 unseen')).toBeInTheDocument()
  })

  it('shows a failed test', async () => {
    routes[`POST /api/inbound-connections/${ID}/test`] = { status: 200, body: { ok: false, error: 'the mail server refused the login' } }
    const user = userEvent.setup()
    setup()
    await user.click(await screen.findByRole('button', { name: 'Test connection' }))
    expect(await screen.findByText('The test failed.')).toBeInTheDocument()
    expect(screen.getByText(/refused the login/)).toBeInTheDocument()
  })

  it('disables and deletes', async () => {
    routes[`PATCH /api/inbound-connections/${ID}`] = { status: 200, body: { ...MAILBOX, enabled: false } }
    routes[`DELETE /api/inbound-connections/${ID}`] = { status: 204 }
    const user = userEvent.setup()
    setup()
    await user.click(await screen.findByRole('button', { name: 'Disable' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ enabled: false }))
    await user.click(screen.getByRole('button', { name: 'Delete' }))
    await user.click(await screen.findByRole('button', { name: 'Delete channel' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'DELETE' && c.url === `/api/inbound-connections/${ID}`)).toBe(true))
  })

  it('lets a viewer read but not change', async () => {
    mockGetMe.mockResolvedValue(VIEWER)
    setup()
    await screen.findByRole('listitem', { name: 'Channel Support mailbox' })
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Add a channel' })).not.toBeInTheDocument())
    for (const b of ['Disable', 'Test connection', 'Delete']) expect(screen.queryByRole('button', { name: b })).not.toBeInTheDocument()
  })

  it('explains a missing PostgreSQL store', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('{"error":"x"}', { status: 503 }))))
    setup()
    expect(await screen.findByRole('alert')).toHaveTextContent(/PostgreSQL store/)
  })
})

describe('AgentChannels, chat platforms', () => {
  it('shows a chat connection with its receive URL, scope and setup steps from the adapter', async () => {
    kinds = [SLACKISH]
    current = [CHATCONN]
    setup()
    const card = await screen.findByRole('listitem', { name: 'Channel Support chat' })
    expect(await within(card).findByText('Chatty')).toBeInTheDocument()
    expect(within(card).getByLabelText('URL of Support chat')).toHaveTextContent(`/inbound/chatty/${CHAT_ID}`)
    expect(within(card).getByText(/U1\. Only in: C1\. Answers: mention/)).toBeInTheDocument()
    const steps = within(card).getByLabelText('Setup steps of Support chat')
    expect(steps).toHaveTextContent(`Paste ${window.location.origin}/inbound/chatty/${CHAT_ID} into the app`)
  })

  it('offers every registered kind and draws the form from the adapter descriptor', async () => {
    kinds = [SLACKISH]
    current = [CHATCONN]
    routes['POST /api/inbound-connections'] = { status: 201, body: CHATCONN }
    const user = userEvent.setup()
    setup()
    await screen.findByText('Chatty')
    await user.click(screen.getByRole('button', { name: 'Add a channel' }))
    await user.click(await screen.findByRole('button', { name: 'Chatty' }))
    expect(screen.getByText(/keys botToken, signingSecret/)).toBeInTheDocument()
    await user.type(screen.getByLabelText('Name'), 'Support chat')
    await user.type(screen.getByLabelText('Secret (can be added later)'), 'chat-creds')
    await user.type(screen.getByLabelText('Workspace id'), 'T1')
    await user.click(screen.getByLabelText('Show a reaction while working'))
    await user.type(screen.getByLabelText(/Only in these channels/), 'C1, C2')
    await user.click(screen.getByLabelText('Slash commands'))
    await user.click(screen.getByRole('button', { name: 'Create channel' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    expect(calls.find((c) => c.method === 'POST')!.body).toEqual({
      namespace: 'team-a', agent: 'helper', name: 'Support chat', kind: 'chatty', secretRef: 'chat-creds',
      config: {
        allowSenders: [], allowGroups: [], allowChannels: ['C1', 'C2'], modes: ['direct', 'mention'],
        adapter: { workspaceId: 'T1', reaction: true },
      },
    })
  })

  it('checks the required setting, the allow-list and the channel mode before sending', async () => {
    kinds = [SLACKISH]
    current = [CHATCONN]
    const user = userEvent.setup()
    setup()
    await screen.findByText('Chatty')
    await user.click(screen.getByRole('button', { name: 'Add a channel' }))
    await user.click(await screen.findByRole('button', { name: 'Chatty' }))
    await user.type(screen.getByLabelText('Name'), 'x')
    await user.type(screen.getByLabelText('Secret (can be added later)'), 's')
    await user.click(screen.getByRole('button', { name: 'Create channel' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/Nobody is allowed by default/)
    await user.type(screen.getByLabelText(/Who may write \(user ids\)/), 'U1')
    await user.click(screen.getByRole('button', { name: 'Create channel' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Workspace id is required.')
    await user.type(screen.getByLabelText('Workspace id'), 'T1')
    await user.click(screen.getByLabelText(/Every message in the listed channels/))
    await user.click(screen.getByRole('button', { name: 'Create channel' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/needs that channel on the list/)
    expect(calls.some((c) => c.method === 'POST')).toBe(false)
  })

  it('shows the platform steps before asking for anything', async () => {
    kinds = [SLACKISH]
    current = []
    const user = userEvent.setup()
    setup()
    await user.click(await screen.findByRole('button', { name: 'Add a channel' }))
    await user.click(await screen.findByRole('button', { name: 'Chatty' }))
    const steps = screen.getByRole('list', { name: 'Steps on Chatty' })
    expect(steps).toHaveTextContent('Paste (the address shown once the channel is created) into the app')
    expect(steps).toHaveTextContent('Invite the bot to a channel.')
    expect(steps.compareDocumentPosition(screen.getByLabelText('Name')) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  it('creates a draft without a Secret, so the address exists before the app does', async () => {
    kinds = [SLACKISH]
    current = []
    routes['POST /api/inbound-connections'] = { status: 201, body: { ...CHATCONN, secretRef: '' } }
    const user = userEvent.setup()
    setup()
    await user.click(await screen.findByRole('button', { name: 'Add a channel' }))
    await user.click(await screen.findByRole('button', { name: 'Chatty' }))
    await user.type(screen.getByLabelText('Name'), 'x')
    await user.type(screen.getByLabelText('Workspace id'), 'T1')
    await user.type(screen.getByLabelText(/Who may write \(user ids\)/), 'U1')
    await user.click(screen.getByRole('button', { name: 'Create channel' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    expect(calls.find((c) => c.method === 'POST')!.body).toMatchObject({ secretRef: '' })
  })

  it('says a draft is not ready, keeps its steps open and takes the Secret later', async () => {
    kinds = [SLACKISH]
    current = [{ ...CHATCONN, secretRef: '' }]
    routes[`PATCH /api/inbound-connections/${CHAT_ID}`] = { status: 200, body: CHATCONN }
    const user = userEvent.setup()
    setup()
    const card = await screen.findByRole('listitem', { name: 'Channel Support chat' })
    expect(within(card).getByText('Not ready')).toBeInTheDocument()
    expect(within(card).queryByRole('button', { name: 'Test connection' })).not.toBeInTheDocument()
    await waitFor(() => expect(within(card).getByLabelText('Setup steps of Support chat').closest('details')).toHaveAttribute('open'))
    await user.type(within(card).getByLabelText('Secret'), 'chat-creds')
    await user.click(within(card).getByRole('button', { name: 'Add credentials' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ secretRef: 'chat-creds' }))
  })

  it('shows why adding credentials was refused', async () => {
    kinds = [SLACKISH]
    current = [{ ...CHATCONN, secretRef: '' }]
    routes[`PATCH /api/inbound-connections/${CHAT_ID}`] = { status: 400, body: { error: 'the credentials were not accepted: auth.test: invalid_auth' } }
    const user = userEvent.setup()
    setup()
    const card = await screen.findByRole('listitem', { name: 'Channel Support chat' })
    await user.type(within(card).getByLabelText('Secret'), 'wrong')
    await user.click(within(card).getByRole('button', { name: 'Add credentials' }))
    expect(await within(card).findByRole('alert')).toHaveTextContent('invalid_auth')
  })

  it('uses the configured public address, and warns when there is none', async () => {
    kinds = [SLACKISH]
    current = [{ ...CHATCONN, url: `https://hooks.example.com/inbound/chatty/${CHAT_ID}` }]
    const first = setup()
    let card = await screen.findByRole('listitem', { name: 'Channel Support chat' })
    expect(within(card).getByLabelText('URL of Support chat')).toHaveTextContent(`https://hooks.example.com/inbound/chatty/${CHAT_ID}`)
    expect(within(card).getByLabelText('Setup steps of Support chat')).toHaveTextContent('Paste https://hooks.example.com/')
    expect(within(card).queryByText(/address you are using now/)).not.toBeInTheDocument()
    first.unmount()
    current = [CHATCONN]
    setup()
    card = await screen.findByRole('listitem', { name: 'Channel Support chat' })
    expect(within(card).getByText(/address you are using now/)).toBeInTheDocument()
  })

  it('says why messages were refused', async () => {
    kinds = [SLACKISH]
    current = [{ ...CHATCONN, refused: { sender_not_allowed: 2, channel_not_allowed: 1, something_new: 4 } }]
    setup()
    const list = await screen.findByRole('list', { name: 'Messages refused in the last 24 hours' })
    expect(list).toHaveTextContent('2: the sender is not allowed')
    expect(list).toHaveTextContent('1: that channel is not on the list')
    expect(list).toHaveTextContent('4: something_new')
  })

  it('tests a chat connection and shows the bot and workspace without secrets', async () => {
    kinds = [SLACKISH]
    current = [CHATCONN]
    routes[`POST /api/inbound-connections/${CHAT_ID}/test`] = { status: 200, body: { ok: true, details: { bot: 'orca-bot', workspace: 'Acme' } } }
    const user = userEvent.setup()
    setup()
    await user.click(await screen.findByRole('button', { name: 'Test connection' }))
    expect(await screen.findByText('bot: orca-bot')).toBeInTheDocument()
    expect(screen.getByText('workspace: Acme')).toBeInTheDocument()
  })
})

describe('parseList', () => {
  it('splits on lines, spaces and commas', () => {
    expect(parseList('a@x.com, b@x.com\n c@x.com ,,')).toEqual(['a@x.com', 'b@x.com', 'c@x.com'])
    expect(parseList('  ')).toEqual([])
  })
})
