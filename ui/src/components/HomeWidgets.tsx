/**
 * The Home widgets that are not about the signed-in user's own runs (those are
 * in MyWork.tsx): ask an agent, latest scheduled reports, problems, upcoming
 * schedules and agent health. Each loads its own data, so Home only fetches for
 * the widgets a person has.
 */
import { useCallback, useEffect, useState, type CSSProperties } from 'react'
import { Link, useNavigate } from 'react-router'
import { getAgentManifest, getCatalog, type AgentManifest, type CatalogEntry } from '../api/catalog'
import { getFleet, type Fleet, type FleetHealthStatus } from '../api/fleet'
import { listSchedules, type ScheduleSummary } from '../api/schedules'
import { getRun, getRunHistoryDetail, listRuns, type AgentRunSummary } from '../api/sse'
import { DESIGN, ds } from '../lib/designSystem'
import { agentDetailsPath, archivedRunPath, liveRunPath, ROUTES } from '../lib/routes'
import { useNamespace } from '../lib/namespace'
import { plainPreview, runTitle } from '../lib/runText'
import { timeAgo } from '../lib/time'
import { RunForm } from './AgentPage'
import { RequiredConnections, useInvokerIdentities, useOwnerNeedsReconnect, useRequiredConnections } from './RequiredConnections'
import { OutputCard } from './OutputCard'
import { StatusBadge } from './StatusBadge'

const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

/** Loads on mount and then every `everyMs`, keeping the last good data. */
function usePolled<T>(load: () => Promise<T>, everyMs = 30000): { data: T | null; error: string | null } {
  const [state, setState] = useState<{ data: T | null; error: string | null }>({ data: null, error: null })
  useEffect(() => {
    let live = true
    setState({ data: null, error: null })
    const tick = () => {
      load()
        .then((data) => { if (live) setState({ data, error: null }) })
        .catch((e: unknown) => { if (live) setState((s) => ({ data: s.data, error: message(e) })) })
    }
    tick()
    const id = setInterval(tick, everyMs)
    return () => { live = false; clearInterval(id) }
  }, [load, everyMs])
  return state
}

function Status({ data, error, what }: { data: unknown; error: string | null; what: string }) {
  if (error) return <p role="alert" style={w.error}>Could not load {what}: {error}</p>
  return data === null ? <p role="status" style={w.quiet}>Loading {what}…</p> : null
}

function useSchedules() {
  const { namespace } = useNamespace()
  return usePolled<ScheduleSummary[]>(useCallback(() => listSchedules(namespace), [namespace]))
}

function when(iso: string): string {
  return new Date(iso).toLocaleString(undefined, { weekday: 'short', hour: 'numeric', minute: '2-digit' })
}

// ── Ask an agent ─────────────────────────────────────────────────────────────

export function AskWidget() {
  const { namespace } = useNamespace()
  const { data, error } = usePolled<CatalogEntry[]>(useCallback(() => getCatalog(namespace), [namespace]), 120000)
  const [chosen, setChosen] = useState('')
  const agents = (data ?? []).filter((a) => !a.template)
    .sort((a, b) => Number(b.published) - Number(a.published) || a.name.localeCompare(b.name))
  const agent = agents.find((a) => `${a.namespace}/${a.name}` === chosen) ?? agents[0]

  if (!agent) {
    return data === null || error
      ? <Status data={data} error={error} what="agents" />
      : <p style={w.quiet}>There are no agents to ask yet. <Link to={ROUTES.agents} style={w.link}>Browse agents</Link></p>
  }
  const id = `${agent.namespace}/${agent.name}`
  return (
    <div style={w.stack}>
      <label style={w.pick}>
        Agent
        <select value={id} onChange={(e) => setChosen(e.target.value)} style={w.select}>
          {agents.map((a) => (
            <option key={`${a.namespace}/${a.name}`} value={`${a.namespace}/${a.name}`}>
              {a.name}{agents.some((o) => o !== a && o.name === a.name) ? ` (${a.namespace})` : ''}
            </option>
          ))}
        </select>
      </label>
      <AskForm key={id} namespace={agent.namespace} name={agent.name} fallbackExamples={agent.exampleInputs} />
    </div>
  )
}

