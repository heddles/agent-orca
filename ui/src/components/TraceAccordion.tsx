/** Collapsible execution trace accordion. */
import { useState } from 'react'
import Markdown from 'react-markdown'
import { STREAM_EVENT_TYPE } from '../contracts/events'
import { isSSEClientParseEventType } from '../api/traceStream'
import type { TraceEntry, TraceEvent } from '../api/sse'
import { MCPAppFrame } from './MCPAppFrame'

interface Props {
  entries: TraceEntry[]
  streaming?: boolean
  markdown?: boolean
}

export function TraceAccordion({ entries, streaming, markdown }: Props) {
  const [open, setOpen] = useState(false)

  const label = streaming ? (
    <span style={{ color: '#3b82f6' }}>● streaming</span>
  ) : (
    <span style={s.badge}>{entries.length} events</span>
  )

  return (
    <div style={s.accordion}>
      <div style={s.header} onClick={() => setOpen((o) => !o)}>
        <div style={s.title}>
          <span>📋</span>
          {streaming ? 'Live Trace' : 'Execution Trace'}
          {label}
        </div>
        <span style={{ ...s.chevron, transform: open ? 'rotate(90deg)' : undefined }}>▶</span>
      </div>
      {open && (
        <div style={s.body}>
          {entries.length === 0 && (
            <div style={s.empty}>No trace events yet.</div>
          )}
          {entries.map((entry) => (
            <TraceRow key={entry.id} entry={entry} markdown={markdown ?? false} />
          ))}
        </div>
      )}
    </div>
  )
}

