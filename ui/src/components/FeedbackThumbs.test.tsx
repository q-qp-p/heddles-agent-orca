import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { STREAM_EVENT_TYPE } from '../contracts/events'
import { TraceAccordion } from './TraceAccordion'
import { OutputCard } from './OutputCard'

const ts = '2026-01-15T12:00:00.000Z'
const decision = {
  type: STREAM_EVENT_TYPE.modelSelected,
  model: 'claude-sonnet-5-5',
  reason: 'Escalated to sonnet: tool calls have been failing repeatedly',
  confidence: 0.9,
  decisionId: 'd-1',
} as const

function mockFetch(opts: { feedback?: unknown[]; status?: number; postStatus?: number }) {
  const fetchMock = vi.fn(async (_url: string, init?: RequestInit) => {
    if (init?.method === 'POST') return new Response(null, { status: opts.postStatus ?? 204 })
    if (opts.status && opts.status !== 200) return new Response('{}', { status: opts.status })
    return new Response(JSON.stringify({ feedback: opts.feedback ?? [] }), { status: 200 })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

const run = { namespace: 'team-a', runName: 'run-1' }

async function renderTrace() {
  render(<TraceAccordion entries={[{ id: 0, event: decision, ts }]} feedbackRun={run} />)
  await userEvent.click(screen.getByText(/Execution Trace|Live Trace/))
}

describe('feedback thumbs', () => {
  beforeEach(() => vi.unstubAllGlobals())
  afterEach(() => vi.unstubAllGlobals())

  it('rates a routing decision', async () => {
    const fetchMock = mockFetch({})
    await renderTrace()
    await userEvent.click(await screen.findByRole('button', { name: /Poor choice: routing to claude-sonnet-5-5/ }))
    const post = fetchMock.mock.calls.find(([, init]) => init?.method === 'POST')!
    expect(post[0]).toBe('/api/runs/team-a/run-1/feedback')
    expect(JSON.parse(post[1]!.body as string)).toEqual({ decisionId: 'd-1', rating: -1 })
    expect(screen.getByRole('button', { name: /Poor choice/ })).toHaveAttribute('aria-pressed', 'true')
  })

  it('shows the rating the caller already gave', async () => {
    mockFetch({ feedback: [{ decisionId: 'd-1', rating: 1 }] })
    await renderTrace()
    expect(await screen.findByRole('button', { name: /Good choice/ })).toHaveAttribute('aria-pressed', 'true')
  })

  it('takes the thumb back when the rating was not saved', async () => {
    mockFetch({ postStatus: 500 })
    await renderTrace()
    await userEvent.click(await screen.findByRole('button', { name: /Good choice/ }))
    expect(await screen.findByRole('alert')).toHaveTextContent('not saved')
    expect(screen.getByRole('button', { name: /Good choice/ })).toHaveAttribute('aria-pressed', 'false')
  })

  it('sends a comment with the rating', async () => {
    const fetchMock = mockFetch({})
    await renderTrace()
    await userEvent.click(await screen.findByRole('button', { name: /Poor choice/ }))
    await userEvent.type(await screen.findByLabelText(/Comment on/), 'should have stayed cheap')
    await userEvent.click(screen.getByRole('button', { name: 'Send' }))
    const bodies = fetchMock.mock.calls.filter(([, i]) => i?.method === 'POST').map(([, i]) => JSON.parse(i!.body as string))
    expect(bodies[1]).toEqual({ decisionId: 'd-1', rating: -1, comment: 'should have stayed cheap' })
  })

  it('shows no thumbs without a PostgreSQL store', async () => {
    mockFetch({ status: 503 })
    await renderTrace()
    await screen.findByText(/Escalated to sonnet/)
    await waitFor(() => expect(screen.queryByRole('button', { name: /Good choice/ })).not.toBeInTheDocument())
  })

  it('shows no thumbs without a run to rate', async () => {
    const fetchMock = mockFetch({})
    render(<TraceAccordion entries={[{ id: 0, event: decision, ts }]} />)
    await userEvent.click(screen.getByText(/Execution Trace|Live Trace/))
    expect(screen.queryByRole('button', { name: /Good choice/ })).not.toBeInTheDocument()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('rates the whole run from the output card, without a decision ID', async () => {
    const fetchMock = mockFetch({})
    render(<OutputCard output="done" feedbackRun={run} />)
    await userEvent.click(await screen.findByRole('button', { name: /Good choice: this run/ }))
    const post = fetchMock.mock.calls.find(([, init]) => init?.method === 'POST')!
    expect(JSON.parse(post[1]!.body as string)).toEqual({ rating: 1 })
  })

  it('does not offer to rate a run that is still streaming', () => {
    const fetchMock = mockFetch({})
    render(<OutputCard output="" streaming feedbackRun={run} />)
    expect(screen.queryByRole('button', { name: /Good choice/ })).not.toBeInTheDocument()
    expect(fetchMock).not.toHaveBeenCalled()
  })
})
