/**
 * RevisionHistory: the History tab of the builder. Lists the versions of a
 * UI-owned resource, shows what an agent was at a version, compares two
 * versions, and restores one. Needs PostgreSQL on the server; says so when it
 * is missing.
 */
import { useCallback, useEffect, useId, useMemo, useRef, useState, type CSSProperties } from 'react'
import { ApiError, type KubeObject } from '../api/builder'
import {
  getRevision, listRevisions, restoreRevision, type RevisionDetail, type RevisionSummary,
} from '../api/revisions'
import { DESIGN, ds } from '../lib/designSystem'
import { Dialog } from '../lib/dialog'
import { diffLines } from '../lib/lineDiff'
import { DSButton } from '../lib/primitives'
import { agentChanges, FIELDS, fieldValues, yamlOf, type FieldChange } from '../lib/revisionDiff'
import { formFromObject } from '../lib/agentForm'
import { field } from './BuilderParts'

const PAGE_SIZE = 20
const CURRENT = 'current' as const
type Side = number | typeof CURRENT

interface Props {
  kind: string
  namespace: string
  name: string
  /** Agents get the field-by-field views; other kinds get YAML only. */
  isAgent: boolean
  /** Changes when something was saved, published or restored, so the list reloads. */
  refreshKey: number
  /** The object as last loaded: the "current" side of a comparison and the source of the live resourceVersion. */
  live: KubeObject | null
  /** Whether Restore is offered. */
  canRestore: boolean
  /** One line that says why history is read-only, when it is. */
  readOnlyNote: string
  /** The form has edits that are not saved; a restore would replace them. */
  dirty: boolean
  /** Reports the newest version number once the list loads (null when there is none or it is not available). */
  onLoaded: (latest: number | null) => void
  onRestored: (obj: KubeObject, from: number) => void
  /** Reloads the live object, so the next restore uses its resourceVersion. */
  onReload: () => Promise<void>
}

type ListState =
  | { state: 'loading' }
  | { state: 'ready' }
  | { state: 'unsupported' }
  | { state: 'error'; message: string }

// ── Wording ──────────────────────────────────────────────────────────────────

/** "2 hours ago", from an RFC 3339 time. */
export function relativeTime(iso: string, now = Date.now()): string {
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return iso
  const secs = Math.round((t - now) / 1000)
  const abs = Math.abs(secs)
  const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' })
  if (abs < 45) return 'just now'
  if (abs < 3600) return rtf.format(Math.round(secs / 60), 'minute')
  if (abs < 86400) return rtf.format(Math.round(secs / 3600), 'hour')
  if (abs < 86400 * 30) return rtf.format(Math.round(secs / 86400), 'day')
  if (abs < 86400 * 365) return rtf.format(Math.round(secs / (86400 * 30)), 'month')
  return rtf.format(Math.round(secs / (86400 * 365)), 'year')
}

/** What the version did, in a sentence: the server's summary when it has one. */
export function describeRevision(r: RevisionSummary): string {
  if (r.summary) return r.summary
  switch (r.action) {
    case 'create': return 'Created'
    case 'update': return 'Saved a change'
    case 'restore': return r.restoredFrom ? `Restored version ${r.restoredFrom}` : 'Restored an earlier version'
    case 'delete': return 'Deleted'
    default: return 'Changed'
  }
}

function Time({ iso }: { iso: string }) {
  const full = new Date(iso).toLocaleString()
  return (
    <time dateTime={iso} title={full}>
      {relativeTime(iso)}
      <span style={srOnly}> ({full})</span>
    </time>
  )
}

