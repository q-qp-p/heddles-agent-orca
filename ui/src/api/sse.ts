/** SSE client for streaming AgentRun trace events from /api/runs/{id}/stream. */

import type { TraceEntry } from './traceStream'

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

/** Fetch every LLM call a run made, oldest first. AgentRun status keeps only the
 *  latest routing decisions, so this is the full history of a long run. Rejects
 *  when the operator has no PostgreSQL store (HTTP 503). */
export async function getRunRoutingEvents(runId: string, namespace: string): Promise<RoutingEventInfo[]> {
  const res = await apiFetch(`/api/runs/${encodeURIComponent(namespace)}/${encodeURIComponent(runId)}/routing-events`)
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  const body = (await res.json()) as { events?: RoutingEventInfo[] }
  return body.events ?? []
}

/** Shows a routing event in the decision list the router accordion renders. */
export function routingEventToDecision(e: RoutingEventInfo): RoutingDecisionInfo {
  const reason = e.fallbackFrom
    ? `fallback from ${e.fallbackFrom}`
    : e.errorClass
      ? `${e.path ?? 'call'} (${e.errorClass})`
      : (e.path ?? '')
  return {
    model: e.model ?? e.provider,
    provider: e.provider,
    strategy: e.strategy ?? '',
    reason,
    confidence: String(e.confidence ?? 0),
    timestamp: e.createdAt,
    decisionID: e.decisionID,
    path: e.path,
    category: e.stage,
  }
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
  return normalizeCostData(await res.json())
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
  /** Identifies the decision across traces, status and routing telemetry. */
  decisionID?: string
  /** What decided: a rule path, `meta`, `meta-cache` or `fallback`. */
  path?: string
  /** The task stage the request was classified as. */
  category?: string
}

