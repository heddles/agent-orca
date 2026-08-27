/**
 * ResourceList — unified left-panel list for Runs, Deployments, and Workflows.
 * Items are grouped into phase-based accordions (Running, Pending, Failed, Succeeded).
 */
import { useEffect, useState } from 'react'
import {
  listRuns,
  listDeployments,
  listWorkflows,
  getCosts,
  type AgentRunSummary,
  type AgentDeploymentSummary,
  type AgentWorkflowSummary,
} from '../api/sse'
import { StatusBadge } from './StatusBadge'
import { PHASE_COLOR } from '../lib/phaseColors'
import { DESIGN, ds } from '../lib/designSystem'
import { Icon, ICON } from '../lib/icons'

export type ResourceTab = 'runs' | 'deployments' | 'workflows' | 'agents' | 'tools' | 'mcpservers' | 'modelproviders' | 'knowledgebases' | 'modelselectors'

export type ResourceSelection =
  | { kind: 'run'; name: string; namespace: string }
  | { kind: 'deployment'; name: string; namespace: string }
  | { kind: 'workflow'; name: string; namespace: string }
  | { kind: 'agent'; name: string; namespace: string }
  | { kind: 'tool'; name: string; namespace: string }
  | { kind: 'mcpserver'; name: string; namespace: string }
  | { kind: 'modelprovider'; name: string; namespace: string }
  | { kind: 'knowledgebase'; name: string; namespace: string }
  | { kind: 'modelselector'; name: string; namespace: string }

interface Props {
  tab: ResourceTab
  namespace?: string
  selection: ResourceSelection | null
  onSelect: (sel: ResourceSelection) => void
}

interface PhaseGroup {
  label: string
  phases: string[]
  color: string
}

const PHASE_GROUPS: PhaseGroup[] = [
  { label: 'Running',   phases: ['Running', 'Creating'],               color: PHASE_COLOR['Running'] },
  { label: 'Pending',   phases: ['Pending', 'Paused'],                 color: PHASE_COLOR['Paused'] },
  { label: 'Failed',    phases: ['Failed', 'Cancelled'],               color: PHASE_COLOR['Failed'] },
  { label: 'Succeeded', phases: ['Succeeded', 'Skipped', 'HandedOff'], color: PHASE_COLOR['Succeeded'] },
]

function groupByPhase<T extends { phase: string }>(items: T[]): Map<string, T[]> {
  const map = new Map<string, T[]>()
  for (const g of PHASE_GROUPS) map.set(g.label, [])
  const other = map.set('Other', [])
  for (const item of items) {
    const group = PHASE_GROUPS.find((g) => g.phases.includes(item.phase))
    const key = group ? group.label : 'Other'
    map.get(key)!.push(item)
  }
  return map
}

interface AccordionProps {
  label: string
  count: number
  color: string
  open: boolean
  onToggle: () => void
  children: React.ReactNode
}

function Accordion({ label, count, color, open, onToggle, children }: AccordionProps) {
  return (
    <div style={s.accordion}>
      <button style={s.accordionHeader} onClick={onToggle} aria-expanded={open} aria-controls={`accordion-body-${label}`}>
        <span style={{ ...s.groupDot, background: color }} />
        <span style={s.groupLabel}>{label}</span>
        <span style={s.groupCount}>{count}</span>
        <span style={{ ...s.chevron, transform: open ? 'rotate(90deg)' : 'rotate(0deg)' }} aria-hidden="true">›</span>
      </button>
      {open && <div id={`accordion-body-${label}`} style={s.accordionBody}>{children}</div>}
    </div>
  )
}

function useAccordionState(keys: string[]) {
  const [open, setOpen] = useState<Record<string, boolean>>(() =>
    Object.fromEntries(keys.map((k) => [k, true])),
  )
  const toggle = (key: string) => setOpen((prev) => ({ ...prev, [key]: !prev[key] }))
  return { open, toggle }
}