function messageOf(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

// ── The component ────────────────────────────────────────────────────────────

export function RevisionHistory({
  kind, namespace, name, isAgent, refreshKey, live, canRestore, readOnlyNote, dirty, onLoaded, onRestored, onReload,
}: Props) {
  const [items, setItems] = useState<RevisionSummary[]>([])
  const [next, setNext] = useState('')
  const [list, setList] = useState<ListState>({ state: 'loading' })
  const [more, setMore] = useState<{ busy: boolean; error: string }>({ busy: false, error: '' })
  const [selected, setSelected] = useState<number | null>(null)
  const [compare, setCompare] = useState<{ from: Side; to: Side } | null>(null)
  const [asYAML, setAsYAML] = useState(false)
  const [details, setDetails] = useState<Record<number, RevisionDetail>>({})
  const [detailErrors, setDetailErrors] = useState<Record<number, string>>({})
  const [restoring, setRestoring] = useState<number | null>(null)
  const requested = useRef(new Set<number>())
  const loadSeq = useRef(0)
  const onLoadedRef = useRef(onLoaded)
  onLoadedRef.current = onLoaded

  // Load the newest page, again whenever something changed.
  useEffect(() => {
    const id = ++loadSeq.current
    setList((l) => (l.state === 'ready' ? l : { state: 'loading' }))
    listRevisions(kind, namespace, name, { limit: PAGE_SIZE })
      .then((page) => {
        if (id !== loadSeq.current) return
        setItems(page.revisions)
        setNext(page.next)
        setList({ state: 'ready' })
        onLoadedRef.current(page.revisions[0]?.revision ?? null)
      })
      .catch((e: unknown) => {
        if (id !== loadSeq.current) return
        onLoadedRef.current(null)
        setItems([])
        setNext('')
        setList(e instanceof ApiError && e.status === 501 ? { state: 'unsupported' } : { state: 'error', message: messageOf(e) })
      })
  }, [kind, namespace, name, refreshKey])

  const loadOlder = async () => {
    setMore({ busy: true, error: '' })
    try {
      const page = await listRevisions(kind, namespace, name, { limit: PAGE_SIZE, before: next })
      setItems((cur) => [...cur, ...page.revisions.filter((r) => !cur.some((c) => c.revision === r.revision))])
      setNext(page.next)
      setMore({ busy: false, error: '' })
    } catch (e) {
      setMore({ busy: false, error: `Could not load older versions: ${messageOf(e)}` })
    }
  }

  // Read the versions on screen, once each.
  const needed = useMemo(() => {
    const set = new Set<number>()
    if (selected !== null) set.add(selected)
    if (compare) for (const s of [compare.from, compare.to]) if (s !== CURRENT) set.add(s)
    return [...set]
  }, [selected, compare])
  useEffect(() => {
    for (const n of needed) {
      if (requested.current.has(n)) continue
      requested.current.add(n)
      getRevision(kind, namespace, name, n)
        .then((d) => setDetails((cur) => ({ ...cur, [n]: d })))
        .catch((e: unknown) => {
          requested.current.delete(n)
          setDetailErrors((cur) => ({ ...cur, [n]: messageOf(e) }))
        })
    }
  }, [needed, kind, namespace, name])

  const select = useCallback((n: number) => {
    setSelected(n)
    setCompare((c) => (c ? { ...c, from: n } : c))
    setDetailErrors({})
  }, [])

  const latest = items[0]?.revision

  const objectFor = (s: Side): KubeObject | undefined => (s === CURRENT ? live ?? undefined : details[s]?.object)
  const labelFor = (s: Side) => (s === CURRENT ? 'Current version' : `Version ${s}`)

  return (
    <section aria-labelledby="history-heading" style={st.wrap}>
      <h2 id="history-heading" style={st.h2}>Version history</h2>
      <div role="status" aria-live="polite" style={{ minHeight: 18, fontSize: 12, color: ds.textSecondary }}>
        {list.state === 'loading' ? 'Loading versions…' : ''}
      </div>

      {list.state === 'unsupported' && (
        <p role="alert" style={field.help}>Version history is not turned on for this installation. It needs PostgreSQL.</p>
      )}
      {list.state === 'error' && <p role="alert" style={field.error}>The version history could not be loaded: {list.message}</p>}

      {list.state === 'ready' && readOnlyNote && <p style={field.help}>{readOnlyNote}</p>}
      {list.state === 'ready' && items.length === 0 && (
        <p style={field.help}>There are no saved versions yet. A version is kept each time this agent is saved.</p>
      )}

      {list.state === 'ready' && items.length > 0 && (
        <div style={st.columns}>
          <div style={st.listCol}>
            <ol aria-label="Versions, newest first" style={st.list}>
              {items.map((r) => {
                const on = r.revision === selected
                return (
                  <li key={r.revision}>
                    <button type="button" aria-current={on ? 'true' : undefined} onClick={() => select(r.revision)} style={{ ...st.row, border: `1px solid ${on ? ds.accent : 'var(--ds-border)'}` }}>
                      <span style={{ fontWeight: 600 }}>Version {r.revision}</span>
                      <span style={st.meta}>
                        {r.userId} · <Time iso={r.createdAt} />
                      </span>
                      <span style={{ fontSize: 13 }}>{describeRevision(r)}</span>
                    </button>
                  </li>
                )
              })}
            </ol>
            {next && (
              <DSButton type="button" size="sm" onClick={loadOlder} disabled={more.busy}>
                {more.busy ? 'Loading…' : 'Load older versions'}
              </DSButton>
            )}
            {more.error && <p role="alert" style={field.error}>{more.error}</p>}
          </div>

          <div style={st.detailCol}>
            {selected === null && !compare && <p style={field.help}>Choose a version to see what the agent was then, or to compare it with another.</p>}

            {selected !== null && (
              <VersionView
                revision={items.find((r) => r.revision === selected)}
                detail={details[selected]}
                error={detailErrors[selected]}
                isAgent={isAgent}
                isLatest={selected === latest}
                canRestore={canRestore}
                onRestore={() => setRestoring(selected)}
                onCompare={() => setCompare({ from: selected, to: CURRENT })}
              />
            )}

            {compare && (
              <CompareView
                items={items}
                compare={compare}
                onChange={setCompare}
                onClose={() => setCompare(null)}
                isAgent={isAgent}
                asYAML={asYAML}
                onToggleYAML={setAsYAML}
                before={objectFor(compare.from)}
                after={objectFor(compare.to)}
                errors={[compare.from, compare.to].flatMap((s) => (s !== CURRENT && detailErrors[s] ? [`${labelFor(s)}: ${detailErrors[s]}`] : []))}
              />
            )}
            {selected !== null && !compare && items.length > 1 && (
              <div>
                <DSButton type="button" size="sm" onClick={() => setCompare({ from: selected === latest ? (items[1]?.revision ?? selected) : selected, to: selected === latest ? latest : CURRENT })}>
                  Compare two versions
                </DSButton>
              </div>
            )}
          </div>
        </div>
      )}

      {restoring !== null && (
        <RestoreDialog
          n={restoring}
          latest={latest}
          dirty={dirty}
          resourceVersion={live?.metadata?.resourceVersion ?? ''}
          onClose={() => setRestoring(null)}
          onReload={onReload}
          restore={(rv) => restoreRevision(kind, namespace, name, restoring, rv)}
          onDone={(obj) => {
            const from = restoring
            setRestoring(null)
            onRestored(obj, from)
          }}
        />
      )}
    </section>
  )
}

// ── One version ──────────────────────────────────────────────────────────────

function VersionView({ revision, detail, error, isAgent, isLatest, canRestore, onRestore, onCompare }: {
  revision: RevisionSummary | undefined
  detail: RevisionDetail | undefined
  error: string | undefined
  isAgent: boolean
  isLatest: boolean
  canRestore: boolean
  onRestore: () => void
  onCompare: () => void
}) {
  const headingId = useId()
  if (!revision) return null
  return (
    <section aria-labelledby={headingId} style={st.panel}>
      <h3 id={headingId} style={st.h3}>Version {revision.revision}</h3>
      <p style={{ ...field.help, margin: 0 }}>
        {describeRevision(revision)} by {revision.userId}, <Time iso={revision.createdAt} />.
      </p>
      <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
        {canRestore && !isLatest && <DSButton type="button" size="sm" variant="primary" onClick={onRestore}>Restore this version</DSButton>}
        <DSButton type="button" size="sm" onClick={onCompare}>Compare with the current version</DSButton>
      </div>
      {isLatest && <p style={field.help}>This is the newest version.</p>}
      {error && <p role="alert" style={field.error}>This version could not be loaded: {error}</p>}
      {!detail && !error && <p role="status" style={field.help}>Loading this version…</p>}
      {detail && (isAgent ? <AgentSummary obj={detail.object} /> : <pre style={st.pre}>{yamlOf(detail.object)}</pre>)}
    </section>
  )
}

/** What the agent was at a version, in the form's own words. */
export function AgentSummary({ obj }: { obj: KubeObject }) {
  const f = formFromObject(obj)
  return (
    <dl style={st.dl}>
      <dt style={st.dt}>Name</dt>
      <dd style={st.dd}>{f.name || 'Not set'}</dd>
      {FIELDS.map(({ key, label }) => {
        const v = fieldValues(f, key)
        return (
          <div key={key} style={{ display: 'contents' }}>
            <dt style={st.dt}>{label}</dt>
            <dd style={st.dd}>
              {v.length === 0 ? <span style={{ color: ds.textMuted }}>None</span>
                : key === 'instructions' ? <span style={{ whiteSpace: 'pre-wrap' }}>{v[0]}</span>
                  : v.length > 1 || key === 'connections' || key === 'knowledgeBases' || key === 'exampleInputs'
                    ? <ul style={st.ul}>{v.map((x, i) => <li key={`${x}-${i}`}>{x}</li>)}</ul>
                    : v[0]}
            </dd>
          </div>
        )
      })}
    </dl>
  )
}

// ── Comparing ────────────────────────────────────────────────────────────────

function CompareView({ items, compare, onChange, onClose, isAgent, asYAML, onToggleYAML, before, after, errors }: {
  items: RevisionSummary[]
  compare: { from: Side; to: Side }
  onChange: (c: { from: Side; to: Side }) => void
  onClose: () => void
  isAgent: boolean
  asYAML: boolean
  onToggleYAML: (v: boolean) => void
  before: KubeObject | undefined
  after: KubeObject | undefined
  errors: string[]
}) {
  const fromId = useId()
  const toId = useId()
  const headingId = useId()
  const parse = (v: string): Side => (v === CURRENT ? CURRENT : Number(v))
  const options = (
    <>
      {items.map((r) => <option key={r.revision} value={r.revision}>Version {r.revision}</option>)}
      <option value={CURRENT}>Current version</option>
    </>
  )
  const label = (s: Side) => (s === CURRENT ? 'Current version' : `Version ${s}`)
  const ready = before && after
  return (
    <section aria-labelledby={headingId} style={st.panel}>
      <h3 id={headingId} style={st.h3}>Compare versions</h3>
      <div style={{ display: 'flex', gap: 12, flexWrap: 'wrap', alignItems: 'flex-end' }}>
        <div style={field.wrap}>
          <label htmlFor={fromId} style={field.label}>Older version</label>
          <select id={fromId} style={field.control} value={String(compare.from)} onChange={(e) => onChange({ ...compare, from: parse(e.target.value) })}>{options}</select>
        </div>
        <div style={field.wrap}>
          <label htmlFor={toId} style={field.label}>Newer version</label>
          <select id={toId} style={field.control} value={String(compare.to)} onChange={(e) => onChange({ ...compare, to: parse(e.target.value) })}>{options}</select>
        </div>
        {isAgent && (
          <label style={{ display: 'flex', gap: 6, alignItems: 'center', fontSize: 13 }}>
            <input type="checkbox" checked={asYAML} onChange={(e) => onToggleYAML(e.target.checked)} /> Show as YAML
          </label>
        )}
        <DSButton type="button" size="sm" onClick={onClose}>Close comparison</DSButton>
      </div>
      {errors.map((m) => <p key={m} role="alert" style={field.error}>{m}</p>)}
      {!ready && errors.length === 0 && <p role="status" style={field.help}>Loading the versions to compare…</p>}
      {ready && (isAgent && !asYAML
        ? <FieldCompare changes={agentChanges(before, after)} beforeLabel={label(compare.from)} afterLabel={label(compare.to)} />
        : <YAMLDiff before={before} after={after} beforeLabel={label(compare.from)} afterLabel={label(compare.to)} />)}
    </section>
  )
}

function FieldCompare({ changes, beforeLabel, afterLabel }: { changes: FieldChange[]; beforeLabel: string; afterLabel: string }) {
  const changed = changes.filter((c) => c.changed).length
  return (
    <>
      <p role="status" style={{ ...field.help, margin: 0 }}>
        {changed === 0 ? 'No differences between these two versions.' : `${changed} ${changed === 1 ? 'field' : 'fields'} changed.`}
      </p>
      <table style={st.table}>
        <thead>
          <tr>
            <th scope="col" style={st.th}>Field</th>
            <th scope="col" style={st.th}>{beforeLabel}</th>
            <th scope="col" style={st.th}>{afterLabel}</th>
          </tr>
        </thead>
        <tbody>
          {changes.map((c) => {
            const added = c.after.filter((v) => !c.before.includes(v))
            const removed = c.before.filter((v) => !c.after.includes(v))
            const multi = c.key === 'connections' || c.key === 'knowledgeBases' || c.key === 'exampleInputs'
            const cell = (vals: string[], mark: string[], sign: string, word: string) => (
              vals.length === 0 ? <span style={{ color: ds.textMuted }}>None</span>
                : multi ? (
                  <ul style={st.ul}>
                    {vals.map((v, i) => (
                      <li key={`${v}-${i}`}>
                        {c.changed && mark.includes(v) ? <><span aria-hidden="true">{sign} </span><span style={srOnly}>{word}: </span><strong>{v}</strong></> : v}
                      </li>
                    ))}
                  </ul>
                ) : <span style={{ whiteSpace: 'pre-wrap' }}>{vals.join('\n')}</span>
            )
            return (
              <tr key={c.key} style={c.changed ? { background: 'rgba(210,153,34,0.14)' } : undefined}>
                <th scope="row" style={st.th2}>
                  {c.label}
                  <div style={{ fontWeight: 400, fontSize: 11, color: c.changed ? ds.textPrimary : ds.textMuted }}>{c.changed ? 'Changed' : 'Unchanged'}</div>
                </th>
                <td style={st.td}>{cell(c.before, removed, '-', 'Removed')}</td>
                <td style={st.td}>{cell(c.after, added, '+', 'Added')}</td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </>
  )
}

function YAMLDiff({ before, after, beforeLabel, afterLabel }: { before: KubeObject; after: KubeObject; beforeLabel: string; afterLabel: string }) {
  const lines = useMemo(() => diffLines(yamlOf(before), yamlOf(after)), [before, after])
  const changed = lines.filter((l) => l.kind !== 'same').length
  return (
    <>
      <p role="status" style={{ ...field.help, margin: 0 }}>
        {changed === 0 ? 'No differences between these two versions.' : `Lines starting with - are only in ${beforeLabel}. Lines starting with + are only in ${afterLabel}.`}
      </p>
      <pre aria-label="Changes as YAML" style={st.pre}>
        {lines.map((l, i) => (
          <div key={i} style={{ background: l.kind === 'add' ? 'rgba(46,160,67,0.18)' : l.kind === 'remove' ? 'rgba(248,81,73,0.18)' : undefined }}>
            <span aria-hidden="true">{l.kind === 'add' ? '+ ' : l.kind === 'remove' ? '- ' : '  '}</span>
            {l.kind !== 'same' && <span style={srOnly}>{l.kind === 'add' ? 'Added: ' : 'Removed: '}</span>}
            {l.text}
          </div>
        ))}
      </pre>
    </>
  )
}

// ── Restore ──────────────────────────────────────────────────────────────────

function RestoreDialog({ n, latest, dirty, resourceVersion, onClose, onReload, restore, onDone }: {
  n: number
  latest: number | undefined
  dirty: boolean
  resourceVersion: string
  onClose: () => void
  onReload: () => Promise<void>
  restore: (resourceVersion: string) => Promise<KubeObject>
  onDone: (obj: KubeObject) => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<{ stale: boolean; text: string } | null>(null)

  const go = async () => {
    setBusy(true)
    setError(null)
    try {
      onDone(await restore(resourceVersion))
    } catch (e) {
      setBusy(false)
      if (e instanceof ApiError && e.status === 409) {
        setError({ stale: true, text: 'Someone changed this agent in the meantime, so it was not restored. Reload to get the latest version, then restore again.' })
      } else {
        setError({ stale: false, text: `Could not restore: ${messageOf(e)}` })
      }
    }
  }
  const reload = async () => {
    setBusy(true)
    try {
      await onReload()
      setError(null)
    } catch (e) {
      setError({ stale: true, text: `Could not reload: ${messageOf(e)}` })
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog title={`Restore version ${n}?`} onClose={onClose}>
      <p style={{ margin: 0, fontSize: 13 }}>
        This makes version {n} the current version again.{latest !== undefined && <> Version {latest} stays in the history, so you can go back.</>}
      </p>
      {dirty && <p style={{ ...field.help, margin: 0 }}>The changes in the form that you have not saved will be replaced.</p>}
      {error && (
        <div role="alert" style={{ ...field.error, fontSize: 13 }}>
          {error.text}
          {error.stale && <div style={{ marginTop: 8 }}><DSButton type="button" size="sm" onClick={reload} disabled={busy}>Reload</DSButton></div>}
        </div>
      )}
      <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
        <DSButton type="button" onClick={onClose} disabled={busy}>Cancel</DSButton>
        <DSButton type="button" variant="primary" onClick={go} disabled={busy || !resourceVersion}>{busy ? 'Restoring…' : `Restore version ${n}`}</DSButton>
      </div>
    </Dialog>
  )
}

// ── Styles ───────────────────────────────────────────────────────────────────

const srOnly: CSSProperties = { position: 'absolute', width: 1, height: 1, overflow: 'hidden', clip: 'rect(0 0 0 0)', whiteSpace: 'nowrap' }

const st: Record<string, CSSProperties> = {
  wrap: { display: 'flex', flexDirection: 'column', gap: 10 },
  h2: { margin: 0, fontSize: 16, fontWeight: 600, color: ds.textPrimary },
  h3: { margin: 0, fontSize: 14, fontWeight: 600, color: ds.textPrimary },
  columns: { display: 'flex', gap: 24, alignItems: 'flex-start', flexWrap: 'wrap' },
  listCol: { flex: '1 1 280px', maxWidth: 380, display: 'flex', flexDirection: 'column', gap: 8 },
  detailCol: { flex: '3 1 420px', minWidth: 0, display: 'flex', flexDirection: 'column', gap: 12 },
  list: { listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 6 },
  row: {
    width: '100%', textAlign: 'left', display: 'flex', flexDirection: 'column', gap: 2, padding: '8px 10px', cursor: 'pointer',
    background: ds.surface, color: ds.textPrimary, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, font: 'inherit',
  },
  meta: { fontSize: 12, color: ds.textSecondary },
  panel: { display: 'flex', flexDirection: 'column', gap: 8, padding: 14, background: ds.surface, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.xl },
  dl: { margin: 0, display: 'grid', gridTemplateColumns: 'minmax(110px, 160px) 1fr', gap: '6px 12px', fontSize: 13 },
  dt: { fontWeight: 600, color: ds.textSecondary },
  dd: { margin: 0, minWidth: 0, overflowWrap: 'anywhere' },
  ul: { margin: 0, paddingLeft: 18 },
  pre: { margin: 0, padding: 10, fontFamily: DESIGN.font.mono, fontSize: 12, overflow: 'auto', maxHeight: 480, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md },
  table: { borderCollapse: 'collapse', width: '100%', fontSize: 13 },
  th: { textAlign: 'left', padding: '6px 8px', borderBottom: '1px solid var(--ds-border)', color: ds.textSecondary, fontSize: 12 },
  th2: { textAlign: 'left', verticalAlign: 'top', padding: '6px 8px', borderBottom: '1px solid var(--ds-border)', fontWeight: 600, width: 130 },
  td: { verticalAlign: 'top', padding: '6px 8px', borderBottom: '1px solid var(--ds-border)', overflowWrap: 'anywhere' },
}
