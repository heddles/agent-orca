/**
 * ConfigDetailView — read-only detail panel for configuration resources.
 * Displays key-value metadata for the selected Agent, Tool, MCPServer, or ModelProvider.
 */
import { useEffect, useState } from 'react'
import {
  listAgents,
  listTools,
  listMCPServers,
  listModelProviders,
  listKnowledgeBases,
  listModelSelectors,
  type AgentSummary,
  type ToolSummary,
  type MCPServerSummary,
  type MCPTool,
  type ModelProviderSummary,
  type KnowledgeBaseSummary,
  type ModelSelectorSummary,
} from '../api/sse'
import type { ResourceSelection } from './ResourceList'
import { DESIGN } from '../lib/designSystem'
import { Icon, ICON } from '../lib/icons'

type ConfigSelection = Extract<ResourceSelection, { kind: 'agent' | 'tool' | 'mcpserver' | 'modelprovider' | 'knowledgebase' | 'modelselector' }>

interface Props {
  selection: ConfigSelection
}

function Field({ label, value }: { label: string; value: React.ReactNode }) {
  if (value === undefined || value === null || value === '') return null
  return (
    <div style={s.field}>
      <div style={s.fieldLabel}>{label}</div>
      <div style={s.fieldValue}>{value}</div>
    </div>
  )
}

function ReadyBadge({ ready }: { ready: boolean }) {
  return (
    <span style={{ ...s.badge, background: ready ? 'rgba(34,197,94,.15)' : 'rgba(239,68,68,.15)', color: ready ? 'var(--ds-success)' : 'var(--ds-error)' }}>
      {ready ? 'Ready' : 'Not Ready'}
    </span>
  )
}

function Tags({ items }: { items: string[] }) {
  if (!items || items.length === 0) return <span style={s.muted}>none</span>
  return (
    <div style={s.tags}>
      {items.map((t) => <span key={t} style={s.tag}>{t}</span>)}
    </div>
  )
}

function AgentDetail({ data, mcpServers }: { data: AgentSummary; mcpServers: MCPServerSummary[] }) {
  const accessibleMCPs = mcpServers
    .filter((m) => m.allowedAgents.includes(data.name))
    .map((m) => m.name)

  return (
    <>
      <Field label="Model Selector" value={data.modelSelectorRef} />
      <Field label="Framework" value={data.framework} />
      <Field label="Tools" value={<Tags items={data.tools || []} />} />
      <Field label="MCP Server Access" value={
        accessibleMCPs.length > 0
          ? <Tags items={accessibleMCPs} />
          : <span style={s.muted}>none</span>
      } />
      <Field label="Service Account" value={data.serviceAccountName} />
      {data.systemPrompt && (
        <div style={s.field}>
          <div style={s.fieldLabel}>System Prompt</div>
          <pre style={s.pre}>{data.systemPrompt}</pre>
        </div>
      )}
    </>
  )
}

function ToolDetail({ data }: { data: ToolSummary }) {
  return (
    <>
      <Field label="Status" value={<ReadyBadge ready={data.ready} />} />
      <Field label="Type" value={data.type} />
      <Field label="Execution Mode" value={data.executionMode} />
      {data.ociRef && <Field label="OCI Image" value={<code style={s.code}>{data.ociRef}</code>} />}
      {data.agentRef && <Field label="Agent Ref" value={data.agentRef} />}
      {data.description && <Field label="Description" value={data.description} />}
    </>
  )
}

function MCPToolList({ tools }: { tools: MCPTool[] }) {
  const formatted = JSON.stringify(tools, null, 2)
  return (
    <div style={s.field}>
      <div style={s.fieldLabel}>Tools</div>
      <pre style={s.pre}>{formatted}</pre>
    </div>
  )
}

