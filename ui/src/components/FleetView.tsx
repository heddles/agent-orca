/**
 * Fleet overview: one row per agent with owner, health, runs, error rate, p95,
 * spend against budget, evaluation score and pending approvals.
 *
 * Spend against a budget is a meter: the unfilled track is a lighter step of
 * the accent, the fill moves accent, warning, error as the budget is used, and
 * the figure beside it always states the numbers, so state never rests on
 * colour alone.
 */
import { useEffect, useMemo, useState, type CSSProperties } from 'react'
import { Link } from 'react-router'
import { getFleet, type FleetHealthStatus, type FleetOwner, type FleetRow, type FleetWindow } from '../api/fleet'
import { ds } from '../lib/designSystem'
import { StatusIcon } from '../lib/icons'
import { PHASE_STYLE } from '../lib/phaseColors'
import { EmptyState } from '../lib/layout'
import { ICON } from '../lib/icons'
import { useNamespace } from '../lib/namespace'
import { agentDetailsPath, agentRunsPath, ROUTES } from '../lib/routes'

// ── Filtering and sorting (pure, tested) ─────────────────────────────────────

export interface FleetFilters {
  query: string
  owner: '' | FleetOwner
  health: '' | FleetHealthStatus
  published: '' | 'published' | 'unpublished'
}

export const NO_FLEET_FILTERS: FleetFilters = { query: '', owner: '', health: '', published: '' }

export function filterFleet(rows: FleetRow[], f: FleetFilters): FleetRow[] {
  const q = f.query.trim().toLowerCase()
  return rows.filter((r) =>
    (!q || r.name.toLowerCase().includes(q) || r.maintainer.toLowerCase().includes(q) || r.namespace.toLowerCase().includes(q))
    && (!f.owner || r.owner === f.owner)
    && (!f.health || r.health.status === f.health)
    && (!f.published || (f.published === 'published') === r.published))
}

export type FleetSortKey =
  'name' | 'owner' | 'health' | 'runs' | 'errorRate' | 'p95' | 'spend' | 'approvals' | 'eval'

/** Worst first when sorted ascending, so problems rise to the top. */
const HEALTH_ORDER: Record<FleetHealthStatus, number> = { failing: 0, degraded: 1, unknown: 2, healthy: 3 }

function sortValue(r: FleetRow, key: FleetSortKey): string | number | null {
  switch (key) {
    case 'name': return r.name.toLowerCase()
    case 'owner': return r.owner
    case 'health': return HEALTH_ORDER[r.health.status]
    case 'runs': return r.runCount
    case 'errorRate': return r.errorRate
    case 'p95': return r.p95DurationSeconds
    case 'spend': return r.spend.usd
    case 'approvals': return r.pendingApprovals
    case 'eval': return r.lastEvalScore
  }
}

/** Sorts a copy. Rows with no value for the column always go last. */
export function sortFleet(rows: FleetRow[], key: FleetSortKey, dir: 'asc' | 'desc'): FleetRow[] {
  const sign = dir === 'asc' ? 1 : -1
  return [...rows].sort((a, b) => {
    const av = sortValue(a, key)
    const bv = sortValue(b, key)
    if (av === null && bv === null) return 0
    if (av === null) return 1
    if (bv === null) return -1
    const c = av < bv ? -1 : av > bv ? 1 : 0
    return c !== 0 ? sign * c : a.name.localeCompare(b.name)
  })
}

// ── Formatting ───────────────────────────────────────────────────────────────

export function formatDuration(seconds: number | null): string {
  if (seconds === null) return '—'
  if (seconds < 60) return `${Math.round(seconds)} s`
  const m = Math.floor(seconds / 60)
  const sec = Math.round(seconds % 60)
  return sec === 0 ? `${m} min` : `${m} min ${sec} s`
}

export function formatPercent(rate: number | null): string {
  return rate === null ? '—' : `${Math.round(rate * 100)}%`
}

export function formatUSD(v: number): string {
  if (v === 0) return '$0.00'
  return v < 0.01 ? 'under $0.01' : `$${v.toFixed(2)}`
}

// ── Health badge ─────────────────────────────────────────────────────────────

