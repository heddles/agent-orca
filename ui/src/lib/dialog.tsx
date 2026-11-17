/** A modal dialog: labelled, closes on Escape, moves focus in and returns it on close. */
import { useEffect, useId, useRef, type CSSProperties, type ReactNode } from 'react'
import { DESIGN, ds } from './designSystem'

interface DialogProps {
  title: string
  onClose: () => void
  children: ReactNode
  width?: number
}

export function Dialog({ title, onClose, children, width = 480 }: DialogProps) {
  const titleId = useId()
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null
    const first = ref.current?.querySelector<HTMLElement>('input, textarea, select, button:not([data-dialog-close])')
    ;(first ?? ref.current)?.focus()
    return () => previous?.focus?.()
  }, [])

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') { e.stopPropagation(); onClose(); return }
    if (e.key !== 'Tab' || !ref.current) return
    const items = Array.from(ref.current.querySelectorAll<HTMLElement>('input, textarea, select, button, a[href]')).filter((el) => !el.hasAttribute('disabled'))
    if (items.length === 0) return
    const firstEl = items[0]
    const lastEl = items[items.length - 1]
    if (e.shiftKey && document.activeElement === firstEl) { e.preventDefault(); lastEl.focus() }
    else if (!e.shiftKey && document.activeElement === lastEl) { e.preventDefault(); firstEl.focus() }
  }

  return (
    <div style={s.backdrop}>
      <div
        ref={ref}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        onKeyDown={onKeyDown}
        style={{ ...s.panel, width }}
      >
        <h2 id={titleId} style={s.title}>{title}</h2>
        {children}
      </div>
    </div>
  )
}

const s: Record<string, CSSProperties> = {
  backdrop: {
    position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.45)', display: 'flex', alignItems: 'center',
    justifyContent: 'center', zIndex: 1000, padding: 16,
  },
  panel: {
    maxWidth: '100%', maxHeight: '90vh', overflowY: 'auto', background: ds.surface, color: ds.textPrimary,
    border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.xl, padding: 20, display: 'flex',
    flexDirection: 'column', gap: 12, boxSizing: 'border-box',
  },
  title: { margin: 0, fontSize: 16, fontWeight: 600 },
}
