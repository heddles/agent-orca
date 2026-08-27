/**
 * ResourceEditor — generic, CRD-aware form for editing any agent-orc resource.
 * Uses better-accessibility §6 (labels on all inputs), §14 (destructive action
 * confirmation), better-ui §9 (scale-on-press feedback via DSButton).
 *
 * The field schema is driven by RESOURCE_SCHEMAS, which maps each resource kind
 * to a list of editable field definitions. Status is displayed read-only below
 * the form — the server strips .status on PUT.
 */
import { useEffect, useState, useCallback } from 'react'
import {
  getResource,
  updateResource,
  deleteResource,
  type ResourceKind,
} from '../api/sse'
import { DSButton, DSCard, DSInput } from '../lib/primitives'
import { DESIGN, ds } from '../lib/designSystem'
import { Icon, ICON } from '../lib/icons'

// ── Field schema definition ─────────────────────────────────────────────────

type FieldType = 'text' | 'textarea' | 'number' | 'select' | 'tags' | 'boolean' | 'json'

interface FieldDef {
  key: string
  label: string
  type: FieldType
  required?: boolean
  options?: string[]
  help?: string
}

// Maps dotted paths (e.g. "runtime.ociRef") to nested spec values.
function getNested(obj: any, path: string): any {
  return path.split('.').reduce((o, k) => (o == null ? undefined : o[k]), obj)
}
function setNested(obj: any, path: string, value: any): void {
  const parts = path.split('.')
  let cur = obj
  for (let i = 0; i < parts.length - 1; i++) {
    if (cur[parts[i]] == null) cur[parts[i]] = {}
    cur = cur[parts[i]]
  }
  cur[parts[parts.length - 1]] = value
}

const RESOURCE_SCHEMAS: Record<ResourceKind, FieldDef[]> = {
  agents: [
    { key: 'name', label: 'Agent Name', type: 'text', required: true },
    { key: 'modelSelectorRef', label: 'Model Selector', type: 'text', required: true },
    { key: 'systemPrompt', label: 'System Prompt', type: 'textarea', help: 'System-level instruction prepended to every conversation.' },
    { key: 'runtime.ociRef', label: 'OCI Image', type: 'text', required: true },
    { key: 'runtime.framework', label: 'Framework', type: 'select', options: ['openai-compatible', 'autogen', 'semantic-kernel', 'langgraph', 'shim'] },
    { key: 'runtime.command', label: 'Command Override', type: 'tags' },
    { key: 'tools', label: 'Tools', type: 'tags' },
    { key: 'knowledgeBases', label: 'Knowledge Bases', type: 'tags' },
    { key: 'guardrailPolicyRef', label: 'Guardrail Policy', type: 'text' },
  ],
  tools: [
    { key: 'name', label: 'Tool Name', type: 'text', required: true },
    { key: 'type', label: 'Type', type: 'select', options: ['regular', 'agent', 'mcp', 'wasm'] },
    { key: 'executionMode', label: 'Execution Mode', type: 'select', options: ['pod', 'sidecar', 'wasm'] },
    { key: 'ociRef', label: 'OCI Image', type: 'text' },
    { key: 'agentRef', label: 'Agent Ref', type: 'text' },
  ],
  mcpservers: [
    { key: 'name', label: 'MCP Server Name', type: 'text', required: true },
    { key: 'transport', label: 'Transport', type: 'select', options: ['stdio', 'http', 'sse'] },
    { key: 'url', label: 'URL', type: 'text' },
    { key: 'ociRef', label: 'OCI Image', type: 'text' },
    { key: 'allowedAgents', label: 'Allowed Agents', type: 'tags' },
    { key: 'allowApps', label: 'Allow Apps', type: 'boolean' },
  ],
  modelproviders: [
    { key: 'name', label: 'Provider Name', type: 'text', required: true },
    { key: 'liteLLMModel', label: 'LiteLLM Model', type: 'text', required: true },
    { key: 'baseURL', label: 'Base URL', type: 'text' },
    { key: 'latencyProfile', label: 'Latency Profile', type: 'select', options: ['fast', 'medium', 'slow'] },
    { key: 'capabilities', label: 'Capabilities', type: 'tags' },
    { key: 'constraints.costPerMillionInputTokens', label: 'Cost / 1M Input Tokens', type: 'text' },
    { key: 'constraints.costPerMillionOutputTokens', label: 'Cost / 1M Output Tokens', type: 'text' },
    { key: 'constraints.contextWindow', label: 'Context Window', type: 'number' },
  ],
  knowledgebases: [
    { key: 'name', label: 'KB Name', type: 'text', required: true },
    { key: 'description', label: 'Description', type: 'textarea' },
    { key: 'vectorStore.provider', label: 'Vector Store Provider', type: 'select', options: ['qdrant'] },
    { key: 'vectorStore.collectionName', label: 'Collection Name', type: 'text' },
    { key: 'embedding.modelSelectorRef', label: 'Embedding Model Selector', type: 'text', required: true },
    { key: 'embedding.chunkSize', label: 'Chunk Size', type: 'number' },
    { key: 'embedding.chunkOverlap', label: 'Chunk Overlap', type: 'number' },
  ],
  modelselectors: [
    { key: 'name', label: 'Selector Name', type: 'text', required: true },
    { key: 'strategy', label: 'Strategy', type: 'select', options: ['rule-based', 'llm-meta', 'hybrid'] },
    { key: 'fallbackChain', label: 'Fallback Chain', type: 'tags' },
  ],
  agentdeployments: [
    { key: 'name', label: 'Deployment Name', type: 'text', required: true },
    { key: 'agentRef', label: 'Agent', type: 'text', required: true },
    { key: 'inputSource.type', label: 'Input Source', type: 'select', options: ['chat', 'queue', 'pubsub', 'loop'] },
    { key: 'replicas', label: 'Replicas', type: 'number' },
    { key: 'restartPolicy.minBackoffSeconds', label: 'Min Backoff (s)', type: 'number' },
    { key: 'restartPolicy.maxBackoffSeconds', label: 'Max Backoff (s)', type: 'number' },
    { key: 'maxRequestsPerPod', label: 'Max Requests Per Pod', type: 'number' },
    { key: 'toolExecutionTimeoutSec', label: 'Tool Timeout (s)', type: 'number' },
  ],
  agentworkflows: [
    { key: 'name', label: 'Workflow Name', type: 'text', required: true },
    { key: 'description', label: 'Description', type: 'textarea' },
    { key: 'onStepFailure', label: 'On Step Failure', type: 'select', options: ['stop', 'continue'] },
  ],
}

