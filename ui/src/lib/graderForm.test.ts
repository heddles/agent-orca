import { describe, expect, it } from 'vitest'
import { GRADER_KINDS } from '../api/evals'
import { configFromForm, emptyGraderForm, formFromConfig, summarizeGrader } from './graderForm'

const form = (over: Partial<ReturnType<typeof emptyGraderForm>>) => ({ ...emptyGraderForm(), ...over })

describe('configFromForm', () => {
  it('builds the config of each kind', () => {
    expect(configFromForm('exact', form({ normalize: ['trim'], value: 'Yes' }))).toEqual({ ok: true, config: { normalize: ['trim'], value: 'Yes' } })
    expect(configFromForm('exact', form({}))).toEqual({ ok: true, config: {} })
    expect(configFromForm('contains', form({ value: 'x', caseInsensitive: true }))).toEqual({ ok: true, config: { value: 'x', caseInsensitive: true } })
    expect(configFromForm('not_contains', form({ value: 'x' }))).toEqual({ ok: true, config: { value: 'x' } })
    expect(configFromForm('regex', form({ pattern: '^a+$', mustMatch: false }))).toEqual({ ok: true, config: { pattern: '^a+$', mustMatch: false } })
    expect(configFromForm('regex', form({ pattern: 'a' }))).toEqual({ ok: true, config: { pattern: 'a' } })
    expect(configFromForm('json_schema', form({ schema: '{"type":"object"}' }))).toEqual({ ok: true, config: { schema: { type: 'object' } } })
    expect(configFromForm('json_field_equals', form({ path: ' status ', jsonValue: '"open"' }))).toEqual({ ok: true, config: { path: 'status', value: 'open' } })
    expect(configFromForm('tool_called', form({ tool: 'search' }))).toEqual({ ok: true, config: { tool: 'search' } })
    expect(configFromForm('tool_not_called', form({ tool: 'delete' }))).toEqual({ ok: true, config: { tool: 'delete' } })
    expect(configFromForm('max_cost', form({ usd: '0.05' }))).toEqual({ ok: true, config: { usd: 0.05 } })
    expect(configFromForm('max_latency', form({ ms: '2000' }))).toEqual({ ok: true, config: { ms: 2000 } })
  })
  it('names the problem in words', () => {
    expect(configFromForm('contains', form({}))).toMatchObject({ ok: false, error: expect.stringMatching(/text/) })
    expect(configFromForm('json_schema', form({ schema: '[1]' }))).toMatchObject({ ok: false })
    expect(configFromForm('json_field_equals', form({ path: 'a', jsonValue: 'open' }))).toMatchObject({ ok: false, error: expect.stringMatching(/JSON/) })
    expect(configFromForm('max_cost', form({ usd: '-1' }))).toMatchObject({ ok: false })
    expect(configFromForm('max_latency', form({ ms: '1.5' }))).toMatchObject({ ok: false })
    expect(configFromForm('regex', form({ pattern: 'a'.repeat(1025) }))).toMatchObject({ ok: false })
  })
  it('round-trips every kind through the form', () => {
    const samples: Record<string, Record<string, unknown>> = {
      exact: { normalize: ['trim', 'lowercase'], value: 'a' }, contains: { value: 'a', caseInsensitive: true }, not_contains: { value: 'a' },
      regex: { pattern: 'a', mustMatch: false }, json_schema: { schema: { type: 'object' } }, json_field_equals: { path: 'a.b', value: 3 },
      tool_called: { tool: 't' }, tool_not_called: { tool: 't' }, max_cost: { usd: 1.5 }, max_latency: { ms: 30 },
    }
    for (const k of GRADER_KINDS) {
      expect(configFromForm(k, formFromConfig(k, samples[k]))).toEqual({ ok: true, config: samples[k] })
      expect(summarizeGrader(k, samples[k]).length).toBeGreaterThan(3)
    }
  })
})
