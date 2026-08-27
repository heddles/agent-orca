/**
 * DeploymentView — chat interface for an AgentDeployment.
 * Migrated from ChatWindow with the new output-first layout and status bar.
 */
import { useCallback, useEffect, useRef, useState } from 'react'
import Markdown from 'react-markdown'
import {
  answerClarification,
  cancelRun,
  executeDeployment,
  getChatHistory,
  getCosts,
  getDeployment,
  getRun,
  listRuns,
  saveChatResponse,
  subscribeToRunStream,
  type AgentDeploymentDetail,
  type AgentRunSummary,
  type ChatMessage,
  type TraceEntry,
} from '../api/sse'
import { STREAM_EVENT_TYPE } from '../contracts/events'
import { StatusBadge } from './StatusBadge'
import { PHASE_COLOR } from '../lib/phaseColors'
import { MCPAppFrame } from './MCPAppFrame'
import { DESIGN } from '../lib/designSystem'
import { Icon, ICON } from '../lib/icons'

interface Props {
  namespace: string
  name: string
  onNavigateToRun?: (runName: string, namespace: string) => void
}

interface PendingRun {
  runName: string
  status: 'running' | 'succeeded' | 'failed'
}

/** Local chat message — extends ChatMessage with optional persisted trace entries. */
interface LocalMessage {
  role: 'user' | 'assistant'
  content: string
  /** Tool/MCP trace entries captured during this assistant turn. */
  traceEntries?: TraceEntry[]
}

const SESSION_KEY_PREFIX = 'agentorc-chat-session:'
const MSGS_KEY_PREFIX = 'agentorc-chat-msgs:'

/** Quick-prompt suggestions shown in the empty chat state (better-writing §11). */
const QUICK_PROMPTS = [
  'Summarize the key features of agent-orc',
  'What model is selected and why?',
  'Show me the cost breakdown',
]

function msgsKey(ns: string, n: string, sid: string) {
  return `${MSGS_KEY_PREFIX}${ns}/${n}:${sid}`
}

function loadCachedMessages(ns: string, n: string, sid: string): LocalMessage[] {
  try {
    const raw = localStorage.getItem(msgsKey(ns, n, sid))
    return raw ? (JSON.parse(raw) as LocalMessage[]) : []
  } catch {
    return []
  }
}

function saveCachedMessages(ns: string, n: string, sid: string, msgs: LocalMessage[]) {
  try {
    localStorage.setItem(msgsKey(ns, n, sid), JSON.stringify(msgs))
  } catch {
    // Storage quota exceeded or private browsing — ignore.
  }
}

/** Convert backend ChatMessage array to LocalMessage, restoring persisted trace entries. */
function fromChatMessages(msgs: ChatMessage[]): LocalMessage[] {
  return msgs.map((m) => {
    let traceEntries: TraceEntry[] | undefined
    if (m.traceEntries) {
      try {
        traceEntries = JSON.parse(m.traceEntries) as TraceEntry[]
      } catch {
        // Corrupted trace data — skip.
      }
    }
    return { role: m.role, content: m.content, traceEntries }
  })
}

