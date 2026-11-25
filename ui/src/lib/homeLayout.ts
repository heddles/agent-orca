/**
 * Which widgets a person has on Home, in what order and how wide. Kept in this
 * browser, per signed-in user; nothing is sent to the server. Only a changed
 * layout is stored, so people who never customize get later defaults.
 */

export const WIDGET_IDS = ['needs-you', 'reports', 'problems', 'ask', 'my-runs', 'upcoming', 'agents'] as const
export type WidgetId = (typeof WIDGET_IDS)[number]

export interface Placement {
  id: WidgetId
  /** Spans the full width instead of one column. */
  wide: boolean
}

/** Always on Home: hiding it would hide agents waiting for an answer or approval. */
export const REQUIRED: WidgetId = 'needs-you'

export const DEFAULT_LAYOUT: Placement[] = [
  { id: 'needs-you', wide: true },
  { id: 'reports', wide: false },
  { id: 'problems', wide: false },
  { id: 'ask', wide: true },
  { id: 'my-runs', wide: true },
]

const key = (userId: string) => `agentorca-home:v1:${userId}`
const isWidgetId = (v: unknown): v is WidgetId => (WIDGET_IDS as readonly unknown[]).includes(v)

export function loadLayout(userId: string): Placement[] {
  let saved: unknown
  try {
    saved = JSON.parse(localStorage.getItem(key(userId)) ?? 'null')
  } catch {
    return DEFAULT_LAYOUT // storage blocked, or not what we wrote
  }
  if (!Array.isArray(saved)) return DEFAULT_LAYOUT
  const seen = new Set<WidgetId>()
  const out: Placement[] = []
  for (const p of saved as Array<Partial<Placement> | null>) {
    if (!p || !isWidgetId(p.id) || seen.has(p.id)) continue
    seen.add(p.id)
    out.push({ id: p.id, wide: p.wide === true })
  }
  return seen.has(REQUIRED) ? out : [{ id: REQUIRED, wide: true }, ...out]
}

export function saveLayout(userId: string, layout: Placement[]): void {
  try { localStorage.setItem(key(userId), JSON.stringify(layout)) } catch { /* private browsing */ }
}

export function resetLayout(userId: string): void {
  try { localStorage.removeItem(key(userId)) } catch { /* private browsing */ }
}

export function addWidget(layout: Placement[], id: WidgetId): Placement[] {
  return layout.some((p) => p.id === id) ? layout : [...layout, { id, wide: false }]
}

export function removeWidget(layout: Placement[], id: WidgetId): Placement[] {
  return id === REQUIRED ? layout : layout.filter((p) => p.id !== id)
}

export function toggleWide(layout: Placement[], id: WidgetId): Placement[] {
  return layout.map((p) => (p.id === id ? { ...p, wide: !p.wide } : p))
}

/** Swaps a widget with its neighbour among those shown (step -1 is earlier). */
export function moveWidget(layout: Placement[], id: WidgetId, step: -1 | 1, visible: WidgetId[]): Placement[] {
  const shown = layout.filter((p) => visible.includes(p.id))
  const at = shown.findIndex((p) => p.id === id)
  const other = shown[at + step]
  if (at < 0 || !other) return layout
  const a = layout.findIndex((p) => p.id === id)
  const b = layout.findIndex((p) => p.id === other.id)
  const out = [...layout]
  ;[out[a], out[b]] = [out[b], out[a]]
  return out
}
