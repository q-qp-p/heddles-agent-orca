/**
 * RoutingInsightsView — what the operator has found about how models are routed.
 *
 * Recommendations are changes to a ModelSelector that benchmark and traffic
 * evidence support. Each shows the numbers behind it, the patch, and how to apply
 * it: copy it for kubectl, or, where a benchmark in proposeApply mode allows,
 * ask for an approval. Nothing here changes a selector; an approver does, in the
 * approvals inbox. Benchmarks shows what the replayed calls found per Agent.
 */
import { useCallback, useEffect, useState } from 'react'
import {
  askRecommendationAction, listRoutingBenchmarks, listRoutingRecommendations,
  type RecommendationEvidence, type RoutingBenchmark, type RoutingRecommendation,
} from '../api/sse'
import { DSButton, DSCard, DSChip, DSTabBar } from '../lib/primitives'
import { ds } from '../lib/designSystem'
import { Icon, ICON } from '../lib/icons'
import { toYAML } from '../lib/yaml'
import { FrontierChart } from './FrontierChart'

type Tab = 'recommendations' | 'benchmarks'
type Scope = 'open' | 'all'

const DETECTOR_LABEL: Record<RoutingRecommendation['detector'], string> = {
  overspend: 'Overspend',
  underpowered: 'Underpowered',
  threshold: 'Threshold',
  deadProvider: 'Dead provider',
  strategySwitch: 'Strategy switch',
}

const PHASE_VARIANT: Record<string, 'warning' | 'success' | 'error' | 'neutral' | 'default'> = {
  Proposed: 'default',
  PendingApproval: 'warning',
  Applied: 'success',
  Rejected: 'error',
  Stale: 'neutral',
  Superseded: 'neutral',
  RolledBack: 'neutral',
  Expired: 'neutral',
}

const VERDICT_VARIANT: Record<string, 'success' | 'error' | 'neutral'> = {
  Confirmed: 'success',
  Regressed: 'error',
  Inconclusive: 'neutral',
  NotAssessed: 'neutral',
}

export const pct = (v: number, digits = 0) => `${(v * 100).toFixed(digits)}%`
export const usd = (v: number) => `$${Math.abs(v) >= 0.01 ? v.toFixed(2) : v.toFixed(4)}`

export function RoutingInsightsView() {
  const [tab, setTab] = useState<Tab>('recommendations')
  const [scope, setScope] = useState<Scope>('open')
  const [recs, setRecs] = useState<RoutingRecommendation[]>([])
  const [benches, setBenches] = useState<RoutingBenchmark[]>([])
  const [error, setError] = useState<string | null>(null)
  const [loaded, setLoaded] = useState(false)

  const load = useCallback(async () => {
    try {
      const [r, b] = await Promise.all([listRoutingRecommendations(), listRoutingBenchmarks()])
      setRecs(r)
      setBenches(b)
      setError(null)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoaded(true)
    }
  }, [])

  useEffect(() => {
    void load()
    const id = setInterval(() => void load(), 15000)
    return () => clearInterval(id)
  }, [load])

  const open = recs.filter((r) => r.phase === 'Proposed' || r.phase === 'PendingApproval')
  const shown = scope === 'open' ? open : recs

  return (
    <div style={s.root}>
      <div style={s.header}>
        <div>
          <h2 style={s.title}>Routing</h2>
          <p style={s.subtitle}>
            What the operator has found about how models are chosen, from traffic and from calls a benchmark replayed on
            another model. It never changes a selector itself: copy a change to apply it, or ask for an approval where a
            benchmark allows.
          </p>
        </div>
        <DSTabBar
          tabs={[{ key: 'recommendations', label: 'Recommendations' }, { key: 'benchmarks', label: 'Benchmarks' }]}
          active={tab}
          onChange={(k) => setTab(k as Tab)}
          count={{ recommendations: open.length }}
        />
      </div>

      {error && (
        <div role="alert" style={s.error}>
          <Icon icon={ICON.warning} size={14} ariaHidden /> {error}
        </div>
      )}

      {tab === 'recommendations' && (
        <>
          <div style={s.filters}>
            <DSTabBar
              tabs={[{ key: 'open', label: 'Open' }, { key: 'all', label: 'All' }]}
              active={scope}
              onChange={(k) => setScope(k as Scope)}
            />
          </div>
          {loaded && shown.length === 0 && !error && (
            <div style={s.empty}>
              <Icon icon={ICON.approval} size={36} ariaHidden style={{ opacity: 0.25 }} />
              <div style={s.emptyTitle}>{scope === 'open' ? 'Nothing to recommend' : 'No recommendations yet'}</div>
              <div style={s.emptyBody}>
                Recommendations appear once a selector has enough traffic, or a RoutingBenchmark has judged enough replayed
                calls, to say something. They need PostgreSQL.
              </div>
            </div>
          )}
          <div style={s.list}>
            {shown.map((r) => (
              <RecommendationCard key={`${r.namespace}/${r.name}`} rec={r} onAsked={() => void load()} />
            ))}
          </div>
        </>
      )}

      {tab === 'benchmarks' && (
        <>
          {loaded && benches.length === 0 && !error && (
            <div style={s.empty}>
              <Icon icon={ICON.database} size={36} ariaHidden style={{ opacity: 0.25 }} />
              <div style={s.emptyTitle}>No benchmarks</div>
              <div style={s.emptyBody}>
                A RoutingBenchmark replays a sample of an Agent's calls on another model and has a judge compare the answers.
              </div>
            </div>
          )}
          <div style={s.list}>
            {benches.map((b) => <BenchmarkCard key={`${b.namespace}/${b.name}`} bench={b} />)}
          </div>
        </>
      )}
    </div>
  )
}

