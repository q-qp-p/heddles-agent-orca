/**
 * RunHistoryDetailView — read-only detail view for an archived AgentRun.
 *
 * Mirrors the visual language of RunView (header stat chips → OutputCard →
 * input section → RouterAccordion) but with NO streaming, no stop button, and
 * data sourced from PostgreSQL via getRunHistoryDetail. Tabs (Details | Output |
 * Model Router | Execution Trace) give progressive disclosure (better-layout §5).
 *
 * The Execution Trace tab renders the full trace with TraceAccordion — same as
 * the live runs tab — when the run's Redis token stream was captured at archival
 * time (traceEntries field). For older archives without an archived trace, it
 * falls back to the metadata-based ExecutionTrace timeline.
 *
 * Tabs are keyboard-navigable (better-accessibility §3), numerics use
 * tabular-nums (better-typography §11), and status chips carry redundant
 * icon+color cues (better-accessibility §9).
 */
import { useEffect, useState } from 'react'
import { getRunHistoryDetail, type RunHistoryDetail, type TraceEntry } from '../api/sse'
import { OutputCard } from './OutputCard'
import { RouterAccordion } from './RouterAccordion'
import { StatusBadge } from './StatusBadge'
import { TraceAccordion } from './TraceAccordion'
import { DESIGN } from '../lib/designSystem'
import { Icon, ICON } from '../lib/icons'

interface Props {
  runId: string
  namespace?: string
  /** Optional keyboard shortcut hint: Escape to go back. */
  onBack: () => void
  /** Navigate into a child run's detail (kept on the history tab). */
  onNavigateToRun?: (runName: string, namespace: string) => void
}

type DetailTab = 'details' | 'output' | 'routing' | 'trace'

