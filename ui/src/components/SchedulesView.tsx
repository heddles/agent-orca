/**
 * SchedulesView — scheduled runs: what runs when, whether it is healthy, what it
 * spent, and the controls: pause, resume, run now, edit, delete. Builders create
 * and edit schedules; a schedule managed in Git opens read-only with Copy YAML
 * and Duplicate, and is paused in Git.
 */
import { useCallback, useEffect, useState, type CSSProperties } from 'react'
import { Link } from 'react-router'
import { useNamespace } from '../lib/namespace'
import { liveRunPath, workflowPath } from '../lib/routes'
import {
  createResource, updateResource, duplicateResource, writeBody, type KubeObject,
} from '../api/builder'
import { getCatalog, type CatalogEntry } from '../api/catalog'
import {
  SCHEDULE_KIND, deleteSchedule, describeScheduleError, getSchedule, listNotificationChannels, listScheduleRuns, listSchedules, previewSchedule,
  resumeSchedule, runScheduleNow, suspendSchedule, type NotificationChannel, type SchedulePreview, type ScheduleRun, type ScheduleSummary,
} from '../api/schedules'
import { canRouteReports, canWriteEvals, useRoles } from '../lib/access'
import { ds, DESIGN } from '../lib/designSystem'
import { Dialog } from '../lib/dialog'
import { ICON } from '../lib/icons'
import { EmptyState } from '../lib/layout'
import { DSButton } from '../lib/primitives'
import {
  browserTimeZone, cronOf, describeCron, formatIn, PRESET_LABEL, timeZones, WEEKDAYS, whenOf, DEFAULT_WHEN, SKIP_REASON,
  type Preset, type WhenForm,
} from '../lib/schedule'
import { stringifyYAML } from '../lib/yaml'
import { Field, field } from './BuilderParts'
import { CopyButton } from './AgentTriggers'
import { lastResultText, ScheduleStateChip } from './ScheduleParts'
import { ChangeChannelDialog, DestinationFields, DestinationLine, VIA_SLACK, destinationText, runNowHint, runNowMessage, slackProblem, type DestinationForm } from './ScheduleDestination'

