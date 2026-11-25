/**
 * Route table. Every view has a URL:
 *
 *   /                                         Home (widgets each user arranges)
 *   /agents                                   Agent catalog
 *   /agents/templates[/:ns/:name]             Templates (read only)
 *   /agents/details/:ns/:name                 an agent: run form, chat
 *   /agents/published/:ns/:name               a published agent (chat, runs)
 *   /agents/definitions/:ns/:name             an agent definition
 *   /runs                                     Runs (live + past, with filters)
 *   /runs/live/:ns/:name                      a live run
 *   /runs/history/:ns/:name                   an archived run
 *   /approvals                                Approvals
 *   /connections                              your connected accounts for MCP servers (and the OAuth callback landing page)
 *   /build                                    my agents, with Create and Use a template
 *   /build/new, /build/:ns/:name              the agent builder
 *   /evaluate, /evaluate/:id                  evaluation datasets, and one with its cases and graders
 *   /evaluate/experiments, .../:id, .../:id/review   experiments, one experiment, its blind review queue
 *   /operate/{fleet,schedules,costs,routing,status}   Operate tabs (fleet is the default)
 *   /operate/workflows[/:ns/:name]            workflows
 *   /settings/providers[/:ns/:name]           model providers
 *   /settings/models[/:ns/:name]              model selection
 *   /settings/connections/{mcp,tools}/:ns/:name   MCP servers and tools
 *   /settings/knowledge[/:ns/:name]           knowledge bases
 *
 * `/?tab=<old tab>` is redirected to the matching route (approval
 * notifications still send `/?tab=approvals`).
 */
import { useState, type ReactNode } from 'react'
import { Link, Navigate, Outlet, Route, Routes, useLocation, useNavigate, useParams, useSearchParams } from 'react-router'
import { ResourceList, type ResourceTab } from './components/ResourceList'
import { ConfigList } from './components/ConfigList'
import { ConfigDetailView } from './components/ConfigDetailView'
import { RunView } from './components/RunView'
import { DeploymentView } from './components/DeploymentView'
import { WorkflowView } from './components/WorkflowView'
import { RunsView } from './components/RunsView'
import { RunHistoryDetailView } from './components/RunHistoryDetailView'
import { SystemStatusPage } from './components/SystemStatusPage'
import { CostDashboard } from './components/CostDashboard'
import { SystemDashboard } from './components/SystemDashboard'
import { HomeBoard } from './components/HomeBoard'
import { AgentCatalog } from './components/AgentCatalog'
import { AgentPage } from './components/AgentPage'
import { TemplateDetail, TemplatesGallery } from './components/TemplatesGallery'
import { ApprovalsView } from './components/ApprovalsView'
import { FleetView } from './components/FleetView'
import { SchedulesView } from './components/SchedulesView'
import { ConnectionsView } from './components/ConnectionsView'
import { RoutingInsightsView } from './components/RoutingInsightsView'
import { AgentBuilder } from './components/AgentBuilder'
import { BuildHome } from './components/BuildHome'
import { EvalDatasets } from './components/EvalDatasets'
import { EvalDatasetDetail } from './components/EvalDatasetDetail'
import { EvalExperiments } from './components/EvalExperiments'
import { EvalExperimentDetail } from './components/EvalExperimentDetail'
import { EvalReview } from './components/EvalReview'
import { AppShell } from './shell/AppShell'
import { ICON } from './lib/icons'
import { ds } from './lib/designSystem'
import { canSee, useRoles } from './lib/access'
import { EmptyState, ListGroupHeading, MasterDetail, Page, PageHeader, Section, SubNav } from './lib/layout'
import { useNamespace } from './lib/namespace'
import {
  ROUTES, archivedRunPath, experimentsPath, legacyTabPath, liveRunPath, pathForSelection, pathForTab, selectionFromPath,
} from './lib/routes'

// ── Home ─────────────────────────────────────────────────────────────────────

