/**
 * ConfigList — flat list for configuration resources (Agents, Tools, MCP Servers, Model Providers).
 * Unlike ResourceList, these don't have phase-based lifecycles — just a flat filtered list with ready dots.
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
  type ModelProviderSummary,
  type KnowledgeBaseSummary,
  type ModelSelectorSummary,
} from '../api/sse'
import type { ResourceSelection, ResourceTab } from './ResourceList'
import { DESIGN } from '../lib/designSystem'
import { Icon, ICON } from '../lib/icons'

type ConfigTab = 'agents' | 'tools' | 'mcpservers' | 'modelproviders' | 'knowledgebases' | 'modelselectors'

interface Props {
  tab: ConfigTab
  namespace?: string
  selection: ResourceSelection | null
  onSelect: (sel: ResourceSelection) => void
}

type ConfigItem = {
  name: string
  namespace: string
  subtitle: string
  ready?: boolean
  kind: 'agent' | 'tool' | 'mcpserver' | 'modelprovider' | 'knowledgebase' | 'modelselector'
}

function toItems(tab: ConfigTab, agents: AgentSummary[], tools: ToolSummary[], mcps: MCPServerSummary[], providers: ModelProviderSummary[], kbs: KnowledgeBaseSummary[], selectors: ModelSelectorSummary[]): ConfigItem[] {
  switch (tab) {
    case 'agents':
      return agents.map((a) => ({
        name: a.name,
        namespace: a.namespace,
        subtitle: `${a.framework} · ${a.modelSelectorRef}`,
        kind: 'agent' as const,
      }))
    case 'tools':
      return tools.map((t) => ({
        name: t.name,
        namespace: t.namespace,
        subtitle: `${t.type} · ${t.executionMode}`,
        ready: t.ready,
        kind: 'tool' as const,
      }))
    case 'mcpservers':
      return mcps.map((m) => ({
        name: m.name,
        namespace: m.namespace,
        subtitle: `${m.transport}${m.url ? ` · ${m.url}` : ''} · ${m.toolCount} tools`,
        ready: m.ready,
        kind: 'mcpserver' as const,
      }))
    case 'modelproviders':
      return providers.map((p) => ({
        name: p.name,
        namespace: p.namespace,
        subtitle: `${p.litellmModel} · ${p.latencyProfile}`,
        ready: p.ready,
        kind: 'modelprovider' as const,
      }))
    case 'knowledgebases':
      return kbs.map((kb) => ({
        name: kb.name,
        namespace: kb.namespace,
        subtitle: !kb.ready && kb.message
          ? `${kb.message}`
          : `${kb.documentCount} docs · ${kb.chunkCount} chunks`,
        ready: kb.ready,
        kind: 'knowledgebase' as const,
      }))
    case 'modelselectors':
      return selectors.map((ms) => ({
        name: ms.name,
        namespace: ms.namespace,
        subtitle: `${ms.strategy} · ${ms.providers?.length ?? 0} providers`,
        kind: 'modelselector' as const,
      }))
  }
}

export function ConfigList({ tab, namespace = 'default', selection, onSelect }: Props) {
  const [agents, setAgents] = useState<AgentSummary[]>([])
  const [tools, setTools] = useState<ToolSummary[]>([])
  const [mcps, setMCPs] = useState<MCPServerSummary[]>([])
  const [providers, setProviders] = useState<ModelProviderSummary[]>([])
  const [kbs, setKBs] = useState<KnowledgeBaseSummary[]>([])
  const [selectors, setSelectors] = useState<ModelSelectorSummary[]>([])
  const [loading, setLoading] = useState(false)
  const [filterText, setFilterText] = useState('')

  useEffect(() => {
    setLoading(true)
    const load = async () => {
      try {
        const byName = <T extends { name: string }>(a: T, b: T) => a.name.localeCompare(b.name)
        switch (tab) {
          case 'agents': { const d = await listAgents(namespace); d.sort(byName); setAgents(d); break }
          case 'tools': { const d = await listTools(namespace); d.sort(byName); setTools(d); break }
          case 'mcpservers': { const d = await listMCPServers(namespace); d.sort(byName); setMCPs(d); break }
          case 'modelproviders': { const d = await listModelProviders(namespace); d.sort(byName); setProviders(d); break }
          case 'knowledgebases': { const d = await listKnowledgeBases(namespace); d.sort(byName); setKBs(d); break }
          case 'modelselectors': { const d = await listModelSelectors(namespace); d.sort(byName); setSelectors(d); break }
        }
      } catch { /* ignore */ } finally {
        setLoading(false)
      }
    }
    load()
    const interval = setInterval(load, 5000)
    return () => clearInterval(interval)
  }, [tab, namespace])

  useEffect(() => { setFilterText('') }, [tab])

  const items = toItems(tab, agents, tools, mcps, providers, kbs, selectors)
  const q = filterText.trim().toLowerCase()
  const filtered = q ? items.filter((i) => i.name.toLowerCase().includes(q)) : items

  if (loading && items.length === 0) {
    return <div style={s.loading}>Loading…</div>
  }

  const emptyLabel = { agents: 'agents', tools: 'tools', mcpservers: 'MCP servers', modelproviders: 'model providers', knowledgebases: 'knowledge bases', modelselectors: 'model selectors' }[tab]

  return (
    <div style={s.list}>
      <div style={s.filterWrapper}>
        <Icon icon={ICON.search} size={12} style={s.filterIcon} ariaHidden={true} />
        <input
          type="text"
          placeholder="Filter by name…"
          value={filterText}
          onChange={(e) => setFilterText((e.target as HTMLInputElement).value)}
          style={s.filterInput}
        />
        {filterText && (
          <button
            type="button"
            style={s.filterClear}
            onClick={() => setFilterText('')}
            aria-label="Clear filter"
            title="Clear filter"
          >
            <Icon icon={ICON.close} size={10} />
          </button>
        )}
      </div>
      {filtered.length === 0 && (
        <div style={s.empty}>{q ? `No ${emptyLabel} match your filter.` : `No ${emptyLabel} found.`}</div>
      )}
      {filtered.map((item) => {
        const active = selection?.kind === item.kind && selection.name === item.name
        return (
          <button
            key={`${item.namespace}/${item.name}`}
            type="button"
            style={{ ...s.item, ...(active ? s.itemActive : {}) }}
            onClick={() => onSelect({ kind: item.kind, name: item.name, namespace: item.namespace })}
          >
            <div style={s.itemRow}>
              {item.ready !== undefined && (
                <span style={{ ...s.readyDot, background: item.ready ? 'var(--ds-success)' : 'var(--ds-error)' }} />
              )}
              <span style={s.itemName}>{item.name}</span>
            </div>
            <div style={s.itemSub}>{item.subtitle}</div>
          </button>
        )
      })}
    </div>
  )
}

