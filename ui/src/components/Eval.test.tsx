import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router'

const mockGetMe = vi.fn()
vi.mock('../api/fleet', () => ({ getMe: () => mockGetMe() }))

import { EvalDatasets } from './EvalDatasets'
import { EvalDatasetDetail } from './EvalDatasetDetail'

const me = (roles: string[]) => ({ userId: 'u', groups: [], tenant: { id: 't', name: 'T' }, workspaces: [{ name: 'A', namespace: 'team-a', primary: true }], roles })
const BUILDER = me(['viewer', 'runner', 'builder'])
const VIEWER = me(['viewer'])

const DATASET = { id: 7, namespace: 'team-a', name: 'refunds', description: 'Refund questions', version: 3, caseCount: 1, createdBy: 'u', createdAt: '', updatedAt: '' }
const CASE = { id: 11, datasetId: 7, addedIn: 1, input: 'Can I return it?', reference: 'Yes, within 30 days', referenceKind: 'exact', tags: ['refund'], weight: 1, redacted: true, createdAt: '' }
const GRADER = { id: 21, datasetId: 7, name: 'polite', kind: 'contains', config: { value: 'please' }, weight: 1, required: true, createdBy: 'u', createdAt: '', updatedAt: '' }

type Reply = { status: number; body?: unknown }
let state: { datasets: unknown[]; cases: unknown[]; graders: unknown[] }
let handler: (method: string, url: string, body: unknown) => Reply | undefined
const calls: { method: string; url: string; body: unknown }[] = []

beforeEach(() => {
  calls.length = 0
  mockGetMe.mockResolvedValue(BUILDER)
  state = { datasets: [DATASET], cases: [CASE], graders: [GRADER] }
  handler = () => undefined
  vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET'
    const body = init?.body ? JSON.parse(String(init.body)) : undefined
    calls.push({ method, url, body })
    const custom = handler(method, url, body)
    let r: Reply = custom ?? { status: 404, body: { error: 'not found' } }
    if (!custom && method === 'GET') {
      if (/\/datasets(\?.*)?$/.test(url)) r = { status: 200, body: { datasets: state.datasets } }
      else if (/\/datasets\/7$/.test(url)) r = { status: 200, body: DATASET }
      else if (/\/cases/.test(url)) r = { status: 200, body: { cases: state.cases, datasetVersion: 3 } }
      else if (/\/graders$/.test(url)) r = { status: 200, body: { graders: state.graders } }
    }
    return Promise.resolve(new Response(r.body === undefined ? null : JSON.stringify(r.body), { status: r.status }))
  }))
})
afterEach(() => vi.unstubAllGlobals())

const posted = (method: string, re: RegExp) => calls.find((c) => c.method === method && re.test(c.url))

