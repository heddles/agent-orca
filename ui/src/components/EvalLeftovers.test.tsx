import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'

const mockGetMe = vi.fn()
vi.mock('../api/fleet', () => ({ getMe: () => mockGetMe() }))

import { OutputCard } from './OutputCard'
import { EvalDatasetDetail } from './EvalDatasetDetail'

const me = (roles: string[]) => ({ userId: 'u', groups: [], tenant: { id: 't', name: 'T' }, workspaces: [{ name: 'A', namespace: 'team-a', primary: true }], roles })
const DATASET = { id: 7, namespace: 'team-a', name: 'refunds', version: 3, caseCount: 1, createdBy: 'u', createdAt: '', updatedAt: '' }

const calls: { method: string; url: string; body: unknown }[] = []
let rating: 1 | -1 | null = 1

function stubFetch() {
  vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET'
    const body = init?.body ? JSON.parse(String(init.body)) : undefined
    calls.push({ method, url, body })
    const json = (b: unknown, status = 200, headers: Record<string, string> = {}) =>
      Promise.resolve(new Response(JSON.stringify(b), { status, headers }))
    if (/\/feedback$/.test(url)) return json({ feedback: rating === null ? [] : [{ rating }] })
    if (/\/export$/.test(url)) {
      return Promise.resolve(new Response('{"input":"hi"}\n', { status: 200, headers: { 'Content-Disposition': 'attachment; filename="refunds-v3.jsonl"' } }))
    }
    if (/\/api\/evals\/datasets\?/.test(url)) return json({ datasets: [DATASET] })
    if (/\/datasets\/7$/.test(url)) return json(DATASET)
    if (/\/cases/.test(url)) return json({ cases: [], datasetVersion: 3 })
    if (/\/graders$/.test(url)) return json({ graders: [] })
    if (/\/dataset$/.test(url)) {
      const preview = (body as { preview?: boolean }).preview
      return json({ case: { id: preview ? 0 : 5, input: 'redacted input', referenceKind: 'none' }, redaction: { input: { replacements: 1 } }, preview, datasetVersion: preview ? undefined : 4 })
    }
    return json({ error: 'not found' }, 404)
  }))
}

beforeEach(() => {
  calls.length = 0
  rating = 1
  mockGetMe.mockResolvedValue(me(['viewer', 'runner', 'builder']))
  stubFetch()
})
afterEach(() => vi.unstubAllGlobals())

describe('thumbs-up to dataset bridge', () => {
  const setup = () => render(<OutputCard output="done" feedbackRun={{ namespace: 'team-a', runName: 'run-1' }} />)

  it('offers Add to dataset only after a thumbs-up, and never copies on its own', async () => {
    setup()
    expect(await screen.findByRole('button', { name: /Add to dataset/ })).toBeInTheDocument()
    expect(calls.some((c) => c.method === 'POST')).toBe(false)
  })

  it('does not offer it without a thumbs-up', async () => {
    rating = -1
    setup()
    await screen.findByRole('button', { name: /Poor choice/ })
    expect(screen.queryByRole('button', { name: /Add to dataset/ })).not.toBeInTheDocument()
  })

  it('does not offer it to a viewer', async () => {
    mockGetMe.mockResolvedValue(me(['viewer']))
    setup()
    await screen.findByRole('button', { name: /Good choice/ })
    await waitFor(() => expect(mockGetMe).toHaveBeenCalled())
    expect(screen.queryByRole('button', { name: /Add to dataset/ })).not.toBeInTheDocument()
  })

  it('previews, then adds, without using the output as the reference by default', async () => {
    setup()
    await userEvent.click(await screen.findByRole('button', { name: /Add to dataset/ }))
    await screen.findByRole('option', { name: 'refunds' })
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }))
    expect(await screen.findByText('redacted input')).toBeInTheDocument()
    const prev = calls.find((c) => c.method === 'POST')!
    expect(prev.url).toBe('/api/runs/team-a/run-1/dataset')
    expect(prev.body).toEqual({ datasetId: 7, tags: [], preview: true })
    await userEvent.click(screen.getByRole('button', { name: 'Add case' }))
    await screen.findByText(/Added to refunds/)
    const posts = calls.filter((c) => c.method === 'POST')
    expect(posts).toHaveLength(2)
    expect(posts[1].body).toEqual({ datasetId: 7, tags: [], preview: false })
    expect(posts[1].body).not.toHaveProperty('useOutput')
  })

  it('sends useOutput only when the box is ticked', async () => {
    setup()
    await userEvent.click(await screen.findByRole('button', { name: /Add to dataset/ }))
    await screen.findByRole('option', { name: 'refunds' })
    await userEvent.click(screen.getByRole('checkbox'))
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }))
    await screen.findByText('redacted input')
    expect(calls.find((c) => c.method === 'POST')!.body).toMatchObject({ useOutput: true })
  })
})

describe('dataset export', () => {
  const setup = () => render(<MemoryRouter><EvalDatasetDetail id={7} /></MemoryRouter>)

  it('downloads the JSONL for a builder', async () => {
    const create = vi.fn(() => 'blob:x')
    const revoke = vi.fn()
    vi.stubGlobal('URL', Object.assign(URL, { createObjectURL: create, revokeObjectURL: revoke }))
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Export JSONL' }))
    await waitFor(() => expect(click).toHaveBeenCalled())
    expect(calls.some((c) => c.method === 'GET' && c.url === '/api/evals/datasets/7/export')).toBe(true)
    expect(create).toHaveBeenCalled()
    click.mockRestore()
  })

  it('hides export from a viewer', async () => {
    mockGetMe.mockResolvedValue(me(['viewer']))
    setup()
    await screen.findByText(/You can read this dataset/)
    expect(screen.queryByRole('button', { name: 'Export JSONL' })).not.toBeInTheDocument()
  })
})
