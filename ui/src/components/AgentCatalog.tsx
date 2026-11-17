/**
 * AgentCatalog — the cards on /agents. Each card says what an agent does,
 * shows example inputs, who owns it, whether it is published and what a run
 * typically costs. Search and filters narrow the list; a card opens the agent.
 */
import { useEffect, useMemo, useState, type CSSProperties } from 'react'
import { Link } from 'react-router'
import { getCatalog, type CatalogEntry } from '../api/catalog'
import { DSChip } from '../lib/primitives'
import { ds, DESIGN } from '../lib/designSystem'
import { ICON } from '../lib/icons'
import { EmptyState } from '../lib/layout'
import { agentDetailsPath, ROUTES } from '../lib/routes'
import { useNamespace } from '../lib/namespace'

export type PublishedFilter = 'all' | 'published' | 'unpublished'

export interface CatalogFilters {
  query: string
  published: PublishedFilter
  /** Empty for any maintainer. */
  maintainer: string
}

export const NO_FILTERS: CatalogFilters = { query: '', published: 'all', maintainer: '' }

/** Narrows catalog entries by free-text search, published state and maintainer. */
export function filterCatalog(entries: CatalogEntry[], f: CatalogFilters): CatalogEntry[] {
  const q = f.query.trim().toLowerCase()
  return entries.filter((e) => {
    if (f.published === 'published' && !e.published) return false
    if (f.published === 'unpublished' && e.published) return false
    if (f.maintainer && e.maintainer !== f.maintainer) return false
    if (!q) return true
    const hay = [e.name, e.description, e.maintainer, ...e.exampleInputs, ...e.tools].join('\n').toLowerCase()
    return hay.includes(q)
  })
}

/** "$0.20", or "under $0.01" for fractions of a cent. */
export function formatCost(usd: number): string {
  if (usd > 0 && usd < 0.01) return 'under $0.01'
  return `$${usd.toFixed(2)}`
}

function firstParagraph(text: string, max = 220): string {
  const para = text.trim().split(/\n\s*\n/)[0].replace(/\s+/g, ' ')
  return para.length > max ? `${para.slice(0, max - 1).trimEnd()}…` : para
}

export function AgentCard({ agent, href }: { agent: CatalogEntry; href: string }) {
  const description = firstParagraph(agent.description)
  return (
    <li style={s.item}>
      <Link to={href} style={s.card} aria-label={`${agent.name}, ${agent.published ? 'published' : 'not published'}`}>
        <div style={s.cardHead}>
          <span style={s.name}>{agent.name}</span>
          <DSChip variant={agent.published ? 'success' : 'neutral'} size="sm" dot>
            {agent.published ? 'Published' : 'Not published'}
          </DSChip>
        </div>
        <p style={s.description}>{description || 'No description yet. The maintainer can add one.'}</p>
        {agent.exampleInputs.length > 0 && (
          <div style={s.examples}>
            <span style={s.examplesLabel}>Try asking</span>
            {agent.exampleInputs.slice(0, 2).map((x) => <span key={x} style={s.example}>{x}</span>)}
          </div>
        )}
        <div style={s.foot}>
          <span>Maintained by: {agent.maintainer || 'Not set'}</span>
          <span>
            {agent.typicalCostUSD === null
              ? 'No cost data yet'
              : `Typically ${formatCost(agent.typicalCostUSD)} per run`}
          </span>
        </div>
      </Link>
    </li>
  )
}

