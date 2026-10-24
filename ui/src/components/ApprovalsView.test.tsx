import { describe, expect, it, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { Approval } from '../api/sse'

const mockList = vi.fn()
const mockDecide = vi.fn()
vi.mock('../api/sse', async () => {
  const actual = await vi.importActual<typeof import('../api/sse')>('../api/sse')
  return {
    ...actual,
    listApprovals: (...args: unknown[]) => mockList(...args),
    decideApproval: (...args: unknown[]) => mockDecide(...args),
  }
})

import { ApprovalsView, formatRelative } from './ApprovalsView'

function pending(over: Partial<Approval> = {}): Approval {
  return {
    id: 'a1',
    namespace: 'team-a',
    runName: 'coder-run',
    kind: 'egress',
    summary: '/usr/bin/curl wants to reach example.com:443',
    details: {
      hosts: ['example.com:443'],
      binaries: ['/usr/bin/curl'],
      rationale: 'Allow curl to connect to example.com:443 (HTTPS).',
      validation: 'prover: no new findings',
      hits: 4,
    },
    approverGroups: ['platform'],
    status: 'pending',
    requestedAt: new Date().toISOString(),
    expiresAt: new Date(Date.now() + 20 * 60000).toISOString(),
    ...over,
  }
}

describe('ApprovalsView', () => {
  beforeEach(() => {
    mockList.mockReset()
    mockDecide.mockReset()
  })

  it('shows what a pending request asks for', async () => {
    mockList.mockResolvedValue([pending()])
    render(<ApprovalsView />)
    expect(await screen.findByText('/usr/bin/curl wants to reach example.com:443')).toBeInTheDocument()
    expect(screen.getByText('example.com:443')).toBeInTheDocument()
    expect(screen.getByText('prover: no new findings')).toBeInTheDocument()
    expect(screen.getByText('platform')).toBeInTheDocument()
    expect(screen.getByText('team-a/coder-run')).toBeInTheDocument()
    expect(mockList).toHaveBeenCalledWith('pending')
  })

  it('approves with the typed reason and refreshes', async () => {
    mockList.mockResolvedValueOnce([pending()]).mockResolvedValue([])
    mockDecide.mockResolvedValue({ ...pending(), status: 'approved' })
    render(<ApprovalsView />)
    const user = userEvent.setup()
    await user.type(await screen.findByLabelText('Reason for the decision'), 'needed for the docs build')
    await user.click(screen.getByRole('button', { name: 'Approve' }))
    expect(mockDecide).toHaveBeenCalledWith('a1', 'approve', 'needed for the docs build')
    expect(await screen.findByText('Nothing waiting for a decision')).toBeInTheDocument()
  })

  it('shows why a decision was refused', async () => {
    mockList.mockResolvedValue([pending()])
    mockDecide.mockRejectedValue(new Error('you are not in an approver group for this request'))
    render(<ApprovalsView />)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Reject' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('not in an approver group')
  })

  it('warns about security notes from the policy prover', async () => {
    mockList.mockResolvedValue([pending({ details: { securityNotes: 'requests a write method', hosts: ['x:443'] } })])
    render(<ApprovalsView />)
    expect(await screen.findByRole('note')).toHaveTextContent('requests a write method')
  })

  it('explains when approvals are not configured', async () => {
    mockList.mockRejectedValue(new Error('not configured'))
    render(<ApprovalsView />)
    expect(await screen.findByText('Approvals are not available')).toBeInTheDocument()
  })

  it('lists decided requests on the Recent tab', async () => {
    mockList.mockImplementation(async (status: string) =>
      status === '' ? [pending({ status: 'approved', decidedBy: 'alice@example.com', reason: 'ok' })] : [],
    )
    render(<ApprovalsView />)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('tab', { name: /Recent/ }))
    await waitFor(() => expect(mockList).toHaveBeenCalledWith(''))
    expect(await screen.findByText(/by alice@example.com/)).toBeInTheDocument()
  })
})

describe('formatRelative', () => {
  it('reads as a deadline or an age', () => {
    const now = Date.parse('2026-10-23T12:00:00Z')
    expect(formatRelative('2026-10-23T12:12:00Z', now)).toBe('in 12m')
    expect(formatRelative('2026-10-23T11:57:00Z', now)).toBe('3m ago')
    expect(formatRelative('2026-10-23T14:00:00Z', now)).toBe('in 2h')
  })
})
