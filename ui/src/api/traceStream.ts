/** SSE trace stream types and subscription — shared with model-router / uiapi event shapes. */

import { STREAM_EVENT_TYPE } from '../contracts/events'

// ── Types ────────────────────────────────────────────────────────────────────

/**
 * SSE `type` discriminants for trace events.
 * These string values must stay in sync with the Go constants in internal/router.
 */
export const TRACE_EVENT_TYPE = {
  ...STREAM_EVENT_TYPE,
  thought: 'thought',
} as const

export type TraceEvent =
  | { type: typeof TRACE_EVENT_TYPE.thought; content: string }
  | { type: typeof STREAM_EVENT_TYPE.token; content: string }
  | { type: typeof STREAM_EVENT_TYPE.toolCall; name: string; arguments: string }
  | { type: typeof STREAM_EVENT_TYPE.toolResult; name: string; result: string; appUrl?: string; toolArgs?: string; toolResult?: string }
  | { type: typeof STREAM_EVENT_TYPE.modelSelected; model: string; reason: string; confidence: number }
  | { type: typeof STREAM_EVENT_TYPE.finalOutput; output: string }
  | { type: typeof STREAM_EVENT_TYPE.error; message: string }
  | { type: typeof STREAM_EVENT_TYPE.clarify; question: string }
  | { type: typeof STREAM_EVENT_TYPE.done; output: string }
  | { type: typeof STREAM_EVENT_TYPE.fail; reason: string }
  | { type: typeof STREAM_EVENT_TYPE.agentEvent; eventType: string; message: string }
  | { type: typeof STREAM_EVENT_TYPE.placeholder; message: string }

/** True when the server typically closes the SSE connection after this event (router done/fail, uiapi terminal). */
export function isTerminalTraceEvent(event: TraceEvent): boolean {
  switch (event.type) {
    case STREAM_EVENT_TYPE.finalOutput:
    case STREAM_EVENT_TYPE.error:
    case STREAM_EVENT_TYPE.clarify:
    case STREAM_EVENT_TYPE.done:
    case STREAM_EVENT_TYPE.fail:
      return true
    default:
      return false
  }
}

export type TraceEventHandler = (event: TraceEvent) => void

/** A timestamped trace event entry, used in trace display components. */
export interface TraceEntry {
  id: number
  event: TraceEvent
  ts: string
  /** Set when this entry originated from a child run's stream. */
  childRunName?: string
}

/** Synthetic `agent_event` kinds emitted by the UI client when SSE JSON fails validation (§5 guardrails). */
export const SSE_CLIENT_PARSE_EVENT_TYPE = {
  malformedJson: 'sse_malformed_json',
  missingOrInvalidType: 'sse_missing_or_invalid_type',
} as const

/** True when `eventType` is reserved for client-side SSE parse recovery (`sse_*`). */
export function isSSEClientParseEventType(eventType: string): boolean {
  return eventType.startsWith('sse_')
}

const MAX_SSE_PAYLOAD_PREVIEW = 512

function truncateTracePayload(s: string, max: number): string {
  if (s.length <= max) return s
  return s.slice(0, max) + '…'
}

/**
 * Subscribe to a run's SSE event stream.
 * Returns a cleanup function to close the connection.
 */
export function subscribeToRunStream(runId: string, onEvent: TraceEventHandler, namespace: string): () => void {
  const url = `/api/runs/${encodeURIComponent(namespace)}/${runId}/stream`
  const source = new EventSource(url)
  let terminated = false

  source.onmessage = (e) => {
    const raw = typeof e.data === 'string' ? e.data : String(e.data)
    let parsed: unknown
    try {
      parsed = JSON.parse(raw)
    } catch {
      onEvent({
        type: STREAM_EVENT_TYPE.agentEvent,
        eventType: SSE_CLIENT_PARSE_EVENT_TYPE.malformedJson,
        message: truncateTracePayload(raw, MAX_SSE_PAYLOAD_PREVIEW),
      })
      return
    }
    if (
      parsed === null ||
      typeof parsed !== 'object' ||
      Array.isArray(parsed) ||
      !('type' in parsed) ||
      typeof (parsed as { type?: unknown }).type !== 'string'
    ) {
      onEvent({
        type: STREAM_EVENT_TYPE.agentEvent,
        eventType: SSE_CLIENT_PARSE_EVENT_TYPE.missingOrInvalidType,
        message: truncateTracePayload(raw, MAX_SSE_PAYLOAD_PREVIEW),
      })
      return
    }
    const event = parsed as TraceEvent
    if (isTerminalTraceEvent(event)) {
      terminated = true
    }
    onEvent(event)
  }

  source.onerror = () => {
    // Only report as an error if the stream closed unexpectedly.
    // Server closes the connection after final_output/error — that's normal.
    if (!terminated) {
      onEvent({ type: 'error', message: 'Stream connection lost' })
    }
    source.close()
  }

  return () => source.close()
}
