/** One evaluation dataset: its details, cases and graders. */
import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router'
import { describeEvalError, exportDataset, getDataset, type EvalDataset } from '../api/evals'
import { canGradeEvals, canWriteEvals, useRoles } from '../lib/access'
import { ds } from '../lib/designSystem'
import { ICON } from '../lib/icons'
import { EmptyState, Page, PageHeader } from '../lib/layout'
import { DSButton } from '../lib/primitives'
import { ROUTES } from '../lib/routes'
import { EvalCases } from './EvalCases'
import { EvalGraders } from './EvalGraders'

export function EvalDatasetDetail({ id }: { id: number }) {
  const roles = useRoles()
  const [dataset, setDataset] = useState<EvalDataset | null>(null)
  const [error, setError] = useState<{ status?: number; message: string } | null>(null)
  const [exportError, setExportError] = useState('')

  const load = useCallback(() => {
    let live = true
    getDataset(id)
      .then((d) => { if (live) { setDataset(d); setError(null) } })
      .catch((e: unknown) => { if (live) setError({ status: (e as { status?: number }).status, message: describeEvalError(e) }) })
    return () => { live = false }
  }, [id])
  useEffect(() => load(), [load])

  const back = (
    <nav aria-label="Breadcrumb" style={{ padding: '10px 24px 0', fontSize: 12 }}>
      <Link to={ROUTES.evaluate} style={{ color: ds.accent, textDecoration: 'none' }}>Evaluate</Link>
      <span style={{ color: ds.textMuted }}> / </span>
      <span style={{ color: ds.textSecondary }}>{dataset?.name ?? 'Dataset'}</span>
    </nav>
  )

  if (error && !dataset) {
    return (
      <Page>
        {back}
        <EmptyState icon={ICON.warning} title={error.status === 404 ? 'Dataset not found' : 'Could not load the dataset'}>{error.message}</EmptyState>
      </Page>
    )
  }
  if (!dataset) {
    return <Page>{back}<div role="status" style={{ padding: '0 24px', color: ds.textMuted, fontSize: 13 }}>Loading the dataset…</div></Page>
  }
  const writable = canWriteEvals(roles)
  const download = async () => {
    setExportError('')
    try {
      const { blob, filename } = await exportDataset(dataset.id)
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = filename
      a.click()
      URL.revokeObjectURL(url)
    } catch (e) { setExportError(describeEvalError(e)) }
  }
  return (
    <Page>
      {back}
      <PageHeader
        actions={writable ? <DSButton size="sm" onClick={() => void download()}>Export JSONL</DSButton> : undefined}
        title={dataset.name}
        description={[dataset.description, `Workspace ${dataset.namespace}`, dataset.agent ? `for agent ${dataset.agent}` : '', `version ${dataset.version}`].filter(Boolean).join(' · ')}
      />
      <div style={{ padding: '0 24px 24px', display: 'flex', flexDirection: 'column', gap: 24 }}>
        {exportError && <p role="alert" style={{ margin: 0, fontSize: 12, color: ds.error }}>Could not export the dataset: {exportError}</p>}
        {!writable && <p role="status" style={{ margin: 0, fontSize: 12, color: ds.textSecondary }}>You can read this dataset. Changing it needs the builder role.</p>}
        <EvalCases datasetId={dataset.id} caseCount={dataset.caseCount} writable={writable} onChanged={load} />
        <EvalGraders datasetId={dataset.id} writable={writable} canGrade={canGradeEvals(roles)} />
      </div>
    </Page>
  )
}
