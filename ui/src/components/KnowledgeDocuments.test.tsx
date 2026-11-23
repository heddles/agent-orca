import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

import { KnowledgeDocuments } from './KnowledgeDocuments'

const LIMITS = { maxDocBytes: 262144, maxDocs: 50, maxTotalBytes: 921600, types: ['.txt', '.md', '.markdown', '.csv'] }
const docsBody = (over: object = {}) => ({ documents: [{ name: 'returns.md', bytes: 2048 }], totalBytes: 2048, limits: LIMITS, editable: true, ...over })

type Reply = { status: number; body?: unknown }
let routes: Record<string, Reply>
const calls: { method: string; url: string; body: string | undefined }[] = []

beforeEach(() => {
  calls.length = 0
  routes = { 'GET /api/knowledge/team-a/handbook/documents$': { status: 200, body: docsBody() } }
  vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET'
    calls.push({ method, url, body: typeof init?.body === 'string' ? init.body : undefined })
    const key = Object.keys(routes).find((k) => { const [m, re] = k.split(' '); return m === method && new RegExp(re).test(url) })
    const r = key ? routes[key] : { status: 404, body: { error: 'not found' } }
    return Promise.resolve(new Response(r.body === undefined ? null : JSON.stringify(r.body), { status: r.status }))
  }))
  vi.stubGlobal('confirm', vi.fn(() => true))
})
afterEach(() => vi.unstubAllGlobals())

const items = [{ name: 'handbook', namespace: 'team-a' }, { name: 'other-ns', namespace: 'team-b' }]
const setup = () => render(<KnowledgeDocuments namespace="team-a" items={items} />)

describe('KnowledgeDocuments', () => {
  it('lists the names and sizes of the documents, and only knowledge bases of the workspace', async () => {
    setup()
    const list = await screen.findByRole('list', { name: 'Documents' })
    expect(within(list).getByText(/returns\.md/)).toBeInTheDocument()
    expect(within(list).getByText('(2.0 KB)')).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'handbook' })).toBeInTheDocument()
    expect(screen.queryByRole('option', { name: 'other-ns' })).not.toBeInTheDocument()
    expect(screen.getByText(/Up to 50 documents/)).toBeInTheDocument()
  })

  it('uploads chosen files as plain text and shows the new list', async () => {
    routes['PUT /api/knowledge/team-a/handbook/documents/notes.md'] = {
      status: 201, body: docsBody({ documents: [{ name: 'notes.md', bytes: 7 }, { name: 'returns.md', bytes: 2048 }] }),
    }
    setup()
    await screen.findByRole('list', { name: 'Documents' })
    const file = new File(['# Notes'], 'notes.md', { type: 'text/markdown' })
    await userEvent.setup().upload(screen.getByLabelText('Choose files'), file)
    await waitFor(() => expect(screen.getByText(/notes\.md/)).toBeInTheDocument())
    const put = calls.find((c) => c.method === 'PUT')
    expect(put?.body).toBe('# Notes')
    expect(put?.url).toContain('/documents/notes.md')
    expect(screen.getByLabelText('Choose files')).toHaveAttribute('accept', '.txt,.md,.markdown,.csv')
  })

  it('shows the server refusal (not text, too big, Git-owned) and keeps the list', async () => {
    routes['PUT /api/knowledge/team-a/handbook/documents/scan.txt'] = { status: 415, body: { error: 'the document is not UTF-8 text: only text, markdown and csv files can be added here' } }
    setup()
    await screen.findByRole('list', { name: 'Documents' })
    await userEvent.setup().upload(screen.getByLabelText('Choose files'), new File(['x'], 'scan.txt'))
    expect(await screen.findByRole('alert')).toHaveTextContent('only text, markdown and csv')
    expect(screen.getByText(/returns\.md/)).toBeInTheDocument()
  })

  it('adds pasted text under the given file name', async () => {
    routes['PUT /api/knowledge/team-a/handbook/documents/policy.md'] = { status: 201, body: docsBody({ documents: [{ name: 'policy.md', bytes: 5 }] }) }
    setup()
    await screen.findByRole('list', { name: 'Documents' })
    const user = userEvent.setup()
    expect(screen.getByRole('button', { name: 'Add text' })).toBeDisabled()
    await user.type(screen.getByLabelText('Or paste text: file name'), 'policy.md')
    await user.type(screen.getByLabelText('Text'), 'hello')
    await user.click(screen.getByRole('button', { name: 'Add text' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'PUT')?.body).toBe('hello'))
    expect(within(await screen.findByRole('list', { name: 'Documents' })).getByText(/policy\.md/)).toBeInTheDocument()
  })

  it('removes a document after a warning that embedded text stays, and shows the server caution', async () => {
    routes['DELETE /api/knowledge/team-a/handbook/documents/returns.md'] = { status: 200, body: docsBody({ documents: [], warning: 'Text already embedded stays.' }) }
    setup()
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Remove returns.md' }))
    expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining('stays in the knowledge base'))
    expect(await screen.findByText('Text already embedded stays.')).toBeInTheDocument()
    expect(screen.getByText('No documents have been added here yet.')).toBeInTheDocument()
  })

  it('offers no way to add when the knowledge base is not editable, and says why', async () => {
    routes['GET /api/knowledge/team-a/handbook/documents$'] = { status: 200, body: docsBody({ editable: false, reason: 'This knowledge is managed in Git, so documents are added there.' }) }
    setup()
    expect(await screen.findByText(/managed in Git/)).toBeInTheDocument()
    expect(screen.queryByLabelText('Choose files')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Remove returns.md' })).not.toBeInTheDocument()
  })

  it('renders nothing without a knowledge base in the workspace', () => {
    const { container } = render(<KnowledgeDocuments namespace="team-a" items={[{ name: 'x', namespace: 'team-b' }]} />)
    expect(container).toBeEmptyDOMElement()
  })
})
