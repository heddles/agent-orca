/** SSE client for streaming AgentRun trace events from /api/runs/{id}/stream. */

export * from './traceStream'

// ── Auth token ───────────────────────────────────────────────────────────────

const TOKEN_KEY = 'agentorca_token'

/** Persist a Kubernetes SA token for authenticating API requests. */
export function setToken(token: string): void {
  sessionStorage.setItem(TOKEN_KEY, token)
}

/** Retrieve the stored token, or null if none is set. */
export function getToken(): string | null {
  return sessionStorage.getItem(TOKEN_KEY)
}

function authHeaders(): Record<string, string> {
  const token = getToken()
  return token ? { Authorization: `Bearer ${token}` } : {}
}

/** fetch wrapper that automatically includes the Bearer token when set. */
function apiFetch(url: string, init?: RequestInit): Promise<Response> {
  return fetch(url, { ...init, headers: { ...authHeaders(), ...init?.headers } }).then((res) => {
    // Interactive OIDC login is required. A 401 means no valid session — send the
    // browser to the login picker instead of surfacing a bare "HTTP 401".
    if (res.status === 401 && !url.startsWith("/oauth/")) {
      window.location.href = "/oauth/login";
    }
    return res;
  });
}

/** Fetch a list of AgentRuns, optionally filtered by deployment name. */
export async function listRuns(namespace = '', deployment?: string): Promise<AgentRunSummary[]> {
  let url = `/api/runs?namespace=${encodeURIComponent(namespace)}`
  if (deployment) url += `&deployment=${encodeURIComponent(deployment)}`
  const res = await apiFetch(url)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

/** Fetch a single AgentRun with full detail including routing decisions. */
export async function getRun(runId: string, namespace: string): Promise<AgentRunDetail> {
  const res = await apiFetch(`/api/runs/${encodeURIComponent(namespace)}/${runId}`)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

/** Fetch all Agents across all namespaces. */
export async function listAgents(namespace = ''): Promise<AgentSummary[]> {
  const res = await apiFetch(`/api/agents?namespace=${namespace}`)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

/** Fetch all ModelSelectors across all namespaces. */
export async function listModelSelectors(namespace = ''): Promise<ModelSelectorSummary[]> {
  const res = await apiFetch(`/api/modelselectors?namespace=${namespace}`)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

/** Fetch all Tools across all namespaces. */
export async function listTools(namespace = ''): Promise<ToolSummary[]> {
  const res = await apiFetch(`/api/tools?namespace=${namespace}`)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

/** Fetch all MCPServers across all namespaces. */
export async function listMCPServers(namespace = ''): Promise<MCPServerSummary[]> {
  const res = await apiFetch(`/api/mcpservers?namespace=${namespace}`)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

/** Fetch all ModelProviders across all namespaces. */
export async function listModelProviders(namespace = ''): Promise<ModelProviderSummary[]> {
  const res = await apiFetch(`/api/modelproviders?namespace=${namespace}`)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

/** Fetch a paginated list of AgentDeployments across all namespaces. */
export async function listDeployments(namespace = ''): Promise<AgentDeploymentSummary[]> {
  const res = await apiFetch(`/api/deployments?namespace=${namespace}`)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

/** Fetch a single AgentDeployment with detail. */
export async function getDeployment(namespace: string, name: string): Promise<AgentDeploymentDetail> {
  const res = await apiFetch(`/api/deployments/${namespace}/${name}`)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

/** Fetch cost data across all namespaces. */
export async function getCosts(
  namespace = '',
  opts?: { run?: string; deployment?: string },
): Promise<CostData> {
  let url = `/api/costs?namespace=${namespace}`
  if (opts?.run) url += `&run=${encodeURIComponent(opts.run)}`
  if (opts?.deployment) url += `&deployment=${encodeURIComponent(opts.deployment)}`
  const res = await apiFetch(url)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

// ── Type shapes returned by the operator API ───────────────────────────────

export interface AgentRunSummary {
  name: string
  namespace: string
  agentRef: string
  phase: string
  spendUSD: string
  restartCount: number
  startTime?: string
  completionTime?: string
}

export interface AgentSummary {
  name: string
  namespace: string
  modelSelectorRef: string
  framework: string
  tools: string[] | null
  systemPrompt?: string
  serviceAccountName: string
}

export interface ToolSummary {
  name: string
  namespace: string
  type: string
  executionMode: string
  description?: string
  ready: boolean
  ociRef?: string
  agentRef?: string
}

export interface MCPTool {
  name: string
  description?: string
  inputSchema?: Record<string, unknown>
}

export interface MCPServerSummary {
  name: string
  namespace: string
  transport: string
  url?: string
  ociRef?: string
  toolCount: number
  ready: boolean
  /** Agent names explicitly granted access to this MCP server. Empty means no access (default-deny). */
  allowedAgents: string[]
  tools?: MCPTool[]
  /** Whether MCP App iframe rendering is enabled for this server. Defaults to false. */
  allowApps: boolean
}

export interface ModelProviderSummary {
  name: string
  namespace: string
  litellmModel: string
  baseURL?: string
  latencyProfile: string
  capabilities: string[]
  costPerMillionInputTokens?: string
  costPerMillionOutputTokens?: string
  contextWindow?: number
  ready: boolean
}

export interface ModelSelectorSummary {
  name: string
  namespace: string
  strategy: string
  providers?: Array<{ name: string; weight?: number; routingHint?: string }>
  fallbackChain?: string[]
  capabilityRouting?: Record<string, string>
  budget?: { perRun?: string; perDay?: string }
  activeProviders?: string[]
}

export interface AgentDeploymentSummary {
  name: string
  namespace: string
  agentRef: string
  phase: string
  readyReplicas: number
  inputSourceType?: string
  lastUpdateTime?: string
  message?: string
}

export interface AgentDeploymentDetail {
  name: string
  namespace: string
  agentRef: string
  phase: string
  readyReplicas: number
  availableReplicas: number
  consecutiveFailures: number
  inputSourceType?: string
  lastUpdateTime?: string
  message?: string
  contextUsedTokens?: number
  maxContextTokens?: number
}

export type SidebarSelection =
  | { kind: 'run'; name: string }
  | { kind: 'deployment'; name: string; namespace: string }

export interface RoutingDecisionInfo {
  model: string
  provider: string
  strategy: string
  reason: string
  confidence: string
  timestamp?: string
}

export interface AgentRunDetail {
  name: string
  namespace: string
  agentRef: string
  input: string
  phase: string
  output?: string
  spendUSD: string
  restartCount: number
  lastRestartReason?: string
  startTime?: string
  completionTime?: string
  routingDecisions: RoutingDecisionInfo[]
  /** Child AgentRun names spawned by this run via the agent-as-tool pattern. */
  childRunRefs?: string[]
  /** Parent AgentRun name, if this run was spawned as a child. */
  parentRunRef?: string
  /** Clarifying question when phase is WaitingForInput. */
  clarifyQuestion?: string
  /** Human's answer to the clarifying question. */
  clarifyAnswer?: string
  /** When the run entered WaitingForInput. */
  waitingSince?: string
  /** Continuation run created when the human answers a clarification. */
  continuationRunRef?: string
  /** Estimated token count of the conversation context. */
  contextUsedTokens?: number
  /** Maximum context window size for the selected model. */
  maxContextTokens?: number
}

export interface CostData {
  totalUSD: string
  byAgent: Record<string, string>
  byModel: Record<string, string>
  byDay: Array<{ date: string; usd: string }>
}

// ── Workflow API ─────────────────────────────────────────────────────────────

export interface AgentWorkflowSummary {
  name: string
  namespace: string
  description?: string
  phase: string
  stepCount: number
  totalSpendUSD: string
  startTime?: string
  completionTime?: string
}

export interface WorkflowStepSummary {
  name: string
  agentRef: string
  input?: string
  phase: string
  agentRunRef?: string
  output?: string
  spendUSD?: string
  failureReason?: string
  startTime?: string
  completionTime?: string
}

export interface AgentWorkflowDetail extends AgentWorkflowSummary {
  steps: WorkflowStepSummary[]
}

/** Fetch all AgentWorkflows across all namespaces. */
export async function listWorkflows(namespace = ''): Promise<AgentWorkflowSummary[]> {
  const res = await apiFetch(`/api/workflows?namespace=${namespace}`)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

/** Fetch a single AgentWorkflow with step detail. */
export async function getWorkflow(namespace: string, name: string): Promise<AgentWorkflowDetail> {
  const res = await apiFetch(`/api/workflows/${namespace}/${name}`)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

// ── Chat API ────────────────────────────────────────────────────────────────

export interface ChatMessage {
  role: 'user' | 'assistant'
  content: string
  /** JSON-encoded trace entries captured during this assistant turn. */
  traceEntries?: string
}

export interface ExecuteResponse {
  runName: string
  sessionId: string
}

export interface ChatHistoryResponse {
  sessionId: string
  messages: ChatMessage[]
}

/** Send a chat message to a deployment. Creates an AgentRun and returns the run name. */
export async function executeDeployment(
  namespace: string,
  name: string,
  input: string,
  sessionId?: string,
): Promise<ExecuteResponse> {
  const res = await apiFetch(`/api/deployments/${namespace}/${name}/execute`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ input, sessionId }),
  })
  if (!res.ok) throw new Error(await res.text())
  return res.json()
}

/** Load conversation history for a session. */
export async function getChatHistory(
  namespace: string,
  name: string,
  sessionId: string,
): Promise<ChatHistoryResponse> {
  const res = await apiFetch(
    `/api/deployments/${namespace}/${name}/history?sessionId=${encodeURIComponent(sessionId)}`,
  )
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

/** Fetch operator-level system status with subsystem health and metrics.
 *  Pass `range` ("1h" | "6h" | "24h" | "7d") to scope the returned metric
 *  samples to a time window (default 24h). */
export async function getSystemStatus(range?: MetricRange): Promise<SystemStatus> {
  let url = '/api/system/status'
  if (range) url += `?range=${encodeURIComponent(range)}`
  const res = await apiFetch(url)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

export type MetricRange = '1h' | '6h' | '24h' | '7d'

/** A point-in-time metric snapshot used to render time-series graphs. */
export interface MetricSample {
  time: number
  requestCount: number
  egressPublished: number
  egressFailed: number
  p50LatencyMs: number
  p95LatencyMs: number
  p99LatencyMs: number
  tokenThroughput: number
}

export interface SystemSubSystemStatus {
  name: string
  status: 'up' | 'down' | 'degraded'
  message?: string
  latencyMs?: number
}

export interface ProviderHealth {
  name: string
  namespace: string
  ready: boolean
  latencyMs?: number
  message?: string
}

export interface SystemMetrics {
  requestCount24h: number
  p50LatencyMs: number
  p95LatencyMs: number
  p99LatencyMs: number
  tokenThroughput: number
  egressPublished: number
  egressFailed: number
  /** Time-series snapshots for the requested range (see ?range=). */
  samples?: MetricSample[]
}

export interface AlertEntry {
  id: string
  subsystem: string
  state: 'firing' | 'resolved'
  message: string
  firstSeen: string
  lastSeen: string
  resolvedAt?: string
}

export interface SystemStatus {
  stateConfigured: boolean
  /** True when the PostgreSQL archival store is wired up (controls /api/runs/history). */
  runHistoryConfigured: boolean
  version?: string
  subsystems: SystemSubSystemStatus[]
  modelProviders: ProviderHealth[]
  metrics?: SystemMetrics
  alerts: AlertEntry[]
}

/** Fetch alert history from the alert manager. */
export async function getAlerts(): Promise<AlertEntry[]> {
  const res = await apiFetch('/api/system/alerts')
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

export interface KnowledgeBaseCondition {
  type: string
  status: string
  reason?: string
  message?: string
  lastTransitionTime?: string
}

export interface KnowledgeBaseSummary {
  name: string
  namespace: string
  description?: string
  ready: boolean
  /** Human-readable status message, populated on failure or while waiting. */
  message?: string
  conditions?: KnowledgeBaseCondition[]
  documentCount: number
  chunkCount: number
  vectorStoreURL?: string
  collectionName?: string
  modelSelectorRef: string
  dimensions: number
  chunkSize: number
  chunkOverlap: number
  storageUsedPercent: number
  lastSyncTime?: string
}

/** Fetch all KnowledgeBases across all namespaces. */
export async function listKnowledgeBases(namespace = ''): Promise<KnowledgeBaseSummary[]> {
  const res = await apiFetch(`/api/knowledgebases?namespace=${namespace}`)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

/** Cancel a Pending or Running AgentRun. */
export async function cancelRun(runId: string, namespace: string): Promise<void> {
  const res = await apiFetch(`/api/runs/${encodeURIComponent(namespace)}/${runId}/stop`, { method: 'POST' })
  if (!res.ok) throw new Error(await res.text())
}

/** Submit a human answer to a clarifying question for a WaitingForInput run.
 *  Returns the name of the continuation run created to carry the work forward. */
export async function answerClarification(
  runId: string,
  answer: string,
  namespace: string,
): Promise<{ runName: string }> {
  const res = await apiFetch(`/api/runs/${encodeURIComponent(namespace)}/${runId}/answer`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ answer }),
  })
  if (!res.ok) throw new Error(await res.text())
  return res.json()
}

/** Save an assistant response to the session checkpoint. */
export async function saveChatResponse(
  namespace: string,
  name: string,
  sessionId: string,
  output: string,
  traceEntries?: string,
): Promise<void> {
  const res = await apiFetch(`/api/deployments/${namespace}/${name}/complete`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sessionId, output, traceEntries }),
  })
  if (!res.ok) throw new Error(await res.text())
}

// ── Run History (PostgreSQL archival) ────────────────────────────────────────

export interface RunHistorySummary {
  name: string
  namespace: string
  agentRef: string
  phase: string
  spendUSD: string
  startTime?: string
  completionTime?: string
  tenant?: string
  /** Estimated token count of the conversation context at archival time. */
  contextUsedTokens?: number
  /** Maximum context window size for the selected model. */
  maxContextTokens?: number
}

export interface RunHistoryResponse {
  runs: RunHistorySummary[]
  total: number
  limit: number
  offset: number
}

export interface RunHistoryQuery {
  limit?: number
  offset?: number
  phase?: string
  agentRef?: string
  search?: string
  namespace?: string
}

/** Fetch paginated, filtered historical runs from PostgreSQL archival store. */
export async function listRunHistory(opts: RunHistoryQuery = {}): Promise<RunHistoryResponse> {
  const params = new URLSearchParams()
  if (opts.limit) params.set('limit', String(opts.limit))
  if (opts.offset) params.set('offset', String(opts.offset))
  if (opts.phase) params.set('phase', opts.phase)
  if (opts.agentRef) params.set('agentRef', opts.agentRef)
  if (opts.search) params.set('search', opts.search)
  if (opts.namespace) params.set('namespace', opts.namespace)
  const res = await apiFetch(`/api/runs/history?${params}`)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

// ── Archived run detail ────────────────────────────────────────────────────

/** Full detail of a single archived AgentRun (read from PostgreSQL). */
export interface RunHistoryDetail {
  name: string
  namespace: string
  agentRef: string
  input: string
  output: string
  phase: string
  spendUSD: string
  restartCount: number
  startTime?: string
  completionTime?: string
  contextUsedTokens?: number
  maxContextTokens?: number
  routingDecisions: RoutingDecisionInfo[]
  childRunRefs?: string[]
  /** Resolved Tool names from the Agent CRD spec. */
  tools?: string[]
  /** Resolved MCP server names accessible to this agent in its namespace. */
  mcpServers?: string[]
  /** The model selected at runtime, derived from the last routing decision. */
  resolvedModel?: string
  podName?: string
}

/** Fetch the full detail of a single archived run from PostgreSQL. */
export async function getRunHistoryDetail(namespace: string, name: string): Promise<RunHistoryDetail> {
  const res = await apiFetch(`/api/runs/history/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

// ── Generic Resource CRUD ───────────────────────────────────────────────────

export type ResourceKind =
  | 'agents' | 'tools' | 'mcpservers' | 'modelproviders'
  | 'knowledgebases' | 'modelselectors' | 'agentdeployments' | 'agentworkflows'

/** List resources of a given kind (scoped to tenant namespace when authed). */
export async function listResources(kind: ResourceKind, namespace = ''): Promise<any[]> {
  const url = `/api/resources/${encodeURIComponent(kind)}?namespace=${encodeURIComponent(namespace)}`
  const res = await apiFetch(url)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

/** Get a single resource by kind, namespace, and name. */
export async function getResource(kind: ResourceKind, namespace: string, name: string): Promise<any> {
  const res = await apiFetch(`/api/resources/${encodeURIComponent(kind)}/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}