export function SchedulesView() {
  const { namespace } = useNamespace()
  const roles = useRoles()
  const builder = canWriteEvals(roles)
  const admin = canRouteReports(roles)
  const [rows, setRows] = useState<ScheduleSummary[] | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [editing, setEditing] = useState<{ ns: string; name: string } | 'new' | null>(null)
  const [deleting, setDeleting] = useState<ScheduleSummary | null>(null)
  const [open, setOpen] = useState('')
  const [routing, setRouting] = useState<ScheduleSummary | null>(null)

  const load = useCallback(() => {
    let live = true
    listSchedules(namespace)
      .then((r) => { if (live) { setRows(r); setError('') } })
      .catch((e: unknown) => { if (live) { setError(describeScheduleError(e)); setRows((old) => old ?? []) } })
    return () => { live = false }
  }, [namespace])
  useEffect(() => load(), [load])

  const act = async <T,>(what: () => Promise<T>, done: string | ((result: T) => string)) => {
    setNotice('')
    try { const result = await what(); setNotice(typeof done === 'function' ? done(result) : done); setError(''); load() } catch (e) { setError(describeScheduleError(e)) }
  }

  if (rows === null) return <div role="status" style={{ padding: 24, color: ds.textMuted, fontSize: 13 }}>Loading schedules…</div>

  return (
    <section aria-label="Schedules" style={s.wrap}>
      <div style={s.row}>
        <p style={{ ...s.lead, flex: 1 }}>
          A schedule starts an agent on a clock, for example every weekday at 8:00. Each time it runs it makes an ordinary run
          that you can open like any other. Nobody is signed in when it runs, so it is attributed to the schedule.
        </p>
        {builder && <DSButton variant="primary" onClick={() => setEditing('new')}>New schedule</DSButton>}
      </div>
      {error && <p role="alert" style={field.error}>{error}</p>}
      {notice && <p role="status" style={s.ok}>{notice}</p>}
      {rows.length === 0 && !error && (
        <EmptyState icon={ICON.workflow} title="No schedules yet">
          {builder ? 'Create one to have an agent run on its own.' : 'Ask a builder in your workspace to set one up.'}
        </EmptyState>
      )}
      <ul style={s.list}>
        {rows.map((r) => {
          const key = `${r.namespace}/${r.name}`
          const git = r.ownership.owner !== 'ui'
          return (
            <li key={key} style={s.card} aria-label={`Schedule ${r.name}`}>
              <div style={s.row}>
                <strong style={{ fontSize: 14 }}>{r.name}</strong>
                <ScheduleStateChip state={r.state} />
                {git && <span style={s.muted}>Managed in Git</span>}
                <span style={s.muted}>{r.namespace}</span>
              </div>
              <p style={s.lead}>
                {describeCron(r.schedule)} ({r.timeZone}) runs {r.targetKind === 'agent' ? 'the agent' : 'the workflow template'} <strong>{r.target}</strong>
              </p>
              {r.stateReason && r.state !== 'active' && <p style={r.state === 'suspended' ? s.muted : s.warn}>{r.stateReason}</p>}
              <div style={s.facts}>
                <span>{r.nextRun ? <>Next run <strong>{formatIn(r.nextRun, r.timeZone)}</strong> ({r.timeZone}){r.nextRunLocal && r.timeZone !== browserTimeZone() ? <>, <span title={r.nextRun}>{formatIn(r.nextRun, browserTimeZone())} for you</span></> : null}</> : 'No run planned'}</span>
                <span>{lastResultText(r)}</span>
                <span>Spent ${r.spentTodayUSD || '0'} today, ${r.spentMonthUSD || '0'} this month</span>
                {r.targetKind === 'agent' && <DestinationLine notify={r.notify} git={git} />}
              </div>
              <div style={s.row}>
                {!r.suspended ? (
                  <DSButton size="sm" variant="secondary" disabled={git} title={git ? 'This schedule is managed in Git: pause it there.' : undefined}
                    onClick={() => act(() => suspendSchedule(r.namespace, r.name), `${r.name} is paused.`)}>Pause</DSButton>
                ) : (
                  <DSButton size="sm" variant="secondary" disabled={git} title={git ? 'This schedule is managed in Git: resume it there.' : undefined}
                    onClick={() => act(() => resumeSchedule(r.namespace, r.name), `${r.name} is on again.`)}>Resume</DSButton>
                )}
                {r.state === 'degraded' && !git && (
                  <DSButton size="sm" variant="primary" onClick={() => act(() => resumeSchedule(r.namespace, r.name), `${r.name} will run at its next time.`)}>Start again</DSButton>
                )}
                <DSButton size="sm" variant="secondary" title={runNowHint(r.targetKind, r.notify)}
                  onClick={() => act(() => runScheduleNow(r.namespace, r.name), (run) => runNowMessage(r.name, r.notify, run.reportOn))}>Run now</DSButton>
                {admin && git && r.targetKind === 'agent' && r.notify && (
                  <DSButton size="sm" variant="secondary" onClick={() => setRouting(r)}>Change channel</DSButton>
                )}
                <DSButton size="sm" variant="ghost" onClick={() => setOpen(open === key ? '' : key)} aria-expanded={open === key}>
                  {open === key ? 'Hide history' : 'History'}
                </DSButton>
                <DSButton size="sm" variant="ghost" onClick={() => setEditing({ ns: r.namespace, name: r.name })}>
                  {builder && !git ? 'Edit' : 'View'}
                </DSButton>
                {builder && !git && <DSButton size="sm" variant="ghost" onClick={() => setDeleting(r)}>Delete</DSButton>}
              </div>
              {open === key && <History schedule={r} />}
            </li>
          )
        })}
      </ul>

      {routing && (
        <ChangeChannelDialog schedule={routing} onClose={() => setRouting(null)}
          onSaved={(msg) => { setRouting(null); setNotice(msg); load() }} />
      )}

      {editing && (
        <ScheduleDialog
          target={editing}
          builder={builder}
          admin={admin}
          summary={editing === 'new' ? undefined : rows.find((r) => r.namespace === editing.ns && r.name === editing.name)}
          onClose={() => setEditing(null)}
          onSaved={(msg) => { setEditing(null); setNotice(msg); load() }}
        />
      )}
      {deleting && (
        <Dialog title={`Delete ${deleting.name}?`} onClose={() => setDeleting(null)}>
          <p style={s.lead}>No more runs will start. The runs it already made are deleted with it.</p>
          <div style={s.row}>
            <DSButton variant="primary" onClick={async () => {
              const d = deleting
              setDeleting(null)
              await act(() => deleteSchedule(d.namespace, d.name), `${d.name} was deleted.`)
            }}>Delete schedule</DSButton>
            <DSButton variant="ghost" onClick={() => setDeleting(null)}>Cancel</DSButton>
          </div>
        </Dialog>
      )}
    </section>
  )
}

