/**
 * AppShell — top bar, task-grouped left sidebar, setup banner and the routed
 * page. Section visibility goes through `canSee` (lib/access.ts), which reads
 * the roles from /api/me.
 */
import { useEffect, useRef, useState, type CSSProperties } from 'react'
import { Link, Outlet } from 'react-router'
import { getSystemStatus, listApprovals } from '../api/sse'
import { ds, toggleTheme, getPreferredTheme, type Theme } from '../lib/designSystem'
import { ICON, Icon } from '../lib/icons'
import { Sidebar, type SidebarGroup } from '../lib/layout'
import { canSee, canSeeSetupBanners, useRoles, type Roles } from '../lib/access'
import { useNamespace } from '../lib/namespace'
import { ROUTES } from '../lib/routes'
import logoUrl from '../../../assets/logo.png'

/** Polls the number of pending approvals for the nav badge. 0 when approvals are off. */
export function usePendingApprovalCount(): number {
  const [count, setCount] = useState(0)
  useEffect(() => {
    let cancelled = false
    const poll = () => {
      listApprovals('pending')
        .then((a) => { if (!cancelled) setCount(a.length) })
        .catch(() => { if (!cancelled) setCount(0) })
    }
    poll()
    const id = setInterval(poll, 15000)
    return () => { cancelled = true; clearInterval(id) }
  }, [])
  return count
}

export function buildSidebarGroups(pendingApprovals: number, roles: Roles = null): SidebarGroup[] {
  const work: SidebarGroup['items'] = []
  if (canSee(roles, 'home')) work.push({ to: ROUTES.home, label: 'Home', icon: ICON.home, end: true })
  if (canSee(roles, 'agents')) work.push({ to: ROUTES.agents, label: 'Agents', icon: ICON.agent })
  if (canSee(roles, 'runs')) work.push({ to: ROUTES.runs, label: 'Runs', icon: ICON.runs })
  if (canSee(roles, 'approvals')) {
    work.push({
      to: ROUTES.approvals, label: 'Approvals', icon: ICON.approval,
      badge: pendingApprovals, badgeLabel: 'pending',
    })
  }
  const create: SidebarGroup['items'] = []
  if (canSee(roles, 'build')) create.push({ to: ROUTES.build, label: 'Build', icon: ICON.tool })
  if (canSee(roles, 'evaluate')) create.push({ to: ROUTES.evaluate, label: 'Evaluate', icon: ICON.check })
  const admin: SidebarGroup['items'] = []
  if (canSee(roles, 'operate')) admin.push({ to: ROUTES.operate, label: 'Operate', icon: ICON.chart })
  if (canSee(roles, 'settings')) admin.push({ to: ROUTES.settings, label: 'Settings', icon: ICON.settings })
  return [{ items: work }, { items: create }, { items: admin }].filter((g) => g.items.length > 0)
}

function StateBanner({ onDismiss }: { onDismiss: () => void }) {
  return (
    <div role="status" style={s.banner}>
      <Icon icon={ICON.warning} size={14} ariaHidden={true} />
      <span style={{ flex: 1 }}>
        <strong>Costs and chat history are not being saved.</strong> Cost totals will show $0, and
        conversations are lost when the service restarts, until a platform admin connects a state
        store. See <Link to={ROUTES.status} style={s.bannerLink}>System status</Link> for details.
      </span>
      <button type="button" style={s.bannerDismiss} onClick={onDismiss} aria-label="Dismiss this notice">
        <Icon icon={ICON.close} size={14} />
      </button>
    </div>
  )
}

/** Closes a popup on Escape or a click outside it. */
function useDismiss(open: boolean, close: () => void) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') close() }
    const onDown = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) close() }
    document.addEventListener('keydown', onKey)
    document.addEventListener('mousedown', onDown)
    return () => {
      document.removeEventListener('keydown', onKey)
      document.removeEventListener('mousedown', onDown)
    }
  }, [open, close])
  return ref
}

/**
 * Workspace switcher. The API has no workspace list yet, so the only choice is
 * "All workspaces"; the selection is held in the namespace context that the
 * list views read, so wiring real workspaces later is a data change only.
 */
function WorkspaceSwitcher() {
  const { namespace, setNamespace } = useNamespace()
  return (
    <label style={s.switcher}>
      <span style={s.srOnly}>Workspace</span>
      <select
        value={namespace}
        onChange={(e) => setNamespace(e.target.value)}
        style={s.select}
        title="Workspaces arrive with multi-tenant support"
      >
        <option value="">All workspaces</option>
        {namespace && <option value={namespace}>{namespace}</option>}
      </select>
    </label>
  )
}

/**
 * User menu. The UI API has no "who am I" endpoint, so no name, tenant or role
 * can be shown yet; the menu offers sign-out via the OIDC logout route.
 */
function UserMenu() {
  const [open, setOpen] = useState(false)
  const ref = useDismiss(open, () => setOpen(false))
  return (
    <div ref={ref} style={{ position: 'relative' }}>
      <button
        type="button"
        style={s.iconBtn}
        aria-label="Account menu"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <span style={s.avatar} aria-hidden="true">U</span>
      </button>
      {open && (
        <div role="menu" style={s.menu}>
          <div style={s.menuNote}>Signed in. Your name and role will appear here once the platform exposes them.</div>
          {/* A full page load: /oauth/logout is served by the API, not the SPA. */}
          <a role="menuitem" href="/oauth/logout" style={s.menuItem}>Sign out</a>
        </div>
      )}
    </div>
  )
}

