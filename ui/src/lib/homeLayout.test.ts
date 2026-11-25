import { beforeEach, describe, expect, it } from 'vitest'
import {
  addWidget, DEFAULT_LAYOUT, loadLayout, moveWidget, removeWidget, resetLayout, saveLayout, toggleWide,
  type Placement, type WidgetId,
} from './homeLayout'

const ids = (l: Placement[]) => l.map((p) => p.id)

beforeEach(() => localStorage.clear())

describe('home layout', () => {
  it('is the default until someone changes it, and again after a reset', () => {
    expect(loadLayout('ada')).toEqual(DEFAULT_LAYOUT)
    saveLayout('ada', removeWidget(DEFAULT_LAYOUT, 'ask'))
    expect(ids(loadLayout('ada'))).not.toContain('ask')
    resetLayout('ada')
    expect(loadLayout('ada')).toEqual(DEFAULT_LAYOUT)
  })

  it('is kept per user', () => {
    saveLayout('ada', removeWidget(DEFAULT_LAYOUT, 'ask'))
    expect(loadLayout('bo')).toEqual(DEFAULT_LAYOUT)
  })

  it('never lets what waits on a person be removed or lost', () => {
    expect(ids(removeWidget(DEFAULT_LAYOUT, 'needs-you'))).toContain('needs-you')
    localStorage.setItem('agentorca-home:v1:ada', JSON.stringify([{ id: 'reports', wide: false }]))
    expect(ids(loadLayout('ada'))).toEqual(['needs-you', 'reports'])
  })

  it('drops unknown and repeated widgets from saved data, and survives junk', () => {
    localStorage.setItem('agentorca-home:v1:ada', JSON.stringify([
      { id: 'needs-you', wide: true }, { id: 'gone', wide: true }, { id: 'ask', wide: 'yes' }, { id: 'ask', wide: true }, null,
    ]))
    expect(loadLayout('ada')).toEqual([{ id: 'needs-you', wide: true }, { id: 'ask', wide: false }])
    localStorage.setItem('agentorca-home:v1:ada', '{not json')
    expect(loadLayout('ada')).toEqual(DEFAULT_LAYOUT)
  })

  it('adds a widget once, at the end', () => {
    const one = addWidget(DEFAULT_LAYOUT, 'agents')
    expect(ids(one)[one.length - 1]).toBe('agents')
    expect(addWidget(one, 'agents')).toBe(one)
  })

  it('moves past widgets this user cannot see', () => {
    const layout: Placement[] = [{ id: 'needs-you', wide: true }, { id: 'agents', wide: false }, { id: 'ask', wide: false }]
    const visible: WidgetId[] = ['needs-you', 'ask']
    expect(ids(moveWidget(layout, 'ask', -1, visible))).toEqual(['ask', 'agents', 'needs-you'])
    expect(moveWidget(layout, 'needs-you', -1, visible)).toBe(layout)
  })

  it('switches a widget between half and full width', () => {
    expect(toggleWide(DEFAULT_LAYOUT, 'ask').find((p) => p.id === 'ask')!.wide).toBe(false)
  })
})