// ── History ──────────────────────────────────────────────────────────────────

function History({ schedule }: { schedule: ScheduleSummary }) {
  const [runs, setRuns] = useState<ScheduleRun[] | null>(null)
  const [next, setNext] = useState('')
  const [error, setError] = useState('')
  useEffect(() => {
    let live = true
    listScheduleRuns(schedule.namespace, schedule.name)
      .then((r) => { if (live) { setRuns(r.runs); setNext(r.nextBefore) } })
      .catch((e: unknown) => { if (live) { setError(describeScheduleError(e)); setRuns([]) } })
    return () => { live = false }
  }, [schedule.namespace, schedule.name])
  const more = async () => {
    try { const r = await listScheduleRuns(schedule.namespace, schedule.name, next); setRuns((old) => [...(old ?? []), ...r.runs]); setNext(r.nextBefore) } catch (e) { setError(describeScheduleError(e)) }
  }
  if (runs === null) return <p role="status" style={s.muted}>Loading history…</p>
  return (
    <div aria-label={`History of ${schedule.name}`} style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
      {error && <p role="alert" style={field.error}>{error}</p>}
      {schedule.recentSkips.length > 0 && (
        <ul style={{ ...s.list, gap: 2 }} aria-label="Skipped times">
          {schedule.recentSkips.map((k) => (
            <li key={k.time + k.reason} style={s.muted}>Skipped {formatIn(k.time, schedule.timeZone)}: {SKIP_REASON[k.reason] ?? k.reason}{k.message ? ` (${k.message})` : ''}</li>
          ))}
        </ul>
      )}
      {runs.length === 0 && <p style={s.muted}>No runs yet.</p>}
      <ul style={{ ...s.list, gap: 2 }}>
        {runs.map((r) => (
          <li key={r.name} style={s.histRow}>
            <span>{formatIn(r.createdAt, schedule.timeZone)}</span>
            <Link to={r.kind === 'AgentRun' ? liveRunPath(r.namespace, r.name) : workflowPath(r.namespace, r.name)}>{r.name}</Link>
            <span>{r.phase || 'Pending'}</span>
            <span style={s.muted}>{r.trigger === 'manual' ? 'Run now' : 'Scheduled'}</span>
            {r.spendUSD && <span style={s.muted}>${r.spendUSD}</span>}
          </li>
        ))}
      </ul>
      {next && <div><DSButton size="sm" variant="ghost" onClick={more}>Older runs</DSButton></div>}
    </div>
  )
}

// ── The builder section ──────────────────────────────────────────────────────

interface FormState extends DestinationForm {
  name: string
  namespace: string
  agent: string
  when: WhenForm
  timeZone: string
  input: string
  concurrency: 'Forbid' | 'Allow' | 'Replace'
  maxPerRun: string
  maxPerDay: string
  maxPerMonth: string
  /** Succeeded, Failed, Skipped, BreakerOpen. */
  notifyOn: string[]
  notifyOutput: boolean
}

/** What a channel can be told, in the words of the form. */
export const NOTIFY_EVENTS: { value: string; label: string }[] = [
  { value: 'Failed', label: 'A run fails' },
  { value: 'Succeeded', label: 'A run succeeds' },
  { value: 'Skipped', label: 'The spend limit stops the schedule' },
  { value: 'BreakerOpen', label: 'The schedule stops after repeated failures' },
]

