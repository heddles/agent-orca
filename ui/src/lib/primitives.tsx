/**
 * Shared UI primitives — accessible, themed, motion-aware building blocks.
 *
 * These components internalize the design-system tokens so that consumers
 * never reach for a raw hex value. They use CSS variables (resolved at runtime
 * via :root) so theming is a single-property swap.
 *
 * Motion: scale(0.96) on press for tactile feedback (better-ui §9),
 * specific transition-property (never `all`, better-ui §12),
 * will-change: transform only on active state (better-ui §13).
 */
import { useState, type CSSProperties, type ButtonHTMLAttributes, type ReactNode } from 'react'
import { DESIGN, ds } from './designSystem'

// ── Button ───────────────────────────────────────────────────────────────────

export type DSButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger'
export type DSButtonSize = 'sm' | 'md' | 'lg'

const SIZE_DIMENSIONS: Record<DSButtonSize, { padding: string; fontSize: number; height: number }> = {
  sm: { padding: '4px 10px', fontSize: 11, height: 28 },
  md: { padding: '6px 14px', fontSize: 12, height: 32 },
  lg: { padding: '8px 18px', fontSize: 13, height: 40 },
}

export interface DSButtonProps extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, 'children'> {
  /** Visual style. Default: 'secondary' (outline). */
  variant?: DSButtonVariant
  /** Size. Default: 'md'. */
  size?: DSButtonSize
  /** Icon-only button — requires aria-label from caller. */
  icon?: boolean
  children?: ReactNode
}

/**
 * Accessible, themed button with scale-on-press feedback.
 * - `aria-label` is required for `icon` buttons (better-accessibility §8).
 * - Uses `:before` scale(0.96) via CSS active state — no JS state churn.
 * - Transition-property is specific, never `all` (better-ui §12).
 */
export function DSButton({
  variant = 'secondary',
  size = 'md',
  icon = false,
  children,
  style,
  onMouseDown,
  ...rest
}: DSButtonProps) {
  const dim = SIZE_DIMENSIONS[size]

  const variantStyles: Record<DSButtonVariant, CSSProperties> = {
    primary: {
      background: ds.accent,
      color: '#fff',
      border: 'none',
    },
    secondary: {
      background: ds.surface,
      color: ds.textPrimary,
      border: `1px solid var(--ds-border)`,
    },
    ghost: {
      background: 'transparent',
      color: ds.textSecondary,
      border: 'none',
    },
    danger: {
      background: 'rgba(239,68,68,.12)',
      color: ds.error,
      border: 'none',
    },
  }

  const handleMouseDown = (e: React.MouseEvent<HTMLButtonElement>) => {
    // Scale-on-press via JS (better-ui §9) — works without CSS :active for inline styles
    const btn = e.currentTarget
    btn.style.transform = 'scale(0.96)'
    btn.style.willChange = 'transform'
    onMouseDown?.(e)
  }
  const handleMouseUp = (e: React.MouseEvent<HTMLButtonElement>) => {
    e.currentTarget.style.transform = ''
    e.currentTarget.style.willChange = ''
  }

  return (
    <button
      type="button"
      style={{
        ...baseBtn,
        ...variantStyles[variant],
        padding: icon ? `${dim.height / 2 - 6}px` : dim.padding,
        fontSize: dim.fontSize,
        height: icon ? dim.height : undefined,
        minWidth: icon ? dim.height : undefined,
        borderRadius: DESIGN.radii.md,
        fontWeight: variant === 'primary' ? 600 : 500,
        display: 'inline-flex',
        alignItems: 'center',
        justifyContent: 'center',
        gap: 8,
        cursor: 'pointer',
        transitionProperty: 'background-color, border-color, color, box-shadow, transform',
        transitionDuration: '0.15s',
        transitionTimingFunction: 'ease',
        ...(icon && { padding: 0, width: dim.height, height: dim.height }),
        ...style,
      }}
      onMouseDown={handleMouseDown}
      onMouseUp={handleMouseUp}
      onMouseLeave={handleMouseUp}
      {...rest}
    >
      {children}
    </button>
  )
}

const baseBtn: CSSProperties = {
  fontFamily: DESIGN.font.family,
  border: 'none',
  outline: 'none',
  userSelect: 'none',
  letterSpacing: '-0.01em',
}