function HomeRoute() {
  const [params] = useSearchParams()
  const navigate = useNavigate()
  const legacy = legacyTabPath(params.get('tab'))
  if (legacy) return <Navigate to={legacy} replace />
  return (
    <div style={{ flex: 1, overflowY: 'auto' }}>
      <HomeBoard />
      <PlatformOverview onOpen={(tab) => navigate(pathForTab(tab))} />
    </div>
  )
}

/** The platform-wide resource overview, kept below My work and loaded on request. */
function PlatformOverview({ onOpen }: { onOpen: (tab: ResourceTab) => void }) {
  const [show, setShow] = useState(false)
  const roles = useRoles()
  if (!canSee(roles, 'operate')) return null
  return (
    <Section
      title="Platform overview"
      actions={
        <button type="button" style={{ background: 'none', border: 'none', color: ds.accent, cursor: 'pointer', fontSize: 12 }} aria-expanded={show} onClick={() => setShow((v) => !v)}>
          {show ? 'Hide' : 'Show'}
        </button>
      }
    >
      {show && <SystemDashboard navigateToTab={onOpen} />}
    </Section>
  )
}

// ── Lists that highlight the open resource ───────────────────────────────────

function ListPane({ tab }: { tab: ResourceTab }) {
  const { namespace } = useNamespace()
  const { pathname } = useLocation()
  const navigate = useNavigate()
  const selection = selectionFromPath(pathname)
  const onSelect = (sel: Parameters<typeof pathForSelection>[0]) => navigate(pathForSelection(sel))
  if (tab === 'runs' || tab === 'deployments' || tab === 'workflows') {
    return <ResourceList tab={tab} namespace={namespace} selection={selection} onSelect={onSelect} />
  }
  return <ConfigList tab={tab} namespace={namespace} selection={selection} onSelect={onSelect} />
}

function ConfigDetailRoute() {
  const { pathname } = useLocation()
  const sel = selectionFromPath(pathname)
  if (!sel || sel.kind === 'run' || sel.kind === 'deployment' || sel.kind === 'workflow') return <Navigate to={ROUTES.home} replace />
  return <ConfigDetailView key={pathname} selection={sel} />
}

// ── Agents ───────────────────────────────────────────────────────────────────

/** The agent catalog and the templates, as two tabs of one page. */
function AgentsHome() {
  return (
    <div style={{ flex: 1, overflowY: 'auto' }}>
      <PageHeader title="Agents" description="Find an agent that can do what you need, and ask it to do it." />
      <SubNav
        label="Agents"
        items={[
          { to: ROUTES.agents, label: 'Catalog', end: true },
          { to: ROUTES.templates, label: 'Templates' },
        ]}
      />
      <Outlet />
    </div>
  )
}

function AgentPageRoute() {
  const { ns = '', name = '' } = useParams()
  const navigate = useNavigate()
  const [params, setParams] = useSearchParams()
  return (
    <AgentPage
      key={`${ns}/${name}`}
      namespace={ns}
      name={name}
      tab={params.get('tab') ?? 'run'}
      onTabChange={(tab) => setParams(tab === 'run' ? {} : { tab }, { replace: true })}
      onNavigateToRun={(runName, runNs) => navigate(liveRunPath(runNs, runName))}
    />
  )
}

function TemplateDetailRoute() {
  const { ns = '', name = '' } = useParams()
  return <TemplateDetail namespace={ns} name={name} />
}

function AgentsLayout() {
  return (
    <MasterDetail
      title="Agents"
      list={
        <>
          <ListGroupHeading>Published agents</ListGroupHeading>
          <ListPane tab="deployments" />
          <ListGroupHeading>All agents</ListGroupHeading>
          <ListPane tab="agents" />
        </>
      }
    >
      <Outlet />
    </MasterDetail>
  )
}

function PublishedAgentRoute() {
  const { ns = '', name = '' } = useParams()
  const navigate = useNavigate()
  return (
    <DeploymentView
      key={`${ns}/${name}`}
      namespace={ns}
      name={name}
      onNavigateToRun={(runName, runNs) => navigate(liveRunPath(runNs, runName))}
    />
  )
}