function emptyForm(namespace: string): FormState {
  return {
    name: '', namespace, agent: '', when: DEFAULT_WHEN, timeZone: browserTimeZone(), input: '',
    concurrency: 'Forbid', maxPerRun: '1.00', maxPerDay: '', maxPerMonth: '',
    notifyChannel: '', slackApp: '', slackChannelID: '', slackChannelName: '', notifyOn: ['Failed'], notifyOutput: false,
  }
}

function formOf(obj: KubeObject): FormState {
  const spec = (obj.spec ?? {}) as Record<string, any>
  return {
    name: obj.metadata.name ?? '', namespace: obj.metadata.namespace ?? '',
    agent: spec.agentRef ?? '', when: whenOf(spec.schedule ?? ''), timeZone: spec.timeZone ?? 'UTC', input: spec.input ?? '',
    concurrency: spec.concurrencyPolicy ?? 'Forbid',
    maxPerRun: spec.budget?.maxUSD ?? '', maxPerDay: spec.spend?.maxUSDPerDay ?? '', maxPerMonth: spec.spend?.maxUSDPerMonth ?? '',
    notifyChannel: spec.result?.notify?.slack ? VIA_SLACK : spec.result?.notify?.channel ?? '',
    slackApp: spec.result?.notify?.slack?.app ?? '', slackChannelID: spec.result?.notify?.slack?.channelID ?? '',
    slackChannelName: spec.result?.notify?.slack?.channelName ?? '', notifyOn: spec.result?.notify?.on ?? ['Failed'], notifyOutput: spec.result?.notify?.includeOutput ?? false,
  }
}

const MONEY = /^[0-9]+(\.[0-9]+)?$/

/** Problems with the form, by field, in plain words. Empty when it can be saved. */
export function formProblems(f: FormState, existing: boolean): Record<string, string> {
  const p: Record<string, string> = {}
  if (!existing && !/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(f.name)) p.name = 'Use lower-case letters, numbers and dashes.'
  else if (f.name.length > 52) p.name = 'Use at most 52 characters.'
  if (!f.agent) p.agent = 'Choose the agent to run.'
  if (!f.input.trim()) p.input = 'Say what the agent should do.'
  if (!cronOf(f.when)) p.when = 'Write a schedule.'
  for (const [k, v] of [['maxPerRun', f.maxPerRun], ['maxPerDay', f.maxPerDay], ['maxPerMonth', f.maxPerMonth]] as const) {
    if (v && (!MONEY.test(v) || Number(v) <= 0)) p[k] = 'Enter an amount in dollars, such as 1.50.'
  }
  if (f.notifyChannel === VIA_SLACK && slackProblem(f)) p.slack = slackProblem(f)
  if (f.notifyChannel && f.notifyOn.length === 0) p.notifyOn = 'Choose at least one thing to be told about.'
  else if (f.notifyChannel && f.notifyOutput && !f.notifyOn.some((e) => e === 'Succeeded' || e === 'Failed')) p.notifyOutput = 'Only a finished run has output: also choose "A run fails" or "A run succeeds".'
  if (!f.maxPerRun && !f.maxPerDay && !f.maxPerMonth) p.maxPerRun = 'Set a spend limit: a schedule that runs by itself needs one.'
  return p
}

