import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { STREAM_EVENT_TYPE } from '../contracts/events'
import { SSE_CLIENT_PARSE_EVENT_TYPE } from '../api/traceStream'
import type { TraceEvent } from '../api/sse'
import { TraceAccordion } from './TraceAccordion'

const ts = '2026-01-15T12:00:00.000Z'

function entry(id: number, event: TraceEvent) {
  return { id, event, ts }
}

async function openAccordion() {
  await userEvent.click(screen.getByText(/Execution Trace|Live Trace/))
}

describe('TraceAccordion', () => {
  it('shows streaming label when streaming', () => {
    render(<TraceAccordion entries={[]} streaming />)
    expect(screen.getByText(/streaming/)).toBeInTheDocument()
  })

  it('shows event count when not streaming', () => {
    render(<TraceAccordion entries={[entry(0, { type: 'thought', content: 'x' })]} />)
    expect(screen.getByText('1 events')).toBeInTheDocument()
  })

  it('shows empty body when expanded with no entries', async () => {
    render(<TraceAccordion entries={[]} />)
    await openAccordion()
    expect(screen.getByText(/Checking for traces./)).toBeInTheDocument()
  })

  it('shows retrieving events with a quip when streaming and empty', async () => {
    render(<TraceAccordion entries={[]} streaming />)
    await openAccordion()
    expect(screen.getByText(/Retrieving events/)).toBeInTheDocument()
  })

  it('renders thought as an expandable thinking panel', async () => {
    const user = userEvent.setup()
    render(
      <TraceAccordion
        entries={[
          entry(0, { type: 'thought', content: 'planning' }),
          entry(1, { type: STREAM_EVENT_TYPE.toolCall, name: 'x', arguments: '{}' }),
          entry(2, { type: STREAM_EVENT_TYPE.toolResult, name: 'x', result: 'ok' }),
        ]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/thinking/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /expand thinking/i })).toBeInTheDocument()
    // Collapsed by default — the reasoning content is not in the DOM yet.
    expect(screen.queryByText('planning')).not.toBeInTheDocument()
    // Expanding reveals the captured thinking.
    await user.click(screen.getByRole('button', { name: /expand thinking/i }))
    expect(screen.getByText('planning')).toBeInTheDocument()
    expect(screen.getByText(/calls tool/)).toBeInTheDocument()
    expect(screen.getByText(/result/)).toBeInTheDocument()
  })

  it('renders tool_result with markdown path when markdown is on', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: STREAM_EVENT_TYPE.toolResult, name: 'fn', result: '`code`' })]}
        markdown
      />,
    )
    await openAccordion()
    expect(screen.getByText('code')).toBeInTheDocument()
  })

  it('renders model_selected with confidence', async () => {
    render(
      <TraceAccordion entries={[entry(0, { type: STREAM_EVENT_TYPE.modelSelected, model: 'm', reason: 'r', confidence: 0.42 })]} />,
    )
    await openAccordion()
    expect(screen.getByText(/routed/)).toBeInTheDocument()
    expect(screen.getByText(/42%/)).toBeInTheDocument()
  })

  it('renders final_output with markdown when enabled', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: STREAM_EVENT_TYPE.finalOutput, output: '# Title' })]}
        markdown
      />,
    )
    await openAccordion()
    expect(screen.getByRole('heading', { level: 1, name: 'Title' })).toBeInTheDocument()
  })

  it('renders final_output and done as plain text when markdown is off', async () => {
    render(
      <TraceAccordion
        entries={[
          entry(0, { type: STREAM_EVENT_TYPE.finalOutput, output: 'plain out' }),
          entry(1, { type: STREAM_EVENT_TYPE.done, output: 'done plain' }),
        ]}
      />,
    )
    await openAccordion()
    expect(screen.getByText('plain out')).toBeInTheDocument()
    expect(screen.getByText('done plain')).toBeInTheDocument()
  })

  it('renders done with markdown when enabled', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: STREAM_EVENT_TYPE.done, output: '## Done' })]}
        markdown
      />,
    )
    await openAccordion()
    expect(screen.getByRole('heading', { level: 2, name: 'Done' })).toBeInTheDocument()
  })

  it('renders error, token, clarify', async () => {
    render(
      <TraceAccordion
        entries={[
          entry(0, { type: 'error', message: 'bad' }),
          entry(1, { type: 'token', content: 't' }),
          entry(2, { type: 'clarify', question: 'q?' }),
        ]}
      />,
    )
    await openAccordion()
    expect(screen.getByText('bad')).toBeInTheDocument()
    expect(screen.getByText(/token/)).toBeInTheDocument()
    expect(screen.getByText(/clarify/)).toBeInTheDocument()
  })

  it('renders done, fail, agent_event, placeholder', async () => {
    render(
      <TraceAccordion
        entries={[
          entry(0, { type: STREAM_EVENT_TYPE.done, output: 'done out' }),
          entry(1, { type: STREAM_EVENT_TYPE.fail, reason: 'fail r' }),
          entry(2, { type: STREAM_EVENT_TYPE.agentEvent, eventType: 'evt', message: 'msg' }),
          entry(3, { type: STREAM_EVENT_TYPE.placeholder, message: 'wait' }),
        ]}
      />,
    )
    await openAccordion()
    expect(screen.getByText('done out')).toBeInTheDocument()
    expect(screen.getByText('fail r')).toBeInTheDocument()
    expect(screen.getByText(/evt/)).toBeInTheDocument()
    expect(screen.getByText('wait')).toBeInTheDocument()
  })

  it('renders sse_* agent_event rows with SSE parse styling', async () => {
    render(
      <TraceAccordion
        entries={[
          entry(0, {
            type: STREAM_EVENT_TYPE.agentEvent,
            eventType: SSE_CLIENT_PARSE_EVENT_TYPE.malformedJson,
            message: 'not-json{',
          }),
          entry(1, {
            type: STREAM_EVENT_TYPE.agentEvent,
            eventType: SSE_CLIENT_PARSE_EVENT_TYPE.missingOrInvalidType,
            message: '{}',
          }),
        ]}
      />,
    )
    await openAccordion()
    expect(screen.getAllByText(/SSE parse/).length).toBe(2)
    expect(screen.getByText(SSE_CLIENT_PARSE_EVENT_TYPE.malformedJson)).toBeInTheDocument()
    expect(screen.getByText(SSE_CLIENT_PARSE_EVENT_TYPE.missingOrInvalidType)).toBeInTheDocument()
    expect(screen.getByText('not-json{')).toBeInTheDocument()
  })

  it('falls back to JSON for unknown event types', async () => {
    const bogus = { type: 'unknown_kind', foo: 1 } as unknown as TraceEvent
    render(<TraceAccordion entries={[entry(0, bogus)]} />)
    await openAccordion()
    expect(screen.getByText(/unknown trace type/)).toBeInTheDocument()
    expect(screen.getAllByText('unknown_kind').length).toBeGreaterThanOrEqual(1)
  })

  it('renders ragResult with KB name, query, and result count', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: 'ragResult', name: 'llm-research-kb', query: 'attention mechanism', results: 7, collection: 'research-docs' } as TraceEvent)]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/RAG search/)).toBeInTheDocument()
    expect(screen.getByText(/7 results/)).toBeInTheDocument()
    expect(screen.getByText(/research-docs/)).toBeInTheDocument()
  })

  it('renders mcpDiscovery with server name and tool count', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: 'mcpDiscovery', server: 'filesystem', tools: 12 } as TraceEvent)]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/MCP server/)).toBeInTheDocument()
    expect(screen.getByText(/filesystem/)).toBeInTheDocument()
    expect(screen.getByText(/12 tools/)).toBeInTheDocument()
  })

  it('renders guardrail block event', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: 'guardrail', action: 'blocked', reason: 'PII detected' } as TraceEvent)]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/guardrail/)).toBeInTheDocument()
    expect(screen.getByText(/blocked/)).toBeInTheDocument()
    expect(screen.getByText(/PII detected/)).toBeInTheDocument()
  })

  it('renders providerFallback event', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: 'providerFallback', from: 'openai', to: 'anthropic', reason: 'rate limited' } as TraceEvent)]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/fallback/)).toBeInTheDocument()
    expect(screen.getByText(/openai/)).toBeInTheDocument()
    expect(screen.getByText(/anthropic/)).toBeInTheDocument()
  })

  it('renders providerFallback exhausted event', async () => {
    render(
      <TraceAccordion
        entries={[entry(0, { type: 'providerFallback', from: 'openai', to: '', reason: 'timeout', exhausted: true } as TraceEvent)]}
      />,
    )
    await openAccordion()
    expect(screen.getByText(/exhausted/)).toBeInTheDocument()
  })
})
