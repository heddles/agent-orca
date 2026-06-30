import { useEffect, useState } from 'react'
import {
  createAgent,
  listModelSelectors,
  listTools,
  listMCPServers,
  type ModelSelectorSummary,
  type ToolSummary,
  type MCPServerSummary,
} from '../api/sse'

const FRAMEWORKS = [
  'openai-compatible',
  'autogen',
  'semantic-kernel',
  'langgraph',
  'shim',
]

// Pre-defined agent templates
const AGENT_TEMPLATES = [
  {
    id: 'support-bot',
    name: 'Support Bot',
    description: 'Customer support agent with tool access',
    icon: '🎧',
    ociRef: 'python:3.12-slim',
    systemPrompt: 'You are a helpful customer support agent. Be concise, friendly, and solve the user\'s problem efficiently.',
    framework: 'openai-compatible',
  },
  {
    id: 'research-assistant',
    name: 'Research Assistant',
    description: 'Deep research and analysis agent',
    icon: '🔍',
    ociRef: 'python:3.12-slim',
    systemPrompt: 'You are a research assistant. Gather information thoroughly and present findings in a clear, organized manner.',
    framework: 'openai-compatible',
  },
  {
    id: 'code-reviewer',
    name: 'Code Reviewer',
    description: 'Code review and refactoring suggestions',
    icon: '💻',
    ociRef: 'python:3.12-slim',
    systemPrompt: 'You are a senior software engineer. Review code for bugs, performance issues, and best practices. Provide actionable feedback.',
    framework: 'openai-compatible',
  },
  {
    id: 'data-analyst',
    name: 'Data Analyst',
    description: 'Data analysis and visualization',
    icon: '📊',
    ociRef: 'python:3.12-slim',
    systemPrompt: 'You are a data analyst. Analyze data carefully and create clear visualizations. Explain insights in plain language.',
    framework: 'openai-compatible',
  },
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
  const [tools, setTools] = useState<ToolSummary[]>([])
  const [mcps, setMCPs] = useState<MCPServerSummary[]>([])
  const [selectedTools, setSelectedTools] = useState<string[]>([])
  const [selectedMCPs, setSelectedMCPs] = useState<string[]>([])
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [warning, setWarning] = useState<string | null>(null)
  const [mode, setMode] = useState<'templates' | 'custom'>('templates')

  useEffect(() => {
    Promise.all([
      listModelSelectors(namespace),
      listTools(namespace),
      listMCPServers(namespace),
    ]).then(([sel, t, m]) => {
      setSelectors(sel)
      if (sel.length > 0 && !modelSelectorRef) setModelSelectorRef(sel[0].name)
      setTools(t.filter((tool) => tool.ready))
      setMCPs(m.filter((mcp) => mcp.ready))
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
          setTimeout(() => onCreated(), 2000)
        } else {
          onCreated()
        }
      })
      .catch((e: Error) => setError(e.message))
      .finally(() => setSubmitting(false))
  }

  const applyTemplate = (template: typeof AGENT_TEMPLATES[0]) => {
    setName('')
    setOciRef(template.ociRef)
    setFramework(template.framework)
    setSystemPrompt(template.systemPrompt)
    setMode('custom')
  }

  const toggleTool = (toolName: string) => {
    setSelectedTools((prev) =>
      prev.includes(toolName)
        ? prev.filter((t) => t !== toolName)
        : [...prev, toolName]
    )
  }

  const toggleMCP = (mcpName: string) => {
    setSelectedMCPs((prev) =>
      prev.includes(mcpName)
        ? prev.filter((m) => m !== mcpName)
        : [...prev, mcpName]
    )
  }

  return (
    <div style={s.overlay} onClick={onClose}>
      <div style={s.panel} onClick={(e) => e.stopPropagation()}>
        <div style={s.header}>
          <div style={s.titleRow}>
            <span style={s.title}>Create Agent</span>
            {mode === 'custom' && (
              <span style={s.badge}>Custom</span>
            )}
          </div>
          <button style={s.closeBtn} onClick={onClose}>{'✕'}</button>
        </div>

        {/* Mode Tabs */}
        <div style={s.modeTabs}>
          <button
            style={{ ...s.modeTab, ...(mode === 'templates' ? s.modeTabActive : {}) }}
            onClick={() => setMode('templates')}
          >
            Templates
          </button>
          <button
            style={{ ...s.modeTab, ...(mode === 'custom' ? s.modeTabActive : {}) }}
            onClick={() => setMode('custom')}
          >
            Custom
          </button>
        </div>

        <div style={s.body}>
          {mode === 'templates' ? (
            // Template Gallery
            <div style={s.templatesGrid}>
              {AGENT_TEMPLATES.map((template) => (
                <div
                  key={template.id}
                  style={s.templateCard}
                  onClick={() => applyTemplate(template)}
                >
                  <div style={s.templateIcon}>{template.icon}</div>
                  <div style={s.templateName}>{template.name}</div>
                  <div style={s.templateDesc}>{template.description}</div>
                </div>
              ))}
            </div>
          ) : (
            // Custom Form
            <>
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

              <Field label="Tools">
                <div style={s.selectorGrid}>
                  {tools.map((tool) => {
                    const selected = selectedTools.includes(tool.name)
                    return (
                      <button
                        key={tool.name}
                        style={{ ...s.selectorItem, ...(selected ? s.selectorItemSelected : {}) }}
                        onClick={() => toggleTool(tool.name)}
                        title={tool.description}
                      >
                        <span style={s.selectorName}>{tool.name}</span>
                        <span style={s.selectorType}>{tool.type}</span>
                      </button>
                    )
                  })}
                  {tools.length === 0 && (
                    <span style={s.muted}>No tools available</span>
                  )}
                </div>
                {selectedTools.length > 0 && (
                  <span style={s.hint}>Selected: {selectedTools.join(', ')}</span>
                )}
              </Field>

              <Field label="MCP Servers">
                <div style={s.selectorGrid}>
                  {mcps.map((mcp) => {
                    const selected = selectedMCPs.includes(mcp.name)
                    return (
                      <button
                        key={mcp.name}
                        style={{ ...s.selectorItem, ...(selected ? s.selectorItemSelected : {}) }}
                        onClick={() => toggleMCP(mcp.name)}
                        title={`${mcp.toolCount} tools available`}
                      >
                        <span style={s.selectorName}>{mcp.name}</span>
                        <span style={s.selectorType}>{mcp.transport}</span>
                      </button>
                    )
                  })}
                  {mcps.length === 0 && (
                    <span style={s.muted}>No MCP servers available</span>
                  )}
                </div>
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
            </>
          )}
        </div>

        <div style={s.footer}>
          <button style={s.cancelBtn} onClick={onClose}>Cancel</button>
          {mode === 'custom' && (
            <button
              style={{ ...s.createBtn, opacity: canSubmit ? 1 : 0.4 }}
              disabled={!canSubmit}
              onClick={handleSubmit}
            >
              {submitting ? 'Creating...' : 'Create Agent'}
            </button>
          )}
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
    width: 420,
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
  titleRow: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
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
  modeTabs: {
    display: 'flex',
    borderBottom: '1px solid #1e293b',
  },
  modeTab: {
    flex: 1,
    padding: '10px 16px',
    background: 'transparent',
    border: 'none',
    borderBottom: '2px solid transparent',
    color: '#64748b',
    fontSize: 13,
    fontWeight: 500,
    cursor: 'pointer',
  },
  modeTabActive: {
    color: '#3b82f6',
    borderBottomColor: '#3b82f6',
  },
  body: {
    flex: 1,
    overflowY: 'auto',
    padding: 20,
    display: 'flex',
    flexDirection: 'column',
    gap: 16,
  },
  templatesGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(2, 1fr)',
    gap: 12,
  },
  templateCard: {
    background: '#1e293b',
    border: '1px solid #334155',
    borderRadius: 12,
    padding: 16,
    cursor: 'pointer',
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'center',
    gap: 8,
    transition: 'all 0.15s',
  },
  templateIcon: { fontSize: 32 },
  templateName: { fontSize: 14, fontWeight: 600, color: '#f1f5f9' },
  templateDesc: { fontSize: 11, color: '#64748b', textAlign: 'center' },
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
  muted: { color: '#475569', fontSize: 12 },
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
  selectorGrid: {
    display: 'flex',
    flexWrap: 'wrap',
    gap: 6,
    marginTop: -2,
  },
  selectorItem: {
    padding: '6px 10px',
    borderRadius: 6,
    border: '1px solid #334155',
    background: '#1e293b',
    color: '#94a3b8',
    fontSize: 12,
    cursor: 'pointer',
    display: 'flex',
    flexDirection: 'column',
    gap: 2,
    minWidth: 100,
  },
  selectorItemSelected: {
    background: 'rgba(59,130,246,.15)',
    borderColor: '#3b82f6',
    color: '#f1f5f9',
  },
  selectorName: { fontSize: 12, fontWeight: 500 },
  selectorType: { fontSize: 10, opacity: 0.6 },
  badge: {
    fontSize: 11,
    padding: '2px 8px',
    borderRadius: 4,
    background: 'rgba(59,130,246,.15)',
    color: '#3b82f6',
    fontWeight: 600,
  },
}