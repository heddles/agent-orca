/**
 * Where a schedule's reports go. Only an admin may change it. A schedule managed
 * in Git is changed in Git, and both the list and the editor say so.
 */
import { useEffect, useState } from 'react'
import { Link } from 'react-router'
import { listChatApps, listPlatformPlaces, type ChatApp, type PlatformPlace } from '../api/chatApps'
import type { NotificationChannel, ScheduleNotifyView, SlackTarget } from '../api/schedules'
import { ds } from '../lib/designSystem'
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

/** On a schedule's card: where its reports go, where that is changed, and any problem with it. */
export function DestinationLine({ notify, git }: { notify?: ScheduleNotifyView; git: boolean }) {
  return (
    <>
      <span>
        {notify ? <>Sends messages to <strong>{destinationText(notify)}</strong></> : 'Sends no messages: results are kept on the run'}
        {git && ' · set in Git, change it there'}
      </span>
      {notify?.problem && <span style={{ color: ds.warning }}>Messages cannot be sent: {notify.problem}</span>}
    </>
  )
}

export interface DestinationForm {
  /** '' for nowhere, VIA_SLACK, or the name of a TenantConfig channel. */
  notifyChannel: string
  slackApp: string
  slackChannelID: string
  slackChannelName: string
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

  const app = apps?.find((a) => a.name === form.slackApp)
  const appID = app?.id
  const canList = app?.canListPlaces ?? false
  useEffect(() => {
    setPlaces(null)
    setPlacesError('')
    if (!editable || !viaSlack || !appID || !canList) return
    let live = true
    listPlatformPlaces(appID)
      .then((p) => { if (live) setPlaces(p ?? []) })
      .catch((e: unknown) => { if (live) setPlacesError(e instanceof Error ? e.message : String(e)) })
    return () => { live = false }
  }, [editable, viaSlack, appID, canList])

  // One app needs no choosing.
  useEffect(() => {
    if (editable && viaSlack && !form.slackApp && apps?.length === 1) onChange({ slackApp: apps[0].name })
  }, [editable, viaSlack, form.slackApp, apps, onChange])

  const known = places?.find((p) => p.id === form.slackChannelID)
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
      {viaSlack && !editable && (
        <p style={{ margin: 0, fontSize: 13, color: ds.textSecondary }}>
          {slackLabel({ app: form.slackApp, channelID: form.slackChannelID, channelName: form.slackChannelName })}
        </p>
      )}
      {viaSlack && editable && (
        <>
          {apps?.length === 0 && !form.slackApp && (
            <p style={{ margin: 0, fontSize: 12, color: ds.textMuted }}>
              No Slack app is connected yet. Connect one under <Link to={ROUTES.chatApps} style={{ color: ds.accent }}>Settings, Chat apps</Link>.
            </p>
          )}
          <Field label="Slack app">
            {(p) => (
              <select {...p} style={field.control} value={form.slackApp}
                onChange={(e) => onChange({ slackApp: e.target.value, slackChannelID: '', slackChannelName: '' })}>
                <option value="">Choose an app…</option>
                {form.slackApp && !app && <option value={form.slackApp}>{form.slackApp}</option>}
                {(apps ?? []).map((a) => <option key={a.id} value={a.name}>{a.name}</option>)}
              </select>
            )}
          </Field>
          {places && places.length > 0 ? (
            <Field label="Slack channel" error={error} help="The app can post only in channels it has been invited to.">
              {(p) => (
                <select {...p} style={field.control} value={form.slackChannelID} onChange={(e) => {
                  const place = places.find((x) => x.id === e.target.value)
                  onChange({ slackChannelID: e.target.value, slackChannelName: place?.name ?? '' })
                }}>
                  <option value="">Choose a channel…</option>
                  {form.slackChannelID && !known && <option value={form.slackChannelID}>{form.slackChannelName || form.slackChannelID}</option>}
                  {places.map((x) => <option key={x.id} value={x.id}>{x.name}{x.member ? '' : ' (invite the app first)'}</option>)}
                </select>
              )}
            </Field>
          ) : (
            <Field label="Slack channel ID" error={error}
              help="In Slack, open the channel's details: the ID is at the bottom and looks like C0123456789. Invite the app to the channel.">
              {(p) => <input {...p} style={field.control} value={form.slackChannelID}
                onChange={(e) => onChange({ slackChannelID: e.target.value.trim(), slackChannelName: '' })} />}
            </Field>
          )}
          {known && !known.member && <p role="alert" style={field.error}>The app is not in {known.name}. Invite it there, or messages will not arrive.</p>}
          {placesError && <p role="alert" style={field.error}>{placesError}</p>}
        </>
      )}
    </>
  )
}
