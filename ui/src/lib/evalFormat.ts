/** Number and label formatting shared by the evaluation screens. */
import type { Experiment, ExperimentArm, ExperimentStatus, GateStatus } from '../api/evals'

export const pct = (x: number | undefined, digits = 0) => (x === undefined || Number.isNaN(x) ? '-' : `${(x * 100).toFixed(digits)}%`)

/** A signed percentage-point difference, for example "+12 pts". */
export function points(x: number | undefined): string {
  if (x === undefined || Number.isNaN(x)) return '-'
  const v = Math.round(x * 100)
  return `${v > 0 ? '+' : ''}${v} pts`
}

function signedFmt(x: number, body: string, signed: boolean): string {
  if (!signed) return x < 0 ? `-${body}` : body
  return x > 0 ? `+${body}` : x < 0 ? `-${body}` : body
}

export function usd(x: number | undefined, signed = false): string {
  if (x === undefined || Number.isNaN(x)) return '-'
  const abs = Math.abs(x)
  const digits = abs === 0 ? 2 : abs < 0.01 ? 4 : 2
  return signedFmt(x, `$${abs.toFixed(digits)}`, signed)
}

export function ms(x: number | undefined, signed = false): string {
  if (x === undefined || Number.isNaN(x)) return '-'
  const abs = Math.abs(x)
  return signedFmt(x, abs >= 1000 ? `${(abs / 1000).toFixed(1)} s` : `${Math.round(abs)} ms`, signed)
}

export function interval(lo: number | undefined, hi: number | undefined): string {
  return lo === undefined || hi === undefined ? '' : `${pct(lo)} to ${pct(hi)}`
}

/** "p = 0.031", or "p < 0.001". */
export function pValue(p: number | undefined): string {
  if (p === undefined || Number.isNaN(p)) return '-'
  return p < 0.001 ? 'p < 0.001' : `p = ${p.toFixed(3)}`
}

export const STATUS_WORDS: Record<ExperimentStatus, string> = {
  queued: 'Queued', running: 'Running', grading: 'Grading', awaiting_review: 'Waiting for review',
  complete: 'Complete', cancelled: 'Cancelled', failed: 'Failed',
}

export const STATUS_VARIANT: Record<ExperimentStatus, 'neutral' | 'info' | 'warning' | 'success' | 'error'> = {
  queued: 'neutral', running: 'info', grading: 'info', awaiting_review: 'warning',
  complete: 'success', cancelled: 'neutral', failed: 'error',
}

export const GATE_WORDS: Record<GateStatus, string> = {
  passed: 'Passed', failed: 'Failed', missing: 'Not evaluated', stale: 'Out of date',
}

export const GATE_VARIANT: Record<GateStatus, 'success' | 'error' | 'neutral' | 'warning'> = {
  passed: 'success', failed: 'error', missing: 'neutral', stale: 'warning',
}

/** What an arm runs, in words: "helper (live)", "helper at revision 3", "helper on gpt-4o". */
export function armWords(a: ExperimentArm): string {
  const parts = [a.agent]
  if (a.revision) parts.push(`at revision ${a.revision}`)
  else if (!a.modelProvider) parts.push('(live)')
  if (a.modelProvider) parts.push(`on ${a.modelProvider}`)
  return parts.join(' ')
}

export const armLabel = (a: ExperimentArm, i: number) => a.label || `arm${i + 1}`

export const isActive = (e: Pick<Experiment, 'status'>) => ['queued', 'running', 'grading', 'awaiting_review'].includes(e.status)

export const when = (iso?: string) => (iso ? new Date(iso).toLocaleString() : '-')
