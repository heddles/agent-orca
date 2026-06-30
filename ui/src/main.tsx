import { StrictMode, useEffect, useState } from 'react'
import { createRoot } from 'react-dom/client'
import { getSystemStatus } from './api/sse'
import { ResourceList, type ResourceSelection, type ResourceTab } from './components/ResourceList'
import { ConfigList } from './components/ConfigList'
import { ConfigDetailView } from './components/ConfigDetailView'
import { RunView } from './components/RunView'
import { DeploymentView } from './components/DeploymentView'
import { WorkflowView } from './components/WorkflowView'
import { CostDashboard } from './components/CostDashboard'
import { CreateAgentPanel } from './components/CreateAgentPanel'
import { SystemDashboard } from './components/SystemDashboard'
import { MarketplaceView } from './components/MarketplaceView'

type TopLevelTab = ResourceTab | 'home' | 'marketplace'

// Inject global keyframe animations (used by StatusBadge, OutputCard pulse cursors).
const styleEl = document.createElement('style')
styleEl.textContent = `
  @keyframes aoPulse {
    0%, 100% { opacity: 1; transform: scale(1); }
    50% { opacity: 0.5; transform: scale(0.8); }
  }
  .ao-md table { border-collapse: collapse; width: 100%; margin: 12px 0; font-size: 13px; }
  .ao-md th, .ao-md td { border: 1px solid #334155; padding: 6px 12px; text-align: left; vertical-align: top; }
  .ao-md th { background: #1e293b; color: #94a3b8; font-weight: 600; font-size: 11px; text-transform: uppercase; letter-spacing: 0.06em; }
  .ao-md tr:nth-child(even) td { background: rgba(51,65,85,.25); }
  .ao-md p { margin: 0 0 8px; }
  .ao-md p:last-child { margin-bottom: 0; }
  .ao-md h1, .ao-md h2, .ao-md h3 { color: #f1f5f9; margin: 16px 0 8px; }
  .ao-md code { background: rgba(51,65,85,.6); border-radius: 3px; padding: 1px 5px; font-family: monospace; font-size: 12px; }
  .ao-md pre { background: rgba(15,23,42,.8); border: 1px solid #334155; border-radius: 6px; padding: 12px; overflow-x: auto; }
  .ao-md pre code { background: none; padding: 0; }
  .ao-md ul, .ao-md ol { padding-left: 20px; margin: 8px 0; }
`
document.head.appendChild(styleEl)

function RedisBanner({ onDismiss }: { onDismiss: () => void }) {
  return (
    <div style={banner.root}>
      <span>⚠</span>
      <span style={{ flex: 1 }}>
        <strong>Redis is not configured.</strong> Cost tracking will show $0 and conversation
        history will not persist across pod restarts. Set{' '}
        <code style={banner.code}>STATE_BACKEND=redis</code> and{' '}
        <code style={banner.code}>REDIS_URL</code> on the operator to enable these features.
      </span>
      <button style={banner.dismiss} onClick={onDismiss} aria-label="Dismiss">✕</button>
    </div>
  )
}

