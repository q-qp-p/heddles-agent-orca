import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { STREAM_EVENT_TYPE } from '../contracts/events'
import { SSE_CLIENT_PARSE_EVENT_TYPE } from '../api/traceStream'
import type { TraceEvent } from '../api/sse'
import { TraceAccordion } from './TraceAccordion'

const ts = '2026-01-15T12:00:00.000Z'

function entry(id: number, event: TraceEvent) {
  return { id, event, ts }
}

async function openAccordion() {
  await userEvent.click(screen.getByText(/Execution Trace|Live Trace/))
}

describe('TraceAccordion', () => {
  it('shows streaming label when streaming', () => {
    render(<TraceAccordion entries={[]} streaming />)
    expect(screen.getByText(/streaming/)).toBeInTheDocument()
  })

  it('shows event count when not streaming', () => {
    render(<TraceAccordion entries={[entry(0, { type: 'thought', content: 'x' })]} />)
    expect(screen.getByText('1 events')).toBeInTheDocument()
  })

  it('shows no-events message immediately when non-streaming and empty (no quip delay)', async () => {
    render(<TraceAccordion entries={[]} />)
    await openAccordion()
    // Non-streaming mode must not block for QUIP_MIN_DURATION — the empty
    // state surfaces immediately so historical (archived) runs don't hang.
    expect(screen.getByText(/No trace events yet/)).toBeInTheDocument()
    expect(screen.queryByText(/Checking for traces/)).not.toBeInTheDocument()
    expect(screen.queryByText(/Retrieving events/)).not.toBeInTheDocument()
  })

  it('shows retrieving events with a quip when streaming and empty', async () => {
    render(<TraceAccordion entries={[]} streaming />)
    await openAccordion()
    expect(screen.getByText(/Retrieving events/)).toBeInTheDocument()
  })

  it('renders thought as an expandable thinking panel', async () => {
    const user = userEvent.setup()
    render(
      <TraceAccordion
        entries={[
          entry(0, { type: 'thought', content: 'planning' }),
          entry(1, { type: STREAM_EVENT_TYPE.toolCall, name: 'x', arguments: '{}' }),
          entry(2, { type: STREAM_EVENT_TYPE.toolResult, name: 'x', result: 'ok' }),
        ]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/thinking/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /expand thinking/i })).toBeInTheDocument()
    // Collapsed by default — the reasoning content is not in the DOM yet.
    expect(screen.queryByText('planning')).not.toBeInTheDocument()
    // Expanding reveals the captured thinking.
    await user.click(screen.getByRole('button', { name: /expand thinking/i }))
    expect(screen.getByText('planning')).toBeInTheDocument()
    expect(screen.getByText(/calls tool/)).toBeInTheDocument()
    expect(screen.getByText(/result/)).toBeInTheDocument()
  })

  it('renders tool_result with markdown path when markdown is on', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: STREAM_EVENT_TYPE.toolResult, name: 'fn', result: '`code`' })]}
        markdown
      />,
    )
    await openAccordion()
    expect(screen.getByText('code')).toBeInTheDocument()
  })

  it('renders tool approval events under the Tools filter', async () => {
    render(
      <TraceAccordion
        entries={[
          entry(0, { type: 'toolApproval', state: 'requested', tool: 'deploy', sideEffect: 'actuate', approvalId: 'a1' }),
          entry(1, { type: 'toolApproval', state: 'rejected', tool: 'deploy', sideEffect: 'actuate', by: 'bob', reason: 'not on a Friday' }),
          entry(2, { type: STREAM_EVENT_TYPE.modelSelected, model: 'm', reason: 'r', confidence: 0.5 }),
        ]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/waiting for an approver/)).toBeInTheDocument()
    expect(screen.getByText(/rejected by bob — not on a Friday/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Tools' }))
    expect(screen.queryByText(/routed/)).toBeNull()
    expect(screen.getByText(/waiting for an approver/)).toBeInTheDocument()
  })

  it('renders budget events and filters them under Budget', async () => {
    render(
      <TraceAccordion
        entries={[
          entry(0, { type: 'budget', state: 'warning', spentUSD: '1.6200', capUSD: '2.0000' }),
          entry(1, { type: 'budget', state: 'exceeded', spentUSD: '2.0300', capUSD: '2.0000' }),
          entry(2, { type: 'budget', state: 'resumed', spentUSD: '2.0300', capUSD: '3.5000' }),
          entry(3, { type: STREAM_EVENT_TYPE.modelSelected, model: 'm', reason: 'r', confidence: 0.5 }),
        ]}
      />,
    )
    await openAccordion()
    expect(screen.getByText('budget warning')).toBeInTheDocument()
    expect(screen.getByText('budget reached, paused for approval')).toBeInTheDocument()
    expect(screen.getByText('budget raised, resuming')).toBeInTheDocument()
    expect(screen.getByText(/\$2.0300 spent of \$3.5000/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Budget' }))
    expect(screen.queryByText(/routed/)).toBeNull()
    expect(screen.getByText('budget warning')).toBeInTheDocument()
  })

  it('renders sandbox network decisions and filters them under Network', async () => {
    render(
      <TraceAccordion
        entries={[
          entry(0, { type: 'egress', kind: 'net', action: 'allowed', binary: '/usr/bin/curl', destination: 'pypi.org:443', policy: 'agentorca-egress-0', raw: 'NET:OPEN ...' }),
          entry(1, { type: 'egress', kind: 'http', action: 'denied', method: 'POST', url: 'https://pypi.org/simple/', reason: 'POST /simple/ not permitted by policy', raw: 'HTTP:POST ...' }),
          entry(2, { type: STREAM_EVENT_TYPE.modelSelected, model: 'm', reason: 'r', confidence: 0.5 }),
        ]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/pypi.org:443/)).toBeInTheDocument()
    expect(screen.getByText(/by \/usr\/bin\/curl/)).toBeInTheDocument()
    expect(screen.getByText('blocked')).toBeInTheDocument()
    expect(screen.getByText(/POST https:\/\/pypi.org\/simple\//)).toBeInTheDocument()
    expect(screen.getByText(/ — POST \/simple\/ not permitted by policy/)).toBeInTheDocument()
    expect(screen.queryByText(/2014/)).toBeNull()

    await userEvent.click(screen.getByRole('button', { name: 'Network' }))
    expect(screen.queryByText(/routed/)).not.toBeInTheDocument()
    expect(screen.getByText('allowed')).toBeInTheDocument()
  })

  it('renders model_selected with confidence', async () => {
    render(
      <TraceAccordion entries={[entry(0, { type: STREAM_EVENT_TYPE.modelSelected, model: 'm', reason: 'r', confidence: 0.42 })]} />,
    )
    await openAccordion()
    expect(screen.getByText(/routed/)).toBeInTheDocument()
    expect(screen.getByText(/42%/)).toBeInTheDocument()
  })

  it('renders final_output with markdown when enabled', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: STREAM_EVENT_TYPE.finalOutput, output: '# Title' })]}
        markdown
      />,
    )
    await openAccordion()
    expect(screen.getByRole('heading', { level: 1, name: 'Title' })).toBeInTheDocument()
  })

  it('renders final_output and done as plain text when markdown is off', async () => {
    render(
      <TraceAccordion
        entries={[
          entry(0, { type: STREAM_EVENT_TYPE.finalOutput, output: 'plain out' }),
          entry(1, { type: STREAM_EVENT_TYPE.done, output: 'done plain' }),
        ]}
      />,
    )
    await openAccordion()
    expect(screen.getByText('plain out')).toBeInTheDocument()
    expect(screen.getByText('done plain')).toBeInTheDocument()
  })

  it('renders done with markdown when enabled', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: STREAM_EVENT_TYPE.done, output: '## Done' })]}
        markdown
      />,
    )
    await openAccordion()
    expect(screen.getByRole('heading', { level: 2, name: 'Done' })).toBeInTheDocument()
  })

  it('renders error, token, clarify', async () => {
    render(
      <TraceAccordion
        entries={[
          entry(0, { type: 'error', message: 'bad' }),
          entry(1, { type: 'token', content: 't' }),
          entry(2, { type: 'clarify', question: 'q?' }),
        ]}
      />,
    )
    await openAccordion()
    expect(screen.getByText('bad')).toBeInTheDocument()
    expect(screen.getByText(/token/)).toBeInTheDocument()
    expect(screen.getByText(/clarify/)).toBeInTheDocument()
  })

  it('renders done, fail, agent_event, placeholder', async () => {
    render(
      <TraceAccordion
        entries={[
          entry(0, { type: STREAM_EVENT_TYPE.done, output: 'done out' }),
          entry(1, { type: STREAM_EVENT_TYPE.fail, reason: 'fail r' }),
          entry(2, { type: STREAM_EVENT_TYPE.agentEvent, eventType: 'evt', message: 'msg' }),
          entry(3, { type: STREAM_EVENT_TYPE.placeholder, message: 'wait' }),
        ]}
      />,
    )
    await openAccordion()
    expect(screen.getByText('done out')).toBeInTheDocument()
    expect(screen.getByText('fail r')).toBeInTheDocument()
    expect(screen.getByText(/evt/)).toBeInTheDocument()
    expect(screen.getByText('wait')).toBeInTheDocument()
  })

  it('renders sse_* agent_event rows with SSE parse styling', async () => {
    render(
      <TraceAccordion
        entries={[
          entry(0, {
            type: STREAM_EVENT_TYPE.agentEvent,
            eventType: SSE_CLIENT_PARSE_EVENT_TYPE.malformedJson,
            message: 'not-json{',
          }),
          entry(1, {
            type: STREAM_EVENT_TYPE.agentEvent,
            eventType: SSE_CLIENT_PARSE_EVENT_TYPE.missingOrInvalidType,
            message: '{}',
          }),
        ]}
      />,
    )
    await openAccordion()
    expect(screen.getAllByText(/SSE parse/).length).toBe(2)
    expect(screen.getByText(SSE_CLIENT_PARSE_EVENT_TYPE.malformedJson)).toBeInTheDocument()
    expect(screen.getByText(SSE_CLIENT_PARSE_EVENT_TYPE.missingOrInvalidType)).toBeInTheDocument()
    expect(screen.getByText('not-json{')).toBeInTheDocument()
  })

  it('falls back to JSON for unknown event types', async () => {
    const bogus = { type: 'unknown_kind', foo: 1 } as unknown as TraceEvent
    render(<TraceAccordion entries={[entry(0, bogus)]} />)
    await openAccordion()
    expect(screen.getByText(/unknown trace type/)).toBeInTheDocument()
    expect(screen.getAllByText('unknown_kind').length).toBeGreaterThanOrEqual(1)
  })

  it('renders ragResult with KB name, query, and result count', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: 'ragResult', name: 'llm-research-kb', query: 'attention mechanism', results: 7, collection: 'research-docs' } as TraceEvent)]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/RAG search/)).toBeInTheDocument()
    expect(screen.getByText(/7 results/)).toBeInTheDocument()
    expect(screen.getByText(/research-docs/)).toBeInTheDocument()
  })

  it('renders mcpDiscovery with server name and tool count', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: 'mcpDiscovery', server: 'filesystem', tools: 12 } as TraceEvent)]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/MCP server/)).toBeInTheDocument()
    expect(screen.getByText(/filesystem/)).toBeInTheDocument()
    expect(screen.getByText(/12 tools/)).toBeInTheDocument()
  })

  it('renders guardrail block event', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: 'guardrail', action: 'blocked', reason: 'PII detected' } as TraceEvent)]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/guardrail/)).toBeInTheDocument()
    expect(screen.getByText(/blocked/)).toBeInTheDocument()
    expect(screen.getByText(/PII detected/)).toBeInTheDocument()
  })

  it('renders providerFallback event', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: 'providerFallback', from: 'openai', to: 'anthropic', reason: 'rate limited' } as TraceEvent)]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/fallback/)).toBeInTheDocument()
    expect(screen.getByText(/openai/)).toBeInTheDocument()
    expect(screen.getByText(/anthropic/)).toBeInTheDocument()
  })

  it('renders providerFallback exhausted event', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: 'providerFallback', from: 'openai', to: '', reason: 'timeout', exhausted: true } as TraceEvent)]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/exhausted/)).toBeInTheDocument()
  })

  // Regression for #42: the router's pre-send truncation must be visible in the
  // web trace, not fall through to the unknown-event row.
  it('renders truncation event with before/after/budget', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: 'truncation', provider: 'openrouter', before: 1021745, after: 800000, budget: 838860 })]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/context compacted/)).toBeInTheDocument()
    expect(screen.getByText(/1,021,745/)).toBeInTheDocument()
    expect(screen.getByText(/800,000/)).toBeInTheDocument()
    expect(screen.getByText(/budget 838,860/)).toBeInTheDocument()
  })

  // #47: summary compaction says how the context was compacted, and the start
  // of a compaction is visible so the summary call's pause does not look hung.
  it('renders summary compaction and its start', async () => {
    render(
      <TraceAccordion
        entries={[
          entry(0, { type: 'compaction.started', mode: 'summary', before: 803741 }),
          entry(1, { type: 'truncation', mode: 'summary', before: 803741, after: 201214 }),
        ]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/compacting context/)).toBeInTheDocument()
    expect(screen.getByText(/context summarized/)).toBeInTheDocument()
    expect(screen.getByText(/201,214/)).toBeInTheDocument()
  })

  it('renders a degraded compaction with its reason', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: 'truncation', mode: 'degraded', before: 900, after: 400, budget: 500, reason: 'missing sections' })]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/summary failed/)).toBeInTheDocument()
    expect(screen.getByText(/missing sections/)).toBeInTheDocument()
  })

  it('renders truncation event without optional after/budget', async () => {
    render(<TraceAccordion entries={[entry(0, { type: 'truncation', before: 5000 })]} />)
    await openAccordion()
    expect(screen.getByText(/context compacted/)).toBeInTheDocument()
    expect(screen.queryByText(/budget/)).not.toBeInTheDocument()
  })
})