function ScheduleDialog({ target, builder, admin, summary, onClose, onSaved }: {
  target: { ns: string; name: string } | 'new'
  builder: boolean
  admin: boolean
  summary?: ScheduleSummary
  onClose: () => void
  onSaved: (message: string) => void
}) {
  const { namespace } = useNamespace()
  const isNew = target === 'new'
  const [obj, setObj] = useState<KubeObject | null>(null)
  const [form, setForm] = useState<FormState>(() => emptyForm(namespace))
  const [agents, setAgents] = useState<CatalogEntry[]>([])
  const [channels, setChannels] = useState<NotificationChannel[]>([])
  const [preview, setPreview] = useState<SchedulePreview | null>(null)
  const [previewError, setPreviewError] = useState('')
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [failure, setFailure] = useState('')
  const [busy, setBusy] = useState(false)
  const [loadError, setLoadError] = useState('')

  useEffect(() => {
    if (isNew) return
    getSchedule(target.ns, target.name)
      .then((o) => { setObj(o); setForm(formOf(o)) })
      .catch((e: unknown) => setLoadError(describeScheduleError(e)))
  }, [isNew, target])
  useEffect(() => {
    getCatalog(namespace).then((c) => setAgents(c.filter((a) => !a.template))).catch(() => setAgents([]))
  }, [namespace])

  useEffect(() => {
    if (!builder) return
    listNotificationChannels(namespace).then(setChannels).catch(() => setChannels([]))
  }, [builder, namespace])

  const cron = cronOf(form.when)
  useEffect(() => {
    if (!cron) { setPreview(null); return }
    let live = true
    const t = setTimeout(() => {
      previewSchedule(cron, form.timeZone, 5)
        .then((p) => { if (live) { setPreview(p); setPreviewError('') } })
        .catch((e: unknown) => { if (live) { setPreview(null); setPreviewError(describeScheduleError(e)) } })
    }, 250)
    return () => { live = false; clearTimeout(t) }
  }, [cron, form.timeZone])

  const git = !isNew && obj !== null && obj.ownership?.owner !== 'ui'
  const readOnly = !builder || git
  const canRoute = !readOnly && admin
  const set = <K extends keyof FormState>(k: K, v: FormState[K]) => setForm((f) => ({ ...f, [k]: v }))
  const setDestination = useCallback((change: Partial<DestinationForm>) => setForm((f) => ({ ...f, ...change })), [])

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const p = formProblems(form, !isNew)
    // What the caller cannot change is not theirs to fix.
    if (!admin) { delete p.slack; delete p.notifyOn; delete p.notifyOutput }
    setErrors(p)
    setFailure('')
    if (Object.keys(p).length > 0) return
    if (preview?.tooFrequent) { setFailure(`This runs more often than every ${preview.minInterval}.`); return }
    setBusy(true)
    try {
      const spec: Record<string, unknown> = {
        ...((obj?.spec ?? {}) as Record<string, unknown>),
        agentRef: form.agent, schedule: cron, timeZone: form.timeZone, input: form.input, concurrencyPolicy: form.concurrency,
      }
      if (form.maxPerRun) spec.budget = { ...((spec.budget as object | undefined) ?? {}), maxUSD: form.maxPerRun }
      else delete spec.budget
      if (form.maxPerDay || form.maxPerMonth) {
        spec.spend = { ...((spec.spend as object | undefined) ?? {}), maxUSDPerDay: form.maxPerDay || undefined, maxUSDPerMonth: form.maxPerMonth || undefined }
      } else delete spec.spend
      // Keep what an administrator set on result (egress, callbacks). Only an admin
      // may change notify, so for anyone else it goes back as it was read.
      const result = { ...((spec.result as Record<string, unknown> | undefined) ?? {}) }
      if (admin) {
        const to = form.notifyChannel === VIA_SLACK
          ? { slack: { app: form.slackApp, channelID: form.slackChannelID, ...(form.slackChannelName ? { channelName: form.slackChannelName } : {}) } }
          : { channel: form.notifyChannel }
        if (form.notifyChannel) result.notify = { ...to, on: form.notifyOn, includeOutput: form.notifyOutput }
        else delete result.notify
      }
      if (Object.keys(result).length > 0) spec.result = result
      else delete spec.result
      if (isNew) {
        await createResource(SCHEDULE_KIND, { apiVersion: 'agentorca.agentorca.io/v1alpha1', kind: 'AgentSchedule', metadata: { name: form.name, namespace: form.namespace }, spec })
        onSaved(`${form.name} was created.`)
      } else if (obj) {
        await updateResource(SCHEDULE_KIND, target.ns, target.name, { ...obj, spec })
        onSaved(`${target.name} was saved.`)
      }
    } catch (err) {
      setFailure(describeScheduleError(err))
      setBusy(false)
    }
  }

  if (loadError) return <Dialog title="Schedule" onClose={onClose}><p role="alert" style={field.error}>{loadError}</p></Dialog>
  if (!isNew && !obj) return <Dialog title="Schedule" onClose={onClose}><p role="status" style={s.muted}>Loading…</p></Dialog>

  const title = isNew ? 'New schedule' : readOnly ? `Schedule ${form.name}` : `Edit ${form.name}`
  return (
    <Dialog title={title} onClose={onClose} width={620}>
      {git && obj && <GitOwned obj={obj} onDone={onSaved} />}
      <form onSubmit={submit} noValidate style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        <fieldset disabled={readOnly || busy} style={s.fieldset}>
          {isNew && (
            <Field label="Name" error={errors.name}>
              {(p) => <input {...p} style={field.control} value={form.name} maxLength={52} onChange={(e) => set('name', e.target.value)} />}
            </Field>
          )}
          <Field label="Which agent?" error={errors.agent}>
            {(p) => (
              <select {...p} style={field.control} value={form.agent} onChange={(e) => set('agent', e.target.value)}>
                <option value="">Choose an agent…</option>
                {form.agent && !agents.some((a) => a.name === form.agent) && <option value={form.agent}>{form.agent}</option>}
                {agents.map((a) => <option key={`${a.namespace}/${a.name}`} value={a.name}>{a.name}</option>)}
              </select>
            )}
          </Field>

          <h3 style={s.h3}>When should this run?</h3>
          <div style={s.row}>
            <Field label="Repeat">
              {(p) => (
                <select {...p} style={field.control} value={form.when.preset} onChange={(e) => {
                  const preset = e.target.value as Preset
                  set('when', { ...form.when, preset, cron: preset === 'custom' ? cron : form.when.cron })
                }}>
                  {(Object.keys(PRESET_LABEL) as Preset[]).map((k) => <option key={k} value={k}>{PRESET_LABEL[k]}</option>)}
                </select>
              )}
            </Field>
            {form.when.preset === 'weekly' && (
              <Field label="On">
                {(p) => (
                  <select {...p} style={field.control} value={form.when.weekday} onChange={(e) => set('when', { ...form.when, weekday: Number(e.target.value) })}>
                    {WEEKDAYS.map((d, i) => <option key={d} value={i}>{d}</option>)}
                  </select>
                )}
              </Field>
            )}
            {form.when.preset !== 'custom' && form.when.preset !== 'hourly' && (
              <Field label="At hour">
                {(p) => <input {...p} type="number" min={0} max={23} style={field.control} value={form.when.hour} onChange={(e) => set('when', { ...form.when, hour: Math.min(23, Math.max(0, Number(e.target.value) || 0)) })} />}
              </Field>
            )}
            {form.when.preset !== 'custom' && (
              <Field label={form.when.preset === 'hourly' ? 'At minute' : 'Minute'}>
                {(p) => <input {...p} type="number" min={0} max={59} style={field.control} value={form.when.minute} onChange={(e) => set('when', { ...form.when, minute: Math.min(59, Math.max(0, Number(e.target.value) || 0)) })} />}
              </Field>
            )}
          </div>
          {form.when.preset === 'custom' && (
            <Field label="Cron expression" help="Five fields: minute, hour, day of month, month, day of week." error={errors.when}>
              {(p) => <input {...p} style={{ ...field.control, fontFamily: DESIGN.font.mono }} value={form.when.cron} onChange={(e) => set('when', { ...form.when, cron: e.target.value })} />}
            </Field>
          )}
          <Field label="Time zone">
            {(p) => (
              <select {...p} style={field.control} value={form.timeZone} onChange={(e) => set('timeZone', e.target.value)}>
                {timeZones(form.timeZone).map((z) => <option key={z} value={z}>{z}</option>)}
              </select>
            )}
          </Field>
          <div aria-label="Next runs" aria-live="polite" style={s.preview}>
            {previewError && <span role="alert" style={field.error}>{previewError}</span>}
            {preview && (
              <>
                <strong style={{ fontSize: 12 }}>Next runs</strong>
                <ul style={{ ...s.list, gap: 2 }}>
                  {preview.nextRuns.map((n) => <li key={n.utc} style={s.muted}>{formatIn(n.utc, form.timeZone)} ({form.timeZone})</li>)}
                </ul>
                {preview.tooFrequent && <span role="alert" style={field.error}>That is more often than every {preview.minInterval}: choose a longer interval.</span>}
              </>
            )}
          </div>

          <h3 style={s.h3}>What should it do?</h3>
          <Field label="Instruction" help="{{schedule.date}} becomes the day it runs, {{schedule.time}} the time, {{schedule.name}} this schedule's name." error={errors.input}>
            {(p) => <textarea {...p} rows={4} style={{ ...field.control, resize: 'vertical' }} value={form.input} onChange={(e) => set('input', e.target.value)} />}
          </Field>
          {!readOnly && <div><DSButton type="button" size="sm" variant="ghost" onClick={() => set('input', `${form.input}${form.input && !form.input.endsWith(' ') ? ' ' : ''}{{schedule.date}}`)}>Insert today's date</DSButton></div>}

          <h3 style={s.h3}>If the last run is still going</h3>
          <div role="radiogroup" aria-label="If the last run is still going" style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
            {([['Forbid', 'Skip this time'], ['Allow', 'Run anyway'], ['Replace', 'Stop the old one and start a new one']] as const).map(([v, label]) => (
              <label key={v} style={s.radio}><input type="radio" name="concurrency" checked={form.concurrency === v} onChange={() => set('concurrency', v)} /> {label}</label>
            ))}
          </div>

          <h3 style={s.h3}>Send the result to</h3>
          <p style={s.muted}>The result is always kept on the run, which you can open from the history. A channel gets a short message as well.</p>
          {git && <p role="status" style={s.warn}>This schedule is managed in Git. What is shown here is what Git says (<code>spec.result.notify</code>). To send its messages to another Slack channel without changing Git, an admin can use Change channel on the schedule list.</p>}
          {git && summary?.notify?.override && <p style={s.warn}>A channel chosen here is in force: messages go to {destinationText(summary.notify)}.</p>}
          {!git && !admin && <p style={s.muted}>Only an admin can change where reports go and what they include.</p>}
          <fieldset disabled={!canRoute} aria-label="Report destination" style={s.fieldset}>
            <DestinationFields form={form} channels={channels} editable={canRoute} error={errors.slack} onChange={setDestination} />
            {form.notifyChannel && (
              <>
                <fieldset aria-label="Send a message when" style={{ ...s.fieldset, gap: 4 }}>
                  <legend style={s.muted}>Send a message when</legend>
                  {NOTIFY_EVENTS.map((ev) => (
                    <label key={ev.value} style={s.radio}>
                      <input type="checkbox" checked={form.notifyOn.includes(ev.value)}
                        onChange={(e) => set('notifyOn', e.target.checked ? [...form.notifyOn, ev.value] : form.notifyOn.filter((x) => x !== ev.value))} /> {ev.label}
                    </label>
                  ))}
                  {errors.notifyOn && <span role="alert" style={field.error}>{errors.notifyOn}</span>}
                </fieldset>
                <label style={s.radio}>
                  <input type="checkbox" checked={form.notifyOutput} onChange={(e) => set('notifyOutput', e.target.checked)} /> Include the run's output in the message
                </label>
                <p style={s.muted}>{form.notifyChannel === VIA_SLACK
                  ? 'The whole output is sent; a long one takes several messages.'
                  : 'The output is cut to 2000 characters.'} Anyone who can read the channel can read it, so leave this off for private results.</p>
                {errors.notifyOutput && <span role="alert" style={field.error}>{errors.notifyOutput}</span>}
              </>
            )}
          </fieldset>

          <h3 style={s.h3}>Spend limit</h3>
          <div style={s.row}>
            <Field label="Per run (USD)" error={errors.maxPerRun}>
              {(p) => <input {...p} inputMode="decimal" style={field.control} value={form.maxPerRun} onChange={(e) => set('maxPerRun', e.target.value)} />}
            </Field>
            <Field label="Per day, all runs (USD)" error={errors.maxPerDay}>
              {(p) => <input {...p} inputMode="decimal" style={field.control} value={form.maxPerDay} onChange={(e) => set('maxPerDay', e.target.value)} />}
            </Field>
            <Field label="Per month, all runs (USD)" error={errors.maxPerMonth}>
              {(p) => <input {...p} inputMode="decimal" style={field.control} value={form.maxPerMonth} onChange={(e) => set('maxPerMonth', e.target.value)} />}
            </Field>
          </div>
          <p style={s.muted}>A run stops when it reaches its limit. When the day or month limit is reached, scheduled runs are skipped until the next day or month.</p>
        </fieldset>
        {failure && <p role="alert" style={field.error}>{failure}</p>}
        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
          <DSButton type="button" variant="ghost" onClick={onClose}>{readOnly ? 'Close' : 'Cancel'}</DSButton>
          {!readOnly && <DSButton type="submit" variant="primary" disabled={busy}>{busy ? 'Saving…' : isNew ? 'Create schedule' : 'Save changes'}</DSButton>}
        </div>
      </form>
    </Dialog>
  )
}

