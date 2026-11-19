import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'

const mockGetMe = vi.fn()
vi.mock('../api/fleet', () => ({ getMe: () => mockGetMe() }))

import { EvalExperiments, armFromDraft } from './EvalExperiments'
import { EvalExperimentDetail } from './EvalExperimentDetail'
import { EvalReview } from './EvalReview'

const me = (roles: string[]) => ({ userId: 'u', groups: [], tenant: { id: 't', name: 'T' }, workspaces: [{ name: 'A', namespace: 'team-a', primary: true }], roles })
const BUILDER = me(['viewer', 'runner', 'builder'])
const VIEWER = me(['viewer'])
const RUNNER = me(['viewer', 'runner'])

const ARMS = [{ label: 'arm1', agent: 'helper' }, { label: 'arm2', agent: 'helper', revision: 2 }]
const EXP = {
  id: 9, namespace: 'team-a', datasetId: 7, datasetVersion: 3, name: 'v2 check', status: 'running', arms: ARMS, repeats: 1,
  budgetUsd: 5, spentUsd: 1.25, resultsDone: 4, resultsTotal: 10, createdAt: '2026-01-01T00:00:00Z',
}
const ARM_SUM = (label: string, passed: number, rate: number) => ({
  label, cases: 10, skipped: 0, passed, excludedError: 0, excludedPending: 0, passRate: rate, passRateCi: { lo: rate - 0.2, hi: Math.min(1, rate + 0.2) },
  meanScore: rate, failureRate: 0, costTotalUsd: 0.5, costMedianUsd: 0.05, costIncomplete: false, hasLatency: true, latencyP50Ms: 1200, latencyP95Ms: 3000,
  repeatedCases: 0, flakyCases: 0, stability: 1,
})
const SUMMARY = {
  experimentId: 9, status: 'running', live: true, partial: true,
  summary: {
    graders: [{ name: 'polite', graded: 8, passed: 6 }],
    comparison: {
      baseline: 'arm1',
      arms: [ARM_SUM('arm1', 6, 0.6), ARM_SUM('arm2', 8, 0.8)],
      excludedError: 1, excludedPending: 2,
      comparisons: [{
        baseline: 'arm1', candidate: 'arm2', pairs: 10, skipped: 0,
        passRate: { counts: { bothPass: 5, improved: 3, regressed: 1, bothFail: 1 }, n: 10, baselineRate: 0.6, candidateRate: 0.8, diff: 0.2, ci: { lo: -0.05, hi: 0.4 }, mcnemar: { improved: 3, regressed: 1, exact: true, statistic: 0, p: 0.625 } },
        cost: { n: 10, mean: -0.01, hasCi: true, ci: { lo: -0.02, hi: 0 } },
        latency: { n: 10, mean: 250, hasCi: false, ci: { lo: 0, hi: 0 } },
        verdict: { verdict: 'no clear difference', conclusive: false, nonInferior: true, margin: 0.05, minCases: 30, reason: 'The interval includes zero.' },
        regressedCases: [4], improvedCases: [1, 2, 3], skippedCases: [],
      }],
    },
  },
}
const RESULT = {
  id: 1, datasetId: 7, caseId: 11, arm: 'arm2', repeat: 0, phase: 'Succeeded', output: '<b>Sure, please</b>', toolCalls: ['search'], costUsd: 0.01, latencyMs: 900,
  casePass: true, createdAt: '', grades: [{ graderId: 2, grader: 'polite', score: 1, pass: true, detail: 'found "please"', status: 'ok' }],
}

type Reply = { status: number; body?: unknown }
let routes: Record<string, Reply | ((method: string, body: unknown) => Reply)>
const calls: { method: string; url: string; body: unknown }[] = []

beforeEach(() => {
  calls.length = 0
  mockGetMe.mockResolvedValue(BUILDER)
  routes = {}
  vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET'
    const body = init?.body ? JSON.parse(String(init.body)) : undefined
    calls.push({ method, url, body })
    const key = Object.keys(routes).find((k) => {
      const [m, re] = k.split(' ')
      return m === method && new RegExp(re).test(url)
    })
    const r = key ? (typeof routes[key] === 'function' ? (routes[key] as (m: string, b: unknown) => Reply)(method, body) : (routes[key] as Reply)) : { status: 404, body: { error: 'not found' } }
    return Promise.resolve(new Response(r.body === undefined ? null : JSON.stringify(r.body), { status: r.status }))
  }))
})
afterEach(() => vi.unstubAllGlobals())

