/**
 * AgentBuilder: a guided page to create or change an agent without YAML.
 * The form and the YAML view edit the same object. Agents managed in Git open
 * read-only, with Copy YAML and Duplicate.
 */
import { useCallback, useEffect, useMemo, useRef, useState, type CSSProperties } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router'
import {
  ApiError, cancelHandoff, createResource, dryRunResource, getBuilderOptions, getResource, publishAgent,
  unpublishAgent, updateResource, type BuilderOptions, type KubeObject, type OptionItem,
} from '../api/builder'
import { getCatalog } from '../api/catalog'
import {
  AUTO_MODEL_SELECTOR, autoAvailable, emptyForm, fieldForServerError, formFromObject, nameProblem, objectFromForm, validateForm,
  type AgentForm, type FormErrors,
} from '../lib/agentForm'
import { DESIGN, ds } from '../lib/designSystem'
import { EmptyState, Page, PageHeader } from '../lib/layout'
import { DSButton, DSChip } from '../lib/primitives'
import { ICON } from '../lib/icons'
import { parseYAML, stringifyYAML } from '../lib/yaml'
import { publishedAgentPath, ROUTES, buildPath } from '../lib/routes'
import { RevisionHistory } from './RevisionHistory'
import { BuilderTestPanel } from './BuilderTestPanel'
import { DuplicateDialog, ExportDialog, Field, field, useWorkspaces } from './BuilderParts'

const KIND = 'agents'

type DryState = { state: 'idle' | 'checking' | 'ok' | 'unavailable'; errors: string[] } | { state: 'invalid'; errors: string[] }

/** Drops what the server owns, so the form and the YAML view only hold what the user can write. */
function clean(obj: KubeObject): KubeObject {
  const copy = JSON.parse(JSON.stringify(obj)) as KubeObject
  delete copy.ownership
  delete copy.status
  if (copy.metadata) {
    delete copy.metadata.managedFields
    delete copy.metadata.uid
    delete copy.metadata.creationTimestamp
    delete copy.metadata.generation
  }
  return copy
}