// ── Modal overlay ────────────────────────────────────────────────────────────

interface Props {
  kind: ResourceKind
  name: string
  namespace: string
  onClose: () => void
  onSaved: () => void
}

export function ResourceEditor({ kind, name, namespace, onClose, onSaved }: Props) {
  const [spec, setSpec] = useState<any>(null)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [showDeleteConfirm, setShowDeleteConfirm] = useState(false)

  useEffect(() => {
    setLoading(true)
    void getResource(kind, namespace, name)
      .then((data) => {
        // Extract .spec from the CRD response, defaulting to {}.
        setSpec(data?.spec ?? {})
      })
      .catch(() => setError('Failed to load resource'))
      .finally(() => setLoading(false))
  }, [kind, name, namespace])

  const handleFieldChange = useCallback((field: FieldDef, value: any) => {
    setSpec((prev: any) => {
      const next = { ...prev }
      setNested(next, field.key, value)
      return next
    })
  }, [])

  const save = useCallback(async () => {
    if (!spec) return
    setSaving(true)
    setError(null)
    try {
      await updateResource(kind, namespace, name, spec)
      onSaved()
      onClose()
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Save failed')
    } finally {
      setSaving(false)
    }
  }, [kind, namespace, name, spec, onSaved, onClose])

  const handleDelete = useCallback(async () => {
    try {
      await deleteResource(kind, namespace, name)
      onSaved()
      onClose()
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Delete failed')
    }
  }, [kind, namespace, name, onSaved, onClose])

  if (loading) {
    return <ModalOverlay onClose={onClose}><DSCard padded>Loading…</DSCard></ModalOverlay>
  }

  const fields = RESOURCE_SCHEMAS[kind] ?? []

  return (
    <ModalOverlay onClose={onClose}>
      <div style={editorStyles.root}>
        <div style={editorStyles.header}>
          <h2 style={editorStyles.title}>Edit {kind.slice(0, -1)} / {name}</h2>
          <button type="button" style={editorStyles.closeBtn} onClick={onClose} aria-label="Close editor">
            <Icon icon={ICON.close} size={16} />
          </button>
        </div>

        {error && (
          <div style={editorStyles.errorBanner}>
            <Icon icon={ICON.error} size={14} ariaHidden={true} />
            <span>{error}</span>
          </div>
        )}

        <DSCard padded style={editorStyles.formCard}>
          <div style={editorStyles.fieldGroup}>
            {fields.map((f) => (
              <FieldInput
                key={f.key}
                field={f}
                value={getNested(spec, f.key)}
                onChange={(v) => handleFieldChange(f, v)}
              />
            ))}
          </div>

          <div style={editorStyles.actions}>
            <DSButton variant="secondary" onClick={onClose} disabled={saving}>
              Cancel
            </DSButton>
            <DSButton variant="danger" onClick={() => setShowDeleteConfirm(true)} disabled={saving}>
              <Icon icon={ICON.error} size={14} ariaHidden={true} /> Delete
            </DSButton>
            <DSButton variant="primary" onClick={save} disabled={saving}>
              {saving ? 'Saving…' : 'Save Changes'}
            </DSButton>
          </div>
        </DSCard>

        {/* Status section (read-only) */}
        <h3 style={editorStyles.sectionTitle}>Status (read-only)</h3>
        <DSCard padded style={editorStyles.statusCard}>
          <StatusReadout kind={kind} spec={spec} />
        </DSCard>
      </div>

      {/* Delete confirmation modal */}
      {showDeleteConfirm && (
        <DeleteConfirm
          resourceName={name}
          resourceKind={kind.slice(0, -1)}
          onCancel={() => setShowDeleteConfirm(false)}
          onConfirm={handleDelete}
        />
      )}
    </ModalOverlay>
  )
}

