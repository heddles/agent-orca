/**
 * SystemStatusPage — system health view showing subsystem status, model-provider
 * reachability, and Prometheus metrics as time-series graphs + a latency
 * heatmap.
 *
 * Metrics are real: egress/request counters and the request-duration histogram
 * are read from their actual registries (default registry for egress, the
 * external API's externalReg for requests), and a rolling sample ring buffer
 * backs the time-range graphs.
 *
 * Alerts are intentionally NOT displayed here — alerting is an org-level
 * concern, not the UI's. The raw alert history remains available via the
 * GET /api/system/alerts API for an org's alertmanager to consume.
 *
 * Uses better-layout §5 (progressive disclosure), better-accessibility §9
 * (redundant status cue: icon + color), better-typography §11 (tabular-nums),
 * and better-ui §3 (shadow-based elevation).
 */
import { useEffect, useState, useCallback, useMemo } from 'react'
import { getSystemStatus, type SystemStatus, type MetricSample, type MetricRange } from '../api/sse'
import { DSChip, DSCard, DSButton } from '../lib/primitives'
import { DESIGN, ds } from '../lib/designSystem'
import { Icon, ICON } from '../lib/icons'

interface Props {
  navigateToTab: (tab: string) => void
}

const TIME_RANGES: MetricRange[] = ['1h', '6h', '24h', '7d']

