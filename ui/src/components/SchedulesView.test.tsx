import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'

const mockGetMe = vi.fn()
vi.mock('../api/fleet', () => ({ getMe: () => mockGetMe() }))
const mockCatalog = vi.fn()
vi.mock('../api/catalog', () => ({ getCatalog: () => mockCatalog() }))

import { NamespaceProvider } from '../lib/namespace'
import { SchedulesView, formProblems } from './SchedulesView'
import { ScheduleBadge, ScheduledByChip } from './ScheduleParts'
import type { ScheduleSummary } from '../api/schedules'

const me = (roles: string[]) => ({ userId: 'u', groups: [], tenant: { id: 't', name: 'T' }, workspaces: [{ name: 'A', namespace: 'team-a', primary: true }], roles })
const BUILDER = me(['viewer', 'runner', 'builder'])
const ADMIN = me(['viewer', 'runner', 'builder', 'admin'])
const VIEWER = me(['viewer'])

const ui = { owner: 'ui', editable: true, handoff: { pending: false } }
const summary = (over: Partial<ScheduleSummary> = {}): ScheduleSummary => ({
  name: 'weekday-digest', namespace: 'team-a', target: 'digest-agent', targetKind: 'agent', schedule: '0 8 * * 1-5', timeZone: 'Europe/Berlin',
  concurrencyPolicy: 'Forbid', suspended: false, state: 'active', nextRun: '2026-11-16T07:00:00Z', nextRunLocal: '2026-11-16T08:00:00+01:00',
  lastResult: { status: 'succeeded', at: '2026-11-13T07:00:30Z' }, spentTodayUSD: '0.2000', spentMonthUSD: '3.1000', consecutiveFailures: 0,
  active: [], recentSkips: [], ownership: ui as ScheduleSummary['ownership'], ...over,
})

const stored = (owner = 'ui') => ({
  apiVersion: 'agentorca.agentorca.io/v1alpha1', kind: 'AgentSchedule',
  metadata: { name: 'weekday-digest', namespace: 'team-a', resourceVersion: '7' },
  spec: { agentRef: 'digest-agent', schedule: '0 8 * * 1-5', timeZone: 'Europe/Berlin', input: 'Digest for {{schedule.date}}', budget: { maxUSD: '1.00' } },
  ownership: { owner, editable: owner === 'ui', handoff: { pending: false } },
})

type Reply = { status?: number; body?: unknown }
let list: ScheduleSummary[]
let routes: Record<string, Reply>
const calls: { method: string; url: string; body: any }[] = []

beforeEach(() => {
  calls.length = 0
  list = [summary()]
  routes = {}
  mockGetMe.mockResolvedValue(BUILDER)
  mockCatalog.mockResolvedValue([{ name: 'digest-agent', namespace: 'team-a', template: false }, { name: 'other', namespace: 'team-a', template: false }])
  vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET'
    calls.push({ method, url, body: init?.body ? JSON.parse(String(init.body)) : undefined })
    const json = (b: unknown, status = 200) => Promise.resolve(new Response(JSON.stringify(b), { status }))
    if (method === 'GET' && url.startsWith('/api/schedules/preview')) {
      return json({ timeZone: 'Europe/Berlin', minInterval: '5m0s', shortestGap: '24h0m0s', tooFrequent: url.includes('cron=*+*') || url.includes('cron=*%20*'),
        nextRuns: [{ utc: '2026-11-16T07:00:00Z', local: '2026-11-16T08:00:00+01:00' }, { utc: '2026-11-17T07:00:00Z', local: '2026-11-17T08:00:00+01:00' }] })
    }
    if (method === 'GET' && url.startsWith('/api/schedules?')) return json(list)
    if (method === 'GET' && url.endsWith('/runs')) return json({ runs: [{ name: 'weekday-digest-1', namespace: 'team-a', kind: 'AgentRun', trigger: 'schedule', phase: 'Succeeded', createdAt: '2026-11-13T07:00:00Z', spendUSD: '0.10' }], nextBefore: '' })
    const key = Object.keys(routes).find((k) => { const [m, re] = k.split(' '); return m === method && new RegExp(re).test(url) })
    const r = key ? routes[key] : { status: 404, body: { error: 'not found' } }
    return json(r.body ?? {}, r.status ?? 200)
  }))
})
afterEach(() => vi.unstubAllGlobals())