/** The banner of a schedule managed in Git: why it is read-only, Copy YAML and Duplicate. */
function GitOwned({ obj, onDone }: { obj: KubeObject; onDone: (message: string) => void }) {
  const [copyName, setCopyName] = useState(`${obj.metadata.name}-copy`)
  const [asking, setAsking] = useState(false)
  const [error, setError] = useState('')
  const yaml = stringifyYAML(writeBody(obj))
  const duplicate = async () => {
    try {
      await duplicateResource(SCHEDULE_KIND, obj.metadata.namespace ?? '', obj.metadata.name ?? '', copyName)
      onDone(`${copyName} was created. It is paused until you turn it on.`)
    } catch (e) { setError(describeScheduleError(e)) }
  }
  return (
    <div role="note" style={s.gitBox}>
      <p style={s.lead}>This schedule is managed in Git, so it cannot be changed here. Change it in Git, or make a copy that you can edit. To pause it, set <code>suspend: true</code> in Git.</p>
      <div style={s.row}>
        <CopyButton text={yaml} label="Copy YAML" />
        {!asking && <DSButton size="sm" variant="secondary" onClick={() => setAsking(true)}>Duplicate</DSButton>}
      </div>
      {asking && (
        <div style={s.row}>
          <input aria-label="Name for the copy" style={field.control} value={copyName} onChange={(e) => setCopyName(e.target.value)} />
          <DSButton size="sm" variant="primary" onClick={duplicate}>Make a copy</DSButton>
        </div>
      )}
      {error && <p role="alert" style={field.error}>{error}</p>}
    </div>
  )
}