function TraceRow({ entry, markdown }: { entry: TraceEntry; markdown: boolean }) {
  const { event, ts } = entry
  const time = ts.slice(11, 23)
  const childLabel = entry.childRunName
    ? <span style={s.childBadge}>{entry.childRunName.split('-').slice(-2).join('-')}</span>
    : null

  const iconStyle = (color: string): React.CSSProperties => ({
    width: 20,
    flexShrink: 0,
    color,
    textAlign: 'center',
    paddingTop: 1,
    fontFamily: 'monospace',
  })

  const row: React.CSSProperties = {
    display: 'flex',
    gap: 10,
    marginBottom: 6,
    alignItems: 'flex-start',
    fontSize: 12,
    fontFamily: 'monospace',
    lineHeight: 1.5,
  }

  const muted: React.CSSProperties = { color: '#94a3b8', flexShrink: 0, fontSize: 11 }
  const hi: React.CSSProperties = { color: '#f1f5f9' }
  const content: React.CSSProperties = { color: '#94a3b8', flex: 1, lineHeight: 1.5, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }
  const mdWrap: React.CSSProperties = { color: '#94a3b8', fontFamily: 'system-ui,sans-serif', fontSize: 12, lineHeight: 1.5, flex: 1 }

  switch (event.type) {
    case 'thought':
      return (
        <div style={row}>
          <span style={muted}>{time}</span>
          <span style={iconStyle('#3b82f6')}>◈</span>
          <span style={content}><span style={hi}>thought</span> {event.content}</span>
        </div>
      )
    case STREAM_EVENT_TYPE.toolCall:
      return (
        <div style={row}>
          <span style={muted}>{time}</span>
          <span style={iconStyle('#f59e0b')}>⚙</span>
          <span style={content}>{childLabel}<span style={hi}>tool →</span> {event.name} <span style={{ color: '#64748b' }}>{event.arguments}</span></span>
        </div>
      )
    case STREAM_EVENT_TYPE.toolResult:
      return (
        <div style={{ ...row, flexDirection: 'column' }}>
          <div style={{ ...row, marginBottom: event.appUrl ? 0 : undefined }}>
            <span style={muted}>{time}</span>
            <span style={iconStyle('#10b981')}>◉</span>
            {markdown ? (
              <div style={mdWrap}>{childLabel}<span style={hi}>result</span> {event.name}: <Markdown>{event.result}</Markdown></div>
            ) : (
              <span style={content}>{childLabel}<span style={hi}>result</span> {event.name}: {event.result}</span>
            )}
          </div>
          {event.appUrl && <MCPAppFrame appUrl={event.appUrl} toolName={event.name} toolArgs={event.toolArgs} toolResult={event.toolResult} />}
        </div>
      )
    case STREAM_EVENT_TYPE.modelSelected:
      return (
        <div style={row}>
          <span style={muted}>{time}</span>
          <span style={iconStyle('#a78bfa')}>⇝</span>
          <span style={content}><span style={hi}>routed</span> {event.model} — {event.reason} (confidence: {(event.confidence * 100).toFixed(0)}%)</span>
        </div>
      )
    case STREAM_EVENT_TYPE.finalOutput:
      return (
        <div style={{ ...row, background: '#0f2a1e', borderRadius: 4, padding: '4px 8px', margin: '2px 0' }}>
          <span style={muted}>{time}</span>
          <span style={iconStyle('#22c55e')}>◉</span>
          {markdown ? (
            <div style={mdWrap}><Markdown>{event.output}</Markdown></div>
          ) : (
            <span style={content}>{event.output}</span>
          )}
        </div>
      )
    case STREAM_EVENT_TYPE.error:
      return (
        <div style={{ ...row, background: '#2a0f0f', borderRadius: 4, padding: '4px 8px', margin: '2px 0' }}>
          <span style={muted}>{time}</span>
          <span style={iconStyle('#ef4444')}>✕</span>
          <span style={{ ...content, color: '#fca5a5' }}>{event.message}</span>
        </div>
      )
    case STREAM_EVENT_TYPE.clarify:
      return (
        <div style={{ ...row, background: '#1e1b2e', borderRadius: 4, padding: '4px 8px', margin: '2px 0' }}>
          <span style={muted}>{time}</span>
          <span style={iconStyle('#c084fc')}>?</span>
          <span style={content}><span style={hi}>clarify</span> {event.question}</span>
        </div>
      )
    case STREAM_EVENT_TYPE.done:
      return (
        <div style={{ ...row, background: '#0f2a1e', borderRadius: 4, padding: '4px 8px', margin: '2px 0' }}>
          <span style={muted}>{time}</span>
          <span style={iconStyle('#22c55e')}>✓</span>
          {markdown ? (
            <div style={mdWrap}><Markdown>{event.output}</Markdown></div>
          ) : (
            <span style={content}>{event.output}</span>
          )}
        </div>
      )
    case STREAM_EVENT_TYPE.fail:
      return (
        <div style={{ ...row, background: '#2a0f0f', borderRadius: 4, padding: '4px 8px', margin: '2px 0' }}>
          <span style={muted}>{time}</span>
          <span style={iconStyle('#ef4444')}>!</span>
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
              background: 'rgba(245,158,11,.07)',
              borderRadius: 4,
              padding: '4px 8px',
              margin: '2px 0',
              borderLeft: '2px solid rgba(245,158,11,.45)',
            }}
          >
            <span style={muted}>{time}</span>
            <span style={iconStyle('#f59e0b')}>⎋</span>
            <span style={{ ...content, color: '#fcd34d' }}>
              <span style={hi}>SSE parse</span> <span style={{ color: '#fde68a' }}>{event.eventType}</span>{' '}
              <span style={{ color: '#94a3b8', wordBreak: 'break-all' }}>{event.message}</span>
            </span>
          </div>
        )
      }
      return (
        <div style={row}>
          <span style={muted}>{time}</span>
          <span style={iconStyle('#eab308')}>◎</span>
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
          <span style={iconStyle('#64748b')}>…</span>
          <span style={content}>{event.message}</span>
        </div>
      )
    default: {
      const t =
        event && typeof event === 'object' && 'type' in event
          ? String((event as { type: unknown }).type)
          : '(no type)'
      return (
        <div style={{ ...row, background: 'rgba(148,163,184,.06)', borderRadius: 4, padding: '2px 0' }}>
          <span style={muted}>{time}</span>
          <span style={iconStyle('#64748b')}>?</span>
          <span style={content}>
            <span style={hi}>unknown trace type</span> <span style={{ color: '#64748b' }}>{t}</span>{' '}
            {JSON.stringify(event)}
          </span>
        </div>
      )
    }
  }
}

const s: Record<string, React.CSSProperties> = {
  accordion: {
    background: '#1e293b',
    border: '1px solid #334155',
    borderRadius: 8,
    display: 'flex',
    flexDirection: 'column',
    maxHeight: '60vh',
  },
  header: {
    padding: '11px 16px',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    cursor: 'pointer',
    userSelect: 'none',
  },
  title: {
    fontSize: 12,
    fontWeight: 600,
    color: '#94a3b8',
    display: 'flex',
    alignItems: 'center',
    gap: 8,
  },
  badge: {
    fontSize: 10,
    padding: '1px 6px',
    borderRadius: 4,
    background: 'rgba(148,163,184,.1)',
    color: '#94a3b8',
  },
  chevron: {
    fontSize: 11,
    color: '#94a3b8',
    transition: 'transform 0.2s',
  },
  body: {
    padding: '14px 16px',
    borderTop: '1px solid #334155',
    flex: 1,
    overflowY: 'auto' as const,
    minHeight: 0,
  },
  empty: {
    color: '#475569',
    fontSize: 12,
    fontStyle: 'italic',
  },
  childBadge: {
    color: '#475569',
    fontSize: 10,
    background: 'rgba(148,163,184,.08)',
    padding: '1px 4px',
    borderRadius: 3,
    marginRight: 4,
    fontFamily: 'monospace',
    flexShrink: 0,
  },
}
