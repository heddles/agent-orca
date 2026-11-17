/**
 * Run-form model generated from an agent's input JSON Schema.
 *
 * Supported: string, number/integer, boolean, enum, arrays of strings, and
 * objects nested one level deep, with `required`, `description` as help text,
 * `title`, `default`, `minimum`/`maximum` and `minLength`/`maxLength`.
 * Anything else (arrays of objects, deeper nesting, unions) is edited as raw
 * JSON in that one field, so no schema is ever unusable.
 */

export interface JSONSchema {
  type?: string | string[]
  title?: string
  description?: string
  properties?: Record<string, JSONSchema>
  required?: string[]
  enum?: unknown[]
  items?: JSONSchema
  default?: unknown
  minimum?: number
  maximum?: number
  minLength?: number
  maxLength?: number
  [key: string]: unknown
}

export type FieldKind = 'string' | 'number' | 'integer' | 'boolean' | 'enum' | 'stringArray' | 'object' | 'json'

/** What a form field edits. Values are held as the user typed them. */
export type FieldValue = string | boolean | FormValues
export interface FormValues { [key: string]: FieldValue }

function typeOf(schema: JSONSchema): string | undefined {
  const t = schema.type
  if (Array.isArray(t)) return t.find((x) => x !== 'null')
  return t
}

/** How a property is rendered. `depth` is 0 for top-level properties. */
export function fieldKind(schema: JSONSchema, depth = 0): FieldKind {
  if (Array.isArray(schema.enum) && schema.enum.length > 0 && schema.enum.every((v) => typeof v === 'string' || typeof v === 'number')) {
    return 'enum'
  }
  switch (typeOf(schema)) {
    case 'string': return 'string'
    case 'number': return 'number'
    case 'integer': return 'integer'
    case 'boolean': return 'boolean'
    case 'array':
      return schema.items && typeOf(schema.items) === 'string' && !schema.items.enum ? 'stringArray' : 'json'
    case 'object':
      return depth === 0 && schema.properties && Object.keys(schema.properties).length > 0 ? 'object' : 'json'
    default:
      return 'json'
  }
}

/** True when the root schema can be shown as a form (an object with properties). */
export function isFormSchema(schema: JSONSchema | undefined | null): schema is JSONSchema {
  return !!schema && typeOf(schema) === 'object' && !!schema.properties && Object.keys(schema.properties).length > 0
}

/** The agent's default shape: a single free-text `input` field. */
export function isPlainTextSchema(schema: JSONSchema | undefined | null): boolean {
  if (!isFormSchema(schema)) return false
  const keys = Object.keys(schema.properties ?? {})
  return keys.length === 1 && keys[0] === 'input' && fieldKind(schema.properties!.input) === 'string'
}

function initialFor(schema: JSONSchema, depth: number): FieldValue {
  const kind = fieldKind(schema, depth)
  const d = schema.default
  switch (kind) {
    case 'boolean': return d === true
    case 'object': return initialObject(schema, depth + 1)
    case 'stringArray': return Array.isArray(d) ? d.join('\n') : ''
    case 'json': return d === undefined ? '' : JSON.stringify(d, null, 2)
    default: return d === undefined || d === null ? '' : String(d)
  }
}

function initialObject(schema: JSONSchema, depth: number): FormValues {
  const out: FormValues = {}
  for (const [k, p] of Object.entries(schema.properties ?? {})) out[k] = initialFor(p, depth)
  return out
}

export function initialValues(schema: JSONSchema): FormValues {
  return initialObject(schema, 0)
}

export type FormErrors = Record<string, string>

function label(key: string, schema: JSONSchema): string {
  return schema.title || key
}

function lines(text: string): string[] {
  return text.split('\n').map((l) => l.trim()).filter((l) => l !== '')
}

interface Converted { value: unknown; empty: boolean }

