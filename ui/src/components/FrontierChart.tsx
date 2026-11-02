/**
 * FrontierChart — cost against quality loss for the thresholds a benchmark
 * tried. Each point is one `base_threshold`: how much a call would have cost on
 * average, and how often it would have gone to the model whose answer lost.
 * Lower and to the left is better. The thresholds nothing beats on both are
 * shown; the one in use and the one proposed are marked by shape and labelled,
 * not by colour alone, and a table view carries the same numbers.
 */
import { useState } from 'react'
import type { FrontierPoint } from '../api/sse'
import { ds } from '../lib/designSystem'

interface Props {
  points: FrontierPoint[]
  /** The threshold the recommendation proposes. */
  proposed?: number
}

const W = 360
const H = 200
const M = { top: 14, right: 16, bottom: 34, left: 48 }
const TICKS = 3

export function FrontierChart({ points, proposed }: Props) {
  const [hover, setHover] = useState<number | null>(null)
  const [asTable, setAsTable] = useState(false)
  if (points.length === 0) return null

  const costs = points.map((p) => p.costUsd)
  const losses = points.map((p) => p.loss)
  const [x0, x1] = pad(Math.min(...costs), Math.max(...costs))
  const [y0, y1] = pad(0, Math.max(...losses), true)
  const x = (v: number) => M.left + ((v - x0) / (x1 - x0)) * (W - M.left - M.right)
  const y = (v: number) => H - M.bottom - ((v - y0) / (y1 - y0)) * (H - M.top - M.bottom)
  const isProposed = (p: FrontierPoint) => proposed !== undefined && Math.abs(p.threshold - proposed) < 1e-9

  if (asTable) {
    return (
      <div>
        <button type="button" style={s.toggle} onClick={() => setAsTable(false)}>Show as chart</button>
        <table style={s.table} aria-label="Cost and quality loss by threshold">
          <thead>
            <tr><th style={s.th}>Threshold</th><th style={s.th}>Cost per call</th><th style={s.th}>Routed to the losing model</th><th style={s.th} /></tr>
          </thead>
          <tbody>
            {points.map((p) => (
              <tr key={p.threshold}>
                <td style={s.td}>{p.threshold.toFixed(2)}</td>
                <td style={s.td}>${p.costUsd.toFixed(5)}</td>
                <td style={s.td}>{(p.loss * 100).toFixed(1)}%</td>
                <td style={s.td}>{p.current ? 'in use' : isProposed(p) ? 'proposed' : ''}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    )
  }

  const hovered = hover !== null ? points[hover] : null
  return (
    <div style={{ position: 'relative', maxWidth: W }}>
      <svg
        viewBox={`0 0 ${W} ${H}`}
        width="100%"
        role="img"
        aria-label={`Cost against quality loss for ${points.length} thresholds`}
        onMouseLeave={() => setHover(null)}
      >
        {ticks(y0, y1).map((t) => (
          <g key={`y${t}`}>
            <line x1={M.left} x2={W - M.right} y1={y(t)} y2={y(t)} stroke={ds.border} strokeWidth={1} />
            <text x={M.left - 6} y={y(t) + 3} textAnchor="end" fontSize={10} fill={ds.textMuted}>{(t * 100).toFixed(0)}%</text>
          </g>
        ))}
        {ticks(x0, x1).map((t) => (
          <text key={`x${t}`} x={x(t)} y={H - M.bottom + 14} textAnchor="middle" fontSize={10} fill={ds.textMuted}>${t.toFixed(4)}</text>
        ))}
        <text x={(M.left + W - M.right) / 2} y={H - 4} textAnchor="middle" fontSize={10} fill={ds.textSecondary}>cost per call</text>
        <text x={10} y={(M.top + H - M.bottom) / 2} textAnchor="middle" fontSize={10} fill={ds.textSecondary}
          transform={`rotate(-90 10 ${(M.top + H - M.bottom) / 2})`}>routed to the losing model</text>

        {points.map((p, i) => {
          const cx = x(p.costUsd)
          const cy = y(p.loss)
          const label = p.current ? `in use ${p.threshold.toFixed(2)}` : isProposed(p) ? `proposed ${p.threshold.toFixed(2)}` : null
          return (
            <g key={p.threshold} onMouseEnter={() => setHover(i)}>
              {/* A generous target around the mark, so it is easy to hover. */}
              <circle cx={cx} cy={cy} r={12} fill="transparent" />
              {p.current ? (
                // The threshold in use is a diamond.
                <polygon points={`${cx},${cy - 6} ${cx + 6},${cy} ${cx},${cy + 6} ${cx - 6},${cy}`} fill={ds.surface} stroke={ds.textPrimary} strokeWidth={2} />
              ) : isProposed(p) ? (
                // The proposal is a filled circle.
                <circle cx={cx} cy={cy} r={6} fill={ds.accent} stroke={ds.surface} strokeWidth={2} />
              ) : (
                <circle cx={cx} cy={cy} r={4} fill={ds.textMuted} stroke={ds.surface} strokeWidth={2} />
              )}
              {label && (
                <text x={cx + 9} y={cy - 8} fontSize={10} fontWeight={600} fill={ds.textPrimary}>{label}</text>
              )}
            </g>
          )
        })}
      </svg>
      {hovered && (
        <div role="tooltip" style={{ ...s.tooltip, left: `${(x(hovered.costUsd) / W) * 100}%`, top: `${(y(hovered.loss) / H) * 100}%` }}>
          <strong>{hovered.threshold.toFixed(2)}</strong>
          {hovered.current ? ' (in use)' : isProposed(hovered) ? ' (proposed)' : ''}
          <div>${hovered.costUsd.toFixed(5)} per call</div>
          <div>{(hovered.loss * 100).toFixed(1)}% routed to the losing model</div>
        </div>
      )}
      <button type="button" style={s.toggle} onClick={() => setAsTable(true)}>Show as table</button>
    </div>
  )
}

/** Widens a range a little so marks do not sit on the frame; a flat range gets some height. */
function pad(lo: number, hi: number, fromZero = false): [number, number] {
  const span = hi - lo || Math.max(Math.abs(hi), 1e-6)
  return [fromZero ? 0 : lo - span * 0.1, hi + span * 0.15]
}

/** A few round-ish values across a range, for gridlines and labels. */
function ticks(lo: number, hi: number): number[] {
  return Array.from({ length: TICKS }, (_, i) => lo + ((hi - lo) * i) / (TICKS - 1))
}

const s: Record<string, React.CSSProperties> = {
  toggle: { marginTop: 4, padding: 0, background: 'none', border: 'none', color: ds.accent, fontSize: 12, cursor: 'pointer' },
  tooltip: {
    position: 'absolute', transform: 'translate(8px, -110%)', padding: '6px 8px', borderRadius: 6, pointerEvents: 'none',
    background: ds.surface, border: `1px solid ${ds.border}`, color: ds.textPrimary, fontSize: 11, whiteSpace: 'nowrap',
  },
  table: { borderCollapse: 'collapse', fontSize: 12, marginTop: 4 },
  th: { textAlign: 'left', padding: '4px 12px 4px 0', color: ds.textMuted, fontWeight: 500 },
  td: { padding: '4px 12px 4px 0', color: ds.textPrimary },
}
