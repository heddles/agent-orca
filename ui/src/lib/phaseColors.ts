/** Single source of truth for phase colors used across the UI. */

/** Raw hex color per phase — for dot indicators and inline backgrounds. */
export const PHASE_COLOR: Record<string, string> = {
  Pending:         '#f59e0b',
  Running:         '#3b82f6',
  Creating:        '#3b82f6',
  Succeeded:       '#22c55e',
  Failed:          '#ef4444',
  Cancelled:       '#ef4444',
  HandedOff:       '#8b5cf6',
  Skipped:         '#a78bfa',
  Paused:          '#64748b',
  WaitingForInput: '#f59e0b',
}

/** Full badge style (background tint + text color) per phase. */
export const PHASE_STYLE: Record<string, React.CSSProperties> = {
  Succeeded:       { background: 'rgba(34,197,94,.12)',   color: '#22c55e' },
  Running:         { background: 'rgba(59,130,246,.12)',  color: '#3b82f6' },
  Creating:        { background: 'rgba(59,130,246,.12)',  color: '#3b82f6' },
  Failed:          { background: 'rgba(239,68,68,.12)',   color: '#ef4444' },
  Cancelled:       { background: 'rgba(239,68,68,.12)',   color: '#ef4444' },
  Pending:         { background: 'rgba(148,163,184,.1)',  color: '#94a3b8' },
  Paused:          { background: 'rgba(148,163,184,.1)',  color: '#94a3b8' },
  Skipped:         { background: 'rgba(167,139,250,.12)', color: '#a78bfa' },
  HandedOff:       { background: 'rgba(167,139,250,.12)', color: '#a78bfa' },
  WaitingForInput: { background: 'rgba(245,158,11,.12)',  color: '#f59e0b' },
}