const s: Record<string, CSSProperties> = {
  wrap: { padding: '12px 24px', display: 'flex', flexDirection: 'column', gap: 12, maxWidth: 900 },
  lead: { margin: 0, fontSize: 13, color: ds.textSecondary },
  muted: { margin: 0, fontSize: 12, color: ds.textMuted },
  warn: { margin: 0, fontSize: 12, color: ds.warning },
  ok: { margin: 0, fontSize: 12, color: ds.success },
  h3: { margin: '6px 0 0', fontSize: 13, fontWeight: 600, color: ds.textPrimary },
  list: { listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 10 },
  card: { border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, padding: 12, display: 'flex', flexDirection: 'column', gap: 8 },
  row: { display: 'flex', gap: 8, alignItems: 'flex-end', flexWrap: 'wrap' },
  facts: { display: 'flex', flexDirection: 'column', gap: 2, fontSize: 12, color: ds.textSecondary },
  histRow: { display: 'flex', gap: 12, fontSize: 12, flexWrap: 'wrap', alignItems: 'center' },
  fieldset: { border: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 12, minWidth: 0 },
  preview: { display: 'flex', flexDirection: 'column', gap: 4, minHeight: 20 },
  radio: { fontSize: 13, display: 'flex', gap: 6, alignItems: 'center' },
  gitBox: { border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, padding: 10, marginBottom: 10, display: 'flex', flexDirection: 'column', gap: 8 },
}
