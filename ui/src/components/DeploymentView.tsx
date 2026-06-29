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
  const bottomRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLTextAreaElement>(null)
  const unsubRef = useRef<(() => void) | null>(null)
  const childRunUnsubsRef = useRef<Array<() => void>>([])
  const traceCounter = useRef(0)
  const traceEventsRef = useRef<TraceEntry[]>([])
  const sessionRef = useRef<string | null>(null)
  sessionRef.current = sessionId

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
            if (!waiting) return
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
        if (event.type === STREAM_EVENT_TYPE.finalOutput || event.type === STREAM_EVENT_TYPE.done) {
          clearInterval(pollId)
          unsubRef.current = null
          unsub()
          runCompleted = true
          // Final sweep: pick up any child refs that appeared after the last poll.
          getRun(runName, namespace).then((detail) => {
            for (const childName of detail.childRunRefs ?? []) {
              subscribeToChild(childName)
            }
          }).catch(() => {})
          // Snapshot trace entries into the assistant message so they persist.
          const snapshotTrace = [...traceEventsRef.current]
          const traceJSON = snapshotTrace.length > 0 ? JSON.stringify(snapshotTrace) : undefined
          setMessages((prev) => {
            const next: LocalMessage[] = [...prev, { role: 'assistant' as const, content: event.output, traceEntries: snapshotTrace.length > 0 ? snapshotTrace : undefined }]
            saveCachedMessages(namespace, name, sid, next)
            return next
          })
          setPending(null)
          try {
            await saveChatResponse(namespace, name, sid, event.output, traceJSON)
          } catch {
            // Checkpoint unavailable — optimistic message already displayed.
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
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      send()
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
        {deployment && (
          <div style={s.depMeta}>
            {deployment.readyReplicas} replica{deployment.readyReplicas !== 1 ? 's' : ''} · {deployment.agentRef}
            {deployment.inputSourceType && ` · ${deployment.inputSourceType}`}
          </div>
        )}
        {deployment && (deployment.maxContextTokens ?? 0) > 0 && (
          <span style={s.contextChip}>
            📏 {deployment.contextUsedTokens?.toLocaleString() ?? 0} / {deployment.maxContextTokens?.toLocaleString() ?? '—'} tokens
          </span>
        )}
        <div style={{ flex: 1 }} />
        <span style={s.costChip}>${totalCost}</span>
        {sessionId && viewTab === 'chat' && (
          <button style={s.newBtn} onClick={newSession}>New session</button>
        )}
      </div>

      {/* Tab bar */}
      <div style={s.tabBar}>
        <button
          style={{ ...s.tab, ...(viewTab === 'chat' ? s.tabActive : {}) }}
          onClick={() => setViewTab('chat')}
        >
          Chat
        </button>
        <button
          style={{ ...s.tab, ...(viewTab === 'runs' ? s.tabActive : {}) }}
          onClick={() => setViewTab('runs')}
        >
          Runs ({runs.length})
        </button>
      </div>

      {viewTab === 'runs' ? (
        <div style={s.runsList}>
          {runs.length === 0 && (
            <div style={s.empty}>No runs yet. Send a message to create one.</div>
          )}
          {runs.map((run) => (
            <div
              key={run.name}
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
            </div>
          ))}
        </div>
      ) : (
      <>
      {/* Messages */}
      <div style={s.messages}>
        {messages.length === 0 && !pending && (
          <div style={s.empty}>
            Send a message to start chatting with <strong>{name}</strong>
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
                          <span style={s.traceIcon}>⚙</span>
                          {badge}
                          <span style={s.traceName}>{ev.name}</span>
                          <span style={s.traceArgs}>{ev.arguments}</span>
                        </>
                      ) : ev.type === STREAM_EVENT_TYPE.toolResult ? (
                        <>
                          <span style={s.traceResultIcon}>✓</span>
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
                            <span style={s.traceIcon}>⚙</span>
                            {badge}
                            <span style={s.traceName}>{ev.name}</span>
                            <span style={s.traceArgs}>{ev.arguments}</span>
                          </>
                        ) : ev.type === STREAM_EVENT_TYPE.toolResult ? (
                          <>
                            <span style={s.traceResultIcon}>✓</span>
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
              >
                ■ Stop
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
                    if (e.key === 'Enter' && !e.shiftKey) {
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
                >
                  ↑
                </button>
                <button
                  style={s.cancelBtn}
                  onClick={cancelClarify}
                  title="Cancel this request"
                >
                  ✕
                </button>
              </div>
            </div>
          </div>
        )}
        {error && <div style={s.errorBanner}>{error}</div>}
        <div ref={bottomRef} />
      </div>

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
          style={{ ...s.sendBtn, opacity: input.trim() && pending?.status !== 'running' && !clarify ? 1 : 0.4 }}
          onClick={send}
          disabled={!input.trim() || pending?.status === 'running' || !!clarify}
        >
          ↑
        </button>
      </div>
      </>
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
    borderBottom: '1px solid #334155',
    background: '#0f172a',
    flexShrink: 0,
  },
  depName: { fontSize: 16, fontWeight: 700, color: '#f1f5f9' },
  depMeta: { fontSize: 12, color: '#64748b' },
  costChip: {
    fontSize: 12,
    fontWeight: 600,
    color: '#4ade80',
    background: 'rgba(74,222,128,.1)',
    padding: '3px 10px',
    borderRadius: 12,
    fontFamily: 'monospace',
  },
  contextChip: {
    fontSize: 12,
    fontWeight: 600,
    color: '#3b82f6',
    background: 'rgba(59,130,246,.1)',
    padding: '3px 10px',
    borderRadius: 12,
    fontFamily: 'monospace',
    marginLeft: 8,
  },
  tabBar: {
    display: 'flex',
    gap: 0,
    borderBottom: '1px solid #334155',
    background: '#0f172a',
    flexShrink: 0,
    padding: '0 28px',
  },
  tab: {
    padding: '8px 16px',
    fontSize: 13,
    fontWeight: 500,
    color: '#64748b',
    background: 'none',
    border: 'none',
    borderBottom: '2px solid transparent',
    cursor: 'pointer',
    transition: 'color 0.15s',
  },
  tabActive: {
    color: '#3b82f6',
    borderBottomColor: '#3b82f6',
  },
  runsList: {
    flex: 1,
    overflowY: 'auto',
    padding: '12px 28px',
    display: 'flex',
    flexDirection: 'column',
    gap: 6,
  },
  runCard: {
    padding: '10px 14px',
    borderRadius: 8,
    border: '1px solid #334155',
    background: '#1e293b',
    cursor: 'pointer',
    transition: 'border-color 0.15s, background 0.15s',
    display: 'flex',
    flexDirection: 'column',
    gap: 4,
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
    fontSize: 13,
    fontWeight: 600,
    color: '#f1f5f9',
    fontFamily: 'monospace',
  },
  runCardPhase: {
    fontSize: 11,
    color: '#64748b',
  },
  runCardCost: {
    fontSize: 11,
    fontWeight: 600,
    color: '#4ade80',
    fontFamily: 'monospace',
  },
  runCardMeta: {
    fontSize: 11,
    color: '#64748b',
    paddingLeft: 15,
  },
  newBtn: {
    fontSize: 11,
    padding: '4px 10px',
    borderRadius: 6,
    border: '1px solid #334155',
    background: 'transparent',
    color: '#94a3b8',
    cursor: 'pointer',
    fontWeight: 500,
  },
  messages: {
    flex: 1,
    minHeight: 0,
    overflowY: 'auto',
    padding: '20px 28px',
    display: 'flex',
    flexDirection: 'column',
    gap: 16,
  },
  empty: {
    color: '#475569',
    fontSize: 14,
    textAlign: 'center',
    marginTop: 64,
  },
  msgWrap: { display: 'flex', flexDirection: 'column' },
  userMsg: {
    alignSelf: 'flex-end',
    maxWidth: '75%',
    padding: '12px 16px',
    borderRadius: 12,
    borderBottomRightRadius: 3,
    background: 'rgba(59,130,246,.2)',
    color: '#f1f5f9',
    fontSize: 14,
    lineHeight: 1.65,
  },
  assistantMsg: {
    alignSelf: 'flex-start',
    maxWidth: '75%',
    padding: '12px 16px',
    borderRadius: 12,
    borderBottomLeftRadius: 3,
    background: '#1e293b',
    border: '1px solid #334155',
    color: '#f1f5f9',
    fontSize: 14,
    lineHeight: 1.65,
  },
  role: {
    fontSize: 10,
    fontWeight: 700,
    textTransform: 'uppercase',
    color: '#94a3b8',
    marginBottom: 6,
    letterSpacing: '0.04em',
  },
  content: { whiteSpace: 'pre-wrap', wordBreak: 'break-word' },
  mdWrap: { color: '#f1f5f9', fontSize: 14, lineHeight: 1.65, wordBreak: 'break-word' },
  thinking: { color: '#64748b', fontSize: 13, fontStyle: 'italic' },
  traceBlock: { display: 'flex', flexDirection: 'column' as const, gap: 4 },
  traceRow: {
    display: 'flex',
    alignItems: 'baseline',
    gap: 6,
    fontSize: 12,
    fontFamily: 'monospace',
    color: '#94a3b8',
    lineHeight: 1.5,
  },
  traceIcon: { color: '#f59e0b', flexShrink: 0 },
  traceResultIcon: { color: '#10b981', flexShrink: 0 },
  traceName: { color: '#f1f5f9', fontWeight: 600, flexShrink: 0 },
  traceArgs: { color: '#64748b', whiteSpace: 'pre-wrap' as const, wordBreak: 'break-word' as const, overflow: 'hidden', maxHeight: 60, textOverflow: 'ellipsis' },
  traceResult: { color: '#94a3b8', whiteSpace: 'pre-wrap' as const, wordBreak: 'break-word' as const, overflow: 'hidden', maxHeight: 60, textOverflow: 'ellipsis' },
  childBadge: { color: '#475569', fontSize: 10, background: 'rgba(148,163,184,.08)', padding: '1px 4px', borderRadius: 3, fontFamily: 'monospace', flexShrink: 0 },
  clarifyMsg: {
    alignSelf: 'flex-start',
    maxWidth: '75%',
    padding: '12px 16px',
    borderRadius: 12,
    borderBottomLeftRadius: 3,
    background: 'rgba(245,158,11,.1)',
    border: '1px solid rgba(245,158,11,.3)',
    color: '#f1f5f9',
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
    borderRadius: 8,
    border: '1px solid #334155',
    background: '#1e293b',
    color: '#f1f5f9',
    fontSize: 13,
    fontFamily: 'system-ui, sans-serif',
    lineHeight: 1.5,
    outline: 'none',
  },
  errorBanner: {
    padding: '8px 12px',
    borderRadius: 8,
    background: '#3a1e1e',
    color: '#fca5a5',
    fontSize: 13,
    border: '1px solid #7f1d1d',
  },
  inputArea: {
    display: 'flex',
    gap: 10,
    padding: '16px 20px',
    borderTop: '1px solid #334155',
    alignItems: 'flex-end',
    flexShrink: 0,
  },
  textarea: {
    flex: 1,
    resize: 'none',
    padding: '10px 14px',
    borderRadius: 10,
    border: '1px solid #334155',
    background: '#1e293b',
    color: '#f1f5f9',
    fontSize: 14,
    fontFamily: 'system-ui, sans-serif',
    lineHeight: 1.5,
    outline: 'none',
    minHeight: 42,
    maxHeight: 120,
  },
  sendBtn: {
    width: 42,
    height: 42,
    background: '#3b82f6',
    border: 'none',
    borderRadius: 10,
    cursor: 'pointer',
    color: '#fff',
    fontSize: 18,
    fontWeight: 700,
    flexShrink: 0,
    transition: 'opacity 0.15s',
  },
  cancelBtn: {
    width: 42,
    height: 42,
    background: 'transparent',
    border: '1px solid #475569',
    borderRadius: 10,
    cursor: 'pointer',
    color: '#94a3b8',
    fontSize: 16,
    fontWeight: 700,
    flexShrink: 0,
    transition: 'border-color 0.15s, color 0.15s',
  },
  stopBtn: {
    marginTop: 8,
    padding: '4px 12px',
    background: 'transparent',
    border: '1px solid #ef4444',
    borderRadius: 6,
    cursor: 'pointer',
    color: '#ef4444',
    fontSize: 12,
    fontWeight: 600,
    transition: 'background 0.15s, color 0.15s',
  } as React.CSSProperties,
}
