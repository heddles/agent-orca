/**
 * AgentPage — one agent: what it does, a run form generated from its input
 * schema, and for published agents the chat and webhook triggers on further tabs.
 */
import { useEffect, useMemo, useState, type CSSProperties } from 'react'
import { Link } from 'react-router'
import { getAgentManifest, getCatalog, startAgentRun, type AgentManifest, type CatalogEntry } from '../api/catalog'
import { DSButton, DSChip, DSTabBar } from '../lib/primitives'
import { ds, DESIGN } from '../lib/designSystem'
import { ICON } from '../lib/icons'
import { EmptyState, Page, PageHeader, Section } from '../lib/layout'
import { agentPath, ROUTES } from '../lib/routes'
import {
  buildPayload, initialValues, isFormSchema, isPlainTextSchema, parseRawInput, payloadToMessage,
  type FormErrors, type FormValues, type JSONSchema,
} from '../lib/schemaForm'
import { SchemaForm } from './SchemaForm'
import { DeploymentView } from './DeploymentView'
import { AgentTriggers } from './AgentTriggers'
import { formatCost } from './AgentCatalog'

/** The input shape every agent has: one free-text field. */
const DEFAULT_SCHEMA: JSONSchema = {
  type: 'object',
  properties: { input: { type: 'string', title: 'What should the agent do?', description: 'Describe the task in your own words.' } },
  required: ['input'],
}

interface RunFormProps {
  namespace: string
  name: string
  schema: JSONSchema | undefined
  examples: string[]
  onStarted: (runName: string) => void
}

