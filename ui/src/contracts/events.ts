/**
 * Stream event type discriminants emitted by the model-router SSE stream.
 * These string values must stay in sync with the Go constants in internal/router.
 */
export const STREAM_EVENT_TYPE = {
  token:         'token',
  toolCall:      'toolCall',
  toolResult:    'toolResult',
  modelSelected: 'modelSelected',
  finalOutput:   'finalOutput',
  error:         'error',
  clarify:       'clarify',
  done:          'done',
  fail:          'fail',
  agentEvent:    'agentEvent',
  placeholder:   'placeholder',
  // Extended-thinking tokens streamed by reasoning models (delta.reasoning).
  // Emitted by the model-router, surfaced by the UI as an expandable panel.
  thought:       'thought',
} as const

export type StreamEventType = typeof STREAM_EVENT_TYPE[keyof typeof STREAM_EVENT_TYPE]
