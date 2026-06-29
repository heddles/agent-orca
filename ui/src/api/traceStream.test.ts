import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { STREAM_EVENT_TYPE } from '../contracts/events'
import * as sseBarrel from './sse'
import {
  isSSEClientParseEventType,
  isTerminalTraceEvent,
  SSE_CLIENT_PARSE_EVENT_TYPE,
  subscribeToRunStream,
  type TraceEvent,
} from './traceStream'

describe('isSSEClientParseEventType', () => {
  it('is true only for sse_* client recovery kinds', () => {
    expect(isSSEClientParseEventType('sse_malformed_json')).toBe(true)
    expect(isSSEClientParseEventType('sse_missing_or_invalid_type')).toBe(true)
    expect(isSSEClientParseEventType('handoff')).toBe(false)
    expect(isSSEClientParseEventType('')).toBe(false)
  })
})

describe('api/sse barrel', () => {
  it('re-exports traceStream SSE helpers', () => {
    expect(sseBarrel.SSE_CLIENT_PARSE_EVENT_TYPE).toStrictEqual(SSE_CLIENT_PARSE_EVENT_TYPE)
    expect(sseBarrel.isSSEClientParseEventType).toBe(isSSEClientParseEventType)
  })
})


describe('isTerminalTraceEvent', () => {
  it('returns true for terminal stream types', () => {
    expect(isTerminalTraceEvent({ type: STREAM_EVENT_TYPE.finalOutput, output: 'x' })).toBe(true)
    expect(isTerminalTraceEvent({ type: STREAM_EVENT_TYPE.error, message: 'x' })).toBe(true)
    expect(isTerminalTraceEvent({ type: STREAM_EVENT_TYPE.clarify, question: 'x' })).toBe(true)
    expect(isTerminalTraceEvent({ type: STREAM_EVENT_TYPE.done, output: 'x' })).toBe(true)
    expect(isTerminalTraceEvent({ type: STREAM_EVENT_TYPE.fail, reason: 'x' })).toBe(true)
  })

  it('returns false for non-terminal types', () => {
    expect(isTerminalTraceEvent({ type: STREAM_EVENT_TYPE.token, content: 'x' })).toBe(false)
    expect(isTerminalTraceEvent({ type: STREAM_EVENT_TYPE.toolCall, name: 'n', arguments: '{}' })).toBe(false)
  })
})

describe('subscribeToRunStream', () => {
  let mockSource: {
    url: string
    onmessage: ((e: MessageEvent) => void) | null
    onerror: (() => void) | null
    close: ReturnType<typeof vi.fn>
  }

  beforeEach(() => {
    mockSource = {
      url: '',
      onmessage: null,
      onerror: null,
      close: vi.fn(),
    }
    vi.stubGlobal(
      'EventSource',
      vi.fn(function EventSourceMock(this: void, url: string) {
        mockSource.url = url
        return mockSource
      }) as unknown as typeof EventSource,
    )
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('opens EventSource with encoded namespace and run id', () => {
    const onEvent = vi.fn()
    subscribeToRunStream('run-1', onEvent, 'my ns')
    expect(EventSource).toHaveBeenCalledWith('/api/runs/my%20ns/run-1/stream')
  })

  it('delivers parsed events and marks terminal after final_output', () => {
    const onEvent = vi.fn()
    subscribeToRunStream('r', onEvent, 'default')
    expect(mockSource.onmessage).toBeTypeOf('function')

    mockSource.onmessage?.({ data: JSON.stringify({ type: 'token', content: 'hi' }) } as MessageEvent)
    expect(onEvent).toHaveBeenCalledWith({ type: 'token', content: 'hi' })

    mockSource.onmessage?.({ data: JSON.stringify({ type: 'final_output', output: 'done' }) } as MessageEvent)
    expect(onEvent).toHaveBeenLastCalledWith({ type: 'final_output', output: 'done' })

    mockSource.onerror?.()
    expect(onEvent).not.toHaveBeenCalledWith(
      expect.objectContaining({ message: 'Stream connection lost' }),
    )
    expect(mockSource.close).toHaveBeenCalled()
  })

  it('emits agent_event when SSE payload is not valid JSON', () => {
    const onEvent = vi.fn()
    subscribeToRunStream('r', onEvent, 'default')
    mockSource.onmessage?.({ data: 'not-json' } as MessageEvent)
    expect(onEvent).toHaveBeenCalledWith({
      type: STREAM_EVENT_TYPE.agentEvent,
      eventType: SSE_CLIENT_PARSE_EVENT_TYPE.malformedJson,
      message: 'not-json',
    })
  })

  it('emits agent_event when JSON has no string type field', () => {
    const onEvent = vi.fn()
    subscribeToRunStream('r', onEvent, 'default')
    mockSource.onmessage?.({ data: '[1,2,3]' } as MessageEvent)
    expect(onEvent).toHaveBeenCalledWith({
      type: STREAM_EVENT_TYPE.agentEvent,
      eventType: SSE_CLIENT_PARSE_EVENT_TYPE.missingOrInvalidType,
      message: '[1,2,3]',
    })
  })

  it('emits agent_event when type is present but not a string', () => {
    const onEvent = vi.fn()
    subscribeToRunStream('r', onEvent, 'default')
    mockSource.onmessage?.({ data: '{"type":1,"x":true}' } as MessageEvent)
    expect(onEvent).toHaveBeenCalledWith({
      type: STREAM_EVENT_TYPE.agentEvent,
      eventType: SSE_CLIENT_PARSE_EVENT_TYPE.missingOrInvalidType,
      message: '{"type":1,"x":true}',
    })
  })

  it('onerror emits connection lost when stream was not terminated', () => {
    const onEvent = vi.fn()
    subscribeToRunStream('r', onEvent, 'default')
    mockSource.onerror?.()
    expect(onEvent).toHaveBeenCalledWith({ type: 'error', message: 'Stream connection lost' })
  })

  it('cleanup closes the EventSource', () => {
    const onEvent = vi.fn()
    const cleanup = subscribeToRunStream('r', onEvent, 'default')
    cleanup()
    expect(mockSource.close).toHaveBeenCalled()
  })
})
