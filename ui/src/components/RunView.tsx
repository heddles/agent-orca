/**
 * RunView — output-first detail view for a single AgentRun.
 * Shows: header stats → OutputCard (hero) → input section → TraceAccordion → RouterAccordion.
 * When child runs exist, a "Child Runs" tab is added alongside "Details".
 */
import { useEffect, useRef, useState } from 'react'
import {
  getRun,
  isSSEClientParseEventType,
  subscribeToRunStream,
  type AgentRunDetail,
  type AgentRunSummary,
  type TraceEntry,
} from '../api/sse'
import { STREAM_EVENT_TYPE } from '../contracts/events'
import { StatusBadge } from './StatusBadge'
import { OutputCard } from './OutputCard'
import { TraceAccordion } from './TraceAccordion'
import { RouterAccordion } from './RouterAccordion'
import type { ResourceSelection } from './ResourceList'
import { PHASE_COLOR } from '../lib/phaseColors'

interface Props {
  runId: string
  namespace?: string
  /** Ancestor navigation stack — entries before the current run. */
  breadcrumbs?: ResourceSelection[]
  /** Whether this run was navigated to from the home dashboard. */
  cameFromHome?: boolean
  /** Jump back to breadcrumb at the given index. */
  onNavigateUp?: (index: number) => void
  /** Navigate into a child run. */
  onNavigateToRun?: (runName: string, namespace: string) => void
}