describe('TraceAccordion routed row', () => {
  const decision = {
    type: STREAM_EVENT_TYPE.modelSelected,
    model: 'claude-sonnet-5-5',
    reason: 'Escalated to sonnet: tool calls have been failing repeatedly (stage tool-exec; stage_router override)',
    confidence: 0.9,
    provider: 'anthropic-claude-sonnet-5-5',
    algorithm: 'stage_router',
    label: 'override',
    stage: 'tool-exec',
    path: 'switchyard',
    decisionId: 'd-1',
  } as const

  it('shows the decision reason as a sentence', async () => {
    render(<TraceAccordion entries={[entry(0, decision)]} />)
    await openAccordion()
    expect(screen.getByText(/Escalated to sonnet: tool calls have been failing repeatedly/)).toBeInTheDocument()
  })

  it('puts the label, stage and algorithm behind the disclosure', async () => {
    render(<TraceAccordion entries={[entry(0, decision)]} />)
    await openAccordion()
    const details = screen.getByTestId('routing-details')
    expect(details).not.toHaveAttribute('open')
    await userEvent.click(screen.getByText('why this model'))
    expect(details).toHaveAttribute('open')
    for (const [k, v] of [['Algorithm', 'stage_router'], ['Label', 'override'], ['Stage', 'tool-exec'], ['Path', 'switchyard']]) {
      expect(screen.getByText(k)).toBeInTheDocument()
      expect(screen.getByText(v)).toBeInTheDocument()
    }
  })

  it('shows the trace-stream routing event the same way', async () => {
    render(<TraceAccordion entries={[entry(0, { type: 'routing', model: 'glm', reason: 'Chose glm: picked by random', label: 'x' })]} />)
    await openAccordion()
    expect(screen.getByText(/Chose glm: picked by random/)).toBeInTheDocument()
    expect(screen.getByTestId('routing-details')).toBeInTheDocument()
  })
})