export function RunHistoryDetailView({ runId, namespace = 'default', onBack, onNavigateToRun }: Props) {
  const [detail, setDetail] = useState<RunHistoryDetail | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [tab, setTab] = useState<DetailTab>('details')
  const [markdown, setMarkdown] = useState(false)

  useEffect(() => {
    let cancelled = false
    const ns = namespace
    setLoading(true)
    setError(null)
    getRunHistoryDetail(ns, runId)
      .then((d) => { if (!cancelled) setDetail(d) })
      .catch((e: Error) => { if (!cancelled) setError(e.message) })
      .finally(() => { if (!cancelled) setLoading(false) })

    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onBack()
    }
    window.addEventListener('keydown', onKey)
    return () => { cancelled = true; window.removeEventListener('keydown', onKey) }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runId, namespace])

  if (loading) {
    return (
      <div style={dv.root}>
        <div style={dv.skeletonLine} />
        <div style={{ ...dv.skeletonLine, width: '60%', marginTop: 6 }} />
      </div>
    )
  }

  if (error) {
    return (
      <div style={dv.empty}>
        <Icon icon={ICON.error} size={32} ariaHidden style={{ color: 'var(--ds-error)' }} />
        <div style={dv.emptyTitle}>Run not found in archive</div>
        <div style={dv.emptyBody}>{error}</div>
        <button type="button" style={dv.retryBtn} onClick={onBack}>
          <Icon icon={ICON.chevronRight} size={12} ariaHidden style={{ transform: 'rotate(180deg)' }} /> Back to history
        </button>
      </div>
    )
  }

  if (!detail) return null

  const usedCtx = detail.contextUsedTokens ?? 0
  const maxCtx = detail.maxContextTokens ?? 0
  const ctxPct = maxCtx ? Math.round(usedCtx / maxCtx * 100) : 0
  const ctxColor = ctxPct > 85 ? 'var(--ds-error)' : ctxPct > 65 ? 'var(--ds-warning)' : 'var(--ds-success)'
  const hasOutput = Boolean(detail.output)
  const hasRouting = (detail.routingDecisions?.length ?? 0) > 0
  // Full trace events are available when the run was archived while the Redis
  // token stream was still live (within 24h of completion).
  const hasFullTrace = (detail.traceEntries?.length ?? 0) > 0
  // Show the Execution Trace tab when full trace events are available, or when
  // enough metadata exists to reconstruct a summary timeline (routing decisions,
  // child runs, restarts, context usage).
  const hasTrace =
    hasFullTrace ||
    hasRouting ||
    (detail.childRunRefs?.length ?? 0) > 0 ||
    (detail.maxContextTokens ?? 0) > 0 ||
    (detail.restartCount ?? 0) > 0

  return (
    <div style={dv.root}>
      {/* Back + title */}
      <div style={dv.header}>
        <button type="button" style={dv.backBtn} onClick={onBack} aria-label="Back to run history">
          <Icon icon={ICON.chevronRight} size={12} ariaHidden style={{ transform: 'rotate(180deg)' }} />
          History
        </button>
        <div>
          <div style={dv.title}>{detail.name}</div>
          <div style={dv.subtitle}>
            {detail.agentRef} · {detail.namespace} · {detail.podName}
          </div>
        </div>
      </div>

      {/* Stat chips */}
      <div style={dv.chips}>
        <StatusBadge phase={detail.phase} />
        <Chip icon={ICON.cost} label={`$${detail.spendUSD || '0.0000'}`} title="Spend (USD)" />
        {maxCtx > 0 && (
          <Chip
            icon={ICON.tokens}
            label={`${usedCtx.toLocaleString()} / ${maxCtx.toLocaleString()}`}
            title={`Context ${ctxPct}% utilized`}
            color={ctxColor}
          />
        )}
        {detail.resolvedModel && <Chip icon={ICON.modelprovider} label={detail.resolvedModel} title="Resolved model" />}
        {detail.restartCount > 0 && <Chip icon={ICON.retry} label={`${detail.restartCount} restarts`} title="Restart count" />}
        {hasFullTrace && (
          <label style={dv.mdToggle}>
            <input
              type="checkbox"
              checked={markdown}
              onChange={(e: { target: { checked: boolean } }) => setMarkdown(e.target.checked)}
              style={{ accentColor: '#3b82f6', cursor: 'pointer', margin: 0 }}
            />
            <span style={dv.mdLabel}>Markdown</span>
          </label>
        )}
      </div>

      {/* Detail meta — cost/context/tools/mcps (better-layout §2 grouping) */}
      <div style={dv.metaGrid}>
        <MetaCard title="Cost" value={`$${detail.spendUSD || '0.0000'}`} icon={ICON.cost} />
        {maxCtx > 0 && (
          <MetaCard
            title="Context"
            value={`${usedCtx.toLocaleString()} / ${maxCtx.toLocaleString()}`}
            sublabel={`${ctxPct}% utilized`}
            icon={ICON.tokens}
          />
        )}
        <MetaCard title="Agent" value={detail.agentRef || '—'} icon={ICON.agent} />
        <MetaCard title="Model" value={detail.resolvedModel || detail.routingDecisions?.[detail.routingDecisions.length - 1]?.model || detail.routingDecisions?.[0]?.model || '—'} icon={ICON.modelprovider} />
        {detail.tools && detail.tools.length > 0 && (
          <MetaCard title="Tools" value={detail.tools.join(', ')} icon={ICON.tool} mono />
        )}
        {detail.mcpServers && detail.mcpServers.length > 0 && (
          <MetaCard title="MCP Servers" value={detail.mcpServers.join(', ')} icon={ICON.mcpserver} mono />
        )}
        {detail.childRunRefs && detail.childRunRefs.length > 0 && (
          <MetaCard title="Child Runs" value={detail.childRunRefs.join(', ')} icon={ICON.runs} mono />
        )}
      </div>

      {/* Tab bar (progressive disclosure of chat log / routing) */}
      <nav style={dv.tabs} role="tablist" aria-label="Run detail tabs">
        <TabBar tab="details" label="Details" active={tab === 'details'} onClick={() => setTab('details')} />
        <TabBar tab="output" label="Output" active={tab === 'output'} onClick={() => setTab('output')} count={detail.output ? undefined : 0} />
        <TabBar tab="routing" label="Model Router" active={tab === 'routing'} onClick={() => setTab('routing')} hasContent={hasRouting} />
        {hasTrace && <TabBar tab="trace" label="Execution Trace" active={tab === 'trace'} onClick={() => setTab('trace')} />}
      </nav>

      {tab === 'details' && (
        <div style={dv.panel} role="region" aria-label="Details">
          <Section label="Run ID">{detail.name}</Section>
          <Section label="Namespace">{detail.namespace}</Section>
          <Section label="Started">{detail.startTime ? new Date(detail.startTime).toLocaleString() : '—'}</Section>
          <Section label="Completed">{detail.completionTime ? new Date(detail.completionTime).toLocaleString() : '—'}</Section>
          <Section label="Pod">{detail.podName || '—'}</Section>
          <Section label="Spend">${detail.spendUSD || '0.0000'}</Section>
          <Section label="Restart count">{String(detail.restartCount)}</Section>
        </div>
      )}

      {(tab === 'output' || tab === 'details') && hasOutput && (
        <div style={dv.panel} role="region" aria-label="Output">
          <div style={dv.sectionLabel}>Final output</div>
          <OutputCard output={detail.output} markdown={markdown} streaming={false} feedbackRun={{ namespace, runName: runId }} />
        </div>
      )}

      {(tab === 'details' || tab === 'routing') && (
        <div style={dv.panel} role="region" aria-label="Routing">
          <div style={dv.sectionLabel}>Model routing decisions</div>
          {hasRouting ? (
            <RouterAccordion decisions={detail.routingDecisions} />
          ) : (
            <div style={dv.emptyInline}>No routing decisions recorded.</div>
          )}
        </div>
      )}

      {/* Execution Trace — when the run's Redis token stream was archived, show
          the full trace (tokens, tool calls, tool results, etc.) with the same
          TraceAccordion component used by the live runs tab. For runs without an
          archived trace (older archives, expired streams), fall back to the
          metadata-based ExecutionTrace timeline. */}
      {tab === 'trace' && (
        <div style={dv.panel} role="region" aria-label="Execution Trace">
          {hasFullTrace ? (
            <TraceAccordion entries={detail.traceEntries!} streaming={false} markdown={markdown} feedbackRun={{ namespace, runName: runId }} />
          ) : (
            <ExecutionTrace detail={detail} />
          )}
        </div>
      )}

      {/* Input prompt */}
      {detail.input && (
        <div style={dv.panel} role="region" aria-label="Input">
          <div style={dv.sectionLabel}>Input prompt</div>
          <pre style={dv.inputBox}>{detail.input}</pre>
        </div>
      )}
    </div>
  )
}