function RecommendationCard({ rec: r, onAsked }: { rec: RoutingRecommendation; onAsked: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [copied, setCopied] = useState<string | null>(null)

  const ask = async (action: 'requestApply' | 'rollback') => {
    setBusy(true)
    setError(null)
    try {
      await askRecommendationAction(r.namespace, r.name, action)
      onAsked()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }
  const copy = async (what: string, text: string) => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(what)
      setTimeout(() => setCopied((c) => (c === what ? null : c)), 2000)
    } catch {
      setError('Copying is not available here; select the text under "Patch" instead.')
    }
  }

  const rollbackAsked = r.conditions?.some((c) => c.type === 'RollbackRequested' && c.status === 'True')
  const blocked = r.conditions?.filter((c) => c.status === 'True' && /Blocked|Failed/.test(c.type)) ?? []
  const ev = r.evidence
  return (
    <DSCard>
      <div style={s.cardTop}>
        <div style={{ minWidth: 0 }}>
          <div style={s.summary}>{r.summary}</div>
          <div style={s.where}>
            {r.namespace}/{r.selector}
            {r.agent ? ` · ${r.agent}` : ''}
            {r.category ? ` · ${r.category}` : ''}
          </div>
        </div>
        <div style={s.chips}>
          <DSChip>{DETECTOR_LABEL[r.detector] ?? r.detector}</DSChip>
          <DSChip variant={PHASE_VARIANT[r.phase] ?? 'neutral'} dot>{r.phase === 'PendingApproval' ? 'waiting for approval' : r.phase}</DSChip>
        </div>
      </div>

      {ev && <Evidence rec={r} ev={ev} />}

      {r.corroboration && r.corroboration.length > 0 && (
        <div style={s.note}>{r.corroboration.join(' · ')}</div>
      )}
      {r.caveat && (
        <div style={s.caveat} role="note">
          <Icon icon={ICON.warning} size={14} ariaHidden /> {r.caveat}
        </div>
      )}
      {r.postApply && (
        <div style={s.postApply}>
          <DSChip variant={VERDICT_VARIANT[r.postApply.verdict] ?? 'neutral'}>{r.postApply.verdict}</DSChip>
          <span>{r.postApply.message}</span>
        </div>
      )}
      {r.message && r.phase !== 'Proposed' && <div style={s.note}>{r.message}</div>}
      {blocked.map((c) => <div key={c.type} role="note" style={s.caveat}><Icon icon={ICON.warning} size={14} ariaHidden /> {c.message}</div>)}

      {r.patch ? (
        <details style={s.details}>
          <summary style={s.detailsSummary}>Patch</summary>
          <pre style={s.pre}>{pretty(r.patch)}</pre>
          {r.appliedRollbackPatch || r.rollbackPatch ? (
            <>
              <div style={s.factLabel}>Undo with</div>
              <pre style={s.pre}>{pretty(r.appliedRollbackPatch || r.rollbackPatch || '')}</pre>
            </>
          ) : null}
        </details>
      ) : (
        r.reason && <div style={s.note}>Report only: {r.reason}</div>
      )}

      <div style={s.actions}>
        {r.kubectlPatch && (
          <DSButton variant="secondary" onClick={() => void copy('kubectl', r.kubectlPatch ?? '')}>
            {copied === 'kubectl' ? 'Copied' : 'Copy kubectl command'}
          </DSButton>
        )}
        {r.patch && (
          <DSButton variant="secondary" onClick={() => void copy('yaml', toYAML(safeParse(r.patch)))}>
            {copied === 'yaml' ? 'Copied' : 'Copy patch as YAML'}
          </DSButton>
        )}
        {r.canRequestApply && (
          <DSButton variant="primary" disabled={busy} onClick={() => void ask('requestApply')}>
            {busy ? 'Asking…' : 'Ask for approval to apply'}
          </DSButton>
        )}
        {r.canRollback && (
          <DSButton variant="danger" disabled={busy} onClick={() => void ask('rollback')}>
            {busy ? 'Asking…' : 'Ask for approval to undo'}
          </DSButton>
        )}
        {rollbackAsked && <span style={s.note}>Waiting for an approver to undo this change.</span>}
        {!r.canRequestApply && !r.canRollback && r.blockedReason && (r.phase === 'Proposed' || r.phase === 'Applied') && (
          <span style={s.note}>{r.blockedReason}</span>
        )}
      </div>
      {error && <div role="alert" style={s.cardError}>{error}</div>}
    </DSCard>
  )
}

