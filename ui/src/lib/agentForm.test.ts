import { describe, expect, it } from 'vitest'
import { emptyForm, formFromObject, nameProblem, objectFromForm, validateForm } from './agentForm'
import { parseYAML, stringifyYAML } from './yaml'

describe('agent form model', () => {
  it('round-trips and keeps fields the form does not show', () => {
    const obj = {
      metadata: { name: 'a', namespace: 'n', resourceVersion: '3' },
      spec: { modelSelectorRef: 'fast', systemPrompt: 'P', tools: ['t'], resources: { limits: { cpu: '1' } }, runtime: { ociRef: 'img' } },
    }
    const form = formFromObject(obj)
    expect(form).toMatchObject({ name: 'a', modelSelector: 'fast', instructions: 'P', connections: ['t'] })
    const back = objectFromForm(obj, { ...form, instructions: 'Q', modelSelector: 'default' })
    expect(back.spec).toMatchObject({ systemPrompt: 'Q', modelSelectorRef: 'default', resources: { limits: { cpu: '1' } }, runtime: { ociRef: 'img' } })
    expect(back.metadata.resourceVersion).toBe('3')
  })

  it('writes knowledge into knowledgeBaseRefs when the agent already uses them', () => {
    const obj = { metadata: { name: 'a' }, spec: { modelSelectorRef: 'default', knowledgeBaseRefs: [{ name: 'k1', confirmRequired: true }] } }
    const form = formFromObject(obj)
    expect(form.knowledgeBases).toEqual(['k1'])
    const back = objectFromForm(obj, { ...form, knowledgeBases: ['k1', 'k2'] })
    expect(back.spec?.knowledgeBaseRefs).toEqual([{ name: 'k1', confirmRequired: true }, { name: 'k2' }])
  })

  it('validates required fields and names', () => {
    expect(Object.keys(validateForm(emptyForm()))).toEqual(['name', 'description', 'instructions', 'modelSelector'])
    expect(nameProblem('Bad_Name')).not.toBe('')
    expect(nameProblem('good-name-1')).toBe('')
  })

  it('reads and writes YAML', () => {
    expect(parseYAML('a: [1, 2]\nb: x').value).toEqual({ a: [1, 2], b: 'x' })
    expect(parseYAML('a: [').error).toBeTruthy()
    expect(parseYAML(stringifyYAML({ x: { y: ['a', 'b'] } })).value).toEqual({ x: { y: ['a', 'b'] } })
  })
})
