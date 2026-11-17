/** Client for the fleet overview (GET /api/fleet) and the signed-in user (GET /api/me). */
import { apiFetch } from './sse'

export type FleetWindow = '24h' | '7d'
export type FleetHealthStatus = 'healthy' | 'degraded' | 'failing' | 'unknown'
export type FleetOwner = 'ui' | 'git' | 'unknown'

export interface FleetRow {
  name: string
  namespace: string
  maintainer: string
  published: boolean
  deployment?: string
  owner: FleetOwner
  health: { status: FleetHealthStatus; reason: string }
  runCount: number
  /** Failed over finished runs; null when none finished. */
  errorRate: number | null
  p95DurationSeconds: number | null
  spend: { usd: number; agentBudgetUSD: number | null; tenantBudgetUSD: number | null }
  pendingApprovals: number
  /** Null until evaluations exist. */
  lastEvalScore: number | null
}

export interface Fleet {
  window: FleetWindow
  since: string
  tenantSpendUSD: number
  tenantBudgetUSD: number | null
  agents: FleetRow[]
}

export interface Me {
  userId: string
  groups: string[]
  tenant: { id: string; name: string } | null
  workspaces: { name: string; namespace: string; primary: boolean }[]
  roles: string[]
}

export async function getFleet(window: FleetWindow, namespace = ''): Promise<Fleet> {
  const res = await apiFetch(`/api/fleet?window=${window}&namespace=${encodeURIComponent(namespace)}`)
  if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`)
  return res.json()
}

export async function getMe(): Promise<Me> {
  const res = await apiFetch('/api/me')
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  return res.json()
}
