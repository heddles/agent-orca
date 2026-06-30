/**
 * SystemDashboard — card-based overview showing system resource status.
 * Click any card to navigate to that resource's list view.
 */
import { useEffect, useState } from 'react'
import {
  listAgents,
  listTools,
  listMCPServers,
  listModelProviders,
  listDeployments,
  listRuns,
  getCosts,
  listKnowledgeBases,
  listModelSelectors,
  listWorkflows,
  type AgentSummary,
  type ToolSummary,
  type MCPServerSummary,
  type ModelProviderSummary,
  type KnowledgeBaseSummary,
  type ModelSelectorSummary,
  type AgentDeploymentSummary,
  type AgentRunSummary,
  type AgentWorkflowSummary,
  type CostData,
} from '../api/sse'
import { ResourceCard } from './ResourceCard'
import type { ResourceTab } from './ResourceList'

interface Props {
  navigateToTab: (tab: ResourceTab) => void
}

interface Stats {
  agents: { total: number; ready: number }
  tools: { total: number; ready: number }
  mcpservers: { total: number; ready: number }
  modelproviders: { total: number; ready: number }
  knowledgebases: { total: number; ready: number }
  modelselectors: { total: number }
  workflows: { total: number; running: number; succeeded: number; failed: number }
  deployments: { total: number; running: number; paused: number; failed: number }
  runs: { total: number; running: number; pending: number; succeeded: number; failed: number }
  cost: string
}