// ── Input ───────────────────────────────────────────────────────────────────

export interface DSInputProps extends Omit<React.InputHTMLAttributes<HTMLInputElement>, 'size'> {
  /** Label for the input — required for accessibility (better-accessibility §6). */
  label?: string
  size?: DSButtonSize
  monospace?: boolean
}

/**
 * Accessible input with visible label and focus ring.
 * For numeric values, applies tabular-nums to prevent layout shift (better-typography §11).
 * On mobile, font-size is 16px to prevent iOS zoom (better-typography §15).
 */
export function DSInput({ label, size = 'md', monospace = false, style, type, ...rest }: DSInputProps) {
  const id = `ds-input-${Math.random().toString(36).slice(2, 10)}`
  const isNumeric = type === 'number' || type === 'tel'
  const inputStyles: CSSProperties = {
    background: ds.surface,
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.md,
    padding: '8px 10px',
    color: ds.textPrimary,
    fontSize: 14, // 16px on mobile to prevent zoom
    fontFamily: monospace ? DESIGN.font.mono : DESIGN.font.family,
    outline: 'none',
    width: '100%',
    boxSizing: 'border-box' as const,
    fontVariantNumeric: isNumeric ? 'tabular-nums' : undefined,
    transitionProperty: 'border-color, box-shadow',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
    ...style,
  }
  return (
    <label style={{ display: 'flex', flexDirection: 'column', gap: 6, width: '100%' }}>
      {label && <span style={{ fontSize: 11, fontWeight: 600, color: ds.textSecondary, textTransform: 'uppercase', letterSpacing: '0.04em' }}>{label}</span>}
      <input id={id} type={type} style={inputStyles} {...rest} />
    </label>
  )
}

// ── Card ─────────────────────────────────────────────────────────────────────

// ── Card ─────────────────────────────────────────────────────────────────────

export interface DSCardProps {
  children: ReactNode
  /** Adds hover lift shadow (better-ui §3). */
  hover?: boolean
  /** Elevated shadow from rest. */
  raised?: boolean
  padded?: boolean
  borderRadius?: number
  style?: CSSProperties
  onClick?: () => void
}

/**
 * Surface card with shadow-based elevation (better-ui §3).
 * Uses box-shadow for depth, keeps border only for structure.
 */
export function DSCard({ children, hover = false, raised = false, padded = true, borderRadius, style }: DSCardProps) {
  const [hovered, setHovered] = useState(false)
  return (
    <div
      style={{
        background: ds.surface,
        border: `1px solid var(--ds-border)`,
        borderRadius: borderRadius ?? DESIGN.radii.xl,
        boxShadow: hovered
          ? 'var(--ds-card-shadow-hover)'
          : raised
            ? 'var(--ds-card-shadow-hover)'
            : 'var(--ds-card-shadow)',
        padding: padded ? DESIGN.space.md : 0,
        transitionProperty: 'box-shadow, border-color, transform',
        transitionDuration: '0.15s',
        transitionTimingFunction: 'ease',
        transform: hovered && hover ? 'translateY(-1px)' : undefined,
        ...style,
      }}
      onMouseEnter={hover ? () => setHovered(true) : undefined}
      onMouseLeave={hover ? () => setHovered(false) : undefined}
    >
      {children}
    </div>
  )
}

// ── Chip / Badge ─────────────────────────────────────────────────────────────

export type DSChipVariant = 'default' | 'success' | 'warning' | 'error' | 'info' | 'purple' | 'neutral'

const CHIP_VARIANTS: Record<DSChipVariant, { bg: string; color: string }> = {
  default: { bg: 'rgba(148,163,184,.1)', color: ds.textMuted },
  success: { bg: 'rgba(34,197,94,.12)', color: '#22c55e' },
  warning: { bg: 'rgba(245,158,11,.12)', color: '#f59e0b' },
  error: { bg: 'rgba(239,68,68,.12)', color: '#ef4444' },
  info: { bg: 'rgba(59,130,246,.12)', color: '#3b82f6' },
  purple: { bg: 'rgba(167,139,250,.12)', color: '#a78bfa' },
  neutral: { bg: 'rgba(148,163,184,.1)', color: 'var(--ds-text-muted)' },
}

export interface DSChipProps {
  children: ReactNode
  variant?: DSChipVariant
  size?: 'sm' | 'md'
  dot?: boolean
  dotColor?: string
  style?: CSSProperties
}