function AskForm({ namespace, name, fallbackExamples }: { namespace: string; name: string; fallbackExamples: string[] }) {
  const navigate = useNavigate()
  const [manifest, setManifest] = useState<AgentManifest | null>(null)
  const [error, setError] = useState<string | null>(null)
  const required = useRequiredConnections(namespace, name)
  const ownerNeedsReconnect = useOwnerNeedsReconnect(namespace, name)
  const identities = useInvokerIdentities(namespace, name)

  useEffect(() => {
    let live = true
    getAgentManifest(namespace, name)
      .then((m) => { if (live) setManifest(m) })
      .catch((e: unknown) => { if (live) setError(message(e)) })
    return () => { live = false }
  }, [namespace, name])

  if (!manifest) return <Status data={manifest} error={error} what={name} />
  return (
    <>
      <RequiredConnections required={required} identities={identities} ownerNeedsReconnect={ownerNeedsReconnect} />
      <RunForm
        namespace={namespace}
        name={name}
        blocked={required.length > 0}
        schema={manifest.input_schema}
        examples={manifest.example_inputs ?? fallbackExamples}
        onStarted={(run) => navigate(liveRunPath(namespace, run))}
      />
    </>
  )
}

// ── Latest scheduled reports ─────────────────────────────────────────────────

const REPORTS_SHOWN = 3

interface Report { title: string; output: string; path: string }

/** The run behind a schedule's last result; it may have moved to the archive. */
async function loadReport(namespace: string, run: string): Promise<Report> {
  try {
    const d = await getRun(run, namespace)
    return { title: runTitle(d.input, run), output: d.output ?? '', path: liveRunPath(namespace, run) }
  } catch {
    const d = await getRunHistoryDetail(namespace, run)
    return { title: runTitle(d.input, run), output: d.output ?? '', path: archivedRunPath(namespace, run) }
  }
}

export function ReportsWidget() {
  const { data, error } = useSchedules()
  const latest = (data ?? [])
    .filter((s) => s.targetKind === 'agent' && s.lastResult.run)
    .sort((a, b) => (b.lastResult.at ?? '').localeCompare(a.lastResult.at ?? ''))
    .slice(0, REPORTS_SHOWN)
  return (
    <div style={w.stack}>
      <Status data={data} error={error} what="schedules" />
      {data && latest.length === 0 && (
        <p style={w.quiet}>No scheduled run has finished yet. <Link to={ROUTES.schedules} style={w.link}>Manage schedules</Link></p>
      )}
      {latest.length > 0 && (
        <ul style={w.list} aria-label="Latest scheduled reports">
          {latest.map((s) => <li key={`${s.namespace}/${s.name}`} style={w.item}><ReportRow schedule={s} /></li>)}
        </ul>
      )}
    </div>
  )
}

function ReportRow({ schedule: s }: { schedule: ScheduleSummary }) {
  const run = s.lastResult.run as string
  const [report, setReport] = useState<Report | null>(null)
  const [missing, setMissing] = useState(false)
  const [open, setOpen] = useState(false)

  useEffect(() => {
    let live = true
    setReport(null)
    setMissing(false)
    loadReport(s.namespace, run)
      .then((r) => { if (live) setReport(r) })
      .catch(() => { if (live) setMissing(true) })
    return () => { live = false }
  }, [s.namespace, run])

  const phase = s.lastResult.status === 'succeeded' ? 'Succeeded' : s.lastResult.status === 'failed' ? 'Failed' : ''
  return (
    <>
      <div style={w.row}>
        <strong style={w.name}>{s.name}</strong>
        {phase && <StatusBadge phase={phase} />}
        <span style={w.meta}>{timeAgo(s.lastResult.at)}</span>
      </div>
      {report?.output && !open && <p style={w.preview}>{plainPreview(report.output.slice(0, 600))}</p>}
      {!report?.output && s.lastResult.reason && <p style={w.preview}>{s.lastResult.reason}</p>}
      {missing && <p style={w.quiet}>The run is no longer available.</p>}
      {report && (
        <div style={w.row}>
          {report.output && (
            <button type="button" style={w.textBtn} aria-expanded={open} onClick={() => setOpen((o) => !o)}>
              {open ? 'Hide report' : 'Read report'}
            </button>
          )}
          <Link to={report.path} style={w.link}>Open run</Link>
        </div>
      )}
      {open && report?.output && <OutputCard output={report.output} markdown />}
    </>
  )
}

