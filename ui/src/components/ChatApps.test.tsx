import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ChatApp } from '../api/chatApps'
import type { InboundKind } from '../api/inbound'

const mockRoles = vi.fn()
vi.mock('../lib/access', () => ({
  useRoles: () => mockRoles(), canAdminEvals: (r: string[]) => r.includes('admin'), canWriteEvals: (r: string[]) => r.includes('builder'),
}))
const apps = {
  listChatApps: vi.fn(), createChatApp: vi.fn(), saveAgentRoute: vi.fn(), removeAgentRoute: vi.fn(), setDefaultAgent: vi.fn(), listPlatformPlaces: vi.fn(),
  setAutoPlace: vi.fn(),
}
vi.mock('../api/chatApps', async () => ({
  ...(await vi.importActual<typeof import('../api/chatApps')>('../api/chatApps')),
  listChatApps: () => apps.listChatApps(),
  createChatApp: (...a: unknown[]) => apps.createChatApp(...a),
  saveAgentRoute: (...a: unknown[]) => apps.saveAgentRoute(...a),
  removeAgentRoute: (...a: unknown[]) => apps.removeAgentRoute(...a),
  setDefaultAgent: (...a: unknown[]) => apps.setDefaultAgent(...a),
  setAutoPlace: (...a: unknown[]) => apps.setAutoPlace(...a),
  listPlatformPlaces: (...a: unknown[]) => apps.listPlatformPlaces(...a),
}))
const KIND = { kind: 'slack', descriptor: { title: 'Slack' }, capabilities: { modes: ['direct', 'mention'] }, testable: true, receives: true } as unknown as InboundKind
const pairing = { createPairing: vi.fn() }
vi.mock('../api/inbound', async () => ({
  ...(await vi.importActual<typeof import('../api/inbound')>('../api/inbound')),
  listInboundKinds: () => Promise.resolve([KIND]),
  createPairing: (...a: unknown[]) => pairing.createPairing(...a),
}))

import { AgentChatApps } from './AgentChatApps'
import { ChatApps } from './ChatApps'

const APP = {
  id: 'inb_app', namespace: '', agent: '', name: 'Home', kind: 'slack', enabled: true, config: {}, secretRef: '', createdBy: 'u', createdAt: '',
  receivedCount: 3, repliedCount: 2, rejectedCount: 1, path: '/inbound/slack/inb_app', credentials: 'stored', canListPlaces: true,
  people: [{ id: 'U1', name: 'Sam' }, { id: 'U2', name: 'Ada' }],
  routes: [{
    namespace: 'ns', agent: 'net-chat', allowSenders: ['U1'], approvers: [],
    places: [{ id: 'C1', name: '#home', default: true }, { id: '', default: true }],
  }],
} as unknown as ChatApp

const withSecond: ChatApp = {
  ...APP,
  routes: [...APP.routes, { namespace: 'ns', agent: 'ops-lead', allowSenders: ['U1'], approvers: [], places: [{ id: 'C1', name: '#home' }] }],
}

beforeEach(() => {
  vi.clearAllMocks()
  mockRoles.mockReturnValue(['viewer', 'runner', 'builder', 'admin'])
  apps.listChatApps.mockResolvedValue([APP])
  apps.saveAgentRoute.mockResolvedValue(APP)
  apps.setDefaultAgent.mockResolvedValue(APP)
  apps.setAutoPlace.mockResolvedValue(APP)
})

const renderAgent = () => render(<AgentChatApps namespace="ns" name="ops-lead" kinds={[KIND]} />)