export function RunForm({ namespace, name, schema, examples, onStarted }: RunFormProps) {
  const effective = useMemo<JSONSchema | undefined>(
    () => (isFormSchema(schema) ? { ...schema, properties: withInputTitle(schema) } : undefined),
    [schema],
  )
  const form = effective ?? DEFAULT_SCHEMA
  const formSupported = isFormSchema(schema) || schema === undefined
  const [values, setValues] = useState<FormValues>(() => initialValues(form))
  const [errors, setErrors] = useState<FormErrors>({})
  const [rawMode, setRawMode] = useState(!formSupported)
  const [rawText, setRawText] = useState('')
  const [rawError, setRawError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<string | null>(null)

  useEffect(() => {
    setValues(initialValues(form))
    setErrors({})
  }, [form])

  const plain = isPlainTextSchema(form)
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setFailure(null)
    let message: string
    if (rawMode) {
      const parsed = parseRawInput(schema, rawText)
      setRawError(parsed.error ?? null)
      if (parsed.error) return
      message = payloadToMessage(schema, parsed.payload)
    } else {
      const result = buildPayload(form, values)
      setErrors(result.errors)
      if (!result.payload) return
      message = payloadToMessage(form, result.payload)
    }
    setBusy(true)
    try {
      const run = await startAgentRun(namespace, name, message)
      onStarted(run.run_id)
    } catch (err: unknown) {
      setFailure(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  const useExample = (text: string) => {
    if (rawMode) setRawText(text)
    else if (plain) setValues({ ...values, input: text })
  }

  const showExamples = examples.length > 0 && (rawMode || plain)

  return (
    <form onSubmit={submit} noValidate aria-label={`Run ${name}`} style={s.form}>
      {showExamples && (
        <div style={s.examples}>
          <span style={s.hint}>Examples, click to use:</span>
          {examples.map((x) => (
            <button key={x} type="button" style={s.exampleBtn} onClick={() => useExample(x)}>{x}</button>
          ))}
        </div>
      )}
      {rawMode ? (
        <div style={s.field}>
          <label htmlFor="run-raw" style={s.label}>Input (JSON)</label>
          <p style={s.help}>
            {formSupported ? 'Write the input as JSON.' : 'This agent’s input has a shape the form cannot show, so write it as JSON.'}
          </p>
          <textarea
            id="run-raw"
            style={{ ...s.textarea, fontFamily: DESIGN.font.mono }}
            spellCheck={false}
            value={rawText}
            aria-invalid={rawError ? true : undefined}
            aria-describedby={rawError ? 'run-raw-error' : undefined}
            onChange={(e) => setRawText(e.target.value)}
          />
          {rawError && <p id="run-raw-error" role="alert" style={s.error}>{rawError}</p>}
        </div>
      ) : (
        <SchemaForm schema={form} values={values} onChange={setValues} errors={errors} />
      )}
      {failure && <p role="alert" style={s.error}>Could not start the run: {failure}</p>}
      <div style={s.actions}>
        <DSButton type="submit" variant="primary" disabled={busy}>{busy ? 'Starting…' : 'Run agent'}</DSButton>
        {formSupported && !plain && (
          <DSButton type="button" variant="ghost" onClick={() => setRawMode((m) => !m)}>
            {rawMode ? 'Use the form' : 'Edit as JSON'}
          </DSButton>
        )}
      </div>
    </form>
  )
}

/** Gives the default `input` field a friendlier label than its name. */
function withInputTitle(schema: JSONSchema): Record<string, JSONSchema> {
  const props = { ...(schema.properties ?? {}) }
  if (props.input && !props.input.title) props.input = { ...props.input, title: 'What should the agent do?' }
  return props
}

interface Props {
  namespace: string
  name: string
  tab: string
  onTabChange: (tab: string) => void
  onNavigateToRun: (runName: string, namespace: string) => void
}

export function AgentPage({ namespace, name, tab, onTabChange, onNavigateToRun }: Props) {
  const [manifest, setManifest] = useState<AgentManifest | null>(null)
  const [entry, setEntry] = useState<CatalogEntry | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let live = true
    setManifest(null)
    setError(null)
    getAgentManifest(namespace, name)
      .then((m) => { if (live) setManifest(m) })
      .catch((e: unknown) => { if (live) setError(e instanceof Error ? e.message : String(e)) })
    getCatalog(namespace)
      .then((list) => { if (live) setEntry(list.find((a) => a.name === name && a.namespace === namespace) ?? null) })
      .catch(() => {})
    return () => { live = false }
  }, [namespace, name])

  if (error) {
    return (
      <Page>
        <EmptyState icon={ICON.agent} title="This agent could not be opened" action={<Link to={ROUTES.agents} style={{ color: ds.accent }}>Back to agents</Link>}>
          It may not exist, or it belongs to another team. ({error})
        </EmptyState>
      </Page>
    )
  }
  if (!manifest) return <div role="status" style={{ padding: 24, color: ds.textMuted, fontSize: 13 }}>Loading agent…</div>

  const published = entry?.published ?? false
  const active = published && (tab === 'chat' || tab === 'triggers') ? tab : 'run'
  const tabs = [{ key: 'run', label: 'Run' }, ...(published ? [{ key: 'chat', label: 'Chat' }, { key: 'triggers', label: 'Triggers' }] : [])]
  const examples = manifest.example_inputs ?? entry?.exampleInputs ?? []

  return (
    <Page>
      <nav aria-label="Breadcrumb" style={{ padding: '10px 24px 0', fontSize: 12 }}>
        <Link to={ROUTES.agents} style={{ color: ds.accent, textDecoration: 'none' }}>Agents</Link>
        <span style={{ color: ds.textMuted }}> / </span>
        <span style={{ color: ds.textSecondary }}>{name}</span>
      </nav>
      <PageHeader
        title={name}
        description={firstLine(manifest.description) || 'No description yet.'}
        actions={<Link to={agentPath(namespace, name)} style={{ color: ds.accent, fontSize: 12 }}>How it is set up</Link>}
      />
      <div style={s.meta}>
        {entry && <DSChip variant={published ? 'success' : 'neutral'} size="sm" dot>{published ? 'Published' : 'Not published'}</DSChip>}
        {entry?.maintainer && <DSChip size="sm">Maintained by: {entry.maintainer}</DSChip>}
        {entry && (
          <DSChip size="sm">
            {entry.typicalCostUSD === null ? 'No cost data yet' : `Typically ${formatCost(entry.typicalCostUSD)} per run`}
          </DSChip>
        )}
      </div>
      <div style={{ padding: '8px 24px 0' }}>
        <DSTabBar tabs={tabs} active={active} onChange={onTabChange} />
      </div>
      {active === 'run' ? (
        <div role="tabpanel" id="run-panel" aria-label="Run">
          <Section title="Start a run">
            <div style={{ maxWidth: 640 }}>
              <RunForm
                namespace={namespace}
                name={name}
                schema={manifest.input_schema}
                examples={examples}
                onStarted={(runName) => onNavigateToRun(runName, namespace)}
              />
            </div>
          </Section>
        </div>
      ) : active === 'triggers' ? (
        <div role="tabpanel" id="triggers-panel" aria-label="Triggers">
          <AgentTriggers namespace={namespace} name={name} />
        </div>
      ) : (
        <div role="tabpanel" id="chat-panel" aria-label="Chat" style={{ flex: 1, minHeight: 480, display: 'flex' }}>
          <DeploymentView namespace={namespace} name={entry?.deployment ?? name} onNavigateToRun={onNavigateToRun} />
        </div>
      )}
    </Page>
  )
}

function firstLine(text: string): string {
  const t = text.trim().split(/\n\s*\n/)[0].replace(/\s+/g, ' ')
  return t.length > 240 ? `${t.slice(0, 239).trimEnd()}…` : t
}

const s: Record<string, CSSProperties> = {
  form: { display: 'flex', flexDirection: 'column', gap: 14 },
  meta: { display: 'flex', gap: 8, flexWrap: 'wrap', padding: '0 24px' },
  field: { display: 'flex', flexDirection: 'column', gap: 4 },
  label: { fontSize: 12, fontWeight: 600, color: ds.textPrimary },
  help: { margin: 0, fontSize: 12, color: ds.textSecondary },
  hint: { fontSize: 11, color: ds.textMuted },
  error: { margin: 0, fontSize: 12, color: ds.error },
  textarea: {
    background: ds.surface, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, padding: '8px 10px',
    color: ds.textPrimary, fontSize: 13, minHeight: 140, width: '100%', boxSizing: 'border-box', resize: 'vertical',
  },
  actions: { display: 'flex', gap: 8 },
  examples: { display: 'flex', gap: 6, flexWrap: 'wrap', alignItems: 'center' },
  exampleBtn: {
    background: 'var(--ds-muted-bg)', border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md,
    color: ds.textSecondary, fontSize: 12, padding: '3px 8px', cursor: 'pointer', textAlign: 'left',
  },
}