const wizardRoutes = () => {
  routes['GET /api/evals/datasets$'] = { status: 200, body: { datasets: [{ id: 7, namespace: 'team-a', name: 'refunds', version: 3, caseCount: 12, agent: 'helper' }] } }
  routes['GET /api/catalog'] = { status: 200, body: [{ name: 'helper', namespace: 'team-a', template: false }] }
  routes['GET /api/modelproviders'] = { status: 200, body: [{ name: 'cheap', namespace: 'team-a', litellmModel: 'x/mini' }] }
  routes['GET /api/resources/agents/team-a/helper/revisions'] = { status: 200, body: { revisions: [{ revision: 2, action: 'update', userId: 'u', createdAt: '', summary: 'shorter prompt' }], next: '' } }
}

describe('armFromDraft', () => {
  it('sends only what the kind needs', () => {
    expect(armFromDraft({ label: '', kind: 'live', agent: 'a', revision: '3', modelProvider: 'm' })).toEqual({ agent: 'a' })
    expect(armFromDraft({ label: 'x', kind: 'revision', agent: 'a', revision: '3', modelProvider: 'm' })).toEqual({ agent: 'a', label: 'x', revision: 3 })
    expect(armFromDraft({ label: '', kind: 'model', agent: 'a', revision: '3', modelProvider: 'm' })).toEqual({ agent: 'a', modelProvider: 'm' })
  })
})

