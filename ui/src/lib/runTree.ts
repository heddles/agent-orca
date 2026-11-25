/** Groups sub-runs under the run that spawned them, keeping the given order. */

interface TreeRun {
  name: string
  namespace: string
  parentRunRef?: string
}

export interface RunNode<T> {
  run: T
  /** Every descendant in the list, however deep, oldest spawn level first. */
  children: T[]
}

/**
 * A run whose parent is not in the list (another page, or filtered out) stays a
 * top-level row, so grouping never hides a run.
 */
export function nestRuns<T extends TreeRun>(runs: T[]): RunNode<T>[] {
  const key = (ns: string, name: string) => `${ns}/${name}`
  const byKey = new Map(runs.map((r) => [key(r.namespace, r.name), r]))
  const rootOf = (run: T): T => {
    let at = run
    const seen = new Set<T>([run])
    while (at.parentRunRef) {
      const parent = byKey.get(key(at.namespace, at.parentRunRef))
      if (!parent || seen.has(parent)) break
      seen.add(parent)
      at = parent
    }
    return at
  }
  const nodes = new Map<T, RunNode<T>>()
  for (const run of runs) {
    if (rootOf(run) === run) nodes.set(run, { run, children: [] })
  }
  for (const run of runs) {
    const root = rootOf(run)
    if (root === run) continue
    const node = nodes.get(root)
    if (node) node.children.push(run)
    else nodes.set(run, { run, children: [] })
  }
  // Insertion order put orphans last; restore the caller's order.
  return runs.flatMap((r) => nodes.get(r) ?? [])
}