function Chip({ icon, label, title, color }: { icon: any; label: string; title?: string; color?: string }) {
  return (
    <span style={{ ...dv.chip, ...(color ? { color } : {}) }} title={title}>
      <Icon icon={icon} size={12} ariaHidden style={{ color: color ?? 'var(--ds-text-secondary)' }} />
      {label}
    </span>
  )
}

function TabBar({ tab, label, active, onClick, hasContent = true, count }: {
  tab: string; label: string; active: boolean; onClick: () => void
  hasContent?: boolean; count?: number
}) {
  return (
    <button
      type="button"
      role="tab"
      id={`tab-${tab}`}
      aria-selected={active}
      aria-controls={`panel-${tab}`}
      tabIndex={active ? 0 : -1}
      style={{ ...dv.tab, ...(active ? dv.tabActive : {}) }}
      onClick={onClick}
    >
      {label}
      {!hasContent && <span style={dv.tabDot} aria-label="No data" title="No data" />}
      {count === 0 && <span style={dv.tabCount}>0</span>}
    </button>
  )
}

// ── Execution Trace ────────────────────────────────────────────────────────────

interface TraceEvent {
  time: string | null
  icon: any
  label: string
  detail?: string
}

/**
 * Reconstructs a chronological execution trace for an archived run from the
 * durable fields the archive stores (routing decisions + child runs + spend/
 * context + restarts + timestamps). The full token/tool SSE stream is ephemeral
 * and not archived, so this assembles the durable record of what happened.
 */