// ── Field input renderers ──────────────────────────────────────────────────

function FieldInput({
  field, value, onChange,
}: {
  field: FieldDef
  value: any
  onChange: (v: any) => void
}) {
  const id = `field-${field.key}`
  const commonProps = {
    label: field.label,
    id,
  }

  switch (field.type) {
    case 'textarea':
      return (
        <div style={f.field}>
          <label style={f.label} htmlFor={id}>{field.label}{field.required && ' *'}</label>
          <textarea
            id={id}
            style={f.textarea}
            value={value ?? ''}
            onChange={(e) => onChange(e.target.value || undefined)}
            placeholder={field.help}
            rows={4}
          />
        </div>
      )
    case 'number':
      return (
        <DSInput
          {...commonProps}
          type="number"
          value={value ?? ''}
          onChange={(e) => onChange(e.target.value ? Number(e.target.value) : undefined)}
        />
      )
    case 'select':
      return (
        <div style={f.field}>
          <label style={f.label} htmlFor={id}>{field.label}</label>
          <select
            id={id}
            style={f.select}
            value={value ?? ''}
            onChange={(e) => onChange(e.target.value || undefined)}
          >
            <option value="">— unset —</option>
            {field.options?.map((opt) => (
              <option key={opt} value={opt}>{opt}</option>
            ))}
          </select>
        </div>
      )
    case 'boolean':
      return (
        <div style={f.field}>
          <label style={f.checkboxRow}>
            <input
              type="checkbox"
              checked={value ?? false}
              onChange={(e) => onChange(e.target.checked)}
              style={f.checkbox}
            />
            <span>{field.label}</span>
          </label>
        </div>
      )
    case 'tags':
      return (
        <TagInput
          label={field.label}
          value={Array.isArray(value) ? value : []}
          onChange={onChange}
          placeholder={`Add ${field.label.toLowerCase()}…`}
        />
      )
    case 'json':
      return (
        <div style={f.field}>
          <label style={f.label} htmlFor={id}>{field.label}</label>
          <textarea
            id={id}
            style={f.textarea}
            value={value ? JSON.stringify(value, null, 2) : ''}
            onChange={(e) => {
              try { onChange(JSON.parse(e.target.value)) } catch { /* invalid JSON */ }
            }}
            rows={6}
          />
        </div>
      )
    default:
      return (
        <DSInput
          {...commonProps}
          value={value ?? ''}
          onChange={(e) => onChange(e.target.value || undefined)}
          placeholder={field.help}
        />
      )
  }
}