export function SystemStatusPage({ navigateToTab }: Props) {
  // navigateToTab is reserved for future drill-in (e.g. "view agents for this
  // provider"); it's part of the shell contract but not yet wired.
  void navigateToTab
  const [status, setStatus] = useState<SystemStatus | null>(null)
  const [loading, setLoading] = useState(true)
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null)
  const [timeRange, setTimeRange] = useState<MetricRange>('24h')

  const load = useCallback(async () => {
    try {
      const s = await getSystemStatus(timeRange).catch(() => null)
      if (s) setStatus(s)
    } catch {
      // ignore — empty state will show
    } finally {
      setLoading(false)
      setLastUpdated(new Date())
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [timeRange])

  useEffect(() => {
    void load()
    const id = setInterval(() => void load(), 5000)
    return () => clearInterval(id)
  }, [load])

  if (loading && !status) {
    return <SystemStatusSkeleton />
  }

  if (!status) {
    return (
      <div style={ss.empty}>
        <Icon icon={ICON.warning} size={32} ariaHidden={true} style={{ opacity: 0.4 }} />
        <div style={ss.emptyTitle}>Unable to load system status</div>
        <DSButton variant="secondary" onClick={() => void load()}>
          Retry
        </DSButton>
      </div>
    )
  }

  return (
    <div style={ss.root}>
      {/* Header */}
      <div style={ss.header}>
        <div>
          <h1 style={ss.title}>System Status</h1>
          <p style={ss.subtitle}>
            Real-time health of all subsystems. Auto-refreshes every 5 seconds.
          </p>
        </div>
        <div style={ss.headerRight}>
          <span style={ss.lastUpdated} aria-live="polite">
            {lastUpdated ? `Updated ${formatAge(lastUpdated)} ago` : ''}
          </span>
          <DSButton variant="ghost" size="sm" icon onClick={() => void load()} aria-label="Refresh status" title="Refresh">
            <Icon icon={ICON.retry} size={14} />
          </DSButton>
          <DSChip variant="neutral" size="sm">v{status.version ?? 'dev'}</DSChip>
        </div>
      </div>

      {/* Subsystem health cards */}
      <h2 style={ss.sectionTitle}>Subsystem Health</h2>
      <div style={ss.healthGrid}>
        {(status.subsystems ?? []).map((s) => (
          <HealthCard key={s.name} subSystem={s} />
        ))}
        {/* Redis banner */}
        {!status.stateConfigured && (
          <DSCard padded>
            <div style={ss.redisBanner}>
              <Icon icon={ICON.warning} size={16} ariaHidden={true} />
              <span>
                Redis is not configured. Run history, cost tracking, and streaming will be unavailable.
                Set STATE_BACKEND=redis and REDIS_URL on the operator.
              </span>
            </div>
          </DSCard>
        )}
      </div>

      {/* Model provider health */}
      {status.modelProviders && status.modelProviders.length > 0 && (
        <>
          <h2 style={ss.sectionTitle}>Model Providers</h2>
          <ModelProviderTable providers={status.modelProviders} />
        </>
      )}

      {/* Metrics: graphs + latency heatmap (no alert echo; no MCP aggregate) */}
      {status.metrics && (
        <>
          <h2 style={ss.sectionTitle}>
            Metrics
            <TimeRangeSelector range={timeRange} onChange={setTimeRange} />
          </h2>
          <MetricsBody metrics={status.metrics} />
        </>
      )}
    </div>
  )
}

// ── Sub-components ───────────────────────────────────────────────────────────

function HealthCard({ subSystem }: { subSystem: NonNullable<SystemStatus['subsystems']>[0] }) {
  const isUp = subSystem.status === 'up'
  const isDown = subSystem.status === 'down'
  const variant = isUp ? 'success' : isDown ? 'error' : 'warning'

  return (
    <div style={ss.healthCard}>
      <div style={ss.healthCardHeader}>
        <span style={ss.healthName}>{subSystem.name}</span>
        <DSChip
          variant={variant}
          dot
          dotColor={isUp ? 'var(--ds-success)' : isDown ? 'var(--ds-error)' : 'var(--ds-warning)'}
          size="sm"
        >
          {subSystem.status}
        </DSChip>
      </div>
      {subSystem.message && <div style={ss.healthMsg}>{subSystem.message}</div>}
      {subSystem.latencyMs !== undefined && subSystem.latencyMs > 0 && (
        <div style={ss.healthLatency}>{subSystem.latencyMs} ms</div>
      )}
    </div>
  )
}

type ProviderSortKey = 'name' | 'namespace' | 'ready' | 'latency'

function ModelProviderTable({ providers }: { providers: NonNullable<SystemStatus['modelProviders']> }) {
  // Expand/collapse so the (often wide) provider table can be tucked away when
  // the user is scanning the metrics above it. Collapsed, it shows a one-line
  // summary; expanded, the full sortable table renders.
  const [open, setOpen] = useState(true)
  // Sort state lives on the component (not the page) so the user's choice and
  // the deterministic default survive the 5s poll — rows keep their position and
  // only their content updates in place instead of the whole table
  // "regenerating". Click a column header to sort.
  const [sortKey, setSortKey] = useState<ProviderSortKey>('namespace')
  const [sortDir, setSortDir] = useState<'asc' | 'desc'>('asc')

  const ready = providers.filter((p) => p.ready).length
  const notReady = providers.length - ready

  const sorted = useMemo(() => {
    const dir = sortDir === 'asc' ? 1 : -1
    return [...providers].sort((a, b) => {
      let cmp = 0
      switch (sortKey) {
        case 'name':
          cmp = a.name.localeCompare(b.name)
          break
        case 'ready':
          cmp = (a.ready === b.ready) ? 0 : (a.ready ? 1 : -1)
          break
        case 'latency':
          cmp = (a.latencyMs ?? 0) - (b.latencyMs ?? 0)
          break
        default: // namespace
          cmp = a.namespace.localeCompare(b.namespace)
      }
      if (cmp !== 0) return cmp * dir
      // Stable tiebreak: namespace then name, so equal primary keys never jitter.
      cmp = a.namespace.localeCompare(b.namespace)
      if (cmp !== 0) return cmp
      return a.name.localeCompare(b.name)
    })
  }, [providers, sortKey, sortDir])

  const toggle = (key: ProviderSortKey) => {
    if (sortKey === key) {
      setSortDir(sortDir === 'asc' ? 'desc' : 'asc')
    } else {
      setSortKey(key)
      setSortDir('asc')
    }
  }

  return (
    <DSCard padded>
      <div style={ss.expHeader}>
        <DSButton
          icon
          variant="ghost"
          size="sm"
          aria-expanded={open}
          aria-controls="model-providers-body"
          title={open ? 'Collapse providers' : 'Expand providers'}
          aria-label={open ? 'Collapse providers' : 'Expand providers'}
          onClick={() => setOpen((v) => !v)}
        >
          <Icon
            icon={ICON.chevronDown}
            size={14}
            ariaHidden
            style={{
              transitionProperty: 'transform',
              transitionDuration: '0.15s',
              transitionTimingFunction: 'ease',
              transform: open ? 'rotate(0)' : 'rotate(-90deg)',
            }}
          />
        </DSButton>
        <span style={ss.expSummary} aria-hidden={!open}>
          {providers.length} provider{providers.length !== 1 ? 's' : ''} · {ready} ready, {notReady} not ready
        </span>
      </div>
      {open && (
        <div id="model-providers-body" style={ss.expBody}>
          <div style={ss.tableContainer}>
            <table style={ss.table}>
              <thead>
                <tr>
                  <SortableHeader name="Provider" active={sortKey === 'name'} desc={sortDir === 'desc'} onClick={() => toggle('name')} />
                  <SortableHeader name="Namespace" active={sortKey === 'namespace'} desc={sortDir === 'desc'} onClick={() => toggle('namespace')} />
                  <SortableHeader name="Status" active={sortKey === 'ready'} desc={sortDir === 'desc'} onClick={() => toggle('ready')} />
                  <SortableHeader name="Latency" active={sortKey === 'latency'} desc={sortDir === 'desc'} onClick={() => toggle('latency')} align="right" />
                  <th style={ss.th}>Message</th>
                </tr>
              </thead>
              <tbody>
                {sorted.map((p) => (
                  <tr key={`${p.namespace}/${p.name}`}>
                    <td style={ss.td}>
                      <code style={ss.code}>{p.name}</code>
                    </td>
                    <td style={ss.td}>
                      <code style={ss.code}>{p.namespace || '—'}</code>
                    </td>
                    <td style={ss.td}>
                      <DSChip variant={p.ready ? 'success' : 'error'} size="sm" dot dotColor={p.ready ? 'var(--ds-success)' : 'var(--ds-error)'}>
                        {p.ready ? 'Ready' : 'Not Ready'}
                      </DSChip>
                    </td>
                    <td style={{ ...ss.td, textAlign: 'right' }}>
                      {p.latencyMs ? `${p.latencyMs} ms` : '—'}
                    </td>
                    <td style={ss.td}>
                      <span style={{ ...ss.td, color: p.ready ? 'var(--ds-text-secondary)' : 'var(--ds-error)', wordBreak: 'break-word', maxWidth: 200 }}>{p.message || '—'}</span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </DSCard>
  )
}

function SortableHeader({
  name,
  active,
  desc,
  onClick,
  align = 'left',
}: {
  name: string
  active: boolean
  desc: boolean
  onClick: () => void
  align?: 'left' | 'right'
}) {
  return (
    <th style={{ ...ss.th, textAlign: align }}>
      <button
        type="button"
        style={{
          ...ss.sortBtn,
          ...(active ? { color: 'var(--ds-accent)', fontWeight: 700 } : {}),
        }}
        onClick={onClick}
        aria-sort={active ? (desc ? 'descending' : 'ascending') : 'none'}
        aria-label={`Sort by ${name}`}
      >
        {name}
        {active && <span style={{ marginLeft: 4 }}>{desc ? '↓' : '↑'}</span>}
      </button>
    </th>
  )
}

// ── Metrics (graphs + heatmap) ────────────────────────────────────────────────

type SampleField = 'requestCount' | 'egressPublished' | 'egressFailed' | 'tokenThroughput'

const EGGRESS_TIPS = {
  egressPublished:
    'Results delivered to the configured egress sink (Kafka/PubSub/Redis stream) when an agent run reaches a terminal phase.',
  egressFailed:
    'Egress deliveries that failed — check operator logs and verify the egress sink credentials and connectivity.',
}

function MetricsBody({ metrics }: { metrics: NonNullable<SystemStatus['metrics']> }) {
  // Normalize a partial metrics object so a backend that omits a field (e.g. a
  // rolling upgrade returning `{}` for `metrics`, or a third-party scrape) can
  // never crash the status page with "cannot read property toLocaleString of
  // undefined" — the same blank-screen class as the prior runs:null crash.
  const samples = metrics.samples ?? []
  const requestCount = metrics.requestCount24h ?? 0
  const egressPublished = metrics.egressPublished ?? 0
  const egressFailed = metrics.egressFailed ?? 0
  const tokenThroughput = metrics.tokenThroughput ?? 0
  return (
    <div style={ss.metricsBody}>
      {/* Latency heatmap: p50 / p95 / p99 across the sampled window. */}
      <div style={ss.heatmapCard}>
        <div style={ss.heatmapHeader}>
          <span style={ss.sectionTitleInline}>Request latency</span>
          <span
            style={ss.heatmapHint}
            title="Heatmap of latency percentiles (ms) over the selected time range. Darker color = higher latency."
            aria-label="Latency heatmap: p50, p95, p99 in ms over time"
          >
            <Icon icon={ICON.help} size={12} ariaHidden={false} style={{ color: 'var(--ds-text-muted)' }} />
          </span>
        </div>
        <LatencyHeatmap samples={samples} />
        <div style={ss.heatmapLegend}>
          <span style={ss.legendDot}>●</span> Fast (&lt;100 ms)
          <span style={{ ...ss.legendDot, color: 'var(--ds-warning)' }}>●</span> Moderate (100–500 ms)
          <span style={{ ...ss.legendDot, color: 'var(--ds-error)' }}>●</span> Slow (&gt;500 ms)
        </div>
      </div>

      {/* Time-series graphs for the remaining aggregates. */}
      <div style={ss.graphGrid}>
        <MetricGraph
          title="API Requests"
          value={requestCount.toLocaleString()}
          unit="requests (cumulative)"
          icon={ICON.tokens}
          color="var(--ds-accent)"
          samples={samples}
          field="requestCount"
        />
        <MetricGraph
          title="Egress Published"
          value={egressPublished.toLocaleString()}
          unit="results"
          icon={ICON.success}
          color="var(--ds-success)"
          tooltip={EGGRESS_TIPS.egressPublished}
          samples={samples}
          field="egressPublished"
        />
        <MetricGraph
          title="Egress Failed"
          value={egressFailed.toLocaleString()}
          unit="failures"
          icon={ICON.error}
          color="var(--ds-error)"
          tooltip={EGGRESS_TIPS.egressFailed}
          samples={samples}
          field="egressFailed"
        />
        <MetricGraph
          title="Model-router Tokens"
          value={tokenThroughput > 0 ? Math.round(tokenThroughput).toLocaleString() : '—'}
          unit="tokens (cumulative)"
          icon={ICON.token}
          color={tokenThroughput > 0 ? 'var(--ds-accent)' : 'var(--ds-text-muted)'}
          tooltip="Cumulative output tokens streamed by live model-router pods, scraped from each pod's :9091/metrics endpoint. Shows — when no model-router pods are reachable from the operator."
          samples={samples}
          field="tokenThroughput"
          nullZero
        />
      </div>
    </div>
  )
}

function MetricGraph({
  title,
  value,
  unit,
  icon,
  color,
  samples,
  field,
  tooltip,
  nullZero,
}: {
  title: string
  value: string
  unit: string
  icon: any
  color: string
  samples: MetricSample[]
  field: SampleField
  tooltip?: string
  nullZero?: boolean
}) {
  const values = samples.map((s) => Number(s[field] ?? 0))
  const hasData = values.length > 0
  return (
    <DSCard padded>
      <div style={ss.graphRow}>
        <div style={{ ...ss.graphRow, flex: 1 }}>
          <div style={ss.graphTitle}>
            <Icon icon={icon} size={14} ariaHidden style={{ color: ds.textSecondary }} />
            <span>{title}</span>
            {tooltip && (
              <span title={tooltip} aria-label={tooltip} style={ss.graphTip}>
                <Icon icon={ICON.help} size={11} ariaHidden={false} style={{ color: 'var(--ds-text-muted)' }} />
              </span>
            )}
          </div>
          <div style={ss.graphValue} title={unit}>{value}</div>
          <div style={ss.graphUnit}>{unit}</div>
          {/* Visible caption so the explanation always displays (hover tooltips
              are easy to miss). The help icon keeps the full text for hover too. */}
          {tooltip && <div style={ss.graphCaption} title={tooltip}>{tooltip}</div>}
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          {hasData ? (
            <Sparkline values={values} color={color} nullZero={nullZero} />
          ) : (
            <span style={ss.graphStub}>insufficient history</span>
          )}
        </div>
      </div>
    </DSCard>
  )
}

function Sparkline({ values, color, nullZero }: { values: number[]; color: string; nullZero?: boolean }) {
  // nullZero metrics (e.g. token throughput stub) render a flat baseline instead
  // of a misleading 0-to-0 spike when every sample is 0.
  const show = nullZero && Math.max(...values) === 0 ? [] : values
  if (show.length < 2) {
    return <span style={ss.sparklineStub}>—</span>
  }
  const min = Math.min(...show)
  const max = Math.max(...show)
  const span = max - min || 1
  const w = 110
  const h = 34
  const pad = 3
  const pts = show.map((v, i) => {
    const x = pad + (i / (show.length - 1)) * (w - 2 * pad)
    const y = h - pad - ((v - min) / span) * (h - 2 * pad)
    return `${x.toFixed(1)},${y.toFixed(1)}`
  }).join(' ')
  return (
    <svg width={w} height={h} style={ss.sparkline} aria-hidden="true" role="img" aria-label={`trend across ${values.length} samples`}>
      <polyline points={pts} fill="none" stroke={color} strokeWidth={1.5} strokeLinejoin="round" strokeLinecap="round" />
    </svg>
  )
}

function LatencyHeatmap({ samples }: { samples: MetricSample[] }) {
  if (samples.length === 0) {
    return (
      <div style={ss.heatEmpty}>Recorded metric samples will render here as they accumulate (one per scrape).</div>
    )
  }
  const percs: Array<{ key: string; label: string; get: (s: MetricSample) => number }> = [
    { key: 'p50', label: 'p50', get: (s) => s.p50LatencyMs },
    { key: 'p95', label: 'p95', get: (s) => s.p95LatencyMs },
    { key: 'p99', label: 'p99', get: (s) => s.p99LatencyMs },
  ]
  // Downsample the time axis to at most 24 columns so the heatmap stays readable.
  const maxCols = 24
  const step = Math.max(1, Math.ceil(samples.length / maxCols))
  const cols: MetricSample[] = []
  for (let i = 0; i < samples.length; i += step) cols.push(samples[i])

  return (
    <div style={ss.heatGrid} role="table" aria-label="Latency percentile heatmap">
      {percs.map((p) => (
        <div
          key={p.key}
          style={{ ...ss.heatRow, gridTemplateColumns: `48px repeat(${cols.length}, minmax(28px, 1fr))` }}
          role="row"
        >
          <span style={ss.heatRowLabel} role="rowheader">{p.label}</span>
          {cols.map((s, i) => {
            const v = p.get(s)
            const tier = v > 500 ? 'slow' : v > 100 ? 'moderate' : 'fast'
            return (
              <span
                key={i}
                style={{
                  ...ss.heatCell,
                  ...(tier === 'slow' ? ss.heatCellSlow : tier === 'moderate' ? ss.heatCellMod : ss.heatCellFast),
                }}
                title={`${p.label}: ${v.toFixed(0)} ms · ${new Date(s.time * 1000).toLocaleTimeString()}`}
              >
                {v.toFixed(0)}
              </span>
            )
          })}
        </div>
      ))}
    </div>
  )
}

function TimeRangeSelector({ range, onChange }: { range: MetricRange; onChange: (r: MetricRange) => void }) {
  return (
    <div style={ss.rangeSelector} role="radiogroup" aria-label="Metrics time range">
      {TIME_RANGES.map((r) => (
        <button
          key={r}
          type="button"
          role="radio"
          aria-checked={range === r}
          style={{
            ...ss.rangeBtn,
            ...(range === r ? ss.rangeBtnActive : {}),
          }}
          onClick={() => onChange(r)}
        >
          {r}
        </button>
      ))}
    </div>
  )
}

// ── Skeleton ─────────────────────────────────────────────────────────────────

function SystemStatusSkeleton() {
  return (
    <div style={ss.root}>
      <h1 style={ss.title}>System Status</h1>
      <h2 style={ss.sectionTitle}>Subsystem Health</h2>
      <div style={ss.healthGrid}>
        {Array.from({ length: 4 }).map((_, i) => (
          <DSCard key={i} padded>
            <div style={ss.skeletonLine} />
            <div style={{ ...ss.skeletonLine, width: '60%', marginTop: 6 }} />
          </DSCard>
        ))}
      </div>
    </div>
  )
}

// ── Helpers ──────────────────────────────────────────────────────────────────

function formatAge(d: Date): string {
  const s = Math.max(0, Math.round((Date.now() - d.getTime()) / 1000))
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.round(s / 60)}m`
  return `${Math.round(s / 3600)}h`
}

// ── Styles ───────────────────────────────────────────────────────────────────

const ss: Record<string, React.CSSProperties> = {
  root: {
    flex: 1,
    overflowY: 'auto',
    padding: `${DESIGN.space.xl} ${DESIGN.space.xxl}`,
    display: 'flex',
    flexDirection: 'column',
    gap: DESIGN.space.xl,
  },
  header: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    flexWrap: 'wrap',
    gap: 12,
  },
  title: {
    fontSize: DESIGN.font.size.display,
    fontWeight: 700,
    color: 'var(--ds-text-primary)',
    margin: 0,
    letterSpacing: '-0.5px',
  },
  subtitle: {
    fontSize: 13,
    color: 'var(--ds-text-secondary)',
    marginTop: 4,
  },
  headerRight: {
    display: 'flex',
    alignItems: 'center',
    gap: 12,
    flexShrink: 0,
  },
  lastUpdated: {
    fontSize: 11,
    color: 'var(--ds-text-muted)',
    fontFamily: DESIGN.font.mono,
  },
  sectionTitle: {
    fontSize: 16,
    fontWeight: 600,
    color: 'var(--ds-text-primary)',
    margin: 0,
    display: 'flex',
    alignItems: 'center',
    gap: 12,
  },
  sectionTitleInline: {
    fontSize: 11,
    fontWeight: 600,
    color: 'var(--ds-text-secondary)',
    textTransform: 'uppercase',
    letterSpacing: '0.06em',
  },
  redisBanner: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
    fontSize: 12,
    color: 'var(--ds-warning)',
    lineHeight: 1.5,
  },
  healthGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))',
    gap: 16,
  },
  healthCard: {
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.lg,
    padding: 16,
    display: 'flex',
    flexDirection: 'column',
    gap: 6,
    boxShadow: 'var(--ds-card-shadow)',
  },
  healthCardHeader: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
  },
  healthName: {
    fontSize: 13,
    fontWeight: 600,
    color: 'var(--ds-text-primary)',
    fontFamily: DESIGN.font.mono,
  },
  healthMsg: {
    fontSize: 11,
    color: 'var(--ds-text-muted)',
    lineHeight: 1.4,
  },
  healthLatency: {
    fontSize: 11,
    color: 'var(--ds-text-secondary)',
    fontVariantNumeric: 'tabular-nums' as const,
  },
  tableContainer: {
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.lg,
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
  td: {
    padding: '8px 12px',
    borderBottom: `1px solid var(--ds-border-light)`,
    color: 'var(--ds-text-primary)',
    fontSize: 12,
  },
  code: {
    fontFamily: DESIGN.font.mono,
    fontSize: 11,
    color: 'var(--ds-text-secondary)',
    background: 'rgba(148,163,184,.08)',
    padding: '1px 5px',
    borderRadius: 3,
  },
  sortBtn: {
    background: 'transparent',
    border: 'none',
    padding: 0,
    cursor: 'pointer',
    fontFamily: DESIGN.font.family,
    fontSize: 10,
    color: 'var(--ds-text-secondary)',
    transitionProperty: 'color, font-weight',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
    display: 'inline-flex',
    alignItems: 'center',
  },
  expHeader: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
    marginBottom: 8,
  },
  expSummary: {
    fontSize: 12,
    fontWeight: 500,
    color: 'var(--ds-text-secondary)',
    fontFamily: DESIGN.font.mono,
  },
  expBody: {
    borderTop: `1px solid var(--ds-border)`,
    paddingTop: 8,
  },
  metricsBody: {
    display: 'flex',
    flexDirection: 'column',
    gap: DESIGN.space.lg,
  },
  heatmapCard: {
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.lg,
    padding: 16,
    boxShadow: 'var(--ds-card-shadow)',
  },
  heatmapHeader: {
    display: 'flex',
    alignItems: 'center',
    gap: 6,
    marginBottom: 10,
  },
  heatmapHint: {
    display: 'inline-flex',
    cursor: 'help',
  },
  heatmapLegend: {
    marginTop: 8,
    fontSize: 11,
    color: 'var(--ds-text-muted)',
    display: 'flex',
    gap: 10,
    alignItems: 'center',
    flexWrap: 'wrap',
  },
  legendDot: {
    fontSize: 12,
    color: 'var(--ds-success)',
    fontFamily: DESIGN.font.mono,
  },
  heatGrid: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 6,
  },
  // Each percentile row is its own grid: 48px label column + one column per
  // (down-sampled) time bucket, so cells lay out horizontally instead of
  // stacking into a 2-wide vertical run of scattered boxes.
  heatRow: {
    display: 'grid',
    gap: 2,
    alignItems: 'center',
  },
  heatRowLabel: {
    fontFamily: DESIGN.font.mono,
    fontSize: 11,
    fontWeight: 700,
    color: 'var(--ds-text-secondary)',
    justifySelf: 'start',
  },
  heatCell: {
    fontFamily: DESIGN.font.mono,
    fontSize: 9,
    fontWeight: 600,
    color: 'var(--ds-bg)',
    textAlign: 'center' as const,
    borderRadius: DESIGN.radii.sm,
    padding: '3px 4px',
    minWidth: 36,
    boxSizing: 'border-box' as const,
    fontVariantNumeric: 'tabular-nums' as const,
  },
  heatCellFast: { background: 'var(--ds-success)' },
  heatCellMod: { background: 'var(--ds-warning)' },
  heatCellSlow: { background: 'var(--ds-error)' },
  heatEmpty: {
    fontSize: 12,
    color: 'var(--ds-text-muted)',
    fontStyle: 'italic',
    padding: '8px 0',
  },
  graphGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))',
    gap: 16,
  },
  graphRow: {
    display: 'flex',
    alignItems: 'center',
    gap: 12,
  },
  graphTitle: {
    fontSize: 11,
    fontWeight: 600,
    color: 'var(--ds-text-secondary)',
    textTransform: 'uppercase',
    letterSpacing: '0.06em',
    display: 'flex',
    alignItems: 'center',
    gap: 6,
    marginBottom: 4,
  },
  graphTip: {
    cursor: 'help',
    display: 'inline-flex',
  },
  graphValue: {
    fontSize: 22,
    fontWeight: 700,
    color: 'var(--ds-text-primary)',
    fontVariantNumeric: 'tabular-nums' as const,
    lineHeight: 1,
  },
  graphUnit: {
    fontSize: 9,
    color: 'var(--ds-text-muted)',
    textTransform: 'uppercase',
    letterSpacing: '0.06em',
    marginTop: 2,
  },
  graphCaption: {
    fontSize: 9,
    color: 'var(--ds-text-secondary)',
    lineHeight: 1.3,
    marginTop: 2,
    wordBreak: 'break-word',
    maxWidth: 200,
  },
  sparkline: {
    flexShrink: 0,
  },
  sparklineStub: {
    fontFamily: DESIGN.font.mono,
    fontSize: 11,
    color: 'var(--ds-text-muted)',
    minWidth: 80,
  },
  graphStub: {
    fontFamily: DESIGN.font.mono,
    fontSize: 10,
    color: 'var(--ds-text-muted)',
    minWidth: 80,
  },
  rangeSelector: {
    display: 'inline-flex',
    gap: 2,
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.full,
    padding: '2px 4px',
    marginLeft: 'auto',
  },
  rangeBtn: {
    padding: '4px 10px',
    fontSize: 11,
    fontWeight: 500,
    color: 'var(--ds-text-secondary)',
    background: 'transparent',
    border: 'none',
    borderRadius: DESIGN.radii.full,
    cursor: 'pointer',
    fontFamily: DESIGN.font.mono,
    fontVariantNumeric: 'tabular-nums' as const,
    transitionProperty: 'color, background-color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  rangeBtnActive: {
    background: 'var(--ds-accent)',
    color: '#fff',
  },
  empty: {
    flex: 1,
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'center',
    justifyContent: 'center',
    gap: 12,
    color: 'var(--ds-text-muted)',
    padding: DESIGN.space.xxl,
  },
  emptyTitle: {
    fontSize: 16,
    fontWeight: 500,
    color: 'var(--ds-text-secondary)',
  },
  skeletonLine: {
    background: 'rgba(148,163,184,.15)',
    borderRadius: DESIGN.radii.md,
    height: 22,
    width: '50%',
    animation: 'aoPulse 1.5s ease infinite',
  },
}
