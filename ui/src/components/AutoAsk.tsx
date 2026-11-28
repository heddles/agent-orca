/** Asking without choosing an agent: the platform picks one from the request. */
import { useState, type CSSProperties, type FormEvent } from 'react'
import { useNavigate } from 'react-router'
import { startRoutedRun, type Routing } from '../api/routing'
import { canAdminEvals, useRoles } from '../lib/access'
import { DESIGN, ds } from '../lib/designSystem'
import { DSButton } from '../lib/primitives'
import { liveRunPath } from '../lib/routes'
import { field } from './BuilderParts'

export function AutoAsk({ namespace, routing }: { namespace: string; routing: Routing }) {
  const navigate = useNavigate()
  const admin = canAdminEvals(useRoles())
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (!text.trim()) { setError('Say what you need.'); return }
    setBusy(true)
    setError('')
    try {
      const run = await startRoutedRun(text.trim(), namespace)
      navigate(liveRunPath(run.namespace, run.run_id))
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit} noValidate aria-label="Ask without choosing an agent" style={s.form}>
      <label style={s.label}>
        What do you need?
        <textarea style={s.textarea} value={text} onChange={(e) => setText(e.target.value)} />
      </label>
      {!routing.hasDefault && (
        <p style={s.note}>
          {routing.defaultAgent
            ? `You cannot run the default agent, ${routing.defaultAgent}, so a request that fits no agent clearly may be turned down.`
            : 'No default agent is set, so a request that fits no agent clearly may be turned down.'}
          {admin && !routing.defaultAgent && ' Set spec.dispatch.defaultAgent in the tenant configuration.'}
        </p>
      )}
      {error && <p role="alert" style={field.error}>{error}</p>}
      <div><DSButton type="submit" variant="primary" disabled={busy}>{busy ? 'Choosing an agent…' : 'Ask'}</DSButton></div>
    </form>
  )
}

const s: Record<string, CSSProperties> = {
  form: { display: 'flex', flexDirection: 'column', gap: 10 },
  label: { display: 'flex', flexDirection: 'column', gap: 4, fontSize: 12, fontWeight: 600, color: ds.textPrimary },
  textarea: {
    background: ds.surface, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, padding: '8px 10px',
    color: ds.textPrimary, fontSize: 13, fontWeight: 400, fontFamily: DESIGN.font.family, minHeight: 90, width: '100%',
    boxSizing: 'border-box', resize: 'vertical',
  },
  note: { margin: 0, fontSize: 12, color: ds.textSecondary },
}