// ── Runs ─────────────────────────────────────────────────────────────────────

function RunsRoute() {
  const navigate = useNavigate()
  const { namespace } = useNamespace()
  const [params] = useSearchParams()
  const agent = params.get('agent') ?? ''
  return (
    <div style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
      <RunsView
        key={agent}
        namespace={namespace}
        initialAgent={agent}
        onNavigateToRun={(name, ns, source) =>
          navigate(source === 'live' ? liveRunPath(ns, name) : archivedRunPath(ns, name))}
      />
    </div>
  )
}

function BackToRuns() {
  return (
    <nav aria-label="Breadcrumb" style={{ padding: '10px 24px 0', fontSize: 12 }}>
      <Link to={ROUTES.runs} style={{ color: ds.accent, textDecoration: 'none' }}>Runs</Link>
      <span style={{ color: ds.textMuted }}> / </span>
      <span style={{ color: ds.textSecondary }}>Run</span>
    </nav>
  )
}

function LiveRunRoute() {
  const { ns = '', name = '' } = useParams()
  const navigate = useNavigate()
  return (
    <>
      <BackToRuns />
      <RunView
        key={`${ns}/${name}`}
        runId={name}
        namespace={ns}
        onNavigateToRun={(runName, runNs) => navigate(liveRunPath(runNs, runName))}
      />
    </>
  )
}

function ArchivedRunRoute() {
  const { ns = '', name = '' } = useParams()
  const navigate = useNavigate()
  return (
    <RunHistoryDetailView
      key={`${ns}/${name}`}
      runId={name}
      namespace={ns}
      onBack={() => navigate(ROUTES.runs)}
      onNavigateToRun={(runName, runNs) => navigate(archivedRunPath(runNs, runName))}
    />
  )
}

// ── Approvals ────────────────────────────────────────────────────────────────

function ApprovalsRoute() {
  const navigate = useNavigate()
  return (
    <div style={{ flex: 1, overflowY: 'auto' }}>
      <ApprovalsView onNavigateToRun={(runName, ns) => navigate(liveRunPath(ns, runName))} />
    </div>
  )
}

// ── Build ────────────────────────────────────────────────────────────────────

/** Build is for builders and admins; the API enforces it, this explains it. */
function BuildGuard() {
  const roles = useRoles()
  if (!canSee(roles, 'build')) {
    return (
      <Page>
        <EmptyState icon={ICON.tool} title="Building agents needs the builder role">
          Ask an administrator of your workspace to give you the builder role. You can still use the agents
          that others have published.
        </EmptyState>
      </Page>
    )
  }
  return <Outlet />
}

function BuilderRoute() {
  const { ns = '', name = '' } = useParams()
  return <AgentBuilder key={`${ns}/${name}`} namespace={ns} name={name} />
}

// ── Evaluate ─────────────────────────────────────────────────────────────────

function EvalDatasetRoute() {
  const { id = '' } = useParams()
  const n = Number(id)
  if (!Number.isInteger(n) || n < 1) return <Navigate to={ROUTES.evaluate} replace />
  return <EvalDatasetDetail key={n} id={n} />
}

function EvaluateLayout() {
  return (
    <>
      <PageHeader title="Evaluate" description="Datasets say what a good answer is. Experiments compare versions of an agent on them, and a gate stops a version that is worse from being published." />
      <SubNav label="Evaluate" items={[{ to: ROUTES.evaluate, label: 'Datasets', end: true }, { to: experimentsPath, label: 'Experiments' }]} />
      <Outlet />
    </>
  )
}

function useEvalId(): number | null {
  const { id = '' } = useParams()
  const n = Number(id)
  return Number.isInteger(n) && n >= 1 ? n : null
}

function EvalExperimentRoute() {
  const n = useEvalId()
  if (n === null) return <Navigate to={experimentsPath} replace />
  return <EvalExperimentDetail key={n} id={n} />
}

function EvalReviewRoute() {
  const n = useEvalId()
  if (n === null) return <Navigate to={experimentsPath} replace />
  return <EvalReview key={n} id={n} />
}