describe('EvalExperiments', () => {
  const setup = () => render(<MemoryRouter><EvalExperiments /></MemoryRouter>)

  it('lists experiments with status, progress and a link', async () => {
    routes['GET /api/evals/experiments'] = { status: 200, body: { experiments: [EXP] } }
    setup()
    expect(await screen.findByRole('link', { name: 'v2 check' })).toHaveAttribute('href', '/evaluate/experiments/9')
    expect(screen.getByText('Running')).toBeInTheDocument()
    expect(screen.getByText('4 of 10')).toBeInTheDocument()
    expect(screen.getByText('$1.25 of $5.00')).toBeInTheDocument()
  })

  it('explains an empty list and hides the button from a viewer', async () => {
    mockGetMe.mockResolvedValue(VIEWER)
    routes['GET /api/evals/experiments'] = { status: 200, body: { experiments: [] } }
    setup()
    expect(await screen.findByText('No experiments yet')).toBeInTheDocument()
    await waitFor(() => expect(screen.queryByRole('button', { name: 'New experiment' })).not.toBeInTheDocument())
  })

  it('says why on 503 and 403', async () => {
    routes['GET /api/evals/experiments'] = { status: 503, body: { error: 'x' } }
    const { unmount } = setup()
    expect(await screen.findByRole('alert')).toHaveTextContent(/PostgreSQL/)
    unmount()
    routes['GET /api/evals/experiments'] = { status: 403, body: { error: 'forbidden' } }
    setup()
    expect(await screen.findByRole('alert')).toHaveTextContent(/not allowed/)
  })

  it('walks the wizard and posts the experiment', async () => {
    const user = userEvent.setup()
    routes['GET /api/evals/experiments'] = { status: 200, body: { experiments: [] } }
    wizardRoutes()
    routes['POST /api/evals/experiments'] = { status: 201, body: { ...EXP, status: 'queued' } }
    setup()
    await user.click(await screen.findByRole('button', { name: 'New experiment' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('button', { name: 'Next' })).toBeDisabled()
    await waitFor(() => expect(within(dialog).getByRole('option', { name: /refunds/ })).toBeInTheDocument())
    await user.selectOptions(within(dialog).getByLabelText('Dataset'), '7')
    await user.click(within(dialog).getByRole('button', { name: 'Next' }))

    // The dataset's agent is filled in for both versions.
    await waitFor(() => expect(within(dialog).getByLabelText('Version 1 agent')).toHaveValue('helper'))
    await user.selectOptions(within(dialog).getByLabelText('Version 2 runs'), 'revision')
    await waitFor(() => expect(within(dialog).getByRole('option', { name: /Revision 2/ })).toBeInTheDocument())
    await user.selectOptions(within(dialog).getByLabelText('Version 2 saved version'), '2')
    await user.click(within(dialog).getByRole('button', { name: 'Add a version' }))
    await user.selectOptions(within(dialog).getByLabelText('Version 3 agent'), 'helper')
    await waitFor(() => expect(within(dialog).getByRole('option', { name: /cheap/ })).toBeInTheDocument())
    await user.selectOptions(within(dialog).getByLabelText('Version 3 model'), 'cheap')
    await user.click(within(dialog).getByRole('button', { name: 'Next' }))

    await user.clear(within(dialog).getByLabelText('Repeats'))
    await user.type(within(dialog).getByLabelText('Repeats'), '3')
    await user.click(within(dialog).getByRole('button', { name: 'Start experiment' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'POST')).toBeTruthy())
    expect(calls.find((c) => c.method === 'POST')!.body).toMatchObject({
      datasetId: 7, repeats: 3, budgetUsd: 5, concurrency: 4,
      arms: [{ agent: 'helper' }, { agent: 'helper', revision: 2 }, { agent: 'helper', modelProvider: 'cheap' }],
    })
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })

  it('validates limits before posting', async () => {
    const user = userEvent.setup()
    routes['GET /api/evals/experiments'] = { status: 200, body: { experiments: [] } }
    wizardRoutes()
    setup()
    await user.click(await screen.findByRole('button', { name: 'New experiment' }))
    const dialog = await screen.findByRole('dialog')
    await waitFor(() => expect(within(dialog).getByRole('option', { name: /refunds/ })).toBeInTheDocument())
    await user.selectOptions(within(dialog).getByLabelText('Dataset'), '7')
    await user.click(within(dialog).getByRole('button', { name: 'Next' }))
    await user.selectOptions(within(dialog).getByLabelText('Version 2 runs'), 'model')
    await user.selectOptions(within(dialog).getByLabelText('Version 2 model'), 'cheap')
    await user.click(within(dialog).getByRole('button', { name: 'Next' }))
    await user.clear(within(dialog).getByLabelText('Concurrency'))
    await user.type(within(dialog).getByLabelText('Concurrency'), '40')
    expect(within(dialog).getByText(/Concurrency is a whole number from 1 to 16/)).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: 'Start experiment' })).toBeDisabled()
  })

  it('shows the 422 missing payload readably', async () => {
    const user = userEvent.setup()
    routes['GET /api/evals/experiments'] = { status: 200, body: { experiments: [] } }
    wizardRoutes()
    routes['POST /api/evals/experiments'] = {
      status: 422,
      body: { error: 'missing', missing: [{ arm: 'arm2', kind: 'ModelProvider', name: 'gpt-big', namespace: 'team-a', alternatives: ['cheap'] }, { arm: 'arm1', kind: 'Tool', name: 'search', namespace: 'team-a', alternatives: [] }] },
    }
    setup()
    await user.click(await screen.findByRole('button', { name: 'New experiment' }))
    const dialog = await screen.findByRole('dialog')
    await waitFor(() => expect(within(dialog).getByRole('option', { name: /refunds/ })).toBeInTheDocument())
    await user.selectOptions(within(dialog).getByLabelText('Dataset'), '7')
    await user.click(within(dialog).getByRole('button', { name: 'Next' }))
    await user.selectOptions(within(dialog).getByLabelText('Version 2 runs'), 'model')
    await user.selectOptions(within(dialog).getByLabelText('Version 2 model'), 'cheap')
    await user.click(within(dialog).getByRole('button', { name: 'Next' }))
    await user.click(within(dialog).getByRole('button', { name: 'Start experiment' }))
    const group = await within(dialog).findByRole('group', { name: 'Missing from the workspace' })
    expect(group).toHaveTextContent('Version arm2 needs ModelProvider gpt-big in team-a. Available instead: cheap.')
    expect(group).toHaveTextContent('Nothing similar is available')
  })

  it('shows the 409 limit message', async () => {
    const user = userEvent.setup()
    routes['GET /api/evals/experiments'] = { status: 200, body: { experiments: [] } }
    wizardRoutes()
    routes['POST /api/evals/experiments'] = { status: 409, body: { error: 'too many experiments are queued or running' } }
    setup()
    await user.click(await screen.findByRole('button', { name: 'New experiment' }))
    const dialog = await screen.findByRole('dialog')
    await waitFor(() => expect(within(dialog).getByRole('option', { name: /refunds/ })).toBeInTheDocument())
    await user.selectOptions(within(dialog).getByLabelText('Dataset'), '7')
    await user.click(within(dialog).getByRole('button', { name: 'Next' }))
    await user.selectOptions(within(dialog).getByLabelText('Version 2 runs'), 'model')
    await user.selectOptions(within(dialog).getByLabelText('Version 2 model'), 'cheap')
    await user.click(within(dialog).getByRole('button', { name: 'Next' }))
    await user.click(within(dialog).getByRole('button', { name: 'Start experiment' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('too many experiments are queued or running')
  })
})

describe('EvalExperimentDetail', () => {
  const setup = () => render(<MemoryRouter><EvalExperimentDetail id={9} /></MemoryRouter>)
  const detailRoutes = () => {
    routes['GET /api/evals/experiments/9$'] = { status: 200, body: EXP }
    routes['GET /api/evals/experiments/9/summary'] = { status: 200, body: SUMMARY }
    routes['GET /api/evals/experiments/9/results'] = { status: 200, body: { results: [RESULT] } }
  }

  it('shows progress, budget, per-arm rates with intervals, the verdict and the partial flag', async () => {
    detailRoutes()
    setup()
    expect(await screen.findByRole('heading', { name: 'v2 check' })).toBeInTheDocument()
    expect(screen.getByText('Results: 4 of 10')).toBeInTheDocument()
    expect(screen.getByText('Budget used: $1.25 of $5.00')).toBeInTheDocument()
    const table = await screen.findByRole('table', { name: 'Pass rate by version' })
    expect(within(table).getByText('60%')).toBeInTheDocument()
    expect(within(table).getByText('40% to 80%')).toBeInTheDocument()
    expect(within(table).getByText('80%')).toBeInTheDocument()
    const card = screen.getByRole('region', { name: 'arm2 against arm1' })
    expect(within(card).getByText('No clear difference')).toBeInTheDocument()
    expect(card).toHaveTextContent('+20 pts')
    expect(card).toHaveTextContent('p = 0.625')
    expect(card).toHaveTextContent('3 improved, 1 regressed')
    expect(card).toHaveTextContent('-$0.01')
    expect(card).toHaveTextContent('+250 ms')
    expect(screen.getByText(/Partial: these figures cover only the cases finished so far/)).toBeInTheDocument()
    expect(screen.getByText(/1 result a check could not grade/)).toHaveTextContent('2 waiting for a person to review')
  })

  it('shows each result with its answer as text and its grade details', async () => {
    const user = userEvent.setup()
    detailRoutes()
    setup()
    const table = await screen.findByRole('table', { name: 'Results by case' })
    await user.click(within(table).getByText(/Show answer and 1 check/))
    expect(within(table).getByText('<b>Sure, please</b>')).toBeInTheDocument()
    expect(within(table).getByText(/found "please"/)).toBeInTheDocument()
    expect(within(table).getByText(/Tools called: search/)).toBeInTheDocument()
  })

  it('cancels a running experiment after confirmation', async () => {
    const user = userEvent.setup()
    detailRoutes()
    routes['POST /api/evals/experiments/9/cancel'] = { status: 200, body: { ...EXP, cancelRequested: true } }
    setup()
    await user.click(await screen.findByRole('button', { name: 'Cancel experiment' }))
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Cancel experiment' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST' && c.url.endsWith('/cancel'))).toBe(true))
  })

  it('shows a 409 on cancel in words, and hides cancel from a viewer and for ended experiments', async () => {
    const user = userEvent.setup()
    detailRoutes()
    routes['POST /api/evals/experiments/9/cancel'] = { status: 409, body: { error: 'ended' } }
    const { unmount } = setup()
    await user.click(await screen.findByRole('button', { name: 'Cancel experiment' }))
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Cancel experiment' }))
    expect(await screen.findByText(/already ended/)).toBeInTheDocument()
    unmount()

    mockGetMe.mockResolvedValue(VIEWER)
    setup()
    await screen.findByRole('heading', { name: 'v2 check' })
    expect(screen.queryByRole('button', { name: 'Cancel experiment' })).not.toBeInTheDocument()
  })

  it('shows a finished experiment without cancel, and a failure reason', async () => {
    routes['GET /api/evals/experiments/9$'] = { status: 200, body: { ...EXP, status: 'failed', error: 'the dataset has no graders' } }
    routes['GET /api/evals/experiments/9/summary'] = { status: 409, body: { error: 'no summary' } }
    routes['GET /api/evals/experiments/9/results'] = { status: 200, body: { results: [] } }
    setup()
    expect(await screen.findByText(/The experiment failed: the dataset has no graders/)).toBeInTheDocument()
    expect(await screen.findByText(/There is no comparison yet/)).toBeInTheDocument()
    expect(screen.getByText('This experiment has no results.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Cancel experiment' })).not.toBeInTheDocument()
  })

  it('says when the experiment is not found', async () => {
    routes['GET /api/evals/experiments/9$'] = { status: 404, body: { error: 'nope' } }
    routes['GET /api/evals/experiments/9/summary'] = { status: 404, body: { error: 'nope' } }
    setup()
    expect(await screen.findByText('Experiment not found')).toBeInTheDocument()
  })

  it('links to the review while the experiment waits for it', async () => {
    detailRoutes()
    routes['GET /api/evals/experiments/9$'] = { status: 200, body: { ...EXP, status: 'awaiting_review' } }
    setup()
    expect(await screen.findByRole('link', { name: 'Review answers' })).toHaveAttribute('href', '/evaluate/experiments/9/review')
  })
})

