/**
 * Helpers for the schedule builder: the presets a person picks ("every weekday
 * at 8:00") and the five-field cron expression they write, in both directions;
 * plain-language descriptions; and the browser's time zone.
 */

export type Preset = 'weekdays' | 'weekly' | 'daily' | 'hourly' | 'custom'

export interface WhenForm {
  preset: Preset
  /** 0-23 */
  hour: number
  /** 0-59 */
  minute: number
  /** 0 Sunday .. 6 Saturday, for the weekly preset. */
  weekday: number
  /** Used by the custom preset. */
  cron: string
}

export const WEEKDAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday']

export const PRESET_LABEL: Record<Preset, string> = {
  weekdays: 'Every weekday',
  weekly: 'Every week',
  daily: 'Every day',
  hourly: 'Every hour',
  custom: 'Custom (cron)',
}

export const DEFAULT_WHEN: WhenForm = { preset: 'weekdays', hour: 8, minute: 0, weekday: 1, cron: '0 8 * * 1-5' }

/** The cron expression for a form. */
export function cronOf(w: WhenForm): string {
  switch (w.preset) {
    case 'weekdays': return `${w.minute} ${w.hour} * * 1-5`
    case 'weekly': return `${w.minute} ${w.hour} * * ${w.weekday}`
    case 'daily': return `${w.minute} ${w.hour} * * *`
    case 'hourly': return `${w.minute} * * * *`
    case 'custom': return w.cron.trim()
  }
}

const INT = /^\d{1,2}$/

/** The form that writes `cron`: a preset when it is one, else custom. */
export function whenOf(cron: string): WhenForm {
  const custom: WhenForm = { ...DEFAULT_WHEN, preset: 'custom', cron }
  const f = cron.trim().split(/\s+/)
  if (f.length !== 5) return custom
  const [m, h, dom, mon, dow] = f
  if (!INT.test(m) || +m > 59) return custom
  const minute = +m
  if (h === '*' && dom === '*' && mon === '*' && dow === '*') return { ...DEFAULT_WHEN, preset: 'hourly', minute, cron }
  if (!INT.test(h) || +h > 23 || dom !== '*' || mon !== '*') return custom
  const hour = +h
  if (dow === '*') return { ...DEFAULT_WHEN, preset: 'daily', hour, minute, cron }
  if (dow === '1-5') return { ...DEFAULT_WHEN, preset: 'weekdays', hour, minute, cron }
  if (/^[0-6]$/.test(dow)) return { ...DEFAULT_WHEN, preset: 'weekly', hour, minute, weekday: +dow, cron }
  return custom
}

const two = (n: number) => String(n).padStart(2, '0')

/** "Every weekday at 08:00", or the expression itself when it is not a preset. */
export function describeCron(cron: string): string {
  const w = whenOf(cron)
  switch (w.preset) {
    case 'weekdays': return `Every weekday at ${two(w.hour)}:${two(w.minute)}`
    case 'weekly': return `Every ${WEEKDAYS[w.weekday]} at ${two(w.hour)}:${two(w.minute)}`
    case 'daily': return `Every day at ${two(w.hour)}:${two(w.minute)}`
    case 'hourly': return `Every hour at :${two(w.minute)}`
    case 'custom': return cron
  }
}

/** The browser's IANA time zone, UTC when it cannot be told. */
export function browserTimeZone(): string {
  try { return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC' } catch { return 'UTC' }
}

/** IANA zone names for the picker, with the current one first. */
export function timeZones(current: string): string[] {
  let all: string[] = []
  try {
    const f = (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf
    all = f ? f('timeZone') : []
  } catch { /* an older browser */ }
  if (all.length === 0) all = ['UTC', 'America/New_York', 'America/Chicago', 'America/Los_Angeles', 'Europe/London', 'Europe/Berlin', 'Asia/Tokyo', 'Australia/Sydney']
  if (!all.includes('UTC')) all = ['UTC', ...all]
  return current && !all.includes(current) ? [current, ...all] : all
}

/** A time in the given zone, for display: "Mon 16 Nov, 08:00". */
export function formatIn(iso: string | undefined, timeZone: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  try {
    return d.toLocaleString(undefined, { timeZone, weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' })
  } catch { return d.toLocaleString() }
}

export const SKIP_REASON: Record<string, string> = {
  MissedSchedule: 'It was too late to start',
  SkippedConcurrency: 'The previous run was still going',
  SkippedQuota: 'The workspace was over a limit',
  SkippedBudget: 'The spend limit was reached',
  SkippedTargetMissing: 'The agent is missing or not allowed',
  SkippedInvalid: 'The schedule has no spend limit',
}
