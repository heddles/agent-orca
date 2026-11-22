/**
 * Add documents to a knowledge base from the builder. Files are read in the browser as text and sent as they
 * are; their text is shown nowhere (only names and sizes), so nothing in a document is ever rendered as HTML.
 * Only text, markdown and csv are accepted, and only for a knowledge base this UI owns.
 */
import { useEffect, useRef, useState } from 'react'
import { deleteKnowledgeDoc, listKnowledgeDocs, putKnowledgeDoc, type KnowledgeDocs, type OptionItem } from '../api/builder'
import { ds } from '../lib/designSystem'
import { DSButton } from '../lib/primitives'
import { Field, field } from './BuilderParts'

const kb = (n: number) => (n < 1024 ? `${n} bytes` : `${(n / 1024).toFixed(n < 10240 ? 1 : 0)} KB`)

function messageOf(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

export function KnowledgeDocuments({ namespace, items }: { namespace: string; items: OptionItem[] }) {
  const mine = items.filter((i) => !i.namespace || i.namespace === namespace)
  const [selected, setSelected] = useState('')
  const [docs, setDocs] = useState<KnowledgeDocs | null>(null)
  const [error, setError] = useState('')
  const [notes, setNotes] = useState<string[]>([])
  const [busy, setBusy] = useState(false)
  const [pasteName, setPasteName] = useState('')
  const [pasteText, setPasteText] = useState('')
  const fileRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (!selected && mine.length > 0) setSelected(mine[0].name)
  }, [mine, selected])

  useEffect(() => {
    if (!selected || !namespace) return
    let live = true
    setDocs(null)
    setError('')
    listKnowledgeDocs(namespace, selected)
      .then((d) => { if (live) setDocs(d) })
      .catch((e: unknown) => { if (live) setError(messageOf(e)) })
    return () => { live = false }
  }, [namespace, selected])

  if (mine.length === 0) return null

  const run = async (steps: Array<() => Promise<KnowledgeDocs>>) => {
    setBusy(true)
    setError('')
    setNotes([])
    const warn: string[] = []
    try {
      for (const step of steps) {
        try {
          const d = await step()
          setDocs(d)
          if (d.warning) warn.push(d.warning)
        } catch (e) {
          setError(messageOf(e))
          break
        }
      }
    } finally {
      setNotes(warn)
      setBusy(false)
    }
  }

  const onFiles = async (files: FileList | null) => {
    if (!files || files.length === 0) return
    const list = Array.from(files)
    await run(list.map((f) => async () => putKnowledgeDoc(namespace, selected, f.name, await f.text())))
    if (fileRef.current) fileRef.current.value = ''
  }

  const addPasted = async () => {
    await run([() => putKnowledgeDoc(namespace, selected, pasteName.trim(), pasteText)])
    setPasteText('')
  }

  const editable = !!docs?.editable
  const accept = docs?.limits.types.join(',') ?? '.txt,.md,.markdown,.csv'

  return (
    <div role="group" aria-label="Add documents" style={{ display: 'flex', flexDirection: 'column', gap: 8, borderTop: '1px solid var(--ds-border)', paddingTop: 10 }}>
      <h3 style={{ margin: 0, fontSize: 13, fontWeight: 600, color: ds.textPrimary }}>Add documents</h3>
      <Field label="Add documents to" help="Text, markdown and csv files only. The agent can look things up in them once the knowledge base has read them, which can take a few minutes.">
        {(p) => (
          <select {...p} style={field.control} value={selected} onChange={(e) => setSelected(e.target.value)}>
            {mine.map((i) => <option key={i.name} value={i.name}>{i.name}</option>)}
          </select>
        )}
      </Field>
      <div role="status" aria-live="polite">
        {docs && !editable && docs.reason && <p style={field.help}>{docs.reason}</p>}
        {error && <p role="alert" style={field.error}>{error}</p>}
        {notes.map((n) => <p key={n} style={field.help}>{n}</p>)}
      </div>
      {docs && (
        <>
          {docs.documents.length === 0 ? (
            <p style={field.help}>No documents have been added here yet.</p>
          ) : (
            <ul aria-label="Documents" style={{ margin: 0, paddingLeft: 18, fontSize: 12, color: ds.textPrimary }}>
              {docs.documents.map((d) => (
                <li key={d.name}>
                  {d.name} <span style={{ color: ds.textSecondary }}>({kb(d.bytes)})</span>{' '}
                  {editable && (
                    <DSButton type="button" variant="ghost" size="sm" disabled={busy} aria-label={`Remove ${d.name}`}
                      onClick={() => { if (window.confirm(`Remove ${d.name}? Text already read from it stays in the knowledge base until the knowledge base is rebuilt.`)) void run([() => deleteKnowledgeDoc(namespace, selected, d.name)]) }}>
                      Remove
                    </DSButton>
                  )}
                </li>
              ))}
            </ul>
          )}
          {editable && (
            <>
              <p style={field.help}>
                Up to {docs.limits.maxDocs} documents of {kb(docs.limits.maxDocBytes)} each, {kb(docs.limits.maxTotalBytes)} in all. Do not add anything
                that people who use this agent should not see: what is in a document can be quoted in answers.
              </p>
              <div>
                <label style={field.label} htmlFor="kb-files">Choose files</label>{' '}
                <input id="kb-files" ref={fileRef} type="file" multiple accept={accept} disabled={busy} onChange={(e) => { void onFiles(e.target.files) }} />
              </div>
              <Field label="Or paste text: file name" help="For example returns-policy.md">
                {(p) => <input {...p} style={field.control} value={pasteName} disabled={busy} onChange={(e) => setPasteName(e.target.value)} />}
              </Field>
              <Field label="Text">
                {(p) => <textarea {...p} rows={4} style={field.control} value={pasteText} disabled={busy} onChange={(e) => setPasteText(e.target.value)} />}
              </Field>
              <div>
                <DSButton type="button" variant="secondary" disabled={busy || pasteName.trim() === '' || pasteText.trim() === ''} onClick={() => { void addPasted() }}>
                  Add text
                </DSButton>
              </div>
            </>
          )}
        </>
      )}
    </div>
  )
}
