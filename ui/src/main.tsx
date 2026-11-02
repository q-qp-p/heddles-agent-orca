import { StrictMode, useEffect, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { getSystemStatus, listApprovals } from './api/sse'
import { ResourceList, type ResourceSelection, type ResourceTab } from './components/ResourceList'
import { ConfigList } from './components/ConfigList'
import { ConfigDetailView } from './components/ConfigDetailView'
import { RunView } from './components/RunView'
import { DeploymentView } from './components/DeploymentView'
import { WorkflowView } from './components/WorkflowView'
import { RunHistoryView } from './components/RunHistoryView'
import { RunHistoryDetailView } from './components/RunHistoryDetailView'
import { SystemStatusPage } from './components/SystemStatusPage'
import { CostDashboard } from './components/CostDashboard'
import { SystemDashboard } from './components/SystemDashboard'
import { MarketplaceView } from './components/MarketplaceView'
import { ApprovalsView } from './components/ApprovalsView'
import { RoutingInsightsView } from './components/RoutingInsightsView'
import { applyDesignSystem, ds, toggleTheme, applyTheme, getPreferredTheme, type Theme } from './lib/designSystem'
import { ICON, Icon } from './lib/icons'
import type { IconComponent } from './lib/icons'
import logoUrl from '../../assets/logo.png'

// Register the design system (CSS variables, font smoothing, reduced-motion,
// focus-visible baseline, aoPulse keyframe) before rendering.
applyDesignSystem()
// Apply the saved or system-default theme.
applyTheme(getPreferredTheme())

type TopLevelTab = ResourceTab | 'home' | 'marketplace' | 'history' | 'status' | 'approvals' | 'routing'

// ── Navigation state persistence (survives page refresh) ────────────────────
//
// SessionStorage preserves the user's navigation across refreshes without
// leaking across tabs/sessions. Only navigation-relevant state is saved;
// transient UI state (create-agent panel, redis banner, cost view) resets.
const NAV_STATE_KEY = 'agentorca-nav-state'

interface NavState {
  tab: TopLevelTab
  selection: ResourceSelection | null
  breadcrumbs: ResourceSelection[]
  navigationOrigin: 'sidebar' | 'home' | 'marketplace'
}

function loadNavState(): Partial<NavState> {
  try {
    const raw = sessionStorage.getItem(NAV_STATE_KEY)
    if (!raw) return {}
    return JSON.parse(raw) as Partial<NavState>
  } catch {
    return {}
  }
}

function saveNavState(state: Partial<NavState>): void {
  try {
    sessionStorage.setItem(NAV_STATE_KEY, JSON.stringify(state))
  } catch {
    // private browsing / quota exceeded
  }
}

// Validate the saved tab — ignore stale tabs from a previous deployment.
const VALID_TABS: TopLevelTab[] = [
  'home', 'marketplace', 'history', 'status', 'approvals', 'routing', 'runs', 'deployments', 'workflows',
  'agents', 'tools', 'mcpservers', 'modelproviders', 'knowledgebases', 'modelselectors',
]

const savedState = loadNavState()
// A ?tab= link (e.g. from an approval notification) wins over the saved tab.
const linkedTab = new URLSearchParams(window.location.search).get('tab') as TopLevelTab | null
const initialTab: TopLevelTab = linkedTab && VALID_TABS.includes(linkedTab)
  ? linkedTab
  : VALID_TABS.includes(savedState.tab as TopLevelTab)
    ? (savedState.tab as TopLevelTab)
    : 'home'

// Inject global markdown rendering styles (aoPulse keyframe now lives in the design system).
const styleEl = document.createElement('style')
styleEl.textContent = `
  .ao-md table { border-collapse: collapse; width: 100%; margin: 12px 0; font-size: 13px; }
  .ao-md th, .ao-md td { border: 1px solid var(--ds-border); padding: 6px 12px; text-align: left; vertical-align: top; }
  .ao-md th { background: var(--ds-surface); color: var(--ds-text-secondary); font-weight: 600; font-size: 11px; text-transform: uppercase; letter-spacing: 0.06em; }
  .ao-md tr:nth-child(even) td { background: var(--ds-md-table-bg); }
  .ao-md p { margin: 0 0 8px; }
  .ao-md p:last-child { margin-bottom: 0; }
  .ao-md h1, .ao-md h2, .ao-md h3 { color: var(--ds-text-primary); margin: 16px 0 8px; }
  .ao-md code { background: var(--ds-md-code-bg); border-radius: 3px; padding: 1px 5px; font-family: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace'; font-size: 12px; }
  .ao-md pre { background: var(--ds-md-pre-bg); border: 1px solid var(--ds-border); border-radius: 6px; padding: 12px; overflow-x: auto; }
  .ao-md pre code { background: none; padding: 0; }
  .ao-md ul, .ao-md ol { padding-left: 20px; margin: 8px 0; }
  .ao-md blockquote { border-left: 3px solid var(--ds-accent); padding-left: 12px; margin: 12px 0; color: var(--ds-text-secondary); }
  .ao-md img { max-width: 100%; border-radius: 6px; }
  /* Tabular numerals for stable markdown tables and inline values */
  .ao-md { font-variant-numeric: tabular-nums; }
`
document.head.appendChild(styleEl)

function RedisBanner({ onDismiss }: { onDismiss: () => void }) {
  return (
    <div style={banner.root}>
      <span style={{ display: "inline-flex" }}><Icon icon={ICON.warning} size={14} ariaHidden={true} /></span>
      <span style={{ flex: 1 }}>
        <strong>Redis is not configured.</strong> Cost tracking will show $0 and conversation
        history will not persist across pod restarts. Set{' '}
        <code style={banner.code}>STATE_BACKEND=redis</code> and{' '}
        <code style={banner.code}>REDIS_URL</code> on the operator to enable these features.
      </span>
      <button style={banner.dismiss} onClick={onDismiss} aria-label="Dismiss Redis warning"><Icon icon={ICON.close} size={14} /></button>
    </div>
  )
}

const banner: Record<string, React.CSSProperties> = {
  root: {
    display: 'flex',
    alignItems: 'center',
    gap: 10,
    padding: '8px 20px',
    background: 'rgba(245,158,11,.10)',
    borderBottom: '1px solid var(--ds-border)',
    fontSize: 13,
    color: '#fcd34d',
    flexShrink: 0,
  },
  code: {
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", monospace',
    background: 'rgba(245,158,11,.20)',
    borderRadius: 3,
    padding: '1px 4px',
    color: '#fde68a',
  },
  dismiss: {
    background: 'transparent',
    border: 'none',
    color: '#fde68a',
    cursor: 'pointer',
    fontSize: 14,
    flexShrink: 0,
  },
}

const skipLinkStyle: React.CSSProperties = {
  position: 'absolute',
  top: -40,
  left: 6,
  padding: '6px 10px',
  fontSize: 12,
  fontWeight: 600,
  color: ds.textPrimary,
  background: ds.surface,
  border: `1px solid var(--ds-border)`,
  borderRadius: 4,
  zIndex: 100,
  transitionProperty: 'top',
  transitionDuration: '0.15s',
  transitionTimingFunction: 'ease',
}

const TAB_LABELS: Record<ResourceTab, string> = {
  runs: 'Runs',
  deployments: 'Deployments',
  workflows: 'Workflows',
  agents: 'Agents',
  tools: 'Tools',
  mcpservers: 'MCP Servers',
  modelproviders: 'Providers',
  knowledgebases: 'Knowledge Bases',
  modelselectors: 'Model Selectors',
}

const OPS_TABS: ResourceTab[] = ['runs', 'deployments', 'workflows']
const CONFIG_TABS: ResourceTab[] = ['agents', 'tools', 'mcpservers', 'modelproviders', 'knowledgebases', 'modelselectors']

function isConfigTab(tab: ResourceTab): tab is 'agents' | 'tools' | 'mcpservers' | 'modelproviders' | 'knowledgebases' | 'modelselectors' {
  return CONFIG_TABS.includes(tab)
}

const HOME_TABS: { key: 'home' | 'marketplace'; label: string }[] = [
  { key: 'home', label: 'Home' },
  { key: 'marketplace', label: 'Marketplace' },
]

// Dashboard-style tabs that don't use the sidebar navigation.
const DASHBOARD_TABS: TopLevelTab[] = ['history', 'status', 'approvals', 'routing']

/** Polls the number of pending approvals for the nav badge. 0 when approvals are off. */
function usePendingApprovalCount(): number {
  const [count, setCount] = useState(0)
  useEffect(() => {
    let cancelled = false
    const poll = () => {
      listApprovals('pending')
        .then((a) => { if (!cancelled) setCount(a.length) })
        .catch(() => { if (!cancelled) setCount(0) })
    }
    poll()
    const id = setInterval(poll, 15000)
    return () => { cancelled = true; clearInterval(id) }
  }, [])
  return count
}

function App() {
  const [tab, setTab] = useState<TopLevelTab>(initialTab)
  const [selection, setSelection] = useState<ResourceSelection | null>(savedState.selection ?? null)
  // breadcrumbs holds the navigation stack of ancestors above the current selection.
  const [breadcrumbs, setBreadcrumbs] = useState<ResourceSelection[]>(savedState.breadcrumbs ?? [])
  // Track if we navigated from home so breadcrumbs show Home origin
  const [navigationOrigin, setNavigationOrigin] = useState<'sidebar' | 'home' | 'marketplace'>(savedState.navigationOrigin ?? 'sidebar')
  const [showCosts, setShowCosts] = useState(false)
  const pendingApprovals = usePendingApprovalCount()
  const [showRedisBanner, setShowRedisBanner] = useState(false)
  const [showSkip, setShowSkip] = useState(false)

  // Persist navigation state to sessionStorage on change (survives page refresh).
  useEffect(() => {
    saveNavState({ tab, selection, breadcrumbs, navigationOrigin })
  }, [tab, selection, breadcrumbs, navigationOrigin])
  const [theme, setTheme] = useState<Theme>(() => getPreferredTheme())

  useEffect(() => {
    getSystemStatus()
      .then((s) => { if (!s.stateConfigured) setShowRedisBanner(true) })
      .catch(() => {})
  }, [])

  const tabFor = (sel: ResourceSelection): ResourceTab => {
    switch (sel.kind) {
      case 'deployment': return 'deployments'
      case 'workflow': return 'workflows'
      case 'agent': return 'agents'
      case 'tool': return 'tools'
      case 'mcpserver': return 'mcpservers'
      case 'modelprovider': return 'modelproviders'
      case 'knowledgebase': return 'knowledgebases'
      case 'modelselector': return 'modelselectors'
      default: return 'runs'
    }
  }

  // When switching tabs, clear selection so main panel shows empty state.
  const handleTabSwitch = (t: ResourceTab) => {
    setTab(t)
    setSelection(null)
    setBreadcrumbs([])
    setShowCosts(false)
  }

  // Direct sidebar selection — clears breadcrumbs and sets origin to sidebar.
  const handleSelect = (sel: ResourceSelection) => {
    setSelection(sel)
    setBreadcrumbs([])
    setNavigationOrigin('sidebar')
    setShowCosts(false)
  }

  // Navigate to a resource tab from home cards (no selection).
  const handleNavigateToTab = (t: ResourceTab) => {
    setTab(t)
    setSelection(null)
    setBreadcrumbs([])
    setNavigationOrigin('home')
    setShowCosts(false)
  }

  // Navigate "into" a child resource, pushing the current selection onto the stack.
  const handleNavigateInto = (target: ResourceSelection) => {
    if (selection) setBreadcrumbs((prev) => [...prev, selection])
    setTab(tabFor(target))
    setSelection(target)
    setShowCosts(false)
  }

  // Jump back to a specific breadcrumb by index, discarding deeper levels.
  const handleNavigateUp = (index: number) => {
    if ((navigationOrigin === 'home' || navigationOrigin === 'marketplace') && index === 0) {
      // Navigate back to home/marketplace tab
      setTab(navigationOrigin)
      setSelection(null)
      setBreadcrumbs([])
      setNavigationOrigin('sidebar')
    } else {
      const target = breadcrumbs[index]
      setBreadcrumbs((prev) => prev.slice(0, index))
      setTab(tabFor(target))
      setSelection(target)
    }
    setShowCosts(false)
  }

  const namespace = ''

  return (
    <div style={layout.root}>
      {/* Skip to content link (better-accessibility §13) */}
      <a href="#main-content" style={{ ...skipLinkStyle, top: showSkip ? 6 : -40 }}
         onFocus={() => setShowSkip(true)} onBlur={() => setShowSkip(false)}
      >Skip to content</a>
      {/* ── Top bar ── */}
      <header style={layout.topbar}>
        <div style={layout.logo}>
          <img src={logoUrl} alt="agent-orca logo" style={layout.logoImg} />
        </div>
        <nav style={layout.nav}>
          {HOME_TABS.map((t) => (
            <button
              key={t.key}
              style={{ ...layout.navTab, ...(tab === t.key && !showCosts ? layout.navTabActive : {}) }}
              onClick={() => setTab(t.key)}
            >
              {t.label}
            </button>
          ))}
          <span style={layout.navDivider} />
          <button
            style={{ ...layout.navTab, ...(tab === 'history' && !showCosts ? layout.navTabActive : {}) }}
            onClick={() => { setTab('history'); setSelection(null); setBreadcrumbs([]); setShowCosts(false) }}
            aria-label="Run history"
            title="Past Runs"
          >
            <Icon icon={ICON.history} size={14} ariaHidden={true} /> History
          </button>
          <button
            style={{ ...layout.navTab, ...(tab === 'status' && !showCosts ? layout.navTabActive : {}) }}
            onClick={() => { setTab('status'); setSelection(null); setBreadcrumbs([]); setShowCosts(false) }}
            aria-label="System status"
            title="System Status"
          >
            <Icon icon={ICON.warning} size={14} ariaHidden={true} /> Status
          </button>
          <button
            style={{ ...layout.navTab, ...(tab === 'approvals' && !showCosts ? layout.navTabActive : {}) }}
            onClick={() => { setTab('approvals'); setSelection(null); setBreadcrumbs([]); setShowCosts(false) }}
            aria-label={pendingApprovals > 0 ? `Approvals, ${pendingApprovals} pending` : 'Approvals'}
            title="Requests waiting for a decision"
          >
            <Icon icon={ICON.approval} size={14} ariaHidden={true} /> Approvals
            {pendingApprovals > 0 && <span style={layout.navBadge}>{pendingApprovals}</span>}
          </button>
          <button
            style={{ ...layout.navTab, ...(tab === 'routing' && !showCosts ? layout.navTabActive : {}) }}
            onClick={() => { setTab('routing'); setSelection(null); setBreadcrumbs([]); setShowCosts(false) }}
            aria-label="Routing"
            title="How models are chosen, and what to change"
          >
            <Icon icon={ICON.chart} size={14} ariaHidden={true} /> Routing
          </button>
          <span style={layout.navDivider} />
          {OPS_TABS.map((t) => (
            <button
              key={t}
              style={{ ...layout.navTab, ...(tab === t && !showCosts ? layout.navTabActive : {}) }}
              onClick={() => handleTabSwitch(t)}
            >
              {TAB_LABELS[t]}
            </button>
          ))}
          <span style={layout.navDivider} />
          {CONFIG_TABS.map((t) => (
            <button
              key={t}
              style={{ ...layout.navTab, ...(tab === t && !showCosts ? layout.navTabActive : {}) }}
              onClick={() => handleTabSwitch(t)}
            >
              {TAB_LABELS[t]}
            </button>
          ))}
        </nav>
        <div style={layout.topbarRight}>
          <button
            style={{ ...layout.iconBtn, ...(showCosts ? layout.iconBtnActive : {}) }}
            onClick={() => setShowCosts((v) => !v)}
            aria-label={showCosts ? 'Hide cost dashboard' : 'Show cost dashboard'}
            title="Cost Dashboard"
          >
            <Icon icon={ICON.cost} size={16} />
          </button>
          <button
            style={layout.iconBtn}
            onClick={() => {
              const next = toggleTheme()
              setTheme(next)
            }}
            aria-label={theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'}
            title={theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'}
          >
            {theme === 'dark' ? <Icon icon={ICON.moon} size={16} /> : <Icon icon={ICON.sun} size={16} />}
          </button>
        </div>
      </header>

      {/* ── Redis banner ── */}
      {showRedisBanner && <RedisBanner onDismiss={() => setShowRedisBanner(false)} />}

      {/* ── Body ── */}
      <div style={layout.body}>
        {/* Left list panel - hidden on home/marketplace/history/status tabs */}
        {tab !== 'home' && tab !== 'marketplace' && tab !== 'history' && tab !== 'status' && tab !== 'approvals' && tab !== 'routing' && (
          <aside data-sidebar style={layout.leftPanel}>
            <div style={layout.listHeader}>
              <span style={layout.listTitle}>{TAB_LABELS[tab as ResourceTab]}</span>
            </div>
            <div style={layout.listScroll}>
              {isConfigTab(tab) ? (
                <ConfigList
                  tab={tab}
                  namespace={namespace}
                  selection={selection}
                  onSelect={handleSelect}
                />
              ) : (
                <ResourceList
                  tab={tab}
                  namespace={namespace}
                  selection={selection}
                  onSelect={handleSelect}
                />
              )}
            </div>
          </aside>
        )}

        {/* Main content */}
        <main id="main-content" tabIndex={-1} style={{
            ...layout.main,
            ...(tab === 'home' || tab === 'marketplace' ? { padding: 0 } : {}),
          }}>
          {showCosts ? (
            // CostDashboard expects the old SidebarSelection type — adapt.
            <CostDashboard
              onViewRuns={() => { setTab('history'); setSelection(null); setBreadcrumbs([]); setShowCosts(false) }}
              onNavigateToRun={(runName, ns) => {
                setTab('history')
                setShowCosts(false)
                setSelection({ kind: 'run', name: runName, namespace: ns })
              }}
              selection={
                selection
                  ? selection.kind === 'run'
                    ? { kind: 'run', name: selection.name }
                    : { kind: 'deployment', name: selection.name, namespace: selection.namespace }
                  : null
              }
            />
          ) : tab === 'home' ? (
            <SystemDashboard navigateToTab={handleNavigateToTab} />
          ) : tab === 'marketplace' ? (
            <MarketplaceView />
          ) : tab === 'history' && selection?.kind === 'run' ? (
            <RunHistoryDetailView
              runId={selection.name}
              namespace={selection.namespace}
              onBack={() => { setSelection(null); setBreadcrumbs([]) }}
              onNavigateToRun={(runName, ns) => setSelection({ kind: 'run', name: runName, namespace: ns })}
            />
          ) : tab === 'history' ? (
            <RunHistoryView
              onNavigateToRun={(runName, ns) => {
                setSelection({ kind: 'run', name: runName, namespace: ns })
              }}
            />
          ) : tab === 'approvals' ? (
            <ApprovalsView
              onNavigateToRun={(runName, ns) => {
                setTab('runs')
                setSelection({ kind: 'run', name: runName, namespace: ns })
              }}
            />
          ) : tab === 'routing' ? (
            <RoutingInsightsView />
          ) : tab === 'status' ? (
            <SystemStatusPage navigateToTab={(t) => { setTab(t as TopLevelTab); setSelection(null); setBreadcrumbs([]); setShowCosts(false) }} />
          ) : selection?.kind === 'run' ? (
            <RunView
              runId={selection.name}
              namespace={selection.namespace}
              breadcrumbs={breadcrumbs}
              cameFromHome={navigationOrigin === 'home'}
              onNavigateUp={handleNavigateUp}
              onNavigateToRun={(runName, ns) =>
                handleNavigateInto({ kind: 'run', name: runName, namespace: ns })
              }
            />
          ) : selection?.kind === 'deployment' ? (
            <DeploymentView
              namespace={selection.namespace}
              name={selection.name}
              onNavigateToRun={(runName, ns) =>
                handleNavigateInto({ kind: 'run', name: runName, namespace: ns })
              }
            />
          ) : selection?.kind === 'workflow' ? (
            <WorkflowView namespace={selection.namespace} name={selection.name} />
          ) : selection && (selection.kind === 'agent' || selection.kind === 'tool' || selection.kind === 'mcpserver' || selection.kind === 'modelprovider' || selection.kind === 'knowledgebase' || selection.kind === 'modelselector') ? (
            <ConfigDetailView selection={selection} />
          ) : (
            <EmptyState tab={tab as ResourceTab} />
          )}
        </main>
      </div>
    </div>
  )
}

function EmptyState({ tab }: { tab: ResourceTab }) {
  const icons: Record<ResourceTab, IconComponent> = {
    runs: ICON.runs,
    deployments: ICON.deployment,
    workflows: ICON.workflow,
    agents: ICON.agent,
    tools: ICON.tool,
    mcpservers: ICON.mcpserver,
    modelproviders: ICON.modelprovider,
    knowledgebases: ICON.knowledgebase,
    modelselectors: ICON.modelselector,
  }
  const messages: Record<ResourceTab, string> = {
    runs: 'Select a run to see its output',
    deployments: 'Select a deployment to start chatting',
    workflows: 'Select a workflow to inspect its steps',
    agents: 'Select an agent to view its configuration',
    tools: 'Select a tool to view its details',
    mcpservers: 'Select an MCP server to view its details',
    modelproviders: 'Select a model provider to view its details',
    knowledgebases: 'Select a knowledge base to view its details',
    modelselectors: 'Select a model selector to view its routing configuration',
  }
  return (
    <div style={empty.root}>
      <div style={empty.icon}><Icon icon={icons[tab]} size={32} strokeWidth={1.25} /></div>
      <div style={empty.title}>{messages[tab]}</div>
    </div>
  )
}

const empty: Record<string, React.CSSProperties> = {
  root: {
    flex: 1,
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'center',
    justifyContent: 'center',
    gap: 12,
    color: ds.textMuted,
  },
  icon: { fontSize: 32, opacity: 0.4 },
  title: { fontSize: 15, fontWeight: 500, color: 'var(--ds-text-muted)' },
}

const layout: Record<string, React.CSSProperties> = {
  root: {
    display: 'flex',
    flexDirection: 'column',
    height: '100vh',
    background: ds.bg,
    color: ds.textPrimary,
    fontFamily: 'system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif',
    overflow: 'hidden',
  },
  topbar: {
    height: 52,
    background: ds.surface,
    borderBottom: '1px solid var(--ds-border)',
    display: 'flex',
    alignItems: 'center',
    padding: '0 20px',
    gap: 8,
    flexShrink: 0,
    zIndex: 10,
  },
  logo: {
    fontSize: 15,
    fontWeight: 700,
    color: ds.textPrimary,
    letterSpacing: '-0.3px',
    display: 'flex',
    alignItems: 'center',
    gap: 8,
    marginRight: 16,
  },
  logoImg: {
    // Size by height, let width follow the logo's natural aspect ratio, and
    // never let flex shrink squeeze it. object-fit:contain guards against any
    // aspect-ratio mismatch (the logo is a wide 768x613 PNG).
    height: 28,
    width: 'auto',
    flexShrink: 0,
    objectFit: 'contain',
  },
  nav: { display: 'flex', gap: 4, alignItems: 'center' },
  // Group with space, not lines (better-layout §1)
  navDivider: {
    width: 1,
    height: 20,
    background: 'var(--ds-border)',
    margin: '0 6px',
    flexShrink: 0,
  },
  navTab: {
    padding: '6px 14px',
    borderRadius: 6,
    cursor: 'pointer',
    fontSize: 13,
    fontWeight: 500,
    color: ds.textSecondary,
    border: 'none',
    background: 'none',
    transitionProperty: 'background-color, color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  navTabActive: {
    color: ds.accent,
    background: 'rgba(59,130,246,.15)',
  },
  navBadge: {
    marginLeft: 6,
    minWidth: 18,
    padding: '0 5px',
    borderRadius: 9,
    background: ds.warning,
    color: '#111',
    fontSize: 11,
    fontWeight: 700,
    lineHeight: '18px',
    textAlign: 'center' as const,
    fontVariantNumeric: 'tabular-nums',
  },
  topbarRight: {
    marginLeft: 'auto',
    display: 'flex',
    alignItems: 'center',
    gap: 6,
  },
  iconBtn: {
    width: 32,
    height: 32,
    borderRadius: 6,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    cursor: 'pointer',
    color: ds.textSecondary,
    border: 'none',
    background: 'none',
    fontSize: 16,
    transitionProperty: 'background-color, color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  iconBtnActive: {
    color: ds.accent,
    background: 'rgba(59,130,246,.15)',
  },
  body: {
    display: 'flex',
    flex: 1,
    overflow: 'hidden',
  },
  leftPanel: {
    width: 240,
    flexShrink: 0,
    borderRight: '1px solid var(--ds-border)',
    display: 'flex',
    flexDirection: 'column',
    overflow: 'hidden',
  },
  listHeader: {
    padding: '12px 14px 8px',
    borderBottom: '1px solid var(--ds-border)',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
  },
  listTitle: {
    fontSize: 11,
    fontWeight: 600,
    textTransform: 'uppercase',
    letterSpacing: '0.08em',
    color: ds.textSecondary,
  },
  listScroll: {
    flex: 1,
    overflowY: 'auto',
  },
  main: {
    flex: 1,
    overflow: 'hidden',
    display: 'flex',
    flexDirection: 'column',
  },
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
