/**
 * HomeBoard — Home as a grid of widgets each person arranges: add, remove,
 * reorder and make wide or narrow. The layout is saved in this browser (see
 * lib/homeLayout.ts).
 */
import { useEffect, useState, type ComponentType, type CSSProperties } from 'react'
import { getMe } from '../api/fleet'
import { canSee, useRoles, type Section as AccessSection } from '../lib/access'
import { DESIGN, ds } from '../lib/designSystem'
import { PageHeader } from '../lib/layout'
import { DSButton } from '../lib/primitives'
import {
  addWidget, DEFAULT_LAYOUT, loadLayout, moveWidget, removeWidget, REQUIRED, resetLayout, saveLayout, toggleWide, WIDGET_IDS,
  type Placement, type WidgetId,
} from '../lib/homeLayout'
import { AgentHealthWidget, AskWidget, ProblemsWidget, ReportsWidget, UpcomingWidget } from './HomeWidgets'
import { NeedsYou, RecentRuns } from './MyWork'

interface WidgetInfo {
  title: string
  /** Shown where widgets are added. */
  about: string
  view: ComponentType
  /** The sidebar section whose role this widget needs. */
  needs?: AccessSection
}

export const WIDGETS: Record<WidgetId, WidgetInfo> = {
  'needs-you': { title: 'Waiting for you', about: 'Questions from agents and requests for approval.', view: NeedsYou },
  ask: { title: 'Ask an agent', about: 'Pick an agent and give it something to do.', view: AskWidget },
  reports: { title: 'Latest reports', about: 'What your scheduled runs last produced.', view: ReportsWidget },
  problems: { title: 'Problems', about: 'Failed runs in the last day and schedules that need attention.', view: ProblemsWidget },
  'my-runs': { title: 'Your recent runs', about: 'What you asked for lately, with the results.', view: RecentRuns },
  upcoming: { title: 'Coming up', about: 'The next scheduled runs.', view: UpcomingWidget },
  agents: { title: 'Agent health', about: 'How each agent did over the last 24 hours.', view: AgentHealthWidget, needs: 'operate' },
}

export function HomeBoard() {
  const roles = useRoles()
  const [userId, setUserId] = useState<string | null>(null)
  const [layout, setLayout] = useState<Placement[]>(DEFAULT_LAYOUT)
  const [editing, setEditing] = useState(false)

  // The layout is per user, so wait to know who this is before reading it.
  useEffect(() => {
    let live = true
    const use = (id: string) => { if (live) { setLayout(loadLayout(id)); setUserId(id) } }
    getMe().then((me) => use(me.userId ?? '')).catch(() => use(''))
    return () => { live = false }
  }, [])

  const allowed = (id: WidgetId) => {
    const needs = WIDGETS[id].needs
    return !needs || canSee(roles, needs)
  }
  const shown = layout.filter((p) => allowed(p.id))
  const visible = shown.map((p) => p.id)
  const available = WIDGET_IDS.filter((id) => allowed(id) && !layout.some((p) => p.id === id))

  const change = (next: Placement[]) => {
    if (next === layout || userId === null) return
    setLayout(next)
    saveLayout(userId, next)
  }
  const reset = () => {
    if (userId === null) return
    resetLayout(userId)
    setLayout(DEFAULT_LAYOUT)
  }

  return (
    <>
      <PageHeader
        title="Home"
        description="What is waiting on you, what your agents produced, and what needs attention."
        actions={
          <DSButton size="sm" variant={editing ? 'primary' : 'secondary'} aria-pressed={editing} disabled={userId === null} onClick={() => setEditing((e) => !e)}>
            {editing ? 'Done' : 'Customize'}
          </DSButton>
        }
      />
      {editing && (
        <section style={s.editor} aria-label="Customize Home">
          <p style={s.hint}>Move, resize or remove widgets below. Changes are saved in this browser as you make them.</p>
          {available.length > 0 && (
            <ul style={s.addList} aria-label="Widgets you can add">
              {available.map((id) => (
                <li key={id} style={s.addItem}>
                  <DSButton size="sm" aria-label={`Add ${WIDGETS[id].title}`} onClick={() => change(addWidget(layout, id))}>Add</DSButton>
                  <span><strong>{WIDGETS[id].title}</strong> <span style={s.about}>{WIDGETS[id].about}</span></span>
                </li>
              ))}
            </ul>
          )}
          <div><DSButton size="sm" onClick={reset}>Reset to default</DSButton></div>
        </section>
      )}
      {userId !== null && (
        <div style={s.grid}>
          {shown.map((p, i) => {
            const { title, view: View } = WIDGETS[p.id]
            return (
              <section key={p.id} aria-label={title} style={{ ...s.widget, ...(p.wide ? s.wide : {}), ...(editing ? s.widgetEditing : {}) }}>
                <div style={s.head}>
                  <h2 style={s.title}>{title}</h2>
                  {editing && (
                    <div style={s.tools}>
                      <button type="button" style={s.tool} disabled={i === 0} aria-label={`Move ${title} earlier`} onClick={() => change(moveWidget(layout, p.id, -1, visible))}>↑</button>
                      <button type="button" style={s.tool} disabled={i === shown.length - 1} aria-label={`Move ${title} later`} onClick={() => change(moveWidget(layout, p.id, 1, visible))}>↓</button>
                      <button type="button" style={s.tool} aria-label={`${title}: use ${p.wide ? 'half' : 'full'} width`} onClick={() => change(toggleWide(layout, p.id))}>{p.wide ? 'Half width' : 'Full width'}</button>
                      {p.id !== REQUIRED && (
                        <button type="button" style={s.tool} aria-label={`Remove ${title}`} onClick={() => change(removeWidget(layout, p.id))}>Remove</button>
                      )}
                    </div>
                  )}
                </div>
                <View />
              </section>
            )
          })}
        </div>
      )}
    </>
  )
}

const s: Record<string, CSSProperties> = {
  // Two columns when there is room, one when there is not.
  grid: {
    display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(min(100%, 400px), 1fr))',
    gap: 16, padding: '8px 24px 24px', maxWidth: 1180, alignItems: 'start', flexShrink: 0,
  },
  widget: {
    minWidth: 0, display: 'flex', flexDirection: 'column', gap: 10, padding: 16, background: ds.surface,
    border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.xl, boxShadow: 'var(--ds-card-shadow)',
  },
  widgetEditing: { border: '1px dashed var(--ds-accent)' },
  wide: { gridColumn: '1 / -1' },
  head: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8, flexWrap: 'wrap' },
  title: { margin: 0, fontSize: 11, fontWeight: 600, textTransform: 'uppercase', letterSpacing: '0.08em', color: ds.textSecondary },
  tools: { display: 'flex', gap: 4, flexWrap: 'wrap' },
  tool: {
    minWidth: 28, height: 26, padding: '0 8px', borderRadius: DESIGN.radii.md, border: '1px solid var(--ds-border)',
    background: ds.bg, color: ds.textPrimary, fontSize: 12, cursor: 'pointer',
  },
  editor: { margin: '4px 24px 8px', maxWidth: 1132, display: 'flex', flexDirection: 'column', gap: 10, padding: 14, background: ds.surface, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.xl },
  hint: { margin: 0, fontSize: 13, color: ds.textSecondary },
  addList: { listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 6 },
  addItem: { display: 'flex', alignItems: 'center', gap: 10, fontSize: 13, color: ds.textPrimary },
  about: { color: ds.textSecondary },
}
