/** Build home: the agents made in this UI (the caller's drafts and agents), with ways to start a new one. */
import { useEffect, useState, type CSSProperties } from 'react'
import { Link } from 'react-router'
import { getCatalog, type CatalogEntry } from '../api/catalog'
import { DESIGN, ds } from '../lib/designSystem'
import { ICON } from '../lib/icons'
import { EmptyState, Page, PageHeader } from '../lib/layout'
import { useNamespace } from '../lib/namespace'
import { DSChip } from '../lib/primitives'
import { buildNewPath, buildPath, ROUTES } from '../lib/routes'

const linkButton: CSSProperties = {
  display: 'inline-block', padding: '6px 14px', fontSize: 12, borderRadius: DESIGN.radii.md, textDecoration: 'none',
  border: '1px solid var(--ds-border)', color: ds.textPrimary, background: ds.surface,
}
const primaryLink: CSSProperties = { ...linkButton, background: ds.accent, color: '#fff', borderColor: ds.accent }

export function BuildHome() {
  const { namespace } = useNamespace()
  const [agents, setAgents] = useState<CatalogEntry[] | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let live = true
    getCatalog(namespace)
      .then((list) => { if (live) { setAgents(list.filter((e) => e.ownership?.owner === 'ui' && !e.template)); setError('') } })
      .catch((e: unknown) => { if (live) setError(e instanceof Error ? e.message : String(e)) })
    return () => { live = false }
  }, [namespace])

  const actions = (
    <div style={{ display: 'flex', gap: 8 }}>
      <Link to={ROUTES.templates} style={linkButton}>Use a template</Link>
      <Link to={buildNewPath} style={primaryLink}>Create an agent</Link>
    </div>
  )

  return (
    <Page>
      <PageHeader title="Build" description="Create agents and change the ones you made, without writing YAML." actions={actions} />
      <div style={{ padding: '0 24px 24px' }}>
        {error ? (
          <div role="alert" style={{ color: ds.error, fontSize: 13 }}>Could not load your agents: {error}</div>
        ) : !agents ? (
          <div role="status" style={{ color: ds.textMuted, fontSize: 13 }}>Loading your agents…</div>
        ) : agents.length === 0 ? (
          <EmptyState icon={ICON.agent} title="You have not built an agent yet">
            Start from a blank page, or copy a template and change it. Agents managed in Git by your platform
            team are on the Agents page.
          </EmptyState>
        ) : (
          <ul aria-label="My agents" style={grid}>
            {agents.map((a) => (
              <li key={`${a.namespace}/${a.name}`} style={{ display: 'flex' }}>
                <Link to={buildPath(a.namespace, a.name)} style={card}>
                  <span style={{ display: 'flex', justifyContent: 'space-between', gap: 8, alignItems: 'center' }}>
                    <span style={{ fontSize: 15, fontWeight: 600, overflowWrap: 'anywhere' }}>{a.name}</span>
                    <DSChip variant={a.published ? 'success' : 'neutral'} size="sm" dot>{a.published ? 'Published' : 'Draft'}</DSChip>
                  </span>
                  <span style={{ fontSize: 13, color: ds.textSecondary }}>{a.description || 'No description yet.'}</span>
                  {a.ownership?.handoff.pending && <DSChip size="sm" variant="warning">Being handed to Git</DSChip>}
                </Link>
              </li>
            ))}
          </ul>
        )}
      </div>
    </Page>
  )
}

const grid: CSSProperties = {
  listStyle: 'none', margin: 0, padding: 0, display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(280px, 1fr))', gap: 14,
}
const card: CSSProperties = {
  display: 'flex', flexDirection: 'column', gap: 8, width: '100%', padding: 14, textDecoration: 'none', color: ds.textPrimary,
  background: ds.surface, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.xl, boxShadow: 'var(--ds-card-shadow)',
}
