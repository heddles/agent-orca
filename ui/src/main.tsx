import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router'
import { AppRoutes } from './routes'
import { NamespaceProvider } from './lib/namespace'
import { applyDesignSystem, applyTheme, getPreferredTheme } from './lib/designSystem'

// Register the design system (CSS variables, font smoothing, reduced-motion,
// focus-visible baseline, aoPulse keyframe) before rendering.
applyDesignSystem()
// Apply the saved or system-default theme.
applyTheme(getPreferredTheme())

// Inject global markdown rendering styles (aoPulse keyframe now lives in the design system).
const styleEl = document.createElement('style')
styleEl.textContent = `
  .ao-md table { border-collapse: collapse; width: 100%; margin: 12px 0; font-size: 13px; }
  .ao-md th, .ao-md td { border: 1px solid var(--ds-border); padding: 6px 12px; text-align: left; vertical-align: top; }
  .ao-md th { background: var(--ds-surface); color: var(--ds-text-secondary); font-weight: 600; font-size: 11px; text-transform: uppercase; letter-spacing: 0.06em; }
  .ao-md tr:nth-child(even) td { background: var(--ds-md-table-bg); }
  .ao-md p { margin: 0 0 8px; }
  .ao-md p:last-child { margin-bottom: 0; }
  .ao-md h1, .ao-md h2, .ao-md h3 { color: var(--ds-text-primary); margin: 16px 0 8px; }
  .ao-md code { background: var(--ds-md-code-bg); border-radius: 3px; padding: 1px 5px; font-family: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace'; font-size: 12px; }
  .ao-md pre { background: var(--ds-md-pre-bg); border: 1px solid var(--ds-border); border-radius: 6px; padding: 12px; overflow-x: auto; }
  .ao-md pre code { background: none; padding: 0; }
  .ao-md ul, .ao-md ol { padding-left: 20px; margin: 8px 0; }
  .ao-md blockquote { border-left: 3px solid var(--ds-accent); padding-left: 12px; margin: 12px 0; color: var(--ds-text-secondary); }
  .ao-md img { max-width: 100%; border-radius: 6px; }
  /* Tabular numerals for stable markdown tables and inline values */
  .ao-md { font-variant-numeric: tabular-nums; }
`
document.head.appendChild(styleEl)

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <BrowserRouter>
      <NamespaceProvider>
        <AppRoutes />
      </NamespaceProvider>
    </BrowserRouter>
  </StrictMode>,
)
