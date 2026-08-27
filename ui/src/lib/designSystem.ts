/**
 * Design system for agent-orc UI — single source of truth for colors, spacing,
 * typography, and global root styles.
 *
 * CSS custom properties are registered on `:root` via `applyDesignSystem()`.
 * Components reference them as `var(--ds-*)` so that a future light-mode or
 * custom-theme swap is a one-line change. The `DESIGN` object mirrors the same
 * values for JS-level access (sizes, weights) where a CSS variable cannot be
 * shorthanded into a `React.CSSProperties` object cleanly.
 */

// ── CSS custom property names ───────────────────────────────────────────────

export const CSS_VARS = {
  // Colors — semantic layer (better-colors §4)
  background: '--ds-bg',
  surface: '--ds-surface',
  surfaceHover: '--ds-surface-hover',
  border: '--ds-border',
  borderLight: '--ds-border-light',
  textPrimary: '--ds-text-primary',
  textSecondary: '--ds-text-secondary',
  textMuted: '--ds-text-muted',
  accent: '--ds-accent',
  success: '--ds-success',
  warning: '--ds-warning',
  error: '--ds-error',
  // Elevation shadows (better-ui §3 — shadows for depth, borders for structure)
  cardShadow: '--ds-card-shadow',
  cardShadowHover: '--ds-card-shadow-hover',
} as const

// ── Design token values (mirrors the CSS variables above) ────────────────────

export const DESIGN = {
  color: {
    background: '#0f172a',
    surface: '#1e293b',
    surfaceHover: '#253244',
    border: '#334155',
    borderLight: '#1e293b',
    textPrimary: '#f1f5f9',
    textSecondary: '#94a3b8',
    textMuted: '#647489',
    accent: '#3b82f6',
    success: '#22c55e',
    warning: '#f59e0b',
    error: '#ef4444',
    purple: '#8b5cf6',
  },
  radii: { sm: 4, md: 6, lg: 8, xl: 12, full: 9999 },
  space: { xs: 4, sm: 8, md: 12, lg: 16, xl: 24, xxl: 32 },
  font: {
    // Type scale (better-typography §5)
    size: {
      xs: 10,    // captions, trace timestamps
      sm: 11,    // subtitles, field labels
      md: 12,    // list items, secondary text
      body: 13,  // body copy
      lg: 14,    // chat messages, descriptions
      xl: 16,    // deployment name
      heading: 18, // run title
      title: 20,   // config detail title
      display: 24, // hero title (slightly toned down from 36)
      cost: 32,    // total cost display
    },
    weight: {
      normal: 400,
      medium: 500,
      semibold: 600,
      bold: 700,
    },
    family: 'system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif',
    mono: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", "Consolas", "monospace"',
  },
} as const

// ── Light mode palette (inverted + reduced vividness, better-colors §10) ─────

export const DESIGN_LIGHT = {
  color: {
    background: '#f8fafc',
    surface: '#ffffff',
    surfaceHover: '#f1f5f6',
    border: '#e2e8f0',
    borderLight: '#ffffff',
    textPrimary: '#0f172a',
    textSecondary: '#475569',
    textMuted: '#647489',
    accent: '#2563eb',
    success: '#16a34a',
    warning: '#d97706',
    error: '#dc2626',
    purple: '#7c3aed',
  },
} as const

// Re-export phase colors for convenience
export { PHASE_COLOR, PHASE_STYLE } from './phaseColors'

// ── Theme persistence ─────────────────────────────────────────────────────────

export const THEME_KEY = 'agentorc-theme'
export type Theme = 'dark' | 'light'

/** Returns the user's saved theme preference, or the system default. */
export function getPreferredTheme(): Theme {
  if (typeof window === 'undefined') return 'dark'
  let saved: string | null = null
  try { saved = localStorage.getItem(THEME_KEY) } catch { /* private browsing */ }
  if (saved === 'dark' || saved === 'light') return saved
  if (window.matchMedia) {
    return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark'
  }
  return 'dark'
}