/** Health reuses the phase badge styles, so its colours match the rest of the UI. */
const HEALTH_STYLE: Record<FleetHealthStatus, { phase: string; label: string }> = {
  healthy: { phase: 'Succeeded', label: 'Healthy' },
  degraded: { phase: 'WaitingForInput', label: 'Degraded' },
  failing: { phase: 'Failed', label: 'Failing' },
  unknown: { phase: 'Paused', label: 'Unknown' },
}

export function HealthBadge({ status, reason }: { status: FleetHealthStatus; reason: string }) {
  const h = HEALTH_STYLE[status]
  return (
    <span title={reason} style={{ ...s.badge, ...PHASE_STYLE[h.phase] }}>
      <StatusIcon phase={h.phase} />
      {h.label}
    </span>
  )
}

// ── Spend meter ──────────────────────────────────────────────────────────────

const WARN_AT = 0.8

export function SpendMeter({ usd, budget, scope }: { usd: number; budget: number | null; scope: 'agent' | 'tenant' }) {
  if (budget === null) return <span style={s.figure}>{formatUSD(usd)}</span>
  const ratio = usd / budget
  const fill = ratio > 1 ? ds.error : ratio >= WARN_AT ? ds.warning : ds.accent
  const label = `${formatUSD(usd)} of ${formatUSD(budget)} ${scope} budget, ${Math.round(ratio * 100)}% used`
  return (
    <div style={s.meterWrap}>
      <div role="meter" aria-label={label} aria-valuemin={0} aria-valuemax={budget} aria-valuenow={Math.min(usd, budget)} style={s.track}>
        <div style={{ ...s.fill, width: `${Math.min(ratio, 1) * 100}%`, background: fill }} />
      </div>
      <span style={s.figure}>
        {formatUSD(usd)} of {formatUSD(budget)}
        {scope === 'tenant' && <span style={s.muted}> (tenant)</span>}
        {ratio > 1 && <strong style={{ color: ds.error }}> over</strong>}
      </span>
    </div>
  )
}

// ── The view ─────────────────────────────────────────────────────────────────

const COLUMNS: { key: FleetSortKey; label: string; numeric?: boolean }[] = [
  { key: 'name', label: 'Agent' },
  { key: 'owner', label: 'Owner' },
  { key: 'health', label: 'Health' },
  { key: 'runs', label: 'Runs', numeric: true },
  { key: 'errorRate', label: 'Error rate', numeric: true },
  { key: 'p95', label: 'p95 time', numeric: true },
  { key: 'spend', label: 'Spend vs budget' },
  { key: 'approvals', label: 'Approvals', numeric: true },
  { key: 'eval', label: 'Eval score', numeric: true },
]

const OWNER_LABEL: Record<FleetOwner, string> = { ui: 'Created in the UI', git: 'Managed in Git', unknown: 'Unknown' }

