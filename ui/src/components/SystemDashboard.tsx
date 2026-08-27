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
import { PHASE_COLOR } from '../lib/phaseColors'
import { ICON } from '../lib/icons'
import type { ResourceTab } from './ResourceList'
import { DESIGN } from '../lib/designSystem'

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
      <div style={s.root}>
        <div style={s.hero}>
          <div style={s.skeletonLine} />
          <div style={{ ...s.skeletonLine, width: '60%', marginTop: 4 }} />
        </div>
        <h2 style={s.sectionTitle}>System Overview</h2>
        <div style={s.cardsGrid}>
          {Array.from({ length: 9 }).map((_, i) => (
            <div key={i} style={s.cardSkeleton} />
          ))}
        </div>
        <h2 style={s.sectionTitle}>Total Cost</h2>
        <div style={s.cardSkeleton} />
      </div>
    )
  }

  return (
    <div style={s.root}>
      {/* Hero Header */}
      <div style={s.hero}>
        <div>
          <h1 style={s.heroTitle}>Agent Orcastrator</h1>
          <p style={s.heroSubtitle}>AI Agent Orchestration Platform</p>
        </div>
      </div>

      {/* Resource Cards Grid */}
      <div style={s.section}>
        <h2 style={s.sectionTitle}>System Overview</h2>
        <div style={s.cardsGrid}>
          <ResourceCard
            title="Agents"
            icon={ICON.agent}
            total={stats.agents.total}
            statuses={[
              { label: 'Ready', count: stats.agents.ready, color: '#22c55e' },
              { label: 'Issues', count: stats.agents.total - stats.agents.ready, color: '#ef4444' },
            ]}
            onClick={() => navigateToTab('agents')}
          />
          <ResourceCard
            title="Agent Runs"
            icon={ICON.runs}
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
            icon={ICON.deployment}
            total={stats.deployments.total}
            statuses={[
              { label: 'Running', count: stats.deployments.running, color: '#3b82f6' },
              { label: 'Paused', count: stats.deployments.paused, color: 'var(--ds-text-muted)' },
              { label: 'Failed', count: stats.deployments.failed, color: '#ef4444' },
            ]}
            onClick={() => navigateToTab('deployments')}
          />
          <ResourceCard
            title="Workflows"
            icon={ICON.workflow}
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
            icon={ICON.mcpserver}
            total={stats.mcpservers.total}
            statuses={[
              { label: 'Ready', count: stats.mcpservers.ready, color: '#22c55e' },
              { label: 'Failed', count: stats.mcpservers.total - stats.mcpservers.ready, color: '#ef4444' },
            ]}
            onClick={() => navigateToTab('mcpservers')}
          />
          <ResourceCard
            title="Knowledge Bases"
            icon={ICON.knowledgebase}
            total={stats.knowledgebases.total}
            statuses={[
              { label: 'Ready', count: stats.knowledgebases.ready, color: '#22c55e' },
              { label: 'Pending', count: stats.knowledgebases.total - stats.knowledgebases.ready, color: '#f59e0b' },
            ]}
            onClick={() => navigateToTab('knowledgebases')}
          />
          <ResourceCard
            title="Tools"
            icon={ICON.tool}
            total={stats.tools.total}
            statuses={[
              { label: 'Ready', count: stats.tools.ready, color: '#22c55e' },
              { label: 'Failed', count: stats.tools.total - stats.tools.ready, color: '#ef4444' },
            ]}
            onClick={() => navigateToTab('tools')}
          />
          <ResourceCard
            title="Model Providers"
            icon={ICON.modelprovider}
            total={stats.modelproviders.total}
            statuses={[
              { label: 'Ready', count: stats.modelproviders.ready, color: '#22c55e' },
              { label: 'Failed', count: stats.modelproviders.total - stats.modelproviders.ready, color: '#ef4444' },
            ]}
            onClick={() => navigateToTab('modelproviders')}
          />
          <ResourceCard
            title="Model Selectors"
            icon={ICON.modelselector}
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
    padding: `${DESIGN.space.xl} ${DESIGN.space.xxl}`,
    display: 'flex',
    flexDirection: 'column',
    gap: DESIGN.space.xxl,
    background: 'var(--ds-bg)',
  },
  loading: {
    flex: 1,
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'center',
    justifyContent: 'center',
    gap: 16,
    color: 'var(--ds-text-muted)',
    fontSize: 14,
  },
  skeletonLine: {
    background: 'rgba(148,163,184,.15)',
    borderRadius: DESIGN.radii.md,
    height: 24,
    width: '40%',
    animation: 'aoPulse 1.5s ease infinite',
  },
  cardSkeleton: {
    background: 'rgba(148,163,184,.1)',
    borderRadius: DESIGN.radii.lg,
    height: 120,
    width: '100%',
    animation: 'aoPulse 1.5s ease infinite',
    willChange: 'opacity',
  },
  hero: {
    display: 'flex',
    flexDirection: 'column',
    gap: 8,
    padding: DESIGN.space.xxl,
    background: 'linear-gradient(135deg, var(--ds-surface) 0%, var(--ds-bg) 100%)',
    borderRadius: DESIGN.radii.xl,
    border: `1px solid var(--ds-border)`,
    boxShadow: 'var(--ds-card-shadow)',
  },
  heroTitle: {
    fontSize: DESIGN.font.size.display,
    fontWeight: 700,
    color: 'var(--ds-text-primary)',
    margin: 0,
    letterSpacing: '-0.5px',
    lineHeight: 1.1,
    textWrap: 'balance' as const,
  },
  heroSubtitle: {
    fontSize: 16,
    color: 'var(--ds-text-secondary)',
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
    color: 'var(--ds-text-primary)',
    margin: 0,
  },
  cardsGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))',
    gap: 16,
  },
  costCard: {
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.xl,
    padding: 20,
    display: 'flex',
    alignItems: 'baseline',
    gap: 8,
    boxShadow: 'var(--ds-card-shadow)',
  },
  costValue: {
    fontSize: DESIGN.font.size.cost,
    fontWeight: 700,
    color: 'var(--ds-success)',
    fontVariantNumeric: 'tabular-nums',
  },
  costLabel: {
    fontSize: 14,
    color: 'var(--ds-text-secondary)',
  },
}
