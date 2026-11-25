import { describe, expect, it, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import type { TraceEntry } from '../api/sse'

type StreamEvent = TraceEntry['event']

const mockGetRun = vi.fn()
const mockArchived = vi.fn()
let live: StreamEvent[] = []
vi.mock('../api/sse', async () => ({
  ...(await vi.importActual<typeof import('../api/sse')>('../api/sse')),
  getRun: () => mockGetRun(),
  getRunHistoryDetail: () => mockArchived(),
  subscribeToRunStream: (_run: string, onEvent: (e: StreamEvent) => void) => {
    live.forEach(onEvent)
    return () => {}
  },
}))
// Which entries reach the trace is what matters here; how it draws them has its own tests.
vi.mock('./TraceAccordion', () => ({
  TraceAccordion: ({ entries }: { entries: TraceEntry[] }) => <ul>{entries.map((e) => <li key={e.id}>entry:{e.event.type}</li>)}</ul>,
}))
vi.mock('./FeedbackThumbs', () => ({ FeedbackThumbs: () => null, useRunFeedback: () => undefined }))

import { RunView } from './RunView'

const entry = (id: number, type: string) => ({ id, ts: '2026-11-25T10:00:00Z', event: { type } }) as unknown as TraceEntry
const routing = { type: 'modelSelected', model: 'm', provider: 'p', reason: 'rule', strategy: 's' } as unknown as StreamEvent

const openTrace = async () => {
  render(<MemoryRouter><RunView runId="r1" namespace="ns" /></MemoryRouter>)
  await userEvent.click(await screen.findByRole('tab', { name: 'Execution Trace' }))
}

beforeEach(() => {
  vi.clearAllMocks()
  live = [routing]
  mockGetRun.mockResolvedValue({ name: 'r1', namespace: 'ns', agentRef: 'a', phase: 'Succeeded', input: 'hi', spendUSD: '', routingDecisions: [] })
})

describe('RunView execution trace', () => {
  it('shows the archived trace when the live stream has only routing left', async () => {
    mockArchived.mockResolvedValue({ traceEntries: [entry(0, 'tool_call'), entry(1, 'tool_result'), entry(2, 'modelSelected')] })
    await openTrace()
    expect(await screen.findByText('entry:tool_call')).toBeInTheDocument()
    expect(screen.getByText('entry:tool_result')).toBeInTheDocument()
    expect(screen.getAllByText('entry:modelSelected')).toHaveLength(1)
  })

  it('keeps the live trace when it has the run’s activity', async () => {
    live = [routing, { type: 'tool_call' } as unknown as StreamEvent]
    await openTrace()
    expect(screen.getByText('entry:tool_call')).toBeInTheDocument()
    expect(mockArchived).not.toHaveBeenCalled()
  })

  it('says so when no detailed trace is left anywhere', async () => {
    mockArchived.mockResolvedValue({ traceEntries: [entry(0, 'modelSelected')] })
    await openTrace()
    expect(await screen.findByRole('note')).toHaveTextContent(/no longer has a detailed trace/)
    expect(screen.getByText('entry:modelSelected')).toBeInTheDocument()
  })

  it('says so when the run is not in the archive', async () => {
    mockArchived.mockRejectedValue(new Error('404'))
    await openTrace()
    expect(await screen.findByRole('note')).toHaveTextContent(/no longer has a detailed trace/)
  })
})