// ── Problems ─────────────────────────────────────────────────────────────────

const DAY_MS = 24 * 3600 * 1000
const BROKEN_SCHEDULE: Partial<Record<ScheduleSummary['state'], string>> = {
  degraded: 'keeps failing',
  budgetExhausted: 'has used up its budget',
  targetMissing: 'points at something that no longer exists',
  invalid: 'is not valid',
}

export function ProblemsWidget() {
  const { namespace } = useNamespace()
  const runs = usePolled<AgentRunSummary[]>(useCallback(() => listRuns(namespace), [namespace]))
  const schedules = useSchedules()
  const since = Date.now() - DAY_MS
  const failed = (runs.data ?? [])
    .filter((r) => r.phase === 'Failed' && new Date(r.startTime ?? 0).getTime() >= since)
    .sort((a, b) => (b.startTime ?? '').localeCompare(a.startTime ?? ''))
  const broken = (schedules.data ?? []).filter((s) => BROKEN_SCHEDULE[s.state])
  const loaded = runs.data !== null && schedules.data !== null
  return (
    <div style={w.stack}>
      <Status data={runs.data} error={runs.error} what="runs" />
      <Status data={schedules.data} error={schedules.error} what="schedules" />
      {loaded && !runs.error && !schedules.error && failed.length === 0 && broken.length === 0 && (
        <p style={w.quiet}>No failed runs in the last 24 hours, and every schedule is in order.</p>
      )}
      {(failed.length > 0 || broken.length > 0) && (
        <ul style={w.list} aria-label="Problems">
          {broken.map((s) => (
            <li key={`s/${s.namespace}/${s.name}`} style={w.item}>
              <Link to={ROUTES.schedules} style={w.name}>Schedule {s.name} {BROKEN_SCHEDULE[s.state]}</Link>
              {s.stateReason && <p style={w.preview}>{s.stateReason}</p>}
            </li>
          ))}
          {failed.slice(0, 5).map((r) => (
            <li key={`r/${r.namespace}/${r.name}`} style={w.item}>
              <Link to={liveRunPath(r.namespace, r.name)} style={w.clamp}>{runTitle(r.input, r.name)}</Link>
              <span style={w.meta}>{r.agentRef} failed {timeAgo(r.startTime)}</span>
            </li>
          ))}
        </ul>
      )}
      {failed.length > 5 && <Link to={ROUTES.runs} style={w.link}>{failed.length - 5} more failed runs</Link>}
    </div>
  )
}

// ── Coming up ────────────────────────────────────────────────────────────────

export function UpcomingWidget() {
  const { data, error } = useSchedules()
  const next = (data ?? [])
    .filter((s) => !s.suspended && s.nextRun)
    .sort((a, b) => (a.nextRun as string).localeCompare(b.nextRun as string))
    .slice(0, 5)
  return (
    <div style={w.stack}>
      <Status data={data} error={error} what="schedules" />
      {data && next.length === 0 && <p style={w.quiet}>Nothing is scheduled.</p>}
      {next.length > 0 && (
        <ul style={w.list} aria-label="Upcoming scheduled runs">
          {next.map((s) => (
            <li key={`${s.namespace}/${s.name}`} style={w.item}>
              <div style={w.row}>
                <strong style={w.name}>{s.name}</strong>
                <span style={w.meta}>{when(s.nextRun as string)}</span>
              </div>
              <span style={w.meta}>
                {s.targetKind === 'agent'
                  ? <Link to={agentDetailsPath(s.namespace, s.target)} style={w.link}>{s.target}</Link>
                  : s.target}
              </span>
            </li>
          ))}
        </ul>
      )}
      <Link to={ROUTES.schedules} style={w.link}>All schedules</Link>
    </div>
  )
}