export function DeploymentView({ namespace, name, onNavigateToRun }: Props) {
  const [deployment, setDeployment] = useState<AgentDeploymentDetail | null>(null)
  const [messages, setMessages] = useState<LocalMessage[]>([])
  const [input, setInput] = useState('')
  const [sessionId, setSessionId] = useState<string | null>(null)
  const [pending, setPending] = useState<PendingRun | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [clarify, setClarify] = useState<{ runName: string; question: string } | null>(null)
  const [clarifyInput, setClarifyInput] = useState('')
  const [traceEvents, setTraceEvents] = useState<TraceEntry[]>([])
  const [viewTab, setViewTab] = useState<'chat' | 'runs'>('chat')
  const [runs, setRuns] = useState<AgentRunSummary[]>([])
  const [totalCost, setTotalCost] = useState('0.0000')
  const [showJumpBtn, setShowJumpBtn] = useState(false)
  const bottomRef = useRef<HTMLDivElement>(null)
  const messagesRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLTextAreaElement>(null)
  const unsubRef = useRef<(() => void) | null>(null)
  const childRunUnsubsRef = useRef<Array<() => void>>([])
  const traceCounter = useRef(0)
  const traceEventsRef = useRef<TraceEntry[]>([])
  const sessionRef = useRef<string | null>(null)
  sessionRef.current = sessionId

  // Scroll listener for the jump-to-bottom button
  useEffect(() => {
    const el = messagesRef.current
    if (!el) return
    const onScroll = () => {
      const atBottom = el.scrollHeight - el.scrollTop <= el.clientHeight + 50
      setShowJumpBtn(!atBottom)
    }
    el.addEventListener('scroll', onScroll, { passive: true })
    return () => el.removeEventListener('scroll', onScroll)
  }, [])

  // Load deployment metadata and cost.
  useEffect(() => {
    const load = () => {
      getDeployment(namespace, name).then(setDeployment).catch(() => {})
      getCosts(namespace, { deployment: name })
        .then((c) => setTotalCost(c.totalUSD))
        .catch(() => {})
    }
    load()
    const id = setInterval(load, 2000)
    return () => clearInterval(id)
  }, [namespace, name])

  // Poll runs for the runs tab.
  useEffect(() => {
    const load = () =>
      listRuns(namespace, name)
        .then((data) => {
          data.sort((a, b) => (b.startTime ?? '').localeCompare(a.startTime ?? ''))
          setRuns(data)
        })
        .catch(() => {})
    load()
    const id = setInterval(load, 5_000)
    return () => clearInterval(id)
  }, [namespace, name])

  const reloadHistory = useCallback(
    async (sid: string) => {
      try {
        const res = await getChatHistory(namespace, name, sid)
        const msgs = res.messages ?? []
        if (msgs.length > 0) {
          // Backend now returns traceEntries — no need for fragile index-based merge.
          const loaded = fromChatMessages(msgs)
          setMessages(loaded)
          saveCachedMessages(namespace, name, sid, loaded)
        }
      } catch {
        // ignore — cached messages already displayed
      }
    },
    [namespace, name],
  )

  const subscribeToRun = useCallback(
    (runName: string, sid: string) => {
      unsubRef.current?.()
      childRunUnsubsRef.current.forEach((fn) => fn())
      childRunUnsubsRef.current = []
      setPending({ runName, status: 'running' })
      setTraceEvents([])
      traceEventsRef.current = []
      traceCounter.current = 0

      // Fallback poller: if the SSE stream misses the clarify event (e.g., due to
      // proxy buffering or CRD cache lag), periodically check all runs for this
      // deployment and transition to the clarify UI when any run enters
      // WaitingForInput. Polling deployment-wide (not just runName) ensures child
      // runs spawned by escalation chains are also detected.
      // Run once immediately (handles already-WaitingForInput at subscription time),
      // then every 1.5s.
      // Track which child runs we've already subscribed to (dedup guard).
      const subscribedChildren = new Set<string>()
      // Set to true after final_output so child events route to messages rather than traceEvents.
      let runCompleted = false
      // Track whether finalOutput has been received — if not, the poll fallback
      // (Succeeded detection) will fetch the output from the API as a safety net.
      let finalOutputReceived = false

      const subscribeToChild = (childName: string) => {
        if (subscribedChildren.has(childName)) return
        subscribedChildren.add(childName)
        const childUnsub = subscribeToRunStream(childName, (childEvent) => {
          if (childEvent.type === STREAM_EVENT_TYPE.token) return
          if (childEvent.type === STREAM_EVENT_TYPE.error && childEvent.message === 'Stream connection lost') return
          const childEntry: TraceEntry = { id: traceCounter.current++, event: childEvent, ts: new Date().toISOString(), childRunName: childName }
          traceEventsRef.current = [...traceEventsRef.current, childEntry]
          if (runCompleted) {
            // Parent is done — append directly to the last assistant message so traces persist.
            setMessages((prev) => {
              if (prev.length === 0) return prev
              const last = prev[prev.length - 1]
              if (last.role !== 'assistant') return prev
              const updated: LocalMessage = { ...last, traceEntries: [...(last.traceEntries ?? []), childEntry] }
              const next = [...prev.slice(0, -1), updated]
              saveCachedMessages(namespace, name, sid, next)
              return next
            })
          } else {
            setTraceEvents((prev) => [...prev, childEntry])
          }
        }, namespace)
        childRunUnsubsRef.current.push(childUnsub)
      }

      let pollId: ReturnType<typeof setInterval>
      // finalizeRun persists the assistant turn with the given output. Called from
      // the finalOutput SSE handler and from the poll fallback (Succeeded detection).
      const finalizeRun = async (output: string, snapshotTrace: TraceEntry[]) => {
        clearInterval(pollId)
        unsubRef.current = null
        unsub()
        runCompleted = true
        finalOutputReceived = true
        // Final sweep: pick up any child refs that appeared after the last poll.
        getRun(runName, namespace).then((detail) => {
          for (const childName of detail.childRunRefs ?? []) {
            subscribeToChild(childName)
          }
        }).catch(() => {})
        // Snapshot trace entries into the assistant message so they persist.
        const traceJSON = snapshotTrace.length > 0 ? JSON.stringify(snapshotTrace) : undefined
        setMessages((prev) => {
          const next: LocalMessage[] = [...prev, { role: 'assistant' as const, content: output, traceEntries: snapshotTrace.length > 0 ? snapshotTrace : undefined }]
          saveCachedMessages(namespace, name, sid, next)
          return next
        })
        setPending(null)
        try {
          await saveChatResponse(namespace, name, sid, output, traceJSON)
        } catch {
          // Checkpoint unavailable — optimistic message already displayed.
        }
      }

      const checkForClarifyAndChildren = () => {
        // Check for new child runs to subscribe to (live bubbling).
        getRun(runName, namespace)
          .then((detail) => {
            for (const childName of detail.childRunRefs ?? []) {
              subscribeToChild(childName)
            }
          })
          .catch(() => {})
        // Check deployment-wide for WaitingForInput (clarify fallback).
        listRuns(namespace, name)
          .then((runs) => {
            const waiting = runs.find((r) => r.phase === 'WaitingForInput')
            if (!waiting) {
              // No WaitingForInput — check if the done event fired but we never
              // received finalOutput (e.g. SSE connection dropped before Phase 3).
              // Fall back to the controller's persisted output for a Succeeded run.
              if (runCompleted && !finalOutputReceived) {
                getRun(runName, namespace)
                  .then((detail) => {
                    if (!finalOutputReceived && detail.output) {
                      finalOutputReceived = true
                      const snapshotTrace = [...traceEventsRef.current]
                      void finalizeRun(detail.output, snapshotTrace)
                    }
                  })
                  .catch(() => {})
              }
              return
            }
            getRun(waiting.name, namespace)
              .then((detail) => {
                if (detail.clarifyQuestion && !detail.clarifyAnswer) {
                  clearInterval(pollId)
                  // Snapshot accumulated traces before the clarify question appears.
                  const clarifyTrace = [...traceEventsRef.current]
                  if (clarifyTrace.length > 0) {
                    setMessages((prev) => {
                      const next = [...prev, { role: 'assistant' as const, content: '', traceEntries: clarifyTrace }]
                      saveCachedMessages(namespace, name, sid, next)
                      return next
                    })
                    traceEventsRef.current = []
                    setTraceEvents([])
                  }
                  setPending(null)
                  setClarify({ runName: waiting.name, question: detail.clarifyQuestion })
                  unsubRef.current?.()
                  unsubRef.current = null
                }
              })
              .catch(() => {})
          })
          .catch(() => {})
      }
      checkForClarifyAndChildren()
      pollId = setInterval(checkForClarifyAndChildren, 1500)

      const unsub = subscribeToRunStream(runName, async (event) => {
        if (
          event.type === STREAM_EVENT_TYPE.toolCall ||
          event.type === STREAM_EVENT_TYPE.toolResult ||
          event.type === STREAM_EVENT_TYPE.agentEvent
        ) {
          const entry: TraceEntry = { id: traceCounter.current++, event, ts: new Date().toISOString() }
          traceEventsRef.current = [...traceEventsRef.current, entry]
          setTraceEvents((prev) => [...prev, entry])
          return
        }
        // done: model-router signaled turn-complete via Redis trace event (emitted
        // by the _done tool, which carries output). Set runCompleted for child routing.
        // If the done event carries output, use it as a fast-path completion signal
        // rather than waiting for finalOutput (which requires the UI API's Phase 3
        // polling round-trip after TailTokens exits). Only fires once via finalOutputReceived.
        if (event.type === STREAM_EVENT_TYPE.done) {
          runCompleted = true
          if (event.output != null && !finalOutputReceived) {
            finalOutputReceived = true
            const snapshotTrace = [...traceEventsRef.current]
            void finalizeRun(event.output, snapshotTrace)
          }
          return
        }
        if (event.type === STREAM_EVENT_TYPE.finalOutput) {
          if (!finalOutputReceived) {
            finalOutputReceived = true
            const snapshotTrace = [...traceEventsRef.current]
            await finalizeRun(event.output ?? '', snapshotTrace)
          }
        } else if (event.type === STREAM_EVENT_TYPE.clarify) {
          clearInterval(pollId)
          // Snapshot any accumulated traces before the clarify question appears.
          const clarifyTrace = [...traceEventsRef.current]
          if (clarifyTrace.length > 0) {
            setMessages((prev) => {
              const next = [...prev, { role: 'assistant' as const, content: '', traceEntries: clarifyTrace }]
              saveCachedMessages(namespace, name, sid, next)
              return next
            })
            traceEventsRef.current = []
            setTraceEvents([])
          }
          setPending(null)
          setClarify({ runName, question: event.question })
          unsubRef.current = null
          unsub()
        } else if (event.type === STREAM_EVENT_TYPE.error || event.type === STREAM_EVENT_TYPE.fail) {
          clearInterval(pollId)
          setError(event.type === STREAM_EVENT_TYPE.error ? event.message : event.reason)
          setPending(null)
          unsubRef.current = null
          unsub()
        }
      }, namespace)

      unsubRef.current = () => {
        clearInterval(pollId)
        unsub()
      }
    },
    [namespace, name, reloadHistory],
  )

  const submitClarifyAnswer = useCallback(() => {
    const answer = clarifyInput.trim()
    if (!answer || !clarify) return
    const originalRunName = clarify.runName
    const savedQuestion = clarify.question
    // Optimistically append the Q&A exchange to the chat.
    const appendedCount = 2
    setMessages((prev) => [
      ...prev,
      { role: 'assistant' as const, content: savedQuestion } satisfies LocalMessage,
      { role: 'user' as const, content: answer } satisfies LocalMessage,
    ])
    setClarify(null)
    setClarifyInput('')
    setPending({ runName: originalRunName, status: 'running' })
    answerClarification(originalRunName, answer, namespace)
      .then((res) => {
        // Subscribe to the new continuation run's stream.
        subscribeToRun(res.runName, sessionRef.current ?? '')
      })
      .catch((e) => {
        // Roll back the optimistically-added messages and restore the clarify UI.
        setMessages((prev) => prev.slice(0, -appendedCount))
        setClarify({ runName: originalRunName, question: savedQuestion })
        setClarifyInput(answer)
        setPending(null)
        setError(e instanceof Error ? e.message : 'Failed to submit answer')
      })
  }, [clarify, clarifyInput, namespace, subscribeToRun])

  const cancelClarify = useCallback(() => {
    if (!clarify) return
    cancelRun(clarify.runName, namespace)
      .then(() => {
        setMessages((prev) => [
          ...prev,
          { role: 'assistant' as const, content: clarify.question } satisfies LocalMessage,
          { role: 'assistant' as const, content: 'Clarification cancelled by user.' } satisfies LocalMessage,
        ])
        setClarify(null)
        setClarifyInput('')
      })
      .catch((e) => {
        setError(e instanceof Error ? e.message : 'Failed to cancel')
      })
  }, [clarify, namespace])

  useEffect(() => {
    return () => {
      unsubRef.current?.()
      unsubRef.current = null
      childRunUnsubsRef.current.forEach((fn) => fn())
      childRunUnsubsRef.current = []
    }
  }, [namespace, name])

  useEffect(() => {
    const stored = localStorage.getItem(SESSION_KEY_PREFIX + namespace + '/' + name)
    if (stored) {
      // Hydrate from local cache immediately so messages appear before the
      // backend round-trip completes (or if the backend has no history).
      const cached = loadCachedMessages(namespace, name, stored)
      if (cached.length > 0) setMessages(cached)
      setSessionId(stored)
    } else {
      setMessages([])
      setSessionId(null)
    }
    setPending(null)
    setError(null)
  }, [namespace, name])

  useEffect(() => {
    if (!sessionId) return
    let cancelled = false

    getChatHistory(namespace, name, sessionId)
      .then((res) => {
        if (cancelled) return
        const backendMsgs = res.messages ?? []
        if (backendMsgs.length > 0) {
          // Backend now includes trace entries — use them directly.
          const loaded = fromChatMessages(backendMsgs)
          setMessages(loaded)
          saveCachedMessages(namespace, name, sessionId, loaded)
        }
        // If backend returns empty, keep the localStorage-hydrated messages already displayed.
        return listRuns(namespace, name)
      })
      .then((runs) => {
        if (cancelled || !runs) return
        const waiting = runs.find((r) => r.phase === 'WaitingForInput')
        if (waiting) {
          // Restore clarify state from a WaitingForInput run — but only if it
          // hasn't already been answered (the backend transitions phase on answer,
          // but check clarifyAnswer as a safety net for race conditions).
          getRun(waiting.name, namespace).then((detail) => {
            if (detail.clarifyQuestion && !detail.clarifyAnswer) {
              setClarify({ runName: waiting.name, question: detail.clarifyQuestion })
            }
          }).catch(() => {})
          return
        }
        const inflight = runs.find((r) => r.phase === 'Pending' || r.phase === 'Running')
        if (inflight) subscribeToRun(inflight.name, sessionId)
      })
      .catch(() => {})

    return () => { cancelled = true }
  }, [namespace, name, sessionId, subscribeToRun])

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [messages, pending])

  const send = useCallback(async () => {
    const text = input.trim()
    if (!text || pending?.status === 'running') return

    setInput('')
    setError(null)
    setMessages((prev) => [...prev, { role: 'user' as const, content: text }])

    try {
      const res = await executeDeployment(namespace, name, text, sessionId ?? undefined)
      const sid = res.sessionId
      setSessionId(sid)
      localStorage.setItem(SESSION_KEY_PREFIX + namespace + '/' + name, sid)
      // Cache the user message now that we have a confirmed sessionId.
      setMessages((prev) => {
        saveCachedMessages(namespace, name, sid, prev)
        return prev
      })
      subscribeToRun(res.runName, sid)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to send message')
      setMessages((prev) => prev.slice(0, -1))
    }
  }, [input, namespace, name, sessionId, pending, subscribeToRun])

  const handleKeyDown = (e: React.KeyboardEvent) => {
    // Enter to send, Shift+Enter for newline, Cmd/Ctrl+Enter always sends (better-accessibility)
    if (e.key === 'Enter' && (!e.shiftKey || e.metaKey || e.ctrlKey)) {
      e.preventDefault()
      send()
    }
    // Escape to close clarify or stop
    if (e.key === 'Escape' && pending?.status === 'running') {
      e.preventDefault()
    }
  }

  const newSession = () => {
    unsubRef.current?.()
    unsubRef.current = null
    if (sessionId) localStorage.removeItem(msgsKey(namespace, name, sessionId))
    localStorage.removeItem(SESSION_KEY_PREFIX + namespace + '/' + name)
    setMessages([])
    setSessionId(null)
    setPending(null)
    setError(null)
  }

  return (
    <div style={s.root}>
      {/* Status bar */}
      <div style={s.statusBar}>
        <div style={s.depName}>{name}</div>
        {deployment && <StatusBadge phase={deployment.phase} />}
        {pending && (
          <div style={s.streamingIndicator}>
            <Icon icon={ICON.spinner} size={12} style={s.spinner} ariaHidden={true} />
            <span style={s.streamingLabel}>Responding</span>
          </div>
        )}
        {deployment && (
          <div style={s.depMeta}>
            {deployment.readyReplicas} replica{deployment.readyReplicas !== 1 ? 's' : ''} · {deployment.agentRef}
            {deployment.inputSourceType && ` · ${deployment.inputSourceType}`}
          </div>
        )}
        {deployment && deployment.message && deployment.phase === 'Failed' && (
          <span style={s.errorChip} title={deployment.message}>
            {deployment.message}
          </span>
        )}
        {deployment && (deployment.maxContextTokens ?? 0) > 0 && (
          <span style={s.contextChip}>
            <Icon icon={ICON.tokens} size={12} ariaHidden={true} /> {deployment.contextUsedTokens?.toLocaleString() ?? 0} / {deployment.maxContextTokens?.toLocaleString() ?? '—'} tokens
          </span>
        )}
        <div style={{ flex: 1 }} />
        <span style={s.costChip}>${totalCost}</span>
        {sessionId && viewTab === 'chat' && (
          <button type="button" style={s.newBtn} onClick={newSession}>New session</button>
        )}
      </div>

      {/* Tab bar */}
      <div role="tablist" style={s.tabBar}>
        <button
          type="button"
          role="tab"
          aria-selected={viewTab === 'chat'}
          aria-controls="chat-panel"
          tabIndex={viewTab === 'chat' ? 0 : -1}
          style={{ ...s.tab, ...(viewTab === 'chat' ? s.tabActive : {}) }}
          onClick={() => setViewTab('chat')}
        >
          Chat
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={viewTab === 'runs'}
          aria-controls="runs-panel"
          tabIndex={viewTab === 'runs' ? 0 : -1}
          style={{ ...s.tab, ...(viewTab === 'runs' ? s.tabActive : {}) }}
          onClick={() => setViewTab('runs')}
        >
          Runs ({runs.length})
        </button>
      </div>

      {viewTab === 'runs' ? (
        <div id="runs-panel" role="tabpanel" style={s.runsList}>
          {runs.length === 0 && (
            <div style={s.empty}>No runs yet. Send a message to create one.</div>
          )}
          {runs.map((run) => (
            <button
              key={run.name}
              type="button"
              style={s.runCard}
              onClick={() => onNavigateToRun?.(run.name, namespace)}
            >
              <div style={s.runCardHeader}>
                <span
                  style={{
                    ...s.runDot,
                    background: PHASE_COLOR[run.phase] ?? '#6b7280',
                  }}
                />
                <span style={s.runCardName}>{run.name}</span>
                <span style={s.runCardPhase}>{run.phase}</span>
                <span style={{ flex: 1 }} />
                {parseFloat(run.spendUSD) > 0 && (
                  <span style={s.runCardCost}>${run.spendUSD}</span>
                )}
              </div>
              <div style={s.runCardMeta}>
                {run.agentRef}
                {run.startTime && ` · ${new Date(run.startTime).toLocaleString()}`}
              </div>
            </button>
          ))}
        </div>
      ) : (
      <div id="chat-panel" role="tabpanel" aria-live="polite" style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
      {/* Messages */}
      <div ref={messagesRef} style={s.messages}>
        {messages.length === 0 && !pending && (
          <div style={s.empty}>
            <div style={s.emptyTitle}>Start chatting with <strong>{name}</strong></div>
            <p style={s.emptySubtitle}>Send a message to begin. Or try one of these prompts:</p>
            <div style={s.quickPrompts}>
              {QUICK_PROMPTS.map((p) => (
                <button
                  key={p}
                  type="button"
                  style={s.quickPromptBtn}
                  onClick={() => {
                    setInput(p)
                    inputRef.current?.focus()
                  }}
                >
                  {p}
                </button>
              ))}
            </div>
          </div>
        )}
        {messages.map((msg, i) => {
          // Collect MCP app iframes from tool_result trace entries.
          const appEntries = (msg.role === 'assistant' && msg.traceEntries)
            ? msg.traceEntries.filter((e) => e.event.type === STREAM_EVENT_TYPE.toolResult && (e.event as any).appUrl)
            : []
          return (
          <div key={i} style={s.msgWrap}>
            {msg.role === 'assistant' && msg.traceEntries && msg.traceEntries.length > 0 && (
              <div style={s.traceBlock}>
                {msg.traceEntries.map((entry, j) => {
                  const ev = entry.event
                  const badge = entry.childRunName
                    ? <span style={s.childBadge}>{entry.childRunName.split('-').slice(-2).join('-')}</span>
                    : null
                  return (
                    <div key={j} style={s.traceRow}>
                      {ev.type === STREAM_EVENT_TYPE.toolCall ? (
                        <>
                          <Icon icon={ICON.settings} size={12} color="var(--ds-warning)" ariaHidden={true} />
                          {badge}
                          <span style={s.traceName}>{ev.name}</span>
                          <span style={s.traceArgs}>{ev.arguments}</span>
                        </>
                      ) : ev.type === STREAM_EVENT_TYPE.toolResult ? (
                        <>
                          <Icon icon={ICON.success} size={12} color="var(--ds-success)" ariaHidden={true} />
                          {badge}
                          <span style={s.traceName}>{ev.name}</span>
                          <span style={s.traceResult}>{ev.result}</span>
                        </>
                      ) : null}
                    </div>
                  )
                })}
              </div>
            )}
            {msg.content && (
              <div style={msg.role === 'user' ? s.userMsg : s.assistantMsg}>
                <div style={s.role}>{msg.role === 'user' ? 'You' : name}</div>
                {msg.role === 'assistant' ? (
                  <div style={s.mdWrap}><Markdown>{msg.content}</Markdown></div>
                ) : (
                  <div style={s.content}>{msg.content}</div>
                )}
              </div>
            )}
            {appEntries.map((entry, j) => {
              const ev = entry.event as any
              return <MCPAppFrame key={`app-${j}`} appUrl={ev.appUrl} toolName={ev.name} toolArgs={ev.toolArgs} toolResult={ev.toolResult} initialHeight={420} />
            })}
          </div>
          )
        })}
        {pending?.status === 'running' && (
          <div style={s.msgWrap}>
            <div style={s.assistantMsg}>
              <div style={s.role}>{name}</div>
              {traceEvents.length > 0 ? (
                <div style={s.traceBlock}>
                  {traceEvents.map((entry, i) => {
                    const ev = entry.event
                    const badge = entry.childRunName
                      ? <span style={s.childBadge}>{entry.childRunName.split('-').slice(-2).join('-')}</span>
                      : null
                    return (
                      <div key={i} style={s.traceRow}>
                        {ev.type === STREAM_EVENT_TYPE.toolCall ? (
                          <>
                            <Icon icon={ICON.settings} size={12} color="var(--ds-warning)" ariaHidden={true} />
                            {badge}
                            <span style={s.traceName}>{ev.name}</span>
                            <span style={s.traceArgs}>{ev.arguments}</span>
                          </>
                        ) : ev.type === STREAM_EVENT_TYPE.toolResult ? (
                          <>
                            <Icon icon={ICON.success} size={12} color="var(--ds-success)" ariaHidden={true} />
                            {badge}
                            <span style={s.traceName}>{ev.name}</span>
                            <span style={s.traceResult}>{ev.result}</span>
                          </>
                        ) : null}
                      </div>
                    )
                  })}
                  <div style={s.thinking}>Thinking…</div>
                </div>
              ) : (
                <div style={s.thinking}>Thinking…</div>
              )}
              <button
                type="button"
                style={s.stopBtn}
                onClick={() => {
                  if (!pending) return
                  cancelRun(pending.runName, namespace)
                    .then(() => {
                      setPending({ ...pending, status: 'failed' })
                      setMessages((prev) => [
                        ...prev,
                        { role: 'assistant' as const, content: 'Run cancelled by user.' },
                      ])
                    })
                    .catch((e) => setError(e instanceof Error ? e.message : 'Failed to cancel'))
                }}
                title="Stop this run"
                aria-label="Stop running agent"
              >
                <Icon icon={ICON.stop} size={14} /> Stop
              </button>
            </div>
            {traceEvents.filter((e) => e.event.type === STREAM_EVENT_TYPE.toolResult && (e.event as any).appUrl).map((entry, j) => {
              const ev = entry.event as any
              return <MCPAppFrame key={`live-app-${j}`} appUrl={ev.appUrl} toolName={ev.name} toolArgs={ev.toolArgs} toolResult={ev.toolResult} initialHeight={420} />
            })}
          </div>
        )}
        {clarify && (
          <div style={s.msgWrap}>
            <div style={s.clarifyMsg}>
              <div style={s.role}>{name} needs your input</div>
              <div style={s.content}>{clarify.question}</div>
              <div style={s.clarifyInputArea}>
                <textarea
                  style={s.clarifyTextarea}
                  value={clarifyInput}
                  onChange={(e) => setClarifyInput(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' && (!e.shiftKey || e.metaKey || e.ctrlKey)) {
                      e.preventDefault()
                      submitClarifyAnswer()
                    }
                  }}
                  placeholder="Type your answer…"
                  rows={2}
                />
                <button
                  style={{ ...s.sendBtn, opacity: clarifyInput.trim() ? 1 : 0.4 }}
                  disabled={!clarifyInput.trim()}
                  onClick={submitClarifyAnswer}
                  aria-label="Send answer"
                >
                  <Icon icon={ICON.send} size={14} />
                </button>
                <button
                  style={s.cancelBtn}
                  onClick={cancelClarify}
                  aria-label="Cancel clarification request"
                  title="Cancel this request"
                >
                  <Icon icon={ICON.close} size={14} />
                </button>
              </div>
            </div>
          </div>
        )}
        {error && <div style={s.errorBanner}>{error}</div>}
        <div ref={bottomRef} />
      </div>

      {/* Jump to bottom button — shows when scrolled up (better-layout §10) */}
      {showJumpBtn && (
        <button
          type="button"
          style={s.jumpBtn}
          onClick={() => bottomRef.current?.scrollIntoView({ behavior: 'smooth' })}
          aria-label="Jump to latest message"
          title="Jump to bottom"
        >
          <Icon icon={ICON.chevronDown} size={16} />
        </button>
      )}

      {/* Input area */}
      <div style={s.inputArea}>
        <textarea
          ref={inputRef}
          style={s.textarea}
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={handleKeyDown}
          placeholder="Send a message…"
          rows={1}
          disabled={pending?.status === 'running' || !!clarify}
        />
        <button
          type="button"
          style={{ ...s.sendBtn, opacity: input.trim() && pending?.status !== 'running' && !clarify ? 1 : 0.4 }}
          onClick={send}
          disabled={!input.trim() || pending?.status === 'running' || !!clarify}
          aria-label="Send message"
        >
          <Icon icon={ICON.send} size={18} />
        </button>
      </div>
      </div>
      )}
    </div>
  )
}