/** Switch the active theme by toggling a class on <html>. */
export function applyTheme(theme: Theme): void {
  if (typeof document === 'undefined') return
  const root = document.documentElement
  // Suppress transitions during the theme flip so every element doesn't
  // animate its color/background/shadow in sequence (better-ui §11).
  const suppressor = document.createElement('style')
  suppressor.textContent = '*,*::before,*::after{transition:none !important}'
  document.head.appendChild(suppressor)
  root.classList.toggle('light', theme === 'light')
  root.classList.toggle('dark', theme === 'dark')
  try { localStorage.setItem(THEME_KEY, theme) } catch { /* private browsing */ }
  // Remove suppressor on next frame so subsequent interactions keep transitions.
  if (typeof requestAnimationFrame !== 'undefined') {
    requestAnimationFrame(() => {
      suppressor.remove()
      root.offsetHeight // force reflow
    })
  } else {
    suppressor.remove()
  }
}

/** Toggle between dark and light, persisting the choice. Returns the new theme. */
export function toggleTheme(): Theme {
  const next = getPreferredTheme() === 'dark' ? 'light' : 'dark'
  applyTheme(next)
  return next
}

// ── Global root styles ──────────────────────────────────────────────────────

/**
 * Injects the design-system CSS custom properties + global rules into <head>.
 * Call once before createRoot().
 *
 * Includes:
 *  - :root variable declarations (semantic color tokens, shadows)
 *  - font smoothing on html (better-typography §17)
 *  - prefers-reduced-motion blanket (better-accessibility §10)
 *  - :focus-visible baseline outline (better-accessibility §2)
 *  - aoPulse keyframe (gated on reduced-motion)
 *  - tabular-nums on body for stable metrics (better-typography §11)
 *  - body reset
 */
