/**
 * "Describe what you want": a box at the top of a new agent. The workspace's model turns a plain-English
 * description into a draft of the form; the person reviews it, edits it and saves it like any other. Nothing
 * is saved here, and the filled fields are marked as AI suggested until the person changes them.
 */
import { useState } from 'react'
import { ApiError, draftFromDescription, type AgentDraft, type BuilderOptions } from '../api/builder'
import { ds } from '../lib/designSystem'
import { DSButton } from '../lib/primitives'
import { Field, field } from './BuilderParts'

export const MAX_DESCRIPTION = 2000

interface Props {
  namespace: string
  /** From the builder options: whether the feature is set up for this workspace. Null while loading. */
  availability: BuilderOptions['draft'] | null
  onDraft: (draft: AgentDraft) => void
}

function describeError(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 429) return e.message || 'You have asked for too many drafts. Try again later.'
    if (e.status === 413) return 'That description is too long. Shorten it and try again.'
    if (e.status === 409) return e.message || 'Drafting is not set up for this workspace.'
    if (e.status === 502) return 'The model did not give a usable draft. Try rewording what you want.'
    return e.message
  }
  return e instanceof Error ? e.message : String(e)
}

export function DraftFromDescription({ namespace, availability, onDraft }: Props) {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [last, setLast] = useState<AgentDraft | null>(null)
  if (!availability) return null
  const off = !availability.available
  const tooLong = text.length > MAX_DESCRIPTION

  const run = async () => {
    setBusy(true)
    setError('')
    try {
      const d = await draftFromDescription(text.trim(), namespace)
      setLast(d)
      onDraft(d)
    } catch (e) {
      setError(describeError(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <section aria-labelledby="describe-heading" style={{ border: '1px solid var(--ds-border)', borderRadius: 8, padding: 14, display: 'flex', flexDirection: 'column', gap: 8, marginBottom: 12 }}>
      <h2 id="describe-heading" style={{ margin: 0, fontSize: 15, fontWeight: 600, color: ds.textPrimary }}>Describe what you want</h2>
      <Field
        label="In your own words"
        help="Say what the agent should do and for whom. A model writes a first draft of the form below for you to review. Nothing is saved until you save it. Do not paste passwords or keys."
        error={tooLong ? `Keep it under ${MAX_DESCRIPTION} characters.` : undefined}
      >
        {(p) => (
          <textarea
            {...p} rows={3} style={field.control} value={text} disabled={off || busy}
            placeholder="For example: an assistant that answers customers' refund questions using our returns policy"
            onChange={(e) => setText(e.target.value)}
          />
        )}
      </Field>
      <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
        <DSButton type="button" variant="secondary" onClick={() => { void run() }} disabled={off || busy || text.trim().length < 10 || tooLong} title={off ? availability.reason : undefined}>
          {busy ? 'Drafting…' : 'Draft it'}
        </DSButton>
        {off && <span style={field.help}>{availability.reason || 'Drafting is not set up for this workspace.'}</span>}
      </div>
      <div role="status" aria-live="polite">
        {error && <p role="alert" style={field.error}>{error}</p>}
        {last && !error && (
          <div>
            <p style={field.help}>
              The draft is in the form below, with the fields it filled marked “AI suggested”. Read it through and change anything before you save.
              {last.suggestedBudgetUsd ? ` The model thinks one run should cost about $${last.suggestedBudgetUsd} at most; set limits when you publish or schedule.` : ''}
            </p>
            {last.notes.length > 0 && <ul style={{ margin: '4px 0 0', paddingLeft: 18, fontSize: 12, color: ds.textSecondary }}>{last.notes.map((n) => <li key={n}>{n}</li>)}</ul>}
            {!last.valid && (last.errors?.length ?? 0) > 0 && (
              <ul style={{ margin: '4px 0 0', paddingLeft: 18, fontSize: 12, color: ds.error }}>{last.errors?.map((n) => <li key={n}>{n}</li>)}</ul>
            )}
          </div>
        )}
      </div>
    </section>
  )
}