function MCPServerDetail({ data }: { data: MCPServerSummary }) {
  return (
    <>
      <Field label="Status" value={<ReadyBadge ready={data.ready} />} />
      <Field label="Transport" value={data.transport} />
      {data.url && <Field label="URL" value={data.url} />}
      {data.ociRef && <Field label="OCI Image" value={<code style={s.code}>{data.ociRef}</code>} />}
      <Field label="Tool Count" value={data.toolCount} />
      <Field label="Allowed Agents" value={
        data.allowedAgents.length > 0
          ? <Tags items={data.allowedAgents} />
          : <span style={s.muted}>none — access denied by default</span>
      } />
      <Field label="Apps" value={
        data.allowApps
          ? <span style={{ color: 'var(--ds-success)', fontSize: 13, fontWeight: 600 }}>Enabled</span>
          : <span style={s.muted}>Disabled</span>
      } />
      {data.tools && data.tools.length > 0 && <MCPToolList tools={data.tools} />}
    </>
  )
}

function ModelProviderDetail({ data }: { data: ModelProviderSummary }) {
  const pricing = data.costPerMillionInputTokens && data.costPerMillionOutputTokens
    ? `$${data.costPerMillionInputTokens} in / $${data.costPerMillionOutputTokens} out`
    : undefined
  return (
    <>
      <Field label="Status" value={<ReadyBadge ready={data.ready} />} />
      <Field label="Model" value={data.litellmModel} />
      {data.baseURL && <Field label="Endpoint" value={data.baseURL} />}
      <Field label="Latency Profile" value={data.latencyProfile} />
      <Field label="Capabilities" value={<Tags items={data.capabilities || []} />} />
      {data.contextWindow && data.contextWindow > 0 && <Field label="Context Window" value={data.contextWindow.toLocaleString() + ' tokens'} />}
      {pricing && <Field label="Cost per 1M Tokens" value={pricing} />}
    </>
  )
}

function ModelSelectorDetail({ data }: { data: ModelSelectorSummary }) {
  return (
    <>
      <Field label="Strategy" value={data.strategy} />
      {data.providers && data.providers.length > 0 && (
        <div style={s.field}>
          <div style={s.fieldLabel}>Providers</div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
            {data.providers.map((p) => (
              <div key={p.name} style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
                <div style={s.providerRow}>
                  <span style={s.fieldValue}>{p.name}</span>
                  <span style={s.muted}>weight {p.weight}</span>
                </div>
                {p.routingHint && (
                  <span style={{ ...s.muted, fontSize: 11, paddingLeft: 2 }}>{p.routingHint}</span>
                )}
              </div>
            ))}
          </div>
        </div>
      )}
      {data.activeProviders && data.activeProviders.length > 0 && (
        <Field label="Active Providers" value={<Tags items={data.activeProviders} />} />
      )}
      {data.fallbackChain && data.fallbackChain.length > 0 && (
        <Field label="Fallback Chain" value={<Tags items={data.fallbackChain} />} />
      )}
      {data.capabilityRouting && Object.keys(data.capabilityRouting).length > 0 && (
        <div style={s.field}>
          <div style={s.fieldLabel}>Capability Routing</div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
            {Object.entries(data.capabilityRouting).map(([cap, provider]) => (
              <div key={cap} style={s.providerRow}>
                <span style={s.tag}>{cap}</span>
                <Icon icon={ICON.routed} size={12} ariaHidden={true} style={{ color: "var(--ds-text-secondary)" }} />
                <span style={s.fieldValue}>{provider}</span>
              </div>
            ))}
          </div>
        </div>
      )}
      {data.budget && (
        <div style={s.field}>
          <div style={s.fieldLabel}>Budget</div>
          <div style={{ display: 'flex', gap: 16 }}>
            {data.budget.perRun && <span style={s.fieldValue}>${data.budget.perRun} / run</span>}
            {data.budget.perDay && <span style={s.fieldValue}>${data.budget.perDay} / day</span>}
          </div>
        </div>
      )}
    </>
  )
}