// ── Operate ──────────────────────────────────────────────────────────────────

function OperateLayout() {
  return (
    <>
      <PageHeader
        title="Operate"
        description="How your agents and the platform are doing: which agents need attention, what it costs, how models are chosen, and long-running workflows."
      />
      <SubNav
        label="Operate"
        items={[
          { to: ROUTES.fleet, label: 'Fleet' },
          { to: ROUTES.schedules, label: 'Schedules' },
          { to: ROUTES.costs, label: 'Costs' },
          { to: ROUTES.routing, label: 'Model routing' },
          { to: ROUTES.status, label: 'System status' },
          { to: ROUTES.workflows, label: 'Workflows' },
        ]}
      />
      <Outlet />
    </>
  )
}

function CostsRoute() {
  const navigate = useNavigate()
  const { namespace } = useNamespace()
  return (
    <div style={{ flex: 1, overflowY: 'auto' }}>
      <CostDashboard
        namespace={namespace}
        selection={null}
        onViewRuns={() => navigate(ROUTES.runs)}
        onNavigateToRun={(runName, ns) => navigate(archivedRunPath(ns, runName))}
      />
    </div>
  )
}

function RoutingRoute() {
  return <div style={{ flex: 1, overflowY: 'auto' }}><RoutingInsightsView /></div>
}

function StatusRoute() {
  const navigate = useNavigate()
  return (
    <div style={{ flex: 1, overflowY: 'auto' }}>
      <SystemStatusPage navigateToTab={(tab) => navigate(legacyTabPath(tab) ?? ROUTES.home)} />
    </div>
  )
}

function WorkflowsLayout() {
  return (
    <MasterDetail title="Workflows" list={<ListPane tab="workflows" />}>
      <Outlet />
    </MasterDetail>
  )
}

function WorkflowRoute() {
  const { ns = '', name = '' } = useParams()
  return <WorkflowView key={`${ns}/${name}`} namespace={ns} name={name} />
}

// ── Settings ─────────────────────────────────────────────────────────────────

function SettingsLayout() {
  return (
    <>
      <PageHeader
        title="Settings"
        description="The building blocks agents are made from: where models come from, what they can connect to, and what they know."
      />
      <SubNav
        label="Settings"
        items={[
          { to: ROUTES.providers, label: 'Providers' },
          { to: ROUTES.models, label: 'Model selection' },
          { to: ROUTES.connections, label: 'Connections' },
          { to: ROUTES.knowledge, label: 'Knowledge' },
        ]}
      />
      <Outlet />
    </>
  )
}

function SettingsSection({ title, tabs }: { title: string; tabs: { heading?: string; tab: ResourceTab }[] }) {
  return (
    <MasterDetail
      title={title}
      list={tabs.map((t) => (
        <div key={t.tab}>
          {t.heading && <ListGroupHeading>{t.heading}</ListGroupHeading>}
          <ListPane tab={t.tab} />
        </div>
      ))}
    >
      <Outlet />
    </MasterDetail>
  )
}

function SettingsIndex({ children, icon }: { children: ReactNode; icon: typeof ICON.agent }) {
  return <EmptyState icon={icon} title="Pick an item">{children}</EmptyState>
}

// ── Table ────────────────────────────────────────────────────────────────────

