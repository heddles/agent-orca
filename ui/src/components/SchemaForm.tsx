/**
 * SchemaForm — renders a run form from a JSON Schema (see lib/schemaForm.ts for
 * what is supported). Fields are labelled, descriptions become help text linked
 * with aria-describedby, and errors are announced next to the field.
 */
import type { CSSProperties } from 'react'
import { ds, DESIGN } from '../lib/designSystem'
import {
  fieldKind, type FieldValue, type FormErrors, type FormValues, type JSONSchema,
} from '../lib/schemaForm'

interface Props {
  schema: JSONSchema
  values: FormValues
  onChange: (values: FormValues) => void
  errors: FormErrors
  /** Unique prefix so ids stay unique when several forms are on a page. */
  idPrefix?: string
}

export function SchemaForm({ schema, values, onChange, errors, idPrefix = 'run' }: Props) {
  return (
    <ObjectFields
      schema={schema}
      values={values}
      onChange={onChange}
      errors={errors}
      path=""
      depth={0}
      idPrefix={idPrefix}
    />
  )
}

interface ObjectProps extends Props {
  path: string
  depth: number
  idPrefix: string
}

function ObjectFields({ schema, values, onChange, errors, path, depth, idPrefix }: ObjectProps) {
  const required = new Set(schema.required ?? [])
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      {Object.entries(schema.properties ?? {}).map(([key, prop]) => {
        const fieldPath = path ? `${path}.${key}` : key
        const id = `${idPrefix}-${fieldPath.replace(/[^a-zA-Z0-9_-]/g, '_')}`
        const set = (v: FieldValue) => onChange({ ...values, [key]: v })
        const kind = fieldKind(prop, depth)
        const title = prop.title || key
        const isRequired = required.has(key)
        const error = errors[fieldPath]
        const helpId = prop.description ? `${id}-help` : undefined
        const errId = error ? `${id}-error` : undefined
        const hintId = kind === 'json' || kind === 'stringArray' ? `${id}-hint` : undefined
        const describedBy = [helpId, hintId, errId].filter(Boolean).join(' ') || undefined
        const common = { id, 'aria-describedby': describedBy, 'aria-invalid': error ? true : undefined, 'aria-required': isRequired || undefined }

        if (kind === 'object') {
          return (
            <fieldset key={key} style={s.fieldset}>
              <legend style={s.legend}>{title}{isRequired && <span style={s.req}> (required)</span>}</legend>
              {prop.description && <p id={helpId} style={s.help}>{prop.description}</p>}
              <ObjectFields
                schema={prop}
                values={(values[key] as FormValues) ?? {}}
                onChange={(v) => set(v)}
                errors={errors}
                path={fieldPath}
                depth={depth + 1}
                idPrefix={idPrefix}
              />
              {error && <p id={errId} role="alert" style={s.error}>{error}</p>}
            </fieldset>
          )
        }

        if (kind === 'boolean') {
          return (
            <div key={key} style={s.field}>
              <label htmlFor={id} style={{ ...s.label, flexDirection: 'row', alignItems: 'center', gap: 8, textTransform: 'none', fontSize: 13 }}>
                <input
                  {...common}
                  type="checkbox"
                  checked={values[key] === true}
                  onChange={(e) => set(e.target.checked)}
                />
                <span>{title}{isRequired && <span style={s.req}> (required)</span>}</span>
              </label>
              {prop.description && <p id={helpId} style={s.help}>{prop.description}</p>}
            </div>
          )
        }

        return (
          <div key={key} style={s.field}>
            <label htmlFor={id} style={s.label}>
              {title}{isRequired && <span style={s.req}> (required)</span>}
            </label>
            {prop.description && <p id={helpId} style={s.help}>{prop.description}</p>}
            {kind === 'enum' && (
              <select {...common} style={s.input} value={(values[key] as string) ?? ''} onChange={(e) => set(e.target.value)}>
                <option value="">{isRequired ? 'Choose one' : 'Not set'}</option>
                {(prop.enum ?? []).map((v) => <option key={String(v)} value={String(v)}>{String(v)}</option>)}
              </select>
            )}
            {(kind === 'number' || kind === 'integer') && (
              <input
                {...common}
                style={s.input}
                type="number"
                inputMode="decimal"
                step={kind === 'integer' ? 1 : 'any'}
                min={prop.minimum}
                max={prop.maximum}
                value={(values[key] as string) ?? ''}
                onChange={(e) => set(e.target.value)}
              />
            )}
            {kind === 'string' && (
              prop.maxLength === undefined || prop.maxLength > 120 ? (
                <textarea
                  {...common}
                  style={{ ...s.input, minHeight: depth === 0 && key === 'input' ? 120 : 72, resize: 'vertical' }}
                  value={(values[key] as string) ?? ''}
                  onChange={(e) => set(e.target.value)}
                />
              ) : (
                <input {...common} style={s.input} type="text" value={(values[key] as string) ?? ''} onChange={(e) => set(e.target.value)} />
              )
            )}
            {kind === 'stringArray' && (
              <>
                <textarea
                  {...common}
                  style={{ ...s.input, minHeight: 72, resize: 'vertical' }}
                  value={(values[key] as string) ?? ''}
                  onChange={(e) => set(e.target.value)}
                />
                <p id={hintId} style={s.hint}>One entry per line.</p>
              </>
            )}
            {kind === 'json' && (
              <>
                <textarea
                  {...common}
                  style={{ ...s.input, minHeight: 88, resize: 'vertical', fontFamily: DESIGN.font.mono }}
                  spellCheck={false}
                  value={(values[key] as string) ?? ''}
                  onChange={(e) => set(e.target.value)}
                />
                <p id={hintId} style={s.hint}>This field has a shape the form cannot show, so enter it as JSON.</p>
              </>
            )}
            {error && <p id={errId} role="alert" style={s.error}>{error}</p>}
          </div>
        )
      })}
    </div>
  )
}

const s: Record<string, CSSProperties> = {
  field: { display: 'flex', flexDirection: 'column', gap: 4 },
  label: {
    display: 'flex', flexDirection: 'column', fontSize: 12, fontWeight: 600, color: ds.textPrimary,
  },
  req: { fontWeight: 400, color: ds.textMuted },
  help: { margin: 0, fontSize: 12, color: ds.textSecondary, lineHeight: 1.4 },
  hint: { margin: 0, fontSize: 11, color: ds.textMuted },
  error: { margin: 0, fontSize: 12, color: ds.error },
  input: {
    background: ds.surface, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md,
    padding: '8px 10px', color: ds.textPrimary, fontSize: 14, fontFamily: DESIGN.font.family,
    width: '100%', boxSizing: 'border-box',
  },
  fieldset: { border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, padding: '10px 12px', margin: 0 },
  legend: { fontSize: 12, fontWeight: 600, color: ds.textPrimary, padding: '0 6px' },
}
