import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { KubeObject } from '../api/builder'
import type { RevisionDetail, RevisionSummary } from '../api/revisions'
import { diffLines } from '../lib/lineDiff'
import { RevisionHistory, describeRevision, relativeTime } from './RevisionHistory'

function agent(over: { prompt?: string; tools?: string[]; model?: string; rv?: string } = {}): KubeObject {
  return {
    apiVersion: 'agentorca.agentorca.io/v1alpha1',
    kind: 'Agent',
    metadata: { name: 'helper', namespace: 'team-a', resourceVersion: over.rv ?? '9' },
    spec: { modelSelectorRef: over.model ?? 'default', systemPrompt: over.prompt ?? 'Be helpful.', tools: over.tools ?? ['search'] },
  }
}

const NOW = Date.now()
const ago = (ms: number) => new Date(NOW - ms).toISOString()

const PAGE1: RevisionSummary[] = [
  { revision: 3, action: 'update', userId: 'Ada', createdAt: ago(2 * 3600_000), summary: 'Changed the instructions', resourceVersion: '9' },
  { revision: 2, action: 'restore', userId: 'Bo', createdAt: ago(26 * 3600_000), restoredFrom: 1 },
]
const PAGE2: RevisionSummary[] = [{ revision: 1, action: 'create', userId: 'Ada', createdAt: ago(9 * 86400_000) }]

const DETAILS: Record<number, RevisionDetail> = {
  3: { ...PAGE1[0], object: agent({ prompt: 'Be brief.', tools: ['search', 'tickets'] }) },
  2: { ...PAGE1[1], object: agent({ prompt: 'Be helpful.', tools: ['search'] }) },
  1: { ...PAGE2[0], object: agent({ prompt: 'Hello.', tools: [] }) },
}

type Reply = { status: number; body: unknown }
let routes: (url: string, init?: RequestInit) => Reply | undefined
const fetchMock = vi.fn()

beforeEach(() => {
  fetchMock.mockReset()
  routes = (url) => {
    if (/revisions\?/.test(url) && url.includes('before=c1')) return { status: 200, body: { revisions: PAGE2, next: '' } }
    if (/revisions\?/.test(url)) return { status: 200, body: { revisions: PAGE1, next: 'c1' } }
    const m = /revisions\/(\d+)$/.exec(url)
    if (m) return { status: 200, body: DETAILS[Number(m[1])] }
    return undefined
  }
  fetchMock.mockImplementation((url: string, init?: RequestInit) => {
    const r = routes(url, init) ?? { status: 404, body: { error: 'not found' } }
    return Promise.resolve(new Response(JSON.stringify(r.body), { status: r.status }))
  })
  vi.stubGlobal('fetch', fetchMock)
})
afterEach(() => vi.unstubAllGlobals())

function setup(over: Partial<React.ComponentProps<typeof RevisionHistory>> = {}) {
  const props = {
    kind: 'agents', namespace: 'team-a', name: 'helper', isAgent: true, refreshKey: 0,
    live: agent({ prompt: 'Be brief.', tools: ['search', 'tickets'], rv: '9' }),
    canRestore: true, readOnlyNote: '', dirty: false,
    onLoaded: vi.fn(), onRestored: vi.fn(), onReload: vi.fn().mockResolvedValue(undefined),
    ...over,
  }
  const user = userEvent.setup()
  render(<RevisionHistory {...props} />)
  return { props, user }
}

describe('wording', () => {
  it('uses the summary, else a sentence from the action', () => {
    expect(describeRevision(PAGE1[0])).toBe('Changed the instructions')
    expect(describeRevision(PAGE1[1])).toBe('Restored version 1')
    expect(describeRevision({ ...PAGE2[0] })).toBe('Created')
    expect(describeRevision({ ...PAGE2[0], action: 'update' })).toBe('Saved a change')
    expect(describeRevision({ ...PAGE2[0], action: 'delete' })).toBe('Deleted')
  })
  it('says how long ago', () => {
    expect(relativeTime(ago(2 * 3600_000), NOW)).toBe('2 hours ago')
    expect(relativeTime(ago(5_000), NOW)).toBe('just now')
  })
  it('diffs lines', () => {
    expect(diffLines('a\nb\nc\n', 'a\nx\nc\n')).toEqual([
      { kind: 'same', text: 'a' }, { kind: 'remove', text: 'b' }, { kind: 'add', text: 'x' }, { kind: 'same', text: 'c' },
    ])
  })
})

