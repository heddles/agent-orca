/**
 * Role gating hook. This is UX only: the API enforces roles. It decides which
 * sidebar sections and banners are shown, from the roles GET /api/me returns.
 *
 *   viewer, runner  Home, Agents, Runs, Approvals, Evaluate (read only)
 *   builder         adds Build, Operate, and changing evaluation datasets
 *   admin           adds Settings
 *
 * When /api/me fails, or reports no tenant (authentication disabled, or the UI
 * proxy's service-account session), everything is shown, as before roles.
 */
import { useEffect, useState } from 'react'
import { getMe } from '../api/fleet'

export type Section =
  | 'home' | 'agents' | 'runs' | 'approvals' | 'build' | 'evaluate' | 'operate' | 'settings'

/** The lowest role that shows each section. */
const SECTION_ROLE: Record<Section, string> = {
  home: 'viewer', agents: 'viewer', runs: 'viewer', approvals: 'viewer',
  build: 'builder', evaluate: 'viewer', operate: 'builder',
  settings: 'admin',
}

/** The caller's roles, or null when unknown (show everything). */
export type Roles = string[] | null

/** Reads the caller's roles from /api/me once. Null while loading, on failure and without a tenant. */
export function useRoles(): Roles {
  const [roles, setRoles] = useState<Roles>(null)
  useEffect(() => {
    let cancelled = false
    getMe()
      .then((me) => { if (!cancelled) setRoles(me.tenant ? me.roles : null) })
      .catch(() => { if (!cancelled) setRoles(null) })
    return () => { cancelled = true }
  }, [])
  return roles
}

export function canSee(roles: Roles, section: Section): boolean {
  return roles === null || roles.includes(SECTION_ROLE[section])
}

/** Whether platform setup problems (for example a missing state store) are shown: admins only. */
export function canSeeSetupBanners(roles: Roles): boolean {
  return roles === null || roles.includes('admin')
}

/** Whether the caller may change evaluation datasets, cases and graders (the builder role). */
export function canWriteEvals(roles: Roles): boolean {
  return roles === null || roles.includes('builder')
}

/** Whether the caller may grade a result (the runner role, which builders and admins hold too). */
export function canGradeEvals(roles: Roles): boolean {
  return roles === null || roles.includes('runner')
}

/** Whether the caller may set or remove a publish gate and publish past a failing one (the admin role). */
export function canAdminEvals(roles: Roles): boolean {
  return roles === null || roles.includes('admin')
}