function Evidence({ rec: r, ev }: { rec: RoutingRecommendation; ev: RecommendationEvidence }) {
  const facts: [string, string][] = []
  if (r.detector === 'deadProvider' && ev.calls) {
    facts.push(['Failed calls', `${Math.round((ev.errorRate ?? 0) * ev.calls)} of ${ev.calls} (${pct(ev.errorRate ?? 0)})`])
  }
  if (r.detector === 'overspend' || r.detector === 'underpowered') {
    if (ev.samples) facts.push(['Judged pairs', String(ev.samples)])
    if (ev.netWin !== undefined) {
      facts.push(['Dearer model wins by', `${pct(ev.netWin)} (${pct(ev.netWinLo ?? 0)} to ${pct(ev.netWinHi ?? 0)})`])
    }
    if (ev.deltaCostUsd !== undefined) facts.push(['Extra cost per call', usd(ev.deltaCostUsd)])
    if (ev.usdPerNetWin !== undefined) facts.push(['Cost of one extra win', usd(ev.usdPerNetWin)])
    if (ev.projectedMonthlySavingsUsd !== undefined) facts.push(['Projected saving', `${usd(ev.projectedMonthlySavingsUsd)} a month`])
  }
  if (r.detector === 'threshold' && ev.fromThreshold !== undefined && ev.toThreshold !== undefined) {
    facts.push(['Threshold', `${ev.fromThreshold.toFixed(2)} → ${ev.toThreshold.toFixed(2)}`])
    if (ev.samples) facts.push(['Judged calls', String(ev.samples)])
    if (ev.deltaCostUsd !== undefined) facts.push(['Cost change per call', `${ev.deltaCostUsd <= 0 ? '−' : '+'}${usd(ev.deltaCostUsd)}`])
    if (ev.lossChange !== undefined) facts.push(['Routed to the losing model', `${ev.lossChange <= 0 ? '' : '+'}${pct(ev.lossChange, 1)} (at most ${pct(ev.lossChangeHi ?? 0, 1)})`])
  }
  if (r.detector === 'strategySwitch' && ev.routingLift !== undefined) {
    facts.push(['Routing lift', `${pct(ev.routingLift)} (at most ${pct(ev.routingLiftHi ?? 0)})`])
    if (ev.samples) facts.push(['Judged calls', String(ev.samples)])
  }
  if (ev.inconsistentRate && ev.inconsistentRate > 0.2) {
    facts.push(['Judge disagreed with itself', pct(ev.inconsistentRate)])
  }
  return (
    <>
      {facts.length > 0 && (
        <dl style={s.facts}>
          {facts.map(([k, v]) => (
            <Fact key={k} label={k} value={v} />
          ))}
        </dl>
      )}
      {r.detector === 'threshold' && ev.frontier && ev.frontier.length > 0 && (
        <div style={{ marginTop: 12 }}>
          <FrontierChart points={ev.frontier} proposed={ev.toThreshold} />
        </div>
      )}
    </>
  )
}

