/**
 * MyWork — Home. What the signed-in user has running or finished, anything
 * waiting on them (agent questions and approvals), and the outputs, with
 * feedback thumbs. Runs are the user's own when they record who started them;
 * otherwise the team's recent runs are shown and the page says so.
 */
import { useCallback, useEffect, useState, type CSSProperties } from 'react'
import { Link, useNavigate } from 'react-router'
import { answerClarification } from '../api/sse'
import { needsConnection } from '../api/connections'
import { getMyRuns, type MyRun, type MyRuns } from '../api/catalog'
import { DSButton } from '../lib/primitives'
import { ds, DESIGN } from '../lib/designSystem'
import { ICON } from '../lib/icons'
import { EmptyState, Section } from '../lib/layout'
import { liveRunPath, ROUTES, agentDetailsPath } from '../lib/routes'
import { useNamespace } from '../lib/namespace'
import { ApprovalsView } from './ApprovalsView'
import { timeAgo } from '../lib/time'
import { OutputCard } from './OutputCard'
import { StatusBadge } from './StatusBadge'
import { channelLabel, parseRunInput, plainPreview, runTitle } from '../lib/runText'

const ACTIVE = new Set(['Pending', 'Running', 'WaitingForInput'])
const POLL_MS = 5000

/** Plain-language status for a run's phase. */
export function phaseText(phase: string): string {
  switch (phase) {
    case 'Succeeded': return 'Done'
    case 'Failed': return 'Failed'
    case 'Running': return 'Working'
    case 'Pending': return 'Starting'
    case 'WaitingForInput': return 'Waiting for you'
    case 'HandedOff': return 'Passed to another agent'
    default: return phase || 'Unknown'
  }
}

export function MyWork() {
  const { namespace } = useNamespace()
  const [data, setData] = useState<MyRuns | null>(null)
  const [error, setError] = useState<string | null>(null)
  const navigate = useNavigate()
  const [pendingApprovals, setPendingApprovals] = useState<number | null>(null)

  const load = useCallback(() => {
    getMyRuns(namespace)
      .then((d) => { setData(d); setError(null) })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)))
  }, [namespace])

  useEffect(() => {
    load()
    const id = setInterval(load, POLL_MS)
    return () => clearInterval(id)
  }, [load])

  const questions = (data?.runs ?? []).filter((r) => r.phase === 'WaitingForInput' && r.clarifyQuestion)
  const nothingWaiting = questions.length === 0 && pendingApprovals === 0

  return (
    <div>
      <Section title="Waiting for you">
        {questions.length > 0 && (
          <ul style={s.list} aria-label="Questions from agents">
            {questions.map((r) => <li key={`${r.namespace}/${r.name}`}><QuestionCard run={r} onAnswered={load} /></li>)}
          </ul>
        )}
        <div style={{ marginTop: questions.length > 0 ? 12 : 0 }}>
          <ApprovalsView embedded onPendingChange={setPendingApprovals} onNavigateToRun={(runName, ns) => navigate(liveRunPath(ns, runName))} />
        </div>
        {data && nothingWaiting && (
          <p style={s.quiet}>Nothing is waiting for you. Questions from agents and requests for approval show up here.</p>
        )}
      </Section>

      <Section title="Your recent runs">
        {error && <div role="alert" style={s.error}>Could not load your runs: {error}</div>}
        {!data && !error && <div role="status" style={s.quiet}>Loading your runs…</div>}
        {data && data.runs.length === 0 && (
          <EmptyState
            icon={ICON.runs}
            title="You have not run anything yet"
            action={<Link to={ROUTES.agents} style={s.link}>Browse agents</Link>}
          >
            Pick an agent, fill in what you want done, and the result will appear here.
          </EmptyState>
        )}
        {data && data.runs.length > 0 && (
          <>
            <p role="status" style={s.scope}>
              {data.scope === 'user'
                ? 'Runs you started.'
                : 'Showing your team’s recent runs. Runs you start from the agent page will be listed as yours.'}
            </p>
            <ul style={s.list} aria-label="Recent runs">
              {data.runs.map((r) => <li key={`${r.namespace}/${r.name}`}><RunCard run={r} /></li>)}
            </ul>
          </>
        )}
      </Section>
    </div>
  )
}