export function AgentCatalog() {
  const { namespace } = useNamespace()
  const [entries, setEntries] = useState<CatalogEntry[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [filters, setFilters] = useState<CatalogFilters>(NO_FILTERS)

  useEffect(() => {
    let live = true
    getCatalog(namespace)
      .then((list) => { if (live) { setEntries(list); setError(null) } })
      .catch((e: unknown) => { if (live) setError(e instanceof Error ? e.message : String(e)) })
    return () => { live = false }
  }, [namespace])

  const agents = useMemo(() => (entries ?? []).filter((e) => !e.template), [entries])
  const maintainers = useMemo(() => [...new Set(agents.map((a) => a.maintainer).filter(Boolean))].sort(), [agents])
  const shown = useMemo(() => filterCatalog(agents, filters), [agents, filters])
  const filtering = filters.query !== '' || filters.published !== 'all' || filters.maintainer !== ''

  if (error) {
    return <div role="alert" style={s.error}>Could not load the agents: {error}</div>
  }
  if (entries === null) {
    return <div role="status" style={s.loading}>Loading agents…</div>
  }
  if (agents.length === 0) {
    return (
      <EmptyState icon={ICON.agent} title="No agents in this workspace yet">
        Agents are added by your platform team. Start from a template on the Templates tab, or ask your
        platform team to add one for you.
      </EmptyState>
    )
  }

  return (
    <div style={s.wrap}>
      <form role="search" aria-label="Find an agent" style={s.filters} onSubmit={(e) => e.preventDefault()}>
        <label style={s.filter}>
          <span style={s.filterLabel}>Search</span>
          <input
            type="search"
            style={s.control}
            placeholder="Name, what it does, or a tool"
            value={filters.query}
            onChange={(e) => setFilters({ ...filters, query: e.target.value })}
          />
        </label>
        <label style={s.filter}>
          <span style={s.filterLabel}>Status</span>
          <select
            style={s.control}
            value={filters.published}
            onChange={(e) => setFilters({ ...filters, published: e.target.value as PublishedFilter })}
          >
            <option value="all">All agents</option>
            <option value="published">Published</option>
            <option value="unpublished">Not published</option>
          </select>
        </label>
        {maintainers.length > 0 && (
          <label style={s.filter}>
            <span style={s.filterLabel}>Maintainer</span>
            <select style={s.control} value={filters.maintainer} onChange={(e) => setFilters({ ...filters, maintainer: e.target.value })}>
              <option value="">Anyone</option>
              {maintainers.map((o) => <option key={o} value={o}>{o}</option>)}
            </select>
          </label>
        )}
      </form>

      <p role="status" style={s.count}>
        {shown.length === agents.length
          ? `${agents.length} ${agents.length === 1 ? 'agent' : 'agents'}`
          : `${shown.length} of ${agents.length} agents`}
      </p>

      {shown.length === 0 ? (
        <EmptyState
          icon={ICON.search}
          title="No agents match"
          action={filtering ? (
            <button type="button" style={s.clear} onClick={() => setFilters(NO_FILTERS)}>Clear search and filters</button>
          ) : undefined}
        >
          Try a different word, or clear the filters to see every agent.
        </EmptyState>
      ) : (
        <ul style={s.grid} aria-label="Agents">
          {shown.map((a) => <AgentCard key={`${a.namespace}/${a.name}`} agent={a} href={agentDetailsPath(a.namespace, a.name)} />)}
        </ul>
      )}
      <p style={s.footnote}>
        Looking for a starting point? See the <Link to={ROUTES.templates} style={{ color: ds.accent }}>templates</Link>.
      </p>
    </div>
  )
}

const s: Record<string, CSSProperties> = {
  wrap: { padding: '8px 24px 24px', display: 'flex', flexDirection: 'column', gap: 12 },
  filters: { display: 'flex', gap: 12, flexWrap: 'wrap', alignItems: 'flex-end' },
  filter: { display: 'flex', flexDirection: 'column', gap: 4, minWidth: 160 },
  filterLabel: { fontSize: 11, fontWeight: 600, color: ds.textSecondary, textTransform: 'uppercase', letterSpacing: '0.04em' },
  control: {
    background: ds.surface, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md,
    padding: '7px 10px', color: ds.textPrimary, fontSize: 13, fontFamily: DESIGN.font.family,
  },
  count: { margin: 0, fontSize: 12, color: ds.textMuted },
  grid: { listStyle: 'none', margin: 0, padding: 0, display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(280px, 1fr))', gap: 14 },
  item: { display: 'flex' },
  card: {
    display: 'flex', flexDirection: 'column', gap: 8, width: '100%', padding: 14, textDecoration: 'none',
    background: ds.surface, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.xl, color: ds.textPrimary,
    boxShadow: 'var(--ds-card-shadow)',
  },
  cardHead: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 8 },
  name: { fontSize: 15, fontWeight: 600, overflowWrap: 'anywhere' },
  description: { margin: 0, fontSize: 13, color: ds.textSecondary, lineHeight: 1.45, flex: 1 },
  examples: { display: 'flex', flexDirection: 'column', gap: 4 },
  examplesLabel: { fontSize: 11, fontWeight: 600, color: ds.textMuted, textTransform: 'uppercase', letterSpacing: '0.04em' },
  example: {
    fontSize: 12, color: ds.textSecondary, background: 'var(--ds-muted-bg)', borderRadius: DESIGN.radii.md,
    padding: '3px 8px', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
  },
  foot: { display: 'flex', justifyContent: 'space-between', gap: 8, fontSize: 11, color: ds.textMuted, flexWrap: 'wrap' },
  loading: { padding: 24, color: ds.textMuted, fontSize: 13 },
  error: { margin: 24, padding: 12, color: ds.error, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, fontSize: 13 },
  clear: {
    background: 'transparent', border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md,
    color: ds.accent, padding: '6px 12px', cursor: 'pointer', fontSize: 12,
  },
  footnote: { margin: 0, fontSize: 12, color: ds.textMuted },
}
