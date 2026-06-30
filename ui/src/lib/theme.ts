/** Design system for agent-orc UI — single source of truth for colors, spacing, typography, and common styles. */

// Re-export phase colors
export { PHASE_COLOR } from './phaseColors'

export const theme = {
  colors: {
    background: '#0f172a',
    surface: '#1e293b',
    surfaceHover: '#253244',
    border: '#334155',
    borderLight: '#1e293b',
    text: {
      primary: '#f1f5f9',
      secondary: '#94a3b8',
      muted: '#64748b',
    },
    accent: '#3b82f6',
    accentHover: '#2563eb',
    success: '#22c55e',
    successBg: 'rgba(34,197,94,.12)',
    warning: '#f59e0b',
    warningBg: 'rgba(245,158,11,.12)',
    error: '#ef4444',
    errorBg: 'rgba(239,68,68,.12)',
    info: '#3b82f6',
    infoBg: 'rgba(59,130,246,.12)',
    purple: '#8b5cf6',
    purpleBg: 'rgba(167,139,250,.12)',
  },
  radii: {
    sm: 4,
    md: 6,
    lg: 8,
    xl: 12,
    full: 9999,
  },
  spacing: {
    xs: 4,
    sm: 8,
    md: 12,
    lg: 16,
    xl: 24,
    xxl: 32,
  },
  typography: {
    font: 'system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif',
    size: {
      xs: 10,
      sm: 11,
      md: 12,
      lg: 14,
      xl: 16,
      xxl: 20,
    },
    weight: {
      normal: 400,
      medium: 500,
      semibold: 600,
      bold: 700,
    },
  },
} as const

// Helper to create CSSProperties with theme values
export const styles = {
  card: (depth: 'base' | 'raised' = 'base'): React.CSSProperties => ({
    background: theme.colors.surface,
    border: `1px solid ${theme.colors.border}`,
    borderRadius: theme.radii.xl,
    boxShadow: depth === 'raised' ? '0 4px 16px rgba(0,0,0,0.2)' : undefined,
  }),
  button: {
    base: {
      fontFamily: theme.typography.font,
      border: 'none',
      cursor: 'pointer',
      transition: 'all 0.15s ease',
    },
    primary: {
      background: theme.colors.accent,
      color: '#fff',
      padding: '8px 16px',
      borderRadius: theme.radii.md,
      fontSize: theme.typography.size.md,
      fontWeight: theme.typography.weight.semibold,
    },
    secondary: {
      background: 'transparent',
      color: theme.colors.text.secondary,
      padding: '6px 12px',
      borderRadius: theme.radii.md,
      fontSize: theme.typography.size.sm,
    },
    ghost: {
      background: 'transparent',
      color: theme.colors.text.muted,
      padding: 0,
      border: 'none',
      fontSize: theme.typography.size.md,
    },
  },
  input: {
    base: {
      background: theme.colors.surface,
      border: `1px solid ${theme.colors.border}`,
      borderRadius: theme.radii.md,
      padding: '8px 10px',
      color: theme.colors.text.primary,
      fontSize: theme.typography.size.md,
      fontFamily: theme.typography.font,
      outline: 'none',
      width: '100%',
      boxSizing: 'border-box' as const,
    },
    focus: {
      borderColor: theme.colors.accent,
      boxShadow: `0 0 0 2px rgba(59,130,246,0.2)`,
    },
  },
  badge: (variant: 'default' | 'success' | 'warning' | 'error' | 'info' | 'purple' = 'default'): React.CSSProperties => {
    const variants = {
      default: { bg: 'rgba(148,163,184,.1)', color: theme.colors.text.muted },
      success: { bg: theme.colors.successBg, color: theme.colors.success },
      warning: { bg: theme.colors.warningBg, color: theme.colors.warning },
      error: { bg: theme.colors.errorBg, color: theme.colors.error },
      info: { bg: theme.colors.infoBg, color: theme.colors.info },
      purple: { bg: theme.colors.purpleBg, color: theme.colors.purple },
    }
    return {
      display: 'inline-flex',
      alignItems: 'center',
      gap: 4,
      padding: '2px 8px',
      borderRadius: theme.radii.full,
      fontSize: theme.typography.size.sm,
      fontWeight: theme.typography.weight.medium,
      background: variants[variant].bg,
      color: variants[variant].color,
    }
  },
  chip: (selected = false): React.CSSProperties => ({
    display: 'inline-flex',
    alignItems: 'center',
    gap: 4,
    padding: '2px 8px',
    borderRadius: theme.radii.md,
    fontSize: theme.typography.size.sm,
    fontWeight: theme.typography.weight.medium,
    background: selected ? `${theme.colors.accent}20` : 'rgba(59,130,246,.1)',
    color: selected ? theme.colors.accent : '#93c5fd',
    border: selected ? `1px solid ${theme.colors.accent}40` : undefined,
    cursor: 'pointer',
  }),
}