import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { STREAM_EVENT_TYPE } from '../contracts/events'
import type { RunHistoryDetail, TraceEntry } from '../api/sse'

// Mock only getRunHistoryDetail; preserve all other exports (types are erased
// at compile time, but re-exporting keeps the module graph intact).
const mockGetRunHistoryDetail = vi.fn()
vi.mock('../api/sse', async () => {
  const actual = await vi.importActual<typeof import('../api/sse')>('../api/sse')
  return {
    ...actual,
    getRunHistoryDetail: (...args: unknown[]) => mockGetRunHistoryDetail(...args),
  }
})

import { RunHistoryDetailView } from './RunHistoryDetailView'

const baseDetail: RunHistoryDetail = {
  name: 'run-1',
  namespace: 'default',
  agentRef: 'my-agent',
  input: 'hello world',
  output: 'result output',
  phase: 'Succeeded',
  spendUSD: '0.0123',
  restartCount: 0,
  startTime: '2026-01-15T12:00:00.000Z',
  completionTime: '2026-01-15T12:00:05.000Z',
  routingDecisions: [],
}

function makeTraceEntries(): TraceEntry[] {
  return [
    {
      id: 0,
      event: { type: STREAM_EVENT_TYPE.toolCall, name: 'search', arguments: '{"q":"test"}' },
      ts: '2026-01-15T12:00:01.000Z',
    },
    {
      id: 1,
      event: { type: STREAM_EVENT_TYPE.toolResult, name: 'search', result: 'found 3 results' },
      ts: '2026-01-15T12:00:02.000Z',
    },
    {
      id: 2,
      event: { type: 'token', content: 'The answer is' },
      ts: '2026-01-15T12:00:03.000Z',
    },
    {
      id: 3,
      event: { type: STREAM_EVENT_TYPE.finalOutput, output: 'The answer is 42' },
      ts: '2026-01-15T12:00:04.000Z',
    },
  ]
}

// openTraceAccordion expands the TraceAccordion header (the button whose
// accessible name includes the event-count badge, e.g. "4 events").
async function openTraceAccordion() {
  await userEvent.click(screen.getByRole('button', { name: /events/ }))
}

describe('RunHistoryDetailView', () => {
  beforeEach(() => {
    mockGetRunHistoryDetail.mockReset()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('renders full TraceAccordion when traceEntries are archived', async () => {
    mockGetRunHistoryDetail.mockResolvedValue({
      ...baseDetail,
      traceEntries: makeTraceEntries(),
    })

    render(<RunHistoryDetailView runId="run-1" namespace="default" onBack={vi.fn()} />)

    await waitFor(() => {
      expect(screen.getByText('my-agent')).toBeInTheDocument()
    })

    await userEvent.click(screen.getByRole('tab', { name: 'Execution Trace' }))

    // Expand the TraceAccordion to reveal its rows
    await openTraceAccordion()

    // All trace events are rendered, same as the live runs tab
    expect(await screen.findByText(/calls tool/)).toBeInTheDocument()
    expect(screen.getByText(/found 3 results/)).toBeInTheDocument()
    expect(screen.getByText('The answer is 42')).toBeInTheDocument()
    // Event count badge in the header
    expect(screen.getByText('4 events')).toBeInTheDocument()
  })

  it('hides Execution Trace tab when no trace data is available', async () => {
    mockGetRunHistoryDetail.mockResolvedValue({
      ...baseDetail,
      traceEntries: [],
    })

    render(<RunHistoryDetailView runId="run-1" namespace="default" onBack={vi.fn()} />)

    await waitFor(() => {
      expect(screen.getByText('my-agent')).toBeInTheDocument()
    })

    // No trace data → no Execution Trace tab shown
    expect(screen.queryByRole('tab', { name: 'Execution Trace' })).not.toBeInTheDocument()
  })

  it('falls back to ExecutionTrace timeline when traceEntries is undefined', async () => {
    // Old archived run with no trace_entries field but with routing decisions
    mockGetRunHistoryDetail.mockResolvedValue({
      ...baseDetail,
      traceEntries: undefined,
      routingDecisions: [
        { model: 'gpt-4o', provider: 'openai', strategy: 'rule', reason: 'default', confidence: '0.9' },
      ],
    })

    render(<RunHistoryDetailView runId="run-1" namespace="default" onBack={vi.fn()} />)

    await waitFor(() => {
      expect(screen.getByText('my-agent')).toBeInTheDocument()
    })

    const traceTab = screen.getByRole('tab', { name: 'Execution Trace' })
    expect(traceTab).toBeInTheDocument()
    await userEvent.click(traceTab)

    // The fallback ExecutionTrace timeline should show the routing event
    expect(await screen.findByText(/Run started/)).toBeInTheDocument()
    expect(screen.getByText(/Model routed/)).toBeInTheDocument()
    expect(screen.getAllByText(/gpt-4o/).length).toBeGreaterThan(0)
  })

  it('shows routing decisions alongside full trace in trace tab', async () => {
    mockGetRunHistoryDetail.mockResolvedValue({
      ...baseDetail,
      traceEntries: makeTraceEntries(),
      routingDecisions: [
        { model: 'gpt-4o', provider: 'openai', strategy: 'rule', reason: 'default', confidence: '0.95' },
      ],
    })

    render(<RunHistoryDetailView runId="run-1" namespace="default" onBack={vi.fn()} />)

    await waitFor(() => {
      expect(screen.getByText('my-agent')).toBeInTheDocument()
    })

    // The Model Router tab should still work for routing decisions
    await userEvent.click(screen.getByRole('tab', { name: 'Model Router' }))
    expect(await screen.findByText(/Model routing decisions/)).toBeInTheDocument()
    expect(screen.getAllByText(/gpt-4o/).length).toBeGreaterThan(0)
  })
})
