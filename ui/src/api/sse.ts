/** SSE client for streaming AgentRun trace events from /api/runs/{id}/stream. */

export * from './traceStream'

// ── Auth token ───────────────────────────────────────────────────────────────

const TOKEN_KEY = 'agentorc_token'

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
  return fetch(url, { ...init, headers: { ...authHeaders(), ...init?.headers } })
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

/** Create a new Agent (also auto-creates an AgentDeployment). */
export async function createAgent(agent: CreateAgentRequest): Promise<{ name: string; namespace: string; deploymentError?: string }> {
  const res = await apiFetch('/api/agents', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(agent),
  })
  if (!res.ok) throw new Error(await res.text())
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

/** Delete an AgentDeployment. */
export async function deleteDeployment(namespace: string, name: string): Promise<void> {
  const res = await apiFetch(`/api/deployments/${namespace}/${name}`, { method: 'DELETE' })
  if (!res.ok) throw new Error(await res.text())
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

export interface CreateAgentRequest {
  name: string
  namespace?: string
  modelSelectorRef: string
  systemPrompt?: string
  ociRef: string
  framework?: string
  command?: string[]
  args?: string[]
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

/** Fetch operator-level feature flags. */
export async function getSystemStatus(): Promise<SystemStatus> {
  const res = await apiFetch('/api/system/status')
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}

export interface SystemStatus {
  stateConfigured: boolean
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