/** One LLM call from routing telemetry (`GET /api/runs/{ns}/{name}/routing-events`). */
export interface RoutingEventInfo {
  decisionID: string
  parentDecisionID?: string
  provider: string
  model?: string
  strategy?: string
  path?: string
  stage?: string
  confidence?: number
  promptTokens?: number
  completionTokens?: number
  costUSD?: number
  latencyMS?: number
  ttftMS?: number
  finishReason?: string
  fallbackFrom?: string
  errorClass?: string
  toolCalls?: number
  toolErrors?: number
  createdAt?: string
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
  /** Every decision recorded; `routingDecisions` holds only the latest ones. */
  routingDecisionsTotal?: number
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

/** One day of spend, split by run outcome (the run phase). */
export interface CostDay {
  date: string
  usd: string
  byOutcome: Record<string, string>
}

export interface CostData {
  totalUSD: string
  byAgent: Record<string, string>
  /** A run's whole spend is attributed to the model of its last routing decision. */
  byModel: Record<string, string>
  byDay: CostDay[]
  /** Spend by run phase (Succeeded, Failed, Running, ...). */
  byOutcome: Record<string, string>
  /** Finished runs whose spend could not be captured (shown as untracked, never as $0). */
  untrackedCount: number
  /** `namespace/name` of a few untracked runs. */
  untrackedSample: string[]
  /** Spend recorded on runs that have not finished. */
  inFlightUSD: string
  /** Mean spend of finished runs whose spend was captured. */
  avgRunUSD: string
  /** Spend on runs that ended Failed. */
  failedRunUSD: string
  /** The run archive could not be read: totals cover live runs only. */
  archiveUnavailable: boolean
}

/** A CostData with no spend, for loading fallbacks and tests. */
export function emptyCostData(): CostData {
  return {
    totalUSD: '0.0000',
    byAgent: {},
    byModel: {},
    byDay: [],
    byOutcome: {},
    untrackedCount: 0,
    untrackedSample: [],
    inFlightUSD: '0.0000',
    avgRunUSD: '0.0000',
    failedRunUSD: '0.0000',
    archiveUnavailable: false,
  }
}

function asUSD(v: unknown, fallback = '0.0000'): string {
  if (typeof v === 'number' && Number.isFinite(v)) return v.toFixed(4)
  if (typeof v === 'string' && v !== '' && Number.isFinite(Number(v))) return v
  return fallback
}

function asUSDMap(v: unknown): Record<string, string> {
  const out: Record<string, string> = {}
  if (v && typeof v === 'object' && !Array.isArray(v)) {
    for (const [k, val] of Object.entries(v as Record<string, unknown>)) out[k] = asUSD(val)
  }
  return out
}

/**
 * Coerces an /api/costs payload into a CostData, tolerating an operator that
 * predates a field, a null map or array (Go serialises nil as null), and
 * non-numeric amounts, so the dashboard never crashes on a partial response.
 */
export function normalizeCostData(raw: unknown): CostData {
  const r = (raw && typeof raw === 'object' ? raw : {}) as Record<string, unknown>
  const days = Array.isArray(r.byDay) ? r.byDay : []
  return {
    totalUSD: asUSD(r.totalUSD),
    byAgent: asUSDMap(r.byAgent),
    byModel: asUSDMap(r.byModel),
    byDay: days
      .filter((d): d is Record<string, unknown> => !!d && typeof d === 'object' && typeof (d as { date?: unknown }).date === 'string')
      .map((d) => ({ date: d.date as string, usd: asUSD(d.usd), byOutcome: asUSDMap(d.byOutcome) })),
    byOutcome: asUSDMap(r.byOutcome),
    untrackedCount: typeof r.untrackedCount === 'number' && r.untrackedCount > 0 ? r.untrackedCount : 0,
    untrackedSample: Array.isArray(r.untrackedSample) ? r.untrackedSample.filter((x): x is string => typeof x === 'string') : [],
    inFlightUSD: asUSD(r.inFlightUSD),
    avgRunUSD: asUSD(r.avgRunUSD),
    failedRunUSD: asUSD(r.failedRunUSD),
    archiveUnavailable: r.archiveUnavailable === true,
  }
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

/** One run of a step. A step inside a loop has one per pass. */
export interface WorkflowStepPass {
  pass: number
  agentRunRef: string
  phase: string
  output?: string
  spendUSD?: string
}

/** A loop: the steps that repeat, what ends it, and how far along it is. */
export interface WorkflowLoopSummary {
  id: string
  gate: string
  steps: string[]
  until: string
  maxIterations: number
  /** 1-based pass the loop is on. */
  pass: number
  state: 'pending' | 'running' | 'repeating' | 'satisfied' | 'exhausted'
}

export interface WorkflowStepSummary {
  name: string
  type?: string
  agentRef: string
  input?: string
  phase: string
  agentRunRef?: string
  output?: string
  spendUSD?: string
  failureReason?: string
  startTime?: string
  completionTime?: string
  /** 0 on the first pass; set once a loop has sent the step back. */
  iteration?: number
  /** Id of the loop this step repeats in, if any. */
  loop?: string
  /** The output is from the previous pass: the step is queued to run again. */
  staleOutput?: boolean
  passes?: WorkflowStepPass[]
}

export interface AgentWorkflowDetail extends AgentWorkflowSummary {
  steps: WorkflowStepSummary[]
  loops?: WorkflowLoopSummary[]
  /** The shared sandbox never came up or was lost; retry "sandbox" to provision it again. */
  sandboxFailed?: boolean
  /** Phase of the workflow's shared sandbox: Provisioning until its checkout is done. */
  sandboxPhase?: string
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

/** Runs a failed workflow step again. A workflow that already ended is revived;
 *  a failed loop gate restarts its loop. */
export async function retryWorkflowStep(namespace: string, name: string, step: string): Promise<void> {
  const res = await apiFetch(
    `/api/workflows/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/steps/${encodeURIComponent(step)}/retry`,
    { method: 'POST' },
  )
  if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`)
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
  /** Archived execution trace entries from the Redis token stream, rendered
   *  with the same TraceAccordion component as the live runs tab. Present
   *  only when the run was archived while the Redis stream was still
   *  available (within 24h of completion). */
  traceEntries?: TraceEntry[]
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

// ── Approvals (G2) ───────────────────────────────────────────────────────────

/** What a sandbox asked for, as stored in an egress approval's details. */
export interface EgressRequestDetails {
  sandbox?: string
  chunkID?: string
  rule?: string
  hosts?: string[]
  binaries?: string[]
  rationale?: string
  securityNotes?: string
  validation?: string
  hits?: number
  lastSeen?: string
}

/** What a paused run asks for, as stored in a budget approval's details. */
export interface BudgetRequestDetails {
  raise?: number
  spentUSD?: string
  capUSD?: string
  raiseByUSD?: string
  newCapUSD?: string
  description?: string
}

/** What a workflow's approval step asks, as stored in its approval's details. */
export interface WorkflowGateDetails {
  workflow?: string
  step?: string
  message?: string
}

/** What a gated tool call asks for, as stored in a tool approval's details. */
export interface ToolRequestDetails {
  tool?: string
  sideEffect?: string
  arguments?: string
  agent?: string
}

/** A request for a decision: egress a sandbox's policy does not allow (kind
 *  "egress"), or a higher spend cap for a run that reached its budget ("budget"), or a tool call
 *  whose side-effect class needs an approver ("tool"). */
export interface Approval {
  id: string
  namespace: string
  runName: string
  kind: string
  summary: string
  details?: EgressRequestDetails & BudgetRequestDetails & ToolRequestDetails & WorkflowGateDetails
  approverGroups: string[]
  status: 'pending' | 'approved' | 'rejected' | 'expired' | 'withdrawn'
  requestedAt: string
  expiresAt: string
  decidedBy?: string
  decidedAt?: string
  reason?: string
  appliedAt?: string
}

/** Lists approvals in the caller's namespaces. status '' lists every status. */
export async function listApprovals(status = 'pending'): Promise<Approval[]> {
  const res = await apiFetch(`/api/approvals?status=${encodeURIComponent(status)}`)
  if (!res.ok) throw new Error(res.status === 503 ? 'not configured' : `HTTP ${res.status}`)
  const body = await res.json()
  return body.approvals ?? []
}

/** Approves or rejects a pending approval as the signed-in user. */
export async function decideApproval(id: string, action: 'approve' | 'reject', reason: string): Promise<Approval> {
  const res = await apiFetch(`/api/approvals/${encodeURIComponent(id)}/${action}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ reason }),
  })
  if (!res.ok) {
    const text = (await res.text()).trim()
    throw new Error(text || `HTTP ${res.status}`)
  }
  return res.json()
}