function QuestionCard({ run, onAnswered }: { run: MyRun; onAnswered: () => void }) {
  const [answer, setAnswer] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const id = `answer-${run.namespace}-${run.name}`

  const send = async (e: React.FormEvent) => {
    e.preventDefault()
    if (answer.trim() === '') { setError('Type an answer first.'); return }
    setBusy(true)
    setError(null)
    try {
      await answerClarification(run.name, answer.trim(), run.namespace)
      setAnswer('')
      onAnswered()
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={send} style={s.card} aria-label={`Question from ${run.agentRef}`}>
      <div style={s.cardHead}>
        <strong>{run.agentRef} has a question</strong>
        <Link to={liveRunPath(run.namespace, run.name)} style={s.link}>Open run</Link>
      </div>
      <p style={s.question}>{run.clarifyQuestion}</p>
      {needsConnection(run.clarifyQuestion) && <p style={s.question}><Link to={ROUTES.myConnections} style={s.link}>Connect</Link>, then answer here to continue.</p>}
      <label htmlFor={id} style={s.srOnly}>Your answer to {run.agentRef}</label>
      <textarea
        id={id}
        style={s.textarea}
        value={answer}
        aria-invalid={error ? true : undefined}
        onChange={(e) => setAnswer(e.target.value)}
      />
      {error && <p role="alert" style={s.errorText}>{error}</p>}
      <div><DSButton type="submit" variant="primary" size="sm" disabled={busy}>{busy ? 'Sending…' : 'Send answer'}</DSButton></div>
    </form>
  )
}

function RunCard({ run }: { run: MyRun }) {
  const [open, setOpen] = useState(false)
  const finished = !ACTIVE.has(run.phase)
  const panelId = `run-${run.namespace}-${run.name}-output`
  const cost = Number(run.spendUSD)
  const channel = parseRunInput(run.input).channel
  const preview = run.output ? plainPreview(run.output.slice(0, 600)) : ''
  return (
    <div style={s.card}>
      <div style={s.cardHead}>
        <Link to={liveRunPath(run.namespace, run.name)} style={s.title}>{runTitle(run.input, run.name)}</Link>
        <span style={s.meta}>
          {run.spendUSD && Number.isFinite(cost) ? `$${cost.toFixed(2)} · ` : ''}
          {timeAgo(run.completionTime ?? run.createdAt)}
        </span>
      </div>
      <div style={s.tags}>
        <StatusBadge phase={run.phase} pulse={ACTIVE.has(run.phase)} />
        <span style={s.statusText}>{phaseText(run.phase)}</span>
        <Link to={agentDetailsPath(run.namespace, run.agentRef)} style={s.agent}>{run.agentRef}</Link>
        {channel && <span style={s.statusText}>from {channelLabel(channel)}</span>}
      </div>
      {preview && !open && <p style={s.preview}>{preview}</p>}
      <div style={s.cardFoot}>
        {run.output && (
          <button type="button" style={s.toggle} aria-expanded={open} aria-controls={panelId} onClick={() => setOpen((o) => !o)}>
            {open ? 'Hide result' : 'Show result'}
          </button>
        )}
        <Link to={liveRunPath(run.namespace, run.name)} style={s.link}>Open run</Link>
      </div>
      {open && run.output && (
        <div id={panelId}>
          <OutputCard
            output={run.output}
            markdown
            feedbackRun={finished ? { namespace: run.namespace, runName: run.name } : undefined}
          />
        </div>
      )}
    </div>
  )
}

const s: Record<string, CSSProperties> = {
  list: { listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 10, maxWidth: 860 },
  card: {
    display: 'flex', flexDirection: 'column', gap: 8, padding: 14, background: ds.surface,
    border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.xl, boxShadow: 'var(--ds-card-shadow)',
  },
  cardHead: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 8, flexWrap: 'wrap', fontSize: 13, color: ds.textPrimary },
  cardFoot: { display: 'flex', gap: 14, alignItems: 'center' },
  title: { fontWeight: 600, color: ds.textPrimary, textDecoration: 'none', fontSize: 14, flex: '1 1 0', minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' },
  tags: { display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' },
  agent: { color: ds.textSecondary, textDecoration: 'underline', fontSize: 12 },
  statusText: { fontSize: 12, color: ds.textSecondary },
  meta: { fontSize: 11, color: ds.textMuted, fontVariantNumeric: 'tabular-nums' },
  preview: { margin: 0, fontSize: 13, color: ds.textSecondary, lineHeight: 1.45, overflowWrap: 'anywhere', display: '-webkit-box', WebkitLineClamp: 2, WebkitBoxOrient: 'vertical', overflow: 'hidden' },
  question: { margin: 0, fontSize: 14, color: ds.textPrimary },
  link: { color: ds.accent, fontSize: 12, textDecoration: 'none' },
  toggle: { background: 'transparent', border: 'none', color: ds.accent, fontSize: 12, cursor: 'pointer', padding: 0 },
  quiet: { margin: 0, fontSize: 13, color: ds.textMuted },
  scope: { margin: '0 0 10px', fontSize: 12, color: ds.textMuted },
  error: { padding: 12, color: ds.error, fontSize: 13 },
  errorText: { margin: 0, color: ds.error, fontSize: 12 },
  textarea: {
    background: ds.surface, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, padding: '8px 10px',
    color: ds.textPrimary, fontSize: 13, minHeight: 64, width: '100%', boxSizing: 'border-box', resize: 'vertical',
  },
  srOnly: { position: 'absolute', width: 1, height: 1, overflow: 'hidden', clip: 'rect(0 0 0 0)', whiteSpace: 'nowrap' },
}
