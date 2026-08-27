import { useEffect, useRef, useState } from 'react'
import { listRuns, subscribeToRunStream, type AgentRunSummary, type TraceEvent } from '../api/sse'
import { TraceAccordion } from './TraceAccordion'
import { PHASE_COLOR } from '../lib/phaseColors'

interface TraceEntry {
  id: number
  event: TraceEvent
  ts: string
}

interface Props {
  namespace: string
  name: string
}

export function DeploymentRuns({ namespace, name }: Props) {
  const [runs, setRuns] = useState<AgentRunSummary[]>([])
  const [selected, setSelected] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [traceEntries, setTraceEntries] = useState<TraceEntry[]>([])
  const counter = useRef(0)

  useEffect(() => {
    setSelected(null)
    const load = () =>
      listRuns(namespace, name)
        .then((data) => {
          data.sort((a, b) => (b.startTime ?? '').localeCompare(a.startTime ?? ''))
          setRuns(data)
          // Auto-select the most recent run on first load.
          setSelected((prev) => prev ?? data[0]?.name ?? null)
        })
        .catch((e: Error) => setError(e.message))
    load()
    const id = setInterval(load, 5_000)
    return () => clearInterval(id)
  }, [namespace, name])

  useEffect(() => {
    if (!selected) return
    setTraceEntries([])
    counter.current = 0
    const unsub = subscribeToRunStream(selected, (event) => {
      setTraceEntries((prev) => [
        ...prev,
        { id: counter.current++, event, ts: new Date().toISOString() },
      ])
    }, namespace)
    return unsub
  }, [selected, namespace])

  const selectedRun = runs.find((r) => r.name === selected)
  const streaming = selectedRun?.phase === 'Running' || selectedRun?.phase === 'Pending'

  if (error) return <div style={styles.error}>{error}</div>

  return (
    <div style={styles.container}>
      {/* Run picker bar */}
      <div style={styles.picker}>
        <span style={styles.label}>Runs for {name}</span>
        <div style={styles.runList}>
          {runs.length === 0 && <span style={styles.empty}>No runs yet</span>}
          {runs.map((run) => (
            <button
              type="button"
              key={run.name}
              style={{
                ...styles.runBtn,
                ...(selected === run.name ? styles.runBtnActive : {}),
              }}
              onClick={() => setSelected(run.name)}
            >
              <span
                style={{
                  ...styles.dot,
                  background: PHASE_COLOR[run.phase] ?? '#6b7280',
                }}
              />
              <span style={styles.runName}>{run.name}</span>
              <span style={styles.runPhase}>{run.phase}</span>
            </button>
          ))}
        </div>
      </div>

      {/* Trace panel */}
      <div style={styles.trace}>
        {selected ? (
          <TraceAccordion entries={traceEntries} streaming={streaming} />
        ) : (
          <div style={styles.emptyTrace}>
            {runs.length === 0
              ? 'Send a message in the Chat tab to create a run'
              : 'Select a run above'}
          </div>
        )}
      </div>
    </div>
  )
}

const styles: Record<string, React.CSSProperties> = {
  container: {
    display: 'flex',
    flexDirection: 'column',
    height: '100%',
    overflow: 'hidden',
  },
  picker: {
    padding: '8px 12px',
    background: 'var(--ds-surface)',
    borderBottom: '1px solid var(--ds-border)',
    display: 'flex',
    flexDirection: 'column',
    gap: 6,
    flexShrink: 0,
  },
  label: {
    fontSize: 11,
    fontWeight: 700,
    textTransform: 'uppercase',
    color: 'var(--ds-text-muted)',
    letterSpacing: '0.04em',
  },
  runList: {
    display: 'flex',
    gap: 4,
    flexWrap: 'wrap',
    maxHeight: 80,
    overflowY: 'auto',
  },
  runBtn: {
    display: 'flex',
    alignItems: 'center',
    gap: 6,
    padding: '4px 10px',
    borderRadius: 6,
    border: '1px solid var(--ds-border)',
    background: 'transparent',
    color: 'var(--ds-text-secondary)',
    cursor: 'pointer',
    fontSize: 12,
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", "Consolas", monospace',
  },
  runBtnActive: {
    background: 'var(--ds-bg)',
    borderColor: 'var(--ds-accent)',
    color: 'var(--ds-text-primary)',
  },
  dot: {
    width: 6,
    height: 6,
    borderRadius: '50%',
    flexShrink: 0,
  },
  runName: { fontWeight: 600, fontSize: 11 },
  runPhase: { fontSize: 10, color: 'var(--ds-text-muted)' },
  empty: { fontSize: 12, color: 'var(--ds-text-muted)', fontStyle: 'italic' },
  trace: { flex: 1, overflow: 'hidden' },
  emptyTrace: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    height: '100%',
    color: 'var(--ds-text-muted)',
    fontSize: 14,
  },
  error: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    height: '100%',
    color: 'var(--ds-error)',
    fontSize: 14,
  },
}