export function AppShell() {
  const pendingApprovals = usePendingApprovalCount()
  const roles = useRoles()
  const [showStateBanner, setShowStateBanner] = useState(false)
  const [showSkip, setShowSkip] = useState(false)
  const [theme, setTheme] = useState<Theme>(() => getPreferredTheme())

  useEffect(() => {
    getSystemStatus()
      .then((st) => { if (!st.stateConfigured) setShowStateBanner(true) })
      .catch(() => {})
  }, [])

  return (
    <div style={s.root}>
      <a href="#main-content" style={{ ...s.skip, top: showSkip ? 6 : -40 }}
         onFocus={() => setShowSkip(true)} onBlur={() => setShowSkip(false)}
      >Skip to content</a>
      <header style={s.topbar}>
        <Link to={ROUTES.home} style={s.logo} aria-label="agent-orca home">
          <img src={logoUrl} alt="agent-orca logo" style={s.logoImg} />
        </Link>
        <div style={s.topbarRight}>
          <WorkspaceSwitcher />
          <button
            type="button"
            style={s.iconBtn}
            onClick={() => setTheme(toggleTheme())}
            aria-label={theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'}
            title={theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'}
          >
            <Icon icon={theme === 'dark' ? ICON.moon : ICON.sun} size={16} />
          </button>
          <UserMenu />
        </div>
      </header>
      {showStateBanner && canSeeSetupBanners(roles) && <StateBanner onDismiss={() => setShowStateBanner(false)} />}
      <div style={s.body}>
        <Sidebar groups={buildSidebarGroups(pendingApprovals, roles)} />
        <main id="main-content" tabIndex={-1} style={s.main}>
          <Outlet />
        </main>
      </div>
    </div>
  )
}

const s: Record<string, CSSProperties> = {
  root: {
    display: 'flex', flexDirection: 'column', height: '100vh', background: ds.bg, color: ds.textPrimary,
    fontFamily: 'system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif', overflow: 'hidden',
  },
  skip: {
    position: 'absolute', top: -40, left: 6, padding: '6px 10px', fontSize: 12, fontWeight: 600,
    color: ds.textPrimary, background: ds.surface, border: '1px solid var(--ds-border)', borderRadius: 4,
    zIndex: 100, transitionProperty: 'top', transitionDuration: '0.15s', transitionTimingFunction: 'ease',
  },
  topbar: {
    height: 52, background: ds.surface, borderBottom: '1px solid var(--ds-border)', display: 'flex',
    alignItems: 'center', padding: '0 20px', gap: 8, flexShrink: 0, zIndex: 10,
  },
  logo: { display: 'flex', alignItems: 'center', marginRight: 16 },
  // The logo is a wide PNG: size by height, keep its aspect ratio.
  logoImg: { height: 28, width: 'auto', flexShrink: 0, objectFit: 'contain' },
  topbarRight: { marginLeft: 'auto', display: 'flex', alignItems: 'center', gap: 8 },
  switcher: { display: 'flex', alignItems: 'center' },
  select: {
    height: 32, borderRadius: 6, border: '1px solid var(--ds-border)', background: ds.bg,
    color: ds.textSecondary, fontSize: 12, padding: '0 8px',
  },
  srOnly: {
    position: 'absolute', width: 1, height: 1, overflow: 'hidden', clip: 'rect(0 0 0 0)', whiteSpace: 'nowrap',
  },
  iconBtn: {
    width: 32, height: 32, borderRadius: 6, display: 'flex', alignItems: 'center', justifyContent: 'center',
    cursor: 'pointer', color: ds.textSecondary, border: 'none', background: 'none',
    transitionProperty: 'background-color, color', transitionDuration: '0.15s', transitionTimingFunction: 'ease',
  },
  avatar: {
    width: 26, height: 26, borderRadius: '50%', background: 'rgba(59,130,246,.2)', color: ds.accent,
    display: 'flex', alignItems: 'center', justifyContent: 'center', fontSize: 12, fontWeight: 700,
  },
  menu: {
    position: 'absolute', right: 0, top: 38, minWidth: 220, background: ds.surface,
    border: '1px solid var(--ds-border)', borderRadius: 8, padding: 6, zIndex: 20,
    boxShadow: '0 8px 24px rgba(0,0,0,.3)',
  },
  menuNote: { padding: '6px 8px', fontSize: 12, color: ds.textMuted, lineHeight: 1.4 },
  menuItem: {
    display: 'block', padding: '7px 8px', borderRadius: 6, fontSize: 13, color: ds.textPrimary,
    textDecoration: 'none',
  },
  banner: {
    display: 'flex', alignItems: 'center', gap: 10, padding: '8px 20px', background: 'rgba(245,158,11,.10)',
    borderBottom: '1px solid var(--ds-border)', fontSize: 13, color: ds.warning, flexShrink: 0,
  },
  bannerLink: { color: 'inherit', textDecoration: 'underline' },
  bannerDismiss: { background: 'transparent', border: 'none', color: 'inherit', cursor: 'pointer', flexShrink: 0 },
  body: { display: 'flex', flex: 1, overflow: 'hidden' },
  main: { flex: 1, overflow: 'hidden', display: 'flex', flexDirection: 'column', minWidth: 0 },
}
