/**
 * The trace to show for a run. A run's live stream lasts about a day and can be
 * lost sooner, while the run is still listed; what then comes back is only what
 * can be rebuilt from the run itself. The archived run keeps the full trace.
 */
import { useEffect, useState } from 'react'
import { getRunHistoryDetail, type TraceEntry } from '../api/sse'

/** Events rebuilt from the run object: alone, they are not a record of what the agent did. */
const REBUILT = new Set(['modelSelected', 'routing', 'finalOutput', 'done', 'error', 'fail', 'clarify'])

const hasActivity = (entries: TraceEntry[]) => entries.some((e) => !REBUILT.has(e.event.type))

export function useRunTrace(
  namespace: string, runName: string, live: TraceEntry[], wanted: boolean,
): { entries: TraceEntry[]; lost: boolean } {
  const key = `${namespace}/${runName}`
  const [archive, setArchive] = useState<{ key: string; entries: TraceEntry[] } | null>(null)
  const needed = wanted && !hasActivity(live)

  useEffect(() => {
    if (!needed) return
    let current = true
    getRunHistoryDetail(namespace, runName)
      .then((d) => { if (current) setArchive({ key, entries: d.traceEntries ?? [] }) })
      .catch(() => { if (current) setArchive({ key, entries: [] }) })
    return () => { current = false }
  }, [needed, key, namespace, runName])

  if (!needed || archive?.key !== key) return { entries: live, lost: false }
  return hasActivity(archive.entries) ? { entries: archive.entries, lost: false } : { entries: live, lost: true }
}
