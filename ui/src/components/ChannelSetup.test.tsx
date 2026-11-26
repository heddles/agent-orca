import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { InboundConnection, InboundKind } from '../api/inbound'

const mockRoles = vi.fn()
vi.mock('../lib/access', () => ({ useRoles: () => mockRoles(), canAdminEvals: (r: string[]) => r.includes('admin') }))

const api = {
  setInboundCredentials: vi.fn(), createPairing: vi.fn(), getPairing: vi.fn(), approvePairing: vi.fn(), discardPairing: vi.fn(),
}
vi.mock('../api/inbound', async () => ({
  ...(await vi.importActual<typeof import('../api/inbound')>('../api/inbound')),
  setInboundCredentials: (...a: unknown[]) => api.setInboundCredentials(...a),
  createPairing: (...a: unknown[]) => api.createPairing(...a),
  getPairing: (...a: unknown[]) => api.getPairing(...a),
  approvePairing: (...a: unknown[]) => api.approvePairing(...a),
  discardPairing: (...a: unknown[]) => api.discardPairing(...a),
}))

import { ChannelSetup } from './ChannelSetup'

const KIND = {
  kind: 'chatty',
  descriptor: { title: 'Chatty', secretKeys: [{ key: 'botToken', required: true }, { key: 'signingSecret', required: true }], setup: ['Paste {{url}}.'] },
  capabilities: { threads: true, modes: ['direct', 'mention'] }, testable: true, receives: true,
} as unknown as InboundKind

const conn = (over: Partial<InboundConnection> = {}) => ({
  id: 'inb_1', namespace: 'ns', agent: 'a', name: 'chat', kind: 'chatty', enabled: true, config: {}, secretRef: '', createdBy: 'u',
  createdAt: '', receivedCount: 0, repliedCount: 0, rejectedCount: 0, path: '/inbound/chatty/inb_1',
  url: 'https://hooks.example.com/inbound/chatty/inb_1', credentials: '', canStoreCredentials: true,
  setupLink: 'https://chatty.example/new?m=1', ...over,
}) as InboundConnection

const onChanged = vi.fn()
const setup = (c: InboundConnection) =>
  render(<ChannelSetup c={c} kind={KIND} onChanged={onChanged} onClose={() => {}} pollMs={20} patienceMs={60} />)
const step = (n: number) => screen.getByRole('listitem', { name: new RegExp(`^Step ${n}:`) })

beforeEach(() => {
  vi.clearAllMocks()
  mockRoles.mockReturnValue(['viewer', 'runner', 'builder', 'admin'])
})