export function applyDesignSystem(): void {
  if (typeof document === 'undefined') return

  // Avoid double-injection
  if (document.getElementById('agentorc-design-system')) return

  const styleEl = document.createElement('style')
  styleEl.id = 'agentorc-design-system'
  styleEl.textContent = `
    :root {
      /* === Semantic color tokens === */
      --ds-bg: ${DESIGN.color.background};
      --ds-surface: ${DESIGN.color.surface};
      --ds-surface-hover: ${DESIGN.color.surfaceHover};
      --ds-border: ${DESIGN.color.border};
      --ds-border-light: ${DESIGN.color.borderLight};
      --ds-text-primary: ${DESIGN.color.textPrimary};
      --ds-text-secondary: ${DESIGN.color.textSecondary};
      --ds-text-muted: ${DESIGN.color.textMuted};
      --ds-accent: ${DESIGN.color.accent};
      --ds-success: ${DESIGN.color.success};
      --ds-warning: ${DESIGN.color.warning};
      --ds-error: ${DESIGN.color.error};
      --ds-purple: ${DESIGN.color.purple};

      /* === Elevation (shadows, not borders, for depth) === */
      --ds-card-shadow: 0 1px 3px rgba(0,0,0,0.10), 0 1px 2px rgba(0,0,0,0.06);
      --ds-card-shadow-hover: 0 4px 12px rgba(0,0,0,0.15);

      /* === Trace event backgrounds (theme-aware) === */
      --ds-trace-success-bg: rgba(34,197,94,.12);
      --ds-trace-error-bg: rgba(239,68,68,.12);
      --ds-trace-warning-bg: rgba(245,158,11,.12);
      --ds-trace-warning-border: rgba(245,158,11,.25);
      --ds-trace-clarify-bg: rgba(91,76,156,.08);
      --ds-trace-muted-bg: rgba(148,163,184,.06);

      /* === Markdown rendering (theme-aware) === */
      --ds-md-table-bg: rgba(51,65,85,.25);
      --ds-md-code-bg: rgba(51,65,85,.6);
      --ds-md-pre-bg: rgba(15,23,42,.8);

      /* === Semantic tinted backgrounds === */
      --ds-accent-bg: rgba(59,130,246,.1);
      --ds-accent-border: rgba(59,130,246,.25);
      --ds-warning-bg: rgba(245,158,11,.1);
      --ds-warning-border: rgba(245,158,11,.25);
      --ds-error-bg: rgba(239,68,68,.06);
      --ds-error-border: rgba(239,68,68,.25);
      --ds-muted-bg: rgba(148,163,184,.1);

      /* === Transition tokens (specific properties, never 'all') === */
      --ds-transition-fast: background-color, border-color, color, opacity;
      --ds-transition-fast-dur: 0.15s;
      --ds-ease-out: cubic-bezier(0.2, 0, 0, 1);
    }

    /* === Light mode overrides (better-colors §10) === */
    :root.light {
      --ds-bg: ${DESIGN_LIGHT.color.background};
      --ds-surface: ${DESIGN_LIGHT.color.surface};
      --ds-surface-hover: ${DESIGN_LIGHT.color.surfaceHover};
      --ds-border: ${DESIGN_LIGHT.color.border};
      --ds-border-light: ${DESIGN_LIGHT.color.borderLight};
      --ds-text-primary: ${DESIGN_LIGHT.color.textPrimary};
      --ds-text-secondary: ${DESIGN_LIGHT.color.textSecondary};
      --ds-text-muted: ${DESIGN_LIGHT.color.textMuted};
      --ds-accent: ${DESIGN_LIGHT.color.accent};
      --ds-success: ${DESIGN_LIGHT.color.success};
      --ds-warning: ${DESIGN_LIGHT.color.warning};
      --ds-error: ${DESIGN_LIGHT.color.error};
      --ds-purple: ${DESIGN_LIGHT.color.purple};
      --ds-card-shadow: 0 1px 3px rgba(0,0,0,0.06), 0 1px 2px rgba(0,0,0,0.04);
      --ds-card-shadow-hover: 0 4px 12px rgba(0,0,0,0.08);

      /* === Trace event backgrounds (light mode) === */
      --ds-trace-success-bg: rgba(22,163,74,.10);
      --ds-trace-error-bg: rgba(220,38,38,.10);
      --ds-trace-warning-bg: rgba(217,119,6,.10);
      --ds-trace-warning-border: rgba(217,119,6,.20);
      --ds-trace-clarify-bg: rgba(124,58,237,.08);
      --ds-trace-muted-bg: rgba(100,116,139,.06);

      /* === Semantic tinted backgrounds (light mode) === */
      --ds-accent-bg: rgba(37,99,235,.1);
      --ds-accent-border: rgba(37,99,235,.3);
      --ds-warning-bg: rgba(217,119,6,.1);
      --ds-warning-border: rgba(217,119,6,.2);
      --ds-error-bg: rgba(220,38,38,.06);
      --ds-error-border: rgba(220,38,38,.25);
      --ds-muted-bg: rgba(100,116,139,.1);

      /* === Markdown rendering (light mode) === */
      --ds-md-table-bg: rgba(226,232,240,.5);
      --ds-md-code-bg: rgba(15,23,42,.06);
      --ds-md-pre-bg: rgba(248,250,252,.8);

    /* Hover states for sidebar list buttons (gated on hover capability) */
    @media (hover: hover) {
      [data-sidebar] button:hover { background: var(--ds-surface-hover); }
      [data-sidebar] button:hover[aria-selected="true"],
      [data-sidebar] button:active { background: rgba(59,130,246,.1); }
    }

    html {
      -webkit-font-smoothing: antialiased;
      -moz-osx-font-smoothing: grayscale;
    }

    body {
      margin: 0;
      font-family: ${DESIGN.font.family};
      font-size: ${DESIGN.font.size.body}px;
      line-height: 1.6;
      font-variant-numeric: tabular-nums;
      background: var(--ds-bg);
      color: var(--ds-text-primary);
    }

    /* Input placeholders inherit muted text color */
    input::placeholder, textarea::placeholder {
      color: var(--ds-text-muted);
    }

    /* Respect reduced motion — disable all animations/transitions (better-accessibility §10) */
    @media (prefers-reduced-motion: reduce) {
      *, *::before, *::after {
        animation-duration: 0.01ms !important;
        animation-iteration-count: 1 !important;
        transition-duration: 0.01ms !important;
      }
    }

    /* Keyboard focus ring baseline (better-accessibility §2) */
    :focus-visible {
      outline: 2px solid var(--ds-accent);
      outline-offset: 2px;
    }

    /* Pulse animation — only when motion is allowed (better-accessibility §10) */
    @media (prefers-reduced-motion: no-preference) {
      @keyframes aoPulse {
        0%, 100% { opacity: 1; transform: scale(1); }
        50% { opacity: 0.5; transform: scale(0.8); }
      }
      @keyframes spin {
        from { transform: rotate(0deg); }
        to { transform: rotate(360deg); }
      }
    }

    /* Tab bar ARIA pattern baseline (better-accessibility §3) */
    [role="tab"][aria-selected="true"]  { font-weight: 600; }
    [role="tabpanel"] { outline: none; }
  `
  document.head.appendChild(styleEl)
}

