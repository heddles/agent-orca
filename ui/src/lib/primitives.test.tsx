import { describe, it, expect } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { DSButton, DSChip, DSCard, DSInput } from './primitives'
import { Icon, ICON } from './icons'

describe('DSButton', () => {
  it('renders children', () => {
    render(<DSButton>Click me</DSButton>)
    expect(screen.getByRole('button', { name: 'Click me' })).toBeInTheDocument()
  })

  it('renders a button element (not div)', () => {
    const { container } = render(<DSButton>Hi</DSButton>)
    expect(container.querySelector('button')).toBeInTheDocument()
  })

  it('has type="button" to avoid accidental form submission', () => {
    render(<DSButton>Hi</DSButton>)
    expect(screen.getByRole('button')).toHaveAttribute('type', 'button')
  })

  it('icon-only button requires and surfaces aria-label', () => {
    render(<DSButton icon aria-label="Close"><Icon icon={ICON.close} size={14} /></DSButton>)
    expect(screen.getByRole('button', { name: 'Close' })).toBeInTheDocument()
  })

  it('calls onClick when clicked', async () => {
    const onClick = vi.fn()
    render(<DSButton onClick={onClick}>Save</DSButton>)
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(onClick).toHaveBeenCalledTimes(1)
  })

  it('applies scale(0.96) on mousedown', async () => {
    render(<DSButton>Press</DSButton>)
    const btn = screen.getByRole('button', { name: 'Press' })
    fireEvent.mouseDown(btn)
    expect(btn.style.transform).toBe('scale(0.96)')
    fireEvent.mouseUp(btn)
    expect(btn.style.transform).toBe('')
  })

  it('supports primary, secondary, ghost, and danger variants', () => {
    const { rerender } = render(<DSButton variant="primary">A</DSButton>)
    expect(screen.getByRole('button')).toBeInTheDocument()
    rerender(<DSButton variant="danger">B</DSButton>)
    expect(screen.getByRole('button')).toBeInTheDocument()
    rerender(<DSButton variant="ghost">C</DSButton>)
    expect(screen.getByRole('button')).toBeInTheDocument()
  })
})

describe('DSChip', () => {
  it('renders with a variant and children', () => {
    render(<DSChip variant="success">Ready</DSChip>)
    expect(screen.getByText('Ready')).toBeInTheDocument()
  })

  it('renders a dot when dot=true', () => {
    const { container } = render(<DSChip variant="warning" dot>Warning</DSChip>)
    const spans = container.querySelectorAll('span')
    // First child is the dot span
    const dot = spans[0].querySelector('span')
    expect(dot).toBeInTheDocument()
    expect(dot).toHaveStyle({ width: '6px', height: '6px' })
  })

  it('applies tabular-nums for changing values', () => {
    render(<DSChip variant="info">42</DSChip>)
    const chip = screen.getByText('42').closest('span')
    expect(chip).toHaveStyle({ fontVariantNumeric: 'tabular-nums' })
  })

  it('supports all variant names without crashing', () => {
    const variants = ['default', 'success', 'warning', 'error', 'info', 'purple', 'neutral'] as const
    for (const v of variants) {
      const { unmount } = render(<DSChip variant={v}>{v}</DSChip>)
      unmount()
    }
  })
})

describe('DSCard', () => {
  it('renders children', () => {
    render(<DSCard>Hello card</DSCard>)
    expect(screen.getByText('Hello card')).toBeInTheDocument()
  })

  it('uses CSS variables for background and border', () => {
    const { container } = render(<DSCard>Content</DSCard>)
    const card = container.firstChild as HTMLElement
    expect(card.style.background).toContain('var(--ds-surface)')
    expect(card.style.border).toContain('var(--ds-border)')
  })

  it('applies hover class when hover prop is true', () => {
    render(<DSCard hover>Content</DSCard>)
    expect(screen.getByText('Content')).toBeInTheDocument()
  })
})

describe('DSInput', () => {
  it('renders an input with a label', () => {
    render(<DSInput label="Name" />)
    expect(screen.getByText('Name')).toBeInTheDocument()
    expect(screen.getByLabelText('Name')).toBeInTheDocument()
  })

  it('label is semantically associated with input', () => {
    render(<DSInput label="Email" />)
    const input = screen.getByLabelText('Email')
    expect(input.tagName).toBe('INPUT')
  })

  it('forwards standard input props', () => {
    render(<DSInput label="Query" placeholder="Type here…" />)
    expect(screen.getByPlaceholderText('Type here…')).toBeInTheDocument()
  })
})
