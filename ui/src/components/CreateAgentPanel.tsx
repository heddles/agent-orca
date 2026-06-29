import { useEffect, useState } from 'react'
import { createAgent, listModelSelectors, type ModelSelectorSummary } from '../api/sse'

const FRAMEWORKS = [
  'openai-compatible',
  'autogen',
  'semantic-kernel',
  'langgraph',
  'shim',
]

interface Props {
  namespace?: string
  onCreated: () => void
  onClose: () => void
}

export function CreateAgentPanel({ namespace = 'default', onCreated, onClose }: Props) {
  const [name, setName] = useState('')
  const [modelSelectorRef, setModelSelectorRef] = useState('')
  const [ociRef, setOciRef] = useState('')
  const [framework, setFramework] = useState('openai-compatible')
  const [systemPrompt, setSystemPrompt] = useState('')
  const [command, setCommand] = useState('')
  const [args, setArgs] = useState('')
  const [selectors, setSelectors] = useState<ModelSelectorSummary[]>([])
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [warning, setWarning] = useState<string | null>(null)

  useEffect(() => {
    listModelSelectors(namespace).then((list) => {
      setSelectors(list)
      if (list.length > 0 && !modelSelectorRef) setModelSelectorRef(list[0].name)
    }).catch(() => {})
  }, [namespace])

  const canSubmit = name.trim() && modelSelectorRef && ociRef.trim() && !submitting

  const handleSubmit = () => {
    if (!canSubmit) return
    setSubmitting(true)
    setError(null)
    setWarning(null)
    createAgent({
      name: name.trim(),
      namespace,
      modelSelectorRef,
      ociRef: ociRef.trim(),
      framework,
      systemPrompt: systemPrompt.trim() || undefined,
      command: command.trim() ? command.trim().split(/\s+/) : undefined,
      args: args.trim() ? args.trim().split(/\s+/) : undefined,
    })
      .then((result) => {
        if (result.deploymentError) {
          setWarning(result.deploymentError)
          // Still close after a short delay so the user can see the warning.
          setTimeout(() => onCreated(), 2000)
        } else {
          onCreated()
        }
      })
      .catch((e: Error) => setError(e.message))
      .finally(() => setSubmitting(false))
  }

  return (
    <div style={s.overlay} onClick={onClose}>
      <div style={s.panel} onClick={(e) => e.stopPropagation()}>
        <div style={s.header}>
          <span style={s.title}>New Agent</span>
          <button style={s.closeBtn} onClick={onClose}>{'\u00D7'}</button>
        </div>

        <div style={s.body}>
          <Field label="Name" required>
            <input
              style={s.input}
              value={name}
              onChange={(e) => setName(e.currentTarget.value)}
              placeholder="my-agent"
              autoFocus
            />
          </Field>

          <Field label="Model Selector" required>
            {selectors.length > 0 ? (
              <select
                style={s.input}
                value={modelSelectorRef}
                onChange={(e) => setModelSelectorRef(e.currentTarget.value)}
              >
                {selectors.map((ms) => (
                  <option key={ms.name} value={ms.name}>
                    {ms.name} ({ms.strategy})
                  </option>
                ))}
              </select>
            ) : (
              <input
                style={s.input}
                value={modelSelectorRef}
                onChange={(e) => setModelSelectorRef(e.currentTarget.value)}
                placeholder="default"
              />
            )}
          </Field>

          <Field label="OCI Image" required>
            <input
              style={s.input}
              value={ociRef}
              onChange={(e) => setOciRef(e.currentTarget.value)}
              placeholder="python:3.12-slim"
            />
          </Field>

          <Field label="Framework">
            <select
              style={s.input}
              value={framework}
              onChange={(e) => setFramework(e.currentTarget.value)}
            >
              {FRAMEWORKS.map((f) => (
                <option key={f} value={f}>{f}</option>
              ))}
            </select>
          </Field>

          <Field label="Command">
            <input
              style={s.input}
              value={command}
              onChange={(e) => setCommand(e.currentTarget.value)}
              placeholder="python main.py"
            />
            <span style={s.hint}>Overrides the image entrypoint</span>
          </Field>

          <Field label="Args">
            <input
              style={s.input}
              value={args}
              onChange={(e) => setArgs(e.currentTarget.value)}
              placeholder="--port 8000 --verbose"
            />
            <span style={s.hint}>Overrides the image CMD</span>
          </Field>

          <Field label="System Prompt">
            <textarea
              style={{ ...s.input, minHeight: 80, resize: 'vertical' } as React.CSSProperties}
              value={systemPrompt}
              onChange={(e) => setSystemPrompt(e.currentTarget.value)}
              placeholder="You are a helpful assistant..."
            />
          </Field>

          {error && <div style={s.error}>{error}</div>}
          {warning && <div style={s.warning}>{warning}</div>}
        </div>

        <div style={s.footer}>
          <button style={s.cancelBtn} onClick={onClose}>Cancel</button>
          <button
            style={{ ...s.createBtn, opacity: canSubmit ? 1 : 0.4 }}
            disabled={!canSubmit}
            onClick={handleSubmit}
          >
            {submitting ? 'Creating...' : 'Create Agent'}
          </button>
        </div>
      </div>
    </div>
  )
}