export function FleetView() {
  const { namespace } = useNamespace()
  const [windowSel, setWindowSel] = useState<FleetWindow>('24h')
  const [rows, setRows] = useState<FleetRow[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [filters, setFilters] = useState<FleetFilters>(NO_FLEET_FILTERS)
  const [sort, setSort] = useState<{ key: FleetSortKey; dir: 'asc' | 'desc' }>({ key: 'health', dir: 'asc' })

  useEffect(() => {
    let cancelled = false
    setError(null)
    getFleet(windowSel, namespace)
      .then((f) => { if (!cancelled) setRows(f.agents) })
      .catch((e: unknown) => { if (!cancelled) setError(e instanceof Error ? e.message : String(e)) })
    return () => { cancelled = true }
  }, [windowSel, namespace])

  const shown = useMemo(() => sortFleet(filterFleet(rows ?? [], filters), sort.key, sort.dir), [rows, filters, sort])
  const set = <K extends keyof FleetFilters>(k: K, v: FleetFilters[K]) => setFilters((f) => ({ ...f, [k]: v }))
  const filtered = filters.query || filters.owner || filters.health || filters.published

  return (
    <div style={s.root}>
      <h2 style={s.heading}>Fleet</h2>
      <p style={s.lede}>
        How every agent is doing over the last {windowSel === '24h' ? '24 hours' : '7 days'}. Worst health comes first.
      </p>

      <div style={s.toolbar}>
        <input
          type="search" aria-label="Search agents" placeholder="Search by name or maintainer"
          value={filters.query} onChange={(e) => set('query', e.target.value)} style={{ ...s.input, minWidth: 220 }}
        />
        <select aria-label="Filter by owner" value={filters.owner} onChange={(e) => set('owner', e.target.value as FleetFilters['owner'])} style={s.input}>
          <option value="">Any owner</option>
          <option value="ui">{OWNER_LABEL.ui}</option>
          <option value="git">{OWNER_LABEL.git}</option>
          <option value="unknown">{OWNER_LABEL.unknown}</option>
        </select>
        <select aria-label="Filter by health" value={filters.health} onChange={(e) => set('health', e.target.value as FleetFilters['health'])} style={s.input}>
          <option value="">Any health</option>
          {(Object.keys(HEALTH_STYLE) as FleetHealthStatus[]).map((h) => <option key={h} value={h}>{HEALTH_STYLE[h].label}</option>)}
        </select>
        <select aria-label="Filter by published state" value={filters.published} onChange={(e) => set('published', e.target.value as FleetFilters['published'])} style={s.input}>
          <option value="">Published or not</option>
          <option value="published">Published</option>
          <option value="unpublished">Not published</option>
        </select>
        <div role="group" aria-label="Time window" style={s.windowGroup}>
          {(['24h', '7d'] as const).map((w) => (
            <button
              key={w} type="button" aria-pressed={windowSel === w} onClick={() => setWindowSel(w)}
              style={{ ...s.windowBtn, ...(windowSel === w ? s.windowBtnOn : null) }}
            >
              {w === '24h' ? '24 hours' : '7 days'}
            </button>
          ))}
        </div>
      </div>

      {error && <div role="alert" style={s.error}>Could not load the fleet: {error}</div>}
      {!error && rows === null && <p style={s.muted}>Loading the fleet…</p>}
      {rows !== null && rows.length === 0 && (
        <EmptyState icon={ICON.agent} title="No agents yet">
          Agents appear here once they exist in your workspace.
        </EmptyState>
      )}
      {rows !== null && rows.length > 0 && shown.length === 0 && (
        <p role="status" style={s.muted}>
          No agents match {filtered ? 'these filters' : 'this view'}.{' '}
          <button type="button" style={s.link} onClick={() => setFilters(NO_FLEET_FILTERS)}>Clear filters</button>
        </p>
      )}

      {shown.length > 0 && (
        <div style={{ overflowX: 'auto' }}>
          <table style={s.table}>
            <caption style={s.srOnly}>Agents with health, run statistics, spend and pending approvals</caption>
            <thead>
              <tr>
                {COLUMNS.map((c) => {
                  const active = sort.key === c.key
                  return (
                    <th
                      key={c.key} scope="col"
                      aria-sort={active ? (sort.dir === 'asc' ? 'ascending' : 'descending') : 'none'}
                      style={{ ...s.th, textAlign: c.numeric ? 'right' : 'left' }}
                    >
                      <button
                        type="button" style={s.sortBtn}
                        onClick={() => setSort((p) => ({ key: c.key, dir: p.key === c.key && p.dir === 'asc' ? 'desc' : 'asc' }))}
                      >
                        {c.label}{active ? (sort.dir === 'asc' ? ' ▲' : ' ▼') : ''}
                      </button>
                    </th>
                  )
                })}
              </tr>
            </thead>
            <tbody>
              {shown.map((r) => (
                <tr key={`${r.namespace}/${r.name}`}>
                  <td style={s.td}>
                    <Link to={agentDetailsPath(r.namespace, r.name)} style={s.nameLink}>{r.name}</Link>
                    <div style={s.sub}>{r.maintainer}{r.published ? ' · published' : ''}</div>
                  </td>
                  <td style={s.td}>{OWNER_LABEL[r.owner]}</td>
                  <td style={s.td}><HealthBadge status={r.health.status} reason={r.health.reason} /></td>
                  <td style={{ ...s.td, ...s.num }}>
                    {r.runCount > 0
                      ? <Link to={agentRunsPath(r.name)} style={s.link} aria-label={`${r.runCount} runs of ${r.name}`}>{r.runCount}</Link>
                      : 0}
                  </td>
                  <td style={{ ...s.td, ...s.num }}>{formatPercent(r.errorRate)}</td>
                  <td style={{ ...s.td, ...s.num }}>{formatDuration(r.p95DurationSeconds)}</td>
                  <td style={s.td}>
                    {r.spend.agentBudgetUSD !== null
                      ? <SpendMeter usd={r.spend.usd} budget={r.spend.agentBudgetUSD} scope="agent" />
                      : <SpendMeter usd={r.spend.usd} budget={r.spend.tenantBudgetUSD} scope="tenant" />}
                  </td>
                  <td style={{ ...s.td, ...s.num }}>
                    {r.pendingApprovals > 0
                      ? <Link to={ROUTES.approvals} style={s.link} aria-label={`${r.pendingApprovals} pending approvals for ${r.name}`}>{r.pendingApprovals}</Link>
                      : 0}
                  </td>
                  <td style={{ ...s.td, ...s.num }} title={r.lastEvalScore === null ? 'No evaluation has run yet' : undefined}>
                    {r.lastEvalScore === null ? '—' : r.lastEvalScore.toFixed(2)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

const s: Record<string, CSSProperties> = {
  root: { padding: '8px 24px 24px', display: 'flex', flexDirection: 'column', gap: 12, flex: 1, overflowY: 'auto' },
  heading: { margin: 0, fontSize: 20, fontWeight: 700, color: ds.textPrimary },
  lede: { margin: 0, fontSize: 13, color: ds.textSecondary },
  toolbar: { display: 'flex', flexWrap: 'wrap', gap: 8, alignItems: 'center' },
  input: {
    height: 32, borderRadius: 6, border: '1px solid var(--ds-border)', background: ds.bg, color: ds.textPrimary,
    fontSize: 12, padding: '0 8px',
  },
  windowGroup: { marginLeft: 'auto', display: 'flex', gap: 4 },
  windowBtn: {
    height: 32, padding: '0 12px', borderRadius: 6, border: '1px solid var(--ds-border)', background: 'transparent',
    color: ds.textSecondary, fontSize: 12, cursor: 'pointer',
  },
  windowBtnOn: { borderColor: ds.accent, color: ds.textPrimary, fontWeight: 600 },
  table: { width: '100%', borderCollapse: 'collapse', fontSize: 13 },
  th: { padding: '6px 10px', borderBottom: '1px solid var(--ds-border)', whiteSpace: 'nowrap' },
  sortBtn: {
    background: 'none', border: 'none', cursor: 'pointer', color: ds.textSecondary, fontSize: 12, fontWeight: 600, padding: 0,
  },
  td: { padding: '8px 10px', borderBottom: '1px solid var(--ds-border)', verticalAlign: 'middle', color: ds.textPrimary },
  num: { textAlign: 'right', fontVariantNumeric: 'tabular-nums' },
  sub: { fontSize: 11, color: ds.textMuted, marginTop: 2 },
  nameLink: { color: ds.accent, textDecoration: 'none', fontWeight: 600 },
  link: { color: ds.accent, textDecoration: 'none', background: 'none', border: 'none', cursor: 'pointer', fontSize: 'inherit', padding: 0 },
  muted: { color: ds.textMuted, fontSize: 12 },
  error: { padding: 10, borderRadius: 6, background: 'rgba(239,68,68,.1)', color: ds.error, fontSize: 13 },
  badge: {
    display: 'inline-flex', alignItems: 'center', gap: 4, padding: '2px 8px', borderRadius: 4, fontSize: 11, fontWeight: 600,
  },
  meterWrap: { display: 'flex', flexDirection: 'column', gap: 3, minWidth: 140 },
  // The track is a lighter step of the accent, so the whole bar reads as one meter.
  track: { height: 6, borderRadius: 3, background: 'rgba(59,130,246,.18)', overflow: 'hidden' },
  fill: { height: '100%', borderRadius: 3 },
  figure: { fontSize: 11, color: ds.textSecondary, fontVariantNumeric: 'tabular-nums' },
  srOnly: { position: 'absolute', width: 1, height: 1, overflow: 'hidden', clip: 'rect(0 0 0 0)', whiteSpace: 'nowrap' },
}
