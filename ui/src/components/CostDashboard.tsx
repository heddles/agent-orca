import { useEffect, useState } from 'react'
import { getCosts, type CostData, type SidebarSelection } from '../api/sse'

type Scope = 'selection' | 'all'

interface Props {
  namespace?: string
  selection?: SidebarSelection | null
}

export function CostDashboard({ namespace = 'default', selection }: Props) {
  const [data, setData] = useState<CostData | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [scope, setScope] = useState<Scope>('selection')

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
    }, 30_000)
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

  if (error) return <div style={styles.error}>Failed to load costs: {error}</div>
  if (!data) return <div style={styles.loading}>Loading…</div>

  return (
    <div style={styles.container}>
      <div style={styles.headerRow}>
        <h2 style={styles.heading}>Cost Dashboard</h2>
        {selection && (
          <div style={styles.scopeToggle}>
            <button
              style={{ ...styles.scopeBtn, ...(effectiveScope === 'selection' ? styles.scopeBtnActive : {}) }}
              onClick={() => setScope('selection')}
            >
              {selection.kind === 'run' ? 'This run' : 'This deployment'}
            </button>
            <button
              style={{ ...styles.scopeBtn, ...(effectiveScope === 'all' ? styles.scopeBtnActive : {}) }}
              onClick={() => setScope('all')}
            >
              All
            </button>
          </div>
        )}
      </div>

      <div style={styles.scopeLabel}>{scopeLabel}</div>

      <div style={styles.card}>
        <div style={styles.totalLabel}>Total spend</div>
        <div style={styles.totalValue}>${data.totalUSD}</div>
      </div>

      <div style={styles.row}>
        <Section title="By Agent" entries={data.byAgent} />
        <Section title="By Model" entries={data.byModel} />
      </div>

      <div style={styles.card}>
        <div style={styles.sectionTitle}>Daily spend (last {data.byDay.length} days)</div>
        <DayChart days={data.byDay} />
      </div>
    </div>
  )
}

function Section({ title, entries }: { title: string; entries: Record<string, string> }) {
  const sorted = Object.entries(entries).sort(([, a], [, b]) => parseFloat(b) - parseFloat(a))
  return (
    <div style={{ ...styles.card, flex: 1 }}>
      <div style={styles.sectionTitle}>{title}</div>
      {sorted.length === 0 && <div style={styles.empty}>No data</div>}
      {sorted.map(([name, usd]) => (
        <div key={name} style={styles.entryRow}>
          <span style={styles.entryName}>{name}</span>
          <span style={styles.entryValue}>${usd}</span>
        </div>
      ))}
    </div>
  )
}

function DayChart({ days }: { days: Array<{ date: string; usd: string }> }) {
  if (days.length === 0) return <div style={styles.empty}>No data</div>
  const max = Math.max(...days.map((d) => parseFloat(d.usd)), 0.01)
  return (
    <div style={styles.chart}>
      {days.map((d) => {
        const pct = (parseFloat(d.usd) / max) * 100
        return (
          <div key={d.date} style={styles.bar} title={`${d.date}: $${d.usd}`}>
            <div style={{ ...styles.barFill, height: `${pct}%` }} />
            <div style={styles.barLabel}>{d.date.slice(5)}</div>
          </div>
        )
      })}
    </div>
  )
}

const styles: Record<string, React.CSSProperties> = {
  container: {
    padding: 24,
    fontFamily: 'system-ui, sans-serif',
    color: '#e2e8f0',
    background: '#0f172a',
    minHeight: '100%',
  },
  headerRow: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    marginBottom: 4,
  },
  heading: { margin: 0, fontSize: 20, fontWeight: 700 },
  scopeToggle: {
    display: 'flex',
    gap: 2,
    background: '#1e293b',
    borderRadius: 6,
    padding: 2,
  },
  scopeBtn: {
    padding: '4px 12px',
    borderRadius: 4,
    border: 'none',
    background: 'transparent',
    color: '#64748b',
    cursor: 'pointer',
    fontSize: 12,
    fontWeight: 500,
  },
  scopeBtnActive: {
    background: '#334155',
    color: '#f1f5f9',
    fontWeight: 600,
  },
  scopeLabel: {
    fontSize: 12,
    color: '#64748b',
    marginBottom: 16,
  },
  card: {
    background: '#1e293b',
    borderRadius: 8,
    padding: 16,
    marginBottom: 16,
  },
  totalLabel: { fontSize: 13, color: '#64748b', marginBottom: 4 },
  totalValue: { fontSize: 36, fontWeight: 700, color: '#22c55e' },
  row: { display: 'flex', gap: 16 },
  sectionTitle: { fontSize: 13, fontWeight: 600, color: '#94a3b8', marginBottom: 10 },
  entryRow: {
    display: 'flex',
    justifyContent: 'space-between',
    padding: '4px 0',
    borderBottom: '1px solid #334155',
    fontSize: 13,
  },
  entryName: { color: '#e2e8f0' },
  entryValue: { color: '#22c55e', fontWeight: 600 },
  empty: { color: '#64748b', fontSize: 13 },
  loading: { padding: 24, color: '#64748b' },
  error: { padding: 24, color: '#ef4444' },
  chart: {
    display: 'flex',
    alignItems: 'flex-end',
    gap: 4,
    height: 80,
    paddingTop: 4,
  },
  bar: {
    flex: 1,
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'center',
    justifyContent: 'flex-end',
    height: '100%',
  },
  barFill: {
    width: '100%',
    background: '#3b82f6',
    borderRadius: '2px 2px 0 0',
    minHeight: 2,
  },
  barLabel: { fontSize: 9, color: '#64748b', marginTop: 2 },
}