const setup = () => render(<MemoryRouter><NamespaceProvider><SchedulesView /></NamespaceProvider></MemoryRouter>)
const card = () => screen.findByRole('listitem', { name: 'Schedule weekday-digest' })

describe('SchedulesView', () => {
  it('lists a schedule with its words, next run, last result and spend', async () => {
    setup()
    const c = await card()
    expect(within(c).getByText('On')).toBeInTheDocument()
    expect(within(c).getByText(/Every weekday at 08:00/)).toBeInTheDocument()
    expect(within(c).getByText(/Next run/)).toBeInTheDocument()
    expect(within(c).getByText(/Last run succeeded/)).toBeInTheDocument()
    expect(within(c).getByText(/Spent \$0.2000 today, \$3.1000 this month/)).toBeInTheDocument()
  })

  it('shows the plain reason for a stopped schedule and a Start again button', async () => {
    list = [summary({ state: 'degraded', stateReason: 'the last 5 runs failed; the schedule stopped until it is resumed' })]
    routes['POST /resume'] = {}
    setup()
    const c = await card()
    expect(within(c).getByText('Stopped after failures')).toBeInTheDocument()
    expect(within(c).getByText(/the last 5 runs failed/)).toBeInTheDocument()
    await userEvent.click(within(c).getByRole('button', { name: 'Start again' }))
    await waitFor(() => expect(calls.some((x) => x.method === 'POST' && x.url === '/api/schedules/team-a/weekday-digest/resume')).toBe(true))
  })

  it('pauses, resumes and runs now through the API', async () => {
    routes['POST /suspend'] = {}
    routes['POST /run'] = { status: 202, body: { kind: 'AgentRun', name: 'weekday-digest-m-x', namespace: 'team-a' } }
    setup()
    const c = await card()
    await userEvent.click(within(c).getByRole('button', { name: 'Pause' }))
    await userEvent.click(within(c).getByRole('button', { name: 'Run now' }))
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('Started a run of weekday-digest.'))
    expect(calls.filter((x) => x.method === 'POST').map((x) => x.url)).toEqual([
      '/api/schedules/team-a/weekday-digest/suspend', '/api/schedules/team-a/weekday-digest/run'])
  })

  it('says before and after Run now where the report goes', async () => {
    list = [summary({ notify: { slack: { app: 'orca-bot', channelID: 'C0REPORTS', channelName: '#reports' } } })]
    routes['POST /run'] = { status: 202, body: { kind: 'AgentRun', name: 'weekday-digest-m-x', namespace: 'team-a', reportOn: ['Succeeded', 'Failed'] } }
    setup()
    const button = within(await card()).getByRole('button', { name: 'Run now' })
    expect(button).toHaveAttribute('title', expect.stringContaining('#reports (C0REPORTS), through the app orca-bot'))
    await userEvent.click(button)
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent(
      'Started a run of weekday-digest. When it succeeds or fails, a report goes to Slack channel #reports (C0REPORTS), through the app orca-bot.'))
  })

  it('says when Run now only reports a failure', async () => {
    list = [summary({ notify: { channel: 'home-slack' } })]
    routes['POST /run'] = { status: 202, body: { kind: 'AgentRun', name: 'weekday-digest-m-x', namespace: 'team-a', reportOn: ['Failed'] } }
    setup()
    await userEvent.click(within(await card()).getByRole('button', { name: 'Run now' }))
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('Only if it fails, a report goes to the notification channel home-slack.'))
  })

  it('warns after Run now that the report cannot be sent', async () => {
    list = [summary({ notify: { channel: 'home-slack', problem: 'the Secret of channel "home-slack" was not found' } })]
    routes['POST /run'] = { status: 202, body: { kind: 'AgentRun', name: 'weekday-digest-m-x', namespace: 'team-a', reportOn: ['Failed'] } }
    setup()
    await userEvent.click(within(await card()).getByRole('button', { name: 'Run now' }))
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('Its report cannot be sent: the Secret of channel "home-slack" was not found'))
  })

  it('shows a quota refusal from Run now in words', async () => {
    routes['POST /run'] = { status: 429, body: { error: 'concurrent run limit (1) reached' } }
    setup()
    await userEvent.click(within(await card()).getByRole('button', { name: 'Run now' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/concurrent run limit/)
  })

  it('opens the history with the skipped times', async () => {
    list = [summary({ recentSkips: [{ time: '2026-11-12T07:00:00Z', reason: 'SkippedConcurrency' }] })]
    setup()
    await userEvent.click(within(await card()).getByRole('button', { name: 'History' }))
    const h = await screen.findByLabelText('History of weekday-digest')
    expect(within(h).getByText('weekday-digest-1')).toBeInTheDocument()
    expect(within(h).getByText(/The previous run was still going/)).toBeInTheDocument()
    expect(within(h).getByText('Scheduled')).toBeInTheDocument()
  })

  it('a Git-owned schedule is read-only: no edit, delete, pause; Copy YAML and Duplicate', async () => {
    list = [summary({ ownership: { owner: 'git', editable: false, handoff: { pending: false } } as ScheduleSummary['ownership'] })]
    routes['GET /api/resources/agentschedules/team-a/weekday-digest'] = { body: stored('git') }
    routes['POST /duplicate'] = { status: 201, body: stored('ui') }
    setup()
    const c = await card()
    expect(within(c).getByText('Managed in Git')).toBeInTheDocument()
    expect(within(c).queryByRole('button', { name: 'Edit' })).toBeNull()
    expect(within(c).queryByRole('button', { name: 'Delete' })).toBeNull()
    expect(within(c).getByRole('button', { name: 'Pause' })).toBeDisabled()
    await userEvent.click(within(c).getByRole('button', { name: 'View' }))
    const d = await screen.findByRole('dialog')
    expect(within(d).getByRole('note')).toHaveTextContent(/managed in Git/)
    expect(within(d).getByRole('button', { name: 'Copy YAML' })).toBeInTheDocument()
    expect(within(d).queryByRole('button', { name: 'Save changes' })).toBeNull()
    await userEvent.click(within(d).getByRole('button', { name: 'Duplicate' }))
    await userEvent.click(within(d).getByRole('button', { name: 'Make a copy' }))
    await waitFor(() => expect(calls.some((x) => x.method === 'POST' && x.url.endsWith('/weekday-digest/duplicate') && x.body.name === 'weekday-digest-copy')).toBe(true))
  })

  it('a viewer cannot create or edit', async () => {
    mockGetMe.mockResolvedValue(VIEWER)
    routes['GET /api/resources/agentschedules/team-a/weekday-digest'] = { body: stored() }
    setup()
    const c = await card()
    expect(screen.queryByRole('button', { name: 'New schedule' })).toBeNull()
    expect(within(c).queryByRole('button', { name: 'Delete' })).toBeNull()
    await userEvent.click(within(c).getByRole('button', { name: 'View' }))
    const d = await screen.findByRole('dialog')
    expect(within(d).queryByRole('button', { name: 'Save changes' })).toBeNull()
  })

  it('creates a schedule from a preset with a live preview', async () => {
    list = []
    routes['POST /api/resources/agentschedules'] = { status: 201, body: stored() }
    setup()
    await userEvent.click(await screen.findByRole('button', { name: 'New schedule' }))
    const d = await screen.findByRole('dialog')
    await userEvent.type(within(d).getByLabelText('Name'), 'weekday-digest')
    await userEvent.selectOptions(within(d).getByLabelText('Which agent?'), 'digest-agent')
    await userEvent.type(within(d).getByLabelText('Instruction'), 'Digest for')
    await userEvent.click(within(d).getByRole('button', { name: "Insert today's date" }))
    const next = await within(d).findByLabelText('Next runs')
    await waitFor(() => expect(within(next).getAllByRole('listitem')).toHaveLength(2))
    await userEvent.click(within(d).getByRole('button', { name: 'Create schedule' }))
    await waitFor(() => expect(calls.some((x) => x.method === 'POST' && x.url === '/api/resources/agentschedules')).toBe(true))
    const post = calls.find((x) => x.method === 'POST' && x.url === '/api/resources/agentschedules')!
    expect(post.body.metadata).toMatchObject({ name: 'weekday-digest' })
    expect(post.body.spec).toMatchObject({
      agentRef: 'digest-agent', schedule: '0 8 * * 1-5', concurrencyPolicy: 'Forbid', input: 'Digest for {{schedule.date}}', budget: { maxUSD: '1.00' },
    })
    expect(typeof post.body.spec.timeZone).toBe('string')
  })

  it('refuses a schedule that runs too often, and one without a spend limit', async () => {
    list = []
    setup()
    await userEvent.click(await screen.findByRole('button', { name: 'New schedule' }))
    const d = await screen.findByRole('dialog')
    await userEvent.type(within(d).getByLabelText('Name'), 'busy')
    await userEvent.selectOptions(within(d).getByLabelText('Which agent?'), 'other')
    await userEvent.type(within(d).getByLabelText('Instruction'), 'go')
    await userEvent.selectOptions(within(d).getByLabelText('Repeat'), 'custom')
    const cron = within(d).getByLabelText('Cron expression')
    await userEvent.clear(cron)
    await userEvent.type(cron, '* * * * *')
    expect(await within(d).findByText(/more often than every 5m0s/)).toBeInTheDocument()
    await userEvent.click(within(d).getByRole('button', { name: 'Create schedule' }))
    expect(calls.some((x) => x.method === 'POST' && x.url === '/api/resources/agentschedules')).toBe(false)
    await userEvent.clear(within(d).getByLabelText('Per run (USD)'))
    await userEvent.click(within(d).getByRole('button', { name: 'Create schedule' }))
    expect(await within(d).findByText(/needs one/)).toBeInTheDocument()
  })

  it('sends the result to a channel the administrator set up, with the events and output choice', async () => {
    mockGetMe.mockResolvedValue(ADMIN)
    list = []
    routes['GET /api/notification-channels'] = { body: [{ name: 'ops', type: 'slack' }, { name: 'mail', type: 'smtp' }] }
    routes['POST /api/resources/agentschedules'] = { status: 201, body: stored() }
    setup()
    await userEvent.click(await screen.findByRole('button', { name: 'New schedule' }))
    const d = await screen.findByRole('dialog')
    await userEvent.type(within(d).getByLabelText('Name'), 'weekday-digest')
    await userEvent.selectOptions(within(d).getByLabelText('Which agent?'), 'digest-agent')
    await userEvent.type(within(d).getByLabelText('Instruction'), 'go')
    expect(within(d).queryByLabelText('A run fails')).toBeNull()
    const select = within(d).getByLabelText('Also send a message to')
    await waitFor(() => expect(within(select).getByRole('option', { name: 'ops (slack)' })).toBeInTheDocument())
    await userEvent.selectOptions(select, 'ops')
    expect(within(d).getByLabelText('A run fails')).toBeChecked()
    await userEvent.click(within(d).getByLabelText('A run succeeds'))
    await userEvent.click(within(d).getByLabelText("Include the run's output in the message"))
    await userEvent.click(within(d).getByRole('button', { name: 'Create schedule' }))
    await waitFor(() => expect(calls.some((x) => x.method === 'POST' && x.url === '/api/resources/agentschedules')).toBe(true))
    const post = calls.find((x) => x.method === 'POST' && x.url === '/api/resources/agentschedules')!
    expect(post.body.spec.result).toEqual({ notify: { channel: 'ops', on: ['Failed', 'Succeeded'], includeOutput: true } })
  })

  it('refuses a channel with no event chosen, and keeps an administrator-set egress when saving', async () => {
    mockGetMe.mockResolvedValue(ADMIN)
    const withResult = stored()
    ;(withResult.spec as Record<string, unknown>).result = { egress: { type: 'redis' }, notify: { channel: 'ops', on: ['Failed'] } }
    routes['GET /api/notification-channels'] = { body: [{ name: 'ops', type: 'slack' }] }
    routes['GET /api/resources/agentschedules/team-a/weekday-digest'] = { body: withResult }
    routes['PUT /api/resources/agentschedules/team-a/weekday-digest'] = { body: withResult }
    setup()
    await userEvent.click(within(await card()).getByRole('button', { name: 'Edit' }))
    const d = await screen.findByRole('dialog')
    expect(await within(d).findByLabelText('Also send a message to')).toHaveValue('ops')
    await userEvent.click(within(d).getByLabelText('A run fails'))
    await userEvent.click(within(d).getByRole('button', { name: 'Save changes' }))
    expect(await within(d).findByText(/at least one thing/)).toBeInTheDocument()
    expect(calls.some((x) => x.method === 'PUT')).toBe(false)
    await userEvent.click(within(d).getByLabelText('A run fails'))
    await userEvent.click(within(d).getByRole('button', { name: 'Save changes' }))
    await waitFor(() => expect(calls.some((x) => x.method === 'PUT')).toBe(true))
    expect(calls.find((x) => x.method === 'PUT')!.body.spec.result).toEqual({ egress: { type: 'redis' }, notify: { channel: 'ops', on: ['Failed'], includeOutput: false } })
  })

  it('choosing no channel removes notify and leaves the rest of result', async () => {
    mockGetMe.mockResolvedValue(ADMIN)
    const withResult = stored()
    ;(withResult.spec as Record<string, unknown>).result = { egress: { type: 'redis' }, notify: { channel: 'ops' } }
    routes['GET /api/notification-channels'] = { body: [{ name: 'ops', type: 'slack' }] }
    routes['GET /api/resources/agentschedules/team-a/weekday-digest'] = { body: withResult }
    routes['PUT /api/resources/agentschedules/team-a/weekday-digest'] = { body: withResult }
    setup()
    await userEvent.click(within(await card()).getByRole('button', { name: 'Edit' }))
    const d = await screen.findByRole('dialog')
    await userEvent.selectOptions(await within(d).findByLabelText('Also send a message to'), '')
    await userEvent.click(within(d).getByRole('button', { name: 'Save changes' }))
    await waitFor(() => expect(calls.some((x) => x.method === 'PUT')).toBe(true))
    expect(calls.find((x) => x.method === 'PUT')!.body.spec.result).toEqual({ egress: { type: 'redis' } })
  })

  it('an admin sends reports to a Slack channel picked from a connected app', async () => {
    mockGetMe.mockResolvedValue(ADMIN)
    routes['GET /api/chat-apps$'] = { body: [{ id: 'inb_1', name: 'orca-bot', kind: 'slack', canListPlaces: true }, { id: 'inb_2', name: 'mm', kind: 'mattermost', canListPlaces: false }] }
    routes['GET /api/chat-apps/inb_1/places'] = { body: [{ id: 'C0REPORTS', name: '#reports', member: true }, { id: 'C0OTHER', name: '#other', member: false }] }
    const withResult = stored()
    ;(withResult.spec as Record<string, unknown>).result = { notify: { channel: 'ops', on: ['Failed'] } }
    routes['GET /api/resources/agentschedules/team-a/weekday-digest'] = { body: withResult }
    routes['PUT /api/resources/agentschedules/team-a/weekday-digest'] = { body: withResult }
    setup()
    await userEvent.click(within(await card()).getByRole('button', { name: 'Edit' }))
    const d = await screen.findByRole('dialog')
    await userEvent.selectOptions(await within(d).findByLabelText('Also send a message to'), '__slack__')
    await userEvent.click(within(d).getByRole('button', { name: 'Save changes' }))
    expect(await within(d).findByText(/Choose a channel \(its ID|Choose the Slack app and channel/)).toBeInTheDocument()
    expect(calls.some((x) => x.method === 'PUT')).toBe(false)
    // The only Slack app is chosen for them; the Mattermost one is not offered.
    await waitFor(() => expect(within(d).getByLabelText('Slack app')).toHaveValue('orca-bot'))
    expect(within(d).queryByRole('option', { name: 'mm' })).toBeNull()
    const picker = await within(d).findByRole('combobox', { name: 'Slack channel' })
    expect(within(picker).getByRole('option', { name: '#other (invite the app first)' })).toBeInTheDocument()
    await userEvent.selectOptions(picker, 'C0REPORTS')
    if (!(within(d).getByLabelText("Include the run's output in the message") as HTMLInputElement).checked) {
      expect(within(d).getByText(/The whole output is sent; a long one takes several messages\./)).toBeInTheDocument()
    }
    await userEvent.click(within(d).getByRole('button', { name: 'Save changes' }))
    await waitFor(() => expect(calls.some((x) => x.method === 'PUT')).toBe(true))
    expect(calls.find((x) => x.method === 'PUT')!.body.spec.result).toEqual({
      notify: { slack: { app: 'orca-bot', channelID: 'C0REPORTS', channelName: '#reports' }, on: ['Failed'], includeOutput: false },
    })
  })

  it('a builder cannot change where reports go, and saving keeps it as it was', async () => {
    const notify = { slack: { app: 'orca-bot', channelID: 'C0REPORTS', channelName: '#reports' }, on: ['Succeeded'] }
    const withResult = stored()
    ;(withResult.spec as Record<string, unknown>).result = { notify }
    routes['GET /api/resources/agentschedules/team-a/weekday-digest'] = { body: withResult }
    routes['PUT /api/resources/agentschedules/team-a/weekday-digest'] = { body: withResult }
    setup()
    await userEvent.click(within(await card()).getByRole('button', { name: 'Edit' }))
    const d = await screen.findByRole('dialog')
    expect(within(d).getByText(/Only an admin can change where reports go/)).toBeInTheDocument()
    expect(await within(d).findByLabelText('Also send a message to')).toBeDisabled()
    expect(within(d).getByText(/#reports \(C0REPORTS\), through the app orca-bot/)).toBeInTheDocument()
    await userEvent.click(within(d).getByRole('button', { name: 'Save changes' }))
    await waitFor(() => expect(calls.some((x) => x.method === 'PUT')).toBe(true))
    expect(calls.find((x) => x.method === 'PUT')!.body.spec.result).toEqual({ notify })
    expect(calls.some((x) => x.url.startsWith('/api/chat-apps'))).toBe(false)
  })

  it('says on the card and in the editor that a destination comes from Git, and keeps the editor read-only even for an admin', async () => {
    mockGetMe.mockResolvedValue(ADMIN)
    list = [summary({ ownership: { owner: 'git', editable: false, handoff: { pending: false } } as ScheduleSummary['ownership'], notify: { channel: 'home-slack', problem: 'the Secret of channel "home-slack" was not found' } })]
    const git = stored('git')
    ;(git.spec as Record<string, unknown>).result = { notify: { channel: 'home-slack' } }
    routes['GET /api/resources/agentschedules/team-a/weekday-digest'] = { body: git }
    setup()
    const c = await card()
    expect(within(c).getByText(/the notification channel home-slack/)).toBeInTheDocument()
    expect(within(c).getByText(/set in Git/)).toBeInTheDocument()
    expect(within(c).getByText(/Messages cannot be sent: the Secret/)).toBeInTheDocument()
    await userEvent.click(within(c).getByRole('button', { name: 'View' }))
    const d = await screen.findByRole('dialog')
    expect(within(d).getByText(/What is shown here is what Git says/)).toBeInTheDocument()
    expect(within(d).getByLabelText('Also send a message to')).toBeDisabled()
    expect(calls.some((x) => x.url.startsWith('/api/chat-apps'))).toBe(false)
  })

  const gitRow = (notify: ScheduleSummary['notify']) =>
    summary({ ownership: { owner: 'git', editable: false, handoff: { pending: false } } as ScheduleSummary['ownership'], notify })

  it('an admin picks another channel for a Git-managed schedule by name, without touching the schedule', async () => {
    mockGetMe.mockResolvedValue(ADMIN)
    list = [gitRow({ channel: 'home-slack' })]
    routes['GET /api/chat-apps$'] = { body: [{ id: 'inb_1', name: 'orca-bot', kind: 'slack', canListPlaces: true }] }
    routes['GET /api/chat-apps/inb_1/places'] = { body: [{ id: 'C0REPORTS', name: '#team-reports', member: true }] }
    routes['PUT /api/schedules/team-a/weekday-digest/destination'] = { body: { status: 'saved' } }
    setup()
    await userEvent.click(within(await card()).getByRole('button', { name: 'Change channel' }))
    const d = await screen.findByRole('dialog')
    expect(within(d).queryByRole('button', { name: 'Reset to Git' })).toBeNull()
    await userEvent.click(within(d).getByRole('button', { name: 'Save channel' }))
    expect(await within(d).findByText(/Choose a channel \(its ID|Choose the Slack app and channel/)).toBeInTheDocument()
    expect(calls.some((x) => x.method === 'PUT')).toBe(false)
    await userEvent.selectOptions(await within(d).findByRole('combobox', { name: 'Slack channel' }), 'C0REPORTS')
    await userEvent.click(within(d).getByRole('button', { name: 'Save channel' }))
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('now sends its messages to #team-reports'))
    const writes = calls.filter((x) => x.method !== 'GET')
    expect(writes).toEqual([{ method: 'PUT', url: '/api/schedules/team-a/weekday-digest/destination', body: { app: 'orca-bot', channelID: 'C0REPORTS', channelName: '#team-reports' } }])
  })

  it('shows a channel chosen here next to what Git says, resets it, and says why a save was refused', async () => {
    mockGetMe.mockResolvedValue(ADMIN)
    list = [gitRow({ slack: { app: 'orca-bot', channelID: 'C0REPORTS', channelName: '#team-reports' }, override: { setBy: 'ada', setAt: '2026-11-26T00:00:00Z', git: { channel: 'home-slack' } } })]
    routes['PUT /destination'] = { status: 422, body: { error: 'the chat app "orca-bot" is turned off' } }
    routes['DELETE /destination'] = { body: { status: 'reset' } }
    setup()
    const c = await card()
    expect(within(c).getByText(/changed here by ada\. Git says the notification channel home-slack\./)).toBeInTheDocument()
    await userEvent.click(within(c).getByRole('button', { name: 'Change channel' }))
    const d = await screen.findByRole('dialog')
    await userEvent.click(within(d).getByRole('button', { name: 'Save channel' }))
    expect(await within(d).findByText(/is turned off/)).toBeInTheDocument()
    await userEvent.click(within(d).getByRole('button', { name: 'Reset to Git' }))
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('where Git says again'))
    expect(calls.some((x) => x.method === 'DELETE' && x.url.endsWith('/destination'))).toBe(true)
  })

  it('offers Change channel only to an admin, and only for a Git-managed schedule that sends messages', async () => {
    for (const [who, row] of [[BUILDER, gitRow({ channel: 'home-slack' })], [ADMIN, gitRow(undefined)], [ADMIN, summary({ notify: { channel: 'ops' } })]] as const) {
      mockGetMe.mockResolvedValue(who)
      list = [row]
      const view = setup()
      const c = await card()
      await waitFor(() => expect(mockGetMe).toHaveBeenCalled())
      await new Promise((r) => setTimeout(r, 20))
      expect(within(c).queryByRole('button', { name: 'Change channel' })).toBeNull()
      view.unmount()
    }
  })

  it('a viewer does not ask for channels', async () => {
    mockGetMe.mockResolvedValue(VIEWER)
    routes['GET /api/resources/agentschedules/team-a/weekday-digest'] = { body: stored() }
    setup()
    await userEvent.click(within(await card()).getByRole('button', { name: 'View' }))
    await screen.findByRole('dialog')
    expect(calls.some((x) => x.url.startsWith('/api/notification-channels'))).toBe(false)
  })

  it('edits with the version it read and shows a conflict in words', async () => {
    routes['GET /api/resources/agentschedules/team-a/weekday-digest'] = { body: stored() }
    routes['PUT /api/resources/agentschedules/team-a/weekday-digest'] = { status: 409, body: { error: 'AgentSchedule team-a/weekday-digest changed since you read it', currentResourceVersion: '9' } }
    setup()
    await userEvent.click(within(await card()).getByRole('button', { name: 'Edit' }))
    const d = await screen.findByRole('dialog')
    const input = within(d).getByLabelText('Instruction')
    await userEvent.clear(input)
    await userEvent.type(input, 'new text')
    await userEvent.click(within(d).getByRole('button', { name: 'Save changes' }))
    expect(await within(d).findByText(/changed since you read it/)).toBeInTheDocument()
    const put = calls.find((x) => x.method === 'PUT')!
    expect(put.body.metadata.resourceVersion).toBe('7')
    expect(put.body.spec.input).toBe('new text')
    expect(put.body.ownership).toBeUndefined()
  })

  it('deletes after a confirmation', async () => {
    routes['DELETE /api/resources/agentschedules/team-a/weekday-digest'] = {}
    setup()
    await userEvent.click(within(await card()).getByRole('button', { name: 'Delete' }))
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete schedule' }))
    await waitFor(() => expect(calls.some((x) => x.method === 'DELETE')).toBe(true))
  })
})

describe('formProblems', () => {
  const ok = { name: 'a', namespace: 'x', agent: 'ag', when: { preset: 'daily' as const, hour: 8, minute: 0, weekday: 1, cron: '' }, timeZone: 'UTC', input: 'go', concurrency: 'Forbid' as const, maxPerRun: '1', maxPerDay: '', maxPerMonth: '', notifyChannel: '', slackApp: '', slackChannelID: '', slackChannelName: '', notifyOn: ['Failed'], notifyOutput: false }
  it('accepts a complete form', () => expect(formProblems(ok, false)).toEqual({}))
  it('names every problem', () => {
    const p = formProblems({ ...ok, name: 'Bad Name', agent: '', input: ' ', maxPerRun: 'x', maxPerMonth: '0' }, false)
    expect(Object.keys(p).sort()).toEqual(['agent', 'input', 'maxPerMonth', 'maxPerRun', 'name'])
  })
  it('asks for an event, and for a finished run when the output is wanted', () => {
    expect(formProblems({ ...ok, notifyChannel: 'ops', notifyOn: [] }, false)).toHaveProperty('notifyOn')
    expect(formProblems({ ...ok, notifyChannel: 'ops', notifyOn: ['BreakerOpen'], notifyOutput: true }, false)).toHaveProperty('notifyOutput')
    expect(formProblems({ ...ok, notifyChannel: 'ops', notifyOn: ['Failed'], notifyOutput: true }, false)).toEqual({})
    expect(formProblems({ ...ok, notifyChannel: '', notifyOn: [] }, false)).toEqual({})
  })
  it('does not check the name of an existing schedule', () => expect(formProblems({ ...ok, name: 'Whatever' }, true)).toEqual({}))
})

describe('schedule badge and chip', () => {
  it('the fleet badge says when it runs next and how the last run went', () => {
    render(<MemoryRouter><ScheduleBadge schedules={[summary(), summary({ name: 'b' })]} /></MemoryRouter>)
    const a = screen.getByRole('link')
    expect(a).toHaveAccessibleName(/Scheduled: weekday-digest and 1 more, next .*Last run succeeded/)
    expect(a).toHaveAttribute('href', '/operate/schedules')
  })
  it('shows nothing without schedules', () => {
    const { container } = render(<MemoryRouter><ScheduleBadge schedules={[]} /></MemoryRouter>)
    expect(container).toBeEmptyDOMElement()
  })
  it('the run chip names the schedule and runs it again', async () => {
    const again = vi.fn()
    render(<MemoryRouter><ScheduledByChip schedule="weekday-digest" trigger="schedule" scheduledFor="2026-11-16T08:00:00+01:00" onRunAgain={again} /></MemoryRouter>)
    expect(screen.getByText(/Scheduled by/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'weekday-digest' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Run again now' }))
    expect(again).toHaveBeenCalled()
  })
  it('a manual run says so', () => {
    render(<MemoryRouter><ScheduledByChip schedule="weekday-digest" trigger="manual" /></MemoryRouter>)
    expect(screen.getByText(/Run now from/)).toBeInTheDocument()
  })
})
