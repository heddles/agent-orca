/**
 * Where a schedule's reports go. Only an admin may change it. A schedule managed
 * in Git is changed in Git, except that an admin can choose another Slack channel
 * here; the list and the editor say which of the two is in force.
 */
import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router'
import { listChatApps, listPlatformPlaces, type ChatApp, type PlatformPlace } from '../api/chatApps'
import {
  chooseDestination, describeScheduleError, resetDestination, type NotificationChannel, type ScheduleNotifyView, type ScheduleSummary, type SlackTarget,
} from '../api/schedules'
import { ds } from '../lib/designSystem'
import { Dialog } from '../lib/dialog'
import { DSButton } from '../lib/primitives'
import { ROUTES } from '../lib/routes'
import { Field, field } from './BuilderParts'

/** The value of the destination choice that stands for "a Slack channel through a chat app". */
export const VIA_SLACK = '__slack__'

/** Slack's id of a channel, as the API accepts it. */
export const SLACK_CHANNEL_ID = /^[CG][A-Z0-9]{2,30}$/

const slackLabel = (t: SlackTarget) =>
  `Slack channel ${t.channelName ? `${t.channelName} (${t.channelID})` : t.channelID}, through the app ${t.app}`

/** A destination in words. */
export function destinationText(n: { channel?: string; slack?: SlackTarget } | undefined): string {
  if (n?.slack) return slackLabel(n.slack)
  if (n?.channel) return `the notification channel ${n.channel}`
  return ''
}

/** What was chosen here, and what Git says instead. */
const overrideText = (o: NonNullable<ScheduleNotifyView['override']>) =>
  `changed here by ${o.setBy}. Git says ${destinationText(o.git)}.`

/** On a schedule's card: where its reports go, where that was decided, and any problem with it. */
export function DestinationLine({ notify, git }: { notify?: ScheduleNotifyView; git: boolean }) {
  return (
    <>
      <span>
        {notify ? <>Sends messages to <strong>{destinationText(notify)}</strong></> : 'Sends no messages: results are kept on the run'}
        {notify?.override ? <>, {overrideText(notify.override)}</> : git && (notify ? ' · set in Git' : ' · add messages in Git')}
      </span>
      {notify?.problem && <span style={{ color: ds.warning }}>Messages cannot be sent: {notify.problem}</span>}
    </>
  )
}

/**
 * Chooses another Slack channel for a schedule managed in Git, or goes back to
 * the one in Git. The schedule itself is not changed.
 */
export function ChangeChannelDialog({ schedule, onClose, onSaved }: {
  schedule: ScheduleSummary
  onClose: () => void
  onSaved: (message: string) => void
}) {
  const current = schedule.notify?.slack
  const override = schedule.notify?.override
  const [choice, setChoice] = useState<SlackChoice>({
    slackApp: current?.app ?? '', slackChannelID: current?.channelID ?? '', slackChannelName: current?.channelName ?? '',
  })
  const [error, setError] = useState('')
  const [failure, setFailure] = useState('')
  const [busy, setBusy] = useState(false)
  const change = useCallback((c: Partial<SlackChoice>) => { setError(''); setChoice((old) => ({ ...old, ...c })) }, [])

  const run = async (what: () => Promise<unknown>, done: string) => {
    setBusy(true)
    setFailure('')
    try { await what(); onSaved(done) } catch (e) { setFailure(describeScheduleError(e)); setBusy(false) }
  }
  const save = (e: React.FormEvent) => {
    e.preventDefault()
    const problem = slackProblem(choice)
    if (problem) { setError(problem); return }
    void run(() => chooseDestination(schedule.namespace, schedule.name, {
      app: choice.slackApp, channelID: choice.slackChannelID, ...(choice.slackChannelName ? { channelName: choice.slackChannelName } : {}),
    }), `${schedule.name} now sends its messages to ${choice.slackChannelName || choice.slackChannelID}.`)
  }

  return (
    <Dialog title={`Change channel for ${schedule.name}`} onClose={onClose} width={560}>
      <form onSubmit={save} noValidate style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
        <p style={{ margin: 0, fontSize: 13, color: ds.textSecondary }}>
          This schedule is managed in Git. Choosing a channel here changes only where its messages go. Git still decides
          everything else, including when messages are sent and whether they include the run's output. The file in Git is
          not changed, and your choice stays when Git syncs again, until you reset it.
        </p>
        {override && (
          <p role="note" style={{ margin: 0, fontSize: 13, color: ds.warning }}>
            Now sending to {destinationText(schedule.notify)}, {overrideText(override)}
          </p>
        )}
        <SlackPicker choice={choice} editable error={error} onChange={change} />
        <p style={{ margin: 0, fontSize: 12, color: ds.textMuted }}>Anyone who can read the channel can read what is sent to it.</p>
        {failure && <p role="alert" style={field.error}>{failure}</p>}
        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', flexWrap: 'wrap' }}>
          {override && (
            <DSButton type="button" variant="secondary" disabled={busy}
              onClick={() => run(() => resetDestination(schedule.namespace, schedule.name), `${schedule.name} sends its messages where Git says again.`)}>
              Reset to Git
            </DSButton>
          )}
          <DSButton type="button" variant="ghost" onClick={onClose}>Cancel</DSButton>
          <DSButton type="submit" variant="primary" disabled={busy}>{busy ? 'Saving…' : 'Save channel'}</DSButton>
        </div>
      </form>
    </Dialog>
  )
}

export interface SlackChoice {
  slackApp: string
  slackChannelID: string
  slackChannelName: string
}