describe('EvalReview', () => {
  const setup = () => render(<MemoryRouter><EvalReview id={9} /></MemoryRouter>)
  const ITEM = { id: 31, experimentId: 9, caseId: 11, grader: 'tone', scale: 'passfail', instructions: 'Is it polite?', input: 'Can I return it?', reference: 'Yes', output: 'Sure thing', status: 'pending' }
  const reviewRoutes = (item: unknown = ITEM) => {
    routes['GET /api/evals/experiments/9$'] = { status: 200, body: { ...EXP, status: 'awaiting_review' } }
    routes['GET /api/evals/experiments/9/review'] = { status: 200, body: { status: 'awaiting_review', progress: { pending: 1, done: 2, closed: 0 }, items: item ? [item] : [] } }
    routes['GET /api/evals/datasets/7/graders'] = { status: 200, body: { graders: [] } }
  }

  it('shows the item blind and submits a pass/fail label', async () => {
    const user = userEvent.setup()
    reviewRoutes()
    routes['POST /api/evals/review/31'] = { status: 200, body: { ...ITEM, status: 'done' } }
    setup()
    const card = await screen.findByRole('region', { name: 'Answer 3' })
    expect(card).toHaveTextContent('Can I return it?')
    expect(card).toHaveTextContent('Sure thing')
    expect(card).toHaveTextContent('Is it polite?')
    // Nothing about the arm, run, cost or tools is shown, and no arm field is in the queue request.
    expect(document.body.textContent).not.toMatch(/arm1|arm2|revision|cost|tools called/i)
    await user.type(within(card).getByLabelText(/Comment/), 'fine')
    await user.click(within(card).getByRole('button', { name: 'Pass' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'POST')).toBeTruthy())
    expect(calls.find((c) => c.method === 'POST')!.body).toEqual({ pass: true, comment: 'fine' })
  })

  it('submits stars for a 1 to 5 grader', async () => {
    const user = userEvent.setup()
    reviewRoutes({ ...ITEM, scale: '1to5' })
    routes['POST /api/evals/review/31'] = { status: 200, body: ITEM }
    setup()
    const card = await screen.findByRole('region', { name: 'Answer 3' })
    expect(within(card).getByRole('button', { name: 'Submit label' })).toBeDisabled()
    await user.click(within(card).getByRole('radio', { name: '4' }))
    await user.click(within(card).getByRole('button', { name: 'Submit label' }))
    await waitFor(() => expect(calls.find((c) => c.method === 'POST')!.body).toEqual({ stars: 4 }))
  })

  it('explains 409 and 403 on labelling', async () => {
    const user = userEvent.setup()
    reviewRoutes()
    routes['POST /api/evals/review/31'] = { status: 409, body: { error: 'labelled' } }
    const { unmount } = setup()
    await user.click(await screen.findByRole('button', { name: 'Fail' }))
    expect(await screen.findByText(/Someone already labelled this answer/)).toBeInTheDocument()
    unmount()
    routes['POST /api/evals/review/31'] = { status: 403, body: { error: 'you started this experiment' } }
    setup()
    await user.click(await screen.findByRole('button', { name: 'Fail' }))
    expect(await screen.findByText(/You cannot label this/)).toHaveTextContent('you started this experiment')
  })

  it('lets a viewer read but not label', async () => {
    mockGetMe.mockResolvedValue(VIEWER)
    reviewRoutes()
    setup()
    await screen.findByRole('region', { name: 'Answer 3' })
    await waitFor(() => expect(screen.getByText(/Labelling needs the runner role/)).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: 'Pass' })).not.toBeInTheDocument()
  })

  it('shows an empty queue', async () => {
    mockGetMe.mockResolvedValue(RUNNER)
    reviewRoutes(null)
    routes['GET /api/evals/experiments/9/review'] = { status: 200, body: { status: 'complete', progress: { pending: 0, done: 4, closed: 0 }, items: [] } }
    setup()
    expect(await screen.findByText('Nothing left to review')).toBeInTheDocument()
  })

  it('closes the review for a builder only', async () => {
    const user = userEvent.setup()
    reviewRoutes()
    routes['POST /api/evals/experiments/9/review/close'] = { status: 200, body: { closed: 1 } }
    const { unmount } = setup()
    await user.click(await screen.findByRole('button', { name: 'Close review' }))
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Close review' }))
    await waitFor(() => expect(calls.some((c) => c.url.endsWith('/review/close'))).toBe(true))
    unmount()
    mockGetMe.mockResolvedValue(RUNNER)
    setup()
    await screen.findByRole('region', { name: 'Answer 3' })
    expect(screen.queryByRole('button', { name: 'Close review' })).not.toBeInTheDocument()
  })

  describe('calibration', () => {
    const graders = { graders: [{ id: 1, name: 'judge', kind: 'llm' }, { id: 2, name: 'tone', kind: 'human' }] }
    it('reports agreement and kappa with a trust warning', async () => {
      const user = userEvent.setup()
      reviewRoutes()
      routes['GET /api/evals/datasets/7/graders'] = { status: 200, body: graders }
      routes['GET /api/evals/datasets/7/calibration'] = {
        status: 200,
        body: {
          llmGrader: 'judge', humanGrader: 'tone', minPairs: 20, tooFewPairs: false,
          report: { pairs: 30, agreement: 0.7, kappa: 0.25, kappaDefined: true, trustworthy: false, confusion: { bothPass: 15, bothFail: 6, llmPassHumanFail: 6, llmFailHumanPass: 3 }, disagreements: [{ resultId: 1, caseId: 5, llmScore: 0.9, humanScore: 0, llmPass: true, humanPass: false }] },
        },
      }
      setup()
      await user.click(await screen.findByRole('button', { name: 'Check agreement' }))
      expect(await screen.findByText(/Do not trust this judge yet/)).toHaveTextContent('kappa 0.25')
      expect(screen.getByText('70%')).toBeInTheDocument()
      expect(calls.find((c) => c.url.includes('/calibration'))!.url).toContain('llm=judge&human=tone&experimentId=9')
    })
    it('says when there are too few answers', async () => {
      const user = userEvent.setup()
      reviewRoutes()
      routes['GET /api/evals/datasets/7/graders'] = { status: 200, body: graders }
      routes['GET /api/evals/datasets/7/calibration'] = { status: 200, body: { llmGrader: 'judge', humanGrader: 'tone', minPairs: 20, tooFewPairs: true, report: { pairs: 4 } } }
      setup()
      await user.click(await screen.findByRole('button', { name: 'Check agreement' }))
      expect(await screen.findByText(/at least 20 are needed, and there are 4/)).toBeInTheDocument()
    })
    it('explains a dataset without both kinds of check', async () => {
      reviewRoutes()
      setup()
      expect(await screen.findByText(/Calibration needs a dataset with a judge check/)).toBeInTheDocument()
    })
  })
})