// ── Helper functions (produce React.CSSProperties using CSS variables) ────────

const TRANSITION_PROPS = 'background-color, border-color, color, opacity'
const TRANSITION_FAST = `background-color, border-color, color, opacity, transform`

/** Standard interactive transition — specific properties, never "all" (better-ui §12). */
export const interactiveTransition = (includeTransform = false): string =>
  `transition-property: ${includeTransform ? TRANSITION_FAST : TRANSITION_PROPS}; ` +
  `transition-duration: 0.15s; transition-timing-function: ease;`

/** Card surface style with shadow-based elevation (better-ui §3). */
export function surfaceStyle(opts?: {
  hover?: boolean
  raised?: boolean
  padded?: boolean
  borderRadius?: number
}): React.CSSProperties {
  return {
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: opts?.borderRadius ?? DESIGN.radii.xl,
    boxShadow: opts?.raised ? 'var(--ds-card-shadow-hover)' : 'var(--ds-card-shadow)',
    padding: opts?.padded ? DESIGN.space.md : undefined,
    transitionProperty: 'background-color, border-color, box-shadow',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
    ...(opts?.hover && {
      ':hover': undefined, // CSSProperties doesn't support :hover; handled in onMouseEnter
    }),
  }
}

/** Focus-visible outline helper for elements that need a custom ring. */
export const focusRing = (color = 'var(--ds-accent)'): React.CSSProperties => ({
  outline: 'none',
  // We rely on the global :focus-visible rule; this is a no-op marker.
})

/** Hover-surface style mixin — call on onMouseEnter/Leave to swap background. */
export const hoverSurf = (el: HTMLElement) => {
  el.style.background = 'var(--ds-surface-hover)'
}
export const leaveSurf = (el: HTMLElement) => {
  el.style.background = 'var(--ds-surface)'
}

// ── Re-export theme for backward compatibility ────────────────────────────────

// Short aliases that components can use inline
export const ds = {
  bg: 'var(--ds-bg)',
  surface: 'var(--ds-surface)',
  surfaceHover: 'var(--ds-surface-hover)',
  border: 'var(--ds-border)',
  textPrimary: 'var(--ds-text-primary)',
  textSecondary: 'var(--ds-text-secondary)',
  textMuted: 'var(--ds-text-muted)',
  accent: 'var(--ds-accent)',
  success: 'var(--ds-success)',
  warning: 'var(--ds-warning)',
  error: 'var(--ds-error)',
  purple: 'var(--ds-purple)',
}
