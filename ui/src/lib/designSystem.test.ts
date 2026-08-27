import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { applyDesignSystem, DESIGN, CSS_VARS, applyTheme, getPreferredTheme, toggleTheme, THEME_KEY, type Theme } from './designSystem'

describe('applyDesignSystem', () => {
  beforeEach(() => {
    // Clean up any previous injection
    document.getElementById('agentorca-design-system')?.remove()
  })

  afterEach(() => {
    document.getElementById('agentorca-design-system')?.remove()
  })

  it('injects a style element with id agentorca-design-system', () => {
    applyDesignSystem()
    const el = document.getElementById('agentorca-design-system')
    expect(el).not.toBeNull()
    expect(el!.tagName).toBe('STYLE')
  })

  it('registers CSS custom properties on :root', () => {
    applyDesignSystem()
    const el = document.getElementById('agentorca-design-system')
    const css = el!.textContent!
    // Check that key CSS variables are declared
    expect(css).toContain('--ds-bg:')
    expect(css).toContain('--ds-surface:')
    expect(css).toContain('--ds-border:')
    expect(css).toContain('--ds-accent:')
    expect(css).toContain('--ds-text-primary:')
    expect(css).toContain('--ds-text-secondary:')
    expect(css).toContain('--ds-text-muted:')
  })

  it('injects font smoothing on html', () => {
    applyDesignSystem()
    const css = document.getElementById('agentorca-design-system')!.textContent!
    expect(css).toContain('-webkit-font-smoothing: antialiased')
    expect(css).toContain('-moz-osx-font-smoothing: grayscale')
  })

  it('injects prefers-reduced-motion media query', () => {
    applyDesignSystem()
    const css = document.getElementById('agentorca-design-system')!.textContent!
    expect(css).toContain('@media (prefers-reduced-motion: reduce)')
  })

  it('injects :focus-visible baseline rule', () => {
    applyDesignSystem()
    const css = document.getElementById('agentorca-design-system')!.textContent!
    expect(css).toContain(':focus-visible')
    expect(css).toContain('outline: 2px solid')
  })

  it('gates aoPulse keyframe behind prefers-reduced-motion', () => {
    applyDesignSystem()
    const css = document.getElementById('agentorca-design-system')!.textContent!
    expect(css).toContain('@media (prefers-reduced-motion: no-preference)')
    expect(css).toContain('@keyframes aoPulse')
  })

  it('injects tabular-nums on body', () => {
    applyDesignSystem()
    const css = document.getElementById('agentorca-design-system')!.textContent!
    expect(css).toContain('font-variant-numeric: tabular-nums')
  })

  it('is idempotent — calls do not duplicate the style element', () => {
    applyDesignSystem()
    applyDesignSystem()
    applyDesignSystem()
    const els = document.querySelectorAll('#agentorca-design-system')
    expect(els).toHaveLength(1)
  })
})

describe('DESIGN token object', () => {
  it('exposes color tokens', () => {
    expect(DESIGN.color.background).toBe('#0f172a')
    expect(DESIGN.color.surface).toBe('#1e293b')
    expect(DESIGN.color.accent).toBe('#3b82f6')
  })

  it('exposes a spacing scale', () => {
    expect(DESIGN.space.xs).toBe(4)
    expect(DESIGN.space.xl).toBe(24)
    expect(DESIGN.space.xxl).toBe(32)
  })

  it('exposes a type scale', () => {
    expect(DESIGN.font.size.xs).toBe(10)
    expect(DESIGN.font.size.body).toBe(13)
    expect(DESIGN.font.size.display).toBe(24)
  })

  it('exposes font weights', () => {
    expect(DESIGN.font.weight.normal).toBe(400)
    expect(DESIGN.font.weight.bold).toBe(700)
  })
})

describe('CSS_VARS mapping', () => {
  it('maps every color token to a CSS variable name', () => {
    expect(CSS_VARS.background).toBe('--ds-bg')
    expect(CSS_VARS.surface).toBe('--ds-surface')
    expect(CSS_VARS.accent).toBe('--ds-accent')
    expect(CSS_VARS.border).toBe('--ds-border')
  })
})

describe('theme system', () => {
  const originalMatchMedia = window.matchMedia
  const originalLS = window.localStorage

  beforeEach(() => {
    // jsdom doesn't always ship localStorage — provide a minimal mock
    const store: Record<string, string> = {}
    const mockLS = {
      getItem: (k: string) => store[k] ?? null,
      setItem: (k: string, v: string) => { store[k] = v },
      removeItem: (k: string) => { delete store[k] },
      clear: () => { for (const k in store) delete store[k] },
    }
    Object.defineProperty(window, 'localStorage', {
      value: mockLS,
      configurable: true,
      writable: true,
    })
    // matchMedia mock
    Object.defineProperty(window, 'matchMedia', {
      value: (q: string) => ({ matches: false, media: q, addListener: () => {}, removeListener: () => {}, addEventListener: () => {}, removeEventListener: () => {}, dispatchEvent: () => false }),
      configurable: true,
      writable: true,
    })
    document.documentElement.classList.remove('light', 'dark')
  })

  afterEach(() => {
    Object.defineProperty(window, 'localStorage', { value: originalLS, configurable: true, writable: true })
    Object.defineProperty(window, 'matchMedia', { value: originalMatchMedia, configurable: true, writable: true })
    document.documentElement.classList.remove('light', 'dark')
  })

  it('getPreferredTheme defaults to dark when no preference is saved', () => {
    expect(getPreferredTheme()).toBe('dark')
  })

  it('getPreferredTheme returns saved preference from localStorage', () => {
    localStorage.setItem(THEME_KEY, 'light')
    expect(getPreferredTheme()).toBe('light')
  })

  it('applyTheme toggles the light class on html', () => {
    applyTheme('light')
    expect(document.documentElement.classList.contains('light')).toBe(true)
    expect(document.documentElement.classList.contains('dark')).toBe(false)

    applyTheme('dark')
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    expect(document.documentElement.classList.contains('light')).toBe(false)
  })

  it('applyTheme persists to localStorage', () => {
    applyTheme('light')
    expect(localStorage.getItem(THEME_KEY)).toBe('light')
  })

  it('toggleTheme flips and persists the theme', () => {
    localStorage.setItem(THEME_KEY, 'dark')
    expect(toggleTheme()).toBe('light')
    expect(localStorage.getItem(THEME_KEY)).toBe('light')

    expect(toggleTheme()).toBe('dark')
    expect(localStorage.getItem(THEME_KEY)).toBe('dark')
  })
})
