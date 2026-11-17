import { describe, expect, it, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router'
import type { KubeObject, Ownership } from '../api/builder'

const m = {
  getResource: vi.fn(), createResource: vi.fn(), updateResource: vi.fn(), dryRunResource: vi.fn(),
  getBuilderOptions: vi.fn(), publishAgent: vi.fn(), unpublishAgent: vi.fn(), duplicateResource: vi.fn(),
  exportResource: vi.fn(), cancelHandoff: vi.fn(),
  getCatalog: vi.fn(), getAgentManifest: vi.fn(), startAgentRun: vi.fn(),
}
vi.mock('../api/builder', async () => {
  const actual = await vi.importActual<typeof import('../api/builder')>('../api/builder')
  return {
    ...actual,
    getResource: (...a: unknown[]) => m.getResource(...a),
    createResource: (...a: unknown[]) => m.createResource(...a),
    updateResource: (...a: unknown[]) => m.updateResource(...a),
    dryRunResource: (...a: unknown[]) => m.dryRunResource(...a),
    getBuilderOptions: () => m.getBuilderOptions(),
    publishAgent: (...a: unknown[]) => m.publishAgent(...a),
    unpublishAgent: (...a: unknown[]) => m.unpublishAgent(...a),
    duplicateResource: (...a: unknown[]) => m.duplicateResource(...a),
    exportResource: (...a: unknown[]) => m.exportResource(...a),
    cancelHandoff: (...a: unknown[]) => m.cancelHandoff(...a),
  }
})
vi.mock('../api/revisions', () => ({
  listRevisions: () => Promise.resolve({ revisions: [], next: '' }),
  getRevision: () => Promise.reject(new Error('not used')),
  restoreRevision: () => Promise.reject(new Error('not used')),
}))
vi.mock('../api/catalog', async () => {
  const actual = await vi.importActual<typeof import('../api/catalog')>('../api/catalog')
  return {
    ...actual,
    getCatalog: (...a: unknown[]) => m.getCatalog(...a),
    getAgentManifest: (...a: unknown[]) => m.getAgentManifest(...a),
    startAgentRun: (...a: unknown[]) => m.startAgentRun(...a),
  }
})
const mockWorkspaces = vi.hoisted(() => ({ current: [{ name: 'Mine', namespace: 'team-a', primary: true }] }))
vi.mock('../api/fleet', () => ({
  getMe: () => Promise.resolve({
    userId: 'u', groups: [], tenant: { id: 't', name: 'T' }, roles: ['builder'],
    workspaces: mockWorkspaces.current,
  }),
}))
vi.mock('./RunView', () => ({ RunView: (p: { runId: string; namespace: string }) => <div>live run {p.namespace}/{p.runId}</div> }))

import { ApiError } from '../api/builder'
import { AgentBuilder } from './AgentBuilder'

const UI_OWNED: Ownership = { owner: 'ui', editable: true, handoff: { pending: false } }

function agent(over: Partial<KubeObject> = {}, ownership: Ownership = UI_OWNED): KubeObject {
  return {
    apiVersion: 'agentorca.agentorca.io/v1alpha1',
    kind: 'Agent',
    metadata: {
      name: 'helper', namespace: 'team-a', resourceVersion: '5',
      annotations: {
        'agentorca.io/description': 'Helps with questions',
        'agentorca.io/maintainer': 'Support',
        'agentorca.io/example-inputs': '["How do I reset a password?"]',
      },
    },
    spec: {
      modelSelectorRef: 'default', systemPrompt: 'Be helpful.', tools: ['search'],
      runtime: { ociRef: 'img:1', framework: 'openai-compatible' },
    },
    ownership,
    ...over,
  }
}

const OPTIONS = {
  modelSelectors: [{ name: 'default', namespace: 'team-a' }, { name: 'fast', namespace: 'team-a', description: 'Fast and cheap' }],
  tools: [{ name: 'search', namespace: 'team-a', description: 'Web search' }, { name: 'calc', namespace: 'team-a' }],
  mcpServers: [{ name: 'tickets', namespace: 'team-a', description: 'Ticket system' }],
  knowledgeBases: [{ name: 'handbook', namespace: 'team-a' }],
  guardrailPolicies: [{ name: 'strict', namespace: 'team-a' }],
}

function renderBuilder(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/build/new" element={<AgentBuilder key="new" />} />
        <Route path="/build/:ns/:name" element={<RouteBuilder />} />
        <Route path="/build" element={<div>build home</div>} />
      </Routes>
    </MemoryRouter>,
  )
}

