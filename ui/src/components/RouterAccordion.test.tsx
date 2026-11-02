import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { routingEventToDecision, type RoutingDecisionInfo, type RoutingEventInfo } from '../api/sse'
import { RouterAccordion } from './RouterAccordion'

const decision = (model: string, reason = 'weighted random'): RoutingDecisionInfo => ({
  model,
  provider: model,
  strategy: 'hybrid',
  reason,
  confidence: '0.50',
})

async function openAccordion() {
  await userEvent.click(screen.getByRole('button', { name: /model router/i }))
}

describe('RouterAccordion', () => {
  it('shows no truncation note when every decision is held', async () => {
    render(<RouterAccordion decisions={[decision('a'), decision('b')]} total={2} />)
    await openAccordion()
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('shows no truncation note for runs recorded before the total existed', async () => {
    render(<RouterAccordion decisions={[decision('a')]} />)
    await openAccordion()
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('says the list is a window and loads the full history on request', async () => {
    const loadAll = vi.fn().mockResolvedValue([decision('a'), decision('b'), decision('c'), decision('d')])
    render(<RouterAccordion decisions={[decision('c'), decision('d')]} total={4} loadAll={loadAll} />)
    await openAccordion()

    expect(screen.getByRole('status')).toHaveTextContent('Showing the latest 2 of 4 decisions.')
    await userEvent.click(screen.getByRole('button', { name: 'Load full history' }))

    await waitFor(() => expect(screen.queryByRole('status')).toBeNull())
    expect(loadAll).toHaveBeenCalledTimes(1)
    expect(screen.getByText('a')).toBeInTheDocument()
  })

  it('says so when the full history is unavailable and stops offering it', async () => {
    const loadAll = vi.fn().mockRejectedValue(new Error('HTTP 503'))
    render(<RouterAccordion decisions={[decision('c')]} total={150} loadAll={loadAll} />)
    await openAccordion()
    await userEvent.click(screen.getByRole('button', { name: 'Load full history' }))

    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('The full history is not available.'))
    expect(screen.queryByRole('button', { name: 'Load full history' })).toBeNull()
  })

  it('offers no button when there is no way to load more', async () => {
    render(<RouterAccordion decisions={[decision('c')]} total={150} />)
    await openAccordion()
    expect(screen.getByRole('status')).toHaveTextContent('Showing the latest 1 of 150 decisions.')
    expect(screen.queryByRole('button', { name: 'Load full history' })).toBeNull()
  })
})

describe('routingEventToDecision', () => {
  const base: RoutingEventInfo = { decisionID: 'd-1', provider: 'p', model: 'openai/gpt', strategy: 'hybrid', path: 'rule:vision', stage: 'code', confidence: 0.95 }

  it('maps a served call to a decision row', () => {
    expect(routingEventToDecision(base)).toMatchObject({
      model: 'openai/gpt', provider: 'p', reason: 'rule:vision', confidence: '0.95', decisionID: 'd-1', category: 'code',
    })
  })

  it('explains a fallback and a failed attempt', () => {
    expect(routingEventToDecision({ ...base, path: 'fallback', fallbackFrom: 'primary' }).reason).toBe('fallback from primary')
    expect(routingEventToDecision({ ...base, errorClass: 'provider_5xx' }).reason).toBe('rule:vision (provider_5xx)')
  })

  it('falls back to the provider name when the model is unknown', () => {
    expect(routingEventToDecision({ decisionID: 'd', provider: 'only-provider' }).model).toBe('only-provider')
  })
})