export function AppRoutes() {
  return (
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<HomeRoute />} />

        <Route path="agents">
          <Route element={<AgentsHome />}>
            <Route index element={<AgentCatalog />} />
            <Route path="templates" element={<TemplatesGallery />} />
            <Route path="templates/:ns/:name" element={<TemplateDetailRoute />} />
          </Route>
          <Route path="details/:ns/:name" element={<AgentPageRoute />} />
          <Route element={<AgentsLayout />}>
            <Route path="published/:ns/:name" element={<PublishedAgentRoute />} />
            <Route path="definitions/:ns/:name" element={<ConfigDetailRoute />} />
          </Route>
        </Route>

        <Route path="runs">
          <Route index element={<RunsRoute />} />
          <Route path="live/:ns/:name" element={<LiveRunRoute />} />
          <Route path="history/:ns/:name" element={<ArchivedRunRoute />} />
        </Route>

        <Route path="approvals" element={<ApprovalsRoute />} />
        <Route path="connections" element={<div style={{ flex: 1, overflowY: 'auto' }}><ConnectionsView /></div>} />

        <Route path="build" element={<BuildGuard />}>
          <Route index element={<BuildHome />} />
          <Route path="new" element={<AgentBuilder key="new" />} />
          <Route path=":ns/:name" element={<BuilderRoute />} />
        </Route>
        <Route path="evaluate" element={<EvaluateLayout />}>
          <Route index element={<EvalDatasets />} />
          <Route path=":id" element={<EvalDatasetRoute />} />
          <Route path="experiments" element={<EvalExperiments />} />
          <Route path="experiments/:id" element={<EvalExperimentRoute />} />
          <Route path="experiments/:id/review" element={<EvalReviewRoute />} />
        </Route>

        <Route path="operate" element={<OperateLayout />}>
          <Route index element={<Navigate to="fleet" replace />} />
          <Route path="fleet" element={<FleetView />} />
          <Route path="schedules" element={<div style={{ flex: 1, overflowY: 'auto' }}><SchedulesView /></div>} />
          <Route path="costs" element={<CostsRoute />} />
          <Route path="routing" element={<RoutingRoute />} />
          <Route path="status" element={<StatusRoute />} />
          <Route path="workflows" element={<WorkflowsLayout />}>
            <Route index element={<SettingsIndex icon={ICON.workflow}>Choose a workflow on the left to see its steps and progress. If the list is empty, no workflows have been created yet.</SettingsIndex>} />
            <Route path=":ns/:name" element={<WorkflowRoute />} />
          </Route>
        </Route>

        <Route path="settings" element={<SettingsLayout />}>
          <Route index element={<Navigate to="providers" replace />} />
          <Route path="providers" element={<SettingsSection title="Providers" tabs={[{ tab: 'modelproviders' }]} />}>
            <Route index element={<SettingsIndex icon={ICON.modelprovider}>A provider is where models come from, such as Anthropic or an in-house server. Pick one to see its status. If none are listed, agents have no model to use yet.</SettingsIndex>} />
            <Route path=":ns/:name" element={<ConfigDetailRoute />} />
          </Route>
          <Route path="models" element={<SettingsSection title="Model selection" tabs={[{ tab: 'modelselectors' }]} />}>
            <Route index element={<SettingsIndex icon={ICON.modelselector}>Model selection decides which model answers each request. Agents set to Model: Auto use one of these. Pick one to see how it chooses.</SettingsIndex>} />
            <Route path=":ns/:name" element={<ConfigDetailRoute />} />
          </Route>
          <Route
            path="connections"
            element={<SettingsSection title="Connections" tabs={[{ heading: 'Servers', tab: 'mcpservers' }, { heading: 'Tools', tab: 'tools' }]} />}
          >
            <Route index element={<SettingsIndex icon={ICON.mcpserver}>Connections are the systems and actions an agent can use, such as a ticketing system or a web search. Pick one to see what it offers.</SettingsIndex>} />
            <Route path="mcp/:ns/:name" element={<ConfigDetailRoute />} />
            <Route path="tools/:ns/:name" element={<ConfigDetailRoute />} />
          </Route>
          <Route path="knowledge" element={<SettingsSection title="Knowledge" tabs={[{ tab: 'knowledgebases' }]} />}>
            <Route index element={<SettingsIndex icon={ICON.knowledgebase}>Knowledge is the documents an agent can look things up in. Pick one to see how much is indexed.</SettingsIndex>} />
            <Route path=":ns/:name" element={<ConfigDetailRoute />} />
          </Route>
        </Route>

        <Route path="*" element={<Navigate to={ROUTES.home} replace />} />
      </Route>
    </Routes>
  )
}
