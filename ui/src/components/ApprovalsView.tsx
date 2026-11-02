/**
 * ApprovalsView — the approvals inbox. Lists requests waiting for a decision
 * (e.g. egress a sandboxed agent's policy does not allow) and lets a signed-in
 * approver approve or reject each with a reason. Decisions are recorded with the
 * approver's identity and applied to the run within one operator poll.
 */
import { useCallback, useEffect, useState } from 'react'
import { decideApproval, listApprovals, type Approval } from '../api/sse'
import { DSButton, DSCard, DSChip, DSInput, DSTabBar } from '../lib/primitives'
import { ds } from '../lib/designSystem'
import { Icon, ICON } from '../lib/icons'

type View = 'pending' | 'recent'

interface Props {
  /** Open the run an approval belongs to. */
  onNavigateToRun?: (runName: string, namespace: string) => void
}

const STATUS_VARIANT: Record<Approval['status'], 'warning' | 'success' | 'error' | 'neutral'> = {
  pending: 'warning',
  approved: 'success',
  rejected: 'error',
  expired: 'neutral',
  withdrawn: 'neutral',
}

export function ApprovalsView({ onNavigateToRun }: Props) {
  const [view, setView] = useState<View>('pending')
  const [items, setItems] = useState<Approval[]>([])
  const [error, setError] = useState<string | null>(null)
  const [notConfigured, setNotConfigured] = useState(false)
  const [loaded, setLoaded] = useState(false)

  const load = useCallback(async () => {
    try {
      setItems(await listApprovals(view === 'pending' ? 'pending' : ''))
      setError(null)
      setNotConfigured(false)
    } catch (e: unknown) {
      const msg = e instanceof Error ? e.message : String(e)
      if (msg.includes('not configured')) setNotConfigured(true)
      else setError(msg)
    } finally {
      setLoaded(true)
    }
  }, [view])

  useEffect(() => {
    void load()
    const id = setInterval(() => void load(), 10000)
    return () => clearInterval(id)
  }, [load])

  if (notConfigured) {
    return (
      <div style={s.empty}>
        <Icon icon={ICON.database} size={36} ariaHidden style={{ opacity: 0.25 }} />
        <div style={s.emptyTitle}>Approvals are not available</div>
        <div style={s.emptyBody}>
          Approvals are stored in PostgreSQL, which is not configured on the operator.
        </div>
      </div>
    )
  }

  return (
    <div style={s.root}>
      <div style={s.header}>
        <div>
          <h2 style={s.title}>Approvals</h2>
          <p style={s.subtitle}>
            Requests from agents for access their policy does not allow. Approving applies to the
            running agent within seconds; requests not decided before they expire are rejected.
          </p>
        </div>
        <DSTabBar
          tabs={[{ key: 'pending', label: 'Pending' }, { key: 'recent', label: 'Recent' }]}
          active={view}
          onChange={(k) => setView(k as View)}
        />
      </div>

      {error && (
        <div role="alert" style={s.error}>
          <Icon icon={ICON.warning} size={14} ariaHidden /> {error}
        </div>
      )}

      {loaded && items.length === 0 && !error && (
        <div style={s.empty}>
          <Icon icon={ICON.approval} size={36} ariaHidden style={{ opacity: 0.25 }} />
          <div style={s.emptyTitle}>{view === 'pending' ? 'Nothing waiting for a decision' : 'No approvals yet'}</div>
        </div>
      )}

      <div style={s.list}>
        {items.map((a) =>
          a.status === 'pending' ? (
            <PendingCard key={a.id} approval={a} onDecided={() => void load()} onNavigateToRun={onNavigateToRun} />
          ) : (
            <DecidedRow key={a.id} approval={a} onNavigateToRun={onNavigateToRun} />
          ),
        )}
      </div>
    </div>
  )
}