function convert(path: string, key: string, schema: JSONSchema, raw: FieldValue | undefined, required: boolean, depth: number, errors: FormErrors): Converted {
  const kind = fieldKind(schema, depth)
  const name = label(key, schema)
  const need = (msg: string) => { errors[path] = msg }
  switch (kind) {
    case 'boolean':
      // An unticked box is only sent when it means something: required, or the schema gives a default.
      if (raw === true) return { value: true, empty: false }
      return required || schema.default !== undefined ? { value: false, empty: false } : { value: undefined, empty: true }
    case 'string': {
      const text = typeof raw === 'string' ? raw : ''
      if (text.trim() === '') {
        if (required) need(`${name} is required.`)
        return { value: undefined, empty: true }
      }
      if (schema.minLength !== undefined && text.length < schema.minLength) need(`${name} must be at least ${schema.minLength} characters.`)
      if (schema.maxLength !== undefined && text.length > schema.maxLength) need(`${name} must be at most ${schema.maxLength} characters.`)
      return { value: text, empty: false }
    }
    case 'number':
    case 'integer': {
      const text = typeof raw === 'string' ? raw.trim() : ''
      if (text === '') {
        if (required) need(`${name} is required.`)
        return { value: undefined, empty: true }
      }
      const n = Number(text)
      if (!Number.isFinite(n)) { need(`${name} must be a number.`); return { value: undefined, empty: false } }
      if (kind === 'integer' && !Number.isInteger(n)) { need(`${name} must be a whole number.`); return { value: undefined, empty: false } }
      if (schema.minimum !== undefined && n < schema.minimum) need(`${name} must be at least ${schema.minimum}.`)
      if (schema.maximum !== undefined && n > schema.maximum) need(`${name} must be at most ${schema.maximum}.`)
      return { value: n, empty: false }
    }
    case 'enum': {
      const text = typeof raw === 'string' ? raw : ''
      if (text === '') {
        if (required) need(`Choose a value for ${name}.`)
        return { value: undefined, empty: true }
      }
      const match = (schema.enum ?? []).find((v) => String(v) === text)
      if (match === undefined) { need(`${name} must be one of the listed values.`); return { value: undefined, empty: false } }
      return { value: match, empty: false }
    }
    case 'stringArray': {
      const items = lines(typeof raw === 'string' ? raw : '')
      if (items.length === 0) {
        if (required) need(`${name} needs at least one entry.`)
        return { value: undefined, empty: true }
      }
      return { value: items, empty: false }
    }
    case 'object': {
      const obj = convertObject(path, schema, (raw && typeof raw === 'object' ? raw : {}) as FormValues, depth + 1, errors)
      const empty = Object.keys(obj).length === 0
      if (empty && required) need(`${name} is required.`)
      return { value: empty ? undefined : obj, empty }
    }
    default: {
      const text = typeof raw === 'string' ? raw.trim() : ''
      if (text === '') {
        if (required) need(`${name} is required.`)
        return { value: undefined, empty: true }
      }
      try {
        return { value: JSON.parse(text), empty: false }
      } catch {
        need(`${name} is not valid JSON.`)
        return { value: undefined, empty: false }
      }
    }
  }
}

function convertObject(path: string, schema: JSONSchema, values: FormValues, depth: number, errors: FormErrors): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  const required = new Set(schema.required ?? [])
  for (const [k, p] of Object.entries(schema.properties ?? {})) {
    const c = convert(path ? `${path}.${k}` : k, k, p, values[k], required.has(k), depth, errors)
    if (!c.empty && c.value !== undefined) out[k] = c.value
  }
  return out
}

export interface FormResult {
  /** The input to send, or null when there are errors. */
  payload: Record<string, unknown> | null
  errors: FormErrors
}

/** Validates the form and builds the JSON input from it. */
export function buildPayload(schema: JSONSchema, values: FormValues): FormResult {
  const errors: FormErrors = {}
  const payload = convertObject('', schema, values, 0, errors)
  return { payload: Object.keys(errors).length === 0 ? payload : null, errors }
}

/** Validates raw JSON typed into the fallback editor against the root schema. */
export function parseRawInput(schema: JSONSchema | undefined | null, text: string): { payload: unknown; error?: string } {
  const t = text.trim()
  if (t === '') return { payload: null, error: 'Enter the input to send.' }
  let parsed: unknown
  try {
    parsed = JSON.parse(t)
  } catch {
    return { payload: null, error: 'This is not valid JSON.' }
  }
  if (isFormSchema(schema)) {
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
      return { payload: null, error: 'The input must be a JSON object.' }
    }
    const missing = (schema.required ?? []).filter((k) => (parsed as Record<string, unknown>)[k] === undefined)
    if (missing.length > 0) return { payload: null, error: `Missing required: ${missing.join(', ')}.` }
  }
  return { payload: parsed }
}

/** The text sent to the agent for a form payload: plain text for the default
 *  one-field schema, otherwise the JSON. */
export function payloadToMessage(schema: JSONSchema | undefined | null, payload: unknown): string {
  if (isPlainTextSchema(schema) && payload && typeof payload === 'object') {
    const v = (payload as Record<string, unknown>).input
    if (typeof v === 'string') return v
  }
  return typeof payload === 'string' ? payload : JSON.stringify(payload, null, 2)
}