describe('ChannelSetup', () => {
  it('opens the platform with the settings filled in, and keeps later steps closed until their turn', async () => {
    setup(conn())
    expect(screen.getByRole('link', { name: 'Open Chatty with settings filled in' })).toHaveAttribute('href', 'https://chatty.example/new?m=1')
    expect(within(step(3)).queryByRole('form')).not.toBeInTheDocument()
    expect(within(step(4)).queryByRole('button')).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'I created the app' }))
    expect(within(step(3)).getByRole('form', { name: 'Channel credentials' })).toBeInTheDocument()
  })

  it('warns when there is no public address and shows the manual steps without a link', () => {
    setup(conn({ url: undefined, setupLink: undefined }))
    expect(within(step(1)).getByText(/No public address is configured/)).toBeInTheDocument()
    expect(screen.getByRole('list', { name: 'Steps on Chatty' })).toHaveTextContent(`Paste ${window.location.origin}/inbound/chatty/inb_1.`)
  })

  it('takes credentials in password fields, sends them once and clears them', async () => {
    api.setInboundCredentials.mockResolvedValue({})
    setup(conn())
    await userEvent.click(screen.getByRole('button', { name: 'I created the app' }))
    const token = screen.getByLabelText('botToken')
    expect(token).toHaveAttribute('type', 'password')
    expect(token).toHaveAttribute('autocomplete', 'off')
    await userEvent.click(screen.getByRole('button', { name: 'Save credentials' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('botToken is required.')
    await userEvent.type(token, 'tok')
    await userEvent.type(screen.getByLabelText('signingSecret'), 'sig')
    await userEvent.click(screen.getByRole('button', { name: 'Save credentials' }))
    await waitFor(() => expect(api.setInboundCredentials).toHaveBeenCalledWith('inb_1', { botToken: 'tok', signingSecret: 'sig' }))
    expect(onChanged).toHaveBeenCalled()
    expect(screen.getByLabelText('botToken')).toHaveValue('')
  })

  it('leaves credentials to an administrator', async () => {
    mockRoles.mockReturnValue(['viewer', 'runner', 'builder'])
    setup(conn())
    await userEvent.click(screen.getByRole('button', { name: 'I created the app' }))
    expect(within(step(3)).getByText(/An administrator adds the credentials/)).toBeInTheDocument()
    expect(screen.queryByLabelText('botToken')).not.toBeInTheDocument()
  })

  it('never shows stored credentials, only that they exist', () => {
    setup(conn({ credentials: 'stored' }))
    expect(within(step(3)).getByText(/Nobody can read them back/)).toBeInTheDocument()
    expect(screen.queryByLabelText('botToken')).not.toBeInTheDocument()
    expect(within(step(3)).getByRole('button', { name: 'Replace credentials' })).toBeInTheDocument()
  })

  it('shows the code, then asks before allowing whoever sent it', async () => {
    api.createPairing.mockResolvedValue({ id: 'p1', code: 'K7M2QX', status: 'waiting', expiresAt: '' })
    api.getPairing.mockResolvedValue({ id: 'p1', status: 'received', senderId: 'U9', senderName: 'Sam', direct: true, expiresAt: '' })
    api.approvePairing.mockResolvedValue({ replySent: true })
    setup(conn({ credentials: 'stored' }))
    await userEvent.click(screen.getByRole('button', { name: 'Get a code' }))
    expect(await screen.findByLabelText('Pairing code')).toHaveTextContent('pair K7M2QX')
    expect(await screen.findByText('Sam (U9)')).toBeInTheDocument()
    expect(api.approvePairing).not.toHaveBeenCalled()
    await userEvent.click(screen.getByRole('button', { name: 'Allow this person' }))
    expect(await screen.findByText(/the bot replied to them/)).toBeInTheDocument()
    expect(api.approvePairing).toHaveBeenCalledWith('inb_1', 'p1')
  })

  it('says when the person was allowed but the reply failed', async () => {
    api.createPairing.mockResolvedValue({ id: 'p1', code: 'K7M2QX', status: 'waiting', expiresAt: '' })
    api.getPairing.mockResolvedValue({ id: 'p1', status: 'received', senderId: 'U9', channelId: 'C1', expiresAt: '' })
    api.approvePairing.mockResolvedValue({ replySent: false, replyError: 'chat.postMessage: not_in_channel' })
    setup(conn({ credentials: 'stored' }))
    await userEvent.click(screen.getByRole('button', { name: 'Get a code' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Allow this person' }))
    expect(await screen.findByText(/could not reply: chat.postMessage: not_in_channel/)).toBeInTheDocument()
  })

  it.each([
    ['nothing arrives', {}, /No request from Chatty has reached the address/],
    ['a request is refused', { lastAuthError: 'invalid signature', lastAuthErrorAt: new Date(Date.now() + 5000).toISOString() }, /refused: invalid signature/],
    ['messages arrive without the code', { lastRequestAt: new Date(Date.now() + 5000).toISOString() }, /no message carried the code/],
    ['only old requests exist', { lastRequestAt: '2020-01-01T00:00:00Z' }, /No request from Chatty/],
  ])('explains the silence when %s', async (_name, status, text) => {
    api.createPairing.mockResolvedValue({ id: 'p1', code: 'K7M2QX', status: 'waiting', expiresAt: '' })
    api.getPairing.mockResolvedValue({ id: 'p1', status: 'waiting', expiresAt: '' })
    setup(conn({ credentials: 'stored', ...status }))
    await userEvent.click(screen.getByRole('button', { name: 'Get a code' }))
    expect(await screen.findByRole('alert', {}, { timeout: 2000 })).toHaveTextContent(text)
  })

  it('offers a new code once one has expired', async () => {
    api.createPairing.mockResolvedValue({ id: 'p1', code: 'K7M2QX', status: 'waiting', expiresAt: '' })
    api.getPairing.mockRejectedValue(Object.assign(new Error('gone'), { status: 404 }))
    setup(conn({ credentials: 'stored' }))
    await userEvent.click(screen.getByRole('button', { name: 'Get a code' }))
    expect(await screen.findByText('That code expired. Get a new one.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Get a code' })).toBeInTheDocument()
  })
})
