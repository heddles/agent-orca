import { describe, expect, it, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import type { MyRun, MyRuns } from '../api/catalog'

const mockMyRuns = vi.fn()
const mockAnswer = vi.fn()
const mockApprovals = vi.fn()
vi.mock('../api/catalog', async () => {
  const actual = await vi.importActual<typeof import('../api/catalog')>('../api/catalog')
  return { ...actual, getMyRuns: (...a: unknown[]) => mockMyRuns(...a) }
})
vi.mock('../api/sse', async () => {
  const actual = await vi.importActual<typeof import('../api/sse')>('../api/sse')
  return {
    ...actual,
    answerClarification: (...a: unknown[]) => mockAnswer(...a),
    listApprovals: (...a: unknown[]) => mockApprovals(...a),
    getRunFeedback: vi.fn(() => Promise.resolve([])),
    submitRunFeedback: vi.fn(() => Promise.resolve()),
  }
})

import { MyWork } from './MyWork'

function run(over: Partial<MyRun>): MyRun {
  return {
    name: 'run-1', namespace: 'team-a', agentRef: 'researcher', phase: 'Succeeded', spendUSD: '0.12',
    input: 'Look into whales', createdAt: new Date().toISOString(), ...over,
  }
}

function runs(scope: MyRuns['scope'], list: MyRun[]): MyRuns {
  return { scope, userId: 'ada', runs: list }
}

const renderHome = () => render(<MemoryRouter><MyWork /></MemoryRouter>)

beforeEach(() => {
  vi.clearAllMocks()
  mockApprovals.mockResolvedValue([])
})

describe('MyWork', () => {
  it('lists recent runs with plain-language status and says they are the user’s own', async () => {
    mockMyRuns.mockResolvedValue(runs('user', [
      run({ name: 'a', phase: 'Running', output: undefined }),
      run({ name: 'b', phase: 'Succeeded', output: 'Whales are large.' }),
    ]))
    renderHome()
    const list = await screen.findByRole('list', { name: 'Recent runs' })
    expect(within(list).getAllByRole('listitem')).toHaveLength(2)
    expect(screen.getByText('Working')).toBeInTheDocument()
    expect(screen.getByText('Done')).toBeInTheDocument()
    expect(screen.getByText('Runs you started.')).toBeInTheDocument()
  })

  it('says when the runs are the team’s rather than the user’s', async () => {
    mockMyRuns.mockResolvedValue(runs('tenant', [run({})]))
    renderHome()
    expect(await screen.findByText(/Showing your team’s recent runs/)).toBeInTheDocument()
  })

  it('shows the output of a finished run on request, with feedback thumbs', async () => {
    mockMyRuns.mockResolvedValue(runs('user', [run({ output: 'Whales are large.' })]))
    renderHome()
    const toggle = await screen.findByRole('button', { name: 'Show result' })
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    await userEvent.click(toggle)
    expect(await screen.findByText('Whales are large.')).toBeInTheDocument()
    expect(await screen.findByRole('button', { name: /thumbs up|good/i })).toBeInTheDocument()
  })

  it('answers an agent’s question inline', async () => {
    mockAnswer.mockResolvedValue({ runName: 'run-2' })
    mockMyRuns.mockResolvedValue(runs('user', [
      run({ name: 'ask', phase: 'WaitingForInput', clarifyQuestion: 'Which quarter?' }),
    ]))
    renderHome()
    const form = await screen.findByRole('form', { name: 'Question from researcher' })
    expect(within(form).getByText('Which quarter?')).toBeInTheDocument()
    await userEvent.click(within(form).getByRole('button', { name: 'Send answer' }))
    expect(await within(form).findByRole('alert')).toHaveTextContent('Type an answer first.')
    await userEvent.type(within(form).getByLabelText('Your answer to researcher'), 'Q3')
    await userEvent.click(within(form).getByRole('button', { name: 'Send answer' }))
    await waitFor(() => expect(mockAnswer).toHaveBeenCalledWith('ask', 'Q3', 'team-a'))
  })

  it('offers Connect when the question is the agent asking for the user’s account', async () => {
    mockMyRuns.mockResolvedValue(runs('user', [
      run({ name: 'ask', phase: 'WaitingForInput', clarifyQuestion: 'This agent needs your github account. Connect it under Connections, then answer here to continue.' }),
      run({ name: 'plain', agentRef: 'other', phase: 'WaitingForInput', clarifyQuestion: 'Which quarter?' }),
    ]))
    renderHome()
    const form = await screen.findByRole('form', { name: 'Question from researcher' })
    expect(within(form).getByRole('link', { name: 'Connect' })).toHaveAttribute('href', '/connections')
    const other = await screen.findByRole('form', { name: 'Question from other' })
    expect(within(other).queryByRole('link', { name: 'Connect' })).toBeNull()
  })

  it('shows pending approvals inline and no "nothing waiting" note', async () => {
    mockMyRuns.mockResolvedValue(runs('user', [run({})]))
    mockApprovals.mockResolvedValue([{
      id: 'ap1', namespace: 'team-a', runName: 'run-1', kind: 'egress', summary: 'curl wants to reach example.com',
      approverGroups: [], status: 'pending', requestedAt: new Date().toISOString(),
      expiresAt: new Date(Date.now() + 600000).toISOString(),
    }])
    renderHome()
    expect(await screen.findByText('curl wants to reach example.com')).toBeInTheDocument()
    expect(screen.queryByText(/Nothing is waiting for you/)).not.toBeInTheDocument()
  })

  it('tells a new user what to do next', async () => {
    mockMyRuns.mockResolvedValue(runs('tenant', []))
    renderHome()
    expect(await screen.findByText('You have not run anything yet')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Browse agents' })).toHaveAttribute('href', '/agents')
    expect(await screen.findByText(/Nothing is waiting for you/)).toBeInTheDocument()
  })

  it('shows a load failure', async () => {
    mockMyRuns.mockImplementation(() => Promise.reject(new Error('HTTP 500')))
    renderHome()
    expect(await screen.findByRole('alert')).toHaveTextContent('HTTP 500')
  })
})
