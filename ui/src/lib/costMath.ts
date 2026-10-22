/**
 * Pure helpers for the cost dashboard, kept out of the component so the
 * thresholds and the accessible chart summary can be unit-tested.
 */
import type { CostData, CostDay } from '../api/sse'

/** Failed-run spend above this share of the total is highlighted as an error. */
export const FAILED_SHARE_THRESHOLD = 0.2

/** Parses a USD string, treating anything non-numeric as 0. */
export function usdNum(v: string | undefined): number {
  const n = Number(v)
  return Number.isFinite(n) ? n : 0
}

/** Fraction (0..1) of total spend that went to failed runs. */
export function failedShare(d: Pick<CostData, 'totalUSD' | 'failedRunUSD'>): number {
  const total = usdNum(d.totalUSD)
  if (total <= 0) return 0
  return Math.min(1, usdNum(d.failedRunUSD) / total)
}

/** True when failed runs account for a worrying share of spend. */
export function failedShareHigh(d: Pick<CostData, 'totalUSD' | 'failedRunUSD'>): boolean {
  return failedShare(d) > FAILED_SHARE_THRESHOLD
}

/** Entries sorted by amount, largest first (ties by name, for a stable order). */
export function sortedEntries(m: Record<string, string>): Array<[string, string]> {
  return Object.entries(m).sort(([an, a], [bn, b]) => usdNum(b) - usdNum(a) || an.localeCompare(bn))
}

/** Outcome names present anywhere in the chart, largest total first. */
export function outcomesInChart(days: CostDay[]): string[] {
  const totals = new Map<string, number>()
  for (const d of days) {
    for (const [o, v] of Object.entries(d.byOutcome)) totals.set(o, (totals.get(o) ?? 0) + usdNum(v))
  }
  return [...totals.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).map(([o]) => o)
}

/** Screen-reader description of the stacked daily chart. */
export function chartSummary(days: CostDay[]): string {
  if (days.length === 0) return 'Daily spend chart: no data'
  const parts = days.map((d) => {
    const split = sortedEntries(d.byOutcome)
      .filter(([, v]) => usdNum(v) > 0)
      .map(([o, v]) => `${o} $${v}`)
      .join(', ')
    return `${d.date} $${d.usd}${split ? ` (${split})` : ''}`
  })
  return `Daily spend stacked by outcome: ${parts.join('; ')}`
}

/** Splits a `namespace/name` run reference; the namespace is '' when absent. */
export function splitRunRef(ref: string): { namespace: string; name: string } {
  const i = ref.indexOf('/')
  return i < 0 ? { namespace: '', name: ref } : { namespace: ref.slice(0, i), name: ref.slice(i + 1) }
}
