/**
 * WorkflowView — output-first view for an AgentWorkflow.
 * Shows: header → budget bar → horizontal step pipeline → selected step output → step traces accordion.
 */
import React, { useEffect, useRef, useState } from 'react'
import { STREAM_EVENT_TYPE } from '../contracts/events'
import { answerClarification, getRun, getWorkflow, subscribeToRunStream, type AgentWorkflowDetail, type TraceEntry, type TraceEvent, type WorkflowStepSummary } from '../api/sse'
import { StatusBadge } from './StatusBadge'
import { TraceAccordion } from './TraceAccordion'
import { DESIGN } from '../lib/designSystem'
import { Icon, ICON } from '../lib/icons'

interface Props {
  name: string
  namespace: string
}

export function WorkflowView({ name, namespace }: Props) {
  const [detail, setDetail] = useState<AgentWorkflowDetail | null>(null)
  const [selectedStep, setSelectedStep] = useState<WorkflowStepSummary | null>(null)
  const [inputOpen, setInputOpen] = useState(true)
  const [outputOpen, setOutputOpen] = useState(true)
  const [clarifyQuestion, setClarifyQuestion] = useState<string | null>(null)
  const [clarifyRunName, setClarifyRunName] = useState<string | null>(null)
  const [clarifyInput, setClarifyInput] = useState('')
  const [clarifySubmitting, setClarifySubmitting] = useState(false)
  const [stepTraceEntries, setStepTraceEntries] = useState<TraceEntry[]>([])
  const stepTraceUnsubRef = useRef<(() => void) | null>(null)
  const stepChildUnsubsRef = useRef<Array<() => void>>([])
  const stepTraceCounter = useRef(0)

  useEffect(() => {
    setDetail(null)
    setSelectedStep(null)

    const load = () => {
      getWorkflow(namespace, name)
        .then((d) => {
          setDetail(d)
          // Auto-select first step with output, or first step.
          if (d.steps.length > 0) {
            setSelectedStep((prev) => {
              if (prev) {
                const updated = d.steps.find((s) => s.name === prev.name)
                return updated ?? prev
              }
              return d.steps.find((s) => s.output) ?? d.steps[0]
            })
          }
        })
        .catch(() => {})
    }

    load()

    // Poll while running.
    const interval = setInterval(() => {
      if (detail?.phase === 'Running' || detail?.phase === 'Pending') {
        load()
      }
    }, 2000)

    return () => clearInterval(interval)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [name, namespace])

  // Fetch clarify question when the selected step is WaitingForInput.
  useEffect(() => {
    if (selectedStep?.phase !== 'WaitingForInput' || !selectedStep.agentRunRef) {
      setClarifyQuestion(null)
      setClarifyRunName(null)
      return
    }
    getRun(selectedStep.agentRunRef, namespace).then((run) => {
      if (run.clarifyQuestion) {
        setClarifyQuestion(run.clarifyQuestion)
        setClarifyRunName(selectedStep.agentRunRef!)
      }
    }).catch(() => {})
  }, [selectedStep?.phase, selectedStep?.agentRunRef, namespace])

  // Subscribe to the selected step's run stream to show its tool calls.
  useEffect(() => {
    stepTraceUnsubRef.current?.()
    stepChildUnsubsRef.current.forEach((fn) => fn())
    stepChildUnsubsRef.current = []
    setStepTraceEntries([])
    stepTraceCounter.current = 0

    const runRef = selectedStep?.agentRunRef
    if (!runRef) return

    const makeHandler = (childRunName?: string) => (event: TraceEvent) => {
      // Skip token events entirely (too noisy)
      if (event.type === STREAM_EVENT_TYPE.token) return
      
      // Handle error events separately
      if (event.type === STREAM_EVENT_TYPE.error) {
        if (event.message === 'Stream connection lost') return
        setStepTraceEntries((prev) => [...prev, { id: stepTraceCounter.current++, event, ts: new Date().toISOString(), childRunName }])
        return
      }
      
      // Handle modelSelected events
      if (event.type === STREAM_EVENT_TYPE.modelSelected) {
        const isCandidate = event.reason.includes('configured provider')
        if (!isCandidate) {
          setStepTraceEntries((prev) => [...prev, { id: stepTraceCounter.current++, event, ts: new Date().toISOString(), childRunName }])
        }
        return
      }
      
      // For all other events, add to trace
      setStepTraceEntries((prev) => [...prev, { id: stepTraceCounter.current++, event, ts: new Date().toISOString(), childRunName }])
    }

    stepTraceUnsubRef.current = subscribeToRunStream(runRef, makeHandler(), namespace)

    getRun(runRef, namespace).then((detail) => {
      for (const childName of detail.childRunRefs ?? []) {
        const unsub = subscribeToRunStream(childName, makeHandler(childName), namespace)
        stepChildUnsubsRef.current.push(unsub)
      }
    }).catch(() => {})

    return () => {
      stepTraceUnsubRef.current?.()
      stepChildUnsubsRef.current.forEach((fn) => fn())
      stepChildUnsubsRef.current = []
    }
  }, [selectedStep?.agentRunRef, namespace])

  // Keep polling while running or waiting for input.
  useEffect(() => {
    if (detail?.phase !== 'Running' && detail?.phase !== 'Pending') return
    const timer = setInterval(() => {
      getWorkflow(namespace, name)
        .then((d) => {
          setDetail(d)
          setSelectedStep((prev) => {
            if (!prev) return d.steps[0] ?? null
            return d.steps.find((s) => s.name === prev.name) ?? prev
          })
        })
        .catch(() => {})
    }, 2000)
    return () => clearInterval(timer)
  }, [detail?.phase, name, namespace])

  if (!detail) {
    return <div style={s.loading}>Loading workflow…</div>
  }

  const budget = detail as AgentWorkflowDetail & { budgetCap?: string }
  const spendNum = parseFloat(detail.totalSpendUSD || '0')
  // We don't have budgetCap in the API response, so just show the spend bar relative to some max.
  const budgetPct = Math.min((spendNum / 0.5) * 100, 100)

  const elapsed = (() => {
    if (!detail.startTime) return null
    const end = detail.completionTime ? new Date(detail.completionTime) : new Date()
    const secs = Math.round((end.getTime() - new Date(detail.startTime).getTime()) / 1000)
    return secs >= 60 ? `${Math.round(secs / 60)}m ${secs % 60}s` : `${secs}s`
  })()

  const submitClarifyAnswer = () => {
    const answer = clarifyInput.trim()
    if (!answer || !clarifyRunName) return
    setClarifySubmitting(true)
    answerClarification(clarifyRunName, answer, namespace)
      .then(() => {
        // A continuation run was created. The workflow controller will
        // follow the ContinuationRunRef automatically.
        setClarifyQuestion(null)
        setClarifyRunName(null)
        setClarifyInput('')
      })
      .catch(() => {})
      .finally(() => setClarifySubmitting(false))
  }

  return (
    <div style={s.root}>
      {/* Header */}
      <div style={s.header}>
        <div>
          <div style={s.title}>{name}</div>
          {detail.description && <div style={s.description}>{detail.description}</div>}
          <div style={s.subtitle}>
            {detail.stepCount} steps · {namespace}
            {detail.completionTime && ' · completed'}
          </div>
        </div>
        <div style={s.stats}>
          <StatusBadge phase={detail.phase} />
          {elapsed && <div style={s.chip}><Icon icon={ICON.timer} size={12} ariaHidden={true} /> <span style={s.chipVal}>{elapsed}</span></div>}
          <div style={s.chip}><Icon icon={ICON.cost} size={12} ariaHidden={true} /> <span style={s.chipVal}>${detail.totalSpendUSD || '0.0000'}</span></div>
        </div>
      </div>

      {/* Budget bar */}
      <div style={s.budgetWrap}>
        <div style={s.budgetLabel}>
          <span>Spend</span>
          <span>${detail.totalSpendUSD || '0.0000'}</span>
        </div>
        <div style={s.budgetTrack}>
          <div style={{ ...s.budgetFill, width: `${budgetPct}%` }} />
        </div>
      </div>

      {/* Horizontal step pipeline */}
      <div style={s.pipeline}>
        {detail.steps.map((step, i) => (
          <div key={step.name} style={s.pipelineItem}>
            {i > 0 && <div style={s.connector}><Icon icon={ICON.routed} size={16} ariaHidden={true} /></div>}
            <StepCard
              step={step}
              active={selectedStep?.name === step.name}
              onClick={() => setSelectedStep(step)}
            />
          </div>
        ))}
      </div>

      {/* Selected step detail */}
      {selectedStep && (
        <>
          {/* Input */}
          {selectedStep.input && (
            <div style={s.stepOutput}>
              <button type="button" style={{ ...s.stepOutputHeader, cursor: 'pointer', userSelect: 'none', background: 'transparent', border: 'none', color: 'inherit', font: 'inherit', textAlign: 'left' }} onClick={() => setInputOpen((prev: boolean) => !prev)} aria-expanded={inputOpen} aria-controls="input-body">
                <div style={{ ...s.stepOutputLabel, display: 'flex', alignItems: 'center', gap: 8 }}>
                  <Icon icon={ICON.chevronRight} size={11} style={{ ...s.chevron, transform: inputOpen ? 'rotate(90deg)' : undefined }} ariaHidden={true} />
                  Input: <span style={{ color: 'var(--ds-text-primary)' }}>{selectedStep.name}</span>
                </div>
                <button
                  type="button"
                  style={s.iconBtn}
                  onClick={(e: React.MouseEvent) => { e.stopPropagation(); navigator.clipboard.writeText(selectedStep.input!) }}
                  title="Copy input"
                  aria-label="Copy input"
                >
                  <Icon icon={ICON.copy} size={14} />
                </button>
              </button>
              {inputOpen && <div id="input-body" style={s.stepOutputBody}>{selectedStep.input}</div>}
            </div>
          )}

          {/* Clarification card */}
          {selectedStep.phase === 'WaitingForInput' && clarifyQuestion && clarifyRunName && (
            <div style={s.clarifyCard}>
              <div style={s.clarifyTitle}>Agent needs your input</div>
              <div style={s.clarifyBody}>{clarifyQuestion}</div>
              <div style={s.clarifyInputArea}>
                <textarea
                  style={s.clarifyTextarea}
                  value={clarifyInput}
                  onChange={(e: React.ChangeEvent<HTMLTextAreaElement>) => setClarifyInput(e.target.value)}
                  onKeyDown={(e: React.KeyboardEvent) => {
                    if (e.key === 'Enter' && !e.shiftKey) {
                      e.preventDefault()
                      submitClarifyAnswer()
                    }
                  }}
                  placeholder="Type your answer…"
                  rows={2}
                  disabled={clarifySubmitting}
                />
                <button
                  style={{ ...s.clarifyBtn, opacity: clarifyInput.trim() && !clarifySubmitting ? 1 : 0.4 }}
                  disabled={!clarifyInput.trim() || clarifySubmitting}
                  onClick={submitClarifyAnswer}
                >
                  {clarifySubmitting ? '…' : 'Send'}
                </button>
              </div>
            </div>
          )}

          {/* Output */}
          <div style={s.stepOutput}>
            <button type="button" style={{ ...s.stepOutputHeader, cursor: 'pointer', userSelect: 'none', background: 'transparent', border: 'none', color: 'inherit', font: 'inherit', textAlign: 'left' }} onClick={() => setOutputOpen((prev: boolean) => !prev)} aria-expanded={outputOpen} aria-controls="output-step-body">
              <div style={{ ...s.stepOutputLabel, display: 'flex', alignItems: 'center', gap: 8 }}>
                <Icon icon={ICON.chevronRight} size={11} style={{ ...s.chevron, transform: outputOpen ? 'rotate(90deg)' : undefined }} ariaHidden={true} />
                Output: <span style={{ color: 'var(--ds-text-primary)' }}>{selectedStep.name}</span>
              </div>
              {selectedStep.output && (
                <button
                  type="button"
                  style={s.iconBtn}
                  onClick={(e: React.MouseEvent) => { e.stopPropagation(); navigator.clipboard.writeText(selectedStep.output!) }}
                  title="Copy output"
                  aria-label="Copy output"
                >
                  <Icon icon={ICON.copy} size={14} />
                </button>
              )}
            </button>
            {outputOpen && (
              <div id="output-step-body" style={s.stepOutputBody}>
                {selectedStep.output
                  ? selectedStep.output
                  : selectedStep.phase === 'WaitingForInput'
                  ? <span style={{ color: 'var(--ds-warning)', fontStyle: 'italic' }}>Waiting for human input…</span>
                  : selectedStep.phase === 'Running'
                  ? <span style={{ color: 'var(--ds-text-secondary)', fontStyle: 'italic' }}>Running…</span>
                  : selectedStep.phase === 'Failed'
                  ? <span style={{ color: 'var(--ds-error)' }}>{selectedStep.failureReason || 'Step failed'}</span>
                  : <span style={{ color: 'var(--ds-text-muted)', fontStyle: 'italic' }}>Waiting to run</span>
                }
              </div>
            )}
          </div>
        </>
      )}

      {/* Step trace */}
      <TraceAccordion
        entries={stepTraceEntries}
        streaming={selectedStep?.phase === 'Running' || selectedStep?.phase === 'Pending'}
      />
    </div>
  )
}

function StepCard({
  step,
  active,
  onClick,
}: {
  step: WorkflowStepSummary
  active: boolean
  onClick: () => void
}) {
  return (
    <button
      type="button"
      style={{
        ...s.stepCard,
        ...(active ? s.stepCardActive : {}),
      }}
      onClick={onClick}
    >
      <div style={{ ...s.stepSpend, fontVariantNumeric: 'tabular-nums' }}>{step.spendUSD ? `$${step.spendUSD}` : ''}</div>
      <StatusBadge phase={step.phase} />
      <div style={s.stepName}>{step.name}</div>
      {step.agentRef && <div style={s.stepAgent}>{step.agentRef}</div>}
      {step.output && (
        <div style={s.stepPreview}>"{step.output.slice(0, 100)}{step.output.length > 100 ? '…' : ''}"</div>
      )}
    </button>
  )
}

const s: Record<string, React.CSSProperties> = {
  root: {
    flex: 1,
    overflowY: 'auto',
    padding: `${DESIGN.space.xl} ${DESIGN.space.xxl}`,
    display: 'flex',
    flexDirection: 'column',
    gap: DESIGN.space.xl,
  },
  loading: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    height: '100%',
    color: 'var(--ds-text-muted)',
    fontSize: 14,
  },
  header: {
    display: 'flex',
    alignItems: 'flex-start',
    justifyContent: 'space-between',
    flexWrap: 'wrap',
    gap: 12,
  },
  title: { fontSize: 18, fontWeight: 700, color: 'var(--ds-text-primary)' },
  description: { fontSize: 13, color: 'var(--ds-text-secondary)', marginTop: 4, lineHeight: 1.5 },
  subtitle: { fontSize: 12, color: 'var(--ds-text-muted)', marginTop: 4 },
  stats: { display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' },
  chip: {
    background: 'var(--ds-surface)',
    border: '1px solid var(--ds-border)',
    borderRadius: 6,
    padding: '5px 10px',
    display: 'flex',
    alignItems: 'center',
    gap: 6,
    fontSize: 12,
    color: 'var(--ds-text-secondary)',
  },
  chipVal: { color: 'var(--ds-text-primary)', fontWeight: 500 },
  budgetWrap: {
    background: 'var(--ds-surface)',
    border: '1px solid var(--ds-border)',
    borderRadius: 8,
    padding: '12px 16px',
  },
  budgetLabel: {
    fontSize: 11,
    color: 'var(--ds-text-secondary)',
    marginBottom: 8,
    display: 'flex',
    justifyContent: 'space-between',
  },
  budgetTrack: {
    height: 6,
    background: 'rgba(255,255,255,.06)',
    borderRadius: 3,
    overflow: 'hidden',
  },
  budgetFill: {
    height: '100%',
    background: 'linear-gradient(90deg, var(--ds-success), var(--ds-accent))',
    borderRadius: 3,
    transition: 'width 0.5s ease',
  },
  pipeline: {
    display: 'flex',
    alignItems: 'stretch',
    overflowX: 'auto',
    paddingBottom: 4,
    gap: 0,
  },
  pipelineItem: {
    display: 'flex',
    alignItems: 'center',
    flexShrink: 0,
  },
  connector: {
    padding: '0 8px',
    color: 'var(--ds-text-muted)',
    fontSize: 16,
    alignSelf: 'center',
    paddingBottom: 16,
  },
  stepCard: {
    background: 'var(--ds-surface)',
    border: '1px solid var(--ds-border)',
    borderRadius: 12,
    padding: '14px 16px',
    width: 180,
    cursor: 'pointer',
    transitionProperty: 'border-color, background-color, box-shadow, transform', transitionDuration: '0.15s', transitionTimingFunction: 'ease',
    position: 'relative',
    display: 'flex',
    flexDirection: 'column',
    gap: 4,
  },
  stepCardActive: {
    borderColor: 'var(--ds-accent)',
    background: 'rgba(59,130,246,.08)',
    boxShadow: '0 0 0 2px rgba(59,130,246,.15)',
  },
  stepSpend: {
    position: 'absolute',
    top: 10,
    right: 12,
    fontSize: 10,
    color: 'var(--ds-text-muted)',
  },
  stepName: { fontSize: 12, fontWeight: 700, color: 'var(--ds-text-primary)', marginTop: 6 },
  stepAgent: { fontSize: 11, color: 'var(--ds-text-muted)' },
  stepPreview: {
    fontSize: 11,
    color: 'var(--ds-text-secondary)',
    lineHeight: 1.5,
    fontStyle: 'italic',
    overflow: 'hidden',
    display: '-webkit-box',
    WebkitLineClamp: 3,
    WebkitBoxOrient: 'vertical',
    marginTop: 6,
  },
  stepOutput: {
    background: 'var(--ds-surface)',
    border: '1px solid var(--ds-border)',
    borderRadius: 12,
    overflow: 'hidden',
  },
  stepOutputHeader: {
    padding: '12px 16px',
    borderBottom: '1px solid var(--ds-border)',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
  },
  stepOutputLabel: { fontSize: 12, fontWeight: 600, color: 'var(--ds-text-secondary)' },
  iconBtn: {
    width: 28,
    height: 28,
    borderRadius: 5,
    border: '1px solid var(--ds-border)',
    background: 'none',
    color: 'var(--ds-text-secondary)',
    cursor: 'pointer',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    fontSize: 13,
  },
  stepOutputBody: {
    padding: 20,
    fontSize: 14,
    lineHeight: 1.7,
    color: 'var(--ds-text-primary)',
    whiteSpace: 'pre-wrap',
    wordBreak: 'break-word',
    maxHeight: 300,
    overflowY: 'auto',
  },
  accordion: {
    background: 'var(--ds-surface)',
    border: '1px solid var(--ds-border)',
    borderRadius: 8,
    overflow: 'hidden',
  },
  accordionHeader: {
    padding: '11px 16px',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    cursor: 'pointer',
    userSelect: 'none',
  },
  accordionTitle: {
    fontSize: 12,
    fontWeight: 600,
    color: 'var(--ds-text-secondary)',
    display: 'flex',
    alignItems: 'center',
    gap: 8,
  },
  chevron: {
    fontSize: 11,
    color: 'var(--ds-text-secondary)',
    transition: 'transform 0.2s',
  },
  accordionBody: {
    padding: '14px 16px',
    borderTop: '1px solid var(--ds-border)',
    display: 'flex',
    flexDirection: 'column',
    gap: 6,
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", "Consolas", monospace',
    fontSize: 12,
  },
  traceRow: {
    display: 'flex',
    alignItems: 'center',
    gap: 10,
    color: 'var(--ds-text-secondary)',
  },
  traceIcon: { color: 'var(--ds-accent)' },
  tracePhase: {
    padding: '1px 6px',
    borderRadius: 4,
    background: 'var(--ds-muted-bg)',
    fontSize: 10,
  },
  traceSpend: { color: 'var(--ds-text-muted)', fontSize: 10 },
  traceRun: { color: 'var(--ds-text-muted)', fontSize: 10 },
  clarifyCard: {
    background: 'var(--ds-warning-bg)',
    border: '1px solid var(--ds-warning-border)',
    borderRadius: 12,
    padding: '16px 20px',
  },
  clarifyTitle: {
    fontSize: 12,
    fontWeight: 600,
    color: 'var(--ds-warning)',
    marginBottom: 8,
  },
  clarifyBody: {
    fontSize: 14,
    color: '#fcd34d',
    lineHeight: 1.6,
    whiteSpace: 'pre-wrap',
    wordBreak: 'break-word',
    marginBottom: 12,
  },
  clarifyInputArea: {
    display: 'flex',
    gap: 8,
    alignItems: 'flex-end',
  },
  clarifyTextarea: {
    flex: 1,
    resize: 'none',
    padding: '8px 12px',
    borderRadius: 8,
    border: '1px solid var(--ds-border)',
    background: 'var(--ds-bg)',
    color: 'var(--ds-text-primary)',
    fontSize: 13,
    fontFamily: 'inherit',
    outline: 'none',
  },
  clarifyBtn: {
    padding: '8px 16px',
    borderRadius: 8,
    border: 'none',
    background: 'var(--ds-warning)',
    color: 'var(--ds-bg)',
    fontWeight: 600,
    fontSize: 13,
    cursor: 'pointer',
    whiteSpace: 'nowrap',
  },
}
