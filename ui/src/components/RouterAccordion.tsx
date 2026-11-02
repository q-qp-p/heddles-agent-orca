/** Collapsible model router decisions accordion. */
import { useState } from 'react'
import type { RoutingDecisionInfo } from '../api/sse'
import { DESIGN } from '../lib/designSystem'
import { Icon, ICON } from '../lib/icons'
import { DSButton } from '../lib/primitives'

interface Props {
  decisions: RoutingDecisionInfo[]
  /** Every decision the run made. When it exceeds `decisions.length` the list is
   *  the latest window only and the full history can be loaded. */
  total?: number
  /** Loads the run's full decision history; rejects when it is unavailable. */
  loadAll?: () => Promise<RoutingDecisionInfo[]>
}

export function RouterAccordion({ decisions: latest, total, loadAll }: Props) {
  const [open, setOpen] = useState(false)
  const [full, setFull] = useState<RoutingDecisionInfo[] | null>(null)
  const [loading, setLoading] = useState(false)
  const [failed, setFailed] = useState(false)
  const decisions = full ?? latest
  const truncated = full === null && total !== undefined && total > latest.length

  const showAll = async () => {
    if (!loadAll) return
    setLoading(true)
    setFailed(false)
    try {
      setFull(await loadAll())
    } catch {
      setFailed(true)
    } finally {
      setLoading(false)
    }
  }

  // The controller pre-populates decisions with all configured providers
  // (reason starts with "configured provider"). The model-router then appends
  // the actual runtime choice as a separate entry with a real routing reason.
  const chosenIdx = (() => {
    for (let i = decisions.length - 1; i >= 0; i--) {
      if (!decisions[i].reason.startsWith('configured provider')) return i
    }
    return -1
  })()
  const chosen = chosenIdx >= 0 ? decisions[chosenIdx] : null

  return (
    <div style={s.accordion}>
      <button style={s.header} onClick={() => setOpen((o) => !o)} aria-expanded={open} aria-controls="router-body">
        <div style={s.title}>
          <Icon icon={ICON.modelselector} size={14} ariaHidden={true} />
          Model Router
          {chosen ? (
            <span style={s.chosenPill}>{chosen.model}</span>
          ) : (
            <span style={s.badge}>{decisions.length} candidate{decisions.length !== 1 ? 's' : ''}</span>
          )}
        </div>
        <Icon icon={ICON.chevronRight} size={11} style={{ ...s.chevron, transform: open ? 'rotate(90deg)' : undefined }} ariaHidden={true} />
      </button>
      {open && (
        <div id="router-body" role="region" style={s.body}>
          {truncated && (
            <div style={s.truncated} role="status">
              <span>
                Showing the latest {latest.length} of {total} decisions.
                {failed && ' The full history is not available.'}
              </span>
              {loadAll && !failed && (
                <DSButton size="sm" onClick={showAll} disabled={loading}>
                  {loading ? 'Loading…' : 'Load full history'}
                </DSButton>
              )}
            </div>
          )}
          {decisions.length === 0 ? (
            <div style={s.empty}>No routing decisions recorded.</div>
          ) : (
            decisions.map((d, i) => {
              const isChosen = i === chosenIdx
              return (
                <div
                  key={i}
                  style={{
                    ...s.row,
                    ...(isChosen ? s.chosenRow : {}),
                    borderBottom: i < decisions.length - 1 ? '1px solid rgba(51,65,85,.5)' : 'none',
                  }}
                >
                  <div style={s.modelCol}>
                    <div style={s.modelName}>
                      {d.model}
                      {isChosen && <span style={s.selectedBadge}>selected</span>}
                    </div>
                    <div style={s.strategy}>{d.strategy} · {d.provider}</div>
                  </div>
                  <div style={s.confCol}>
                    <div style={s.confLabel}>{(parseFloat(d.confidence) * 100).toFixed(0)}% conf.</div>
                    <ConfBar value={parseFloat(d.confidence) || 0} />
                  </div>
                  <div style={s.reasonCol}>{d.reason}</div>
                </div>
              )
            })
          )}
        </div>
      )}
    </div>
  )
}