export function RunView({
  runId,
  namespace = 'default',
  breadcrumbs = [],
  cameFromHome = false,
  onNavigateUp,
  onNavigateToRun,
}: Props) {
  const [detail, setDetail] = useState<AgentRunDetail | null>(null)
  const [entries, setEntries] = useState<TraceEntry[]>([])
  const [streamOutput, setStreamOutput] = useState('')
  const [markdown, setMarkdown] = useState(false)
  const [viewTab, setViewTab] = useState<'details' | 'children'>('details')
  const [childDetails, setChildDetails] = useState<AgentRunSummary[]>([])
  const counter = useRef(0)
  const childUnsubsRef = useRef<Array<() => void>>([])
  const subscribedChildRefsRef = useRef<Set<string>>(new Set())

  useEffect(() => {
    childUnsubsRef.current.forEach((fn) => fn())
    childUnsubsRef.current = []
    subscribedChildRefsRef.current.clear()
    setDetail(null)
    setEntries([])
    setStreamOutput('')
    setViewTab('details')
    setChildDetails([])
    counter.current = 0
    getRun(runId, namespace).then(setDetail).catch(() => {})
  }, [runId, namespace])

  useEffect(() => {
    const unsub = subscribeToRunStream(runId, (event) => {
      if (event.type !== STREAM_EVENT_TYPE.token) {
        // Suppress pre-populated candidate entries — only show the actual runtime selection.
        const isCandidate = event.type === STREAM_EVENT_TYPE.modelSelected && event.reason.includes('configured provider')
        if (!isCandidate) {
          setEntries((prev: TraceEntry[]) => [...prev, { id: counter.current++, event, ts: new Date().toISOString() }])
        }
      }
      if (event.type === STREAM_EVENT_TYPE.finalOutput || event.type === STREAM_EVENT_TYPE.done) {
        setStreamOutput(event.output)
        getRun(runId, namespace).then(setDetail).catch(() => {})
      } else if (event.type === STREAM_EVENT_TYPE.error || event.type === STREAM_EVENT_TYPE.fail) {
        getRun(runId, namespace).then(setDetail).catch(() => {})
      } else if (
        event.type === STREAM_EVENT_TYPE.agentEvent &&
        isSSEClientParseEventType(event.eventType)
      ) {
        getRun(runId, namespace).then(setDetail).catch(() => {})
      }
    }, namespace)
    return unsub
  }, [runId, namespace])

  // Fetch child run summaries whenever childRunRefs change.
  useEffect(() => {
    const refs = detail?.childRunRefs
    if (!refs || refs.length === 0) { setChildDetails([]); return }

    let cancelled = false
    Promise.all(
      refs.map((name: string) =>
        getRun(name, namespace)
          .then((d): AgentRunSummary => ({
            name: d.name,
            namespace: d.namespace,
            agentRef: d.agentRef,
            phase: d.phase as AgentRunSummary['phase'],
            spendUSD: d.spendUSD,
            restartCount: d.restartCount,
            startTime: d.startTime,
            completionTime: d.completionTime,
          }))
          .catch((): null => null),
      ),
    ).then((results) => {
      if (cancelled) return
      setChildDetails(results.filter((r: AgentRunSummary | null): r is AgentRunSummary => r !== null))
    })

    return () => { cancelled = true }
  }, [detail?.childRunRefs, namespace])

  // Subscribe to each child run's stream to bubble up their tool calls.
  useEffect(() => {
    const refs = detail?.childRunRefs ?? []
    const newRefs = refs.filter((r) => !subscribedChildRefsRef.current.has(r))
    if (newRefs.length === 0) return
    for (const childName of newRefs) {
      subscribedChildRefsRef.current.add(childName)
      const unsub = subscribeToRunStream(childName, (event) => {
        if (event.type === STREAM_EVENT_TYPE.token) return
        if (event.type === STREAM_EVENT_TYPE.error && event.message === 'Stream connection lost') return
        const isCandidate = event.type === STREAM_EVENT_TYPE.modelSelected && event.reason.includes('configured provider')
        if (!isCandidate) {
          setEntries((prev) => [...prev, { id: counter.current++, event, ts: new Date().toISOString(), childRunName: childName }])
        }
      }, namespace)
      childUnsubsRef.current.push(unsub)
    }
  }, [detail?.childRunRefs, namespace])

  const phase = detail?.phase ?? 'Pending'
  const isRunning = phase === 'Running' || phase === 'Pending'
  const isFailed = phase === 'Failed'
  const hasChildren = (detail?.childRunRefs?.length ?? 0) > 0

  const output = streamOutput || detail?.output || ''

  const elapsed = (() => {
    if (!detail?.startTime) return null
    const end = detail.completionTime ? new Date(detail.completionTime) : new Date()
    return `${Math.round((end.getTime() - new Date(detail.startTime).getTime()) / 1000)}s`
  })()

  const agentRelTime = (() => {
    if (!detail?.startTime) return ''
    const diff = Math.round((Date.now() - new Date(detail.startTime).getTime()) / 1000)
    if (diff < 60) return `${diff}s ago`
    if (diff < 3600) return `${Math.round(diff / 60)}m ago`
    return `${Math.round(diff / 3600)}h ago`
  })()

  return (
    <div style={s.root}>
      {/* Breadcrumb trail */}
      {cameFromHome && (
        <div style={s.breadcrumb}>
          <button style={s.breadcrumbBtn} onClick={() => onNavigateUp?.(0)}>
            🏠 Home
          </button>
          <span style={s.breadcrumbSep}>/</span>
          <span style={s.breadcrumbCurrent}>{runId}</span>
        </div>
      )}
      {!cameFromHome && breadcrumbs.length > 0 && (
        <div style={s.breadcrumb}>
          {breadcrumbs.map((crumb, i) => (
            <span key={i} style={s.breadcrumbItem}>
              {i > 0 && <span style={s.breadcrumbSep}>/</span>}
              <button style={s.breadcrumbBtn} onClick={() => onNavigateUp?.(i)}>
                {crumb.name}
              </button>
            </span>
          ))}
          <span style={s.breadcrumbSep}>/</span>
          <span style={s.breadcrumbCurrent}>{runId}</span>
        </div>
      )}

      {/* Header */}
      <div style={s.header}>
        <div>
          <div style={s.title}>{runId}</div>
          <div style={s.subtitle}>
            {detail?.agentRef ?? '…'} · {agentRelTime} · {namespace}
          </div>
        </div>
        <div style={s.stats}>
          <StatusBadge phase={phase} />
          {elapsed && (
            <div style={s.chip}>⏱ <span style={s.chipVal}>{elapsed}</span></div>
          )}
          <div style={s.chip}>💰 <span style={s.chipVal}>${detail?.spendUSD ?? '0.0000'}</span></div>
          {(detail?.restartCount ?? 0) > 0 && (
            <div style={s.chip}>🔁 <span style={s.chipVal}>{detail!.restartCount} retries</span></div>
          )}
          {(detail?.maxContextTokens ?? 0) > 0 && (
            <div style={s.chip}>📏 <span style={s.chipVal}>
              {detail!.contextUsedTokens?.toLocaleString() ?? 0} / {detail!.maxContextTokens?.toLocaleString() ?? '—'} tokens
            </span></div>
          )}
          <label style={s.mdToggle}>
            <input
              type="checkbox"
              checked={markdown}
              onChange={(e: { target: { checked: boolean } }) => setMarkdown(e.target.checked)}
              style={{ accentColor: '#3b82f6', cursor: 'pointer', margin: 0 }}
            />
            <span style={s.mdLabel}>Markdown</span>
          </label>
        </div>
      </div>

      {/* Tab bar — only visible when there are child runs */}
      {hasChildren && (
        <div style={s.tabBar}>
          <button
            style={{ ...s.tab, ...(viewTab === 'details' ? s.tabActive : {}) }}
            onClick={() => setViewTab('details')}
          >
            Details
          </button>
          <button
            style={{ ...s.tab, ...(viewTab === 'children' ? s.tabActive : {}) }}
            onClick={() => setViewTab('children')}
          >
            Child Runs ({detail?.childRunRefs?.length ?? 0})
          </button>
        </div>
      )}

      {viewTab === 'children' ? (
        <div style={s.childList}>
          {childDetails.length === 0 && (
            <div style={s.empty}>Loading child runs…</div>
          )}
          {childDetails.map((run: AgentRunSummary) => (
            <div
              key={run.name}
              style={s.runCard}
              onClick={() => onNavigateToRun?.(run.name, run.namespace)}
            >
              <div style={s.runCardHeader}>
                <span style={{ ...s.runDot, background: PHASE_COLOR[run.phase] ?? '#6b7280' }} />
                <span style={s.runCardName}>{run.name}</span>
                <span style={s.runCardPhase}>{run.phase}</span>
                <span style={{ flex: 1 }} />
                {parseFloat(run.spendUSD) > 0 && (
                  <span style={s.runCardCost}>${run.spendUSD}</span>
                )}
              </div>
              <div style={s.runCardMeta}>
                {run.agentRef}
                {run.startTime && ` · ${new Date(run.startTime).toLocaleString()}`}
              </div>
            </div>
          ))}
        </div>
      ) : (
        <>
          {/* Error banner for failed runs */}
          {isFailed && detail?.lastRestartReason && (
            <div style={s.errorCard}>
              <div style={s.errorTitle}>⚠ Run Failed</div>
              <div style={s.errorBody}>{detail.lastRestartReason}</div>
            </div>
          )}

          {/* Clarify question banner */}
          {phase === 'WaitingForInput' && detail?.clarifyQuestion && (
            <div style={s.clarifyCard}>
              <div style={s.clarifyTitle}>Waiting for human input</div>
              <div style={s.clarifyBody}>{detail.clarifyQuestion}</div>
            </div>
          )}

          {/* Output card */}
          {(output || isRunning) && (
            <OutputCard
              output={output || ''}
              streaming={isRunning && !output}
              markdown={markdown}
            />
          )}

          {/* Input section */}
          {detail?.input && (
            <div style={s.inputSection}>
              <div style={s.inputLabel}>Input prompt</div>
              <div style={s.inputText}>{detail.input}</div>
            </div>
          )}

          {/* Trace accordion */}
          <TraceAccordion entries={entries} streaming={isRunning} markdown={markdown} />

          {/* Router accordion */}
          {detail && detail.routingDecisions.length > 0 && (
            <RouterAccordion decisions={detail.routingDecisions} />
          )}
        </>
      )}
    </div>
  )
}