export function SystemDashboard({ navigateToTab }: Props) {
  const [stats, setStats] = useState<Stats | null>(null)

  useEffect(() => {
    const load = async () => {
      // Use individual try-catch blocks for each API call to prevent one failure from breaking everything
      let agentsList: AgentSummary[] | undefined
      let toolsList: ToolSummary[] | undefined
      let mcpList: MCPServerSummary[] | undefined
      let providerList: ModelProviderSummary[] | undefined
      let deployList: AgentDeploymentSummary[] | undefined
      let runList: AgentRunSummary[] | undefined
      let costData: CostData | undefined
      let kbList: KnowledgeBaseSummary[] | undefined
      let selectorList: ModelSelectorSummary[] | undefined
      let workflowList: AgentWorkflowSummary[] | undefined

      try { agentsList = await listAgents() } catch { agentsList = [] }
      try { toolsList = await listTools() } catch { toolsList = [] }
      try { mcpList = await listMCPServers() } catch { mcpList = [] }
      try { providerList = await listModelProviders() } catch { providerList = [] }
      try { deployList = await listDeployments() } catch { deployList = [] }
      try { runList = await listRuns() } catch { runList = [] }
      try { costData = await getCosts() } catch { costData = { totalUSD: '0.00', byAgent: {}, byModel: {}, byDay: [] } }
      try { kbList = await listKnowledgeBases() } catch { kbList = [] }
      try { selectorList = await listModelSelectors() } catch { selectorList = [] }
      try { workflowList = await listWorkflows() } catch { workflowList = [] }

      const toolNames = new Set(toolsList?.map(t => t.name) ?? [])
      const providerNames = new Set(providerList?.map(p => p.name) ?? [])

      const agentReadyCount = agentsList?.filter(a => {
        const tools = a.tools ?? []
        const hasTools = tools.every(t => toolNames.has(t))
        const hasProvider = a.modelSelectorRef ? providerNames.has(a.modelSelectorRef) : false
        return hasTools && hasProvider
      }).length ?? 0

      setStats({
        agents: {
          total: agentsList?.length ?? 0,
          ready: agentReadyCount,
        },
        tools: {
          total: toolsList?.length ?? 0,
          ready: toolsList?.filter(t => t.ready).length ?? 0,
        },
        mcpservers: {
          total: mcpList?.length ?? 0,
          ready: mcpList?.filter(m => m.ready).length ?? 0,
        },
        modelproviders: {
          total: providerList?.length ?? 0,
          ready: providerList?.filter(p => p.ready).length ?? 0,
        },
        knowledgebases: {
          total: kbList?.length ?? 0,
          ready: kbList?.filter(k => k.ready).length ?? 0,
        },
        modelselectors: {
          total: selectorList?.length ?? 0,
        },
        workflows: {
          total: workflowList?.length ?? 0,
          running: workflowList?.filter((w) => w.phase === 'Running').length ?? 0,
          succeeded: workflowList?.filter((w) => w.phase === 'Succeeded').length ?? 0,
          failed: workflowList?.filter((w) => w.phase === 'Failed').length ?? 0,
        },
        deployments: {
          total: deployList?.length ?? 0,
          running: deployList?.filter((d) => d.phase === 'Running' || d.phase === 'Creating').length ?? 0,
          paused: deployList?.filter((d) => d.phase === 'Paused').length ?? 0,
          failed: deployList?.filter((d) => d.phase === 'Failed').length ?? 0,
        },
        runs: {
          total: runList?.length ?? 0,
          running: runList?.filter((r) => r.phase === 'Running').length ?? 0,
          pending: runList?.filter((r) => r.phase === 'Pending' || r.phase === 'WaitingForInput').length ?? 0,
          succeeded: runList?.filter((r) => r.phase === 'Succeeded' || r.phase === 'HandedOff').length ?? 0,
          failed: runList?.filter((r) => r.phase === 'Failed').length ?? 0,
        },
        cost: costData?.totalUSD ?? '0.00',
      })
    }
    load()
    const interval = setInterval(load, 3000)
    return () => clearInterval(interval)
  }, [])

  if (!stats) {
    return (
      <div style={s.loading}>
        <div style={s.loadingSpinner} />
        <span>Loading system overview…</span>
      </div>
    )
  }

  return (
    <div style={s.root}>
      {/* Hero Header */}
      <div style={s.hero}>
        <div>
          <h1 style={s.heroTitle}>agent-orc</h1>
          <p style={s.heroSubtitle}>AI Agent Orchestration Platform</p>
        </div>
      </div>

      {/* Resource Cards Grid */}
      <div style={s.section}>
        <h2 style={s.sectionTitle}>System Overview</h2>
        <div style={s.cardsGrid}>
          <ResourceCard
            title="Agents"
            icon="🤖"
            total={stats.agents.total}
            statuses={[
              { label: 'Ready', count: stats.agents.ready, color: '#22c55e' },
              { label: 'Issues', count: stats.agents.total - stats.agents.ready, color: '#ef4444' },
            ]}
            onClick={() => navigateToTab('agents')}
          />
          <ResourceCard
            title="Agent Runs"
            icon="▶"
            total={stats.runs.total}
            statuses={[
              { label: 'Running', count: stats.runs.running, color: '#3b82f6' },
              { label: 'Pending', count: stats.runs.pending, color: '#f59e0b' },
              { label: 'Succeeded', count: stats.runs.succeeded, color: '#22c55e' },
              { label: 'Failed', count: stats.runs.failed, color: '#ef4444' },
            ]}
            onClick={() => navigateToTab('runs')}
          />
          <ResourceCard
            title="Deployments"
            icon="⚡"
            total={stats.deployments.total}
            statuses={[
              { label: 'Running', count: stats.deployments.running, color: '#3b82f6' },
              { label: 'Paused', count: stats.deployments.paused, color: '#64748b' },
              { label: 'Failed', count: stats.deployments.failed, color: '#ef4444' },
            ]}
            onClick={() => navigateToTab('deployments')}
          />
          <ResourceCard
            title="Workflows"
            icon="⟳"
            total={stats.workflows.total}
            statuses={[
              { label: 'Running', count: stats.workflows.running, color: '#3b82f6' },
              { label: 'Succeeded', count: stats.workflows.succeeded, color: '#22c55e' },
              { label: 'Failed', count: stats.workflows.failed, color: '#ef4444' },
            ]}
            onClick={() => navigateToTab('workflows')}
          />
          <ResourceCard
            title="MCP Servers"
            icon="🔌"
            total={stats.mcpservers.total}
            statuses={[
              { label: 'Ready', count: stats.mcpservers.ready, color: '#22c55e' },
              { label: 'Failed', count: stats.mcpservers.total - stats.mcpservers.ready, color: '#ef4444' },
            ]}
            onClick={() => navigateToTab('mcpservers')}
          />
          <ResourceCard
            title="Knowledge Bases"
            icon="📚"
            total={stats.knowledgebases.total}
            statuses={[
              { label: 'Ready', count: stats.knowledgebases.ready, color: '#22c55e' },
              { label: 'Pending', count: stats.knowledgebases.total - stats.knowledgebases.ready, color: '#f59e0b' },
            ]}
            onClick={() => navigateToTab('knowledgebases')}
          />
          <ResourceCard
            title="Tools"
            icon="🔧"
            total={stats.tools.total}
            statuses={[
              { label: 'Ready', count: stats.tools.ready, color: '#22c55e' },
              { label: 'Failed', count: stats.tools.total - stats.tools.ready, color: '#ef4444' },
            ]}
            onClick={() => navigateToTab('tools')}
          />
          <ResourceCard
            title="Model Providers"
            icon="🧠"
            total={stats.modelproviders.total}
            statuses={[
              { label: 'Ready', count: stats.modelproviders.ready, color: '#22c55e' },
              { label: 'Failed', count: stats.modelproviders.total - stats.modelproviders.ready, color: '#ef4444' },
            ]}
            onClick={() => navigateToTab('modelproviders')}
          />
          <ResourceCard
            title="Model Selectors"
            icon="🔀"
            total={stats.modelselectors.total}
            statuses={[]}
            onClick={() => navigateToTab('modelselectors')}
          />
        </div>
      </div>

      {/* Cost Summary */}
      <div style={s.section}>
        <h2 style={s.sectionTitle}>Total Cost</h2>
        <div style={s.costCard}>
          <span style={s.costValue}>${stats.cost}</span>
          <span style={s.costLabel}>USD</span>
        </div>
      </div>
    </div>
  )
}