function ConfBar({ value }: { value: number }) {
  const pct = Math.round(value * 100)
  const fill = pct >= 80 ? '#22c55e' : pct >= 60 ? '#f59e0b' : '#ef4444'
  return (
    <div style={s.barOuter}>
      <div style={{ ...s.barInner, width: `${pct}%`, background: fill }} />
    </div>
  )
}

const s: Record<string, React.CSSProperties> = {
  accordion: {
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.lg,
    display: 'flex',
    flexDirection: 'column',
    maxHeight: '50vh',
    boxShadow: 'var(--ds-card-shadow)',
  },
  header: {
    padding: '11px 16px',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    cursor: 'pointer',
    userSelect: 'none',
    background: 'none',
    border: 'none',
    color: 'inherit',
    font: 'inherit',
    textAlign: 'left',
  },
  title: {
    fontSize: 12,
    fontWeight: 600,
    color: 'var(--ds-text-secondary)',
    display: 'flex',
    alignItems: 'center',
    gap: 8,
  },
  badge: {
    fontSize: 10,
    padding: '1px 6px',
    borderRadius: DESIGN.radii.sm,
    background: 'var(--ds-muted-bg)',
    color: 'var(--ds-text-secondary)',
    fontVariantNumeric: 'tabular-nums',
  },
  chevron: {
    fontSize: 11,
    color: 'var(--ds-text-secondary)',
    transitionProperty: 'transform',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'cubic-bezier(0.2, 0, 0, 1)',
  },
  body: {
    padding: '14px 16px',
    borderTop: `1px solid var(--ds-border)`,
    flex: 1,
    overflowY: 'auto',
    minHeight: 0,
  },
  truncated: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    gap: 12,
    marginBottom: 10,
    fontSize: 11,
    color: 'var(--ds-text-secondary)',
  },
  empty: {
    color: 'var(--ds-text-muted)',
    fontSize: 12,
    fontStyle: 'italic',
  },
  row: {
    display: 'grid',
    gridTemplateColumns: '1fr 100px 1fr',
    gap: 12,
    alignItems: 'center',
    padding: '8px 0',
  },
  modelCol: { display: 'flex', flexDirection: 'column', gap: 2 },
  modelName: { fontSize: 12, fontWeight: 600, color: 'var(--ds-text-primary)' },
  strategy: { fontSize: 11, color: 'var(--ds-text-secondary)' },
  confCol: { display: 'flex', flexDirection: 'column', gap: 4 },
  confLabel: { fontSize: 10, color: 'var(--ds-text-secondary)' },
  barOuter: {
    height: 4,
    background: 'rgba(255,255,255,.08)',
    borderRadius: 2,
    overflow: 'hidden',
  },
  barInner: {
    height: '100%',
    borderRadius: 2,
    transitionProperty: 'width',
    transitionDuration: '0.3s',
    transitionTimingFunction: 'ease',
  },
  reasonCol: { fontSize: 11, color: 'var(--ds-text-secondary)', fontStyle: 'italic' },
  chosenPill: {
    fontSize: 10,
    padding: '2px 8px',
    borderRadius: DESIGN.radii.sm,
    background: 'rgba(34,197,94,.15)',
    color: '#4ade80',
    fontWeight: 600,
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace',
    maxWidth: 260,
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
    fontVariantNumeric: 'tabular-nums',
  },
  chosenRow: {
    background: 'rgba(34,197,94,.06)',
    borderRadius: DESIGN.radii.sm,
    padding: '6px 8px',
    margin: '0 -8px',
  },
  selectedBadge: {
    marginLeft: 8,
    fontSize: 9,
    padding: '1px 5px',
    borderRadius: 3,
    background: 'rgba(34,197,94,.2)',
    color: '#4ade80',
    fontWeight: 700,
    textTransform: 'uppercase',
    letterSpacing: '0.06em',
    verticalAlign: 'middle',
  },
}
