/**
 * Role gating hook.
 *
 * The API exposes no roles yet, so every section is visible to everyone. When
 * roles land, this is the ONE place to read the session's role and hide
 * sections: the sidebar and the setup banners both ask here.
 */
export type Section =
  | 'home' | 'agents' | 'runs' | 'approvals' | 'build' | 'evaluate' | 'operate' | 'settings'

// TODO(roles): replace with a check against the signed-in user's role, for
// example Operate for builders and admins, Settings for admins only.
export function canSee(_section: Section): boolean {
  return true
}

/** Whether platform setup problems (for example a missing state store) are shown. */
// TODO(roles): admins only once roles exist.
export function canSeeSetupBanners(): boolean {
  return true
}
