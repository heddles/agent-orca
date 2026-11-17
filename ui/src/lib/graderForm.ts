/** Grader kinds as forms: what each kind asks for, and the conversion to and from the API config. */
import type { GraderKind } from '../api/evals'

export const KIND_INFO: Record<GraderKind, { label: string; help: string }> = {
  exact: { label: 'Exact match', help: 'The output equals the expected answer of the case, or a fixed value, after the cleanups you pick.' },
  contains: { label: 'Contains', help: 'The output includes some text.' },
  not_contains: { label: 'Does not contain', help: 'The output must not include some text.' },
  regex: { label: 'Pattern', help: 'The output matches a pattern (RE2 syntax, at most 1 KiB).' },
  json_schema: { label: 'JSON schema', help: 'The output is JSON that fits a schema (at most 64 KiB, no remote references).' },
  json_field_equals: { label: 'JSON field equals', help: 'The output is JSON and one field has a given value.' },
  tool_called: { label: 'Tool was called', help: 'The run called a tool.' },
  tool_not_called: { label: 'Tool was not called', help: 'The run did not call a tool.' },
  max_cost: { label: 'Cost at most', help: 'The run cost no more than an amount.' },
  max_latency: { label: 'Time at most', help: 'The run took no longer than a time.' },
}

export const NORMALIZERS = [
  { value: 'trim', label: 'Trim spaces at the ends' },
  { value: 'collapseWhitespace', label: 'Collapse runs of whitespace' },
  { value: 'lowercase', label: 'Ignore upper and lower case' },
] as const

/** All the fields any kind uses, as text, so one form state serves every kind. */
export interface GraderForm {
  normalize: string[]
  value: string
  caseInsensitive: boolean
  pattern: string
  mustMatch: boolean
  schema: string
  path: string
  jsonValue: string
  tool: string
  usd: string
  ms: string
}

export const emptyGraderForm = (): GraderForm => ({
  normalize: [], value: '', caseInsensitive: false, pattern: '', mustMatch: true, schema: '', path: '', jsonValue: '', tool: '', usd: '', ms: '',
})

/** The form state for an existing config. */
export function formFromConfig(kind: GraderKind, config: Record<string, unknown>): GraderForm {
  const f = emptyGraderForm()
  const str = (v: unknown) => (typeof v === 'string' ? v : '')
  switch (kind) {
    case 'exact':
      f.normalize = Array.isArray(config.normalize) ? config.normalize.filter((x): x is string => typeof x === 'string') : []
      f.value = str(config.value)
      break
    case 'contains':
    case 'not_contains':
      f.value = str(config.value)
      f.caseInsensitive = config.caseInsensitive === true
      break
    case 'regex':
      f.pattern = str(config.pattern)
      f.mustMatch = config.mustMatch !== false
      break
    case 'json_schema': f.schema = config.schema === undefined ? '' : JSON.stringify(config.schema, null, 2); break
    case 'json_field_equals':
      f.path = str(config.path)
      f.jsonValue = config.value === undefined ? '' : JSON.stringify(config.value)
      break
    case 'tool_called':
    case 'tool_not_called': f.tool = str(config.tool); break
    case 'max_cost': f.usd = config.usd === undefined ? '' : String(config.usd); break
    case 'max_latency': f.ms = config.ms === undefined ? '' : String(config.ms); break
  }
  return f
}

export type ConfigResult = { ok: true; config: Record<string, unknown> } | { ok: false; error: string }

function parseJSON(text: string): { ok: true; value: unknown } | { ok: false } {
  try { return { ok: true, value: JSON.parse(text) } } catch { return { ok: false } }
}

/** The API config for a form, or a readable problem. The server validates again. */
export function configFromForm(kind: GraderKind, f: GraderForm): ConfigResult {
  const fail = (error: string): ConfigResult => ({ ok: false, error })
  switch (kind) {
    case 'exact': {
      const config: Record<string, unknown> = {}
      if (f.normalize.length) config.normalize = f.normalize
      if (f.value !== '') config.value = f.value
      return { ok: true, config }
    }
    case 'contains':
    case 'not_contains': {
      if (f.value === '') return fail('Enter the text to look for.')
      const config: Record<string, unknown> = { value: f.value }
      if (f.caseInsensitive) config.caseInsensitive = true
      return { ok: true, config }
    }
    case 'regex':
      if (f.pattern === '') return fail('Enter a pattern.')
      if (new TextEncoder().encode(f.pattern).length > 1024) return fail('The pattern is over 1 KiB.')
      return { ok: true, config: f.mustMatch ? { pattern: f.pattern } : { pattern: f.pattern, mustMatch: false } }
    case 'json_schema': {
      const p = parseJSON(f.schema)
      if (!p.ok || typeof p.value !== 'object' || p.value === null || Array.isArray(p.value)) return fail('The schema must be a JSON object.')
      return { ok: true, config: { schema: p.value } }
    }
    case 'json_field_equals': {
      if (f.path.trim() === '') return fail('Enter the path of the field, for example status.')
      const p = parseJSON(f.jsonValue)
      if (!p.ok) return fail('The value must be JSON: text in double quotes, a number, true, false or null.')
      return { ok: true, config: { path: f.path.trim(), value: p.value } }
    }
    case 'tool_called':
    case 'tool_not_called':
      if (f.tool.trim() === '') return fail('Enter the name of the tool.')
      return { ok: true, config: { tool: f.tool.trim() } }
    case 'max_cost': {
      const n = Number(f.usd)
      if (f.usd.trim() === '' || !Number.isFinite(n) || n <= 0) return fail('Enter an amount in US dollars above 0.')
      return { ok: true, config: { usd: n } }
    }
    case 'max_latency': {
      const n = Number(f.ms)
      if (f.ms.trim() === '' || !Number.isInteger(n) || n <= 0) return fail('Enter a whole number of milliseconds above 0.')
      return { ok: true, config: { ms: n } }
    }
  }
}

/** One line saying what a grader checks, for the list. */
export function summarizeGrader(kind: GraderKind, config: Record<string, unknown>): string {
  const q = (v: unknown) => `"${String(v ?? '')}"`
  switch (kind) {
    case 'exact': return config.value !== undefined ? `Equals ${q(config.value)}` : "Equals the case's expected answer"
    case 'contains': return `Contains ${q(config.value)}`
    case 'not_contains': return `Does not contain ${q(config.value)}`
    case 'regex': return `${config.mustMatch === false ? 'Does not match' : 'Matches'} ${String(config.pattern ?? '')}`
    case 'json_schema': return 'Fits a JSON schema'
    case 'json_field_equals': return `${String(config.path ?? '')} = ${JSON.stringify(config.value)}`
    case 'tool_called': return `Called ${String(config.tool ?? '')}`
    case 'tool_not_called': return `Did not call ${String(config.tool ?? '')}`
    case 'max_cost': return `Cost at most $${String(config.usd ?? '')}`
    case 'max_latency': return `Time at most ${String(config.ms ?? '')} ms`
  }
}