const s: Record<string, React.CSSProperties> = {
  root: {
    flex: 1,
    overflowY: 'auto',
    padding: '24px 28px',
    display: 'flex',
    flexDirection: 'column',
    gap: 16,
  },
  breadcrumb: {
    display: 'flex',
    alignItems: 'center',
    gap: 4,
    flexWrap: 'wrap',
    marginBottom: -8,
  },
  breadcrumbItem: {
    display: 'flex',
    alignItems: 'center',
    gap: 4,
  },
  breadcrumbBtn: {
    background: 'none',
    border: 'none',
    color: '#3b82f6',
    fontSize: 12,
    fontWeight: 500,
    cursor: 'pointer',
    padding: 0,
    fontFamily: 'monospace',
  },
  breadcrumbSep: {
    fontSize: 12,
    color: '#334155',
  },
  breadcrumbCurrent: {
    fontSize: 12,
    color: '#64748b',
    fontFamily: 'monospace',
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
    maxWidth: 260,
  },
  header: {
    display: 'flex',
    alignItems: 'flex-start',
    justifyContent: 'space-between',
    flexWrap: 'wrap',
    gap: 12,
  },
  title: { fontSize: 18, fontWeight: 700, color: '#f1f5f9' },
  subtitle: { fontSize: 12, color: '#94a3b8', marginTop: 4 },
  stats: {
    display: 'flex',
    alignItems: 'center',
    gap: 10,
    flexWrap: 'wrap',
  },
  chip: {
    background: '#1e293b',
    border: '1px solid #334155',
    borderRadius: 6,
    padding: '5px 10px',
    display: 'flex',
    alignItems: 'center',
    gap: 6,
    fontSize: 12,
    color: '#94a3b8',
  },
  chipVal: { color: '#f1f5f9', fontWeight: 500 },
  mdToggle: {
    display: 'flex',
    alignItems: 'center',
    gap: 6,
    cursor: 'pointer',
    fontSize: 12,
  },
  mdLabel: { color: '#94a3b8', fontWeight: 500, userSelect: 'none' },
  tabBar: {
    display: 'flex',
    borderBottom: '1px solid #334155',
    marginTop: -8,
  },
  tab: {
    padding: '8px 16px',
    fontSize: 13,
    fontWeight: 500,
    color: '#64748b',
    background: 'none',
    border: 'none',
    borderBottom: '2px solid transparent',
    cursor: 'pointer',
    transition: 'color 0.15s',
  },
  tabActive: {
    color: '#3b82f6',
    borderBottomColor: '#3b82f6',
  },
  childList: {
    display: 'flex',
    flexDirection: 'column',
    gap: 6,
  },
  runCard: {
    padding: '10px 14px',
    borderRadius: 8,
    border: '1px solid #334155',
    background: '#1e293b',
    cursor: 'pointer',
    display: 'flex',
    flexDirection: 'column',
    gap: 4,
  },
  runCardHeader: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
  },
  runDot: {
    width: 7,
    height: 7,
    borderRadius: '50%',
    flexShrink: 0,
  },
  runCardName: {
    fontSize: 13,
    fontWeight: 600,
    color: '#f1f5f9',
    fontFamily: 'monospace',
  },
  runCardPhase: {
    fontSize: 11,
    color: '#64748b',
  },
  runCardCost: {
    fontSize: 11,
    fontWeight: 600,
    color: '#4ade80',
    fontFamily: 'monospace',
  },
  runCardMeta: {
    fontSize: 11,
    color: '#64748b',
    paddingLeft: 15,
  },
  empty: {
    color: '#475569',
    fontSize: 13,
    padding: '16px 0',
  },
  errorCard: {
    background: 'rgba(239,68,68,.06)',
    border: '1px solid rgba(239,68,68,.25)',
    borderRadius: 8,
    padding: '14px 16px',
  },
  errorTitle: {
    fontSize: 12,
    fontWeight: 600,
    color: '#ef4444',
    marginBottom: 8,
    display: 'flex',
    alignItems: 'center',
    gap: 6,
  },
  errorBody: {
    fontFamily: 'monospace',
    fontSize: 12,
    color: '#fca5a5',
    lineHeight: 1.5,
    whiteSpace: 'pre-wrap',
  },
  inputSection: {
    background: '#1e293b',
    border: '1px solid #334155',
    borderRadius: 8,
    padding: '12px 16px',
  },
  inputLabel: {
    fontSize: 11,
    fontWeight: 600,
    textTransform: 'uppercase',
    letterSpacing: '0.08em',
    color: '#94a3b8',
    marginBottom: 8,
  },
  inputText: {
    fontSize: 13,
    color: '#94a3b8',
    fontStyle: 'italic',
    lineHeight: 1.5,
    whiteSpace: 'pre-wrap',
    wordBreak: 'break-word',
  },
  clarifyCard: {
    background: 'rgba(245,158,11,.06)',
    border: '1px solid rgba(245,158,11,.25)',
    borderRadius: 8,
    padding: '14px 16px',
  },
  clarifyTitle: {
    fontSize: 12,
    fontWeight: 600,
    color: '#f59e0b',
    marginBottom: 8,
  },
  clarifyBody: {
    fontSize: 13,
    color: '#fcd34d',
    lineHeight: 1.5,
    whiteSpace: 'pre-wrap',
    wordBreak: 'break-word',
  },
}