// TagInput — chip-based multi-value input for array fields.
function TagInput({
  label, value, onChange, placeholder,
}: {
  label: string
  value: string[]
  onChange: (v: string[]) => void
  placeholder: string
}) {
  const [input, setInput] = useState('')

  const addTag = () => {
    const trimmed = input.trim()
    if (trimmed && !value.includes(trimmed)) {
      onChange([...value, trimmed])
    }
    setInput('')
  }

  const removeTag = (tag: string) => {
    onChange(value.filter((t) => t !== tag))
  }

  return (
    <div style={f.field}>
      <label style={f.label}>{label}</label>
      <div style={f.tagContainer}>
        {value.map((t) => (
          <span key={t} style={f.tag}>
            {t}
            <button
              type="button"
              style={f.tagRemove}
              onClick={() => removeTag(t)}
              aria-label={`Remove ${t}`}
            >
              <Icon icon={ICON.close} size={10} ariaHidden={true} />
            </button>
          </span>
        ))}
        <input
          type="text"
          style={f.tagInput}
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); addTag() } }}
          placeholder={value.length === 0 ? placeholder : ''}
        />
      </div>
    </div>
  )
}

// StatusReadout — displays the status fields of the resource read-only.
function StatusReadout({ kind, spec }: { kind: ResourceKind; spec: any }) {
  const status = spec?.status
  if (!status || typeof status !== 'object') {
    return <span style={f.muted}>No status available (resource may not have a status subresource).</span>
  }
  return (
    <div style={f.field}>
      {Object.entries(status).map(([k, v]) => {
        if (typeof v === 'object' || Array.isArray(v)) return null
        return (
          <div key={k} style={f.statusRow}>
            <span style={f.fieldLabel}>{k}</span>
            <span style={f.fieldValue}>{String(v ?? '—')}</span>
          </div>
        )
      })}
    </div>
  )
}

// DeleteConfirm — modal that requires typing the resource name to confirm.
function DeleteConfirm({
  resourceName, resourceKind, onCancel, onConfirm,
}: {
  resourceName: string
  resourceKind: string
  onCancel: () => void
  onConfirm: () => void
}) {
  const [input, setInput] = useState('')
  const confirmed = input === resourceName

  return (
    <ModalOverlay onClose={onCancel}>
      <DSCard padded style={editorStyles.deleteCard}>
        <h3 style={editorStyles.deleteTitle}>
          <Icon icon={ICON.error} size={16} ariaHidden={true} /> Delete {resourceKind}?
        </h3>
        <p style={editorStyles.deleteBody}>
          Type <strong>{resourceName}</strong> to confirm deletion. This action cannot be undone.
        </p>
        <input
          type="text"
          style={editorStyles.deleteInput}
          value={input}
          onChange={(e) => setInput(e.target.value)}
          placeholder={resourceName}
          autoFocus
        />
        <div style={editorStyles.actions}>
          <DSButton variant="secondary" onClick={onCancel}>Cancel</DSButton>
          <DSButton variant="danger" onClick={onConfirm} disabled={!confirmed}>
            Delete Resource
          </DSButton>
        </div>
      </DSCard>
    </ModalOverlay>
  )
}

// ModalOverlay — full-screen dimmed overlay with focus-trap-ready close on Escape.
function ModalOverlay({ onClose, children }: { onClose: () => void; children: React.ReactNode }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  return (
    <div style={editorStyles.overlay} onClick={onClose}>
      <div style={editorStyles.modal} onClick={(e) => e.stopPropagation()}>
        {children}
      </div>
    </div>
  )
}

// ── Styles ───────────────────────────────────────────────────────────────────

