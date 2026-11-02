import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { RoutingBenchmark, RoutingRecommendation } from '../api/sse'

const mockRecs = vi.fn()
const mockBenches = vi.fn()
const mockAsk = vi.fn()
vi.mock('../api/sse', async () => {
  const actual = await vi.importActual<typeof import('../api/sse')>('../api/sse')
  return {
    ...actual,
    listRoutingRecommendations: (...a: unknown[]) => mockRecs(...a),
    listRoutingBenchmarks: (...a: unknown[]) => mockBenches(...a),
    askRecommendationAction: (...a: unknown[]) => mockAsk(...a),
  }
})

import { RoutingInsightsView, pct, usd } from './RoutingInsightsView'

const PATCH = '{"spec":{"capabilityRouting":{"stage:chat":"weak"}}}'

function rec(over: Partial<RoutingRecommendation> = {}): RoutingRecommendation {
  return {
    name: 'adaptive-overspend-1', namespace: 'team-a', selector: 'adaptive', benchmark: 'bench', detector: 'overspend',
    agent: 'coder', category: 'stage:chat', summary: 'strong serves stage:chat calls of coder that weak answers as well',
    patch: PATCH, rollbackPatch: '{"spec":{"capabilityRouting":{"stage:chat":null}}}', baseGeneration: 3, action: 'none',
    phase: 'Proposed', createdAt: '2026-11-01T00:00:00Z', canRequestApply: false, canRollback: false,
    kubectlPatch: `kubectl patch modelselector adaptive -n team-a --type merge -p '${PATCH}'`,
    evidence: { samples: 100, netWin: 0, netWinLo: -0.03, netWinHi: 0.03, deltaCostUsd: 0.009, projectedMonthlySavingsUsd: 11.57 },
    ...over,
  }
}

function bench(over: Partial<RoutingBenchmark> = {}): RoutingBenchmark {
  return {
    name: 'bench', namespace: 'team-a', mode: 'reportOnly', agents: ['coder'], ready: true, judge: 'judge', sampleRate: '0.05',
    perDayUSD: '2.00', shadowSpendTodayUSD: '0.040000', sampledCalls: 12, judgedPairs: 10, judgeInconsistentRate: '0.100',
    categories: [{ agent: 'coder', category: 'stage:chat', samples: 10, servedWins: 3, shadowWins: 1, ties: 6 }],
    ...over,
  }
}

describe('formatting', () => {
  it('shows percentages and small amounts of money readably', () => {
    expect(pct(0.031)).toBe('3%')
    expect(pct(0.0312, 1)).toBe('3.1%')
    expect(usd(11.5714)).toBe('$11.57')
    expect(usd(0.009)).toBe('$0.0090')
  })
})

