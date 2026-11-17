/**
 * TemplatesGallery — read-only list and details of agent templates. A template
 * is an Agent labelled agentorca.io/template: "true" (and scope: platform) that
 * the platform team keeps in Git, optionally with an AgentDeployment and Tools
 * that share an agentorca.io/template-group label.
 */
import { useEffect, useState, type CSSProperties } from 'react'
import { Link } from 'react-router'
import { getCatalog, type CatalogEntry } from '../api/catalog'
import { DSChip } from '../lib/primitives'
import { ds, DESIGN } from '../lib/designSystem'
import { ICON } from '../lib/icons'
import { EmptyState, Page, PageHeader, Section } from '../lib/layout'
import { ROUTES, templatePath } from '../lib/routes'
import { useNamespace } from '../lib/namespace'
import { AgentCard } from './AgentCatalog'

function useTemplates(): { templates: CatalogEntry[] | null; error: string | null } {
  const { namespace } = useNamespace()
  const [templates, setTemplates] = useState<CatalogEntry[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  useEffect(() => {
    let live = true
    getCatalog(namespace)
      .then((list) => { if (live) { setTemplates(list.filter((e) => e.template)); setError(null) } })
      .catch((e: unknown) => { if (live) setError(e instanceof Error ? e.message : String(e)) })
    return () => { live = false }
  }, [namespace])
  return { templates, error }
}

export function TemplatesGallery() {
  const { templates, error } = useTemplates()
  if (error) return <div role="alert" style={s.error}>Could not load the templates: {error}</div>
  if (!templates) return <div role="status" style={s.loading}>Loading templates…</div>
  if (templates.length === 0) {
    return (
      <EmptyState icon={ICON.templates.research} title="No templates yet">
        Templates are ready-made agents your platform team keeps up to date. None have been published to
        this workspace. Ask your platform team to add them, or see the Agents tab for what already exists.
      </EmptyState>
    )
  }
  return (
    <div style={s.wrap}>
      <p style={s.intro}>
        Templates are ready-made agents maintained by your platform team. Open one to see what it does and
        which tools it uses.
      </p>
      <ul style={s.grid} aria-label="Templates">
        {templates.map((t) => <AgentCard key={`${t.namespace}/${t.name}`} agent={t} href={templatePath(t.namespace, t.name)} />)}
      </ul>
    </div>
  )
}

export function TemplateDetail({ namespace, name }: { namespace: string; name: string }) {
  const { templates, error } = useTemplates()
  if (error) return <div role="alert" style={s.error}>Could not load the template: {error}</div>
  if (!templates) return <div role="status" style={s.loading}>Loading template…</div>
  const t = templates.find((x) => x.namespace === namespace && x.name === name)
  if (!t) {
    return (
      <Page>
        <EmptyState icon={ICON.agent} title="Template not found" action={<Link to={ROUTES.templates} style={{ color: ds.accent }}>Back to templates</Link>}>
          It may have been removed by your platform team.
        </EmptyState>
      </Page>
    )
  }
  const deployments = (t.members ?? []).filter((m) => m.kind === 'AgentDeployment')
  const tools = (t.members ?? []).filter((m) => m.kind === 'Tool')
  return (
    <Page>
      <nav aria-label="Breadcrumb" style={{ padding: '10px 24px 0', fontSize: 12 }}>
        <Link to={ROUTES.templates} style={{ color: ds.accent, textDecoration: 'none' }}>Templates</Link>
        <span style={{ color: ds.textMuted }}> / </span>
        <span style={{ color: ds.textSecondary }}>{t.name}</span>
      </nav>
      <PageHeader
        title={t.name}
        description={t.description.trim().split(/\n\s*\n/)[0].replace(/\s+/g, ' ')}
        actions={<UseTemplateButton />}
      />
      <Section title="What is included">
        <dl style={s.dl}>
          <dt>Maintained by</dt><dd>{t.maintainer || 'Not set'}</dd>
          <dt>Model</dt><dd>{t.modelSelector || 'Not set'}</dd>
          <dt>Tools</dt><dd>{t.tools.length > 0 ? t.tools.join(', ') : 'None'}</dd>
          {deployments.length > 0 && (<><dt>Published as</dt><dd>{deployments.map((d) => d.name).join(', ')}</dd></>)}
          {tools.length > 0 && (<><dt>Bundled tools</dt><dd>{tools.map((d) => d.name).join(', ')}</dd></>)}
        </dl>
      </Section>
      {t.exampleInputs.length > 0 && (
        <Section title="Example inputs">
          <ul style={{ margin: 0, paddingLeft: 18, display: 'flex', flexDirection: 'column', gap: 4, fontSize: 13, color: ds.textSecondary }}>
            {t.exampleInputs.map((x) => <li key={x}>{x}</li>)}
          </ul>
        </Section>
      )}
      <Section title="How it is set up">
        <p style={{ margin: 0, fontSize: 13, color: ds.textSecondary, maxWidth: 640 }}>
          This template is kept in Git by the platform team and cannot be changed here. Using it makes your
          own copy in your workspace, which you can then change.
        </p>
        <div style={{ marginTop: 8 }}><DSChip size="sm">Read only</DSChip></div>
      </Section>
    </Page>
  )
}

/**
 * "Use template" copies the template into the caller's workspace as UI-owned.
 * It needs the safe write path from the builder stage, so it is disabled for now.
 */
function UseTemplateButton() {
  // TODO(U1/U5): when POST /api/templates/{ns}/{name}/use exists, call it here
  // and open the new agent in the builder. Until then the button stays disabled.
  const reason = 'Using a template arrives with the builder.'
  return (
    <span title={reason} style={{ display: 'inline-flex', flexDirection: 'column', alignItems: 'flex-end', gap: 4 }}>
      <button
        type="button"
        disabled
        aria-describedby="use-template-note"
        style={s.disabledBtn}
      >
        Use template
      </button>
      <span id="use-template-note" style={{ fontSize: 11, color: ds.textMuted }}>{reason}</span>
    </span>
  )
}

const s: Record<string, CSSProperties> = {
  wrap: { padding: '8px 24px 24px', display: 'flex', flexDirection: 'column', gap: 12 },
  intro: { margin: 0, fontSize: 13, color: ds.textSecondary, maxWidth: 640 },
  grid: { listStyle: 'none', margin: 0, padding: 0, display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(280px, 1fr))', gap: 14 },
  loading: { padding: 24, color: ds.textMuted, fontSize: 13 },
  error: { margin: 24, padding: 12, color: ds.error, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, fontSize: 13 },
  dl: { display: 'grid', gridTemplateColumns: 'max-content 1fr', gap: '6px 16px', margin: 0, fontSize: 13, color: ds.textSecondary },
  disabledBtn: {
    background: 'var(--ds-muted-bg)', color: ds.textMuted, border: '1px solid var(--ds-border)',
    borderRadius: DESIGN.radii.md, padding: '6px 14px', fontSize: 12, cursor: 'not-allowed',
  },
}