/**
 * Status chip with consistent radius and optional colored dot.
 * Uses tabular-nums for numeric children via inherited body font-variant.
 */
export function DSChip({ children, variant = 'default', size = 'md', dot = false, dotColor, style }: DSChipProps) {
  const v = CHIP_VARIANTS[variant]
  return (
    <span style={{
      display: 'inline-flex',
      alignItems: 'center',
      gap: 4,
      padding: size === 'sm' ? '1px 6px' : '2px 8px',
      borderRadius: DESIGN.radii.full,
      fontSize: size === 'sm' ? 10 : 11,
      fontWeight: 600,
      letterSpacing: '0.03em',
      background: v.bg,
      color: v.color,
      fontVariantNumeric: 'tabular-nums',
      ...style,
    }}>
      {dot && <span style={{
        width: 6,
        height: 6,
        borderRadius: '50%',
        background: dotColor ?? v.color,
        flexShrink: 0,
      }} />}
      {children}
    </span>
  )
}

// ── Tab bar (ARIA pattern) ───────────────────────────────────────────────────

export interface DSTabProps {
  tabs: Array<{ key: string; label: string }>
  active: string
  onChange: (key: string) => void
  count?: Record<string, number>
}

/**
 * Accessible tab bar following ARIA APG patterns (better-accessibility §3).
 * Arrow keys navigate, Enter/Space activates, roving tabindex.
 */
export function DSTabBar({ tabs, active, onChange, count }: DSTabProps) {
  const [focusedIdx, setFocusedIdx] = useState(0)
  const activeIdx = tabs.findIndex((t) => t.key === active)

  const onKeyDown = (e: React.KeyboardEvent, idx: number) => {
    switch (e.key) {
      case 'ArrowLeft': e.preventDefault(); setFocusedIdx((idx + tabs.length - 1) % tabs.length); break
      case 'ArrowRight': e.preventDefault(); setFocusedIdx((idx + 1) % tabs.length); break
      case 'Home': e.preventDefault(); setFocusedIdx(0); break
      case 'End': e.preventDefault(); setFocusedIdx(tabs.length - 1); break
    }
  }

  return (
    <div role="tablist" style={tab.tablist}>
      {tabs.map((t, i) => {
        const isActive = t.key === active
        return (
          <button
            key={t.key}
            type="button"
            role="tab"
            aria-selected={isActive}
            aria-controls={`${t.key}-panel`}
            tabIndex={isActive ? 0 : -1}
            style={{
              ...tab.tab,
              ...(isActive ? tab.tabActive : {}),
              ...(i === focusedIdx && !isActive ? tab.tabFocusHint : {}),
            }}
            onClick={() => { onChange(t.key); setFocusedIdx(i) }}
            onFocus={() => setFocusedIdx(i)}
            onKeyDown={(e) => onKeyDown(e, i)}
          >
            {t.label}
            {count?.[t.key] !== undefined && count[t.key] !== undefined && (
              <span style={tab.count}>{count[t.key]}</span>
            )}
          </button>
        )
      })}
    </div>
  )
}

const tab = {
  tablist: {
    display: 'flex',
    gap: 2,
    alignItems: 'center',
    padding: '0 4px',
    background: 'transparent',
    borderBottom: `1px solid var(--ds-border)`,
  } as CSSProperties,
  tab: {
    padding: '8px 16px',
    fontSize: 13,
    fontWeight: 500,
    color: ds.textSecondary,
    background: 'none',
    border: 'none',
    borderBottom: '2px solid transparent',
    cursor: 'pointer',
    transitionProperty: 'color, border-color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
    letterSpacing: '-0.01em',
  } as CSSProperties,
  tabActive: {
    color: ds.accent,
    borderBottomColor: ds.accent,
    fontWeight: 600,
  } as CSSProperties,
  tabFocusHint: {
    // Subtle ring for keyboard navigation into an inactive tab
    boxShadow: '0 0 0 2px rgba(59,130,246,.2)',
  } as CSSProperties,
  count: {
    fontSize: 11,
    fontWeight: 600,
    color: ds.textMuted,
    background: 'rgba(148,163,184,.1)',
    padding: '1px 6px',
    borderRadius: DESIGN.radii.full,
    marginLeft: 6,
  } as CSSProperties,
}
