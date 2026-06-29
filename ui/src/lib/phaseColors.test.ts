import { describe, it, expect } from 'vitest'
import { PHASE_COLOR, PHASE_STYLE } from './phaseColors'

describe('PHASE_COLOR', () => {
  it('maps all expected phases to hex colors', () => {
    const expected = ['Pending', 'Running', 'Creating', 'Succeeded', 'Failed', 'Cancelled', 'HandedOff', 'Skipped', 'Paused', 'WaitingForInput']
    for (const phase of expected) {
      expect(PHASE_COLOR[phase]).toBeDefined()
      expect(PHASE_COLOR[phase]).toMatch(/^#[0-9a-f]{6}$/i)
    }
  })

  it('uses the same color for Running and Creating', () => {
    expect(PHASE_COLOR['Running']).toBe(PHASE_COLOR['Creating'])
  })
})

describe('PHASE_STYLE', () => {
  it('provides background and color for every phase', () => {
    for (const [phase, style] of Object.entries(PHASE_STYLE)) {
      expect(style.background, `${phase} missing background`).toBeDefined()
      expect(style.color, `${phase} missing color`).toBeDefined()
    }
  })

  it('covers the same phases as PHASE_COLOR', () => {
    expect(Object.keys(PHASE_STYLE).sort()).toEqual(Object.keys(PHASE_COLOR).sort())
  })
})
