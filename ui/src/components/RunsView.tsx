/**
 * RunsView — one table for every run: live runs (AgentRun resources in the
 * cluster) merged with archived runs (read from PostgreSQL). The two are
 * different data sources, so they are fetched separately and merged here; when
 * a run is in both, the live copy wins. Filters: search, status, agent, source.
 * Clicking a row opens the live or the archived detail view for that run.
 *
 * Honours better-layout §5 (progressive disclosure — filter bar collapses on
 * narrow viewports), better-accessibility §5 (keyboard operable sort buttons),
 * better-accessibility §9 (redundant status cue via StatusBadge icon+color), and
 * better-typography §11 (tabular-nums for stable numeric columns).
 */
import { useEffect, useState, useCallback, useMemo } from 'react'
import {
  listRunHistory,
  listRuns,
  type AgentRunSummary,
  type RunHistorySummary,
  type RunHistoryResponse,
} from '../api/sse'
import { StatusBadge } from './StatusBadge'
import { DESIGN, ds } from '../lib/designSystem'
import { Icon, ICON } from '../lib/icons'
import { PageHeader, EmptyState } from '../lib/layout'

const PAGE_SIZES = [25, 50, 100]
const PHASE_OPTIONS = ['', 'Pending', 'Running', 'Succeeded', 'Failed', 'HandedOff', 'WaitingForInput']

type SortKey = 'name' | 'agent' | 'phase' | 'cost' | 'started' | 'completed'
type SortDir = 'asc' | 'desc'
export type RunSource = 'live' | 'archived'
type SourceFilter = '' | RunSource

/** A run from either source, tagged with where it came from. */
type RunRow = RunHistorySummary & { source: RunSource }

interface Props {
  /** Open a run's detail view: the live view or the archived one. */
  onNavigateToRun: (runName: string, namespace: string, source: RunSource) => void
  /** Workspace to list; empty means all. */
  namespace?: string
}