import { useParams } from 'react-router'
function RouteBuilder() {
  const { ns = '', name = '' } = useParams()
  return <AgentBuilder key={`${ns}/${name}`} namespace={ns} name={name} />
}

beforeEach(() => {
  Object.values(m).forEach((f) => f.mockReset())
  m.getBuilderOptions.mockResolvedValue(OPTIONS)
  m.getCatalog.mockResolvedValue([])
  m.dryRunResource.mockResolvedValue({ valid: true, errors: [] })
  m.getAgentManifest.mockResolvedValue({ name: 'helper', description: 'd' })
  mockWorkspaces.current = [{ name: 'Mine', namespace: 'team-a', primary: true }]
  vi.stubGlobal('confirm', vi.fn(() => true))
})

async function fillNew(user: ReturnType<typeof userEvent.setup>) {
  await user.type(await screen.findByLabelText('Name'), 'helper')
  await user.type(screen.getByLabelText('Description'), 'Helps with questions')
  await user.type(screen.getByLabelText('Instructions', { selector: 'textarea' }), 'Be helpful.')
}

describe('AgentBuilder: a new agent', () => {
  it('uses plain words, not CRD names, in the form', async () => {
    renderBuilder('/build/new')
    await screen.findByLabelText('Name')
    for (const label of ['What it does', 'Instructions', 'Model', 'Connections', 'Knowledge', 'Safety', 'How people use it']) {
      expect(screen.getByRole('heading', { name: label })).toBeInTheDocument()
    }
    expect(screen.queryByText(/systemPrompt|modelSelectorRef|knowledgeBases/)).not.toBeInTheDocument()
    expect(await screen.findByRole('option', { name: 'Auto (recommended)' })).toBeInTheDocument()
  })

  it('shows what is missing and does not save', async () => {
    const user = userEvent.setup()
    renderBuilder('/build/new')
    await screen.findByLabelText('Name')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByText('Give the agent a name.')).toBeInTheDocument()
    expect(screen.getByText('Write the instructions the agent follows.')).toBeInTheDocument()
    expect(screen.getByLabelText('Name')).toBeInvalid()
    expect(m.createResource).not.toHaveBeenCalled()
  })

  it('says honestly what is not available: uploads and schedules', async () => {
    renderBuilder('/build/new')
    await screen.findByLabelText('Name')
    expect(screen.getByText(/Uploading documents is not available here yet/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Settings → Knowledge' })).toHaveAttribute('href', '/settings/knowledge')
    expect(screen.getByRole('checkbox', { name: /On a schedule/ })).toBeDisabled()
    expect(screen.getByText(/Scheduled runs are not available yet/)).toBeInTheDocument()
  })

  it('shows the dry-run errors from the platform, next to the form', async () => {
    m.dryRunResource.mockResolvedValue({ valid: false, errors: ['spec.systemPrompt: too long'] })
    const user = userEvent.setup()
    renderBuilder('/build/new')
    await fillNew(user)
    expect((await screen.findAllByText('spec.systemPrompt: too long', {}, { timeout: 3000 })).length).toBeGreaterThan(0)
    expect(screen.getByText('The platform would not accept this yet:')).toBeInTheDocument()
    expect(m.dryRunResource).toHaveBeenLastCalledWith('agents', expect.objectContaining({ metadata: expect.objectContaining({ name: 'helper', namespace: 'team-a' }) }), null)
  })

  it('creates the agent with the chosen connections, model and knowledge', async () => {
    m.createResource.mockResolvedValue(agent())
    m.getResource.mockResolvedValue(agent())
    const user = userEvent.setup()
    renderBuilder('/build/new')
    await fillNew(user)
    await user.selectOptions(screen.getByLabelText('Model', { selector: 'select' }), 'fast')
    await user.click(screen.getByRole('checkbox', { name: /search/ }))
    await user.click(screen.getByRole('checkbox', { name: /tickets/ }))
    await user.click(screen.getByRole('checkbox', { name: /handbook/ }))
    await user.selectOptions(screen.getByLabelText('Safety policy'), 'strict')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(m.createResource).toHaveBeenCalledTimes(1))
    const [kind, body] = m.createResource.mock.calls[0] as [string, KubeObject]
    expect(kind).toBe('agents')
    expect(body.metadata).toMatchObject({ name: 'helper', namespace: 'team-a' })
    expect(body.spec).toMatchObject({
      modelSelectorRef: 'fast', systemPrompt: 'Be helpful.', tools: ['search', 'tickets'],
      knowledgeBases: ['handbook'], guardrailPolicyRef: 'strict',
    })
    expect((body.spec as { runtime: { ociRef: string } }).runtime.ociRef).toBeTruthy()
    // Auto stays the default selector.
    expect(body.metadata.annotations?.['agentorca.io/description']).toBe('Helps with questions')
    // After creating, the saved agent opens at its own address.
    await waitFor(() => expect(m.getResource).toHaveBeenCalledWith('agents', 'team-a', 'helper'))
  })

  it('disables the test panel and publishing until the agent is saved', async () => {
    renderBuilder('/build/new')
    await screen.findByLabelText('Name')
    expect(screen.getByText('Save the agent first. Then you can try it here.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Publish' })).toBeDisabled()
    expect(screen.getByText(/runs the last saved version/i)).toBeInTheDocument()
  })
})

