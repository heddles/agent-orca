/**
 * CostDashboard — spend overview: KPI row, daily spend stacked by run outcome,
 * and a By Agent / By Model / By Outcome breakdown.
 *
 * Runs whose spend could not be captured are reported as untracked (banner +
 * chip), never folded into the totals as $0. Built from design-system
 * primitives: icons via `Icon`/`ICON`, colors via `var(--ds-*)` tokens and the
 * shared phase palette, `tabular-nums` on every amount (better-typography §11),
 * and a redundant icon cue next to every color (better-accessibility §9).
 */
import { useEffect, useState } from 'react'
import { getCosts, type CostData, type SidebarSelection } from '../api/sse'
import { DSButton, DSCard, DSChip, DSTabBar } from '../lib/primitives'
import { Icon, ICON, StatusIcon } from '../lib/icons'
import { PHASE_COLOR } from '../lib/phaseColors'
import {
  chartSummary,
  failedShare,
  failedShareHigh,
  outcomesInChart,
  sortedEntries,
  splitRunRef,
  usdNum,
} from '../lib/costMath'

type Scope = 'selection' | 'all'
type Breakdown = 'agent' | 'model' | 'outcome'

const BREAKDOWN_TABS: Array<{ key: Breakdown; label: string }> = [
  { key: 'agent', label: 'By Agent' },
  { key: 'model', label: 'By Model' },
  { key: 'outcome', label: 'By Outcome' },
]

const POLL_MS = 30_000

interface Props {
  namespace?: string
  selection?: SidebarSelection | null
  /** Open the run history, e.g. to inspect untracked runs. */
  onViewRuns?: () => void
  /** Open one run (from the untracked banner). */
  onNavigateToRun?: (runName: string, namespace: string) => void
}

/** Segment color for an outcome; unknown outcomes fall back to the muted token. */
function outcomeColor(outcome: string): string {
  return PHASE_COLOR[outcome] ?? 'var(--ds-text-muted)'
}