// ── Agent health ─────────────────────────────────────────────────────────────

const HEALTH_ORDER: FleetHealthStatus[] = ['failing', 'degraded', 'unknown', 'healthy']
const HEALTH_COLOR: Record<FleetHealthStatus, string> = {
  failing: 'var(--ds-error)', degraded: 'var(--ds-warning)', unknown: 'var(--ds-text-muted)', healthy: 'var(--ds-success)',
}

export function AgentHealthWidget() {
  const { namespace } = useNamespace()
  const { data, error } = usePolled<Fleet>(useCallback(() => getFleet('24h', namespace), [namespace]), 60000)
  const rows = [...(data?.agents ?? [])]
    .sort((a, b) => HEALTH_ORDER.indexOf(a.health.status) - HEALTH_ORDER.indexOf(b.health.status))
  return (
    <div style={w.stack}>
      <Status data={data} error={error} what="agent health" />
      {data && rows.length === 0 && <p style={w.quiet}>No agents yet.</p>}
      {rows.length > 0 && (
        <ul style={w.list} aria-label="Agent health, last 24 hours">
          {rows.slice(0, 8).map((a) => (
            <li key={`${a.namespace}/${a.name}`} style={w.item}>
              <div style={w.row}>
                <Link to={agentDetailsPath(a.namespace, a.name)} style={w.name}>{a.name}</Link>
                <span style={{ ...w.meta, color: HEALTH_COLOR[a.health.status] }}>{a.health.status}</span>
              </div>
              <span style={w.meta}>
                {a.runCount} run{a.runCount === 1 ? '' : 's'}{a.health.reason ? ` · ${a.health.reason}` : ''}
              </span>
            </li>
          ))}
        </ul>
      )}
      <Link to={ROUTES.fleet} style={w.link}>Fleet overview</Link>
    </div>
  )
}

const w: Record<string, CSSProperties> = {
  stack: { display: 'flex', flexDirection: 'column', gap: 10, minWidth: 0 },
  list: { listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column' },
  item: { display: 'flex', flexDirection: 'column', gap: 4, padding: '10px 0', borderTop: '1px solid var(--ds-border)', minWidth: 0 },
  row: { display: 'flex', alignItems: 'center', gap: 10, minWidth: 0 },
  name: { flex: '1 1 0', minWidth: 0, fontSize: 13, fontWeight: 600, color: ds.textPrimary, textDecoration: 'none', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' },
  clamp: { fontSize: 13, color: ds.textPrimary, textDecoration: 'none', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' },
  meta: { fontSize: 11, color: ds.textMuted, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' },
  preview: { margin: 0, fontSize: 12, color: ds.textSecondary, lineHeight: 1.45, overflowWrap: 'anywhere', display: '-webkit-box', WebkitLineClamp: 2, WebkitBoxOrient: 'vertical', overflow: 'hidden' },
  link: { color: ds.accent, fontSize: 12, textDecoration: 'none' },
  textBtn: { background: 'transparent', border: 'none', color: ds.accent, fontSize: 12, cursor: 'pointer', padding: 0 },
  quiet: { margin: 0, fontSize: 13, color: ds.textMuted },
  error: { margin: 0, fontSize: 13, color: ds.error },
  pick: { display: 'flex', alignItems: 'center', gap: 8, fontSize: 12, fontWeight: 600, color: ds.textPrimary },
  select: { height: 32, minWidth: 200, borderRadius: DESIGN.radii.md, border: '1px solid var(--ds-border)', background: ds.bg, color: ds.textPrimary, fontSize: 13, padding: '0 8px' },
}
