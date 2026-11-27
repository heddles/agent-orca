import { describe, expect, it, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import type { CatalogEntry } from '../api/catalog'

const mockCatalog = vi.fn()
vi.mock('../api/catalog', async () => {
  const actual = await vi.importActual<typeof import('../api/catalog')>('../api/catalog')
  return { ...actual, getCatalog: () => mockCatalog(), getAgentManifest: async () => ({ name: 'digest-agent', description: 'Writes a digest' }) }
})
vi.mock('./RequiredConnections', () => ({
  RequiredConnections: () => null, useRequiredConnections: () => [], useInvokerIdentities: () => [], useOwnerNeedsReconnect: () => [],
}))
vi.mock('./AgentChannels', () => ({ AgentChannels: () => <p>channel settings</p> }))
vi.mock('./AgentTriggers', () => ({ AgentTriggers: () => <p>trigger settings</p> }))
vi.mock('./AgentWidget', () => ({ AgentWidget: () => <p>widget settings</p> }))
vi.mock('./DeploymentView', () => ({ DeploymentView: () => <p>live chat</p> }))

import { AgentPage } from './AgentPage'

function entry(over: Partial<CatalogEntry>): CatalogEntry {
  return {
    name: 'digest-agent', namespace: 'home', description: 'Writes a digest', exampleInputs: [], maintainer: 'Platform',
    published: true, conversational: false, template: false, tools: [], modelSelector: 'deep',
    runCount: 0, typicalCostUSD: null, ...over,
  }
}

function open(tab: string, e: CatalogEntry) {
  mockCatalog.mockResolvedValue([e])
  render(
    <MemoryRouter>
      <AgentPage namespace="home" name="digest-agent" tab={tab} onTabChange={() => {}} onNavigateToRun={() => {}} />
    </MemoryRouter>,
  )
}

describe('AgentPage without a deployment', () => {
  beforeEach(() => mockCatalog.mockReset())

  it('lets a published agent have channels and triggers', async () => {
    open('channels', entry({}))
    expect(await screen.findByText('channel settings')).toBeInTheDocument()
    expect(screen.getByText('Published')).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: 'Triggers' })).toBeInTheDocument()
  })

  it.each([['chat', 'Chat needs a deployment', 'live chat'], ['widget', 'The chat widget needs a deployment', 'widget settings']])(
    'says what the %s tab needs',
    async (tab, title, hidden) => {
      open(tab, entry({}))
      expect(await screen.findByText(title)).toBeInTheDocument()
      expect(screen.queryByText(hidden)).not.toBeInTheDocument()
    },
  )

  it('shows chat and the widget for an agent with a deployment', async () => {
    open('chat', entry({ conversational: true, deployment: 'digest-agent' }))
    expect(await screen.findByText('live chat')).toBeInTheDocument()
  })

  it('says why an agent is not published', async () => {
    open('channels', entry({ published: false, notPublishedReason: 'It cannot run: ModelSelector "deep" does not exist.' }))
    expect(await screen.findByText('It cannot run: ModelSelector "deep" does not exist.')).toBeInTheDocument()
    expect(screen.queryByRole('tab', { name: 'Channels' })).not.toBeInTheDocument()
    expect(screen.queryByText('channel settings')).not.toBeInTheDocument()
  })
})