function PendingCard({ approval: a, onDecided, onNavigateToRun }: {
  approval: Approval
  onDecided: () => void
  onNavigateToRun?: (runName: string, namespace: string) => void
}) {
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState<'approve' | 'reject' | null>(null)
  const [error, setError] = useState<string | null>(null)
  const d = a.details ?? {}

  const decide = async (action: 'approve' | 'reject') => {
    setBusy(action)
    setError(null)
    try {
      await decideApproval(a.id, action, reason)
      onDecided()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(null)
    }
  }

  return (
    <DSCard>
      <div style={s.cardTop}>
        <div style={{ minWidth: 0 }}>
          <div style={s.summary}>{a.summary}</div>
          <RunLink approval={a} onNavigateToRun={onNavigateToRun} />
        </div>
        <DSChip variant="warning" dot>expires {formatRelative(a.expiresAt)}</DSChip>
      </div>

      <dl style={s.facts}>
        {a.kind === 'workflow' && d.workflow && <Fact label="Workflow" value={`${d.workflow} / ${d.step ?? ''}`} mono />}
        {a.kind === 'workflow' && d.message && <Fact label="Asks" value={d.message} wrap />}
        {a.kind === 'workflow' && <Fact label="If rejected" value="the step fails and the workflow stops, unless it is set to continue" />}
        {a.kind === 'tool' && d.tool && <Fact label="Tool" value={`${d.tool}${d.sideEffect ? ` (${d.sideEffect})` : ''}`} mono />}
        {a.kind === 'tool' && d.arguments && <Fact label="Arguments" value={d.arguments} mono />}
        {a.kind === 'tool' && <Fact label="If rejected" value="the tool does not run; the agent is told and the run goes on" />}
        {a.kind === 'budget' && d.spentUSD && <Fact label="Spent" value={`$${d.spentUSD}`} mono />}
        {a.kind === 'budget' && d.capUSD && <Fact label="Current cap" value={`$${d.capUSD}`} mono />}
        {a.kind === 'budget' && d.newCapUSD && (
          <Fact label="If approved" value={`cap rises by $${d.raiseByUSD} to $${d.newCapUSD}; the run resumes`} />
        )}
        {a.kind === 'budget' && <Fact label="If rejected" value="the run fails" />}
        {isRouting(a) && d.selector && <Fact label="Selector" value={`${a.namespace}/${d.selector}`} mono />}
        {isRouting(a) && d.category && <Fact label="Kind of work" value={d.category} mono />}
        {isRouting(a) && d.summary && <Fact label="What the evidence shows" value={d.summary} wrap />}
        {isRouting(a) && typeof d.evidence?.projectedMonthlySavingsUsd === 'number' && (
          <Fact label="Projected saving" value={`$${d.evidence.projectedMonthlySavingsUsd.toFixed(2)} a month`} />
        )}
        {isRouting(a) && d.patch !== undefined && (
          <Fact label={a.kind === 'routing' ? 'Change' : 'Undo with'} value={JSON.stringify(d.patch, null, 2)} mono wrap />
        )}
        {a.kind === 'routing' && (
          <Fact label="If approved" value="the selector is changed now, and the settings it replaces are kept so the change can be undone" />
        )}
        {a.kind === 'routing-rollback' && <Fact label="If approved" value="the selector goes back to how it was before the change" />}
        {isRouting(a) && <Fact label="If rejected" value="the selector is left as it is" />}
        {d.hosts && d.hosts.length > 0 && <Fact label="Destination" value={d.hosts.join(', ')} mono />}
        {d.binaries && d.binaries.length > 0 && <Fact label="Process" value={d.binaries.join(', ')} mono />}
        {d.rationale && <Fact label="Why" value={d.rationale} />}
        {d.validation && <Fact label="Policy check" value={d.validation} />}
        {typeof d.hits === 'number' && <Fact label="Attempts" value={String(d.hits)} />}
        <Fact label="Requested" value={new Date(a.requestedAt).toLocaleString()} />
        <Fact label="Approvers" value={a.approverGroups.length > 0 ? a.approverGroups.join(', ') : 'anyone with access to the namespace'} />
      </dl>

      {isRouting(a) && d.caveat && (
        <div style={s.securityNote} role="note">
          <Icon icon={ICON.warning} size={14} ariaHidden /> {d.caveat}
        </div>
      )}
      {d.securityNotes && (
        <div style={s.securityNote} role="note">
          <Icon icon={ICON.warning} size={14} ariaHidden /> {d.securityNotes}
        </div>
      )}

      <div style={s.actions}>
        <div style={{ flex: 1, minWidth: 200 }}>
          <DSInput
            aria-label="Reason for the decision"
            placeholder="Reason (recorded with your decision)"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          />
        </div>
        <DSButton variant="danger" disabled={busy !== null} onClick={() => void decide('reject')}>
          {busy === 'reject' ? 'Rejecting…' : 'Reject'}
        </DSButton>
        <DSButton variant="primary" disabled={busy !== null} onClick={() => void decide('approve')}>
          {busy === 'approve' ? 'Approving…' : 'Approve'}
        </DSButton>
      </div>
      {error && <div role="alert" style={s.cardError}>{error}</div>}
    </DSCard>
  )
}

/** A routing change, or the undoing of one: they belong to no run. */
function isRouting(a: Approval): boolean {
  return a.kind === 'routing' || a.kind === 'routing-rollback'
}