const s: Record<string, React.CSSProperties> = {
  list: {
    display: 'flex',
    flexDirection: 'column',
    gap: 2,
    padding: '6px 8px',
  },
  item: {
    padding: '9px 10px',
    borderRadius: DESIGN.radii.sm,
    cursor: 'pointer',
    display: 'flex',
    flexDirection: 'column',
    gap: 3,
    border: '1px solid transparent',
    transitionProperty: 'background-color, border-color',
    transitionDuration: '0.12s',
    transitionTimingFunction: 'ease',
    background: 'transparent',
    color: 'inherit',
    font: 'inherit',
    textAlign: 'left',
    textDecoration: 'none',
  },
  itemActive: {
    background: 'rgba(59,130,246,.1)',
    borderColor: 'rgba(59,130,246,.25)',
  },
  itemRow: {
    display: 'flex',
    alignItems: 'center',
    gap: 6,
  },
  readyDot: {
    width: 7,
    height: 7,
    borderRadius: '50%',
    flexShrink: 0,
  },
  itemName: {
    fontSize: 12,
    fontWeight: 600,
    color: 'var(--ds-text-primary)',
    whiteSpace: 'nowrap',
    overflow: 'hidden',
    textOverflow: 'ellipsis',
  },
  itemSub: {
    fontSize: 11,
    color: 'var(--ds-text-muted)',
    whiteSpace: 'nowrap',
    overflow: 'hidden',
    textOverflow: 'ellipsis',
  },
  loading: { padding: 16, color: 'var(--ds-text-muted)', fontSize: 13 },
  empty: { padding: '16px 10px', color: 'var(--ds-text-muted)', fontSize: 13 },
  filterWrapper: {
    position: 'relative' as const,
    width: 'calc(100% - 4px)',
    marginBottom: 4,
  },
  filterIcon: {
    position: 'absolute' as const,
    left: 6,
    top: '50%',
    transform: 'translateY(-50%)',
    color: 'var(--ds-text-muted)',
    pointerEvents: 'none' as const,
  },
  filterClear: {
    position: 'absolute' as const,
    right: 4,
    top: '50%',
    transform: 'translateY(-50%)',
    width: 16,
    height: 16,
    borderRadius: '50%',
    border: 'none',
    background: 'transparent',
    color: 'var(--ds-text-muted)',
    cursor: 'pointer',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    padding: 0,
    transitionProperty: 'color, background-color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  filterInput: {
    width: '100%',
    boxSizing: 'border-box' as const,
    padding: '5px 8px 5px 26px',
    marginBottom: 4,
    background: 'var(--ds-surface)',
    border: '1px solid var(--ds-border)',
    borderRadius: DESIGN.radii.sm,
    color: 'var(--ds-text-primary)',
    fontSize: 12,
    fontFamily: 'system-ui, sans-serif',
    outline: 'none',
    fontVariantNumeric: 'tabular-nums',
    transitionProperty: 'border-color, box-shadow',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
}