describe('AgentBuilder: the YAML view', () => {
  it('syncs both ways and shows parse errors inline', async () => {
    const user = userEvent.setup()
    renderBuilder('/build/new')
    await user.type(await screen.findByLabelText('Name'), 'helper')
    await user.click(screen.getByRole('button', { name: 'YAML' }))
    const editor = screen.getByLabelText('Agent as YAML') as HTMLTextAreaElement
    expect(editor.value).toContain('name: helper')
    expect(editor.value).toContain('kind: Agent')

    // YAML to form.
    await user.clear(editor)
    await user.click(editor)
    await user.paste('apiVersion: agentorca.agentorca.io/v1alpha1\nkind: Agent\nmetadata:\n  name: from-yaml\n  namespace: team-a\nspec:\n  systemPrompt: From the YAML\n  modelSelectorRef: default\n')
    await user.click(screen.getByRole('button', { name: 'Form' }))
    expect(screen.getByLabelText('Name')).toHaveValue('from-yaml')
    expect(screen.getByLabelText('Instructions', { selector: 'textarea' })).toHaveValue('From the YAML')

    // Form to YAML.
    await user.type(screen.getByLabelText('Maintainer'), 'Ops')
    await user.click(screen.getByRole('button', { name: 'YAML' }))
    expect((screen.getByLabelText('Agent as YAML') as HTMLTextAreaElement).value).toContain('agentorca.io/maintainer: Ops')

    // A parse error is shown inline, and the form view waits for a fix.
    const editor2 = screen.getByLabelText('Agent as YAML')
    await user.click(editor2)
    await user.paste('\n  : bad: [')
    expect(await screen.findByRole('alert')).toHaveTextContent(/line/i)
    expect(screen.getByRole('button', { name: 'Form' })).toBeDisabled()
  })
})

