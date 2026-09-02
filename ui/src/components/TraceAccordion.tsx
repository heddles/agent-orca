/** Collapsible execution trace accordion. */
import { useEffect, useState } from 'react'
import Markdown from 'react-markdown'
import { STREAM_EVENT_TYPE } from '../contracts/events'
import { isSSEClientParseEventType } from '../api/traceStream'
import type { TraceEntry, TraceEvent } from '../api/sse'
import { MCPAppFrame } from './MCPAppFrame'
import { Icon, ICON } from '../lib/icons'
import type { IconComponent } from '../lib/icons'

interface Props {
  entries: TraceEntry[]
  streaming?: boolean
  markdown?: boolean
}

/** Fun loading quips shown while trace events are being retrieved.
 *  Intentionally metaphorical — none claim a specific action is happening. */
const LOADING_QUIPS = [
  'The trace elves are working…',
  'Hold my tokens…',
  'Summoning traces from the void…',
  'The model is warming up its keyboard…',
  'Tracing the trace…',
  'Polishing the crystal ball…',
  'The agent is sharpening its pencils…',
  'Consulting the oracle…',
  'The suspense is killing us too…',
  'Counting sheep in the token stream…',
]

/** How long to keep showing quips before falling back to the static message. */
const QUIP_MIN_DURATION = 60_000 // 1 minute

/** Interval for cycling to a new random quip. */
const QUIP_CYCLE_INTERVAL = 10_000 // 10 seconds

const TRACE_FILTERS = [
  { key: 'all', label: 'All' },
  { key: 'tools', label: 'Tools' },
  { key: 'mcp', label: 'MCP' },
  { key: 'rag', label: 'RAG' },
  { key: 'guardrails', label: 'Guardrails' },
  { key: 'fallbacks', label: 'Fallbacks' },
  { key: 'errors', label: 'Errors' },
] as const
type TraceFilter = typeof TRACE_FILTERS[number]['key']

export function TraceAccordion({ entries, streaming, markdown }: Props) {
  const [open, setOpen] = useState(false)
  const [filter, setFilter] = useState<TraceFilter>('all')
  // Pick a random quip; cycles while in the empty/loading state.
  const [loadingQuip, setLoadingQuip] = useState(() => LOADING_QUIPS[Math.floor(Math.random() * LOADING_QUIPS.length)])
  // Show quips for at least QUIP_MIN_DURATION after mount, so users get the full
  // experience even if trace events arrive instantly on reconnect / old runs.
  // Only applies when streaming — non-streaming (historical) traces show the
  // empty state immediately instead of pausing for 60s on a quip.
  const [minLoading, setMinLoading] = useState(streaming ?? false)

  const inEmpty = entries.length === 0 && (streaming || minLoading)

  // One-shot: stop showing quips after the minimum duration (streaming only).
  useEffect(() => {
    if (!streaming) return
    const t = setTimeout(() => setMinLoading(false), QUIP_MIN_DURATION)
    return () => clearTimeout(t)
  }, [streaming])

  // Cycle quips every 10s while the empty/loading message is shown.
  useEffect(() => {
    if (!inEmpty) return
    const interval = setInterval(() => {
      setLoadingQuip(LOADING_QUIPS[Math.floor(Math.random() * LOADING_QUIPS.length)])
    }, QUIP_CYCLE_INTERVAL)
    return () => clearInterval(interval)
  }, [inEmpty])

  const showEmpty = inEmpty

  // Filter entries based on the selected filter
  const filteredEntries = entries.filter((entry) => {
    if (filter === 'all') return true
    const ev = entry.event
    const bt = (ev as { backendType?: string }).backendType
    switch (filter) {
      case 'tools': return ev.type === STREAM_EVENT_TYPE.toolCall || ev.type === STREAM_EVENT_TYPE.toolResult
      case 'mcp': return ((ev.type === STREAM_EVENT_TYPE.toolCall || ev.type === STREAM_EVENT_TYPE.toolResult || ev.type === 'mcpDiscovery') && (bt === 'mcp' || ev.type === 'mcpDiscovery'))
      case 'rag': return ((ev.type === STREAM_EVENT_TYPE.toolCall || ev.type === STREAM_EVENT_TYPE.toolResult || ev.type === 'ragResult') && (bt === 'rag' || ev.type === 'ragResult'))
      case 'guardrails': return ev.type === 'guardrail'
      case 'fallbacks': return ev.type === 'providerFallback'
      case 'errors': return ev.type === STREAM_EVENT_TYPE.error || ev.type === STREAM_EVENT_TYPE.fail
      default: return true
    }
  })

  const label = streaming ? (
    <span style={{ color: 'var(--ds-accent)' }}>● streaming</span>
  ) : (
    <span style={s.badge}>{entries.length} events</span>
  )

  return (
    <div style={s.accordion}>
      <button style={s.header} onClick={() => setOpen((o) => !o)} aria-expanded={open} aria-controls="trace-body">
        <div style={s.title}>
          <Icon icon={ICON.trace} size={14} ariaHidden={true} />
          {streaming ? 'Live Trace' : 'Execution Trace'}
          {label}
        </div>
        <Icon icon={ICON.chevronRight} size={11} style={{ ...s.chevron, transform: open ? 'rotate(90deg)' : undefined }} ariaHidden={true} />
      </button>
      {open && (
        <div id="trace-body" role="region" style={s.body}>
          {entries.length > 0 && (
            <div style={s.filterBar}>
              {TRACE_FILTERS.map((f) => (
                <button
                  key={f.key}
                  type="button"
                  style={{
                    ...s.filterBtn,
                    ...(filter === f.key ? s.filterBtnActive : {}),
                  }}
                  onClick={() => setFilter(f.key)}
                >
                  {f.label}
                </button>
              ))}
            </div>
          )}
          {entries.length === 0 && showEmpty && (
            <div style={s.empty}>
              {streaming ? (
                <>
                  <span style={s.loadingDot} />
                  <span>Retrieving events — {loadingQuip}</span>
                </>
              ) : (
                <>
                  <span style={s.quipDot} />
                  <span>Checking for traces. {loadingQuip}</span>
                </>
              )}
            </div>
          )}
          {entries.length === 0 && !showEmpty && (
            <div style={s.empty}>No trace events yet.</div>
          )}
          {filteredEntries.length === 0 && entries.length > 0 && !showEmpty && (
            <div style={s.empty}>No events match the "{TRACE_FILTERS.find(f => f.key === filter)?.label}" filter.</div>
          )}
          {filteredEntries.map((entry) => (
            <TraceRow key={entry.id} entry={entry} markdown={markdown ?? false} />
          ))}
        </div>
      )}
    </div>
  )
}