describe('EvalDatasets', () => {
  const setup = () => render(<MemoryRouter><EvalDatasets /></MemoryRouter>)

  it('lists datasets with a link to each', async () => {
    setup()
    const link = await screen.findByRole('link', { name: 'refunds' })
    expect(link).toHaveAttribute('href', '/evaluate/7')
    expect(screen.getByRole('button', { name: 'New dataset' })).toBeInTheDocument()
  })

  it('explains an empty list', async () => {
    state.datasets = []
    setup()
    expect(await screen.findByText('No datasets yet')).toBeInTheDocument()
  })

  it('shows a readable message when the store is missing', async () => {
    handler = () => ({ status: 503, body: { error: 'evaluations need the PostgreSQL store' } })
    setup()
    expect(await screen.findByRole('alert')).toHaveTextContent(/need the platform PostgreSQL store/)
  })

  it('hides writing from a viewer', async () => {
    mockGetMe.mockResolvedValue(VIEWER)
    setup()
    await screen.findByRole('link', { name: 'refunds' })
    await waitFor(() => expect(screen.queryByRole('button', { name: 'New dataset' })).not.toBeInTheDocument())
    expect(screen.queryByRole('button', { name: 'Delete refunds' })).not.toBeInTheDocument()
  })

  it('creates a dataset and shows a 409 in words', async () => {
    const user = userEvent.setup()
    handler = (m, u) => (m === 'POST' && /datasets$/.test(u) ? { status: 409, body: { error: 'a dataset with that name exists' } } : undefined)
    setup()
    await user.click(await screen.findByRole('button', { name: 'New dataset' }))
    const dialog = screen.getByRole('dialog')
    await user.type(within(dialog).getByLabelText('Name'), 'faq')
    await user.click(within(dialog).getByRole('button', { name: 'Create dataset' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('a dataset with that name exists')
    expect(posted('POST', /datasets$/)?.body).toMatchObject({ name: 'faq' })
  })

  it('deletes after confirming', async () => {
    const user = userEvent.setup()
    handler = (m, u) => (m === 'DELETE' && /datasets\/7$/.test(u) ? { status: 204 } : undefined)
    setup()
    await user.click(await screen.findByRole('button', { name: 'Delete refunds' }))
    expect(screen.getByRole('dialog')).toHaveTextContent('1 case')
    await user.click(screen.getByRole('button', { name: 'Delete dataset' }))
    await waitFor(() => expect(posted('DELETE', /datasets\/7$/)).toBeTruthy())
  })
})

describe('EvalDatasetDetail', () => {
  const setup = () => render(
    <MemoryRouter initialEntries={['/evaluate/7']}>
      <Routes><Route path="/evaluate/:id" element={<EvalDatasetDetail id={7} />} /></Routes>
    </MemoryRouter>,
  )

  it('shows cases, the redaction notice and the limits, and graders', async () => {
    setup()
    expect(await screen.findByText('Can I return it?')).toBeInTheDocument()
    expect(screen.getByText('Redacted')).toBeInTheDocument()
    expect(screen.getByText(/not redacted; only cases copied from a run are/)).toBeInTheDocument()
    expect(screen.getByText(/65,536 bytes/)).toBeInTheDocument()
    expect(await screen.findByText('polite')).toBeInTheDocument()
    expect(screen.getByText('Required', { selector: 'span' })).toBeInTheDocument()
  })

  it('says when the dataset does not exist', async () => {
    handler = (m, u) => (m === 'GET' && /datasets\/7$/.test(u) ? { status: 404, body: { error: 'not found' } } : undefined)
    setup()
    expect(await screen.findByText('Dataset not found')).toBeInTheDocument()
  })

  it('adds a case with a reference and tags', async () => {
    const user = userEvent.setup()
    handler = (m, u) => (m === 'POST' && /cases$/.test(u) ? { status: 201, body: { added: 1, datasetVersion: 4, cases: [] } } : undefined)
    setup()
    await user.click(await screen.findByRole('button', { name: 'Add a case' }))
    const dialog = screen.getByRole('dialog')
    await user.type(within(dialog).getByLabelText('Input'), 'Where is my order?')
    await user.type(within(dialog).getByLabelText('Expected answer (optional)'), 'Check tracking')
    await user.type(within(dialog).getByLabelText('Tags (optional)'), 'orders, shipping')
    await user.click(within(dialog).getByRole('button', { name: 'Add case' }))
    await waitFor(() => expect(posted('POST', /cases$/)).toBeTruthy())
    expect(posted('POST', /cases$/)?.body).toEqual({
      input: 'Where is my order?', reference: 'Check tracking', referenceKind: 'exact', tags: ['orders', 'shipping'], weight: 1,
    })
  })

  it('shows a full dataset (409) when adding', async () => {
    const user = userEvent.setup()
    handler = (m, u) => (m === 'POST' && /cases$/.test(u) ? { status: 409, body: { error: 'the dataset already holds 2000 cases' } } : undefined)
    setup()
    await user.click(await screen.findByRole('button', { name: 'Add a case' }))
    const dialog = screen.getByRole('dialog')
    await user.type(within(dialog).getByLabelText('Input'), 'x')
    await user.click(within(dialog).getByRole('button', { name: 'Add case' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('already holds 2000 cases')
  })

  it('edits and deletes a case', async () => {
    const user = userEvent.setup()
    handler = (m, u) => {
      if (m === 'PATCH' && /cases\/11$/.test(u)) return { status: 200, body: { case: CASE, datasetVersion: 4 } }
      if (m === 'DELETE' && /cases\/11$/.test(u)) return { status: 200, body: { datasetVersion: 5 } }
      return undefined
    }
    setup()
    await user.click(await screen.findByRole('button', { name: 'Edit case 11' }))
    let dialog = screen.getByRole('dialog')
    const input = within(dialog).getByLabelText('Input')
    await user.clear(input)
    await user.type(input, 'Can I send it back?')
    await user.click(within(dialog).getByRole('button', { name: 'Save case' }))
    await waitFor(() => expect(posted('PATCH', /cases\/11$/)?.body).toMatchObject({ input: 'Can I send it back?', tags: ['refund'] }))
    await user.click(await screen.findByRole('button', { name: 'Delete case 11' }))
    dialog = screen.getByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Delete case' }))
    await waitFor(() => expect(posted('DELETE', /cases\/11$/)).toBeTruthy())
  })

  it('adds a regex grader with a required mark and weight', async () => {
    const user = userEvent.setup()
    handler = (m, u) => (m === 'PUT' && /graders\/has-id$/.test(u) ? { status: 201, body: GRADER } : undefined)
    setup()
    await user.click(await screen.findByRole('button', { name: 'Add a grader' }))
    const dialog = screen.getByRole('dialog')
    await user.type(within(dialog).getByLabelText('Name'), 'has-id')
    await user.selectOptions(within(dialog).getByLabelText('Kind'), 'regex')
    await user.type(within(dialog).getByLabelText('Pattern'), 'ID-[[0-9]+')
    const weight = within(dialog).getByLabelText('Weight')
    await user.clear(weight)
    await user.type(weight, '2')
    await user.click(within(dialog).getByLabelText(/Required/))
    await user.click(within(dialog).getByRole('button', { name: 'Save grader' }))
    await waitFor(() => expect(posted('PUT', /graders\/has-id$/)).toBeTruthy())
    expect(posted('PUT', /graders\/has-id$/)?.body).toEqual({ kind: 'regex', config: { pattern: 'ID-[0-9]+' }, weight: 2, required: true })
  })

  it('keeps Save disabled until the kind config is valid, and shows a server 400 or 422', async () => {
    const user = userEvent.setup()
    handler = (m, u) => (m === 'PUT' ? { status: 422, body: { error: 'the pattern is not valid RE2' } } : undefined)
    setup()
    await user.click(await screen.findByRole('button', { name: 'Add a grader' }))
    const dialog = screen.getByRole('dialog')
    await user.type(within(dialog).getByLabelText('Name'), 'bad')
    await user.selectOptions(within(dialog).getByLabelText('Kind'), 'max_cost')
    const save = within(dialog).getByRole('button', { name: 'Save grader' })
    expect(save).toBeDisabled()
    await user.type(within(dialog).getByLabelText(/Most the run may cost/), '0.5')
    expect(save).toBeEnabled()
    await user.click(save)
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('not valid RE2')
  })

  it('edits and deletes a grader', async () => {
    const user = userEvent.setup()
    handler = (m, u) => {
      if (m === 'PUT' && /graders\/polite$/.test(u)) return { status: 200, body: GRADER }
      if (m === 'DELETE' && /graders\/polite$/.test(u)) return { status: 204 }
      return undefined
    }
    setup()
    await user.click(await screen.findByRole('button', { name: 'Edit grader polite' }))
    let dialog = screen.getByRole('dialog')
    expect(within(dialog).getByLabelText('Name')).toBeDisabled()
    expect(within(dialog).getByLabelText('Text')).toHaveValue('please')
    await user.click(within(dialog).getByRole('button', { name: 'Save grader' }))
    await waitFor(() => expect(posted('PUT', /graders\/polite$/)?.body).toMatchObject({ kind: 'contains', config: { value: 'please' }, required: true }))
    await user.click(await screen.findByRole('button', { name: 'Delete grader polite' }))
    dialog = screen.getByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Delete grader' }))
    await waitFor(() => expect(posted('DELETE', /graders\/polite$/)).toBeTruthy())
  })

  it('tries the graders on pasted output and shows pass, score and detail', async () => {
    const user = userEvent.setup()
    handler = (m, u) => (m === 'POST' && /grade$/.test(u)
      ? { status: 201, body: { id: 1, datasetId: 7, output: 'x', toolCalls: [], costUsd: 0, latencyMs: 0, casePass: false, createdAt: '', grades: [
        { graderId: 21, grader: 'polite', score: 0, pass: false, detail: 'output does not contain "please"' },
        { graderId: 22, grader: 'same', score: 0, pass: false, notGraded: true, detail: 'the case has no reference' },
      ] } }
      : undefined)
    setup()
    await user.type(await screen.findByLabelText('Agent output'), 'Here you go')
    await user.type(screen.getByLabelText('Tools called (optional)'), 'search, refund')
    await waitFor(() => expect(screen.getByRole('button', { name: 'Grade this output' })).toBeEnabled())
    await user.click(screen.getByRole('button', { name: 'Grade this output' }))
    const result = await screen.findByLabelText('Grading result')
    expect(within(result).getByText('Fails')).toBeInTheDocument()
    expect(within(result).getByText('output does not contain "please"')).toBeInTheDocument()
    expect(within(result).getByText('Not graded')).toBeInTheDocument()
    expect(posted('POST', /grade$/)?.body).toMatchObject({ output: 'Here you go', toolCalls: ['search', 'refund'] })
  })

  it('shows 409 when there are no graders to try, and 403 for grading', async () => {
    const user = userEvent.setup()
    handler = (m, u) => (m === 'POST' && /grade$/.test(u) ? { status: 403, body: { error: 'role runner required' } } : undefined)
    setup()
    await user.type(await screen.findByLabelText('Agent output'), 'x')
    await waitFor(() => expect(screen.getByRole('button', { name: 'Grade this output' })).toBeEnabled())
    await user.click(screen.getByRole('button', { name: 'Grade this output' }))
    expect(await screen.findByText(/You are not allowed to do this/)).toBeInTheDocument()
  })

  it('is read only for a viewer', async () => {
    mockGetMe.mockResolvedValue(VIEWER)
    setup()
    await screen.findByText('Can I return it?')
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Add a case' })).not.toBeInTheDocument())
    expect(screen.queryByRole('button', { name: 'Add a grader' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Edit case 11' })).not.toBeInTheDocument()
    expect(screen.getByText(/needs the builder role/)).toBeInTheDocument()
    expect(screen.getByText('Grading needs the runner role.')).toBeInTheDocument()
  })
})