export function ResourceList({ tab, namespace = 'default', selection, onSelect }: Props) {
  const [runs, setRuns] = useState<AgentRunSummary[]>([])
  const [deployments, setDeployments] = useState<AgentDeploymentSummary[]>([])
  const [workflows, setWorkflows] = useState<AgentWorkflowSummary[]>([])
  const [deployCosts, setDeployCosts] = useState<Record<string, string>>({})
  const [loading, setLoading] = useState(false)
  const [filterText, setFilterText] = useState('')

  const groupKeys = [...PHASE_GROUPS.map((g) => g.label), 'Other']
  const { open, toggle } = useAccordionState(groupKeys)

  useEffect(() => {
    setLoading(true)

    const load = async () => {
      try {
        if (tab === 'runs') {
          const data = await listRuns(namespace)
          data.sort((a, b) => (b.startTime ?? '').localeCompare(a.startTime ?? '') || a.name.localeCompare(b.name))
          setRuns(data)
        } else if (tab === 'deployments') {
          const data = await listDeployments(namespace)
          data.sort((a, b) => a.name.localeCompare(b.name))
          setDeployments(data)
          const costs: Record<string, string> = {}
          await Promise.all(
            data.map(async (dep) => {
              try {
                const c = await getCosts(namespace, { deployment: dep.name })
                costs[dep.name] = c.totalUSD
              } catch {
                // ignore per-deployment cost errors
              }
            }),
          )
          setDeployCosts(costs)
        } else {
          const data = await listWorkflows(namespace)
          data.sort((a, b) => (b.startTime ?? '').localeCompare(a.startTime ?? '') || a.name.localeCompare(b.name))
          setWorkflows(data)
        }
      } catch {
        // ignore fetch errors
      } finally {
        setLoading(false)
      }
    }

    load()
    const interval = setInterval(load, 3000)
    return () => clearInterval(interval)
  }, [tab, namespace])

  useEffect(() => {
    setFilterText('')
  }, [tab])

  if (loading && runs.length === 0 && deployments.length === 0 && workflows.length === 0) {
    return <div style={s.loading}>Loading…</div>
  }

  const q = filterText.trim().toLowerCase()
  const filterInput = (
    <div style={s.filterWrapper}>
      <Icon icon={ICON.search} size={12} style={s.filterIcon} ariaHidden={true} />
      <input
        type="text"
        placeholder="Filter by name…"
        value={filterText}
        onChange={(e) => setFilterText((e.target as HTMLInputElement).value)}
        style={s.filterInput}
      />
      {filterText && (
        <button
          type="button"
          style={s.filterClear}
          onClick={() => setFilterText('')}
          aria-label="Clear filter"
          title="Clear filter"
        >
          <Icon icon={ICON.close} size={10} />
        </button>
      )}
    </div>
  )

  if (tab === 'runs') {
    const filtered = q ? runs.filter((r) => r.name.toLowerCase().includes(q)) : runs
    const groups = groupByPhase(filtered)
    const hasAny = filtered.length > 0
    return (
      <div style={s.list}>
        {filterInput}
        {!hasAny && <div style={s.empty}>{q ? 'No runs match your filter.' : 'No runs found.'}</div>}
        {PHASE_GROUPS.map((g) => {
          const items = groups.get(g.label) ?? []
          if (items.length === 0) return null
          return (
            <Accordion
              key={g.label}
              label={g.label}
              count={items.length}
              color={g.color}
              open={open[g.label]}
              onToggle={() => toggle(g.label)}
            >
              {items.map((run) => {
                const active = selection?.kind === 'run' && selection.name === run.name
                return (
                  <button
                    key={run.name}
                    type="button"
                    style={{ ...s.item, ...(active ? s.itemActive : {}) }}
                    onClick={() => onSelect({ kind: 'run', name: run.name, namespace: run.namespace })}
                  >
                    <div style={s.itemName}>{run.name}</div>
                    <div style={s.itemSub}>{run.agentRef}</div>
                    <div style={s.itemMeta}>
                      <StatusBadge phase={run.phase} />
                      <span style={{ ...s.spend, fontVariantNumeric: 'tabular-nums' }}>${run.spendUSD}</span>
                    </div>
                  </button>
                )
              })}
            </Accordion>
          )
        })}
      </div>
    )
  }

  if (tab === 'deployments') {
    const filtered = q ? deployments.filter((d) => d.name.toLowerCase().includes(q)) : deployments
    const groups = groupByPhase(filtered)
    const hasAny = filtered.length > 0
    return (
      <div style={s.list}>
        {filterInput}
        {!hasAny && <div style={s.empty}>{q ? 'No deployments match your filter.' : 'No deployments found.'}</div>}
        {PHASE_GROUPS.map((g) => {
          const items = groups.get(g.label) ?? []
          if (items.length === 0) return null
          return (
            <Accordion
              key={g.label}
              label={g.label}
              count={items.length}
              color={g.color}
              open={open[g.label]}
              onToggle={() => toggle(g.label)}
            >
              {items.map((dep) => {
                const active = selection?.kind === 'deployment' && selection.name === dep.name
                return (
                  <button
                    key={dep.name}
                    type="button"
                    style={{ ...s.item, ...(active ? s.itemActive : {}) }}
                    onClick={() => onSelect({ kind: 'deployment', name: dep.name, namespace: dep.namespace })}
                  >
                    <div style={s.itemName}>{dep.name}</div>
                    <div style={s.itemSub}>{dep.agentRef}{dep.phase === 'Failed' && dep.message ? ` · ${dep.message}` : ''}</div>
                    <div style={s.itemMeta}>
                      <StatusBadge phase={dep.phase} />
                      <span style={{ ...s.spend, fontVariantNumeric: 'tabular-nums' }}>${deployCosts[dep.name] || '0.0000'}</span>
                    </div>
                  </button>
                )
              })}
            </Accordion>
          )
        })}
      </div>
    )
  }

  // workflows
  const filtered = q ? workflows.filter((w) => w.name.toLowerCase().includes(q)) : workflows
  const groups = groupByPhase(filtered)
  const hasAny = filtered.length > 0
  return (
    <div style={s.list}>
      {filterInput}
      {!hasAny && <div style={s.empty}>{q ? 'No workflows match your filter.' : 'No workflows found.'}</div>}
      {PHASE_GROUPS.map((g) => {
        const items = groups.get(g.label) ?? []
        if (items.length === 0) return null
        return (
          <Accordion
            key={g.label}
            label={g.label}
            count={items.length}
            color={g.color}
            open={open[g.label]}
            onToggle={() => toggle(g.label)}
          >
            {items.map((wf) => {
              const active = selection?.kind === 'workflow' && selection.name === wf.name
              return (
                <button
                  key={wf.name}
                  type="button"
                  style={{ ...s.item, ...(active ? s.itemActive : {}) }}
                  onClick={() => onSelect({ kind: 'workflow', name: wf.name, namespace: wf.namespace })}
                >
                  <div style={s.itemName}>{wf.name}</div>
                  <div style={s.itemSub}>{wf.stepCount} steps</div>
                  <div style={s.itemMeta}>
                    <StatusBadge phase={wf.phase} />
                    <span style={{ ...s.spend, fontVariantNumeric: 'tabular-nums' }}>${wf.totalSpendUSD || '0.0000'}</span>
                  </div>
                </button>
              )
            })}
          </Accordion>
        )
      })}
    </div>
  )
}