describe('RoutingInsightsView', () => {
  beforeEach(() => {
    mockRecs.mockReset().mockResolvedValue([])
    mockBenches.mockReset().mockResolvedValue([])
    mockAsk.mockReset()
    Object.assign(navigator, { clipboard: { writeText: vi.fn().mockResolvedValue(undefined) } })
  })
  afterEach(() => vi.useRealTimers())

  it('lists a recommendation with the evidence behind it', async () => {
    mockRecs.mockResolvedValue([rec()])
    render(<RoutingInsightsView />)
    await screen.findByText(/that weak answers as well/)
    expect(screen.getByText('team-a/adaptive · coder · stage:chat')).toBeInTheDocument()
    expect(screen.getByText('Overspend')).toBeInTheDocument()
    expect(screen.getByText('100')).toBeInTheDocument()
    expect(screen.getByText('0% (-3% to 3%)')).toBeInTheDocument()
    expect(screen.getByText('$0.0090')).toBeInTheDocument()
    expect(screen.getByText('$11.57 a month')).toBeInTheDocument()
  })

  it('says when there is nothing to recommend, and what produces recommendations', async () => {
    render(<RoutingInsightsView />)
    await screen.findByText('Nothing to recommend')
    expect(screen.getByText(/They need PostgreSQL/)).toBeInTheDocument()
  })

  it('hides decided recommendations until All is chosen', async () => {
    mockRecs.mockResolvedValue([rec(), rec({ name: 'old', phase: 'Rejected', summary: 'an old finding' })])
    render(<RoutingInsightsView />)
    await screen.findByText(/that weak answers as well/)
    expect(screen.queryByText('an old finding')).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('tab', { name: 'All' }))
    expect(await screen.findByText('an old finding')).toBeInTheDocument()
  })

  it('copies the kubectl command and the patch as YAML', async () => {
    mockRecs.mockResolvedValue([rec()])
    render(<RoutingInsightsView />)
    await screen.findByText(/that weak answers as well/)
    await userEvent.click(screen.getByRole('button', { name: 'Copy kubectl command' }))
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(`kubectl patch modelselector adaptive -n team-a --type merge -p '${PATCH}'`)
    await userEvent.click(screen.getByRole('button', { name: 'Copy patch as YAML' }))
    expect(navigator.clipboard.writeText).toHaveBeenLastCalledWith('spec:\n  capabilityRouting:\n    "stage:chat": weak\n')
  })

  it('shows the patch and how to undo it', async () => {
    mockRecs.mockResolvedValue([rec()])
    render(<RoutingInsightsView />)
    await screen.findByText(/that weak answers as well/)
    const details = screen.getByText('Patch').closest('details') as HTMLElement
    expect(within(details).getByText(/"stage:chat": "weak"/)).toBeInTheDocument()
    expect(within(details).getByText('Undo with')).toBeInTheDocument()
    expect(within(details).getByText(/"stage:chat": null/)).toBeInTheDocument()
  })

  it('offers to ask for approval only where the server says it can', async () => {
    mockRecs.mockResolvedValue([rec({ name: 'a', canRequestApply: true }), rec({ name: 'b', blockedReason: 'applying needs a RoutingBenchmark with mode proposeApply' })])
    render(<RoutingInsightsView />)
    await screen.findAllByText(/that weak answers as well/)
    expect(screen.getAllByRole('button', { name: 'Ask for approval to apply' })).toHaveLength(1)
    expect(screen.getByText(/applying needs a RoutingBenchmark with mode proposeApply/)).toBeInTheDocument()
  })

  it('asks for approval and reloads', async () => {
    mockRecs.mockResolvedValue([rec({ canRequestApply: true })])
    mockAsk.mockResolvedValue(undefined)
    render(<RoutingInsightsView />)
    await userEvent.click(await screen.findByRole('button', { name: 'Ask for approval to apply' }))
    await waitFor(() => expect(mockAsk).toHaveBeenCalledWith('team-a', 'adaptive-overspend-1', 'requestApply'))
    await waitFor(() => expect(mockRecs).toHaveBeenCalledTimes(2))
  })

  it('shows why a request failed', async () => {
    mockRecs.mockResolvedValue([rec({ canRequestApply: true })])
    mockAsk.mockRejectedValue(new Error('asking for a routing change needs a signed-in user'))
    render(<RoutingInsightsView />)
    await userEvent.click(await screen.findByRole('button', { name: 'Ask for approval to apply' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('needs a signed-in user')
  })

  it('offers to ask for an undo for an applied change, and says when one is already asked', async () => {
    mockRecs.mockResolvedValue([
      rec({ name: 'applied', phase: 'Applied', canRollback: true, postApply: { verdict: 'Regressed', message: 'the strong model wins by 40%' } }),
      rec({ name: 'asked', phase: 'Applied', conditions: [{ type: 'RollbackRequested', status: 'True' }] }),
    ])
    render(<RoutingInsightsView />)
    await userEvent.click(await screen.findByRole('tab', { name: 'All' }))
    expect(await screen.findByRole('button', { name: 'Ask for approval to undo' })).toBeInTheDocument()
    expect(screen.getByText('Regressed')).toBeInTheDocument()
    expect(screen.getByText('the strong model wins by 40%')).toBeInTheDocument()
    expect(screen.getByText('Waiting for an approver to undo this change.')).toBeInTheDocument()
  })

  it('says a report-only finding is report only, and why', async () => {
    mockRecs.mockResolvedValue([rec({ patch: undefined, kubectlPatch: undefined, reason: 'a switchyard selector ignores stage routes' })])
    render(<RoutingInsightsView />)
    await screen.findByText(/that weak answers as well/)
    expect(screen.getByText('Report only: a switchyard selector ignores stage routes')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Copy/ })).not.toBeInTheDocument()
  })

  it('draws the cost and quality frontier for a threshold finding', async () => {
    mockRecs.mockResolvedValue([rec({
      detector: 'threshold', agent: undefined, category: undefined, summary: 'Moving adaptive from 0.50 to 0.30 would cost less',
      evidence: {
        samples: 200, fromThreshold: 0.5, toThreshold: 0.3, deltaCostUsd: -0.0027, lossChange: 0, lossChangeHi: 0.02,
        frontier: [{ threshold: 0.3, costUsd: 0.0021, loss: 0.1 }, { threshold: 0.5, costUsd: 0.0048, loss: 0.1, current: true }],
      },
    })])
    render(<RoutingInsightsView />)
    await screen.findByText(/Moving adaptive/)
    expect(screen.getByText('0.50 → 0.30')).toBeInTheDocument()
    expect(screen.getByText('proposed 0.30')).toBeInTheDocument()
    expect(screen.getByText('in use 0.50')).toBeInTheDocument()
  })

  it('shows a caveat and corroboration', async () => {
    mockRecs.mockResolvedValue([rec({ caveat: 'its effect cannot be estimated', corroboration: ['6 thumbs down against 1 up'] })])
    render(<RoutingInsightsView />)
    await screen.findByText(/that weak answers as well/)
    expect(screen.getByRole('note')).toHaveTextContent('its effect cannot be estimated')
    expect(screen.getByText('6 thumbs down against 1 up')).toBeInTheDocument()
  })

  it('shows what the benchmarks found, per Agent and category', async () => {
    mockBenches.mockResolvedValue([bench({ warnings: ['JudgeIsArm: judge also compared'] })])
    render(<RoutingInsightsView />)
    await userEvent.click(screen.getByRole('tab', { name: 'Benchmarks' }))
    await screen.findByText('bench')
    expect(screen.getByText('ready')).toBeInTheDocument()
    expect(screen.getByText('12 sampled, 10 judged')).toBeInTheDocument()
    expect(screen.getByText('$0.0400 of $2.00 today')).toBeInTheDocument()
    expect(screen.getByText('10.0%')).toBeInTheDocument()
    const table = screen.getByRole('table', { name: 'Verdicts for bench' })
    expect(within(table).getByText('3 / 1 / 6')).toBeInTheDocument()
    expect(screen.getByText(/JudgeIsArm: judge also compared/)).toBeInTheDocument()
  })

  it('says why a benchmark is not ready and when its budget is spent', async () => {
    mockBenches.mockResolvedValue([bench({ ready: false, readyMessage: 'no Agent matches the benchmark', budgetExhausted: true, agents: [] })])
    render(<RoutingInsightsView />)
    await userEvent.click(screen.getByRole('tab', { name: 'Benchmarks' }))
    await screen.findByText('not ready')
    expect(screen.getByText(/no Agent matches the benchmark/)).toBeInTheDocument()
    expect(screen.getByText(/budget spent/)).toBeInTheDocument()
    expect(screen.getByText('none yet')).toBeInTheDocument()
  })

  it('shows an error from the server', async () => {
    mockRecs.mockRejectedValue(new Error('listing recommendations failed'))
    render(<RoutingInsightsView />)
    expect(await screen.findByRole('alert')).toHaveTextContent('listing recommendations failed')
  })
})