export function RunsView({ onNavigateToRun, namespace = '' }: Props) {
  const [live, setLive] = useState<AgentRunSummary[]>([])
  const [liveError, setLiveError] = useState<string | null>(null)
  const [sourceFilter, setSourceFilter] = useState<SourceFilter>('')
  const [page, setPage] = useState<RunHistoryResponse>({
    runs: [],
    total: 0,
    limit: 50,
    offset: 0,
  })
  const [filters, setFilters] = useState({ phase: '', agentRef: '', search: '' })
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [notConfigured, setNotConfigured] = useState(false)
  const [lastSynced, setLastSynced] = useState<Date | null>(null)
  const [sortKey, setSortKey] = useState<SortKey>('started')
  const [sortDir, setSortDir] = useState<SortDir>('desc')

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const data = await listRunHistory({
        limit: page.limit,
        offset: page.offset,
        phase: filters.phase,
        agentRef: filters.agentRef,
        search: filters.search,
        namespace,
      })
      setNotConfigured(false)
      // Defensive: an older/empty archive may serialize `runs` as null; coerce
      // to [] so the table never crashes the root render.
      setPage({ ...data, runs: data.runs ?? [] })
    } catch (e: any) {
      const msg = e?.message ?? String(e)
      // 503 with "not configured" body means the PostgreSQL archive is not wired.
      if (msg.includes('503') || msg.includes('not configured')) {
        setNotConfigured(true)
        setError(null)
      } else {
        setNotConfigured(false)
        setError(msg)
      }
    }
    try {
      setLive(await listRuns(namespace))
      setLiveError(null)
    } catch (e: any) {
      setLiveError(e?.message ?? String(e))
    } finally {
      setLoading(false)
      setLastSynced(new Date())
    }
  }, [page.limit, page.offset, filters, namespace])

  useEffect(() => {
    void load()
    const id = setInterval(() => void load(), 5000)
    return () => clearInterval(id)
  }, [load])

  // Re-fetch when the tab regains focus/visibility so history recovers within
  // the poll window after a controller restart or transient outage.
  useEffect(() => {
    const onVisible = () => { if (document.visibilityState === 'visible') void load() }
    document.addEventListener('visibilitychange', onVisible)
    return () => document.removeEventListener('visibilitychange', onVisible)
  }, [load])

  // Reset to first page when filters change (applied via the Apply button).
  const handleFilterChange = (key: string, value: string) => {
    setFilters((prev) => ({ ...prev, [key]: value }))
  }

  const handleApply = () => {
    setPage((prev) => ({ ...prev, offset: 0 }))
    void load()
  }

  // Normalize to a non-null array: the backend may serialize an empty page as
  // `runs: null` (nil slice), which crashes the spread/map below with
  // "can't access property Symbol.iterator, t.runs is null".
  const archived = page.runs ?? []

  // Merge both sources. Archived rows are already filtered by the server; live
  // rows are filtered here with the same criteria. A run present in both is
  // shown once, as live (even when the filters hide the live copy).
  const runs = useMemo<RunRow[]>(() => {
    const phase = filters.phase
    const agent = filters.agentRef.trim().toLowerCase()
    const search = filters.search.trim().toLowerCase()
    const liveRows: RunRow[] = live
      .filter((r) => (!phase || r.phase === phase)
        && (!agent || r.agentRef.toLowerCase().includes(agent))
        && (!search || r.name.toLowerCase().includes(search)))
      .map((r) => ({ ...r, source: 'live' as const }))
    const allLiveKeys = new Set(live.map((r) => `${r.namespace}/${r.name}`))
    const archivedRows: RunRow[] = archived
      .filter((r) => !allLiveKeys.has(`${r.namespace}/${r.name}`))
      .map((r) => ({ ...r, source: 'archived' as const }))
    return [...liveRows, ...archivedRows].filter((r) => !sourceFilter || r.source === sourceFilter)
  }, [live, archived, filters, sourceFilter])

  // Sort the current page's rows (local sort — better for responsiveness than
  // re-querying the server for a column click).
  const sorted = useMemo(() => {
    const rows = [...runs]
    const dir = sortDir === 'desc' ? 1 : -1
    rows.sort((a, b) => {
      let av: string | number, bv: string | number
      switch (sortKey) {
        case 'cost':
          av = parseFloat(a.spendUSD) || 0
          bv = parseFloat(b.spendUSD) || 0
          break
        case 'started':
          av = a.startTime ? new Date(a.startTime).getTime() : 0
          bv = b.startTime ? new Date(b.startTime).getTime() : 0
          break
        case 'completed':
          av = a.completionTime ? new Date(a.completionTime).getTime() : 0
          bv = b.completionTime ? new Date(b.completionTime).getTime() : 0
          break
        case 'agent':
          av = a.agentRef; bv = b.agentRef; break
        case 'phase':
          av = a.phase; bv = b.phase; break
        default:
          av = a.name; bv = b.name; break
      }
      if (typeof av === 'number' && typeof bv === 'number') {
        return av === bv ? 0 : av > bv ? dir : -dir
      }
      return av === bv ? 0 : String(av) > String(bv) ? dir : -dir
    })
    return rows
  }, [runs, sortKey, sortDir])

  const toggleSort = (key: SortKey) => {
    if (sortKey === key) {
      setSortDir(sortDir === 'asc' ? 'desc' : 'asc')
    } else {
      setSortKey(key)
      setSortDir(key === 'started' ? 'desc' : 'asc')
    }
  }

  const filtered = Boolean(filters.phase || filters.agentRef || filters.search || sourceFilter)
  const archivedShown = runs.filter((r) => r.source === 'archived').length
  const liveShown = runs.length - archivedShown
  const total = liveShown + (sourceFilter === 'live' ? 0 : Math.max(page.total - (archived.length - archivedShown), archivedShown))
  const firstRun = !loading && !filtered && !notConfigured && !error && !liveError && runs.length === 0

  const totalPages = Math.ceil(page.total / page.limit)
  const currentPage = totalPages > 0 ? Math.floor(page.offset / page.limit) + 1 : 0

  // Both sources failed: nothing to show. One failing is reported inline below.
  if (error && liveError && !loading) {
    return (
      <div style={s.empty}>
        <Icon icon={ICON.warning} size={36} ariaHidden style={{ color: 'var(--ds-error)' }} />
        <div style={s.emptyTitle}>Failed to load runs</div>
        <div style={s.emptyBody}>{error}</div>
        <button type="button" style={s.retryBtn} onClick={() => void load()}>
          <Icon icon={ICON.retry} size={12} ariaHidden style={{ marginRight: 4 }} /> Retry
        </button>
      </div>
    )
  }

  return (
    <div style={s.root}>
      <PageHeader
        title="Runs"
        description="Every run of your agents, running now and finished. Open one to see its output, cost and what the agent did."
      />
      {notConfigured && (
        <div role="note" style={s.note}>
          Past runs are not being kept, so only runs still in the cluster are listed. A platform admin
          can turn on run history by connecting a database (<code style={s.code}>PG_DSN</code> on the operator).
        </div>
      )}
      {error && <div role="alert" style={s.note}>Could not load past runs: {error}</div>}
      {liveError && <div role="alert" style={s.note}>Could not load live runs: {liveError}</div>}
      {firstRun ? (
        <EmptyState icon={ICON.runs} title="No runs yet">
          Runs appear here once an agent has been started. Open Agents, pick a published agent and send it a message to create your first run.
        </EmptyState>
      ) : (<>
      {/* Filter bar */}
      <div style={s.filterBar}>
        <div style={s.filterGroup}>
          <label style={s.filterLabel}>Search</label>
          <input
            type="text"
            placeholder="Run name…"
            value={filters.search}
            onChange={(e) => handleFilterChange('search', e.target.value)}
            style={{ ...s.searchInput, ...(loading ? { opacity: 0.6 } : {}) }}
            aria-label="Search runs"
          />
        </div>
        <div style={s.filterGroup}>
          <label style={s.filterLabel}>Status</label>
          <select
            value={filters.phase}
            onChange={(e) => handleFilterChange('phase', e.target.value)}
            style={s.select}
            aria-label="Filter by status"
          >
            {PHASE_OPTIONS.map((p) => (
              <option key={p || 'all'} value={p}>{p || 'All statuses'}</option>
            ))}
          </select>
        </div>
        <div style={s.filterGroup}>
          <label style={s.filterLabel}>Source</label>
          <select
            value={sourceFilter}
            onChange={(e) => setSourceFilter(e.target.value as SourceFilter)}
            style={s.select}
            aria-label="Filter by source"
          >
            <option value="">Live and past</option>
            <option value="live">Live only</option>
            <option value="archived">Past only</option>
          </select>
        </div>
        <div style={s.filterGroup}>
          <label style={s.filterLabel}>Agent</label>
          <input
            type="text"
            placeholder="Agent name…"
            value={filters.agentRef}
            onChange={(e) => handleFilterChange('agentRef', e.target.value)}
            style={s.textInput}
            aria-label="Filter by agent"
          />
        </div>
        <button
          type="button"
          style={{ ...s.applyBtn, opacity: loading ? 0.5 : 1 }}
          disabled={loading}
          onClick={handleApply}
          aria-label="Apply filters"
        >
          <Icon icon={ICON.search} size={12} ariaHidden style={{ marginRight: 4 }} /> Apply
        </button>
        <button
          type="button"
          style={s.resetBtn}
          onClick={() => {
            setFilters({ phase: '', agentRef: '', search: '' })
            setPage((prev) => ({ ...prev, offset: 0 }))
            setTimeout(() => void load(), 0)
          }}
          aria-label="Reset filters"
        >
          Reset
        </button>
      </div>

      {/* Results summary */}
      <div style={s.summary}>
        <span style={s.summaryText}>
          {total} run{total !== 1 ? 's' : ''} found
          {filtered ? ' (filtered)' : ''}
        </span>
        <span style={s.summaryText}>
          {lastSynced ? `Last synced ${formatAge(lastSynced)} ago` : '• Idle'}
        </span>
        <span style={s.summaryMuted}>
          Sorted by <span style={s.sortPill}>{sortKey}</span> {sortDir === 'desc' ? '↓' : '↑'}
        </span>
      </div>

      {/* Table */}
      {loading && runs.length === 0 ? (
        <div style={s.loading}>Loading historical runs…</div>
      ) : (
        <div style={s.tableContainer}>
          <table style={s.table} role="table">
            <thead>
              <tr>
                {renderHeader('name', 'Run Name', s.th, s.sortBtn)}
                {renderHeader('agent', 'Agent', s.th, s.sortBtn)}
                {renderHeader('phase', 'Phase', s.th, s.sortBtn)}
                {renderHeader('cost', 'Cost', { ...s.th, textAlign: 'right' }, s.sortBtn)}
                {renderHeader('started', 'Started', s.th, s.sortBtn)}
                {renderHeader('completed', 'Completed', s.th, s.sortBtn)}
                <th style={s.th}>Source</th>
                <th style={s.th}>Namespace</th>
                <th style={s.th}>Context</th>
                <th style={s.th}>Actions</th>
              </tr>
            </thead>
            <tbody>
              {sorted.length === 0 && (
                <tr>
                  <td colSpan={10} style={s.empty}>
                    {filtered ? 'No runs match your filters.' : 'No runs found.'}
                  </td>
                </tr>
              )}
              {sorted.map((run) => (
                <RunHistoryRow
                  key={`${run.namespace}/${run.name}`}
                  run={run}
                  onClick={() => onNavigateToRun(run.name, run.namespace, run.source)}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* Pagination */}
      {page.total > 0 && (
        <div style={s.pagination}>
          <div style={s.pageSize}>
            <label>Rows per page:</label>
            <select
              value={page.limit}
              onChange={(e) => setPage((prev) => ({ ...prev, limit: Number(e.target.value), offset: 0 }))}
              style={s.selectSmall}
              aria-label="Rows per page"
            >
              {PAGE_SIZES.map((sz) => (
                <option key={sz} value={sz}>{sz}</option>
              ))}
            </select>
          </div>
          <div style={s.pageInfo} aria-live="polite">
            {currentPage} of {totalPages}
          </div>
          <div style={s.pageControls}>
            <button
              style={{ ...s.pageBtn, opacity: currentPage <= 1 ? 0.4 : 1 }}
              disabled={currentPage <= 1 || loading}
              onClick={() => setPage((prev) => ({ ...prev, offset: Math.max(0, prev.offset - prev.limit) }))}
              aria-label="Previous page"
            >
              <Icon icon={ICON.chevronRight} size={12} ariaHidden style={{ transform: 'rotate(180deg)' }} />
            </button>
            <button
              style={{ ...s.pageBtn, opacity: currentPage >= totalPages ? 0.4 : 1 }}
              disabled={currentPage >= totalPages || loading}
              onClick={() => setPage((prev) => ({ ...prev, offset: prev.offset + prev.limit }))}
              aria-label="Next page"
            >
              <Icon icon={ICON.chevronRight} size={12} ariaHidden />
            </button>
          </div>
        </div>
      )}
      </>)}
    </div>
  )

  // Render a sortable header button. Inlined as a closure so it captures sort state.
  function renderHeader(label: string, accessibleName: string, thStyle: React.CSSProperties, btnStyle: React.CSSProperties) {
    const key = label.toLowerCase() as SortKey
    const active = sortKey === key
    const ariaSort = !active ? 'none' : sortDir === 'asc' ? 'ascending' : 'descending'
    return (
      <th key={key} style={thStyle}>
        <button
          type="button"
          style={{
            ...btnStyle,
            ...(active ? { color: 'var(--ds-accent)', fontWeight: 700 } : {}),
            cursor: 'pointer',
            border: 'none',
            background: 'transparent',
            padding: 0,
            fontSize: thStyle.fontSize,
          }}
          onClick={() => toggleSort(key)}
          aria-sort={ariaSort}
          aria-label={`Sort by ${accessibleName}`}
        >
          {accessibleName}
          {active && <span style={{ marginLeft: 4 }}>{sortDir === 'desc' ? '↓' : '↑'}</span>}
        </button>
      </th>
    )
  }
}

function RunHistoryRow({ run, onClick }: { run: RunRow; onClick: () => void }) {
  const started = run.startTime ? new Date(run.startTime).toLocaleString() : '—'
  const completed = run.completionTime ? new Date(run.completionTime).toLocaleString() : '—'
  const contextPct = run.maxContextTokens
    ? Math.round((run.contextUsedTokens ?? 0) / run.maxContextTokens * 100)
    : 0
  const contextLabel = run.maxContextTokens
    ? `${run.contextUsedTokens?.toLocaleString() ?? 0} / ${run.maxContextTokens.toLocaleString()} (${contextPct}%)`
    : run.contextUsedTokens
      ? `${run.contextUsedTokens.toLocaleString()} tokens`
      : '—'

  return (
    <tr style={s.row}>
      <td style={s.td}>
        <button type="button" style={s.rowLink} onClick={onClick} aria-label={`View run ${run.name}`}>
          {run.name}
        </button>
      </td>
      <td style={s.td}>{run.agentRef || '—'}</td>
      <td style={s.td}>
        <StatusBadge phase={run.phase} />
      </td>
      <td style={{ ...s.td, textAlign: 'right' }}>
        <span style={s.cost}>${run.spendUSD || '0.0000'}</span>
      </td>
      <td style={s.td}>{started}</td>
      <td style={s.td}>{completed}</td>
      <td style={s.td}>{run.source === 'live' ? 'Live' : 'Past'}</td>
      <td style={s.td}>
        <code style={s.code}>{run.namespace}</code>
      </td>
      <td style={s.td}>
        <span style={{ ...s.cost, color: contextPct > 80 ? 'var(--ds-warning)' : 'var(--ds-text-secondary)' }}>
          {contextLabel}
        </span>
      </td>
      <td style={s.td}>
        <button
          type="button"
          style={s.iconBtn}
          onClick={onClick}
          aria-label={`View ${run.name} output`}
          title="View run detail"
        >
          <Icon icon={ICON.chevronRight} size={14} ariaHidden />
        </button>
      </td>
    </tr>
  )
}

function formatAge(d: Date): string {
  const secs = Math.max(0, Math.round((Date.now() - d.getTime()) / 1000))
  if (secs < 60) return `${secs}s`
  if (secs < 3600) return `${Math.round(secs / 60)}m`
  return `${Math.round(secs / 3600)}h`
}

const s: Record<string, React.CSSProperties> = {
  root: {
    flex: 1,
    overflowY: 'auto',
    padding: `0 ${DESIGN.space.xl} ${DESIGN.space.xl}`,
    display: 'flex',
    flexDirection: 'column',
    gap: DESIGN.space.lg,
  },
  filterBar: {
    display: 'flex',
    alignItems: 'flex-end',
    gap: DESIGN.space.md,
    flexWrap: 'wrap',
    background: ds.surface,
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.lg,
    padding: DESIGN.space.md,
    boxShadow: 'var(--ds-card-shadow)',
  },
  filterGroup: {
    display: 'flex',
    flexDirection: 'column',
    gap: 4,
    minWidth: 140,
  },
  filterLabel: {
    fontSize: 10,
    fontWeight: 600,
    color: 'var(--ds-text-secondary)',
    textTransform: 'uppercase',
    letterSpacing: '0.06em',
  },
  searchInput: {
    padding: '6px 8px',
    background: 'var(--ds-bg)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.sm,
    color: 'var(--ds-text-primary)',
    fontSize: 13,
    fontFamily: DESIGN.font.mono,
    width: 180,
    outline: 'none',
    transitionProperty: 'border-color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  textInput: {
    padding: '6px 8px',
    background: 'var(--ds-bg)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.sm,
    color: 'var(--ds-text-primary)',
    fontSize: 13,
    width: 160,
    outline: 'none',
    boxSizing: 'border-box' as const,
  },
  select: {
    padding: '6px 8px',
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.sm,
    color: 'var(--ds-text-primary)',
    fontSize: 13,
    outline: 'none',
    cursor: 'pointer',
  },
  selectSmall: {
    padding: '2px 6px',
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.sm,
    color: 'var(--ds-text-primary)',
    fontSize: 12,
    outline: 'none',
    cursor: 'pointer',
  },
  applyBtn: {
    padding: '6px 14px',
    background: 'var(--ds-accent)',
    color: '#fff',
    border: 'none',
    borderRadius: DESIGN.radii.sm,
    fontSize: 12,
    fontWeight: 500,
    cursor: 'pointer',
    display: 'inline-flex',
    alignItems: 'center',
    gap: 4,
    height: 32,
  },
  resetBtn: {
    padding: '6px 12px',
    background: 'transparent',
    color: 'var(--ds-text-secondary)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.sm,
    fontSize: 12,
    fontWeight: 500,
    cursor: 'pointer',
    height: 32,
  },
  summary: {
    display: 'flex',
    justifyContent: 'space-between',
    alignItems: 'center',
    gap: 12,
  },
  summaryText: {
    fontSize: 12,
    color: 'var(--ds-text-secondary)',
  },
  summaryMuted: {
    fontSize: 11,
    color: 'var(--ds-text-muted)',
    fontVariantNumeric: 'tabular-nums',
  },
  sortPill: {
    fontSize: 11,
    fontWeight: 600,
    color: 'var(--ds-text-primary)',
    fontFamily: DESIGN.font.mono,
    background: 'rgba(148,163,184,.1)',
    padding: '2px 8px',
    borderRadius: DESIGN.radii.full,
  },
  loading: {
    padding: DESIGN.space.xl,
    color: 'var(--ds-text-muted)',
    fontSize: 13,
  },
  tableContainer: {
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.lg,
    // Scrollable surface so the wide history table (9 columns incl. Context)
    // is reachable at narrow viewports instead of being clipped.
    overflow: 'auto',
    maxHeight: '40vh',
    boxShadow: 'var(--ds-card-shadow)',
  },
  table: {
    width: '100%',
    minWidth: 'min-content',
    borderCollapse: 'collapse',
    fontSize: 12,
  },
  th: {
    background: 'rgba(148,163,184,.05)',
    color: 'var(--ds-text-secondary)',
    fontWeight: 600,
    textTransform: 'uppercase',
    letterSpacing: '0.06em',
    fontSize: 10,
    padding: '8px 12px',
    textAlign: 'left',
    borderBottom: `1px solid var(--ds-border)`,
  },
  sortBtn: {
    color: 'inherit',
    fontFamily: DESIGN.font.family,
    fontSize: 10,
  },
  td: {
    padding: '8px 12px',
    borderBottom: `1px solid var(--ds-border-light)`,
    color: 'var(--ds-text-primary)',
    fontVariantNumeric: 'tabular-nums',
  },
  code: {
    fontFamily: DESIGN.font.mono,
    fontSize: 11,
    color: 'var(--ds-text-secondary)',
    background: 'rgba(148,163,184,.08)',
    padding: '1px 5px',
    borderRadius: 3,
  },
  row: {
    transitionProperty: 'background-color',
    transitionDuration: '0.12s',
  },
  rowLink: {
    background: 'none',
    border: 'none',
    color: 'var(--ds-accent)',
    fontSize: 12,
    padding: 0,
    cursor: 'pointer',
    fontFamily: DESIGN.font.mono,
    textAlign: 'left' as const,
    textDecoration: 'none',
  },
  cost: {
    fontSize: 12,
    fontVariantNumeric: 'tabular-nums',
    color: 'var(--ds-text-secondary)',
  },
  empty: {
    textAlign: 'center' as const,
    padding: DESIGN.space.xl,
    color: 'var(--ds-text-muted)',
    fontSize: 13,
  },
  emptyTitle: {
    fontSize: 16,
    fontWeight: 500,
    color: 'var(--ds-text-secondary)',
    margin: `${DESIGN.space.md} 0 4px`,
  },
  emptyBody: {
    fontSize: 13,
    color: 'var(--ds-text-muted)',
    lineHeight: 1.5,
    maxWidth: 480,
    textAlign: 'center' as const,
  },
  note: {
    padding: '8px 12px',
    fontSize: 13,
    color: 'var(--ds-text-secondary)',
    background: 'var(--ds-surface)',
    border: '1px solid var(--ds-border)',
    borderRadius: DESIGN.radii.md,
  },
  retryBtn: {
    marginTop: DESIGN.space.md,
    padding: '6px 14px',
    background: 'var(--ds-accent)',
    color: '#fff',
    border: 'none',
    borderRadius: DESIGN.radii.sm,
    fontSize: 12,
    fontWeight: 500,
    cursor: 'pointer',
    display: 'inline-flex',
    alignItems: 'center',
    gap: 4,
  },
  iconBtn: {
    width: 24,
    height: 24,
    borderRadius: DESIGN.radii.sm,
    border: `1px solid var(--ds-border)`,
    background: 'transparent',
    color: 'var(--ds-text-secondary)',
    cursor: 'pointer',
    display: 'inline-flex',
    alignItems: 'center',
    justifyContent: 'center',
    padding: 0,
    transitionProperty: 'background-color, color',
    transitionDuration: '0.15s',
  },
  pagination: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    gap: DESIGN.space.md,
  },
  pageSize: {
    display: 'flex',
    alignItems: 'center',
    gap: 6,
  },
  pageInfo: {
    fontSize: 12,
    color: 'var(--ds-text-secondary)',
    fontVariantNumeric: 'tabular-nums',
  },
  pageControls: {
    display: 'flex',
    gap: 4,
  },
  pageBtn: {
    width: 28,
    height: 28,
    borderRadius: DESIGN.radii.sm,
    border: `1px solid var(--ds-border)`,
    background: 'var(--ds-surface)',
    color: 'var(--ds-text-primary)',
    cursor: 'pointer',
    display: 'inline-flex',
    alignItems: 'center',
    justifyContent: 'center',
    padding: 0,
    transitionProperty: 'background-color, border-color',
    transitionDuration: '0.15s',
  },
}

