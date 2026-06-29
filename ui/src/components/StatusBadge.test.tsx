import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { StatusBadge } from './StatusBadge'

describe('StatusBadge', () => {
  it('renders the phase label for known phases', () => {
    render(<StatusBadge phase="Succeeded" />)
    expect(screen.getByText('✓ Succeeded')).toBeInTheDocument()
  })

  it('falls back to raw phase string for unknown phases', () => {
    render(<StatusBadge phase="CustomPhase" />)
    expect(screen.getByText('CustomPhase')).toBeInTheDocument()
  })

  it('shows a pulse dot for Running phase', () => {
    const { container } = render(<StatusBadge phase="Running" />)
    // The outer span contains the pulse dot span + text
    const spans = container.querySelectorAll('span')
    // Pulse dot is the inner span with animation
    const pulseDot = Array.from(spans).find(
      (el) => el.style.animation && el.style.animation.includes('Pulse'),
    )
    expect(pulseDot).toBeDefined()
  })

  it('does not show a pulse dot for Succeeded phase', () => {
    const { container } = render(<StatusBadge phase="Succeeded" />)
    const spans = container.querySelectorAll('span')
    const pulseDot = Array.from(spans).find(
      (el) => el.style.animation && el.style.animation.includes('Pulse'),
    )
    expect(pulseDot).toBeUndefined()
  })

  it('suppresses pulse when pulse=false even for Running', () => {
    const { container } = render(<StatusBadge phase="Running" pulse={false} />)
    const spans = container.querySelectorAll('span')
    const pulseDot = Array.from(spans).find(
      (el) => el.style.animation && el.style.animation.includes('Pulse'),
    )
    expect(pulseDot).toBeUndefined()
  })
})