export function CostDashboard({ namespace = 'default', selection, onViewRuns, onNavigateToRun }: Props) {
  const [data, setData] = useState<CostData | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [scope, setScope] = useState<Scope>('selection')
  const [breakdown, setBreakdown] = useState<Breakdown>('agent')

  // Determine effective scope: fall back to 'all' when nothing is selected.
  const effectiveScope = selection ? scope : 'all'

  const fetchOpts =
    effectiveScope === 'all'
      ? undefined
      : selection?.kind === 'run'
        ? { run: selection.name }
        : selection?.kind === 'deployment'
          ? { deployment: selection.name }
          : undefined

  const effectiveNs =
    effectiveScope !== 'all' && selection?.kind === 'deployment'
      ? selection.namespace
      : namespace

  useEffect(() => {
    setData(null)
    setError(null)
    getCosts(effectiveNs, fetchOpts)
      .then(setData)
      .catch((e: Error) => setError(e.message))
    const id = setInterval(() => {
      getCosts(effectiveNs, fetchOpts).then(setData).catch(() => {})
    }, POLL_MS)
    return () => clearInterval(id)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [effectiveNs, effectiveScope, selection?.kind === 'run' ? selection.name : '', selection?.kind === 'deployment' ? selection.name : ''])

  // Build scope label.
  let scopeLabel = 'All runs'
  if (effectiveScope !== 'all' && selection) {
    scopeLabel =
      selection.kind === 'run'
        ? `Run: ${selection.name}`
        : `Deployment: ${selection.name}`
  }

  if (error) return <div role="alert" style={styles.error}>Failed to load costs: {error}</div>
  if (!data) return <div style={styles.loading}>Loading…</div>

  const entries =
    breakdown === 'agent' ? data.byAgent : breakdown === 'model' ? data.byModel : data.byOutcome
  const share = failedShare(data)
  const high = failedShareHigh(data)

  return (
    <div style={styles.container}>
      <div style={styles.headerRow}>
        <h2 style={styles.heading}>Cost Dashboard</h2>
        {selection && (
          <div role="group" aria-label="Cost scope" style={styles.scopeToggle}>
            <DSButton
              variant="secondary"
              size="sm"
              aria-pressed={effectiveScope === 'selection'}
              style={effectiveScope === 'selection' ? styles.scopeActive : undefined}
              onClick={() => setScope('selection')}
            >
              {selection.kind === 'run' ? 'This run' : 'This deployment'}
            </DSButton>
            <DSButton
              variant="secondary"
              size="sm"
              aria-pressed={effectiveScope === 'all'}
              style={effectiveScope === 'all' ? styles.scopeActive : undefined}
              onClick={() => setScope('all')}
            >
              All
            </DSButton>
          </div>
        )}
      </div>

      <div style={styles.scopeLabel}>{scopeLabel}</div>

      {data.archiveUnavailable && (
        <div style={styles.note}>
          <DSChip variant="warning" size="sm">
            <Icon icon={ICON.warning} size={11} />
            Run archive unavailable — totals cover live runs only
          </DSChip>
        </div>
      )}

      {data.untrackedCount > 0 && (
        <div role="status" style={styles.banner}>
          <Icon icon={ICON.warning} size={16} color="var(--ds-warning)" />
          <div style={styles.bannerBody}>
            <div style={styles.bannerTitle}>
              {data.untrackedCount} {data.untrackedCount === 1 ? 'run has' : 'runs have'} no recorded cost
            </div>
            <div style={styles.bannerText}>
              Their spend could not be read, so it is missing from the totals below rather than counted as $0.
              {data.untrackedSample.length > 0 && (
                <ul style={styles.sampleList} aria-label="Untracked runs">
                  {data.untrackedSample.map((ref) => {
                    const { namespace: ns, name } = splitRunRef(ref)
                    return (
                      <li key={ref}>
                        {onNavigateToRun ? (
                          <DSButton variant="ghost" size="sm" onClick={() => onNavigateToRun(name, ns || namespace)}>
                            {ref}
                          </DSButton>
                        ) : (
                          ref
                        )}
                      </li>
                    )
                  })}
                </ul>
              )}
            </div>
          </div>
          {onViewRuns && (
            <DSButton variant="secondary" size="sm" onClick={onViewRuns}>
              View runs
            </DSButton>
          )}
        </div>
      )}

      <div style={styles.kpiRow}>
        <Kpi label="Total spend" icon={ICON.cost} value={`$${data.totalUSD}`} emphasis="success" />
        <Kpi label="In-flight spend" icon={ICON.timer} value={`$${data.inFlightUSD}`} emphasis="muted" />
        <Kpi label="Avg cost / run" icon={ICON.chart} value={`$${data.avgRunUSD}`} />
        <Kpi
          label="Failed-run spend"
          icon={ICON.warning}
          value={`$${data.failedRunUSD}`}
          detail={`${Math.round(share * 100)}% of total`}
          emphasis={high ? 'error' : undefined}
        />
        {data.untrackedCount > 0 && (
          <DSCard style={styles.kpi}>
            <div style={styles.kpiLabel}>Untracked runs</div>
            <DSChip variant="warning">
              <Icon icon={ICON.warning} size={12} />
              {data.untrackedCount}
            </DSChip>
          </DSCard>
        )}
      </div>

      <DSCard style={styles.section}>
        <div style={styles.sectionTitle}>Daily spend by outcome (last {data.byDay.length} days)</div>
        <DayChart days={data.byDay} />
      </DSCard>

      <DSCard style={styles.section}>
        <DSTabBar
          tabs={BREAKDOWN_TABS}
          active={breakdown}
          onChange={(k) => setBreakdown(k as Breakdown)}
        />
        <div role="tabpanel" id={`${breakdown}-panel`} aria-label={BREAKDOWN_TABS.find((t) => t.key === breakdown)?.label} style={styles.panel}>
          <Entries entries={entries} outcomes={breakdown === 'outcome'} />
        </div>
      </DSCard>
    </div>
  )
}

function Kpi({
  label,
  icon,
  value,
  detail,
  emphasis,
}: {
  label: string
  icon: typeof ICON.cost
  value: string
  detail?: string
  emphasis?: 'success' | 'muted' | 'error'
}) {
  const color =
    emphasis === 'success'
      ? 'var(--ds-success)'
      : emphasis === 'error'
        ? 'var(--ds-error)'
        : emphasis === 'muted'
          ? 'var(--ds-text-muted)'
          : 'var(--ds-text-primary)'
  return (
    <DSCard style={styles.kpi}>
      <div style={styles.kpiLabel}>
        <Icon icon={icon} size={13} color={emphasis === 'error' ? 'var(--ds-error)' : undefined} />
        {label}
      </div>
      <div data-testid={`kpi-${label}`} style={{ ...styles.kpiValue, color }}>{value}</div>
      {detail && <div style={{ ...styles.kpiDetail, color: emphasis === 'error' ? 'var(--ds-error)' : undefined }}>{detail}</div>}
    </DSCard>
  )
}

function Entries({ entries, outcomes }: { entries: Record<string, string>; outcomes: boolean }) {
  const sorted = sortedEntries(entries)
  if (sorted.length === 0) return <div style={styles.empty}>No data</div>
  return (
    <>
      {sorted.map(([name, amount]) => (
        <div key={name} style={styles.entryRow}>
          <span style={styles.entryName}>
            {outcomes && (
              <>
                <StatusIcon phase={name} size={12} />
                <span aria-hidden style={{ ...styles.swatch, background: outcomeColor(name) }} />
              </>
            )}
            {name}
          </span>
          <span style={styles.entryValue}>${amount}</span>
        </div>
      ))}
    </>
  )
}

function DayChart({ days }: { days: CostData['byDay'] }) {
  if (days.length === 0) return <div style={styles.empty}>No data</div>
  const max = Math.max(...days.map((d) => usdNum(d.usd)), 0.01)
  const outcomes = outcomesInChart(days)
  return (
    <>
      <div role="img" aria-label={chartSummary(days)} style={styles.chart}>
        {days.map((d) => (
          <div key={d.date} style={styles.bar} title={`${d.date}: $${d.usd}`}>
            <div style={{ ...styles.stack, height: `${(usdNum(d.usd) / max) * 100}%` }}>
              {outcomes
                .filter((o) => usdNum(d.byOutcome[o]) > 0)
                .map((o) => (
                  <div
                    key={o}
                    data-outcome={o}
                    style={{ ...styles.segment, flexGrow: usdNum(d.byOutcome[o]), background: outcomeColor(o) }}
                  />
                ))}
            </div>
            <div style={styles.barLabel}>{d.date.slice(5)}</div>
          </div>
        ))}
      </div>
      <ul aria-label="Chart legend" style={styles.legend}>
        {outcomes.map((o) => (
          <li key={o} style={styles.legendItem}>
            <StatusIcon phase={o} size={12} />
            <span aria-hidden style={{ ...styles.swatch, background: outcomeColor(o) }} />
            {o}
          </li>
        ))}
      </ul>
    </>
  )
}

const styles: Record<string, React.CSSProperties> = {
  container: {
    padding: 24,
    fontFamily: 'system-ui, sans-serif',
    color: 'var(--ds-text-primary)',
    background: 'var(--ds-bg)',
    minHeight: '100%',
  },
  headerRow: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    marginBottom: 4,
  },
  heading: { margin: 0, fontSize: 20, fontWeight: 700 },
  scopeToggle: { display: 'flex', gap: 4 },
  scopeActive: { borderColor: 'var(--ds-accent)', color: 'var(--ds-text-primary)', fontWeight: 600 },
  scopeLabel: { fontSize: 12, color: 'var(--ds-text-muted)', marginBottom: 16 },
  note: { marginBottom: 12 },
  banner: {
    display: 'flex',
    alignItems: 'flex-start',
    gap: 12,
    padding: 12,
    marginBottom: 16,
    background: 'var(--ds-warning-bg)',
    border: '1px solid var(--ds-warning-border)',
    borderRadius: 8,
  },
  bannerBody: { flex: 1, minWidth: 0 },
  bannerTitle: { fontSize: 13, fontWeight: 600, color: 'var(--ds-text-primary)', marginBottom: 2 },
  bannerText: { fontSize: 12, color: 'var(--ds-text-secondary)' },
  sampleList: { margin: '6px 0 0', padding: 0, listStyle: 'none', display: 'flex', flexWrap: 'wrap', gap: 4 },
  kpiRow: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(170px, 1fr))', gap: 12, marginBottom: 16 },
  kpi: { minWidth: 0 },
  kpiLabel: {
    display: 'flex',
    alignItems: 'center',
    gap: 6,
    fontSize: 12,
    color: 'var(--ds-text-muted)',
    marginBottom: 6,
  },
  kpiValue: { fontSize: 24, fontWeight: 700, fontVariantNumeric: 'tabular-nums' },
  kpiDetail: { fontSize: 11, color: 'var(--ds-text-muted)', marginTop: 2, fontVariantNumeric: 'tabular-nums' },
  section: { marginBottom: 16 },
  sectionTitle: { fontSize: 13, fontWeight: 600, color: 'var(--ds-text-secondary)', marginBottom: 10 },
  panel: { paddingTop: 12 },
  entryRow: {
    display: 'flex',
    justifyContent: 'space-between',
    padding: '6px 0',
    borderBottom: '1px solid var(--ds-border)',
    fontSize: 13,
    fontVariantNumeric: 'tabular-nums',
  },
  entryName: { color: 'var(--ds-text-primary)', display: 'inline-flex', alignItems: 'center', gap: 6 },
  entryValue: { color: 'var(--ds-success)', fontWeight: 600, fontVariantNumeric: 'tabular-nums' },
  swatch: { width: 8, height: 8, borderRadius: 2, display: 'inline-block', flexShrink: 0 },
  empty: { color: 'var(--ds-text-muted)', fontSize: 13 },
  loading: { padding: 24, color: 'var(--ds-text-muted)' },
  error: { padding: 24, color: 'var(--ds-error)' },
  chart: { display: 'flex', alignItems: 'flex-end', gap: 4, height: 96, paddingTop: 4 },
  bar: {
    flex: 1,
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'center',
    justifyContent: 'flex-end',
    height: '100%',
    minWidth: 0,
  },
  stack: {
    width: '100%',
    display: 'flex',
    flexDirection: 'column-reverse',
    borderRadius: '2px 2px 0 0',
    overflow: 'hidden',
    minHeight: 2,
    transitionProperty: 'height',
    transitionDuration: '0.3s',
    transitionTimingFunction: 'ease',
  },
  segment: { flexBasis: 0, minHeight: 1 },
  barLabel: { fontSize: 9, color: 'var(--ds-text-muted)', marginTop: 2, fontVariantNumeric: 'tabular-nums' },
  legend: { display: 'flex', flexWrap: 'wrap', gap: 12, listStyle: 'none', margin: '10px 0 0', padding: 0 },
  legendItem: { display: 'inline-flex', alignItems: 'center', gap: 6, fontSize: 12, color: 'var(--ds-text-secondary)' },
}
