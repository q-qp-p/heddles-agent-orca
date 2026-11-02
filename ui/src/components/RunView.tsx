/**
 * RunView — output-first detail view for a single AgentRun.
 * Shows: header stats → OutputCard (hero) → input section → TraceAccordion → RouterAccordion.
 * When child runs exist, a "Child Runs" tab is added alongside "Details".
 */
import { useEffect, useRef, useState } from 'react'
import {
  getRun,
  getRunRoutingEvents,
  routingEventToDecision,
  isSSEClientParseEventType,
  subscribeToRunStream,
  cancelRun,
  type AgentRunDetail,
  type AgentRunSummary,
  type TraceEntry,
} from '../api/sse'
import { STREAM_EVENT_TYPE } from '../contracts/events'
import { StatusBadge } from './StatusBadge'
import { OutputCard } from './OutputCard'
import { TraceAccordion } from './TraceAccordion'
import { RouterAccordion } from './RouterAccordion'
import type { ResourceSelection } from './ResourceList'
import { PHASE_COLOR } from '../lib/phaseColors'
import { DESIGN } from '../lib/designSystem'
import { Icon, ICON } from '../lib/icons'

interface Props {
  runId: string
  namespace?: string
  /** Ancestor navigation stack — entries before the current run. */
  breadcrumbs?: ResourceSelection[]
  /** Whether this run was navigated to from the home dashboard. */
  cameFromHome?: boolean
  /** Jump back to breadcrumb at the given index. */
  onNavigateUp?: (index: number) => void
  /** Navigate into a child run. */
  onNavigateToRun?: (runName: string, namespace: string) => void
}



