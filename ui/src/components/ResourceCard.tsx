/**
 * ResourceCard — displays a resource summary with total count and status breakdown.
 * Used on the home dashboard to provide system status at a glance.
 */
import React, { type CSSProperties } from 'react'
import { DESIGN, ds } from '../lib/designSystem'
import type { IconComponent } from '../lib/icons'
import { Icon } from '../lib/icons'

interface StatusEntry {
  label: string
  count: number
  color: string
}

interface Props {
  title: string
  icon: IconComponent
  total: number
  statuses: StatusEntry[]
  onClick: () => void
}

export function ResourceCard({ title, icon, total, statuses, onClick }: Props) {
  const hasStatusData = statuses.some(s => s.count > 0)

  return (
    <button
      type="button"
      style={s.card}
      onClick={onClick}
      onMouseEnter={(e) => {
        e.currentTarget.style.borderColor = 'var(--ds-accent)'
        e.currentTarget.style.transform = 'translateY(-2px)'
      }}
      onMouseLeave={(e) => {
        e.currentTarget.style.borderColor = 'var(--ds-border)'
        e.currentTarget.style.transform = 'none'
      }}
    >
      <div style={s.cardHeader}>
        <Icon icon={icon} size={20} ariaHidden={false} />
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
    </button>
  )
}

const s: Record<string, CSSProperties> = {
  card: {
    background: 'var(--ds-surface)',
    border: '1px solid var(--ds-border)',
    borderRadius: DESIGN.radii.lg,
    padding: 20,
    cursor: 'pointer',
    transitionProperty: 'border-color, transform, box-shadow',
    transitionDuration: '0.2s',
    transitionTimingFunction: 'ease',
    display: 'flex',
    flexDirection: 'column',
    gap: 12,
    textAlign: 'left',
    // Remove default button appearance
    font: 'inherit',
    color: 'inherit',
  },
  cardHeader: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
  },
  cardTitle: {
    fontSize: 14,
    fontWeight: 600,
    color: 'var(--ds-text-secondary)',
    textTransform: 'uppercase',
    letterSpacing: '0.06em',
  },
  cardTotal: {
    fontSize: 36,
    fontWeight: 700,
    color: 'var(--ds-text-primary)',
    lineHeight: 1,
    fontVariantNumeric: 'tabular-nums',
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
    color: 'var(--ds-text-muted)',
    flex: 1,
  },
  statusCount: {
    color: 'var(--ds-text-secondary)',
    fontWeight: 600,
    fontVariantNumeric: 'tabular-nums',
  },
}