function messageOf(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

export function ownershipBanner(own: KubeObject['ownership']): string | null {
  if (!own) return null
  if (own.owner === 'git') {
    return `Managed in Git${own.tool ? ` by ${own.tool}` : ''}${own.app ? ` app ${own.app}` : ''}. Changes are made there, so this page is read-only.`
  }
  if (own.owner === 'unknown') {
    return 'This agent was not created in this UI, and nothing shows who manages it, so this page is read-only.'
  }
  if (!own.editable) return 'You can look at this agent but not change it.'
  return null
}

interface Props {
  /** Absent for a new agent. */
  namespace?: string
  name?: string
}

export function AgentBuilder({ namespace, name }: Props) {
  const isNew = !name
  const navigate = useNavigate()
  const { workspaces, primary, loaded: workspacesLoaded, roles } = useWorkspaces()
  const [params, setParams] = useSearchParams()
  const tab: 'edit' | 'history' = params.get('tab') === 'history' && !isNew ? 'history' : 'edit'
  const setTab = (t: 'edit' | 'history') => setParams((p) => { const n = new URLSearchParams(p); if (t === 'history') n.set('tab', 'history'); else n.delete('tab'); return n }, { replace: true })
  const [historyKey, setHistoryKey] = useState(0)
  const [latestVersion, setLatestVersion] = useState<number | null>(null)

  const [loading, setLoading] = useState(!isNew)
  const [loadError, setLoadError] = useState('')
  const [saved, setSaved] = useState<KubeObject | null>(null)
  const [base, setBase] = useState<KubeObject | null>(null)
  const [form, setForm] = useState<AgentForm>(() => emptyForm(namespace ?? ''))
  const [initialJSON, setInitialJSON] = useState('')
  const [options, setOptions] = useState<BuilderOptions | null>(null)
  const [optionsError, setOptionsError] = useState('')

  const [mode, setMode] = useState<'form' | 'yaml'>('form')
  const [yamlText, setYamlText] = useState('')
  const [yamlError, setYamlError] = useState('')

  const [attempted, setAttempted] = useState(false)
  const [dry, setDry] = useState<DryState>({ state: 'idle', errors: [] })
  const [saving, setSaving] = useState(false)
  const [saveErrors, setSaveErrors] = useState<string[]>([])
  const [conflict, setConflict] = useState(false)
  const [notice, setNotice] = useState('')
  const [version, setVersion] = useState('')

  const [published, setPublished] = useState<boolean | null>(null)
  const [chat, setChat] = useState(true)
  const [publishBusy, setPublishBusy] = useState(false)
  const [publishMsg, setPublishMsg] = useState<{ ok: boolean; text: string; deployment?: string } | null>(null)
  const [dialog, setDialog] = useState<'export' | 'duplicate' | null>(null)
  const [handoffMsg, setHandoffMsg] = useState('')
  const [copied, setCopied] = useState(false)

  const seq = useRef(0)

  // The object the form describes, with every field the form does not show kept.
  const current = useMemo(() => objectFromForm(base, form), [base, form])
  const currentJSON = JSON.stringify(current)
  const dirty = !loading && initialJSON !== '' && currentJSON !== initialJSON
  const ownership = saved?.ownership
  const handoffPending = !!ownership?.handoff?.pending
  const readOnly = !isNew && !!ownership && (ownership.owner !== 'ui' || !ownership.editable)
  const banner = readOnly ? ownershipBanner(ownership) : null
  const noModel = !!options && options.modelSelectors.length === 0 && !form.modelSelector
  const clientErrors: FormErrors = useMemo(() => validateForm(form, { needsNamespace: isNew }), [form, isNew])
  const [serverFieldErrors, setServerFieldErrors] = useState<FormErrors>({})
  const showErr = (k: keyof AgentForm) => (attempted ? clientErrors[k] : undefined) ?? serverFieldErrors[k]

  const adopt = useCallback((obj: KubeObject) => {
    const c = clean(obj)
    const f = formFromObject(c)
    setSaved(obj)
    setBase(c)
    setForm(f)
    setInitialJSON(JSON.stringify(objectFromForm(c, f)))
    setYamlText(stringifyYAML(objectFromForm(c, f)))
    setYamlError('')
    setVersion(obj.metadata?.resourceVersion ?? String(Date.now()))
  }, [])

  // Load the agent and the option lists.
  useEffect(() => {
    let live = true
    if (!isNew) {
      getResource(KIND, namespace ?? '', name ?? '')
        .then((obj) => { if (live) { adopt(obj); setLoading(false) } })
        .catch((e: unknown) => { if (live) { setLoadError(messageOf(e)); setLoading(false) } })
      getCatalog(namespace ?? '')
        .then((list) => { if (live) setPublished(list.find((a) => a.name === name && a.namespace === namespace)?.published ?? false) })
        .catch(() => {})
    } else {
      setInitialJSON(JSON.stringify(objectFromForm(null, emptyForm(namespace ?? ''))))
    }
    getBuilderOptions()
      .then((o) => { if (live) setOptions(o) })
      .catch((e: unknown) => { if (live) setOptionsError(messageOf(e)) })
    return () => { live = false }
  }, [isNew, namespace, name, adopt])

  // A new agent goes in the caller's own workspace unless they pick another.
  useEffect(() => {
    if (isNew && !form.namespace && primary) {
      setForm((f) => ({ ...f, namespace: primary }))
      setInitialJSON(JSON.stringify(objectFromForm(null, { ...emptyForm(primary) })))
    }
  }, [isNew, primary, form.namespace])

  // Warn before the page is closed, or a link inside the app is followed, with unsaved changes.
  // (The app uses a plain browser router, so useBlocker is not available; a capturing click handler does the same for links.)
  useEffect(() => {
    if (!dirty) return
    const handler = (e: BeforeUnloadEvent) => { e.preventDefault() }
    const onClick = (e: MouseEvent) => {
      if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
      const a = (e.target as Element | null)?.closest?.('a[href]') as HTMLAnchorElement | null
      if (!a || (a.target && a.target !== '_self') || a.origin !== window.location.origin) return
      if (a.pathname === window.location.pathname && a.search === window.location.search) return
      if (!window.confirm('You have unsaved changes. Leave without saving?')) { e.preventDefault(); e.stopPropagation() }
    }
    window.addEventListener('beforeunload', handler)
    document.addEventListener('click', onClick, true)
    return () => {
      window.removeEventListener('beforeunload', handler)
      document.removeEventListener('click', onClick, true)
    }
  }, [dirty])

  // The model is chosen against the workspace the agent will be saved in: Auto is offered only where
  // a `default` selector exists there. A new agent starts on Auto when it can, and drops it when the
  // workspace changes to one without it. An existing agent's reference is never rewritten.
  const hasAuto = autoAvailable(options?.modelSelectors, form.namespace)
  useEffect(() => {
    if (!isNew || !options) return
    const next = hasAuto && !form.modelSelector ? AUTO_MODEL_SELECTOR : !hasAuto && form.modelSelector === AUTO_MODEL_SELECTOR ? '' : null
    if (next === null) return
    const pristine = currentJSON === initialJSON
    setForm((f) => ({ ...f, modelSelector: next }))
    if (pristine) setInitialJSON(JSON.stringify(objectFromForm(null, { ...emptyForm(form.namespace), modelSelector: next })))
    // Only a change of what is available re-resolves the choice.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isNew, options, hasAuto, form.namespace])

  // Ask the platform whether the draft would be accepted, after a short pause.
  useEffect(() => {
    if (readOnly || loading || yamlError) return
    const id = ++seq.current
    if (nameProblem(form.name) || !form.namespace || (!isNew && !dirty)) {
      setDry({ state: 'idle', errors: [] })
      setServerFieldErrors({})
      return
    }
    setDry({ state: 'checking', errors: [] })
    const timer = setTimeout(() => {
      dryRunResource(KIND, current, isNew ? null : { namespace: form.namespace, name: form.name })
        .then((r) => {
          if (id !== seq.current) return
          setDry(r.valid ? { state: 'ok', errors: [] } : { state: 'invalid', errors: r.errors })
          const byField: FormErrors = {}
          if (!r.valid) for (const m of r.errors) { const k = fieldForServerError(m); if (k && !byField[k]) byField[k] = m }
          setServerFieldErrors(byField)
        })
        .catch(() => { if (id === seq.current) setDry({ state: 'unavailable', errors: [] }) })
    }, 500)
    return () => clearTimeout(timer)
    // `current` follows currentJSON; the string keeps the effect from re-running on identity changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [currentJSON, readOnly, loading, yamlError])

  const update = (patch: Partial<AgentForm>) => setForm((f) => ({ ...f, ...patch }))
  const toggle = (key: 'connections' | 'knowledgeBases', value: string) =>
    setForm((f) => ({ ...f, [key]: f[key].includes(value) ? f[key].filter((v) => v !== value) : [...f[key], value] }))

  // ── YAML view ──
  const showYAML = () => {
    setYamlText(stringifyYAML(current))
    setYamlError('')
    setMode('yaml')
  }
  const onYAMLChange = (text: string) => {
    setYamlText(text)
    const parsed = parseYAML(text)
    if (parsed.error !== undefined) { setYamlError(parsed.error); return }
    const v = parsed.value as KubeObject | null
    if (!v || typeof v !== 'object' || Array.isArray(v)) { setYamlError('This should describe one agent: a list of fields such as apiVersion, kind, metadata and spec.'); return }
    if (v.kind && v.kind !== 'Agent') { setYamlError(`This view edits an Agent, but kind is ${String(v.kind)}.`); return }
    setYamlError('')
    const c = clean(v)
    setBase(c)
    setForm(formFromObject(c))
  }

  // ── Save ──
  const reload = async () => {
    try {
      const obj = await getResource(KIND, form.namespace, form.name)
      adopt(obj)
      setConflict(false)
      setSaveErrors([])
      setNotice('Reloaded the latest version.')
    } catch (e) {
      setSaveErrors([`Could not reload: ${messageOf(e)}`])
    }
  }

  // Restore: the history tab asks for the live version again after someone else changed the agent.
  const reloadLive = async () => {
    const obj = await getResource(KIND, form.namespace, form.name)
    adopt(obj)
    setConflict(false)
    setHistoryKey((k) => k + 1)
  }
  const canRestore = roles.some((r) => r === 'builder' || r === 'admin') && !readOnly && !handoffPending
  const historyNote = readOnly
    ? (ownership?.owner === 'ui' ? 'You can look at the history but not restore a version.' : 'This agent is not managed here, so its history is read-only and no version can be restored.')
    : handoffPending ? 'This agent is being handed over to Git, so versions cannot be restored until the handoff ends.'
      : !canRestore ? 'Restoring a version needs the builder role.' : ''

  const save = async () => {
    setAttempted(true)
    setNotice('')
    setSaveErrors([])
    if (yamlError) { setSaveErrors([`Fix the YAML first: ${yamlError}`]); return }
    if (Object.keys(clientErrors).length > 0) {
      setSaveErrors(['Some details are missing. They are marked in the form.'])
      return
    }
    setSaving(true)
    try {
      let result: KubeObject
      if (isNew) {
        result = await createResource(KIND, current)
      } else {
        try {
          result = await updateResource(KIND, form.namespace, form.name, current)
        } catch (e) {
          if (!(e instanceof ApiError) || e.status !== 428) throw e
          // The write needs the version it replaces: read it once and try again.
          const latest = await getResource(KIND, form.namespace, form.name)
          const retry = { ...current, metadata: { ...current.metadata, resourceVersion: latest.metadata.resourceVersion } }
          result = await updateResource(KIND, form.namespace, form.name, retry)
        }
      }
      setConflict(false)
      if (isNew) {
        const meta = result.metadata ?? current.metadata
        navigate(buildPath(meta.namespace ?? form.namespace, meta.name ?? form.name), { replace: true })
        return
      }
      adopt(result)
      setHistoryKey((k) => k + 1)
      setNotice('Saved.')
    } catch (e) {
      if (e instanceof ApiError && e.status === 409 && !isNew) setConflict(true)
      else if (e instanceof ApiError && e.status === 409) setSaveErrors([`An agent called ${form.name} already exists in this workspace. Choose another name.`])
      else if (e instanceof ApiError && e.errors.length > 0) setSaveErrors(e.errors)
      else setSaveErrors([messageOf(e)])
    } finally {
      setSaving(false)
    }
  }

  // ── Publish ──
  const publish = async () => {
    setPublishBusy(true)
    setPublishMsg(null)
    try {
      const r = await publishAgent(form.namespace, form.name, { chat })
      setPublished(true)
      setHistoryKey((k) => k + 1)
      setPublishMsg({ ok: true, text: 'Published. People can now use this agent.', deployment: r.deployment.metadata?.name })
    } catch (e) {
      setPublishMsg({ ok: false, text: `Could not publish: ${messageOf(e)}` })
    } finally {
      setPublishBusy(false)
    }
  }
  const unpublish = async () => {
    setPublishBusy(true)
    setPublishMsg(null)
    try {
      await unpublishAgent(form.namespace, form.name)
      setPublished(false)
      setPublishMsg({ ok: true, text: 'Unpublished. The agent is no longer offered to people, and your draft is kept.' })
    } catch (e) {
      setPublishMsg({ ok: false, text: `Could not unpublish: ${messageOf(e)}` })
    } finally {
      setPublishBusy(false)
    }
  }

  const cancelPendingHandoff = async () => {
    setHandoffMsg('')
    try {
      await cancelHandoff(KIND, form.namespace, form.name)
      const obj = await getResource(KIND, form.namespace, form.name)
      setSaved(obj)
      setHandoffMsg('The handoff to Git is cancelled. This agent stays managed here.')
    } catch (e) {
      setHandoffMsg(`Could not cancel the handoff: ${messageOf(e)}`)
    }
  }

  const copyYAML = () => {
    const text = stringifyYAML(mode === 'yaml' ? (parseYAML(yamlText).value ?? current) : current)
    navigator.clipboard.writeText(text).then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500) }).catch(() => {})
  }

  if (loading) return <div role="status" style={{ padding: 24, color: ds.textMuted, fontSize: 13 }}>Loading agent…</div>
  if (loadError) {
    return (
      <Page>
        <EmptyState icon={ICON.agent} title="This agent could not be opened" action={<Link to={ROUTES.build} style={{ color: ds.accent }}>Back to Build</Link>}>
          It may not exist, or it belongs to another team. ({loadError})
        </EmptyState>
      </Page>
    )
  }

  const handoff = ownership?.handoff
  // The selectors to choose from. Auto stands for `default` where it exists; a reference the
  // options do not list (for example one in another namespace) is shown as written.
  const modelChoices = withCurrent(options?.modelSelectors, form.modelSelector).filter((m) => !(hasAuto && m.name === AUTO_MODEL_SELECTOR))
  const savedExists = !isNew && !!saved
  const dryText =
    dry.state === 'checking' ? 'Checking with the platform…'
      : dry.state === 'ok' ? 'The platform accepts this agent.'
        : dry.state === 'unavailable' ? 'Could not check with the platform right now. Saving will check again.'
          : ''

  return (
    <Page>
      <nav aria-label="Breadcrumb" style={{ padding: '10px 24px 0', fontSize: 12 }}>
        <Link to={ROUTES.build} style={{ color: ds.accent, textDecoration: 'none' }}>Build</Link>
        <span style={{ color: ds.textMuted }}> / </span>
        <span style={{ color: ds.textSecondary }}>{isNew ? 'New agent' : name}</span>
      </nav>
      <PageHeader
        title={isNew ? 'New agent' : (name ?? '')}
        description={isNew ? 'Describe what the agent should do. You can try it before anyone else uses it.' : 'Change what this agent does, then save and try it.'}
        actions={
          <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
            {latestVersion !== null && <DSChip size="sm" variant="neutral">Version {latestVersion}</DSChip>}
            {dirty && <DSChip size="sm" variant="warning" dot>Unsaved changes</DSChip>}
            {!readOnly && <DSButton type="button" variant="primary" onClick={save} disabled={saving || noModel} title={noModel ? 'Set up a model first' : undefined}>{saving ? 'Saving…' : 'Save'}</DSButton>}
          </div>
        }
      />

      <div style={s.body}>
        {banner && (
          <div role="status" style={{ ...s.banner, borderColor: ds.warning }}>
            <strong>{banner}</strong>
            <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
              <DSButton type="button" size="sm" onClick={copyYAML}>{copied ? 'Copied' : 'Copy YAML'}</DSButton>
              <DSButton type="button" size="sm" onClick={() => setDialog('duplicate')}>Duplicate</DSButton>
            </div>
          </div>
        )}
        {!readOnly && handoff?.pending && (
          <div role="status" style={{ ...s.banner, borderColor: ds.warning }}>
            <strong>Being handed over to Git.</strong>{' '}
            You can keep editing here until {handoff.expires ? new Date(handoff.expires).toLocaleString() : 'the handoff expires'}.
            <div style={{ marginTop: 8 }}>
              <DSButton type="button" size="sm" onClick={cancelPendingHandoff}>Cancel handoff</DSButton>
            </div>
          </div>
        )}
        {handoffMsg && <p role="status" style={field.help}>{handoffMsg}</p>}
        {conflict && (
          <div role="alert" style={{ ...s.banner, borderColor: ds.error }}>
            <strong>Someone changed this agent.</strong>{' '}
            Your edits are still here and have not been saved. Reload to get the latest version; that replaces what you have on screen.
            <div style={{ marginTop: 8 }}><DSButton type="button" size="sm" onClick={reload}>Reload</DSButton></div>
          </div>
        )}
        {saveErrors.length > 0 && (
          <div role="alert" style={{ ...s.banner, borderColor: ds.error }}>
            <strong>This could not be saved.</strong>
            <ul style={{ margin: '4px 0 0', paddingLeft: 18 }}>{saveErrors.map((m) => <li key={m}>{m}</li>)}</ul>
          </div>
        )}
        <div role="status" aria-live="polite" style={{ fontSize: 12, color: ds.textSecondary, minHeight: 16 }}>{notice}</div>

        {!isNew && (
          <div role="tablist" aria-label="Agent sections" style={{ display: 'flex', gap: 4, borderBottom: '1px solid var(--ds-border)' }}>
            {(['edit', 'history'] as const).map((t) => (
              <button
                key={t} type="button" role="tab" id={`tab-${t}`} aria-selected={tab === t} aria-controls={`panel-${t}`}
                tabIndex={tab === t ? 0 : -1} onClick={() => setTab(t)}
                onKeyDown={(e) => { if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') { e.preventDefault(); setTab(t === 'edit' ? 'history' : 'edit'); document.getElementById(`tab-${t === 'edit' ? 'history' : 'edit'}`)?.focus() } }}
                style={{ ...toggleBtn(false), padding: '8px 14px', fontSize: 13, background: 'transparent', color: tab === t ? ds.textPrimary : ds.textSecondary, borderBottom: `2px solid ${tab === t ? ds.accent : 'transparent'}`, fontWeight: tab === t ? 600 : 400 }}
              >
                {t === 'edit' ? 'Edit' : 'History'}
              </button>
            ))}
          </div>
        )}

        {!isNew && (
          <div role="tabpanel" id="panel-history" aria-labelledby="tab-history" hidden={tab !== 'history'}>
            <RevisionHistory
              kind={KIND} namespace={form.namespace} name={form.name} isAgent
              refreshKey={historyKey} live={saved} canRestore={canRestore} readOnlyNote={historyNote} dirty={dirty}
              onLoaded={setLatestVersion}
              onReload={reloadLive}
              onRestored={(obj, from) => {
                adopt(obj)
                setConflict(false)
                setSaveErrors([])
                setHistoryKey((k) => k + 1)
                setNotice(`Restored version ${from}. The form now shows it.`)
              }}
            />
          </div>
        )}

        <div style={tab === 'edit' ? s.columns : { display: 'none' }} role={isNew ? undefined : 'tabpanel'} id="panel-edit" aria-labelledby={isNew ? undefined : 'tab-edit'} hidden={tab !== 'edit'}>
          <div style={s.main}>
            <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
              <div role="group" aria-label="View" style={s.toggle}>
                <button type="button" aria-pressed={mode === 'form'} style={toggleBtn(mode === 'form')} disabled={mode === 'yaml' && !!yamlError} title={yamlError ? 'Fix the YAML first' : undefined} onClick={() => setMode('form')}>Form</button>
                <button type="button" aria-pressed={mode === 'yaml'} style={toggleBtn(mode === 'yaml')} onClick={showYAML}>YAML</button>
              </div>
              {!readOnly && savedExists && (
                <>
                  <DSButton type="button" size="sm" onClick={copyYAML}>{copied ? 'Copied' : 'Copy YAML'}</DSButton>
                  <DSButton type="button" size="sm" onClick={() => setDialog('export')} disabled={dirty} title={dirty ? 'Save first' : undefined}>Export to Git</DSButton>
                </>
              )}
            </div>

            {mode === 'yaml' ? (
              <div style={field.wrap}>
                <label htmlFor="agent-yaml" style={field.label}>Agent as YAML</label>
                <p style={field.help}>The same agent as the form shows. Changes here appear in the form, and the other way round.</p>
                <textarea
                  id="agent-yaml"
                  spellCheck={false}
                  readOnly={readOnly}
                  value={yamlText}
                  onChange={(e) => onYAMLChange(e.target.value)}
                  aria-invalid={yamlError ? true : undefined}
                  aria-describedby={yamlError ? 'agent-yaml-error' : undefined}
                  style={{ ...field.control, fontFamily: DESIGN.font.mono, minHeight: 460, resize: 'vertical' }}
                />
                {yamlError && <p id="agent-yaml-error" role="alert" style={field.error}>{yamlError}</p>}
              </div>
            ) : (
              <fieldset disabled={readOnly} style={s.fieldset}>
                <legend style={s.srOnly}>Agent details</legend>

                <section aria-labelledby="sec-what" style={s.section}>
                  <h2 id="sec-what" style={s.h2}>What it does</h2>
                  <Field label="Name" help="Lower-case letters, numbers and dashes. It cannot be changed after saving." error={showErr('name')}>
                    {(p) => <input {...p} style={field.control} value={form.name} disabled={!isNew || readOnly} onChange={(e) => update({ name: e.target.value })} />}
                  </Field>
                  {isNew && (workspacesLoaded && workspaces.length !== 1) && (
                    <Field label="Workspace" help="Where the agent lives." error={showErr('namespace')}>
                      {(p) => workspaces.length > 1 ? (
                        <select {...p} style={field.control} value={form.namespace} onChange={(e) => update({ namespace: e.target.value })}>
                          {workspaces.map((w) => <option key={w.namespace} value={w.namespace}>{w.name || w.namespace}</option>)}
                        </select>
                      ) : (
                        <input {...p} style={field.control} value={form.namespace} onChange={(e) => update({ namespace: e.target.value })} />
                      )}
                    </Field>
                  )}
                  <Field label="Description" help="One or two sentences, so people can tell whether this is the agent they need." error={showErr('description')}>
                    {(p) => <textarea {...p} rows={2} style={field.control} value={form.description} onChange={(e) => update({ description: e.target.value })} />}
                  </Field>
                  <Field label="Maintainer" help="Who people should ask about this agent.">
                    {(p) => <input {...p} style={field.control} value={form.maintainer} onChange={(e) => update({ maintainer: e.target.value })} />}
                  </Field>
                  <Field label="Example inputs" help="Things people might ask it, one per line. They are shown as starting points.">
                    {(p) => <textarea {...p} rows={3} style={field.control} value={form.exampleInputs} onChange={(e) => update({ exampleInputs: e.target.value })} />}
                  </Field>
                </section>

                <section aria-labelledby="sec-instructions" style={s.section}>
                  <h2 id="sec-instructions" style={s.h2}>Instructions</h2>
                  <Field
                    label="Instructions"
                    help="Say who the agent is, what it should do, how it should answer and what it must not do. Be specific, and describe what a good answer looks like."
                    error={showErr('instructions')}
                  >
                    {(p) => <textarea {...p} rows={12} style={field.control} value={form.instructions} onChange={(e) => update({ instructions: e.target.value })} />}
                  </Field>
                </section>

                <section aria-labelledby="sec-model" style={s.section}>
                  <h2 id="sec-model" style={s.h2}>Model</h2>
                  {noModel ? (
                    <p role="alert" style={field.error}>
                      No model is set up for this workspace yet. Ask an administrator to add one under Settings → Models.
                    </p>
                  ) : (
                    <Field
                      label="Model"
                      help={hasAuto ? 'Auto lets the platform pick the model that fits each request.' : 'Choose which model answers for this agent.'}
                      error={showErr('modelSelector')}
                    >
                      {(p) => (
                        <select {...p} style={field.control} value={form.modelSelector} onChange={(e) => update({ modelSelector: e.target.value })}>
                          {!hasAuto && <option value="">Choose a model…</option>}
                          {hasAuto && <option value={AUTO_MODEL_SELECTOR}>Auto (recommended)</option>}
                          {modelChoices.map((m) => <option key={m.name} value={m.name}>{m.description || m.name}</option>)}
                        </select>
                      )}
                    </Field>
                  )}
                </section>

                <section aria-labelledby="sec-connections" style={s.section}>
                  <h2 id="sec-connections" style={s.h2}>Connections</h2>
                  <p style={field.help}>What the agent can use to get things done. Pick only what it needs.</p>
                  {optionsError && <p role="alert" style={field.error}>The list of connections could not be loaded: {optionsError}</p>}
                  <CheckGroup legend="Tools" items={options?.tools} selected={form.connections} onToggle={(n) => toggle('connections', n)} empty="No tools are set up yet." />
                  <CheckGroup legend="Connected systems" items={options?.mcpServers} selected={form.connections} onToggle={(n) => toggle('connections', n)} empty="No connected systems are set up yet." />
                  {extraSelected(form.connections, options, ['tools', 'mcpServers']).length > 0 && (
                    <CheckGroup legend="Others already on this agent" items={extraSelected(form.connections, options, ['tools', 'mcpServers']).map((n) => ({ name: n, namespace: '' }))} selected={form.connections} onToggle={(n) => toggle('connections', n)} empty="" />
                  )}
                </section>

                <section aria-labelledby="sec-knowledge" style={s.section}>
                  <h2 id="sec-knowledge" style={s.h2}>Knowledge</h2>
                  <p style={field.help}>
                    Documents the agent can look things up in. Uploading documents is not available here yet. To add
                    documents, go to <Link to={ROUTES.knowledge} style={{ color: ds.accent }}>Settings → Knowledge</Link>.
                  </p>
                  <CheckGroup
                    legend="Knowledge to use"
                    items={withExtras(options?.knowledgeBases, form.knowledgeBases)}
                    selected={form.knowledgeBases}
                    onToggle={(n) => toggle('knowledgeBases', n)}
                    empty="No knowledge has been set up yet."
                  />
                </section>

                <section aria-labelledby="sec-safety" style={s.section}>
                  <h2 id="sec-safety" style={s.h2}>Safety</h2>
                  {(options?.guardrailPolicies?.length ?? 0) > 0 || form.guardrail ? (
                    <Field label="Safety policy" help="Rules that check what goes into and comes out of the agent." error={showErr('guardrail')}>
                      {(p) => (
                        <select {...p} style={field.control} value={form.guardrail} onChange={(e) => update({ guardrail: e.target.value })}>
                          <option value="">None</option>
                          {withCurrent(options?.guardrailPolicies, form.guardrail).map((g) => <option key={g.name} value={g.name}>{g.name}</option>)}
                        </select>
                      )}
                    </Field>
                  ) : (
                    <p style={field.help}>No safety policies have been set up in your workspace yet.</p>
                  )}
                </section>

                <section aria-labelledby="sec-use" style={s.section}>
                  <h2 id="sec-use" style={s.h2}>How people use it</h2>
                  <div role="group" aria-label="Ways to use this agent" style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
                    <label style={s.check}><input type="checkbox" checked disabled /> On demand: people ask it to do a task</label>
                    <label style={s.check}>
                      <input type="checkbox" checked={chat} onChange={(e) => setChat(e.target.checked)} />
                      Chat: people can talk with it (applies when you publish)
                    </label>
                    <label style={{ ...s.check, color: ds.textMuted }}>
                      <input type="checkbox" disabled aria-describedby="schedule-note" /> On a schedule
                    </label>
                    <span id="schedule-note" style={field.help}>Coming soon. Scheduled runs are not available yet.</span>
                  </div>
                </section>
              </fieldset>
            )}
          </div>

          <aside aria-label="Check and test" style={s.side}>
            <div role="status" aria-live="polite" style={s.check2}>
              {dryText && <p style={{ ...field.help, margin: 0 }}>{dryText}</p>}
              {dry.state === 'invalid' && (
                <div role="alert">
                  <strong style={{ fontSize: 12, color: ds.error }}>The platform would not accept this yet:</strong>
                  <ul style={{ margin: '4px 0 0', paddingLeft: 18, fontSize: 12, color: ds.error }}>{dry.errors.map((m) => <li key={m}>{m}</li>)}</ul>
                </div>
              )}
            </div>

            {!readOnly && (
              <section aria-labelledby="publish-heading" style={s.card}>
                <h2 id="publish-heading" style={{ ...s.h2, fontSize: 15 }}>Publish</h2>
                <p style={field.help}>Publishing offers the saved version of this agent to the people in your organisation.</p>
                {published && <DSChip size="sm" variant="success" dot>Published</DSChip>}
                <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                  <DSButton type="button" variant="primary" onClick={publish} disabled={!savedExists || dirty || publishBusy} title={!savedExists ? 'Save first' : dirty ? 'Save your changes first' : undefined}>
                    {published ? 'Publish changes' : 'Publish'}
                  </DSButton>
                  {published && <DSButton type="button" variant="secondary" onClick={unpublish} disabled={publishBusy}>Unpublish</DSButton>}
                </div>
                {(!savedExists || dirty) && <p style={field.help}>{!savedExists ? 'Save the agent before publishing.' : 'Save your changes before publishing.'}</p>}
                <div role="status" aria-live="polite">
                  {publishMsg && (
                    <p style={publishMsg.ok ? field.help : field.error}>
                      {publishMsg.text}{' '}
                      {publishMsg.deployment && <Link to={publishedAgentPath(form.namespace, publishMsg.deployment)} style={{ color: ds.accent }}>Open it</Link>}
                    </p>
                  )}
                </div>
              </section>
            )}

            <div style={s.card}>
              <BuilderTestPanel namespace={form.namespace} name={form.name} saved={savedExists} unsavedChanges={dirty} version={version} />
            </div>
          </aside>
        </div>
      </div>

      {dialog === 'export' && (
        <ExportDialog
          kind={KIND}
          namespace={form.namespace}
          name={form.name}
          onClose={() => setDialog(null)}
          onExported={() => { getResource(KIND, form.namespace, form.name).then((o) => setSaved(o)).catch(() => {}) }}
        />
      )}
      {dialog === 'duplicate' && <DuplicateDialog kind={KIND} namespace={form.namespace} name={form.name} onClose={() => setDialog(null)} />}
    </Page>
  )
}

// ── Helpers ──────────────────────────────────────────────────────────────────

/** The option list, plus the current value when the list does not have it (leaving out `auto`). */
function withCurrent(items: OptionItem[] | undefined, current: string, skip?: string): OptionItem[] {
  const list = (items ?? []).filter((i) => i.name !== skip || current === skip)
  if (current && !list.some((i) => i.name === current)) list.push({ name: current, namespace: '' })
  return list
}

function withExtras(items: OptionItem[] | undefined, selected: string[]): OptionItem[] {
  const list = [...(items ?? [])]
  for (const n of selected) if (!list.some((i) => i.name === n)) list.push({ name: n, namespace: '' })
  return list
}

/** Selected names that no option list knows about, so they are not silently lost. */
function extraSelected(selected: string[], options: BuilderOptions | null, keys: Array<'tools' | 'mcpServers'>): string[] {
  if (!options) return []
  const known = new Set(keys.flatMap((k) => options[k].map((i) => i.name)))
  return selected.filter((n) => !known.has(n))
}

function CheckGroup({ legend, items, selected, onToggle, empty }: {
  legend: string; items: OptionItem[] | undefined; selected: string[]; onToggle: (name: string) => void; empty: string
}) {
  return (
    <fieldset style={{ border: 'none', margin: 0, padding: 0, minInlineSize: 0 }}>
      <legend style={{ ...field.label, padding: 0, marginBottom: 6 }}>{legend}</legend>
      {!items ? (
        <p style={field.help}>Loading…</p>
      ) : items.length === 0 ? (
        <p style={field.help}>{empty}</p>
      ) : (
        <div style={s.cards}>
          {items.map((it) => {
            const on = selected.includes(it.name)
            return (
              <label key={`${it.namespace}/${it.name}`} style={{ ...s.optionCard, borderColor: on ? ds.accent : 'var(--ds-border)' }}>
                <input type="checkbox" checked={on} onChange={() => onToggle(it.name)} />
                <span style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
                  <span style={{ fontSize: 13, fontWeight: 600 }}>{it.name}</span>
                  {it.description && <span style={field.help}>{it.description}</span>}
                </span>
              </label>
            )
          })}
        </div>
      )}
    </fieldset>
  )
}

function toggleBtn(active: boolean): CSSProperties {
  return {
    padding: '5px 14px', fontSize: 12, cursor: 'pointer', border: 'none',
    background: active ? ds.accent : 'transparent', color: active ? '#fff' : ds.textSecondary,
  }
}

const s: Record<string, CSSProperties> = {
  body: { padding: '8px 24px 24px', display: 'flex', flexDirection: 'column', gap: 10 },
  columns: { display: 'flex', gap: 24, alignItems: 'flex-start', flexWrap: 'wrap' },
  main: { flex: '1 1 480px', minWidth: 0, display: 'flex', flexDirection: 'column', gap: 12 },
  side: { flex: '1 1 340px', minWidth: 300, display: 'flex', flexDirection: 'column', gap: 12 },
  fieldset: { border: 'none', margin: 0, padding: 0, minInlineSize: 0, display: 'flex', flexDirection: 'column', gap: 20 },
  section: { display: 'flex', flexDirection: 'column', gap: 10 },
  h2: { margin: 0, fontSize: 16, fontWeight: 600, color: ds.textPrimary },
  srOnly: { position: 'absolute', width: 1, height: 1, overflow: 'hidden', clip: 'rect(0 0 0 0)' },
  banner: { border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, padding: 12, fontSize: 13, color: ds.textPrimary, background: ds.surface },
  toggle: { display: 'inline-flex', border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, overflow: 'hidden' },
  check: { display: 'flex', gap: 8, alignItems: 'center', fontSize: 13, color: ds.textPrimary },
  check2: { minHeight: 18 },
  cards: { display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(200px, 1fr))', gap: 8 },
  optionCard: {
    display: 'flex', gap: 8, alignItems: 'flex-start', padding: 10, background: ds.surface, color: ds.textPrimary,
    border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.md, cursor: 'pointer',
  },
  card: { display: 'flex', flexDirection: 'column', gap: 8, padding: 14, background: ds.surface, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.xl },
}
