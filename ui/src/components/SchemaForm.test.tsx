import { describe, expect, it, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { buildPayload, fieldKind, initialValues, isPlainTextSchema, parseRawInput, payloadToMessage, type JSONSchema } from '../lib/schemaForm'
import { SchemaForm } from './SchemaForm'

const mockStart = vi.fn()
vi.mock('../api/catalog', async () => {
  const actual = await vi.importActual<typeof import('../api/catalog')>('../api/catalog')
  return { ...actual, startAgentRun: (...a: unknown[]) => mockStart(...a) }
})
vi.mock('./DeploymentView', () => ({ DeploymentView: () => null }))

import { RunForm } from './AgentPage'

const schema: JSONSchema = {
  type: 'object',
  required: ['topic', 'depth'],
  properties: {
    topic: { type: 'string', title: 'Topic', description: 'What to research.' },
    depth: { type: 'integer', minimum: 1, maximum: 5 },
    format: { type: 'string', enum: ['brief', 'full'] },
    cite: { type: 'boolean', description: 'Include sources.' },
    sources: { type: 'array', items: { type: 'string' } },
    options: {
      type: 'object',
      required: ['region'],
      properties: { region: { type: 'string' }, limit: { type: 'number' } },
    },
    matrix: { type: 'array', items: { type: 'object' } },
  },
}

function Harness({ onValues }: { onValues?: (v: unknown) => void }) {
  const [values, setValues] = useState(initialValues(schema))
  const [errors, setErrors] = useState({})
  onValues?.(values)
  return (
    <>
      <SchemaForm schema={schema} values={values} onChange={setValues} errors={errors} />
      <button type="button" onClick={() => setErrors(buildPayload(schema, values).errors)}>check</button>
    </>
  )
}

describe('schema form model', () => {
  it('classifies fields and falls back to JSON for unsupported shapes', () => {
    expect(fieldKind({ type: 'string' })).toBe('string')
    expect(fieldKind({ type: 'string', enum: ['a'] })).toBe('enum')
    expect(fieldKind({ type: 'array', items: { type: 'string' } })).toBe('stringArray')
    expect(fieldKind({ type: 'array', items: { type: 'object' } })).toBe('json')
    expect(fieldKind({ type: 'object', properties: { a: { type: 'string' } } }, 0)).toBe('object')
    expect(fieldKind({ type: 'object', properties: { a: { type: 'string' } } }, 1)).toBe('json')
    expect(fieldKind({ oneOf: [] })).toBe('json')
  })

  it('reports required, type and range errors with the field path', () => {
    const r = buildPayload(schema, { ...initialValues(schema), depth: '9', options: { region: '', limit: 'x' } })
    expect(r.payload).toBeNull()
    expect(r.errors.topic).toMatch(/required/)
    expect(r.errors.depth).toMatch(/at most 5/)
    expect(r.errors['options.region']).toMatch(/required/)
    expect(r.errors['options.limit']).toMatch(/number/)
  })

  it('builds a typed payload and drops empty optional fields', () => {
    const r = buildPayload(schema, {
      ...initialValues(schema), topic: 'whales', depth: '3', format: 'full', cite: true,
      sources: 'a\n\n b ', options: { region: 'EU', limit: '' }, matrix: '[[1,2]]',
    })
    expect(r.errors).toEqual({})
    expect(r.payload).toEqual({
      topic: 'whales', depth: 3, format: 'full', cite: true, sources: ['a', 'b'],
      options: { region: 'EU' }, matrix: [[1, 2]],
    })
  })

  it('rejects invalid JSON in a fallback field', () => {
    const r = buildPayload(schema, { ...initialValues(schema), topic: 't', depth: '1', options: { region: 'x', limit: '' }, matrix: '{oops' })
    expect(r.errors.matrix).toMatch(/not valid JSON/)
  })

  it('validates raw JSON against the required keys', () => {
    expect(parseRawInput(schema, '').error).toBeTruthy()
    expect(parseRawInput(schema, '{').error).toMatch(/valid JSON/)
    expect(parseRawInput(schema, '[]').error).toMatch(/object/)
    expect(parseRawInput(schema, '{"topic":"a"}').error).toMatch(/depth/)
    expect(parseRawInput(schema, '{"topic":"a","depth":1}').error).toBeUndefined()
  })

  it('sends plain text for the default schema and JSON otherwise', () => {
    const plain: JSONSchema = { type: 'object', properties: { input: { type: 'string' } }, required: ['input'] }
    expect(isPlainTextSchema(plain)).toBe(true)
    expect(payloadToMessage(plain, { input: 'hello' })).toBe('hello')
    expect(payloadToMessage(schema, { topic: 'a' })).toBe('{\n  "topic": "a"\n}')
  })
})

describe('SchemaForm', () => {
  it('labels every field and links descriptions as help text', () => {
    render(<Harness />)
    const topic = screen.getByLabelText(/Topic/)
    expect(topic).toHaveAttribute('aria-required', 'true')
    expect(topic).toHaveAccessibleDescription('What to research.')
    expect(screen.getByLabelText('format')).toBeInstanceOf(HTMLSelectElement)
    expect(screen.getByLabelText(/depth/)).toHaveAttribute('type', 'number')
    expect(screen.getByRole('checkbox', { name: /cite/ })).toBeInTheDocument()
    expect(screen.getByRole('group', { name: /options/ })).toBeInTheDocument()
    expect(screen.getByLabelText(/region/)).toBeInTheDocument()
    expect(screen.getByLabelText('matrix')).toHaveAccessibleDescription(/enter it as JSON/)
  })

  it('shows an announced error next to the field', async () => {
    render(<Harness />)
    await userEvent.click(screen.getByRole('button', { name: 'check' }))
    const alerts = await screen.findAllByRole('alert')
    expect(alerts.map((a) => a.textContent)).toContain('Topic is required.')
    expect(screen.getByLabelText(/Topic/)).toHaveAttribute('aria-invalid', 'true')
  })

  it('edits values', async () => {
    let latest: unknown
    render(<Harness onValues={(v) => { latest = v }} />)
    await userEvent.type(screen.getByLabelText(/Topic/), 'ice')
    await userEvent.selectOptions(screen.getByLabelText('format'), 'brief')
    await userEvent.click(screen.getByRole('checkbox', { name: /cite/ }))
    expect(latest).toMatchObject({ topic: 'ice', format: 'brief', cite: true })
  })
})

describe('RunForm', () => {
  beforeEach(() => { mockStart.mockReset() })

  it('validates before it submits and does not call the API', async () => {
    render(<RunForm namespace="team-a" name="researcher" schema={schema} examples={[]} onStarted={vi.fn()} />)
    await userEvent.click(screen.getByRole('button', { name: 'Run agent' }))
    expect((await screen.findAllByRole('alert')).length).toBeGreaterThan(0)
    expect(mockStart).not.toHaveBeenCalled()
  })

  it('starts the run with the form as JSON and reports the run', async () => {
    mockStart.mockResolvedValue({ run_id: 'run-9' })
    const onStarted = vi.fn()
    render(<RunForm namespace="team-a" name="researcher" schema={schema} examples={[]} onStarted={onStarted} />)
    await userEvent.type(screen.getByLabelText(/Topic/), 'ice')
    await userEvent.type(screen.getByLabelText(/depth/), '2')
    await userEvent.type(screen.getByLabelText(/region/), 'EU')
    await userEvent.click(screen.getByRole('button', { name: 'Run agent' }))
    await waitFor(() => expect(onStarted).toHaveBeenCalledWith('run-9'))
    expect(JSON.parse(mockStart.mock.calls[0][2])).toEqual({ topic: 'ice', depth: 2, options: { region: 'EU' } })
  })

  it('sends plain text for the default one-field agent and offers examples', async () => {
    mockStart.mockResolvedValue({ run_id: 'run-1' })
    const plain: JSONSchema = { type: 'object', properties: { input: { type: 'string', description: 'The task' } }, required: ['input'] }
    render(<RunForm namespace="team-a" name="helper" schema={plain} examples={['Summarise this week']} onStarted={vi.fn()} />)
    await userEvent.click(screen.getByRole('button', { name: 'Summarise this week' }))
    expect(screen.getByLabelText(/What should the agent do/)).toHaveValue('Summarise this week')
    await userEvent.click(screen.getByRole('button', { name: 'Run agent' }))
    await waitFor(() => expect(mockStart).toHaveBeenCalledWith('team-a', 'helper', 'Summarise this week'))
  })

  it('falls back to raw JSON for a schema it cannot show', async () => {
    mockStart.mockResolvedValue({ run_id: 'r' })
    render(<RunForm namespace="n" name="a" schema={{ type: 'array', items: { type: 'number' } }} examples={[]} onStarted={vi.fn()} />)
    const box = screen.getByLabelText('Input (JSON)')
    await userEvent.click(screen.getByRole('button', { name: 'Run agent' }))
    expect(await screen.findByText('Enter the input to send.')).toBeInTheDocument()
    await userEvent.click(box)
    await userEvent.paste('[1,2]')
    await userEvent.click(screen.getByRole('button', { name: 'Run agent' }))
    await waitFor(() => expect(mockStart).toHaveBeenCalledWith('n', 'a', '[\n  1,\n  2\n]'))
  })

  it('shows why a run could not start', async () => {
    mockStart.mockImplementation(() => Promise.reject(new Error('quota exceeded')))
    const plain: JSONSchema = { type: 'object', properties: { input: { type: 'string' } }, required: ['input'] }
    render(<RunForm namespace="n" name="a" schema={plain} examples={[]} onStarted={vi.fn()} />)
    await userEvent.type(screen.getByLabelText(/What should the agent do/), 'go')
    await userEvent.click(screen.getByRole('button', { name: 'Run agent' }))
    expect(await screen.findByText(/quota exceeded/)).toBeInTheDocument()
  })
})
