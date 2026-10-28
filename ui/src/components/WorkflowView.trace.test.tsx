import { beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { AgentWorkflowDetail, TraceEvent } from '../api/sse'

const detail: AgentWorkflowDetail = {
  name: 'wf',
  namespace: 'default',
  phase: 'Succeeded',
  stepCount: 1,
  totalSpendUSD: '0.01',
  steps: [{ name: 'plan', agentRef: 'planner', phase: 'Succeeded', agentRunRef: 'wf-plan', output: 'done' }],
}

let emit: (event: TraceEvent) => void = () => {}

vi.mock('../api/sse', async (orig) => ({
  ...(await orig<typeof import('../api/sse')>()),
  getWorkflow: vi.fn(async () => detail),
  getRun: vi.fn(async () => ({ childRunRefs: [] })),
  subscribeToRunStream: vi.fn((_run: string, onEvent: (event: TraceEvent) => void) => {
    emit = onEvent
    return () => {}
  }),
}))

import { WorkflowView } from './WorkflowView'

describe('WorkflowView trace', () => {
  beforeEach(() => vi.clearAllMocks())

  it('shows a block of model reasoning as one entry, not one per fragment', async () => {
    render(<WorkflowView name="wf" namespace="default" />)
    await userEvent.click(await screen.findByRole('button', { name: /Execution Trace|Live Trace/ }))
    act(() => {
      for (const part of ['The', ' user', ' wants', ' a', ' summary']) emit({ type: 'thought', content: part } as TraceEvent)
      emit({ type: 'toolCall', tool: 'read_file', args: {} } as unknown as TraceEvent)
      for (const part of ['Now', ' check']) emit({ type: 'thought', content: part } as TraceEvent)
    })
    // Two thinking blocks (one on each side of the tool call), not seven.
    expect(screen.getAllByText('thinking')).toHaveLength(2)
  })
})
