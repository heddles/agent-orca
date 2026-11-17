/** A compact line diff (longest common subsequence) for showing what changed between two texts. */

export type DiffKind = 'same' | 'add' | 'remove'
export interface DiffLine { kind: DiffKind; text: string }

/** Lines of `a` and `b` in order, each marked as kept, removed from `a`, or added in `b`. */
export function diffLines(a: string, b: string): DiffLine[] {
  const x = a === '' ? [] : a.replace(/\n$/, '').split('\n')
  const y = b === '' ? [] : b.replace(/\n$/, '').split('\n')
  // Trim the shared head and tail so the table covers only the middle.
  let head = 0
  while (head < x.length && head < y.length && x[head] === y[head]) head++
  let tail = 0
  while (tail < x.length - head && tail < y.length - head && x[x.length - 1 - tail] === y[y.length - 1 - tail]) tail++
  const mx = x.slice(head, x.length - tail)
  const my = y.slice(head, y.length - tail)
  const n = mx.length
  const m = my.length
  const t: number[][] = Array.from({ length: n + 1 }, () => new Array<number>(m + 1).fill(0))
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      t[i][j] = mx[i] === my[j] ? t[i + 1][j + 1] + 1 : Math.max(t[i + 1][j], t[i][j + 1])
    }
  }
  const out: DiffLine[] = x.slice(0, head).map((text) => ({ kind: 'same' as const, text }))
  let i = 0
  let j = 0
  while (i < n && j < m) {
    if (mx[i] === my[j]) { out.push({ kind: 'same', text: mx[i] }); i++; j++ }
    else if (t[i + 1][j] >= t[i][j + 1]) { out.push({ kind: 'remove', text: mx[i++] }) }
    else { out.push({ kind: 'add', text: my[j++] }) }
  }
  while (i < n) out.push({ kind: 'remove', text: mx[i++] })
  while (j < m) out.push({ kind: 'add', text: my[j++] })
  for (const text of x.slice(x.length - tail)) out.push({ kind: 'same', text })
  return out
}
