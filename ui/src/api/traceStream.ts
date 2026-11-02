/** SSE trace stream types and subscription — shared with model-router / uiapi event shapes. */

import { STREAM_EVENT_TYPE } from '../contracts/events'

// ── Types ────────────────────────────────────────────────────────────────────

/**
 * SSE `type` discriminants for trace events.
 * These string values must stay in sync with the Go constants in internal/router.
 */
export const TRACE_EVENT_TYPE = {
  ...STREAM_EVENT_TYPE,
} as const

export type TraceEvent =
  | { type: typeof TRACE_EVENT_TYPE.thought; content: string }
  | { type: typeof STREAM_EVENT_TYPE.token; content: string }
  | { type: typeof STREAM_EVENT_TYPE.toolCall; name: string; arguments: string; backendType?: string; backendRef?: string }
  | { type: typeof STREAM_EVENT_TYPE.toolResult; name: string; result: string; appUrl?: string; toolArgs?: string; toolResult?: string; backendType?: string; backendRef?: string; durationMs?: number }
  | {
      type: typeof STREAM_EVENT_TYPE.modelSelected
      model: string
      // One plain sentence saying what was chosen and why. The fields below back it.
      reason: string
      confidence: number
      provider?: string
      strategy?: string
      decisionId?: string
      path?: string
      stage?: string
      algorithm?: string
      label?: string
      candidates?: string[]
    }
  | { type: typeof STREAM_EVENT_TYPE.finalOutput; output: string }
  | { type: typeof STREAM_EVENT_TYPE.error; message: string }
  | { type: typeof STREAM_EVENT_TYPE.clarify; question: string }
  | { type: typeof STREAM_EVENT_TYPE.done; output?: string; finish_reason?: string }
  | { type: typeof STREAM_EVENT_TYPE.fail; reason: string }
  | { type: typeof STREAM_EVENT_TYPE.agentEvent; eventType: string; message: string }
  | { type: typeof STREAM_EVENT_TYPE.placeholder; message: string }
  | { type: 'ragResult'; name: string; query: string; results: number; collection: string }
  | { type: 'mcpDiscovery'; server: string; tools: number }
  | { type: 'guardrail'; action: string; reason: string }
  | { type: 'providerFallback'; from: string; to: string; reason: string; exhausted?: boolean }
  | {
      type: 'routing'
      model: string
      provider?: string
      strategy?: string
      // One plain sentence saying what was chosen and why. The fields below back it.
      reason?: string
      confidence?: number
      decisionId?: string
      path?: string
      stage?: string
      algorithm?: string
      label?: string
      candidates?: string[]
    }
  | { type: 'hindsight'; op: 'bank' | 'recall' | 'retain' | string; bankID?: string; failed?: boolean; reason?: string }
  // A network decision by the sandbox supervisor (G4): a connection (net) or a request on an
  // allowed connection (http), allowed or denied by the sandbox policy.
  | {
      type: 'egress'
      kind: 'net' | 'http' | string
      action: 'allowed' | 'denied' | string
      binary?: string
      destination?: string
      method?: string
      url?: string
      policy?: string
      engine?: string
      reason?: string
      raw: string
    }
  // The run's spend against its budget (AgentRun.spec.budget): a warning near
  // the cap, a pause at the cap, and how the pause ended.
  | {
      type: 'budget'
      state: 'warning' | 'exceeded' | 'resumed' | 'rejected' | string
      spentUSD: string
      capUSD: string
    }
  // A tool call held for an approver (Tool.spec.sideEffect + GuardrailPolicy
  // toolApproval): requested, then approved, rejected, expired, denied by the
  // policy, or unavailable when the approval service could not be reached.
  | {
      type: 'toolApproval'
      state: 'requested' | 'approved' | 'rejected' | 'expired' | 'denied' | 'withdrawn' | 'unavailable' | string
      tool: string
      sideEffect?: string
      by?: string
      approvalId?: string
      reason?: string
    }
  | {
      type: 'truncation'
      provider?: string
      before: number
      after?: number
      budget?: number
      /** How it was compacted (#47): summary | truncate | degraded | skipped; absent = pre-send truncation. */
      mode?: string
      reason?: string
    }
  | { type: 'compaction.started'; provider?: string; mode?: string; before: number; trigger?: number }

/** True when the server typically closes the SSE connection after this event (uiapi finalOutput, or an error/clarify/fail path). */
export function isTerminalTraceEvent(event: TraceEvent): boolean {
  switch (event.type) {
    case STREAM_EVENT_TYPE.finalOutput:
    case STREAM_EVENT_TYPE.error:
    case STREAM_EVENT_TYPE.clarify:
    case STREAM_EVENT_TYPE.fail:
      return true
    default:
      return false
  }
}

export type TraceEventHandler = (event: TraceEvent) => void

/** A timestamped trace event entry, used in trace display components. */
export interface TraceEntry {
  id: number
  event: TraceEvent
  ts: string
  /** Set when this entry originated from a child run's stream. */
  childRunName?: string
}

/** Synthetic `agent_event` kinds emitted by the UI client when SSE JSON fails validation (§5 guardrails). */
export const SSE_CLIENT_PARSE_EVENT_TYPE = {
  malformedJson: 'sse_malformed_json',
  missingOrInvalidType: 'sse_missing_or_invalid_type',
} as const

/** True when `eventType` is reserved for client-side SSE parse recovery (`sse_*`). */
export function isSSEClientParseEventType(eventType: string): boolean {
  return eventType.startsWith('sse_')
}

const MAX_SSE_PAYLOAD_PREVIEW = 512

function truncateTracePayload(s: string, max: number): string {
  if (s.length <= max) return s
  return s.slice(0, max) + '…'
}

/**
 * Subscribe to a run's SSE event stream.
 * Returns a cleanup function to close the connection.
 */
export function subscribeToRunStream(runId: string, onEvent: TraceEventHandler, namespace: string): () => void {
  const url = `/api/runs/${encodeURIComponent(namespace)}/${runId}/stream`
  const source = new EventSource(url)
  let terminated = false

  source.onmessage = (e) => {
    const raw = typeof e.data === 'string' ? e.data : String(e.data)
    let parsed: unknown
    try {
      parsed = JSON.parse(raw)
    } catch {
      onEvent({
        type: STREAM_EVENT_TYPE.agentEvent,
        eventType: SSE_CLIENT_PARSE_EVENT_TYPE.malformedJson,
        message: truncateTracePayload(raw, MAX_SSE_PAYLOAD_PREVIEW),
      })
      return
    }
    if (
      parsed === null ||
      typeof parsed !== 'object' ||
      Array.isArray(parsed) ||
      !('type' in parsed) ||
      typeof (parsed as { type?: unknown }).type !== 'string'
    ) {
      onEvent({
        type: STREAM_EVENT_TYPE.agentEvent,
        eventType: SSE_CLIENT_PARSE_EVENT_TYPE.missingOrInvalidType,
        message: truncateTracePayload(raw, MAX_SSE_PAYLOAD_PREVIEW),
      })
      return
    }
    const event = parsed as TraceEvent
    if (isTerminalTraceEvent(event)) {
      terminated = true
    }
    onEvent(event)
  }

  source.onerror = () => {
    // Only report as an error if the stream closed unexpectedly.
    // Server closes the connection after final_output/error — that's normal.
    if (!terminated) {
      onEvent({ type: 'error', message: 'Stream connection lost' })
    }
    source.close()
  }

  return () => source.close()
}