const s: Record<string, React.CSSProperties> = {
  list: {
    display: 'flex',
    flexDirection: 'column',
    gap: 2,
    padding: '6px 8px',
  },
  accordion: {
    display: 'flex',
    flexDirection: 'column',
  },
  accordionHeader: {
    display: 'flex',
    alignItems: 'center',
    gap: 6,
    padding: '5px 4px',
    background: 'none',
    border: 'none',
    cursor: 'pointer',
    width: '100%',
    textAlign: 'left',
    fontSize: 13,
    transitionProperty: 'color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  groupDot: {
    width: 7,
    height: 7,
    borderRadius: '50%',
    flexShrink: 0,
  },
  groupLabel: {
    flex: 1,
    fontSize: 11,
    fontWeight: 700,
    color: 'var(--ds-text-secondary)',
    letterSpacing: '0.06em',
    textTransform: 'uppercase',
  },
  groupCount: {
    fontSize: 11,
    color: 'var(--ds-text-muted)',
    fontWeight: 600,
    fontVariantNumeric: 'tabular-nums',
  },
  chevron: {
    fontSize: 14,
    color: 'var(--ds-text-muted)',
    lineHeight: 1,
    transitionProperty: 'transform',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'cubic-bezier(0.2, 0, 0, 1)',
    display: 'inline-block',
  },
  accordionBody: {
    display: 'flex',
    flexDirection: 'column',
    gap: 2,
    paddingBottom: 6,
  },
  item: {
    padding: '9px 10px',
    borderRadius: DESIGN.radii.sm,
    cursor: 'pointer',
    display: 'flex',
    flexDirection: 'column',
    gap: 3,
    border: '1px solid transparent',
    transitionProperty: 'background-color, border-color',
    transitionDuration: '0.12s',
    transitionTimingFunction: 'ease',
    background: 'transparent',
    color: 'inherit',
    font: 'inherit',
    textAlign: 'left',
    textDecoration: 'none',
  },
  itemHover: {
    background: 'var(--ds-surface-hover)',
  },
  itemActive: {
    background: 'rgba(59,130,246,.1)',
    borderColor: 'rgba(59,130,246,.25)',
  },
  itemName: {
    fontSize: 12,
    fontWeight: 600,
    color: 'var(--ds-text-primary)',
    whiteSpace: 'nowrap',
    overflow: 'hidden',
    textOverflow: 'ellipsis',
  },
  itemSub: {
    fontSize: 11,
    color: 'var(--ds-text-muted)',
    whiteSpace: 'nowrap',
    overflow: 'hidden',
    textOverflow: 'ellipsis',
  },
  itemMeta: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    marginTop: 2,
  },
  spend: { fontSize: 10, color: 'var(--ds-text-muted)' },
  loading: { padding: 16, color: 'var(--ds-text-muted)', fontSize: 13 },
  empty: { padding: '16px 10px', color: 'var(--ds-text-muted)', fontSize: 13 },
  filterWrapper: {
    position: 'relative' as const,
    width: 'calc(100% - 4px)',
    marginBottom: 4,
  },
  filterIcon: {
    position: 'absolute' as const,
    left: 6,
    top: '50%',
    transform: 'translateY(-50%)',
    color: 'var(--ds-text-muted)',
    pointerEvents: 'none' as const,
  },
  filterClear: {
    position: 'absolute' as const,
    right: 4,
    top: '50%',
    transform: 'translateY(-50%)',
    width: 16,
    height: 16,
    borderRadius: '50%',
    border: 'none',
    background: 'transparent',
    color: 'var(--ds-text-muted)',
    cursor: 'pointer',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    padding: 0,
    transitionProperty: 'color, background-color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  filterInput: {
    width: '100%',
    boxSizing: 'border-box' as const,
    padding: '5px 8px 5px 26px',
    marginBottom: 4,
    background: 'var(--ds-surface)',
    border: '1px solid var(--ds-border)',
    borderRadius: DESIGN.radii.sm,
    color: 'var(--ds-text-primary)',
    fontSize: 12,
    fontFamily: 'system-ui, sans-serif',
    outline: 'none',
    fontVariantNumeric: 'tabular-nums',
    transitionProperty: 'border-color, box-shadow',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
}
