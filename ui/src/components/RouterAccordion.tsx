/** Collapsible model router decisions accordion. */
import { useState } from 'react'
import type { RoutingDecisionInfo } from '../api/sse'

interface Props {
  decisions: RoutingDecisionInfo[]
}

export function RouterAccordion({ decisions }: Props) {
  const [open, setOpen] = useState(false)

  // The controller pre-populates decisions with all configured providers
  // (reason starts with "configured provider"). The model-router then appends
  // the actual runtime choice as a separate entry with a real routing reason.
  const chosenIdx = (() => {
    for (let i = decisions.length - 1; i >= 0; i--) {
      if (!decisions[i].reason.startsWith('configured provider')) return i
    }
    return -1
  })()
  const chosen = chosenIdx >= 0 ? decisions[chosenIdx] : null

  return (
    <div style={s.accordion}>
      <div style={s.header} onClick={() => setOpen((o) => !o)}>
        <div style={s.title}>
          <span>🔀</span>
          Model Router
          {chosen ? (
            <span style={s.chosenPill}>{chosen.model}</span>
          ) : (
            <span style={s.badge}>{decisions.length} candidate{decisions.length !== 1 ? 's' : ''}</span>
          )}
        </div>
        <span style={{ ...s.chevron, transform: open ? 'rotate(90deg)' : undefined }}>▶</span>
      </div>
      {open && (
        <div style={s.body}>
          {decisions.length === 0 ? (
            <div style={s.empty}>No routing decisions recorded.</div>
          ) : (
            decisions.map((d, i) => {
              const isChosen = i === chosenIdx
              return (
                <div
                  key={i}
                  style={{
                    ...s.row,
                    ...(isChosen ? s.chosenRow : {}),
                    borderBottom: i < decisions.length - 1 ? '1px solid rgba(51,65,85,.5)' : 'none',
                  }}
                >
                  <div style={s.modelCol}>
                    <div style={s.modelName}>
                      {d.model}
                      {isChosen && <span style={s.selectedBadge}>selected</span>}
                    </div>
                    <div style={s.strategy}>{d.strategy} · {d.provider}</div>
                  </div>
                  <div style={s.confCol}>
                    <div style={s.confLabel}>{(parseFloat(d.confidence) * 100).toFixed(0)}% conf.</div>
                    <ConfBar value={parseFloat(d.confidence) || 0} />
                  </div>
                  <div style={s.reasonCol}>{d.reason}</div>
                </div>
              )
            })
          )}
        </div>
      )}
    </div>
  )
}

function ConfBar({ value }: { value: number }) {
  const pct = Math.round(value * 100)
  const fill = pct >= 80 ? '#22c55e' : pct >= 60 ? '#f59e0b' : '#ef4444'
  return (
    <div style={s.barOuter}>
      <div style={{ ...s.barInner, width: `${pct}%`, background: fill }} />
    </div>
  )
}

const s: Record<string, React.CSSProperties> = {
  accordion: {
    background: '#1e293b',
    border: '1px solid #334155',
    borderRadius: 8,
    display: 'flex',
    flexDirection: 'column',
    maxHeight: '50vh',
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
  row: {
    display: 'grid',
    gridTemplateColumns: '1fr 100px 1fr',
    gap: 12,
    alignItems: 'center',
    padding: '8px 0',
  },
  modelCol: { display: 'flex', flexDirection: 'column', gap: 2 },
  modelName: { fontSize: 12, fontWeight: 600, color: '#f1f5f9' },
  strategy: { fontSize: 11, color: '#94a3b8' },
  confCol: { display: 'flex', flexDirection: 'column', gap: 4 },
  confLabel: { fontSize: 10, color: '#94a3b8' },
  barOuter: {
    height: 4,
    background: 'rgba(255,255,255,.08)',
    borderRadius: 2,
    overflow: 'hidden',
  },
  barInner: {
    height: '100%',
    borderRadius: 2,
    transition: 'width 0.3s',
  },
  reasonCol: { fontSize: 11, color: '#94a3b8', fontStyle: 'italic' },
  chosenPill: {
    fontSize: 10,
    padding: '2px 8px',
    borderRadius: 4,
    background: 'rgba(34,197,94,.15)',
    color: '#4ade80',
    fontWeight: 600,
    fontFamily: 'monospace',
    maxWidth: 260,
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap' as const,
  },
  chosenRow: {
    background: 'rgba(34,197,94,.06)',
    borderRadius: 4,
    padding: '6px 8px',
    margin: '0 -8px',
  },
  selectedBadge: {
    marginLeft: 8,
    fontSize: 9,
    padding: '1px 5px',
    borderRadius: 3,
    background: 'rgba(34,197,94,.2)',
    color: '#4ade80',
    fontWeight: 700,
    textTransform: 'uppercase' as const,
    letterSpacing: '0.06em',
    verticalAlign: 'middle',
  },
}
