/**
 * Route map for the app. Every view has a URL; components navigate by building
 * paths here so that no file hard-codes a path string.
 */
import type { ResourceSelection, ResourceTab } from '../components/ResourceList'

const enc = encodeURIComponent

export const ROUTES = {
  home: '/',
  agents: '/agents',
  templates: '/agents/templates',
  runs: '/runs',
  approvals: '/approvals',
  build: '/build',
  evaluate: '/evaluate',
  operate: '/operate',
  fleet: '/operate/fleet',
  costs: '/operate/costs',
  routing: '/operate/routing',
  status: '/operate/status',
  workflows: '/operate/workflows',
  schedules: '/operate/schedules',
  settings: '/settings',
  providers: '/settings/providers',
  models: '/settings/models',
  connections: '/settings/connections',
  knowledge: '/settings/knowledge',
} as const

/** An evaluation dataset. */
export const datasetPath = (id: number) => `/evaluate/${id}`
/** The experiments list, one experiment, and its blind review queue. */
export const experimentsPath = '/evaluate/experiments'
export const experimentPath = (id: number) => `/evaluate/experiments/${id}`
export const reviewPath = (id: number) => `/evaluate/experiments/${id}/review`
export const buildNewPath = '/build/new'
/** An agent in the builder. */
export const buildPath = (ns: string, name: string) => `/build/${enc(ns)}/${enc(name)}`
export const agentPath =(ns: string, name: string) => `/agents/definitions/${enc(ns)}/${enc(name)}`
/** The agent page: run form, and chat when the agent is published. */
export const agentDetailsPath = (ns: string, name: string) => `/agents/details/${enc(ns)}/${enc(name)}`
export const templatePath = (ns: string, name: string) => `/agents/templates/${enc(ns)}/${enc(name)}`
export const publishedAgentPath = (ns: string, name: string) => `/agents/published/${enc(ns)}/${enc(name)}`
/** A live run (an AgentRun resource in the cluster). */
export const liveRunPath = (ns: string, name: string) => `/runs/live/${enc(ns)}/${enc(name)}`
/** Runs of one agent, through the Runs page's agent filter. */
export const agentRunsPath = (agent: string) => `/runs?agent=${enc(agent)}`
/** An archived run (read from the history store). */
export const archivedRunPath = (ns: string, name: string) => `/runs/history/${enc(ns)}/${enc(name)}`
export const workflowPath = (ns: string, name: string) => `/operate/workflows/${enc(ns)}/${enc(name)}`
export const providerPath = (ns: string, name: string) => `/settings/providers/${enc(ns)}/${enc(name)}`
export const modelSelectorPath = (ns: string, name: string) => `/settings/models/${enc(ns)}/${enc(name)}`
export const mcpServerPath = (ns: string, name: string) => `/settings/connections/mcp/${enc(ns)}/${enc(name)}`
export const toolPath = (ns: string, name: string) => `/settings/connections/tools/${enc(ns)}/${enc(name)}`
export const knowledgePath = (ns: string, name: string) => `/settings/knowledge/${enc(ns)}/${enc(name)}`

/** Where selecting a resource in a list leads. */
export function pathForSelection(sel: ResourceSelection): string {
  switch (sel.kind) {
    case 'run': return liveRunPath(sel.namespace, sel.name)
    case 'deployment': return publishedAgentPath(sel.namespace, sel.name)
    case 'workflow': return workflowPath(sel.namespace, sel.name)
    case 'agent': return agentPath(sel.namespace, sel.name)
    case 'tool': return toolPath(sel.namespace, sel.name)
    case 'mcpserver': return mcpServerPath(sel.namespace, sel.name)
    case 'modelprovider': return providerPath(sel.namespace, sel.name)
    case 'knowledgebase': return knowledgePath(sel.namespace, sel.name)
    case 'modelselector': return modelSelectorPath(sel.namespace, sel.name)
  }
}

/** The list route a resource tab lives under. */
export function pathForTab(tab: ResourceTab): string {
  switch (tab) {
    case 'runs': return ROUTES.runs
    case 'deployments':
    case 'agents': return ROUTES.agents
    case 'workflows': return ROUTES.workflows
    case 'tools':
    case 'mcpservers': return ROUTES.connections
    case 'modelproviders': return ROUTES.providers
    case 'knowledgebases': return ROUTES.knowledge
    case 'modelselectors': return ROUTES.models
  }
}

/**
 * Old `?tab=` deep links (approval notifications still send `/?tab=approvals`)
 * mapped to the new routes. Returns null when there is no `tab`; an unknown
 * value lands on Home.
 */
export function legacyTabPath(tab: string | null): string | null {
  if (!tab) return null
  switch (tab) {
    case 'approvals': return ROUTES.approvals
    case 'history':
    case 'runs': return ROUTES.runs
    case 'deployments':
    case 'agents': return ROUTES.agents
    case 'workflows': return ROUTES.workflows
    case 'status': return ROUTES.status
    case 'routing': return ROUTES.routing
    case 'tools':
    case 'mcpservers': return ROUTES.connections
    case 'modelproviders': return ROUTES.providers
    case 'knowledgebases': return ROUTES.knowledge
    case 'modelselectors': return ROUTES.models
    default: return ROUTES.home
  }
}

const SELECTION_PATTERNS: [RegExp, ResourceSelection['kind']][] = [
  [/^\/runs\/live\/([^/]+)\/([^/]+)$/, 'run'],
  [/^\/agents\/published\/([^/]+)\/([^/]+)$/, 'deployment'],
  [/^\/agents\/definitions\/([^/]+)\/([^/]+)$/, 'agent'],
  [/^\/operate\/workflows\/([^/]+)\/([^/]+)$/, 'workflow'],
  [/^\/settings\/providers\/([^/]+)\/([^/]+)$/, 'modelprovider'],
  [/^\/settings\/models\/([^/]+)\/([^/]+)$/, 'modelselector'],
  [/^\/settings\/connections\/mcp\/([^/]+)\/([^/]+)$/, 'mcpserver'],
  [/^\/settings\/connections\/tools\/([^/]+)\/([^/]+)$/, 'tool'],
  [/^\/settings\/knowledge\/([^/]+)\/([^/]+)$/, 'knowledgebase'],
]

/** The resource a detail URL points at, so list panes can highlight it. */
export function selectionFromPath(pathname: string): ResourceSelection | null {
  for (const [re, kind] of SELECTION_PATTERNS) {
    const m = re.exec(pathname)
    if (m) return { kind, namespace: decodeURIComponent(m[1]), name: decodeURIComponent(m[2]) }
  }
  return null
}
