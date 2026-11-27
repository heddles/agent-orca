/**
 * Small pieces of scheduled runs shown outside the Schedules page: the state
 * chip, the badge on an agent in the fleet, and the "Scheduled by" chip on a run.
 */
import { Link } from 'react-router'
import type { ScheduleState, ScheduleSummary } from '../api/schedules'
import { DSButton, DSChip, type DSChipVariant } from '../lib/primitives'
import { ds } from '../lib/designSystem'
import { formatIn, SKIP_REASON } from '../lib/schedule'
import { ROUTES } from '../lib/routes'

export const STATE_LABEL: Record<ScheduleState, string> = {
  active: 'On',
  suspended: 'Paused',
  degraded: 'Stopped after failures',
  budgetExhausted: 'Spend limit reached',
  targetMissing: 'Agent missing',
  invalid: 'Not valid',
}

const STATE_VARIANT: Record<ScheduleState, DSChipVariant> = {
  active: 'success', suspended: 'neutral', degraded: 'error', budgetExhausted: 'warning', targetMissing: 'warning', invalid: 'error',
}

/** The state with the colour and a word, so it never rests on colour alone. */
export function ScheduleStateChip({ state }: { state: ScheduleState }) {
  return <DSChip size="sm" variant={STATE_VARIANT[state]} dot>{STATE_LABEL[state]}</DSChip>
}

/** The last result of a schedule in a short phrase. */
export function lastResultText(s: ScheduleSummary): string {
  const r = s.lastResult
  switch (r.status) {
    case 'succeeded': return `Last run succeeded ${formatIn(r.at, s.timeZone)}`
    case 'failed': return `Last run failed ${formatIn(r.at, s.timeZone)}`
    case 'skipped': return `Skipped ${formatIn(r.at, s.timeZone)}: ${SKIP_REASON[(r.reason ?? '').split(':')[0]] ?? r.reason ?? ''}`
    default: return 'Has not run yet'
  }
}

/** A clock badge on an agent that has schedules: next run and last result. */
export function ScheduleBadge({ schedules }: { schedules: ScheduleSummary[] }) {
  if (schedules.length === 0) return null
  const first = schedules.find((s) => s.nextRun) ?? schedules[0]
  const more = schedules.length > 1 ? ` and ${schedules.length - 1} more` : ''
  const next = first.nextRun ? `next ${formatIn(first.nextRun, first.timeZone)}` : first.suspended ? 'paused' : 'no run planned'
  return (
    <Link
      to={ROUTES.schedules}
      aria-label={`Scheduled: ${first.name}${more}, ${next}. ${lastResultText(first)}`}
      title={`${first.name}${more}: ${next}. ${lastResultText(first)}`}
      style={{ textDecoration: 'none' }}
    >
      <DSChip size="sm" variant={first.state === 'active' ? 'info' : 'warning'}>⏰ Scheduled · {next}</DSChip>
    </Link>
  )
}

/** On a run: who started it and when it was due, with a button to run it again now. */
export function ScheduledByChip({ schedule, trigger, scheduledFor, onRunAgain, busy }: {
  schedule: string
  trigger?: string
  scheduledFor?: string
  onRunAgain?: () => void
  busy?: boolean
}) {
  const manual = trigger === 'manual'
  return (
    <span style={{ display: 'inline-flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
      <DSChip size="sm" variant="info">
        {manual ? 'Run now from' : 'Scheduled by'} <Link to={ROUTES.schedules} style={{ color: ds.accent, marginLeft: 4 }}>{schedule}</Link>
        {scheduledFor && !manual ? ` · due ${new Date(scheduledFor).toLocaleString()}` : ''}
      </DSChip>
      {onRunAgain && <DSButton size="sm" variant="secondary" disabled={busy} onClick={onRunAgain}
        title="Starts a run now. Like a timed run, it sends the schedule's report, if it has one.">{busy ? 'Starting…' : 'Run again now'}</DSButton>}
    </span>
  )
}