function DecidedRow({ approval: a, onNavigateToRun }: {
  approval: Approval
  onNavigateToRun?: (runName: string, namespace: string) => void
}) {
  return (
    <DSCard>
      <div style={s.cardTop}>
        <div style={{ minWidth: 0 }}>
          <div style={s.summary}>{a.summary}</div>
          <RunLink approval={a} onNavigateToRun={onNavigateToRun} />
        </div>
        <DSChip variant={STATUS_VARIANT[a.status]}>{a.status}</DSChip>
      </div>
      <div style={s.decided}>
        {a.decidedBy ? `by ${a.decidedBy}` : ''}
        {a.decidedAt ? ` · ${new Date(a.decidedAt).toLocaleString()}` : ''}
        {a.reason ? ` · “${a.reason}”` : ''}
      </div>
    </DSCard>
  )
}

function RunLink({ approval: a, onNavigateToRun }: {
  approval: Approval
  onNavigateToRun?: (runName: string, namespace: string) => void
}) {
  // A workflow's approval step belongs to no run: show where it is, without a link.
  if (!a.runName) return <div style={s.run}>{a.namespace}</div>
  const label = `${a.namespace}/${a.runName}`
  if (!onNavigateToRun) return <div style={s.run}>{label}</div>
  return (
    <button type="button" style={s.runLink} onClick={() => onNavigateToRun(a.runName, a.namespace)}>
      {label}
    </button>
  )
}

function Fact({ label, value, mono = false, wrap = false }: { label: string; value: string; mono?: boolean; wrap?: boolean }) {
  return (
    <>
      <dt style={s.factLabel}>{label}</dt>
      <dd style={{ ...s.factValue, ...(mono ? s.mono : {}), ...(wrap ? { whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' } : {}) }}>{value}</dd>
    </>
  )
}

/** "in 12m", "3m ago". */
export function formatRelative(iso: string, now = Date.now()): string {
  const diff = new Date(iso).getTime() - now
  const mins = Math.round(Math.abs(diff) / 60000)
  const text = mins < 1 ? 'under a minute' : mins < 60 ? `${mins}m` : `${Math.round(mins / 60)}h`
  return diff >= 0 ? `in ${text}` : `${text} ago`
}

const s: Record<string, React.CSSProperties> = {
  root: { display: 'flex', flexDirection: 'column', gap: 16, padding: 24, maxWidth: 960 },
  header: { display: 'flex', justifyContent: 'space-between', alignItems: 'flex-end', gap: 16, flexWrap: 'wrap' },
  title: { margin: 0, fontSize: 20, color: ds.textPrimary },
  subtitle: { margin: '4px 0 0', fontSize: 13, color: ds.textSecondary, maxWidth: 600 },
  list: { display: 'flex', flexDirection: 'column', gap: 12 },
  cardTop: { display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', gap: 12 },
  summary: { fontSize: 14, fontWeight: 600, color: ds.textPrimary, overflowWrap: 'anywhere' },
  run: { fontSize: 12, color: ds.textMuted, marginTop: 2 },
  runLink: {
    fontSize: 12, color: ds.accent, marginTop: 2, padding: 0, background: 'none', border: 'none',
    cursor: 'pointer', textAlign: 'left',
  },
  facts: { display: 'grid', gridTemplateColumns: 'max-content 1fr', gap: '4px 16px', margin: '12px 0 0' },
  factLabel: { fontSize: 12, color: ds.textMuted },
  factValue: { margin: 0, fontSize: 12, color: ds.textPrimary, overflowWrap: 'anywhere' },
  mono: { fontFamily: 'var(--ds-font-mono, ui-monospace, monospace)' },
  securityNote: {
    display: 'flex', gap: 6, alignItems: 'flex-start', marginTop: 12, padding: '8px 10px', borderRadius: 6,
    background: 'rgba(245,158,11,.12)', color: ds.warning, fontSize: 12,
  },
  actions: { display: 'flex', gap: 8, alignItems: 'flex-end', marginTop: 14, flexWrap: 'wrap' },
  cardError: { marginTop: 8, fontSize: 12, color: ds.error },
  decided: { marginTop: 6, fontSize: 12, color: ds.textMuted },
  error: { display: 'flex', gap: 6, alignItems: 'center', fontSize: 13, color: ds.error },
  empty: { display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 8, padding: 48, textAlign: 'center' },
  emptyTitle: { fontSize: 14, fontWeight: 600, color: ds.textPrimary },
  emptyBody: { fontSize: 13, color: ds.textSecondary, maxWidth: 420 },
}
