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

  it('shows a workflow gate with its message and no run link', async () => {
    mockList.mockResolvedValue([
      pending({
        kind: 'workflow',
        runName: '',
        summary: 'wf / gate: Test report',
        details: { workflow: 'wf', step: 'gate', message: 'Test report:\n42 passed\nShip it?' },
      }),
    ])
    render(<ApprovalsView />)
    expect(await screen.findByText('wf / gate: Test report')).toBeInTheDocument()
    expect(screen.getByText('wf / gate')).toBeInTheDocument()
    expect(screen.getByText(/42 passed/)).toBeInTheDocument()
    expect(screen.getByText(/the step fails and the workflow stops/)).toBeInTheDocument()
    // No run, so no "team-a/" link to a run that does not exist.
    expect(screen.queryByText('team-a/')).toBeNull()
    expect(screen.queryByRole('button', { name: /team-a\// })).toBeNull()
    expect(screen.getByText('team-a')).toBeInTheDocument()
  })

  it('shows a gated tool call with its arguments and what rejecting does', async () => {
    mockList.mockResolvedValue([
      pending({
        kind: 'tool',
        summary: 'coder-run wants to run the actuate tool "deploy"',
        details: { tool: 'deploy', sideEffect: 'actuate', arguments: '{"env":"prod"}', agent: 'coder' },
      }),
    ])
    render(<ApprovalsView />)
    expect(await screen.findByText('coder-run wants to run the actuate tool "deploy"')).toBeInTheDocument()
    expect(screen.getByText('deploy (actuate)')).toBeInTheDocument()
    expect(screen.getByText('{"env":"prod"}')).toBeInTheDocument()
    expect(screen.getByText(/the tool does not run; the agent is told and the run goes on/)).toBeInTheDocument()
    expect(screen.queryByText('Destination')).toBeNull()
  })

  it('shows a run that reached its budget, with what approving and rejecting do', async () => {
    mockList.mockResolvedValue([
      pending({
        kind: 'budget',
        summary: 'coder-run reached its $2.00 budget: raise it by $1.50?',
        details: { raise: 1, spentUSD: '2.0300', capUSD: '2.00', raiseByUSD: '1.5000', newCapUSD: '3.5000' },
      }),
    ])
    render(<ApprovalsView />)
    expect(await screen.findByText('coder-run reached its $2.00 budget: raise it by $1.50?')).toBeInTheDocument()
    expect(screen.getByText('$2.0300')).toBeInTheDocument()
    expect(screen.getByText('$2.00')).toBeInTheDocument()
    expect(screen.getByText(/cap rises by \$1.5000 to \$3.5000; the run resumes/)).toBeInTheDocument()
    expect(screen.getByText('the run fails')).toBeInTheDocument()
    // The egress-only facts stay out of a budget request.
    expect(screen.queryByText('Destination')).toBeNull()
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

describe('ApprovalsView routing approvals', () => {
  beforeEach(() => {
    mockList.mockReset()
    mockDecide.mockReset()
  })

  const routing = (over: Partial<Approval> = {}): Approval =>
    pending({
      kind: 'routing',
      runName: '',
      summary: 'Apply routing change for adaptive: weak answers as well',
      details: {
        selector: 'adaptive',
        category: 'stage:chat',
        summary: 'weak answers as well as strong in 100 judged pairs',
        patch: { spec: { capabilityRouting: { 'stage:chat': 'weak' } } },
        evidence: { projectedMonthlySavingsUsd: 11.57 },
        caveat: 'a stage_router escalates when tool calls fail',
      },
      ...over,
    })

  it('shows what will change, what the evidence says and what happens either way', async () => {
    mockList.mockResolvedValue([routing()])
    render(<ApprovalsView />)
    await screen.findByText('Apply routing change for adaptive: weak answers as well')
    expect(screen.getByText('team-a/adaptive')).toBeInTheDocument()
    expect(screen.getByText('stage:chat')).toBeInTheDocument()
    expect(screen.getByText('weak answers as well as strong in 100 judged pairs')).toBeInTheDocument()
    expect(screen.getByText('$11.57 a month')).toBeInTheDocument()
    // The patch the approver is agreeing to is shown in full.
    expect(screen.getByText(/"stage:chat": "weak"/)).toBeInTheDocument()
    expect(screen.getByText(/the selector is changed now/)).toBeInTheDocument()
    expect(screen.getByText('the selector is left as it is')).toBeInTheDocument()
    expect(screen.getByRole('note')).toHaveTextContent('a stage_router escalates when tool calls fail')
  })

  it('has no run to link to, only the namespace', async () => {
    mockList.mockResolvedValue([routing()])
    render(<ApprovalsView onNavigateToRun={vi.fn()} />)
    await screen.findByText('Apply routing change for adaptive: weak answers as well')
    expect(screen.queryByRole('button', { name: /team-a\// })).not.toBeInTheDocument()
  })

  it('describes an undo as putting the selector back', async () => {
    mockList.mockResolvedValue([
      routing({
        kind: 'routing-rollback',
        summary: 'Undo routing change for adaptive: weak answers as well',
        details: { selector: 'adaptive', patch: { spec: { capabilityRouting: { 'stage:chat': null } } } },
      }),
    ])
    render(<ApprovalsView />)
    await screen.findByText('Undo routing change for adaptive: weak answers as well')
    expect(screen.getByText('Undo with')).toBeInTheDocument()
    expect(screen.getByText('the selector goes back to how it was before the change')).toBeInTheDocument()
    expect(screen.queryByText(/the selector is changed now/)).not.toBeInTheDocument()
  })

  it('approves with the reason the approver typed', async () => {
    mockList.mockResolvedValue([routing()])
    mockDecide.mockResolvedValue({})
    render(<ApprovalsView />)
    await screen.findByText('Apply routing change for adaptive: weak answers as well')
    await userEvent.type(screen.getByLabelText('Reason for the decision'), 'looks right')
    await userEvent.click(screen.getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(mockDecide).toHaveBeenCalledWith('a1', 'approve', 'looks right'))
  })
})