/** What is missing from a choice of Slack channel; '' when it can be saved. */
export function slackProblem(c: SlackChoice): string {
  if (c.slackApp && SLACK_CHANNEL_ID.test(c.slackChannelID)) return ''
  return c.slackApp ? 'Choose a channel (its ID looks like C0123456789).' : 'Choose the Slack app and channel.'
}

/** The Slack app and one of its channels, by name when the app can list them. */
export function SlackPicker({ choice, editable, error, onChange }: {
  choice: SlackChoice
  editable: boolean
  error?: string
  onChange: (change: Partial<SlackChoice>) => void
}) {
  const [apps, setApps] = useState<ChatApp[] | null>(null)
  const [places, setPlaces] = useState<PlatformPlace[] | null>(null)
  const [placesError, setPlacesError] = useState('')

  useEffect(() => {
    if (!editable) return
    let live = true
    // No chat apps (or no database) leaves the list empty; the other choices work without it.
    listChatApps()
      .then((a) => { if (live) setApps(a.filter((x) => x.kind === 'slack')) })
      .catch(() => { if (live) setApps([]) })
    return () => { live = false }
  }, [editable])

  const app = apps?.find((a) => a.name === choice.slackApp)
  const appID = app?.id
  const canList = app?.canListPlaces ?? false
  useEffect(() => {
    setPlaces(null)
    setPlacesError('')
    if (!editable || !appID || !canList) return
    let live = true
    listPlatformPlaces(appID)
      .then((p) => { if (live) setPlaces(p ?? []) })
      .catch((e: unknown) => { if (live) setPlacesError(e instanceof Error ? e.message : String(e)) })
    return () => { live = false }
  }, [editable, appID, canList])

  // One app needs no choosing.
  useEffect(() => {
    if (editable && !choice.slackApp && apps?.length === 1) onChange({ slackApp: apps[0].name })
  }, [editable, choice.slackApp, apps, onChange])

  if (!editable) {
    return (
      <p style={{ margin: 0, fontSize: 13, color: ds.textSecondary }}>
        {slackLabel({ app: choice.slackApp, channelID: choice.slackChannelID, channelName: choice.slackChannelName })}
      </p>
    )
  }
  const known = places?.find((p) => p.id === choice.slackChannelID)
  return (
    <>
      {apps?.length === 0 && !choice.slackApp && (
        <p style={{ margin: 0, fontSize: 12, color: ds.textMuted }}>
          No Slack app is connected yet. Connect one under <Link to={ROUTES.chatApps} style={{ color: ds.accent }}>Settings, Chat apps</Link>.
        </p>
      )}
      <Field label="Slack app">
        {(p) => (
          <select {...p} style={field.control} value={choice.slackApp}
            onChange={(e) => onChange({ slackApp: e.target.value, slackChannelID: '', slackChannelName: '' })}>
            <option value="">Choose an app…</option>
            {choice.slackApp && !app && <option value={choice.slackApp}>{choice.slackApp}</option>}
            {(apps ?? []).map((a) => <option key={a.id} value={a.name}>{a.name}</option>)}
          </select>
        )}
      </Field>
      {places && places.length > 0 ? (
        <Field label="Slack channel" error={error} help="The app can post only in channels it has been invited to.">
          {(p) => (
            <select {...p} style={field.control} value={choice.slackChannelID} onChange={(e) => {
              const place = places.find((x) => x.id === e.target.value)
              onChange({ slackChannelID: e.target.value, slackChannelName: place?.name ?? '' })
            }}>
              <option value="">Choose a channel…</option>
              {choice.slackChannelID && !known && <option value={choice.slackChannelID}>{choice.slackChannelName || choice.slackChannelID}</option>}
              {places.map((x) => <option key={x.id} value={x.id}>{x.name}{x.member ? '' : ' (invite the app first)'}</option>)}
            </select>
          )}
        </Field>
      ) : (
        <Field label="Slack channel ID" error={error}
          help="In Slack, open the channel's details: the ID is at the bottom and looks like C0123456789. Invite the app to the channel.">
          {(p) => <input {...p} style={field.control} value={choice.slackChannelID}
            onChange={(e) => onChange({ slackChannelID: e.target.value.trim(), slackChannelName: '' })} />}
        </Field>
      )}
      {known && !known.member && <p role="alert" style={field.error}>The app is not in {known.name}. Invite it there, or messages will not arrive.</p>}
      {placesError && <p role="alert" style={field.error}>{placesError}</p>}
    </>
  )
}

export interface DestinationForm extends SlackChoice {
  /** '' for nowhere, VIA_SLACK, or the name of a TenantConfig channel. */
  notifyChannel: string
}

/** The choice of destination, and of the Slack app and channel when that is it. Admins only change it. */
export function DestinationFields({ form, channels, editable, error, onChange }: {
  form: DestinationForm
  channels: NotificationChannel[]
  editable: boolean
  error?: string
  onChange: (change: Partial<DestinationForm>) => void
}) {
  const viaSlack = form.notifyChannel === VIA_SLACK
  const stored = form.notifyChannel && !viaSlack && !channels.some((c) => c.name === form.notifyChannel)
  return (
    <>
      <Field label="Also send a message to">
        {(p) => (
          <select {...p} style={field.control} value={form.notifyChannel} onChange={(e) => onChange({ notifyChannel: e.target.value })}>
            <option value="">No one: only keep it on the run</option>
            {(editable || viaSlack) && <option value={VIA_SLACK}>A Slack channel, through a connected app</option>}
            {stored && <option value={form.notifyChannel}>{form.notifyChannel}</option>}
            {channels.map((c) => <option key={c.name} value={c.name}>{c.name} ({c.type})</option>)}
          </select>
        )}
      </Field>
      {viaSlack && <SlackPicker choice={form} editable={editable} error={error} onChange={onChange} />}
    </>
  )
}