/** Map a backendType string to its corresponding Lucide icon. */
function backendTypeIcon(backendType?: string): IconComponent {
  switch (backendType) {
    case 'mcp': return ICON.mcpserver
    case 'rag': return ICON.knowledgebase
    case 'mcp-resource': return ICON.library
    case 'agent': return ICON.agent
    case 'external': return ICON.tool
    default: return ICON.settings
  }
}

function TraceRow({ entry, markdown }: { entry: TraceEntry; markdown: boolean }) {
  const { event, ts } = entry
  const time = ts.slice(11, 23)
  const childLabel = entry.childRunName
    ? <span style={s.childBadge}>{entry.childRunName.split('-').slice(-2).join('-')}</span>
    : null

  // Expanded state for the expandable "thinking" (reasoning) panel.
  const [expanded, setExpanded] = useState(false)

  const row: React.CSSProperties = {
    display: 'flex',
    gap: 10,
    marginBottom: 6,
    alignItems: 'flex-start',
    fontSize: 12,
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace',
    color: 'var(--ds-text-secondary)',
    lineHeight: 1.5,
    fontVariantNumeric: 'tabular-nums',
  }

  const muted: React.CSSProperties = { color: 'var(--ds-text-secondary)', flexShrink: 0, fontSize: 11 }
  const hi: React.CSSProperties = { color: 'var(--ds-text-primary)' }
  const content: React.CSSProperties = { color: 'var(--ds-text-secondary)', flex: 1, lineHeight: 1.5, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }
  const mdWrap: React.CSSProperties = { color: 'var(--ds-text-secondary)', fontFamily: 'system-ui,sans-serif', fontSize: 12, lineHeight: 1.5, flex: 1 }

  switch (event.type) {
    case 'token':
      return (
        <div style={row}>
          <span style={muted}>{time}</span>
          <Icon icon={ICON.token} size={14} color="var(--ds-accent)" />
          <span style={content}><span style={hi}>token</span> {event.content}</span>
        </div>
      )
    case 'thought': {
      // Single expandable "thinking" panel per reasoning block (RunView groups
      // consecutive deltas into one entry). Collapsed by default, click to inspect
      // the model's chain-of-thought without cluttering the trace.
      return (
        <div style={row}>
          <span style={muted}>{time}</span>
          <button
            type="button"
            aria-expanded={expanded}
            aria-label={expanded ? 'Collapse thinking' : 'Expand thinking'}
            onClick={() => setExpanded((e) => !e)}
            style={{
              margin: 0,
              padding: 0,
              border: 'none',
              background: 'transparent',
              cursor: 'pointer',
              display: 'inline-flex',
              alignItems: 'center',
              gap: 6,
            }}
          >
            <Icon icon={ICON.chevronRight} size={12} style={{ transform: expanded ? 'rotate(90deg)' : undefined }} ariaHidden={true} />
            <Icon icon={ICON.thought} size={14} color="var(--ds-accent)" />
            <span style={hi}>thinking</span>
          </button>
          {expanded && (
            <span style={{ ...content, marginTop: 4, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
              {event.content}
            </span>
          )}
        </div>
      )
    }
    case STREAM_EVENT_TYPE.toolCall:
      return (
        <div style={row}>
          <span style={muted}>{time}</span>
          <Icon icon={backendTypeIcon(event.backendType)} size={14} color="var(--ds-warning)" />
          {childLabel}
          {event.backendType && event.backendRef && (
            <span style={s.backendChip}>{event.backendType} · {event.backendRef}</span>
          )}
          {event.backendType && !event.backendRef && (
            <span style={s.backendChip}>{event.backendType}</span>
          )}
          <span style={content}><span style={hi}>calls tool</span> {event.name} <span style={{ color: 'var(--ds-text-muted)' }}>{event.arguments}</span></span>
        </div>
      )
    case STREAM_EVENT_TYPE.toolResult:
      return (
        <div style={{ ...row, flexDirection: 'column' }}>
          <div style={{ ...row, marginBottom: event.appUrl ? 0 : undefined }}>
            <span style={muted}>{time}</span>
            <Icon icon={backendTypeIcon(event.backendType)} size={14} color="var(--ds-success)" />
            {childLabel}
            {event.backendType && event.backendRef && (
              <span style={s.backendChip}>{event.backendType} · {event.backendRef}</span>
            )}
            <span style={content}>
              {markdown ? (
                <div style={mdWrap}><span style={hi}>result</span> {event.name}: <Markdown>{event.result}</Markdown></div>
              ) : (
                <span><span style={hi}>result</span> {event.name}: {event.result}</span>
              )}
              {event.durationMs !== undefined && event.durationMs > 0 && (
                <span style={{ color: 'var(--ds-text-muted)', marginLeft: 8, fontSize: 10 }}>
                  ({event.durationMs}ms)
                </span>
              )}
            </span>
          </div>
          {event.appUrl && <MCPAppFrame appUrl={event.appUrl} toolName={event.name} toolArgs={event.toolArgs} toolResult={event.toolResult} />}
        </div>
      )
    case STREAM_EVENT_TYPE.modelSelected:
      return (
        <div style={row}>
          <span style={muted}>{time}</span>
          <Icon icon={ICON.routed} size={14} color="var(--ds-purple)" />
          <span style={content}><span style={hi}>routed</span> {event.model} — {event.reason} (confidence: {(event.confidence * 100).toFixed(0)}%)</span>
        </div>
      )
    case STREAM_EVENT_TYPE.finalOutput:
      return (
        <div style={{ ...row, background: 'var(--ds-trace-success-bg)', borderRadius: 4, padding: '4px 8px', margin: '2px 0' }}>
          <span style={muted}>{time}</span>
          <Icon icon={ICON.check} size={14} color="var(--ds-success)" />
          {markdown ? (
            <div style={mdWrap}><Markdown>{event.output}</Markdown></div>
          ) : (
            <span style={content}>{event.output}</span>
          )}
        </div>
      )
    case STREAM_EVENT_TYPE.error:
      return (
        <div style={{ ...row, background: 'var(--ds-trace-error-bg)', borderRadius: 4, padding: '4px 8px', margin: '2px 0' }}>
          <span style={muted}>{time}</span>
          <Icon icon={ICON.close} size={14} color="var(--ds-error)" />
          <span style={{ ...content, color: '#fca5a5' }}>{event.message}</span>
        </div>
      )
    case STREAM_EVENT_TYPE.clarify:
      return (
        <div style={{ ...row, background: 'var(--ds-trace-warning-bg)', borderRadius: 4, padding: '4px 8px', margin: '2px 0' }}>
          <span style={muted}>{time}</span>
          <Icon icon={ICON.help} size={14} color="var(--ds-warning)" />
          <span style={content}><span style={hi}>clarify</span> {event.question}</span>
        </div>
      )
    case STREAM_EVENT_TYPE.done: {
      // An auto terminal `done` event (schema-derived completion, no _done tool)
      // may omit `output` (the final text was streamed as tokens). Surface the
      // OpenAI finish_reason in that case so the trace row isn't blank.
      const doneOutput = event.output
      const doneLabel = doneOutput
        ? (markdown ? <div style={mdWrap}><Markdown>{doneOutput}</Markdown></div>
                  : <span style={content}>{doneOutput}</span>)
        : <span style={{ ...content, opacity: 0.6 }}>{event.finish_reason ? `completed (${event.finish_reason})` : 'completed'}</span>
      return (
        <div style={{ ...row, background: 'var(--ds-trace-success-bg)', borderRadius: 4, padding: '4px 8px', margin: '2px 0' }}>
          <span style={muted}>{time}</span>
          <Icon icon={ICON.check} size={14} color="var(--ds-success)" />
          {doneLabel}
        </div>
      )
    }
    case STREAM_EVENT_TYPE.fail:
      return (
        <div style={{ ...row, background: 'var(--ds-trace-error-bg)', borderRadius: 4, padding: '4px 8px', margin: '2px 0' }}>
          <span style={muted}>{time}</span>
          <Icon icon={ICON.close} size={14} color="var(--ds-error)" />
          <span style={{ ...content, color: '#fca5a5' }}>{event.reason}</span>
        </div>
      )
    case STREAM_EVENT_TYPE.agentEvent: {
      const isSSEClientParse = isSSEClientParseEventType(event.eventType)
      if (isSSEClientParse) {
        return (
          <div
            style={{
              ...row,
              background: 'var(--ds-trace-warning-bg)',
              borderRadius: 4,
              padding: '4px 8px',
              margin: '2px 0',
              borderLeft: '2px solid var(--ds-trace-warning-border)',
            }}
          >
            <span style={muted}>{time}</span>
            <Icon icon={ICON.parseError} size={14} color="var(--ds-warning)" />
            <span style={{ ...content, color: '#fcd34d' }}>
              <span style={hi}>SSE parse</span> <span style={{ color: '#fde68a' }}>{event.eventType}</span>{' '}
              <span style={{ color: 'var(--ds-text-secondary)', wordBreak: 'break-all' }}>{event.message}</span>
            </span>
          </div>
        )
      }
      return (
        <div style={row}>
          <span style={muted}>{time}</span>
          <Icon icon={ICON.event} size={14} color="var(--ds-warning)" />
          <span style={content}>
            <span style={hi}>{event.eventType}</span> {event.message}
          </span>
        </div>
      )
    }
    case STREAM_EVENT_TYPE.placeholder:
      return (
        <div style={row}>
          <span style={muted}>{time}</span>
          <Icon icon={ICON.placeholder} size={14} color="var(--ds-text-muted)" />
          <span style={content}>{event.message}</span>
        </div>
      )
    case 'ragResult':
      return (
        <div style={row}>
          <span style={muted}>{time}</span>
          <Icon icon={ICON.knowledgebase} size={14} color="var(--ds-purple)" />
          <span style={content}>
            <span style={hi}>RAG search</span> {event.name} — query: "{event.query}" → {event.results} results in {event.collection}
          </span>
        </div>
      )
    case 'mcpDiscovery':
      return (
        <div style={row}>
          <span style={muted}>{time}</span>
          <Icon icon={ICON.mcpserver} size={14} color="var(--ds-accent)" />
          <span style={content}>
            <span style={hi}>MCP server</span> {event.server} — {event.tools} tools discovered
          </span>
        </div>
      )
    case 'guardrail':
      return (
        <div style={{ ...row, background: 'var(--ds-trace-warning-bg)', borderRadius: 4, padding: '4px 8px', margin: '2px 0' }}>
          <span style={muted}>{time}</span>
          <Icon icon={ICON.help} size={14} color="var(--ds-warning)" />
          <span style={{ ...content, color: '#fcd34d' }}>
            <span style={hi}>guardrail</span> {event.action}: {event.reason}
          </span>
        </div>
      )
    case 'providerFallback': {
      const exhausted = (event as any).exhausted
      return (
        <div style={{ ...row, background: exhausted ? 'var(--ds-trace-error-bg)' : 'var(--ds-trace-warning-bg)', borderRadius: 4, padding: '4px 8px', margin: '2px 0' }}>
          <span style={muted}>{time}</span>
          <Icon icon={ICON.shuffle} size={14} color={exhausted ? 'var(--ds-error)' : 'var(--ds-warning)'} />
          <span style={{ ...content, color: exhausted ? '#fca5a5' : '#fcd34d' }}>
            <span style={hi}>fallback</span> {event.from} → {event.to || '(searching)'} — {event.reason}
            {exhausted && <span style={{ color: '#fca5a5', fontWeight: 600 }}> — exhausted</span>}
          </span>
        </div>
      )
    }
    default: {
      const t =
        event && typeof event === 'object' && 'type' in event
          ? String((event as { type: unknown }).type)
          : '(no type)'
      return (
        <div style={{ ...row, background: 'var(--ds-trace-muted-bg)', borderRadius: 4, padding: '2px 0' }}>
          <span style={muted}>{time}</span>
          <Icon icon={ICON.help} size={14} color="var(--ds-text-muted)" />
          <span style={content}>
            <span style={hi}>unknown trace type</span> <span style={{ color: 'var(--ds-text-muted)' }}>{t}</span>{' '}
            {JSON.stringify(event)}
          </span>
        </div>
      )
    }
  }
}

const s: Record<string, React.CSSProperties> = {
  accordion: {
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: 8,
    display: 'flex',
    flexDirection: 'column',
    maxHeight: '60vh',
    boxShadow: 'var(--ds-card-shadow)',
  },
  header: {
    padding: '11px 16px',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    cursor: 'pointer',
    userSelect: 'none',
    background: 'none',
    border: 'none',
    color: 'inherit',
    font: 'inherit',
    textAlign: 'left',
  },
  title: {
    fontSize: 12,
    fontWeight: 600,
    color: 'var(--ds-text-secondary)',
    display: 'flex',
    alignItems: 'center',
    gap: 8,
  },
  badge: {
    fontSize: 10,
    padding: '1px 6px',
    borderRadius: 4,
    background: 'var(--ds-trace-muted-bg)',
    color: 'var(--ds-text-secondary)',
    fontVariantNumeric: 'tabular-nums',
  },
  backendChip: {
    fontSize: 10,
    padding: '1px 6px',
    borderRadius: 4,
    background: 'rgba(59,130,246,.1)',
    color: 'var(--ds-accent)',
    fontWeight: 600,
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace',
    flexShrink: 0,
    whiteSpace: 'nowrap' as const,
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    maxWidth: 180,
  },
  chevron: {
    fontSize: 11,
    color: 'var(--ds-text-secondary)',
    transitionProperty: 'transform',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'cubic-bezier(0.2, 0, 0, 1)',
    display: 'inline-block',
  },
  body: {
    padding: '14px 16px',
    borderTop: `1px solid var(--ds-border)`,
    flex: 1,
    overflowY: 'auto',
    minHeight: 0,
  },
  filterBar: {
    display: 'flex',
    gap: 4,
    marginBottom: 10,
    flexWrap: 'wrap' as const,
  },
  filterBtn: {
    fontSize: 10,
    padding: '2px 8px',
    borderRadius: 9999,
    border: 'none',
    background: 'rgba(148,163,184,.1)',
    color: 'var(--ds-text-secondary)',
    cursor: 'pointer',
    fontWeight: 500,
    transitionProperty: 'background-color, color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  filterBtnActive: {
    background: 'rgba(59,130,246,.15)',
    color: 'var(--ds-accent)',
  },
  empty: {
    color: 'var(--ds-text-muted)',
    fontSize: 12,
    fontStyle: 'italic',
    display: 'flex',
    alignItems: 'center',
    gap: 8,
    padding: '12px 0',
  },
  loadingDot: {
    width: 7,
    height: 7,
    borderRadius: '50%',
    background: 'var(--ds-accent)',
    animation: 'aoPulse 1.5s ease infinite',
    willChange: 'transform, opacity',
  },
  quipDot: {
    width: 5,
    height: 5,
    borderRadius: '50%',
    background: 'var(--ds-text-muted)',
    opacity: 0.5,
    flexShrink: 0,
  },
  childBadge: {
    color: 'var(--ds-text-muted)',
    fontSize: 10,
    background: 'var(--ds-trace-muted-bg)',
    padding: '1px 4px',
    borderRadius: 3,
    marginRight: 4,
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace',
    flexShrink: 0,
  },
}