describe('AgentBuilder: saving an existing agent', () => {
  beforeEach(() => { m.getResource.mockResolvedValue(agent()) })

  it('loads the agent into the form and saves with the resource version', async () => {
    m.updateResource.mockResolvedValue(agent({ metadata: { ...agent().metadata, resourceVersion: '6' } }))
    const user = userEvent.setup()
    renderBuilder('/build/team-a/helper')
    const prompt = await screen.findByLabelText('Instructions', { selector: 'textarea' })
    expect(prompt).toHaveValue('Be helpful.')
    expect(screen.getByLabelText('Name')).toBeDisabled()
    expect(screen.getByRole('checkbox', { name: /search/ })).toBeChecked()
    expect(screen.getByLabelText('Example inputs')).toHaveValue('How do I reset a password?')
    await user.type(prompt, ' Be brief.')
    expect(screen.getByText('Unsaved changes')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(m.updateResource).toHaveBeenCalledTimes(1))
    const [kind, ns, name, body] = m.updateResource.mock.calls[0] as [string, string, string, KubeObject]
    expect([kind, ns, name]).toEqual(['agents', 'team-a', 'helper'])
    expect(body.metadata.resourceVersion).toBe('5')
    expect((body.spec as { systemPrompt: string }).systemPrompt).toBe('Be helpful. Be brief.')
    // The runtime the form does not show is kept.
    expect((body.spec as { runtime: { ociRef: string } }).runtime.ociRef).toBe('img:1')
    expect(await screen.findByText('Saved.')).toBeInTheDocument()
    expect(screen.queryByText('Unsaved changes')).not.toBeInTheDocument()
  })

  it('on a conflict, keeps the edits and reloads on request', async () => {
    m.updateResource.mockRejectedValue(new ApiError(409, 'conflict', [], '9'))
    const user = userEvent.setup()
    renderBuilder('/build/team-a/helper')
    const prompt = await screen.findByLabelText('Instructions', { selector: 'textarea' })
    await user.type(prompt, ' My edit.')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByText('Someone changed this agent.')).toBeInTheDocument()
    expect(prompt).toHaveValue('Be helpful. My edit.')
    m.getResource.mockResolvedValue(agent({ spec: { modelSelectorRef: 'default', systemPrompt: 'Theirs.', runtime: { ociRef: 'img:1' } }, metadata: { ...agent().metadata, resourceVersion: '9' } }))
    await user.click(screen.getByRole('button', { name: 'Reload' }))
    await waitFor(() => expect(screen.getByLabelText('Instructions', { selector: 'textarea' })).toHaveValue('Theirs.'))
    expect(screen.queryByText('Someone changed this agent.')).not.toBeInTheDocument()
  })

  it('on a missing resource version (428), reads the object and retries once', async () => {
    m.updateResource
      .mockRejectedValueOnce(new ApiError(428, 'resourceVersion required'))
      .mockResolvedValueOnce(agent())
    const user = userEvent.setup()
    renderBuilder('/build/team-a/helper')
    await user.type(await screen.findByLabelText('Instructions', { selector: 'textarea' }), ' More.')
    m.getResource.mockResolvedValue(agent({ metadata: { ...agent().metadata, resourceVersion: '7' } }))
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(m.updateResource).toHaveBeenCalledTimes(2))
    expect((m.updateResource.mock.calls[1][3] as KubeObject).metadata.resourceVersion).toBe('7')
    expect(await screen.findByText('Saved.')).toBeInTheDocument()
  })

  it('shows the server errors clearly when a save is rejected', async () => {
    m.updateResource.mockRejectedValue(new ApiError(422, 'invalid', ['spec.tools[0]: tool not found']))
    const user = userEvent.setup()
    renderBuilder('/build/team-a/helper')
    await user.type(await screen.findByLabelText('Instructions', { selector: 'textarea' }), ' x')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByText('This could not be saved.')).toBeInTheDocument()
    expect(screen.getAllByText('spec.tools[0]: tool not found').length).toBeGreaterThan(0)
  })

  it('warns before the page is closed with unsaved changes', async () => {
    const user = userEvent.setup()
    renderBuilder('/build/team-a/helper')
    await user.type(await screen.findByLabelText('Instructions', { selector: 'textarea' }), ' x')
    const ev = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(ev)
    expect(ev.defaultPrevented).toBe(true)
  })
})