const editorStyles: Record<string, React.CSSProperties> = {
  overlay: {
    position: 'fixed' as const,
    top: 0, left: 0, right: 0, bottom: 0,
    background: 'rgba(0,0,0,.5)',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    zIndex: 1000,
    padding: 24,
  },
  modal: {
    width: '100%',
    maxWidth: 720,
    maxHeight: '80vh',
    overflowY: 'auto' as const,
  },
  root: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: DESIGN.space.lg,
  },
  header: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
  },
  title: {
    fontSize: 20,
    fontWeight: 700,
    color: 'var(--ds-text-primary)',
    margin: 0,
  },
  closeBtn: {
    width: 32,
    height: 32,
    borderRadius: DESIGN.radii.sm,
    border: `1px solid var(--ds-border)`,
    background: 'transparent',
    color: 'var(--ds-text-secondary)',
    cursor: 'pointer',
    display: 'inline-flex',
    alignItems: 'center',
    justifyContent: 'center',
  },
  errorBanner: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
    padding: '8px 12px',
    background: 'rgba(239,68,68,.1)',
    border: `1px solid var(--ds-error-border)`,
    borderRadius: DESIGN.radii.sm,
    color: 'var(--ds-error)',
    fontSize: 12,
  },
  formCard: {
    overflowY: 'auto' as const,
  },
  actions: {
    display: 'flex',
    gap: DESIGN.space.sm,
    justifyContent: 'flex-end' as const,
    marginTop: DESIGN.space.md,
  },
  sectionTitle: {
    fontSize: 13,
    fontWeight: 600,
    color: 'var(--ds-text-secondary)',
    textTransform: 'uppercase',
    letterSpacing: '0.06em',
    margin: 0,
  },
  statusCard: {
    background: 'rgba(148,163,184,.03)',
  },
  deleteCard: {
    background: 'rgba(239,68,68,.05)',
  },
  deleteTitle: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
    fontSize: 16,
    fontWeight: 700,
    color: 'var(--ds-error)',
    margin: 0,
  },
  deleteBody: {
    fontSize: 13,
    color: 'var(--ds-text-secondary)',
    lineHeight: 1.5,
    margin: '12px 0',
  },
  deleteInput: {
    width: '100%',
    boxSizing: 'border-box' as const,
    padding: '8px 10px',
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-error-border)`,
    borderRadius: DESIGN.radii.sm,
    color: 'var(--ds-text-primary)',
    fontFamily: DESIGN.font.mono,
    fontSize: 13,
    marginBottom: DESIGN.space.md,
  },
}

const f: Record<string, React.CSSProperties> = {
  field: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 4,
    marginBottom: DESIGN.space.md,
  },
  label: {
    fontSize: 11,
    fontWeight: 600,
    color: 'var(--ds-text-secondary)',
    textTransform: 'uppercase',
    letterSpacing: '0.06em',
  },
  textarea: {
    padding: '8px 10px',
    background: 'var(--ds-bg)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.sm,
    color: 'var(--ds-text-primary)',
    fontSize: 13,
    fontFamily: DESIGN.font.family,
    outline: 'none',
    resize: 'vertical' as const,
    transitionProperty: 'border-color',
    transitionDuration: '0.15s',
  },
  select: {
    padding: '6px 8px',
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.sm,
    color: 'var(--ds-text-primary)',
    fontSize: 13,
    outline: 'none',
    cursor: 'pointer',
  },
  checkbox: {
    width: 14,
    height: 14,
    accentColor: 'var(--ds-accent)',
    cursor: 'pointer',
  },
  checkboxRow: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
    cursor: 'pointer',
    fontSize: 13,
  },
  tagContainer: {
    display: 'flex',
    flexWrap: 'wrap' as const,
    gap: 4,
    padding: '6px 8px',
    background: 'var(--ds-bg)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.sm,
    minHeight: 32,
    alignItems: 'center',
  },
  tag: {
    display: 'inline-flex',
    alignItems: 'center',
    gap: 4,
    padding: '2px 8px',
    background: 'rgba(59,130,246,.15)',
    color: 'var(--ds-accent)',
    borderRadius: DESIGN.radii.full,
    fontSize: 11,
    fontWeight: 500,
  },
  tagRemove: {
    width: 14,
    height: 14,
    borderRadius: '50%',
    border: 'none',
    background: 'transparent',
    color: 'var(--ds-accent)',
    cursor: 'pointer',
    display: 'inline-flex',
    alignItems: 'center',
    justifyContent: 'center',
    padding: 0,
  },
  tagInput: {
    flex: 1,
    minWidth: 100,
    border: 'none',
    outline: 'none',
    background: 'transparent',
    color: 'var(--ds-text-primary)',
    fontSize: 12,
    fontFamily: DESIGN.font.mono,
  },
  muted: {
    color: 'var(--ds-text-muted)',
    fontSize: 13,
  },
  fieldLabel: {
    fontSize: 11,
    fontWeight: 600,
    color: 'var(--ds-text-secondary)',
  },
  fieldValue: {
    fontSize: 12,
    color: 'var(--ds-text-primary)',
  },
  statusRow: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 3,
    padding: '4px 0',
    borderBottom: `1px solid var(--ds-border-light)`,
  },
  statusRowLast: {
    borderBottom: 'none',
  },
}

// Fix: tagContainer input needs to not grow too large
f.tagInput.flex = 1
f.tagInput.minWidth = 100