const s: Record<string, React.CSSProperties> = {
  root: {
    flex: 1,
    overflowY: 'auto',
    padding: 32,
    display: 'flex',
    flexDirection: 'column',
    gap: 32,
    background: '#0f172a',
  },
  loading: {
    flex: 1,
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'center',
    justifyContent: 'center',
    gap: 16,
    color: '#64748b',
    fontSize: 14,
  },
  loadingSpinner: {
    width: 32,
    height: 32,
    border: '2px solid #334155',
    borderTopColor: '#3b82f6',
    borderRadius: '50%',
    animation: 'aoPulse 1s linear infinite',
  },
  hero: {
    display: 'flex',
    flexDirection: 'column',
    gap: 8,
    padding: 32,
    background: 'linear-gradient(135deg, #1e293b 0%, #0f172a 100%)',
    borderRadius: 16,
    border: '1px solid #334155',
  },
  heroTitle: {
    fontSize: 36,
    fontWeight: 700,
    color: '#f1f5f9',
    margin: 0,
    letterSpacing: '-0.5px',
  },
  heroSubtitle: {
    fontSize: 16,
    color: '#94a3b8',
    margin: '4px 0 0',
  },
  section: {
    display: 'flex',
    flexDirection: 'column',
    gap: 12,
  },
  sectionTitle: {
    fontSize: 16,
    fontWeight: 600,
    color: '#f1f5f9',
    margin: 0,
  },
  cardsGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))',
    gap: 16,
  },
  costCard: {
    background: '#1e293b',
    border: '1px solid #334155',
    borderRadius: 12,
    padding: 20,
    display: 'flex',
    alignItems: 'baseline',
    gap: 8,
  },
  costValue: {
    fontSize: 32,
    fontWeight: 700,
    color: '#f1f5f9',
  },
  costLabel: {
    fontSize: 14,
    color: '#94a3b8',
  },
}