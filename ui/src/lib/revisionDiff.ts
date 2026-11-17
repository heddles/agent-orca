/** What changed between two versions of an agent, field by field, using the form's plain labels. */
import { formFromObject, type AgentForm } from './agentForm'
import { stringifyYAML } from './yaml'
import type { KubeObject } from '../api/builder'

export interface FieldChange {
  key: keyof AgentForm
  label: string
  before: string[]
  after: string[]
  changed: boolean
}

/** The form's fields in the order the form shows them, with the form's labels. */
export const FIELDS: Array<{ key: keyof AgentForm; label: string }> = [
  { key: 'description', label: 'Description' },
  { key: 'maintainer', label: 'Maintainer' },
  { key: 'exampleInputs', label: 'Example inputs' },
  { key: 'instructions', label: 'Instructions' },
  { key: 'modelSelector', label: 'Model' },
  { key: 'connections', label: 'Connections' },
  { key: 'knowledgeBases', label: 'Knowledge' },
  { key: 'guardrail', label: 'Safety policy' },
]

/** A field's value as a list: lists as they are, example inputs per line, text as one item. */
export function fieldValues(f: AgentForm, key: keyof AgentForm): string[] {
  const v = f[key]
  if (Array.isArray(v)) return v
  if (!v) return []
  return key === 'exampleInputs' ? v.split('\n') : [v]
}

/** Every form field with its old and new values; `changed` marks the ones that differ. */
export function agentChanges(before: KubeObject, after: KubeObject): FieldChange[] {
  const a = formFromObject(before)
  const b = formFromObject(after)
  return FIELDS.map(({ key, label }) => {
    const bv = fieldValues(a, key)
    const av = fieldValues(b, key)
    return { key, label, before: bv, after: av, changed: JSON.stringify(bv) !== JSON.stringify(av) }
  })
}

/** Drops what the server owns, so two versions compare only on what a person wrote. */
export function comparable(obj: KubeObject): KubeObject {
  const copy = JSON.parse(JSON.stringify(obj)) as KubeObject
  delete copy.status
  delete copy.ownership
  if (copy.metadata) {
    for (const k of ['managedFields', 'resourceVersion', 'uid', 'generation', 'creationTimestamp']) delete copy.metadata[k]
  }
  return copy
}

export function yamlOf(obj: KubeObject): string {
  return stringifyYAML(comparable(obj))
}