const s: Record<string, React.CSSProperties> = {
  root: {
    display: 'flex',
    flexDirection: 'column',
    height: '100%',
    overflow: 'hidden',
  },
  statusBar: {
    display: 'flex',
    alignItems: 'center',
    gap: 10,
    padding: '12px 28px',
    borderBottom: `1px solid var(--ds-border)`,
    background: 'var(--ds-bg)',
    flexShrink: 0,
  },
  depName: { fontSize: 16, fontWeight: 700, color: 'var(--ds-text-primary)' },
  depMeta: { fontSize: 12, color: 'var(--ds-text-muted)' },
  costChip: {
    fontSize: 12,
    fontWeight: 600,
    color: 'var(--ds-success)',
    background: 'rgba(74,222,128,.1)',
    padding: '3px 10px',
    borderRadius: 12,
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace',
    fontVariantNumeric: 'tabular-nums',
  },
  errorChip: {
    fontSize: 11,
    fontWeight: 500,
    color: 'var(--ds-error)',
    background: 'rgba(239,68,68,.08)',
    border: '1px solid rgba(239,68,68,.3)',
    padding: '3px 10px',
    borderRadius: 12,
    maxWidth: 400,
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
    cursor: 'help',
  },
  streamingIndicator: {
    display: 'inline-flex',
    alignItems: 'center',
    gap: 4,
    padding: '3px 8px',
    borderRadius: 12,
    background: 'rgba(59,130,246,.12)',
    color: 'var(--ds-accent)',
    fontSize: 11,
    fontWeight: 600,
  },
  spinner: {
    animation: 'spin 1s linear infinite',
    willChange: 'transform',
  },
  streamingLabel: {
    fontSize: 10,
  },
  contextChip: {
    fontSize: 12,
    fontWeight: 600,
    color: 'var(--ds-accent)',
    background: 'var(--ds-accent-bg)',
    padding: '3px 10px',
    borderRadius: 12,
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace',
    fontVariantNumeric: 'tabular-nums',
    marginLeft: 8,
  },
  tabBar: {
    display: 'flex',
    gap: 0,
    borderBottom: `1px solid var(--ds-border)`,
    background: 'var(--ds-bg)',
    flexShrink: 0,
    padding: '0 28px',
  },
  tab: {
    padding: '8px 16px',
    fontSize: 13,
    fontWeight: 500,
    color: 'var(--ds-text-muted)',
    background: 'none',
    border: 'none',
    borderBottom: '2px solid transparent',
    cursor: 'pointer',
    transitionProperty: 'color, border-color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  tabActive: {
    color: 'var(--ds-accent)',
    borderBottomColor: 'var(--ds-accent)',
  },
  runsList: {
    flex: 1,
    overflowY: 'auto',
    padding: `${DESIGN.space.md} ${DESIGN.space.xxl}`,
    display: 'flex',
    flexDirection: 'column',
    gap: 6,
  },
  runCard: {
    padding: '10px 14px',
    borderRadius: DESIGN.radii.lg,
    border: '1px solid transparent',
    background: 'transparent',
    color: 'inherit',
    font: 'inherit',
    cursor: 'pointer',
    textAlign: 'left',
    textDecoration: 'none',
    transitionProperty: 'border-color, background-color, box-shadow',
    transitionDuration: '0.12s',
    transitionTimingFunction: 'ease',
    display: 'flex',
    flexDirection: 'column',
    gap: 4,
    boxShadow: 'var(--ds-card-shadow)',
  },
  runCardActive: {
    background: 'var(--ds-surface)',
    borderColor: 'var(--ds-accent-border)',
  },
  runCardHeader: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
  },
  runDot: {
    width: 7,
    height: 7,
    borderRadius: '50%',
    flexShrink: 0,
  },
  runCardName: {
    fontSize: 12,
    fontWeight: 600,
    color: 'var(--ds-text-primary)',
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace',
  },
  runCardPhase: {
    fontSize: 11,
    color: 'var(--ds-text-muted)',
  },
  runCardCost: {
    fontSize: 11,
    fontWeight: 600,
    color: 'var(--ds-success)',
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace',
    fontVariantNumeric: 'tabular-nums',
  },
  runCardMeta: {
    fontSize: 11,
    color: 'var(--ds-text-muted)',
    paddingLeft: 15,
  },
  newBtn: {
    fontSize: 11,
    padding: '4px 10px',
    borderRadius: DESIGN.radii.sm,
    border: `1px solid var(--ds-border)`,
    background: 'transparent',
    color: 'var(--ds-text-secondary)',
    cursor: 'pointer',
    fontWeight: 500,
    transitionProperty: 'background-color, color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  messages: {
    flex: 1,
    minHeight: 0,
    overflowY: 'auto',
    padding: `${DESIGN.space.xxl} ${DESIGN.space.xxl}`,
    display: 'flex',
    flexDirection: 'column',
    gap: DESIGN.space.xl,
  },
  empty: {
    color: 'var(--ds-text-muted)',
    fontSize: 14,
    textAlign: 'center',
    marginTop: 64,
  },
  emptyTitle: {
    fontSize: 16,
    fontWeight: 600,
    color: 'var(--ds-text-primary)',
    marginBottom: 4,
  },
  emptySubtitle: {
    fontSize: 13,
    color: 'var(--ds-text-secondary)',
    lineHeight: 1.5,
    marginBottom: 16,
    maxWidth: 480,
    margin: '0 auto 16px',
  },
  quickPrompts: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 8,
    alignItems: 'center',
    maxWidth: 560,
    margin: '0 auto',
  },
  quickPromptBtn: {
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    borderRadius: DESIGN.radii.md,
    padding: '8px 14px',
    color: 'var(--ds-text-secondary)',
    fontSize: 13,
    cursor: 'pointer',
    textAlign: 'left' as const,
    transitionProperty: 'background-color, border-color, color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  msgWrap: { display: 'flex', flexDirection: 'column' },
  userMsg: {
    alignSelf: 'flex-end',
    maxWidth: '75%',
    padding: '12px 16px',
    borderRadius: DESIGN.radii.xl,
    borderBottomRightRadius: 3,
    background: 'var(--ds-accent-bg)',
    color: 'var(--ds-text-primary)',
    fontSize: 14,
    lineHeight: 1.65,
    fontVariantNumeric: 'tabular-nums',
  },
  assistantMsg: {
    alignSelf: 'flex-start',
    maxWidth: '75%',
    padding: '12px 16px',
    borderRadius: DESIGN.radii.xl,
    borderBottomLeftRadius: 3,
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    color: 'var(--ds-text-primary)',
    fontSize: 14,
    lineHeight: 1.65,
    boxShadow: 'var(--ds-card-shadow)',
  },
  role: {
    fontSize: 10,
    fontWeight: 700,
    textTransform: 'uppercase',
    color: 'var(--ds-text-secondary)',
    marginBottom: 6,
    letterSpacing: '0.04em',
  },
  content: { whiteSpace: 'pre-wrap', wordBreak: 'break-word' },
  mdWrap: { color: 'var(--ds-text-primary)', fontSize: 14, lineHeight: 1.65, wordBreak: 'break-word' },
  thinking: { color: 'var(--ds-text-secondary)', fontSize: 13, fontStyle: 'italic' },
  traceBlock: { display: 'flex', flexDirection: 'column', gap: 4 },
  traceRow: {
    display: 'flex',
    alignItems: 'baseline',
    gap: 6,
    fontSize: 12,
    fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace',
    color: 'var(--ds-text-secondary)',
    lineHeight: 1.5,
    fontVariantNumeric: 'tabular-nums',
  },
  traceIcon: { color: 'var(--ds-warning)', flexShrink: 0 },
  traceResultIcon: { color: 'var(--ds-success)', flexShrink: 0 },
  traceName: { color: 'var(--ds-text-primary)', fontWeight: 600, flexShrink: 0 },
  traceArgs: { color: 'var(--ds-text-muted)', whiteSpace: 'pre-wrap', wordBreak: 'break-word', overflow: 'hidden', maxHeight: 60, textOverflow: 'ellipsis' },
  traceResult: { color: 'var(--ds-text-secondary)', whiteSpace: 'pre-wrap', wordBreak: 'break-word', overflow: 'hidden', maxHeight: 60, textOverflow: 'ellipsis' },
  childBadge: { color: 'var(--ds-text-muted)', fontSize: 10, background: 'var(--ds-trace-muted-bg)', padding: '1px 4px', borderRadius: 3, fontFamily: 'ui-monospace, "SFMono-Regular", "Menlo", "Monaco", monospace', flexShrink: 0 },
  clarifyMsg: {
    alignSelf: 'flex-start',
    maxWidth: '75%',
    padding: '12px 16px',
    borderRadius: DESIGN.radii.xl,
    borderBottomLeftRadius: 3,
    background: 'var(--ds-warning-bg)',
    border: '1px solid var(--ds-warning-border)',
    color: 'var(--ds-text-primary)',
    fontSize: 14,
    lineHeight: 1.65,
  },
  clarifyInputArea: {
    display: 'flex',
    gap: 8,
    marginTop: 10,
    alignItems: 'flex-end',
  },
  clarifyTextarea: {
    flex: 1,
    resize: 'none',
    padding: '8px 12px',
    borderRadius: DESIGN.radii.md,
    border: `1px solid var(--ds-border)`,
    background: 'var(--ds-surface)',
    color: 'var(--ds-text-primary)',
    fontSize: 13,
    fontFamily: 'system-ui, sans-serif',
    lineHeight: 1.5,
    outline: 'none',
    transitionProperty: 'border-color, box-shadow',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  errorBanner: {
    padding: '8px 12px',
    borderRadius: DESIGN.radii.md,
    background: 'var(--ds-error-bg)',
    color: '#fca5a5',
    fontSize: 13,
    border: '1px solid #7f1d1d',
  },
  jumpBtn: {
    position: 'absolute' as const,
    bottom: 80,
    right: 28,
    width: 40,
    height: 40,
    borderRadius: '50%',
    background: 'var(--ds-surface)',
    border: `1px solid var(--ds-border)`,
    color: 'var(--ds-text-secondary)',
    cursor: 'pointer',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    boxShadow: 'var(--ds-card-shadow-hover)',
    transitionProperty: 'background-color, border-color, color, transform, opacity',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
    zIndex: 10,
  },
  inputArea: {
    display: 'flex',
    gap: 10,
    padding: '16px 28px',
    borderTop: `1px solid var(--ds-border)`,
    alignItems: 'flex-end',
    flexShrink: 0,
  },
  textarea: {
    flex: 1,
    resize: 'none',
    padding: '10px 14px',
    borderRadius: DESIGN.radii.lg,
    border: `1px solid var(--ds-border)`,
    background: 'var(--ds-surface)',
    color: 'var(--ds-text-primary)',
    fontSize: 14,
    fontFamily: 'system-ui, sans-serif',
    lineHeight: 1.5,
    outline: 'none',
    minHeight: 42,
    maxHeight: 120,
    transitionProperty: 'border-color, box-shadow',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  sendBtn: {
    width: 42,
    height: 42,
    background: 'var(--ds-accent)',
    border: 'none',
    borderRadius: DESIGN.radii.lg,
    cursor: 'pointer',
    color: '#fff',
    fontSize: 18,
    fontWeight: 700,
    flexShrink: 0,
    transitionProperty: 'opacity, background-color, transform',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  cancelBtn: {
    width: 42,
    height: 42,
    background: 'transparent',
    border: `1px solid var(--ds-text-muted)`,
    borderRadius: DESIGN.radii.lg,
    cursor: 'pointer',
    color: 'var(--ds-text-secondary)',
    fontSize: 16,
    fontWeight: 700,
    flexShrink: 0,
    transitionProperty: 'border-color, color, background-color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
  stopBtn: {
    marginTop: 8,
    padding: '4px 12px',
    background: 'transparent',
    border: `1px solid var(--ds-error)`,
    borderRadius: DESIGN.radii.sm,
    cursor: 'pointer',
    color: 'var(--ds-error)',
    fontSize: 12,
    fontWeight: 600,
    transitionProperty: 'background-color, color',
    transitionDuration: '0.15s',
    transitionTimingFunction: 'ease',
  },
}