function ExecutionTrace({ detail }: { detail: RunHistoryDetail }) {
  const usedCtx = detail.contextUsedTokens ?? 0
  const maxCtx = detail.maxContextTokens ?? 0
  const isFailed = detail.phase === 'Failed'

  const events: TraceEvent[] = []

  if (detail.startTime) {
    events.push({
      time: detail.startTime,
      icon: ICON.trace,
      label: 'Run started',
      detail: `Phase: ${detail.phase} · Agent: ${detail.agentRef || '—'} · Pod: ${detail.podName || '—'}`,
    })
  }

  for (const rd of detail.routingDecisions ?? []) {
    const bits: string[] = []
    if (rd.model) bits.push(rd.model)
    const src = [rd.provider, rd.strategy].filter(Boolean).join(' / ')
    if (src) bits.push(src)
    let desc = bits.join(' via ')
    if (rd.reason) desc += ` — ${rd.reason}`
    if (rd.confidence) desc += ` (${rd.confidence})`
    events.push({ time: rd.timestamp ?? null, icon: ICON.routed, label: 'Model routed', detail: desc || '—' })
  }

  for (const child of detail.childRunRefs ?? []) {
    events.push({ time: null, icon: ICON.runs, label: 'Child run spawned', detail: child })
  }

  if ((detail.restartCount ?? 0) > 0) {
    events.push({ time: null, icon: ICON.retry, label: 'Run restarted', detail: `${detail.restartCount} attempt(s)` })
  }

  if (detail.completionTime) {
    const ctxDetail = maxCtx > 0
      ? `${usedCtx.toLocaleString()} / ${maxCtx.toLocaleString()} tokens (${Math.round((usedCtx / maxCtx) * 100)}% used)`
      : usedCtx ? `${usedCtx.toLocaleString()} tokens` : '—'
    events.push({
      time: detail.completionTime,
      icon: isFailed ? ICON.error : ICON.success,
      label: isFailed ? 'Run failed' : 'Run completed',
      detail: `Spend: $${detail.spendUSD || '0.0000'} · Context: ${ctxDetail}`,
    })
  }

  // Stable sort: timestamped events ascend; timeless events keep their order at the end.
  const sorted = [...events].sort((a, b) => {
    if (!a.time && !b.time) return 0
    if (!a.time) return 1
    if (!b.time) return -1
    return new Date(a.time).getTime() - new Date(b.time).getTime()
  })

  if (sorted.length === 0) {
    return <div style={dv.emptyInline}>No recorded execution events for this run.</div>
  }

  return (
    <div style={dv.traceTimeline} role="list" aria-label="Execution trace">
      {sorted.map((e, i) => (
        <div key={i} role="listitem" style={dv.traceEvent}>
          <span style={dv.traceTime} aria-label={e.time ? new Date(e.time).toLocaleString() : 'time unknown'}>
            {e.time ? new Date(e.time).toLocaleTimeString() : '—'}
          </span>
          <span style={dv.traceDot}>
            <Icon icon={e.icon} size={12} ariaHidden style={{ color: 'var(--ds-text-secondary)' }} />
          </span>
          <div style={dv.traceBody}>
            <div style={dv.traceLabel}>{e.label}</div>
            {e.detail && <div style={dv.traceDetail}>{e.detail}</div>}
          </div>
        </div>
      ))}
    </div>
  )
}

function Section({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div style={dv.metaRow}>
      <span style={dv.metaLabel}>{label}</span>
      <span style={dv.metaValue}>{children}</span>
    </div>
  )
}

function MetaCard({ title, value, sublabel, icon, mono }: {
  title: string; value: string; sublabel?: string; icon: any; mono?: boolean
}) {
  return (
    <div style={dv.metaCard}>
      <div style={dv.metaCardHdr}>
        <Icon icon={icon} size={12} ariaHidden style={{ color: 'var(--ds-text-secondary)' }} />
        <span style={dv.metaCardTitle}>{title}</span>
      </div>
      <div style={{ ...dv.metaCardValue, fontFamily: mono ? DESIGN.font.mono : undefined, fontSize: mono ? 12 : 16 }}>
        {value}
      </div>
      {sublabel && <div style={dv.metaCardSub}>{sublabel}</div>}
    </div>
  )
}