describe('AgentBuilder: test panel and publishing', () => {
  beforeEach(() => { m.getResource.mockResolvedValue(agent()) })

  it('runs the saved agent and shows the live run', async () => {
    m.startAgentRun.mockResolvedValue({ run_id: 'run-1' })
    const user = userEvent.setup()
    renderBuilder('/build/team-a/helper')
    const box = await screen.findByLabelText(/What should the agent do/)
    await user.type(box, 'Hello')
    await user.click(screen.getByRole('button', { name: 'Run agent' }))
    await waitFor(() => expect(m.startAgentRun).toHaveBeenCalledWith('team-a', 'helper', 'Hello'))
    expect(await screen.findByText('live run team-a/run-1')).toBeInTheDocument()
    expect(screen.getByText(/runs the last saved version/i)).toBeInTheDocument()
  })

  it('publishes, shows the result and can unpublish', async () => {
    m.publishAgent.mockResolvedValue({ deployment: { metadata: { name: 'helper-chat' } } })
    m.unpublishAgent.mockResolvedValue(undefined)
    const user = userEvent.setup()
    renderBuilder('/build/team-a/helper')
    await screen.findByLabelText('Name')
    await user.click(screen.getByRole('button', { name: 'Publish' }))
    await waitFor(() => expect(m.publishAgent).toHaveBeenCalledWith('team-a', 'helper', { chat: true }))
    expect(await screen.findByText(/Published\. People can now use this agent/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Open it' })).toHaveAttribute('href', '/agents/published/team-a/helper-chat')
    await user.click(screen.getByRole('button', { name: 'Unpublish' }))
    await waitFor(() => expect(m.unpublishAgent).toHaveBeenCalledWith('team-a', 'helper'))
    expect(await screen.findByText(/Unpublished/)).toBeInTheDocument()
  })
})

describe('AgentBuilder: ownership', () => {
  it('opens a Git-owned agent read-only with Copy YAML and Duplicate', async () => {
    m.getResource.mockResolvedValue(agent({}, { owner: 'git', tool: 'Argo CD', app: 'payments', editable: false, handoff: { pending: false } }))
    m.duplicateResource.mockResolvedValue(agent({ metadata: { name: 'helper-copy', namespace: 'team-a' } }))
    const user = userEvent.setup()
    const writeText = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    renderBuilder('/build/team-a/helper')
    expect(await screen.findByText(/Managed in Git by Argo CD app payments/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument()
    expect(screen.getByLabelText('Instructions', { selector: 'textarea' })).toBeDisabled()
    expect(screen.queryByRole('button', { name: 'Publish' })).not.toBeInTheDocument()

    await user.click(screen.getAllByRole('button', { name: 'Copy YAML' })[0])
    expect(writeText).toHaveBeenCalled()
    expect((writeText.mock.calls as unknown as string[][])[0][0]).toContain('name: helper')

    await user.click(screen.getByRole('button', { name: 'Duplicate' }))
    const dialog = screen.getByRole('dialog', { name: 'Duplicate this agent' })
    await user.click(within(dialog).getByRole('button', { name: 'Duplicate' }))
    await waitFor(() => expect(m.duplicateResource).toHaveBeenCalledWith('agents', 'team-a', 'helper', 'helper-copy'))
  })

  it('opens an agent of unknown ownership read-only', async () => {
    m.getResource.mockResolvedValue(agent({}, { owner: 'unknown', editable: false, handoff: { pending: false } }))
    renderBuilder('/build/team-a/helper')
    expect(await screen.findByText(/nothing shows who manages it/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument()
    expect(screen.getByLabelText('Instructions', { selector: 'textarea' })).toBeDisabled()
  })

  it('exports a UI-owned agent: manifest in a dialog with Copy and Download', async () => {
    m.getResource.mockResolvedValue(agent())
    m.exportResource.mockResolvedValue('kind: Agent\nmetadata:\n  name: helper\n')
    const user = userEvent.setup()
    const writeText = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    renderBuilder('/build/team-a/helper')
    await screen.findByLabelText('Name')
    await user.click(screen.getByRole('button', { name: 'Export to Git' }))
    const dialog = await screen.findByRole('dialog', { name: 'Export to Git' })
    expect(await within(dialog).findByLabelText('Manifest')).toHaveValue('kind: Agent\nmetadata:\n  name: helper\n')
    expect(within(dialog).getByText(/Once Git manages the agent/)).toBeInTheDocument()
    expect(m.exportResource).toHaveBeenCalledWith('agents', 'team-a', 'helper')
    await user.click(within(dialog).getByRole('button', { name: 'Copy' }))
    expect(writeText).toHaveBeenCalledWith('kind: Agent\nmetadata:\n  name: helper\n')
    expect(within(dialog).getByRole('button', { name: 'Download' })).toBeEnabled()
  })

  it('shows a pending handoff with its expiry and cancels it', async () => {
    const pending: Ownership = { owner: 'ui', editable: true, handoff: { pending: true, expires: '2030-01-02T03:04:05Z' } }
    m.getResource.mockResolvedValueOnce(agent({}, pending)).mockResolvedValue(agent())
    m.cancelHandoff.mockResolvedValue(undefined)
    const user = userEvent.setup()
    renderBuilder('/build/team-a/helper')
    expect(await screen.findByText('Being handed over to Git.')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Cancel handoff' }))
    await waitFor(() => expect(m.cancelHandoff).toHaveBeenCalledWith('agents', 'team-a', 'helper'))
    expect(await screen.findByText(/handoff to Git is cancelled/)).toBeInTheDocument()
    expect(screen.queryByText('Being handed over to Git.')).not.toBeInTheDocument()
  })
})

describe('AgentBuilder: choosing a model', () => {
  const modelSelect = () => screen.getByLabelText('Model', { selector: 'select' })

  it('offers Auto and writes the default selector when it exists in the workspace', async () => {
    m.createResource.mockResolvedValue(agent())
    m.getResource.mockResolvedValue(agent())
    const user = userEvent.setup()
    renderBuilder('/build/new')
    await fillNew(user)
    await waitFor(() => expect(modelSelect()).toHaveValue('default'))
    expect(screen.getByRole('option', { name: 'Auto (recommended)' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'Fast and cheap' })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(m.createResource).toHaveBeenCalled())
    expect((m.createResource.mock.calls[0][1] as KubeObject).spec).toMatchObject({ modelSelectorRef: 'default' })
  })

  it('hides Auto when there is no default selector there, and requires a choice', async () => {
    m.getBuilderOptions.mockResolvedValue({ ...OPTIONS, modelSelectors: [{ name: 'default', namespace: 'platform' }, { name: 'fast', namespace: 'team-a', description: 'Fast and cheap' }] })
    const user = userEvent.setup()
    renderBuilder('/build/new')
    await fillNew(user)
    await screen.findByRole('option', { name: 'Fast and cheap' })
    expect(screen.queryByRole('option', { name: 'Auto (recommended)' })).not.toBeInTheDocument()
    expect(modelSelect()).toHaveValue('')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByText('Choose a model.')).toBeInTheDocument()
    expect(m.createResource).not.toHaveBeenCalled()
  })

  it('blocks saving with a plain message when no model is set up', async () => {
    m.getBuilderOptions.mockResolvedValue({ ...OPTIONS, modelSelectors: [] })
    const user = userEvent.setup()
    renderBuilder('/build/new')
    await fillNew(user)
    expect(await screen.findByText(/No model is set up for this workspace yet\. Ask an administrator to add one under Settings → Models\./)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled()
    // The YAML view still works.
    await user.click(screen.getByRole('button', { name: 'YAML' }))
    expect((screen.getByLabelText('Agent as YAML') as HTMLTextAreaElement).value).toContain('name: helper')
  })

  it('shows an existing reference as written and does not rewrite it', async () => {
    const foreign = agent()
    foreign.spec = { ...foreign.spec, modelSelectorRef: 'shared-gpt' }
    m.getResource.mockResolvedValue(foreign)
    m.updateResource.mockResolvedValue(foreign)
    const user = userEvent.setup()
    renderBuilder('/build/team-a/helper')
    await screen.findByLabelText('Name')
    expect(modelSelect()).toHaveValue('shared-gpt')
    expect(screen.getByRole('option', { name: 'shared-gpt' })).toBeInTheDocument()
    await user.type(screen.getByLabelText('Instructions', { selector: 'textarea' }), ' x')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(m.updateResource).toHaveBeenCalled())
    expect((m.updateResource.mock.calls[0][3] as KubeObject).spec).toMatchObject({ modelSelectorRef: 'shared-gpt' })
  })

  it('keeps a default reference as written when the workspace has no Auto', async () => {
    m.getBuilderOptions.mockResolvedValue({ ...OPTIONS, modelSelectors: [{ name: 'default', namespace: 'platform' }] })
    m.getResource.mockResolvedValue(agent())
    renderBuilder('/build/team-a/helper')
    await screen.findByLabelText('Name')
    expect(screen.queryByRole('option', { name: 'Auto (recommended)' })).not.toBeInTheDocument()
    expect(modelSelect()).toHaveValue('default')
  })

  it('re-resolves Auto when the workspace changes', async () => {
    m.getBuilderOptions.mockResolvedValue({ ...OPTIONS, modelSelectors: [{ name: 'default', namespace: 'team-b' }, { name: 'fast', namespace: 'team-a' }] })
    mockWorkspaces.current = [
      { name: 'A', namespace: 'team-a', primary: true },
      { name: 'B', namespace: 'team-b', primary: false },
    ]
    const user = userEvent.setup()
    renderBuilder('/build/new')
    await screen.findByRole('option', { name: 'fast' })
    // team-a is the primary workspace and has no default.
    expect(screen.queryByRole('option', { name: 'Auto (recommended)' })).not.toBeInTheDocument()
    await user.selectOptions(screen.getByLabelText('Workspace'), 'team-b')
    expect(await screen.findByRole('option', { name: 'Auto (recommended)' })).toBeInTheDocument()
    expect(modelSelect()).toHaveValue('default')
    await user.selectOptions(screen.getByLabelText('Workspace'), 'team-a')
    await waitFor(() => expect(screen.queryByRole('option', { name: 'Auto (recommended)' })).not.toBeInTheDocument())
    expect(modelSelect()).toHaveValue('')
  })

  it('asks before following a link away from unsaved changes', async () => {
    const confirm = vi.fn(() => false)
    vi.stubGlobal('confirm', confirm)
    const user = userEvent.setup()
    renderBuilder('/build/new')
    await user.type(await screen.findByLabelText('Name'), 'abc')
    await user.click(screen.getByRole('link', { name: 'Build' }))
    expect(confirm).toHaveBeenCalled()
    expect(screen.queryByText('build home')).not.toBeInTheDocument()
    confirm.mockReturnValue(true)
    await user.click(screen.getByRole('link', { name: 'Build' }))
    expect(await screen.findByText('build home')).toBeInTheDocument()
  })
})
