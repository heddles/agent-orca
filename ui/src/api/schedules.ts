/**
 * Client for scheduled runs (docs/schedules.md). Reading needs the viewer role,
 * suspend, resume and Run now the runner role, and creating, editing and
 * deleting a schedule the builder role. Plain CRUD goes through
 * /api/resources/agentschedules (see api/builder.ts); the rest is /api/schedules.
 */
import { ApiError, failure, getResource, type KubeObject, type Ownership } from './builder'
import { apiFetch } from './sse'

export const SCHEDULE_KIND = 'agentschedules'

export type ScheduleState = 'active' | 'suspended' | 'degraded' | 'budgetExhausted' | 'targetMissing' | 'invalid'

export interface ScheduleResult {
  status: 'succeeded' | 'failed' | 'skipped' | 'none'
  at?: string
  reason?: string
  run?: string
}

export interface ScheduleSummary {
  name: string
  namespace: string
  target: string
  targetKind: 'agent' | 'workflowTemplate'
  schedule: string
  timeZone: string
  concurrencyPolicy: string
  suspended: boolean
  state: ScheduleState
  stateReason?: string
  /** The next run, absolute (UTC) and in the schedule's own zone. */
  nextRun?: string
  nextRunLocal?: string
  lastRun?: string
  lastResult: ScheduleResult
  spentTodayUSD?: string
  spentMonthUSD?: string
  consecutiveFailures: number
  active: string[]
  recentSkips: { time: string; reason: string; message?: string }[]
  ownership: Ownership
  /** Where messages about the schedule go; absent when nowhere. */
  notify?: ScheduleNotifyView
}

/** A Slack channel reached through one of the tenant's chat apps. */
export interface SlackTarget {
  app: string
  channelID: string
  channelName?: string
}

export interface ScheduleNotifyView {
  /** A notification channel of the TenantConfig; absent with slack. */
  channel?: string
  slack?: SlackTarget
  /** Why messages cannot be sent there, when they cannot. */
  problem?: string
  /** Set when the destination above was chosen here in place of the one in Git. */
  override?: { setBy: string; setAt: string; git: { channel?: string; slack?: SlackTarget } }
}

export interface SchedulePreview {
  timeZone: string
  nextRuns: { utc: string; local: string }[]
  minInterval: string
  shortestGap: string
  tooFrequent: boolean
}

export interface ScheduleRun {
  name: string
  namespace: string
  kind: 'AgentRun' | 'AgentWorkflow'
  /** "schedule" for an unattended tick, "manual" for Run now. */
  trigger: 'schedule' | 'manual'
  scheduledFor?: string
  createdBy?: string
  phase: string
  spendUSD?: string
  createdAt: string
  completionTime?: string
}

const enc = encodeURIComponent

async function call<T>(url: string, method = 'GET'): Promise<T> {
  const res = await apiFetch(url, { method })
  if (!res.ok) throw await failure(res)
  return res.json()
}

export async function listSchedules(namespace = ''): Promise<ScheduleSummary[]> {
  return (await call<ScheduleSummary[] | null>(`/api/schedules?namespace=${enc(namespace)}`)) ?? []
}

export const previewSchedule = (cron: string, tz: string, count = 5) =>
  call<SchedulePreview>(`/api/schedules/preview?cron=${enc(cron)}&tz=${enc(tz)}&count=${count}`)

export async function listScheduleRuns(namespace: string, name: string, before = ''): Promise<{ runs: ScheduleRun[]; nextBefore: string }> {
  const q = before ? `?before=${enc(before)}` : ''
  const body = await call<{ runs: ScheduleRun[] | null; nextBefore: string }>(`/api/schedules/${enc(namespace)}/${enc(name)}/runs${q}`)
  return { runs: body.runs ?? [], nextBefore: body.nextBefore }
}

const action = (ns: string, name: string, what: string) => `/api/schedules/${enc(ns)}/${enc(name)}/${what}`
/** Sends the reports of a Git-managed schedule to a channel chosen here (admin). The schedule itself is not changed. */
export async function chooseDestination(ns: string, name: string, to: SlackTarget): Promise<void> {
  const res = await apiFetch(action(ns, name, 'destination'), { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(to) })
  if (!res.ok) throw await failure(res)
}
/** Goes back to the destination in Git (admin). */
export const resetDestination = (ns: string, name: string) => call<{ status: string }>(action(ns, name, 'destination'), 'DELETE')
export const suspendSchedule = (ns: string, name: string) => call<{ status: string }>(action(ns, name, 'suspend'), 'POST')
export const resumeSchedule = (ns: string, name: string) => call<{ status: string }>(action(ns, name, 'resume'), 'POST')
/** reportOn is the outcomes the run is reported on; absent when nothing will be sent. */
export const runScheduleNow = (ns: string, name: string) =>
  call<{ kind: string; name: string; namespace: string; reportOn?: ('Succeeded' | 'Failed')[] }>(action(ns, name, 'run'), 'POST')

/** A notification channel an administrator set up; the destination itself is never sent. */
export interface NotificationChannel {
  name: string
  type: 'webhook' | 'slack' | 'smtp'
}

/** The channels of the caller's tenant (builder role); empty when there are none or the role is missing. */
export async function listNotificationChannels(namespace = ''): Promise<NotificationChannel[]> {
  const res = await apiFetch(`/api/notification-channels?namespace=${enc(namespace)}`)
  if (res.status === 403) return []
  if (!res.ok) throw await failure(res)
  return (await res.json()) ?? []
}

export const getSchedule = (ns: string, name: string) => getResource(SCHEDULE_KIND, ns, name)

export async function deleteSchedule(ns: string, name: string): Promise<void> {
  const res = await apiFetch(`/api/resources/${SCHEDULE_KIND}/${enc(ns)}/${enc(name)}`, { method: 'DELETE' })
  if (!res.ok) throw await failure(res)
}

/** A plain-language message for a failed call. */
export function describeScheduleError(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 403) return e.message || 'You do not have the role this needs.'
    if (e.status === 409) return e.message || 'Someone changed this schedule. Reload it and try again.'
    if (e.status === 429 || e.status === 402) return e.message || 'The workspace is over a limit. Try again later.'
    return e.errors.length ? e.errors.join(' ') : e.message
  }
  return e instanceof Error ? e.message : String(e)
}

export type { KubeObject }