const dv: Record<string, React.CSSProperties> = {
  root: {
    flex: 1,
    overflowY: 'auto',
    padding: `${DESIGN.space.xl} ${DESIGN.space.xxl}`,
    display: 'flex',
    flexDirection: 'column',
    gap: DESIGN.space.lg,
  },
  header: {
    display: 'flex',
    alignItems: 'center',
    gap: 16,
    marginBottom: -4,
  },
  backBtn: {
    background: 'none',
    border: 'none',
    color: 'var(--ds-accent)',
    fontSize: 12,
    fontWeight: 500,
    cursor: 'pointer',
    padding: 0,
    fontFamily: DESIGN.font.family,
    display: 'inline-flex',
    alignItems: 'center',
    gap: 4,
  },
  title: {
    fontSize: DESIGN.font.size.heading,
    fontWeight: 700,
    color: 'var(--ds-text-primary)',
    margin: 0,
    lineHeight: 1.1,
  },
  subtitle: {
    fontSize: 11,
    color: 'var(--ds-text-secondary)',
    marginTop: 2,
    fontFamily: DESIGN.font.mono,
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
    maxWidth: 'calc(100% - 4px)',
  },
  chips: {
    display: 'flex',
    flexWrap: 'wrap',
    gap: 8,
    alignItems: 'center',
  },
  chip: {
    display: 'inline-flex',
    alignItems: 'center',
    gap: 4,
    padding: '4px 10px',
    borderRadius: DESIGN.radii.full,
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    fontSize: 12,
    fontWeight: 500,
    fontVariantNumeric: 'tabular-nums',
  },
  mdToggle: {
    display: 'flex',
    alignItems: 'center',
    gap: 6,
    cursor: 'pointer',
    fontSize: 12,
  },
  mdLabel: { color: 'var(--ds-text-secondary)', fontWeight: 500, userSelect: 'none' },
  metaGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(auto-fit, minmax(160px, 1fr))',
    gap: 12,
  },
  metaCard: {
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.lg,
    padding: 14,
    display: 'flex',
    flexDirection: 'column',
    gap: 4,
    boxShadow: 'var(--ds-card-shadow)',
  },
  metaCardHdr: {
    display: 'flex',
    alignItems: 'center',
    gap: 4,
    fontSize: 10,
    fontWeight: 600,
    color: 'var(--ds-text-secondary)',
    textTransform: 'uppercase',
    letterSpacing: '0.06em',
  },
  metaCardTitle: {
    fontSize: 10,
    fontWeight: 600,
    color: 'var(--ds-text-secondary)',
    textTransform: 'uppercase',
    letterSpacing: '0.06em',
  },
  metaCardValue: {
    fontSize: 16,
    fontWeight: 700,
    color: 'var(--ds-text-primary)',
    fontVariantNumeric: 'tabular-nums',
    lineHeight: 1.2,
    wordBreak: 'break-word' as const,
  },
  metaCardSub: {
    fontSize: 10,
    color: 'var(--ds-text-muted)',
    marginTop: 2,
  },
  metaRow: {
    display: 'flex',
    justifyContent: 'space-between',
    padding: '6px 0',
    borderBottom: `1px solid var(--ds-border-light)`,
  },
  metaLabel: {
    fontSize: 11,
    color: 'var(--ds-text-secondary)',
    fontFamily: DESIGN.font.mono,
  },
  metaValue: {
    fontSize: 11,
    color: 'var(--ds-text-primary)',
    fontFamily: DESIGN.font.mono,
    maxWidth: '60%',
    textAlign: 'right' as const,
    wordBreak: 'break-word' as const,
  },
  sectionLabel: {
    fontSize: 10,
    fontWeight: 600,
    color: 'var(--ds-text-secondary)',
    textTransform: 'uppercase',
    letterSpacing: '0.06em',
    marginBottom: 8,
  },
  inputBox: {
    background: 'var(--ds-bg)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.md,
    padding: 12,
    fontSize: 12,
    fontFamily: DESIGN.font.mono,
    color: 'var(--ds-text-primary)',
    whiteSpace: 'pre-wrap' as const,
    wordBreak: 'break-word' as const,
    margin: 0,
    lineHeight: 1.5,
  },
  panel: {
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.lg,
    padding: 16,
    display: 'flex',
    flexDirection: 'column',
    gap: 12,
    boxShadow: 'var(--ds-card-shadow)',
  },
  emptyInline: {
    fontSize: 12,
    color: 'var(--ds-text-muted)',
    fontStyle: 'italic',
  },
  traceTimeline: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 10,
  },
  traceEvent: {
    display: 'flex',
    alignItems: 'flex-start',
    gap: 10,
  },
  traceTime: {
    minWidth: 90,
    fontSize: 11,
    color: 'var(--ds-text-muted)',
    fontFamily: DESIGN.font.mono,
    fontVariantNumeric: 'tabular-nums' as const,
  },
  traceDot: {
    marginTop: 1,
    flexShrink: 0,
  },
  traceBody: {
    minWidth: 0,
  },
  traceLabel: {
    fontSize: 12,
    fontWeight: 600,
    color: 'var(--ds-text-primary)',
  },
  traceDetail: {
    fontSize: 11,
    color: 'var(--ds-text-secondary)',
    fontFamily: DESIGN.font.mono,
    lineHeight: 1.4,
    wordBreak: 'break-word' as const,
    marginTop: 2,
  },
  tabs: {
    display: 'flex',
    gap: 2,
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.lg,
    padding: '0 4px',
    boxShadow: 'var(--ds-card-shadow)',
  },
  tab: {
    padding: '9px 14px',
    fontSize: 12,
    fontWeight: 500,
    color: 'var(--ds-text-secondary)',
    background: 'transparent',
    border: 'none',
    borderBottom: '2px solid transparent',
    cursor: 'pointer',
    fontFamily: DESIGN.font.family,
    transitionProperty: 'color, border-color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
    display: 'inline-flex',
    alignItems: 'center',
    gap: 6,
  },
  tabActive: {
    color: 'var(--ds-accent)',
    borderBottomColor: 'var(--ds-accent)',
    fontWeight: 600,
  },
  tabDot: {
    width: 6,
    height: 6,
    borderRadius: '50%',
    background: 'var(--ds-text-muted)',
    flexShrink: 0,
  },
  tabCount: {
    fontSize: 9,
    fontWeight: 700,
    color: 'var(--ds-text-muted)',
    background: 'rgba(148,163,184,.1)',
    padding: '0 5px',
    borderRadius: DESIGN.radii.full,
  },
  empty: {
    flex: 1,
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'center',
    justifyContent: 'center',
    gap: 12,
    color: 'var(--ds-text-muted)',
    padding: DESIGN.space.xxl,
  },
  emptyTitle: {
    fontSize: 16,
    fontWeight: 500,
    color: 'var(--ds-text-secondary)',
  },
  emptyBody: {
    fontSize: 13,
    color: 'var(--ds-text-secondary)',
    lineHeight: 1.5,
  },
  retryBtn: {
    marginTop: DESIGN.space.md,
    padding: '6px 14px',
    background: 'var(--ds-accent)',
    color: '#fff',
    border: 'none',
    borderRadius: DESIGN.radii.sm,
    fontSize: 12,
    fontWeight: 500,
    cursor: 'pointer',
    display: 'inline-flex',
    alignItems: 'center',
    gap: 4,
  },
  skeletonLine: {
    background: 'rgba(148,163,184,.15)',
    borderRadius: DESIGN.radii.md,
    height: 22,
    width: '50%',
    animation: 'aoPulse 1.5s ease infinite',
  },
}