describe('RevisionHistory: the list', () => {
  it('lists versions in plain words, with the full time for hover and screen readers, and pages', async () => {
    const { props, user } = setup()
    expect(screen.getByRole('status')).toHaveTextContent('Loading versions')
    const list = await screen.findByRole('list', { name: /Versions, newest first/ })
    const rows = within(list).getAllByRole('listitem')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent('Version 3')
    expect(rows[0]).toHaveTextContent('Ada')
    expect(rows[0]).toHaveTextContent('2 hours ago')
    expect(rows[0]).toHaveTextContent('Changed the instructions')
    expect(rows[1]).toHaveTextContent('Restored version 1')
    const time = rows[0].querySelector('time')!
    expect(time).toHaveAttribute('title', new Date(PAGE1[0].createdAt).toLocaleString())
    expect(time).toHaveTextContent(`(${new Date(PAGE1[0].createdAt).toLocaleString()})`)
    expect(props.onLoaded).toHaveBeenCalledWith(3)

    await user.click(screen.getByRole('button', { name: 'Load older versions' }))
    await waitFor(() => expect(within(screen.getByRole('list', { name: /Versions/ })).getAllByRole('listitem')).toHaveLength(3))
    expect(fetchMock.mock.calls.some(([u]) => String(u).includes('before=c1'))).toBe(true)
    expect(screen.queryByRole('button', { name: 'Load older versions' })).not.toBeInTheDocument()
  })

  it('says so when there are no versions', async () => {
    routes = () => ({ status: 200, body: { revisions: [], next: '' } })
    const { props } = setup()
    expect(await screen.findByText(/There are no saved versions yet/)).toBeInTheDocument()
    expect(props.onLoaded).toHaveBeenCalledWith(null)
  })

  it('explains when PostgreSQL is missing', async () => {
    routes = () => ({ status: 501, body: { error: 'revision history needs PostgreSQL' } })
    setup()
    expect(await screen.findByRole('alert')).toHaveTextContent('Version history is not turned on for this installation. It needs PostgreSQL.')
  })

  it('announces a load error', async () => {
    routes = () => ({ status: 500, body: { error: 'boom' } })
    setup()
    expect(await screen.findByRole('alert')).toHaveTextContent('could not be loaded: boom')
  })

  it('reloads when refreshKey changes', async () => {
    const { props } = setup()
    await screen.findByRole('list', { name: /Versions/ })
    const before = fetchMock.mock.calls.length
    const { rerender } = render(<RevisionHistory {...props} refreshKey={1} />)
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThan(before))
    rerender(<RevisionHistory {...props} refreshKey={2} />)
  })
})

describe('RevisionHistory: viewing and comparing', () => {
  it('shows what the agent was at a version, in the form words', async () => {
    const { user } = setup()
    await user.click(await screen.findByRole('button', { name: /Version 2/ }))
    expect(await screen.findByText('Be helpful.')).toBeInTheDocument()
    expect(screen.getByText('Instructions')).toBeInTheDocument()
    expect(screen.getByText('Connections')).toBeInTheDocument()
    expect(screen.getByText('Safety policy')).toBeInTheDocument()
  })

  it('compares a version with the current one: a changed field, an added connection, an unchanged field', async () => {
    const { user } = setup()
    await user.click(await screen.findByRole('button', { name: /Version 2/ }))
    await user.click(await screen.findByRole('button', { name: 'Compare with the current version' }))
    const table = await screen.findByRole('table')
    const row = (label: string) => within(table).getByRole('rowheader', { name: new RegExp(label) }).closest('tr') as HTMLElement
    expect(row('Instructions')).toHaveTextContent('Changed')
    expect(row('Instructions')).toHaveTextContent('Be helpful.')
    expect(row('Instructions')).toHaveTextContent('Be brief.')
    expect(row('Connections')).toHaveTextContent('Added: tickets')
    expect(row('Model')).toHaveTextContent('Unchanged')
    expect(screen.getByText('2 fields changed.')).toBeInTheDocument()
    expect(within(table).getByRole('columnheader', { name: 'Current version' })).toBeInTheDocument()
  })

  it('lets two versions be chosen and shows the YAML diff with markers beyond colour', async () => {
    const { user } = setup()
    await user.click(await screen.findByRole('button', { name: /Version 2/ }))
    await user.click(await screen.findByRole('button', { name: 'Compare with the current version' }))
    await user.selectOptions(await screen.findByLabelText('Newer version'), '3')
    await user.click(screen.getByRole('checkbox', { name: 'Show as YAML' }))
    const diff = await screen.findByLabelText('Changes as YAML')
    await waitFor(() => expect(diff).toHaveTextContent('Removed: '))
    expect(diff).toHaveTextContent('Removed: systemPrompt: Be helpful.')
    expect(diff).toHaveTextContent('Added: systemPrompt: Be brief.')
    expect(diff).toHaveTextContent('Added: - tickets')
    expect(diff.textContent).toMatch(/\+ .*Be brief/)
    expect(diff.textContent).toMatch(/- .*Be helpful/)
  })

  it('shows YAML only for kinds other than Agent', async () => {
    const { user } = setup({ kind: 'tools', isAgent: false })
    await user.click(await screen.findByRole('button', { name: /Version 2/ }))
    await user.click(await screen.findByRole('button', { name: 'Compare with the current version' }))
    expect(screen.queryByRole('checkbox', { name: 'Show as YAML' })).not.toBeInTheDocument()
    expect(await screen.findByLabelText('Changes as YAML')).toBeInTheDocument()
  })
})