function KnowledgeBaseDetail({ data }: { data: KnowledgeBaseSummary }) {
  // Single reporting path: Status.Message is the authoritative user-facing error,
  // written by the controller's patchKBStatus at every failure path. If Message
  // happens to be empty (e.g. KB created before this controller version), fall
  // back to the "Ready" condition only — NOT all conditions, since benign ones
  // like QdrantUpgrading=False/UpToDate ("Qdrant is at target version X") would
  // produce misleading "errors". This mirrors the Go firstConditionMessage logic.
  const kbError = !data.ready
    ? data.message || data.conditions?.find((c) => c.type === 'Ready' && (c.status === 'False' || c.status === 'Unknown'))?.message
    : undefined

  return (
    <>
      <Field label="Status" value={<ReadyBadge ready={data.ready} />} />
      {!data.ready && kbError && (
        <div style={s.errorField}>
          <div style={s.fieldLabel}>Error</div>
          <div style={s.errorValue}>{kbError}</div>
        </div>
      )}
      {data.description && <Field label="Description" value={data.description} />}
      <Field label="Documents" value={data.documentCount} />
      <Field label="Chunks" value={data.chunkCount} />
      <Field label="Collection" value={data.collectionName || data.name} />
      {data.vectorStoreURL && <Field label="Vector Store URL" value={data.vectorStoreURL} />}
      <Field label="Embedding Model" value={data.modelSelectorRef} />
      <Field label="Dimensions" value={data.dimensions} />
      <Field label="Chunk Size" value={`${data.chunkSize} tokens`} />
      <Field label="Chunk Overlap" value={`${data.chunkOverlap} tokens`} />
      <Field label="Storage Used" value={`${data.storageUsedPercent}%`} />
      {data.lastSyncTime && <Field label="Last Sync" value={new Date(data.lastSyncTime).toLocaleString()} />}
    </>
  )
}

export function ConfigDetailView({ selection }: Props) {
  const [agent, setAgent] = useState<AgentSummary | null>(null)
  const [agentMCPServers, setAgentMCPServers] = useState<MCPServerSummary[]>([])
  const [tool, setTool] = useState<ToolSummary | null>(null)
  const [mcp, setMCP] = useState<MCPServerSummary | null>(null)
  const [provider, setProvider] = useState<ModelProviderSummary | null>(null)
  const [kb, setKB] = useState<KnowledgeBaseSummary | null>(null)
  const [selector, setSelector] = useState<ModelSelectorSummary | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    setLoading(true)
    const load = async () => {
      try {
        switch (selection.kind) {
          case 'agent': {
            const [list, mcps] = await Promise.all([
              listAgents(selection.namespace),
              listMCPServers(selection.namespace),
            ])
            setAgent(list.find((a) => a.name === selection.name) ?? null)
            setAgentMCPServers(mcps)
            break
          }
          case 'tool': {
            const list = await listTools(selection.namespace)
            setTool(list.find((t) => t.name === selection.name) ?? null)
            break
          }
          case 'mcpserver': {
            const list = await listMCPServers(selection.namespace)
            setMCP(list.find((m) => m.name === selection.name) ?? null)
            break
          }
          case 'modelprovider': {
            const list = await listModelProviders(selection.namespace)
            setProvider(list.find((p) => p.name === selection.name) ?? null)
            break
          }
          case 'knowledgebase': {
            const list = await listKnowledgeBases(selection.namespace)
            setKB(list.find((k) => k.name === selection.name) ?? null)
            break
          }
          case 'modelselector': {
            const list = await listModelSelectors(selection.namespace)
            setSelector(list.find((ms) => ms.name === selection.name) ?? null)
            break
          }
        }
      } catch { /* ignore */ } finally {
        setLoading(false)
      }
    }
    load()
  }, [selection.kind, selection.name, selection.namespace])

  if (loading) {
    return <div style={s.loading}>Loading…</div>
  }

  const kindLabels: Record<string, string> = {
    agent: 'Agent',
    tool: 'Tool',
    mcpserver: 'MCP Server',
    modelprovider: 'Model Provider',
    knowledgebase: 'Knowledge Base',
    modelselector: 'Model Selector',
  }

  return (
    <div style={s.root}>
      <div style={s.header}>
        <span style={s.kindLabel}>{kindLabels[selection.kind]}</span>
        <h2 style={s.title}>{selection.name}</h2>
        <span style={s.namespace}>{selection.namespace}</span>
      </div>
      <div style={s.card}>
        {selection.kind === 'agent' && agent && <AgentDetail data={agent} mcpServers={agentMCPServers} />}
        {selection.kind === 'tool' && tool && <ToolDetail data={tool} />}
        {selection.kind === 'mcpserver' && mcp && <MCPServerDetail data={mcp} />}
        {selection.kind === 'modelprovider' && provider && <ModelProviderDetail data={provider} />}
        {selection.kind === 'knowledgebase' && kb && <KnowledgeBaseDetail data={kb} />}
        {selection.kind === 'modelselector' && selector && <ModelSelectorDetail data={selector} />}
      </div>
    </div>
  )
}

