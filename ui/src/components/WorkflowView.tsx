/**
 * WorkflowView — output-first view for an AgentWorkflow.
 * Shows: header → budget bar → horizontal step pipeline → selected step output → step traces accordion.
 */
import React, { useEffect, useRef, useState } from 'react'
import { STREAM_EVENT_TYPE } from '../contracts/events'
import { answerClarification, getRun, getWorkflow, retryWorkflowStep, subscribeToRunStream, type AgentWorkflowDetail, type TraceEntry, type TraceEvent, type WorkflowLoopSummary, type WorkflowStepPass, type WorkflowStepSummary } from '../api/sse'
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
  // A past pass picked in the pass selector; null follows the step's current state.
  const [viewedPass, setViewedPass] = useState<number | null>(null)
  const [inputOpen, setInputOpen] = useState(true)
  const [outputOpen, setOutputOpen] = useState(true)
  const [clarifyQuestion, setClarifyQuestion] = useState<string | null>(null)
  const [clarifyRunName, setClarifyRunName] = useState<string | null>(null)
  const [clarifyInput, setClarifyInput] = useState('')
  const [clarifySubmitting, setClarifySubmitting] = useState(false)
  const [retrying, setRetrying] = useState(false)
  const [retryError, setRetryError] = useState<string | null>(null)
  const [stepTraceEntries, setStepTraceEntries] = useState<TraceEntry[]>([])
  const stepTraceUnsubRef = useRef<(() => void) | null>(null)
  const stepChildUnsubsRef = useRef<Array<() => void>>([])
  const stepTraceCounter = useRef(0)
  const viewedPassRun: WorkflowStepPass | undefined =
    viewedPass === null ? undefined : selectedStep?.passes?.find((p) => p.pass === viewedPass)

  useEffect(() => {
    setDetail(null)
    setSelectedStep(null)
    setViewedPass(null)

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

    const runRef = viewedPassRun?.agentRunRef ?? selectedStep?.agentRunRef
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

      // Trace-stream `routing` entries duplicate the CRD-backed `modelSelected` event;
      // keep one row.
      if (event.type === 'routing') return

      // A model's reasoning arrives a few characters at a time. Fold each stretch of
      // consecutive `thought` events into one entry, so a thinking block is one row.
      if (event.type === 'thought') {
        setStepTraceEntries((prev) => {
          const last = prev[prev.length - 1]
          if (last && last.event.type === 'thought' && last.childRunName === childRunName) {
            return [...prev.slice(0, -1), { ...last, event: { ...last.event, content: last.event.content + event.content } }]
          }
          return [...prev, { id: stepTraceCounter.current++, event, ts: new Date().toISOString(), childRunName }]
        })
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
  }, [selectedStep?.agentRunRef, viewedPassRun?.agentRunRef, namespace])

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

  const retryStep = (step: string) => {
    setRetrying(true)
    setRetryError(null)
    retryWorkflowStep(namespace, name, step)
      .then(() => new Promise((resolve) => setTimeout(resolve, 1500)))
      .then(() => getWorkflow(namespace, name))
      .then((d) => {
        setDetail(d)
        setSelectedStep((prev) => d.steps.find((st) => st.name === prev?.name) ?? prev)
        setViewedPass(null)
      })
      .catch((err: unknown) => setRetryError(err instanceof Error ? err.message : String(err)))
      .finally(() => setRetrying(false))
  }

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

      {/* Loops */}
      {detail.sandboxPhase === 'Provisioning' && detail.phase === 'Running' && (
        <div style={s.stepLoop}>
          <Icon icon={ICON.retry} size={12} ariaHidden={true} /> Starting the workflow's sandbox and checking out the repository…
        </div>
      )}
      {detail.sandboxFailed && (
        <div style={s.retryRow}>
          <span style={{ color: 'var(--ds-error)', fontSize: 12 }}>The workflow sandbox failed to start or was lost.</span>
          <button type="button" style={{ ...s.clarifyBtn, opacity: retrying ? 0.4 : 1 }} disabled={retrying} onClick={() => retryStep('sandbox')}>
            {retrying ? 'Retrying…' : 'Retry sandbox'}
          </button>
          {retryError && <span style={{ color: 'var(--ds-error)', fontSize: 12 }}>{retryError}</span>}
        </div>
      )}
      {(detail.loops ?? []).map((loop) => <LoopBanner key={loop.id} loop={loop} />)}

      {/* Horizontal step pipeline */}
      <div style={s.pipeline}>
        {detail.steps.map((step, i) => (
          <div key={step.name} style={s.pipelineItem}>
            {i > 0 && <div style={s.connector}><Icon icon={ICON.routed} size={16} ariaHidden={true} /></div>}
            <StepCard
              step={step}
              active={selectedStep?.name === step.name}
              loop={(detail.loops ?? []).find((l) => l.id === step.loop)}
              onClick={() => { setSelectedStep(step); setViewedPass(null) }}
            />
          </div>
        ))}
      </div>

      {/* Selected step detail */}
      {selectedStep && (
        <>
          {/* Passes of a step that sits in a loop */}
          {(selectedStep.passes?.length ?? 0) > 0 && (
            <PassSelector
              passes={selectedStep.passes!}
              current={selectedStep.iteration ?? 0}
              viewed={viewedPass}
              onPick={setViewedPass}
            />
          )}

          {/* Input */}
          {selectedStep.input && !viewedPassRun && (
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

          {selectedStep.phase === 'Failed' && (
            <div style={s.retryRow}>
              <button
                type="button"
                style={{ ...s.clarifyBtn, opacity: retrying ? 0.4 : 1 }}
                disabled={retrying}
                onClick={() => retryStep(selectedStep.name)}
                title={selectedStep.loop ? 'Runs the loop again with a full set of passes' : 'Runs this step again'}
              >
                {retrying ? 'Retrying…' : selectedStep.loop ? 'Retry loop' : 'Retry step'}
              </button>
              {retryError && <span style={{ color: 'var(--ds-error)', fontSize: 12 }}>{retryError}</span>}
            </div>
          )}

          {/* Output */}
          <div style={s.stepOutput}>
            <button type="button" style={{ ...s.stepOutputHeader, cursor: 'pointer', userSelect: 'none', background: 'transparent', border: 'none', color: 'inherit', font: 'inherit', textAlign: 'left' }} onClick={() => setOutputOpen((prev: boolean) => !prev)} aria-expanded={outputOpen} aria-controls="output-step-body">
              <div style={{ ...s.stepOutputLabel, display: 'flex', alignItems: 'center', gap: 8 }}>
                <Icon icon={ICON.chevronRight} size={11} style={{ ...s.chevron, transform: outputOpen ? 'rotate(90deg)' : undefined }} ariaHidden={true} />
                Output: <span style={{ color: 'var(--ds-text-primary)' }}>{selectedStep.name}</span>
              </div>
              {(viewedPassRun?.output ?? selectedStep.output) && (
                <button
                  type="button"
                  style={s.iconBtn}
                  onClick={(e: React.MouseEvent) => { e.stopPropagation(); navigator.clipboard.writeText((viewedPassRun?.output ?? selectedStep.output)!) }}
                  title="Copy output"
                  aria-label="Copy output"
                >
                  <Icon icon={ICON.copy} size={14} />
                </button>
              )}
            </button>
            {outputOpen && (
              <div id="output-step-body" style={s.stepOutputBody}>
                {viewedPassRun
                  ? (viewedPassRun.output || <span style={{ color: 'var(--ds-text-muted)', fontStyle: 'italic' }}>{viewedPassRun.phase === 'Running' ? 'Running…' : 'No output'}</span>)
                  : selectedStep.staleOutput
                  ? <>
                      <div style={s.staleNote}><Icon icon={ICON.retry} size={12} ariaHidden={true} /> From pass {selectedStep.iteration} — queued to run again as pass {(selectedStep.iteration ?? 0) + 1}</div>
                      {selectedStep.output}
                    </>
                  : selectedStep.output
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
        streaming={!viewedPassRun && (selectedStep?.phase === 'Running' || selectedStep?.phase === 'Pending')}
      />
    </div>
  )
}

const LOOP_STATE_LABEL: Record<WorkflowLoopSummary['state'], string> = {
  pending: 'not started',
  running: 'running',
  repeating: 'repeating',
  satisfied: 'done: condition met',
  exhausted: 'failed: ran out of passes',
}

function LoopBanner({ loop }: { loop: WorkflowLoopSummary }) {
  const lastPass = loop.pass >= loop.maxIterations && loop.state !== 'satisfied' && loop.state !== 'exhausted'
  const color =
    loop.state === 'satisfied' ? 'var(--ds-success)'
    : loop.state === 'exhausted' ? 'var(--ds-error)'
    : lastPass ? 'var(--ds-warning)'
    : 'var(--ds-accent)'
  return (
    <div style={{ ...s.loopBanner, border: `1px solid ${color}` }}>
      <div style={{ ...s.loopTitle, color }}>
        <Icon icon={ICON.retry} size={13} ariaHidden={true} />
        Loop: {loop.steps.join(' → ')}
        <span style={s.loopMeta}>
          {loop.state === 'pending' ? `up to ${loop.maxIterations} passes` : `pass ${Math.min(loop.pass, loop.maxIterations)} of ${loop.maxIterations}`} · {LOOP_STATE_LABEL[loop.state]}
          {lastPass && ' · last pass'}
        </span>
      </div>
      <div style={s.loopUntil}>repeats until <code>{loop.until}</code></div>
    </div>
  )
}

function PassSelector({
  passes,
  current,
  viewed,
  onPick,
}: {
  passes: WorkflowStepPass[]
  current: number
  viewed: number | null
  onPick: (pass: number | null) => void
}) {
  return (
    <div style={s.passRow} role="group" aria-label="Passes of this step">
      <span style={s.passLabel}>Passes</span>
      <button type="button" style={{ ...s.passChip, ...(viewed === null ? s.passChipActive : {}) }} onClick={() => onPick(null)}>
        Current
      </button>
      {passes.map((p) => (
        <button
          key={p.pass}
          type="button"
          style={{ ...s.passChip, ...(viewed === p.pass ? s.passChipActive : {}) }}
          onClick={() => onPick(p.pass)}
          title={`${p.agentRunRef}${p.spendUSD ? ` · $${p.spendUSD}` : ''}`}
        >
          {p.pass + 1} · {p.phase}{p.pass === current && viewed !== p.pass ? ' ●' : ''}
        </button>
      ))}
    </div>
  )
}

function StepCard({
  step,
  active,
  loop,
  onClick,
}: {
  step: WorkflowStepSummary
  active: boolean
  loop?: WorkflowLoopSummary
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
      {loop && (
        <div style={s.stepLoop}>
          <Icon icon={ICON.retry} size={11} ariaHidden={true} /> pass {(step.iteration ?? 0) + 1}/{loop.maxIterations}
        </div>
      )}
      {step.staleOutput ? (
        <div style={{ ...s.stepPreview, opacity: 0.6 }}>queued for pass {(step.iteration ?? 0) + 1}. Last pass: "{(step.output ?? '').slice(0, 70)}{(step.output ?? '').length > 70 ? '…' : ''}"</div>
      ) : step.output && (
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
  loopBanner: {
    background: 'var(--ds-surface)',
    border: '1px solid var(--ds-accent)',
    borderRadius: 8,
    padding: '10px 14px',
  },
  loopTitle: { display: 'flex', alignItems: 'center', gap: 8, fontSize: 12, fontWeight: 600, flexWrap: 'wrap' },
  loopMeta: { color: 'var(--ds-text-secondary)', fontWeight: 400 },
  loopUntil: { fontSize: 11, color: 'var(--ds-text-muted)', marginTop: 4, wordBreak: 'break-word' },
  stepLoop: { display: 'flex', alignItems: 'center', gap: 4, fontSize: 11, color: 'var(--ds-accent)', fontVariantNumeric: 'tabular-nums' },
  retryRow: { display: 'flex', alignItems: 'center', gap: 10 },
  staleNote: { display: 'flex', alignItems: 'center', gap: 6, fontSize: 12, color: 'var(--ds-warning)', marginBottom: 8 },
  passRow: { display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' },
  passLabel: { fontSize: 11, color: 'var(--ds-text-muted)', marginRight: 2 },
  passChip: {
    padding: '3px 10px',
    borderRadius: 999,
    border: '1px solid var(--ds-border)',
    background: 'var(--ds-surface)',
    color: 'var(--ds-text-secondary)',
    fontSize: 11,
    cursor: 'pointer',
  },
  passChipActive: { border: '1px solid var(--ds-accent)', color: 'var(--ds-text-primary)', background: 'rgba(59,130,246,.08)' },
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