describe('RevisionHistory: restore', () => {
  async function openRestore(user: ReturnType<typeof userEvent.setup>) {
    await user.click(await screen.findByRole('button', { name: /Version 2/ }))
    await user.click(await screen.findByRole('button', { name: 'Restore this version' }))
    return screen.findByRole('dialog')
  }

  it('confirms in words, then sends the live resourceVersion', async () => {
    const restored = agent({ prompt: 'Be helpful.', rv: '10' })
    const posts: Array<{ url: string; body: string }> = []
    const base = routes
    routes = (url, init) => {
      if (init?.method === 'POST') { posts.push({ url, body: String(init.body) }); return { status: 200, body: restored } }
      return base(url, init)
    }
    const { props, user } = setup()
    const dialog = await openRestore(user)
    expect(dialog).toHaveTextContent('This makes version 2 the current version again. Version 3 stays in the history, so you can go back.')
    await user.click(within(dialog).getByRole('button', { name: 'Restore version 2' }))
    await waitFor(() => expect(props.onRestored).toHaveBeenCalledWith(restored, 2))
    expect(posts).toEqual([{ url: '/api/resources/agents/team-a/helper/revisions/2/restore', body: JSON.stringify({ resourceVersion: '9' }) }])
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('cancel changes nothing', async () => {
    const { props, user } = setup()
    const dialog = await openRestore(user)
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(props.onRestored).not.toHaveBeenCalled()
  })

  it('on a 409 says someone changed the agent, offers Reload and keeps the dialog', async () => {
    const base = routes
    routes = (url, init) => (init?.method === 'POST' ? { status: 409, body: { error: 'conflict', currentResourceVersion: '12' } } : base(url, init))
    const { props, user } = setup()
    const dialog = await openRestore(user)
    await user.click(within(dialog).getByRole('button', { name: 'Restore version 2' }))
    const alert = await within(dialog).findByRole('alert')
    expect(alert).toHaveTextContent('Someone changed this agent in the meantime')
    expect(props.onRestored).not.toHaveBeenCalled()
    await user.click(within(alert).getByRole('button', { name: 'Reload' }))
    await waitFor(() => expect(props.onReload).toHaveBeenCalled())
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument())
  })

  it('has no Restore button when restoring is not allowed, and says why', async () => {
    const { user } = setup({ canRestore: false, readOnlyNote: 'This agent is not managed here, so its history is read-only and no version can be restored.' })
    expect(await screen.findByText(/history is read-only/)).toBeInTheDocument()
    await user.click(await screen.findByRole('button', { name: /Version 2/ }))
    await screen.findByRole('button', { name: 'Compare with the current version' })
    expect(screen.queryByRole('button', { name: 'Restore this version' })).not.toBeInTheDocument()
  })

  it('does not offer Restore on the newest version', async () => {
    const { user } = setup()
    await user.click(await screen.findByRole('button', { name: /Version 3/ }))
    await screen.findByText('This is the newest version.')
    expect(screen.queryByRole('button', { name: 'Restore this version' })).not.toBeInTheDocument()
  })
})