export function RunView({
  runId,
  namespace = 'default',
  breadcrumbs = [],
  cameFromHome = false,
  onNavigateUp,
  onNavigateToRun,
}: Props) {
  const [detail, setDetail] = useState<AgentRunDetail | null>(null)
  const [entries, setEntries] = useState<TraceEntry[]>([])
  const [streamOutput, setStreamOutput] = useState('')
  const [markdown, setMarkdown] = useState(false)
  const [viewTab, setViewTab] = useState<'details' | 'trace' | 'children'>('details')
  const [childDetails, setChildDetails] = useState<AgentRunSummary[]>([])
  const counter = useRef(0)
  const childUnsubsRef = useRef<Array<() => void>>([])
  const subscribedChildRefsRef = useRef<Set<string>>(new Set())

  useEffect(() => {
    childUnsubsRef.current.forEach((fn) => fn())
    childUnsubsRef.current = []
    subscribedChildRefsRef.current.clear()
    setDetail(null)
    setEntries([])
    setStreamOutput('')
    setViewTab('details')
    setChildDetails([])
    counter.current = 0
    getRun(runId, namespace).then(setDetail).catch(() => {})
  }, [runId, namespace])

  useEffect(() => {
    const unsub = subscribeToRunStream(runId, (event) => {
      if (event.type !== STREAM_EVENT_TYPE.token) {
        // Suppress pre-populated candidate entries — only show the actual runtime selection.
        const isCandidate = event.type === STREAM_EVENT_TYPE.modelSelected && event.reason.includes('configured provider')
        // Trace-stream `routing` entries duplicate the CRD-backed `modelSelected` event
        // (the model-router records each decision as AgentRun status); keep one row.
        if (!isCandidate && event.type !== 'routing') {
          // Group consecutive `thought` (reasoning) deltas into a single expandable
          // entry so each thinking block renders as one collapsible panel that accumulates
          // progressively, rather than one trace row per reasoning delta.
          if (event.type === 'thought') {
            setEntries((prev: TraceEntry[]) => {
              const last = prev[prev.length - 1]
              if (last && last.event.type === 'thought') {
                return [
                  ...prev.slice(0, -1),
                  {
                    ...last,
                    event: { ...last.event, content: last.event.content + event.content },
                  },
                ]
              }
              return [...prev, { id: counter.current++, event, ts: new Date().toISOString() }]
            })
          } else {
            setEntries((prev: TraceEntry[]) => [...prev, { id: counter.current++, event, ts: new Date().toISOString() }])
          }
        }
      }
      if (event.type === STREAM_EVENT_TYPE.finalOutput || event.type === STREAM_EVENT_TYPE.done) {
        // An explicit _done event carries the agent's output; an auto terminal
        // `done` event (schema-derived completion, no _done tool) may omit output —
        // in that case the final text was already streamed as tokens, so don't
        // overwrite it with undefined.
        if (event.output != null) {
          setStreamOutput(event.output)
        }
        getRun(runId, namespace).then(setDetail).catch(() => {})
      } else if (event.type === STREAM_EVENT_TYPE.error || event.type === STREAM_EVENT_TYPE.fail) {
        getRun(runId, namespace).then(setDetail).catch(() => {})
      } else if (
        event.type === STREAM_EVENT_TYPE.agentEvent &&
        isSSEClientParseEventType(event.eventType)
      ) {
        getRun(runId, namespace).then(setDetail).catch(() => {})
      }
    }, namespace)
    return unsub
  }, [runId, namespace])

  // Fetch child run summaries whenever childRunRefs change.
  useEffect(() => {
    const refs = detail?.childRunRefs
    if (!refs || refs.length === 0) { setChildDetails([]); return }

    let cancelled = false
    Promise.all(
      refs.map((name: string) =>
        getRun(name, namespace)
          .then((d): AgentRunSummary => ({
            name: d.name,
            namespace: d.namespace,
            agentRef: d.agentRef,
            phase: d.phase as AgentRunSummary['phase'],
            spendUSD: d.spendUSD,
            restartCount: d.restartCount,
            startTime: d.startTime,
            completionTime: d.completionTime,
          }))
          .catch((): null => null),
      ),
    ).then((results) => {
      if (cancelled) return
      setChildDetails(results.filter((r: AgentRunSummary | null): r is AgentRunSummary => r !== null))
    })

    return () => { cancelled = true }
  }, [detail?.childRunRefs, namespace])

  // Subscribe to each child run's stream to bubble up their tool calls.
  useEffect(() => {
    const refs = detail?.childRunRefs ?? []
    const newRefs = refs.filter((r) => !subscribedChildRefsRef.current.has(r))
    if (newRefs.length === 0) return
    for (const childName of newRefs) {
      subscribedChildRefsRef.current.add(childName)
      const unsub = subscribeToRunStream(childName, (event) => {
        if (event.type === STREAM_EVENT_TYPE.token) return
        if (event.type === STREAM_EVENT_TYPE.error && event.message === 'Stream connection lost') return
        const isCandidate = event.type === STREAM_EVENT_TYPE.modelSelected && event.reason.includes('configured provider')
        if (!isCandidate && event.type !== 'routing') {
          setEntries((prev) => [...prev, { id: counter.current++, event, ts: new Date().toISOString(), childRunName: childName }])
        }
      }, namespace)
      childUnsubsRef.current.push(unsub)
    }
  }, [detail?.childRunRefs, namespace])

  const phase = detail?.phase ?? 'Pending'
  const isRunning = phase === 'Running' || phase === 'Pending'
  const isFailed = phase === 'Failed'
  const hasChildren = (detail?.childRunRefs?.length ?? 0) > 0

  const output = streamOutput || detail?.output || ''

  const elapsed = (() => {
    if (!detail?.startTime) return null
    const end = detail.completionTime ? new Date(detail.completionTime) : new Date()
    return `${Math.round((end.getTime() - new Date(detail.startTime).getTime()) / 1000)}s`
  })()

  const agentRelTime = (() => {
    if (!detail?.startTime) return ''
    const diff = Math.round((Date.now() - new Date(detail.startTime).getTime()) / 1000)
    if (diff < 60) return `${diff}s ago`
    if (diff < 3600) return `${Math.round(diff / 60)}m ago`
    return `${Math.round(diff / 3600)}h ago`
  })()

  return (
    <div style={s.root}>
      {/* Breadcrumb trail */}
      {cameFromHome && (
        <div style={s.breadcrumb}>
          <button type="button" style={s.breadcrumbBtn} onClick={() => onNavigateUp?.(0)}>
            <Icon icon={ICON.home} size={12} ariaHidden={true} /> Home
          </button>
          <Icon icon={ICON.chevronRight} size={11} style={s.breadcrumbSepIcon} ariaHidden={true} />
          <span style={s.breadcrumbCurrent}>{runId}</span>
        </div>
      )}
      {!cameFromHome && breadcrumbs.length > 0 && (
        <div style={s.breadcrumb}>
          {breadcrumbs.map((crumb, i) => (
            <span key={i} style={s.breadcrumbItem}>
              {i > 0 && <Icon icon={ICON.chevronRight} size={11} style={s.breadcrumbSepIcon} ariaHidden={true} />}
              <button type="button" style={s.breadcrumbBtn} onClick={() => onNavigateUp?.(i)}>
                {crumb.name}
              </button>
            </span>
          ))}
          <Icon icon={ICON.chevronRight} size={11} style={s.breadcrumbSepIcon} ariaHidden={true} />
          <span style={s.breadcrumbCurrent}>{runId}</span>
        </div>
      )}

      {/* Header */}
      <div style={s.header}>
        <div>
          <div style={s.title}>{runId}</div>
          <div style={s.subtitle}>
            {detail?.agentRef ?? '…'} · {agentRelTime} · {namespace}
          </div>
        </div>
        <div style={s.stats}>
          <StatusBadge phase={phase} />
          {elapsed && (
            <div style={s.chip}><Icon icon={ICON.timer} size={12} /> <span style={s.chipVal}>{elapsed}</span></div>
          )}
          {isRunning && (
            <div style={s.liveChip}>
              <span style={s.liveDot} />
              <span style={s.liveText}>Live</span>
            </div>
          )}
          <div style={s.chip}><Icon icon={ICON.cost} size={12} /> <span style={s.chipVal}>${detail?.spendUSD ?? '0.0000'}</span></div>
          {isRunning && (
            <button
              type="button"
              style={s.stopBtn}
              onClick={() => {
                if (window.confirm(`Cancel run "${runId}"? This cannot be undone.`)) {
                  cancelRun(runId, namespace).catch((e) => alert(`Failed to cancel: ${e.message}`))
                }
              }}
              aria-label={`Cancel run ${runId}`}
              title="Cancel run"
            >
              <Icon icon={ICON.stop} size={12} ariaHidden={true} /> Stop
            </button>
          )}
          {(detail?.restartCount ?? 0) > 0 && (
            <div style={s.chip}><Icon icon={ICON.retry} size={12} /> <span style={s.chipVal}>{detail!.restartCount} retries</span></div>
          )}
          {(detail?.maxContextTokens ?? 0) > 0 && (
            <div style={s.chip}><Icon icon={ICON.tokens} size={12} /> <span style={s.chipVal}>
              {detail!.contextUsedTokens?.toLocaleString() ?? 0} / {detail!.maxContextTokens?.toLocaleString() ?? '—'} tokens
            </span></div>
          )}
          <label style={s.mdToggle}>
            <input
              type="checkbox"
              checked={markdown}
              onChange={(e: { target: { checked: boolean } }) => setMarkdown(e.target.checked)}
              style={{ accentColor: '#3b82f6', cursor: 'pointer', margin: 0 }}
            />
            <span style={s.mdLabel}>Markdown</span>
          </label>
        </div>
      </div>

      {/* Tab bar — always visible so the Execution Trace is reachable even when
          the run has no child runs. */}
      <div role="tablist" style={s.tabBar}>
        <button
          type="button"
          role="tab"
          aria-selected={viewTab === 'details'}
          aria-controls="details-panel"
          tabIndex={viewTab === 'details' ? 0 : -1}
          style={{ ...s.tab, ...(viewTab === 'details' ? s.tabActive : {}) }}
          onClick={() => setViewTab('details')}
        >
          Details
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={viewTab === 'trace'}
          aria-controls="trace-panel"
          tabIndex={viewTab === 'trace' ? 0 : -1}
          style={{ ...s.tab, ...(viewTab === 'trace' ? s.tabActive : {}) }}
          onClick={() => setViewTab('trace')}
        >
          Execution Trace
        </button>
        {hasChildren && (
          <button
            type="button"
            role="tab"
            aria-selected={viewTab === 'children'}
            aria-controls="children-panel"
            tabIndex={viewTab === 'children' ? 0 : -1}
            style={{ ...s.tab, ...(viewTab === 'children' ? s.tabActive : {}) }}
            onClick={() => setViewTab('children')}
          >
            Child Runs ({detail?.childRunRefs?.length ?? 0})
          </button>
        )}
      </div>

      {viewTab === 'children' ? (
        <div id="children-panel" role="tabpanel" style={s.childList}>
          {childDetails.length === 0 && (
            <div style={s.empty}>Loading child runs…</div>
          )}
          {childDetails.map((run: AgentRunSummary) => (
            <button
              key={run.name}
              type="button"
              style={s.runCard}
              onClick={() => onNavigateToRun?.(run.name, run.namespace)}
            >
              <div style={s.runCardHeader}>
                <span style={{ ...s.runDot, background: PHASE_COLOR[run.phase] ?? '#6b7280' }} />
                <span style={s.runCardName}>{run.name}</span>
                <span style={s.runCardPhase}>{run.phase}</span>
                <span style={{ flex: 1 }} />
                {parseFloat(run.spendUSD) > 0 && (
                  <span style={s.runCardCost}>${run.spendUSD}</span>
                )}
              </div>
              <div style={s.runCardMeta}>
                {run.agentRef}
                {run.startTime && ` · ${new Date(run.startTime).toLocaleString()}`}
              </div>
            </button>
          ))}
        </div>
      ) : viewTab === 'trace' ? (
        <div id="trace-panel" role="tabpanel" style={{ flex: 1, overflowY: 'auto' }}>
          <TraceAccordion entries={entries} streaming={isRunning} markdown={markdown} />
          {detail && detail.routingDecisions.length > 0 && (
            <RouterAccordion
              decisions={detail.routingDecisions}
              total={detail.routingDecisionsTotal}
              loadAll={() => getRunRoutingEvents(detail.name, detail.namespace).then((evs) => evs.map(routingEventToDecision))}
            />
          )}
        </div>
      ) : (
        <div id="details-panel" role="tabpanel" style={{ flex: 1, display: 'flex', flexDirection: 'column', gap: 16 }}>
          {/* Error banner for failed runs */}
          {isFailed && detail?.lastRestartReason && (
            <div style={s.errorCard}>
              <div style={s.errorTitle}><Icon icon={ICON.warning} size={12} ariaHidden={true} /> Run Failed</div>
              <div style={s.errorBody}>{detail.lastRestartReason}</div>
            </div>
          )}

          {/* Clarify question banner */}
          {phase === 'WaitingForInput' && detail?.clarifyQuestion && (
            <div style={s.clarifyCard}>
              <div style={s.clarifyTitle}>Waiting for human input</div>
              <div style={s.clarifyBody}>{detail.clarifyQuestion}</div>
            </div>
          )}

          {/* Output card */}
          {(output || isRunning) && (
            <OutputCard
              output={output || ''}
              streaming={isRunning && !output}
              markdown={markdown}
            />
          )}

          {/* Input section */}
          {detail?.input && (
            <div style={s.inputSection}>
              <div style={s.inputLabel}>Input prompt</div>
              <div style={s.inputText}>{detail.input}</div>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

const s: Record<string, React.CSSProperties> = {
  root: {
    flex: 1,
    overflowY: 'auto',
    padding: `${DESIGN.space.xl} ${DESIGN.space.xxl}`,
    display: 'flex',
    flexDirection: 'column',
    gap: DESIGN.space.xl,
  },
  breadcrumb: {
    display: 'flex',
    alignItems: 'center',
    gap: 4,
    flexWrap: 'wrap',
    marginBottom: -8,
  },
  breadcrumbItem: {
    display: 'flex',
    alignItems: 'center',
    gap: 4,
  },
  breadcrumbBtn: {
    background: 'none',
    border: 'none',
    color: 'var(--ds-accent)',
    fontSize: 12,
    fontWeight: 500,
    cursor: 'pointer',
    padding: 0,
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace',
    transitionProperty: 'color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  breadcrumbSep: {
    fontSize: 12,
    color: 'var(--ds-border)',
  },
  breadcrumbSepIcon: {
    fontSize: 11,
    color: 'var(--ds-text-muted)',
    flexShrink: 0,
  },
  liveChip: {
    display: 'inline-flex',
    alignItems: 'center',
    gap: 4,
    padding: '3px 8px',
    borderRadius: DESIGN.radii.full,
    background: 'rgba(34,197,94,.12)',
    color: 'var(--ds-success)',
    fontSize: 11,
    fontWeight: 600,
  },
  liveDot: {
    width: 6,
    height: 6,
    borderRadius: '50%',
    background: 'var(--ds-success)',
    animation: 'aoPulse 1.5s ease infinite',
    willChange: 'transform, opacity',
  },
  liveText: {
    fontSize: 10,
    fontVariantNumeric: 'tabular-nums',
  },
  breadcrumbCurrent: {
    fontSize: 12,
    color: 'var(--ds-text-muted)',
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace',
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
    maxWidth: 260,
  },
  header: {
    display: 'flex',
    alignItems: 'flex-start',
    justifyContent: 'space-between',
    flexWrap: 'wrap',
    gap: 12,
  },
  title: {
    fontSize: DESIGN.font.size.heading,
    fontWeight: 700,
    color: 'var(--ds-text-primary)',
    lineHeight: 1.1,
    textWrap: 'balance' as const,
  },
  subtitle: {
    fontSize: 11,
    color: 'var(--ds-text-secondary)',
    marginTop: 4,
  },
  stats: {
    display: 'flex',
    alignItems: 'center',
    gap: 10,
    flexWrap: 'wrap',
  },
  chip: {
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.sm,
    padding: '5px 10px',
    display: 'flex',
    alignItems: 'center',
    gap: 6,
    fontSize: 12,
    color: 'var(--ds-text-secondary)',
    fontVariantNumeric: 'tabular-nums',
  },
  chipVal: { color: 'var(--ds-text-primary)', fontWeight: 500 },
  mdToggle: {
    display: 'flex',
    alignItems: 'center',
    gap: 6,
    cursor: 'pointer',
    fontSize: 12,
  },
  mdLabel: { color: 'var(--ds-text-secondary)', fontWeight: 500, userSelect: 'none' },
  tabBar: {
    display: 'flex',
    borderBottom: `1px solid var(--ds-border)`,
    marginTop: -8,
    gap: 0,
  },
  tab: {
    padding: '8px 16px',
    fontSize: 13,
    fontWeight: 500,
    color: 'var(--ds-text-muted)',
    background: 'none',
    border: 'none',
    borderBottom: '2px solid transparent',
    cursor: 'pointer',
    transitionProperty: 'color, border-color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  tabActive: {
    color: 'var(--ds-accent)',
    borderBottomColor: 'var(--ds-accent)',
  },
  childList: {
    display: 'flex',
    flexDirection: 'column',
    gap: 6,
  },
  runCard: {
    padding: '10px 14px',
    borderRadius: DESIGN.radii.lg,
    border: `1px solid var(--ds-border)`,
    background: 'var(--ds-surface)',
    cursor: 'pointer',
    display: 'flex',
    flexDirection: 'column',
    gap: 4,
    transitionProperty: 'background-color, border-color, box-shadow',
    transitionDuration: '0.12s',
    transitionTimingFunction: 'ease',
    boxShadow: 'var(--ds-card-shadow)',
  },
  runCardHeader: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
  },
  runDot: {
    width: 7,
    height: 7,
    borderRadius: '50%',
    flexShrink: 0,
  },
  runCardName: {
    fontSize: 12,
    fontWeight: 600,
    color: 'var(--ds-text-primary)',
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace',
  },
  runCardPhase: {
    fontSize: 11,
    color: 'var(--ds-text-muted)',
  },
  runCardCost: {
    fontSize: 11,
    fontWeight: 600,
    color: 'var(--ds-success)',
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace',
    fontVariantNumeric: 'tabular-nums',
  },
  runCardMeta: {
    fontSize: 11,
    color: 'var(--ds-text-muted)',
    paddingLeft: 15,
  },
  empty: {
    color: 'var(--ds-text-muted)',
    fontSize: 13,
    padding: '16px 0',
  },
  errorCard: {
    background: 'var(--ds-trace-error-bg)',
    border: '1px solid var(--ds-trace-warning-border)',
    borderRadius: DESIGN.radii.lg,
    padding: '14px 16px',
  },
  errorTitle: {
    fontSize: 12,
    fontWeight: 600,
    color: 'var(--ds-error)',
    marginBottom: 8,
    display: 'flex',
    alignItems: 'center',
    gap: 6,
  },
  errorBody: {
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace',
    fontSize: 12,
    color: '#fca5a5',
    lineHeight: 1.5,
    whiteSpace: 'pre-wrap',
    fontVariantNumeric: 'tabular-nums',
  },
  inputSection: {
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.lg,
    padding: '12px 16px',
  },
  inputLabel: {
    fontSize: 11,
    fontWeight: 600,
    textTransform: 'uppercase',
    letterSpacing: '0.08em',
    color: 'var(--ds-text-secondary)',
    marginBottom: 8,
  },
  inputText: {
    fontSize: 13,
    color: 'var(--ds-text-secondary)',
    fontStyle: 'italic',
    lineHeight: 1.5,
    whiteSpace: 'pre-wrap',
    wordBreak: 'break-word',
  },
  clarifyCard: {
    background: 'var(--ds-trace-warning-bg)',
    border: '1px solid var(--ds-warning-border)',
    borderRadius: DESIGN.radii.lg,
    padding: '14px 16px',
  },
  clarifyTitle: {
    fontSize: 12,
    fontWeight: 600,
    color: 'var(--ds-warning)',
    marginBottom: 8,
  },
  clarifyBody: {
    fontSize: 13,
    color: '#fcd34d',
    lineHeight: 1.5,
    whiteSpace: 'pre-wrap',
    wordBreak: 'break-word',
  },
  stopBtn: {
    padding: '4px 10px',
    background: 'rgba(239,68,68,.12)',
    color: 'var(--ds-error)',
    border: `1px solid var(--ds-error-border)`,
    borderRadius: DESIGN.radii.sm,
    fontSize: 11,
    fontWeight: 600,
    cursor: 'pointer',
    display: 'inline-flex',
    alignItems: 'center',
    gap: 4,
    transitionProperty: 'background-color',
    transitionDuration: '0.15s',
  },
}