const s: Record<string, React.CSSProperties> = {
  root: {
    flex: 1,
    overflow: 'auto',
    padding: DESIGN.space.xl,
  },
  loading: {
    padding: DESIGN.space.xl,
    color: 'var(--ds-text-muted)',
    fontSize: 14,
  },
  header: {
    marginBottom: 20,
  },
  kindLabel: {
    fontSize: 11,
    fontWeight: 600,
    textTransform: 'uppercase',
    letterSpacing: '0.08em',
    color: 'var(--ds-text-secondary)',
  },
  title: {
    fontSize: 20,
    fontWeight: 700,
    color: 'var(--ds-text-primary)',
    margin: '4px 0 2px',
    lineHeight: 1.1,
    textWrap: 'balance' as const,
  },
  namespace: {
    fontSize: 12,
    color: 'var(--ds-text-muted)',
  },
  card: {
    background: 'var(--ds-surface)',
    borderRadius: DESIGN.radii.lg,
    border: `1px solid var(--ds-border)`,
    padding: 16,
    display: 'flex',
    flexDirection: 'column',
    gap: 14,
    boxShadow: 'var(--ds-card-shadow)',
  },
  field: {
    display: 'flex',
    flexDirection: 'column',
    gap: 3,
  },
  fieldLabel: {
    fontSize: 11,
    fontWeight: 600,
    color: 'var(--ds-text-secondary)',
    textTransform: 'uppercase',
    letterSpacing: '0.06em',
  },
  fieldValue: {
    fontSize: 13,
    color: 'var(--ds-text-primary)',
    fontVariantNumeric: 'tabular-nums',
  },
  errorField: {
    display: 'flex',
    flexDirection: 'column',
    gap: 3,
    padding: '8px 10px',
    borderRadius: DESIGN.radii.sm,
    background: 'rgba(239,68,68,.08)',
    border: '1px solid rgba(239,68,68,.3)',
  },
  errorValue: {
    fontSize: 12,
    color: 'var(--ds-error)',
    lineHeight: 1.4,
    wordBreak: 'break-word' as const,
  },
  badge: {
    display: 'inline-block',
    padding: '2px 8px',
    borderRadius: DESIGN.radii.sm,
    fontSize: 11,
    fontWeight: 600,
  },
  tags: {
    display: 'flex',
    flexWrap: 'wrap',
    gap: 4,
  },
  tag: {
    padding: '2px 8px',
    borderRadius: DESIGN.radii.sm,
    fontSize: 11,
    fontWeight: 500,
    background: 'rgba(59,130,246,.15)',
    color: 'var(--ds-accent)',
  },
  muted: {
    color: 'var(--ds-text-muted)',
    fontSize: 13,
  },
  providerRow: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
  },
  pre: {
    margin: 0,
    padding: 10,
    background: 'var(--ds-bg)',
    borderRadius: DESIGN.radii.sm,
    border: `1px solid var(--ds-border)`,
    fontSize: 12,
    color: '#cbd5e1',
    whiteSpace: 'pre-wrap',
    wordBreak: 'break-word',
    maxHeight: 500,
    overflow: 'auto',
    fontVariantNumeric: 'tabular-nums',
  },
  code: {
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace',
    fontSize: 12,
    color: 'var(--ds-accent)',
    background: 'rgba(59,130,246,.1)',
    padding: '1px 5px',
    borderRadius: 3,
  },
}