function Field({ label, required, children }: { label: string; required?: boolean; children: React.ReactNode }) {
  return (
    <label style={s.field}>
      <span style={s.label}>
        {label}
        {required && <span style={s.required}> *</span>}
      </span>
      {children}
    </label>
  )
}

const s: Record<string, React.CSSProperties> = {
  overlay: {
    position: 'fixed',
    inset: 0,
    background: 'rgba(0,0,0,0.5)',
    display: 'flex',
    justifyContent: 'flex-end',
    zIndex: 1000,
  },
  panel: {
    width: 400,
    maxWidth: '100%',
    background: '#0f172a',
    borderLeft: '1px solid #1e293b',
    display: 'flex',
    flexDirection: 'column',
    height: '100%',
    overflow: 'hidden',
  },
  header: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    padding: '16px 20px',
    borderBottom: '1px solid #1e293b',
  },
  title: { fontSize: 16, fontWeight: 700, color: '#f1f5f9' },
  closeBtn: {
    background: 'none',
    border: 'none',
    color: '#64748b',
    fontSize: 22,
    cursor: 'pointer',
    lineHeight: 1,
    padding: 0,
  },
  body: {
    flex: 1,
    overflowY: 'auto',
    padding: 20,
    display: 'flex',
    flexDirection: 'column',
    gap: 16,
  },
  field: {
    display: 'flex',
    flexDirection: 'column',
    gap: 4,
  },
  label: {
    fontSize: 12,
    fontWeight: 600,
    color: '#94a3b8',
    textTransform: 'uppercase',
    letterSpacing: '0.04em',
  },
  required: { color: '#ef4444' },
  hint: { fontSize: 11, color: '#475569', marginTop: 2 },
  input: {
    background: '#1e293b',
    border: '1px solid #334155',
    borderRadius: 6,
    padding: '8px 10px',
    color: '#e2e8f0',
    fontSize: 13,
    fontFamily: 'system-ui, sans-serif',
    outline: 'none',
    width: '100%',
    boxSizing: 'border-box',
  },
  error: {
    background: '#3a1e1e',
    color: '#fca5a5',
    padding: '8px 12px',
    borderRadius: 6,
    fontSize: 12,
  },
  warning: {
    background: '#3a2e1e',
    color: '#fcd34d',
    padding: '8px 12px',
    borderRadius: 6,
    fontSize: 12,
  },
  footer: {
    display: 'flex',
    justifyContent: 'flex-end',
    gap: 8,
    padding: '12px 20px',
    borderTop: '1px solid #1e293b',
  },
  cancelBtn: {
    padding: '6px 14px',
    borderRadius: 6,
    border: '1px solid #334155',
    background: 'transparent',
    color: '#94a3b8',
    fontSize: 13,
    cursor: 'pointer',
  },
  createBtn: {
    padding: '6px 14px',
    borderRadius: 6,
    border: 'none',
    background: '#3b82f6',
    color: '#fff',
    fontSize: 13,
    fontWeight: 600,
    cursor: 'pointer',
  },
}
