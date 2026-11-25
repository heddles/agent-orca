/** "5m ago", "3h ago", "2d ago". Empty for a missing or unreadable time. */
export function timeAgo(iso: string | undefined, now = Date.now()): string {
  const at = iso ? new Date(iso).getTime() : NaN
  if (!Number.isFinite(at)) return ''
  const s = Math.max(0, Math.round((now - at) / 1000))
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 48 * 3600) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}

/** How long a run took, or has taken so far: "42s", "3m 5s", "1h 12m". */
export function duration(start: string | undefined, end: string | undefined, now = Date.now()): string {
  const from = start ? new Date(start).getTime() : NaN
  if (!Number.isFinite(from)) return ''
  const to = end ? new Date(end).getTime() : now
  const s = Math.max(0, Math.round((to - from) / 1000))
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`
}
