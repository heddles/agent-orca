/**
 * ResourceCard — displays a resource summary with total count and status breakdown.
 * Used on the home dashboard to provide system status at a glance.
 */
import React, { type CSSProperties } from 'react'

interface StatusEntry {
  label: string
  count: number
  color: string
}

interface Props {
  title: string
  icon: string
  total: number
  statuses: StatusEntry[]
  onClick: () => void
}

export function ResourceCard({ title, icon, total, statuses, onClick }: Props) {
  const hasStatusData = statuses.some(s => s.count > 0)

  return (
    <div style={s.card} onClick={onClick} onMouseEnter={(e) => {
      e.currentTarget.style.borderColor = '#3b82f6'
      e.currentTarget.style.transform = 'translateY(-2px)'
    }} onMouseLeave={(e) => {
      e.currentTarget.style.borderColor = '#334155'
      e.currentTarget.style.transform = 'none'
    }}>
      <div style={s.cardHeader}>
        <span style={s.cardIcon}>{icon}</span>
        <span style={s.cardTitle}>{title}</span>
      </div>
      <div style={s.cardTotal}>{total}</div>
      {hasStatusData && (
        <div style={s.statusTable}>
          {statuses.map((status) => (
            <div key={status.label} style={s.tableRow}>
              <span style={{ ...s.statusDot, background: status.color }} />
              <span style={s.statusLabel}>{status.label}</span>
              <span style={s.statusCount}>{status.count}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

const s: Record<string, CSSProperties> = {
  card: {
    background: '#1e293b',
    border: '1px solid #334155',
    borderRadius: 12,
    padding: 20,
    cursor: 'pointer',
    transition: 'all 0.2s ease',
    display: 'flex',
    flexDirection: 'column',
    gap: 12,
  },
  cardHeader: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
  },
  cardIcon: {
    fontSize: 20,
  },
  cardTitle: {
    fontSize: 14,
    fontWeight: 600,
    color: '#94a3b8',
    textTransform: 'uppercase',
    letterSpacing: '0.06em',
  },
  cardTotal: {
    fontSize: 36,
    fontWeight: 700,
    color: '#f1f5f9',
    lineHeight: 1,
  },
  statusTable: {
    display: 'flex',
    flexDirection: 'column',
    gap: 6,
    marginTop: 4,
  },
  tableRow: {
    display: 'flex',
    alignItems: 'center',
    gap: 6,
    fontSize: 12,
  },
  statusDot: {
    width: 6,
    height: 6,
    borderRadius: '50%',
    flexShrink: 0,
  },
  statusLabel: {
    color: '#64748b',
    flex: 1,
  },
  statusCount: {
    color: '#94a3b8',
    fontWeight: 600,
  },
}