describe('AgentChatApps', () => {
  it('adds an agent to a channel another agent is in, and says who answers by default', async () => {
    apps.listPlatformPlaces.mockResolvedValue([{ id: 'C1', name: '#home', member: true }, { id: 'C2', name: '#ops', member: false }])
    renderAgent()
    await userEvent.click(await screen.findByRole('button', { name: 'Answer in Home' }))
    await userEvent.click(screen.getByRole('button', { name: 'Pick a channel' }))
    const picker = await screen.findByLabelText('Add a channel')
    // A channel that already has an agent is offered like any other.
    expect(within(picker).getByRole('option', { name: '#home' })).toBeInTheDocument()
    expect(within(picker).getByRole('option', { name: '#ops (invite the bot first)' })).toBeInTheDocument()
    await userEvent.selectOptions(picker, 'C1')
    expect(screen.getByText(/net-chat answers #home by default. Start a message with “ops-lead”/)).toBeInTheDocument()
    expect(screen.getByLabelText('Ada (U2) may approve')).toBeDisabled()
    await userEvent.click(screen.getByLabelText('Sam (U1)'))
    await userEvent.click(screen.getByLabelText('Sam (U1) may approve'))
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(apps.saveAgentRoute).toHaveBeenCalledWith('inb_app', {
      namespace: 'ns', agent: 'ops-lead', places: [{ id: 'C1', name: '#home' }], allowSenders: ['U1'], approvers: ['U1'],
    }))
  })

  it('stops someone approving once they may no longer ask', async () => {
    apps.listChatApps.mockResolvedValue([{ ...APP, routes: [{ ...APP.routes[0], agent: 'ops-lead', approvers: ['U1'] }] }])
    renderAgent()
    await userEvent.click(await screen.findByRole('button', { name: 'Change' }))
    await userEvent.click(screen.getByLabelText('Sam (U1)'))
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(apps.saveAgentRoute).toHaveBeenCalled())
    expect(apps.saveAgentRoute.mock.calls[0][1]).toMatchObject({ allowSenders: [], approvers: [] })
  })

  it('pairs a person for this agent', async () => {
    pairing.createPairing.mockResolvedValue({ id: 'p1', code: 'K7M2QX', status: 'waiting', expiresAt: '' })
    renderAgent()
    await userEvent.click(await screen.findByRole('button', { name: 'Answer in Home' }))
    await userEvent.click(screen.getByRole('button', { name: 'Add a channel or person by sending a code' }))
    await userEvent.click(screen.getByRole('button', { name: 'Get a code' }))
    expect(await screen.findByLabelText('Pairing code')).toHaveTextContent('pair K7M2QX')
    expect(pairing.createPairing).toHaveBeenCalledWith('inb_app', { namespace: 'ns', agent: 'ops-lead' })
  })

  it('lets a viewer read where agents answer but not change it', async () => {
    mockRoles.mockReturnValue(['viewer'])
    apps.listChatApps.mockResolvedValue([withSecond])
    renderAgent()
    await screen.findByText(/Answers in: #home \(when named\)/)
    expect(screen.queryByRole('button', { name: 'Change' })).not.toBeInTheDocument()
  })

  it('shows nothing when there are no shared apps', async () => {
    apps.listChatApps.mockResolvedValue([])
    const { container } = renderAgent()
    await waitFor(() => expect(apps.listChatApps).toHaveBeenCalled())
    expect(container).toBeEmptyDOMElement()
  })
})

describe('ChatApps', () => {
  it('lets an administrator move who answers by default in a shared place', async () => {
    apps.listChatApps.mockResolvedValue([withSecond])
    render(<ChatApps />)
    const table = await screen.findByRole('table', { name: 'Who answers where in Home' })
    await userEvent.selectOptions(within(table).getByLabelText('Default agent for #home'), 'ns/ops-lead')
    await waitFor(() => expect(apps.setDefaultAgent).toHaveBeenCalledWith('inb_app', 'C1', 'ns', 'ops-lead'))
    // Only one agent takes direct messages, so there is nothing to choose.
    expect(within(table).queryByLabelText('Default agent for Direct messages')).not.toBeInTheDocument()
  })

  it('shows anyone else the default without a way to change it', async () => {
    mockRoles.mockReturnValue(['viewer', 'builder'])
    apps.listChatApps.mockResolvedValue([withSecond])
    render(<ChatApps />)
    await screen.findByRole('table', { name: 'Who answers where in Home' })
    expect(screen.queryByRole('combobox')).not.toBeInTheDocument()
    expect(screen.getByText(/Only an administrator can change who answers by default/)).toBeInTheDocument()
  })

  it('connects a new app and opens its setup', async () => {
    const draft = { ...APP, id: 'inb_new', name: 'Work', credentials: '', people: [], routes: [] } as ChatApp
    apps.listChatApps.mockResolvedValueOnce([]).mockResolvedValue([draft])
    apps.createChatApp.mockResolvedValue(draft)
    render(<ChatApps />)
    await userEvent.click(await screen.findByRole('button', { name: 'Connect Slack' }))
    await userEvent.type(screen.getByLabelText('Name'), 'Work')
    await userEvent.click(screen.getByRole('button', { name: 'Start setup' }))
    expect(await screen.findByRole('group', { name: 'Set up Work' })).toBeInTheDocument()
    expect(apps.createChatApp).toHaveBeenCalledWith('Work', 'slack')
    expect(screen.getByText('Setup not finished')).toBeInTheDocument()
  })

  it('says how many agents a disconnect would silence, and asks first', async () => {
    apps.listChatApps.mockResolvedValue([withSecond])
    render(<ChatApps />)
    await userEvent.click(await screen.findByRole('button', { name: 'Disconnect' }))
    expect(screen.getByText(/Every agent stops answering in it \(2 now\)/)).toBeInTheDocument()
  })
})

describe('picking automatically', () => {
  it('lets an administrator turn it on where several agents answer', async () => {
    apps.listChatApps.mockResolvedValue([withSecond])
    render(<ChatApps />)
    const box = await screen.findByRole('checkbox', { name: 'Pick the agent automatically in #home' })
    // One agent leaves nothing to pick.
    expect(screen.queryByRole('checkbox', { name: 'Pick the agent automatically in Direct messages' })).not.toBeInTheDocument()
    await userEvent.click(box)
    await waitFor(() => expect(apps.setAutoPlace).toHaveBeenCalledWith('inb_app', 'C1', true))
  })

  it('shows others where it is on, and what the default is then for', async () => {
    mockRoles.mockReturnValue(['viewer'])
    apps.listChatApps.mockResolvedValue([{ ...withSecond, autoPlaces: ['c1'] }])
    render(<ChatApps />)
    const table = await screen.findByRole('table', { name: 'Who answers where in Home' })
    expect(within(table).getByText('Yes')).toBeInTheDocument()
    expect(within(table).queryByRole('checkbox')).not.toBeInTheDocument()
    expect(screen.getByText(/The default takes it when none clearly does/)).toBeInTheDocument()
  })
})