const banner: Record<string, React.CSSProperties> = {
  root: {
    display: 'flex',
    alignItems: 'center',
    gap: 10,
    padding: '8px 20px',
    background: '#78350f',
    borderBottom: '1px solid #92400e',
    fontSize: 13,
    color: '#fef3c7',
    flexShrink: 0,
  },
  code: {
    fontFamily: 'monospace',
    background: '#92400e',
    borderRadius: 3,
    padding: '1px 4px',
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

function App() {
  const [tab, setTab] = useState<TopLevelTab>('home')
  const [selection, setSelection] = useState<ResourceSelection | null>(null)
  // breadcrumbs holds the navigation stack of ancestors above the current selection.
  const [breadcrumbs, setBreadcrumbs] = useState<ResourceSelection[]>([])
  // Track if we navigated from home so breadcrumbs show Home origin
  const [navigationOrigin, setNavigationOrigin] = useState<'sidebar' | 'home' | 'marketplace'>('sidebar')
  const [showCosts, setShowCosts] = useState(false)
  const [showCreateAgent, setShowCreateAgent] = useState(false)
  const [showRedisBanner, setShowRedisBanner] = useState(false)

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
      {/* ── Top bar ── */}
      <header style={layout.topbar}>
        <div style={layout.logo}>
          <div style={layout.logoDot} />
          agent-orc
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
            title="Cost Dashboard"
          >
            💰
          </button>
          <button
            style={layout.iconBtn}
            onClick={() => setShowCreateAgent(true)}
            title="Create Agent"
          >
            ＋
          </button>
        </div>
      </header>

      {/* ── Redis banner ── */}
      {showRedisBanner && <RedisBanner onDismiss={() => setShowRedisBanner(false)} />}

      {/* ── Body ── */}
      <div style={layout.body}>
        {/* Left list panel - hidden on home/marketplace tabs */}
        {tab !== 'home' && tab !== 'marketplace' && (
          <aside style={layout.leftPanel}>
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
        <main style={{
            ...layout.main,
            ...(tab === 'home' || tab === 'marketplace' ? { padding: 0 } : {}),
          }}>
          {showCosts ? (
            // CostDashboard expects the old SidebarSelection type — adapt.
            <CostDashboard
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

      {/* Create Agent panel */}
      {showCreateAgent && (
        <CreateAgentPanel
          onCreated={() => setShowCreateAgent(false)}
          onClose={() => setShowCreateAgent(false)}
        />
      )}
    </div>
  )
}

function EmptyState({ tab }: { tab: ResourceTab }) {
  const icons: Record<ResourceTab, string> = {
    runs: '▶',
    deployments: '⚡',
    workflows: '⟳',
    agents: '🤖',
    tools: '🔧',
    mcpservers: '🔌',
    modelproviders: '🧠',
    knowledgebases: '📚',
    modelselectors: '🔀',
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
      <div style={empty.icon}>{icons[tab]}</div>
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
    color: '#475569',
  },
  icon: { fontSize: 32, opacity: 0.4 },
  title: { fontSize: 15, fontWeight: 500, color: '#64748b' },
}

const layout: Record<string, React.CSSProperties> = {
  root: {
    display: 'flex',
    flexDirection: 'column',
    height: '100vh',
    background: '#0f172a',
    color: '#e2e8f0',
    fontFamily: 'system-ui, -apple-system, sans-serif',
    overflow: 'hidden',
  },
  topbar: {
    height: 52,
    background: '#1e293b',
    borderBottom: '1px solid #334155',
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
    color: '#f1f5f9',
    letterSpacing: '-0.3px',
    display: 'flex',
    alignItems: 'center',
    gap: 8,
    marginRight: 16,
  },
  logoDot: {
    width: 8,
    height: 8,
    background: '#3b82f6',
    borderRadius: '50%',
    boxShadow: '0 0 8px #3b82f6',
  },
  nav: { display: 'flex', gap: 2, alignItems: 'center' },
  navDivider: {
    width: 1,
    height: 20,
    background: '#334155',
    margin: '0 6px',
    flexShrink: 0,
  },
  navTab: {
    padding: '6px 14px',
    borderRadius: 6,
    cursor: 'pointer',
    fontSize: 13,
    fontWeight: 500,
    color: '#94a3b8',
    border: 'none',
    background: 'none',
    transition: 'all 0.15s',
  },
  navTabActive: {
    color: '#3b82f6',
    background: 'rgba(59,130,246,.15)',
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
    color: '#94a3b8',
    border: 'none',
    background: 'none',
    fontSize: 16,
    transition: 'all 0.15s',
  },
  iconBtnActive: {
    color: '#3b82f6',
    background: 'rgba(59,130,246,.15)',
  },
  body: {
    display: 'flex',
    flex: 1,
    overflow: 'hidden',
  },
  leftPanel: {
    width: 220,
    flexShrink: 0,
    borderRight: '1px solid #334155',
    display: 'flex',
    flexDirection: 'column',
    overflow: 'hidden',
  },
  listHeader: {
    padding: '12px 14px 8px',
    borderBottom: '1px solid #334155',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
  },
  listTitle: {
    fontSize: 11,
    fontWeight: 600,
    textTransform: 'uppercase',
    letterSpacing: '0.08em',
    color: '#94a3b8',
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
