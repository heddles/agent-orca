import { describe, expect, it } from 'vitest'
import { nestRuns } from './runTree'

const r = (name: string, parentRunRef?: string, namespace = 'a') => ({ name, namespace, parentRunRef })
const shape = (runs: ReturnType<typeof r>[]) => nestRuns(runs).map((n) => [n.run.name, n.children.map((c) => c.name)])

describe('nestRuns', () => {
  it('puts sub-runs, and theirs, under the top run', () => {
    expect(shape([r('grandchild', 'child'), r('parent'), r('other'), r('child', 'parent')]))
      .toEqual([['parent', ['grandchild', 'child']], ['other', []]])
  })

  it('keeps a run whose parent is not listed, in the given order', () => {
    expect(shape([r('orphan', 'gone'), r('solo')])).toEqual([['orphan', []], ['solo', []]])
  })

  it('does not match a parent in another namespace', () => {
    expect(shape([r('p', undefined, 'b'), r('c', 'p')])).toEqual([['p', []], ['c', []]])
  })

  it('hides no run when references form a cycle', () => {
    const names = nestRuns([r('x', 'y'), r('y', 'x')]).flatMap((n) => [n.run.name, ...n.children.map((c) => c.name)])
    expect(names.sort()).toEqual(['x', 'y'])
  })
})
