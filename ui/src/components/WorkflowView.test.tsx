import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { AgentWorkflowDetail } from '../api/sse'

const detail: AgentWorkflowDetail = {
  name: 'wf',
  namespace: 'default',
  phase: 'Running',
  stepCount: 3,
  totalSpendUSD: '0.10',
  loops: [{ id: 'adversary', gate: 'adversary', steps: ['ci', 'adversary'], until: 'steps["ci"].output.startsWith("CI: GREEN")', maxIterations: 4, pass: 2, state: 'repeating' }],
  steps: [
    { name: 'publish', agentRef: 'publisher', phase: 'Succeeded', output: 'PR opened' },
    {
      name: 'ci', agentRef: 'fixer', phase: 'Pending', iteration: 1, loop: 'adversary', staleOutput: true, output: 'CI: RED branch naming',
      passes: [{ pass: 0, agentRunRef: 'wf-ci', phase: 'Succeeded', output: 'CI: RED branch naming' }],
    },
    {
      name: 'adversary', agentRef: 'adversary', phase: 'Pending', iteration: 1, loop: 'adversary', staleOutput: true, output: 'VERDICT: REQUEST_CHANGES',
      passes: [{ pass: 0, agentRunRef: 'wf-adversary', phase: 'Succeeded', output: 'VERDICT: REQUEST_CHANGES' }],
    },
  ],
}

vi.mock('../api/sse', async (orig) => ({
  ...(await orig<typeof import('../api/sse')>()),
  getWorkflow: vi.fn(async () => detail),
  retryWorkflowStep: vi.fn(async () => {}),
  getRun: vi.fn(async () => ({ childRunRefs: [] })),
  subscribeToRunStream: vi.fn(() => () => {}),
}))

import { getWorkflow, retryWorkflowStep } from '../api/sse'
import { WorkflowView } from './WorkflowView'

describe('WorkflowView loops', () => {
  beforeEach(() => vi.clearAllMocks())

  it('shows the loop, its pass count and its exit condition', async () => {
    render(<WorkflowView name="wf" namespace="default" />)
    expect(await screen.findByText(/Loop: ci → adversary/)).toBeInTheDocument()
    expect(screen.getByText(/pass 2 of 4 · repeating/)).toBeInTheDocument()
    expect(screen.getByText(/CI: GREEN/)).toBeInTheDocument()
  })

  it('marks a step queued for another pass instead of showing its old output as a result', async () => {
    render(<WorkflowView name="wf" namespace="default" />)
    expect(await screen.findAllByText(/queued for pass 2/)).not.toHaveLength(0)
    expect(screen.getAllByText(/pass 2\/4/).length).toBeGreaterThan(0)
  })

  it('lets you open an earlier pass of a step', async () => {
    render(<WorkflowView name="wf" namespace="default" />)
    await userEvent.click(await screen.findByRole('button', { name: /ci/ }))
    await userEvent.click(await screen.findByRole('button', { name: /1 · Succeeded/ }))
    expect(screen.getByText('CI: RED branch naming', { selector: '#output-step-body' })).toBeInTheDocument()
  })

  it('offers to retry a failed step', async () => {
    const failed: AgentWorkflowDetail = {
      ...detail,
      phase: 'Failed',
      loops: [],
      steps: [{ name: 'merge', agentRef: 'merger', phase: 'Failed', failureReason: 'the workflow sandbox no longer exists' }],
    }
    vi.mocked(getWorkflow).mockResolvedValue(failed)
    render(<WorkflowView name="wf" namespace="default" />)
    await userEvent.click(await screen.findByRole('button', { name: 'Retry step' }))
    expect(retryWorkflowStep).toHaveBeenCalledWith('default', 'wf', 'merge')
  })
})
