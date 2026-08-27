/**
 * Design system for agent-orc UI — legacy facade.
 *
 * The canonical design system now lives in `./designSystem.ts` which registers
 * CSS custom properties on `:root` and exposes the `DESIGN` token object.
 * This file re-exports from the new system for backward compatibility and
 * keeps the same public API (`theme`, `styles`) but backed by CSS variables
 * so that all components automatically get reduced-motion support, font
 * smoothing, and focus-visible styling registered globally.
 *
 * Prefer importing `DESIGN` / `ds` / `applyDesignSystem` from `./designSystem`
 * in new code, and the shared primitives from `./primitives`.
 */
import { DESIGN, ds } from './designSystem'

// Re-export phase colors
export { PHASE_COLOR, PHASE_STYLE } from './phaseColors'

export { DESIGN, ds }

/**
 * Color values now resolve via CSS custom properties (var(--ds-*)).
 * The actual values are registered by applyDesignSystem() on :root,
 * enabling runtime theme switching and reduced-motion/focus baselines.
 */
export const theme = {
  colors: {
    background: ds.bg,
    surface: ds.surface,
    surfaceHover: ds.surfaceHover,
    border: ds.border,
    borderLight: 'var(--ds-border-light)',
    text: {
      primary: ds.textPrimary,
      secondary: ds.textSecondary,
      muted: ds.textMuted,
    },
    accent: ds.accent,
    accentHover: '#2563eb',
    success: ds.success,
    successBg: 'rgba(34,197,94,.12)',
    warning: ds.warning,
    warningBg: 'rgba(245,158,11,.12)',
    error: ds.error,
    errorBg: 'rgba(239,68,68,.12)',
    info: ds.accent,
    infoBg: 'rgba(59,130,246,.12)',
    purple: ds.purple,
    purpleBg: 'rgba(167,139,250,.12)',
  },
  radii: DESIGN.radii,
  spacing: DESIGN.space,
  typography: {
    font: DESIGN.font.family,
    mono: DESIGN.font.mono,
    size: DESIGN.font.size,
    weight: DESIGN.font.weight,
  },
} as const

// ── Helper to create CSSProperties with theme values (backward-compatible API) ─

// Specific transition properties, never `all` (better-ui §12)
const TRANSITION_FAST = 'background-color, border-color, color, opacity, transform'
const TRANSITION_DURATION = '0.15s'
const TRANSITION_EASING = 'ease'

export const styles = {
  card: (depth: 'base' | 'raised' = 'base'): React.CSSProperties => ({
    background: ds.surface,
    border: `1px solid ${ds.border}`,
    borderRadius: DESIGN.radii.xl,
    boxShadow: depth === 'raised'
      ? 'var(--ds-card-shadow-hover)'
      : 'var(--ds-card-shadow)',
    transitionProperty: 'background-color, border-color, box-shadow',
    transitionDuration: TRANSITION_DURATION,
    transitionTimingFunction: TRANSITION_EASING,
  }),
  button: {
    base: {
      fontFamily: DESIGN.font.family,
      border: 'none',
      cursor: 'pointer',
      transitionProperty: TRANSITION_FAST,
      transitionDuration: TRANSITION_DURATION,
      transitionTimingFunction: TRANSITION_EASING,
    } as React.CSSProperties,
    primary: {
      background: ds.accent,
      color: '#fff',
      padding: '8px 16px',
      borderRadius: DESIGN.radii.md,
      fontSize: DESIGN.font.size.md,
      fontWeight: DESIGN.font.weight.semibold,
    } as React.CSSProperties,
    secondary: {
      background: 'transparent',
      color: ds.textSecondary,
      padding: '6px 12px',
      borderRadius: DESIGN.radii.md,
      fontSize: DESIGN.font.size.sm,
    } as React.CSSProperties,
    ghost: {
      background: 'transparent',
      color: ds.textMuted,
      padding: 0,
      border: 'none',
      fontSize: DESIGN.font.size.md,
    } as React.CSSProperties,
  },
  input: {
    base: {
      background: ds.surface,
      border: `1px solid ${ds.border}`,
      borderRadius: DESIGN.radii.md,
      padding: '8px 10px',
      color: ds.textPrimary,
      fontSize: DESIGN.font.size.md,
      fontFamily: DESIGN.font.family,
      outline: 'none',
      width: '100%',
      boxSizing: 'border-box' as const,
      transitionProperty: 'border-color, box-shadow',
      transitionDuration: TRANSITION_DURATION,
      transitionTimingFunction: TRANSITION_EASING,
    } as React.CSSProperties,
    focus: {
      borderColor: ds.accent,
      boxShadow: '0 0 0 2px rgba(59,130,246,0.2)',
    } as React.CSSProperties,
  },
  badge: (variant: 'default' | 'success' | 'warning' | 'error' | 'info' | 'purple' = 'default'): React.CSSProperties => {
    const variants = {
      default: { bg: 'rgba(148,163,184,.1)', color: ds.textMuted },
      success: { bg: theme.colors.successBg, color: ds.success },
      warning: { bg: theme.colors.warningBg, color: ds.warning },
      error: { bg: theme.colors.errorBg, color: ds.error },
      info: { bg: theme.colors.infoBg, color: ds.accent },
      purple: { bg: theme.colors.purpleBg, color: ds.purple },
    }
    return {
      display: 'inline-flex',
      alignItems: 'center',
      gap: 4,
      padding: '2px 8px',
      borderRadius: DESIGN.radii.full,
      fontSize: DESIGN.font.size.sm,
      fontWeight: DESIGN.font.weight.medium,
      fontVariantNumeric: 'tabular-nums',
      background: variants[variant].bg,
      color: variants[variant].color,
    }
  },
  chip: (selected = false): React.CSSProperties => ({
    display: 'inline-flex',
    alignItems: 'center',
    gap: 4,
    padding: '2px 8px',
    borderRadius: DESIGN.radii.md,
    fontSize: DESIGN.font.size.sm,
    fontWeight: DESIGN.font.weight.medium,
    fontVariantNumeric: 'tabular-nums',
    background: selected ? 'rgba(59,130,246,.15)' : 'rgba(59,130,246,.1)',
    color: selected ? ds.accent : '#93c5fd',
    border: selected ? `1px solid ${ds.accent}40` : undefined,
    cursor: 'pointer',
    transitionProperty: 'background-color, color, border-color',
    transitionDuration: TRANSITION_DURATION,
    transitionTimingFunction: TRANSITION_EASING,
  }),
}