function BenchmarkCard({ bench: b }: { bench: RoutingBenchmark }) {
  const spend = b.shadowSpendTodayUSD ? `$${Number(b.shadowSpendTodayUSD).toFixed(4)} of $${b.perDayUSD} today` : `$${b.perDayUSD} a day`
  return (
    <DSCard>
      <div style={s.cardTop}>
        <div style={{ minWidth: 0 }}>
          <div style={s.summary}>{b.name}</div>
          <div style={s.where}>{b.namespace}</div>
        </div>
        <div style={s.chips}>
          <DSChip variant={b.mode === 'proposeApply' ? 'warning' : 'default'}>{b.mode === 'proposeApply' ? 'proposeApply' : 'reportOnly'}</DSChip>
          <DSChip variant={b.ready ? 'success' : 'error'} dot>{b.ready ? 'ready' : 'not ready'}</DSChip>
        </div>
      </div>
      {!b.ready && b.readyMessage && <div style={s.caveat} role="note"><Icon icon={ICON.warning} size={14} ariaHidden /> {b.readyMessage}</div>}
      {b.warnings?.map((w) => <div key={w} style={s.caveat} role="note"><Icon icon={ICON.warning} size={14} ariaHidden /> {w}</div>)}
      <dl style={s.facts}>
        <Fact label="Agents" value={b.agents?.length ? b.agents.join(', ') : 'none yet'} />
        {b.judge && <Fact label="Judge" value={b.judge} mono />}
        {b.sampleRate && <Fact label="Sample rate" value={pct(Number(b.sampleRate))} />}
        <Fact label="Replays" value={`${b.sampledCalls} sampled, ${b.judgedPairs} judged`} />
        <Fact label="Spend" value={b.budgetExhausted ? `${spend} (budget spent)` : spend} />
        {b.judgeInconsistentRate && <Fact label="Judge disagreed with itself" value={pct(Number(b.judgeInconsistentRate), 1)} />}
      </dl>
      {b.categories && b.categories.length > 0 && (
        <table style={s.table} aria-label={`Verdicts for ${b.name}`}>
          <thead>
            <tr><th style={s.th}>Agent</th><th style={s.th}>Category</th><th style={s.thNum}>Pairs</th><th style={s.th}>Served won / replay won / tied</th></tr>
          </thead>
          <tbody>
            {b.categories.map((c) => (
              <tr key={`${c.agent}/${c.category}`}>
                <td style={s.td}>{c.agent}</td>
                <td style={{ ...s.td, ...s.mono }}>{c.category}</td>
                <td style={s.tdNum}>{c.samples}</td>
                <td style={s.td}>
                  <VerdictBar served={c.servedWins} replay={c.shadowWins} tied={c.ties} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </DSCard>
  )
}

/** Three counts as one bar, each segment labelled, so no meaning rests on colour. */
function VerdictBar({ served, replay, tied }: { served: number; replay: number; tied: number }) {
  const total = Math.max(served + replay + tied, 1)
  const seg = (n: number, bg: string) => (n === 0 ? null : <span style={{ width: `${(n / total) * 100}%`, background: bg, height: 8, display: 'inline-block' }} />)
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
      <span style={{ display: 'inline-flex', width: 80, gap: 2, borderRadius: 2, overflow: 'hidden' }} aria-hidden>
        {seg(served, ds.textMuted)}
        {seg(replay, ds.accent)}
        {seg(tied, ds.border)}
      </span>
      <span>{served} / {replay} / {tied}</span>
    </span>
  )
}

function Fact({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return (
    <>
      <dt style={s.factLabel}>{label}</dt>
      <dd style={{ ...s.factValue, ...(mono ? s.mono : {}) }}>{value}</dd>
    </>
  )
}

function safeParse(text: string | undefined): unknown {
  try {
    return JSON.parse(text ?? '{}')
  } catch {
    return {}
  }
}

function pretty(text: string): string {
  try {
    return JSON.stringify(JSON.parse(text), null, 2)
  } catch {
    return text
  }
}

const s: Record<string, React.CSSProperties> = {
  root: { display: 'flex', flexDirection: 'column', gap: 16, padding: 24, maxWidth: 960 },
  header: { display: 'flex', justifyContent: 'space-between', alignItems: 'flex-end', gap: 16, flexWrap: 'wrap' },
  title: { margin: 0, fontSize: 20, color: ds.textPrimary },
  subtitle: { margin: '4px 0 0', fontSize: 13, color: ds.textSecondary, maxWidth: 640 },
  filters: { display: 'flex', gap: 8 },
  list: { display: 'flex', flexDirection: 'column', gap: 12 },
  cardTop: { display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', gap: 12, flexWrap: 'wrap' },
  summary: { fontSize: 14, fontWeight: 600, color: ds.textPrimary, overflowWrap: 'anywhere' },
  where: { fontSize: 12, color: ds.textMuted, marginTop: 2, overflowWrap: 'anywhere' },
  chips: { display: 'flex', gap: 6, flexWrap: 'wrap' },
  facts: { display: 'grid', gridTemplateColumns: 'max-content 1fr', gap: '4px 16px', margin: '12px 0 0' },
  factLabel: { fontSize: 12, color: ds.textMuted },
  factValue: { margin: 0, fontSize: 12, color: ds.textPrimary, overflowWrap: 'anywhere' },
  mono: { fontFamily: 'var(--ds-font-mono, ui-monospace, monospace)' },
  note: { marginTop: 10, fontSize: 12, color: ds.textSecondary },
  caveat: {
    display: 'flex', gap: 6, alignItems: 'flex-start', marginTop: 10, padding: '8px 10px', borderRadius: 6,
    background: 'rgba(245,158,11,.12)', color: ds.warning, fontSize: 12,
  },
  postApply: { display: 'flex', gap: 8, alignItems: 'center', marginTop: 10, fontSize: 12, color: ds.textSecondary, flexWrap: 'wrap' },
  details: { marginTop: 10 },
  detailsSummary: { fontSize: 12, color: ds.accent, cursor: 'pointer' },
  pre: {
    margin: '6px 0', padding: 8, borderRadius: 6, background: ds.bg, color: ds.textPrimary, fontSize: 12, overflowX: 'auto',
    fontFamily: 'var(--ds-font-mono, ui-monospace, monospace)',
  },
  actions: { display: 'flex', gap: 8, alignItems: 'center', marginTop: 14, flexWrap: 'wrap' },
  cardError: { marginTop: 8, fontSize: 12, color: ds.error },
  error: { display: 'flex', gap: 6, alignItems: 'center', fontSize: 13, color: ds.error },
  empty: { display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 8, padding: 48, textAlign: 'center' },
  emptyTitle: { fontSize: 14, fontWeight: 600, color: ds.textPrimary },
  emptyBody: { fontSize: 13, color: ds.textSecondary, maxWidth: 460 },
  table: { borderCollapse: 'collapse', fontSize: 12, marginTop: 12, width: '100%' },
  th: { textAlign: 'left', padding: '4px 12px 4px 0', color: ds.textMuted, fontWeight: 500 },
  thNum: { textAlign: 'right', padding: '4px 12px 4px 0', color: ds.textMuted, fontWeight: 500 },
  td: { padding: '4px 12px 4px 0', color: ds.textPrimary },
  tdNum: { padding: '4px 12px 4px 0', color: ds.textPrimary, textAlign: 'right' },
}
