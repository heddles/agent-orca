/**
 * Layout primitives: page header, section, empty state, sidebar, master-detail.
 * Built on the design-system tokens; the app shell and the pages that were
 * touched for the shell use them instead of per-file style maps.
 */
import type { CSSProperties, ReactNode } from 'react'
import { NavLink } from 'react-router'
import { ds } from './designSystem'
import { Icon, type IconComponent } from './icons'

// ── PageHeader ───────────────────────────────────────────────────────────────

export interface PageHeaderProps {
  title: string
  /** One plain-language sentence saying what this page is for. */
  description?: string
  /** Right-aligned actions (buttons, filters). */
  actions?: ReactNode
}

export function PageHeader({ title, description, actions }: PageHeaderProps) {
  return (
    <header style={s.pageHeader}>
      <div style={{ minWidth: 0 }}>
        <h1 style={s.pageTitle}>{title}</h1>
        {description && <p style={s.pageDescription}>{description}</p>}
      </div>
      {actions && <div style={s.pageActions}>{actions}</div>}
    </header>
  )
}

// ── Section ──────────────────────────────────────────────────────────────────

export function Section({ title, children, actions }: { title?: string; children: ReactNode; actions?: ReactNode }) {
  return (
    <section style={s.section}>
      {(title || actions) && (
        <div style={s.sectionHead}>
          {title && <h2 style={s.sectionTitle}>{title}</h2>}
          {actions}
        </div>
      )}
      {children}
    </section>
  )
}

/** Scrolling page body with consistent padding. */
export function Page({ children }: { children: ReactNode }) {
  return <div style={s.page}>{children}</div>
}

// ── EmptyState ───────────────────────────────────────────────────────────────

export interface EmptyStateProps {
  icon?: IconComponent
  title: string
  /** What the user can do next. */
  children?: ReactNode
  action?: ReactNode
}

export function EmptyState({ icon, title, children, action }: EmptyStateProps) {
  return (
    <div style={s.empty}>
      {icon && <div style={s.emptyIcon}><Icon icon={icon} size={32} strokeWidth={1.25} /></div>}
      <div style={s.emptyTitle}>{title}</div>
      {children && <div style={s.emptyBody}>{children}</div>}
      {action}
    </div>
  )
}

// ── Sidebar ──────────────────────────────────────────────────────────────────

export interface SidebarItem {
  to: string
  label: string
  icon: IconComponent
  /** Mark active for every path below `to`, not only an exact match. */
  end?: boolean
  /** Count shown as a badge; hidden at 0. */
  badge?: number
  /** Extra text for screen readers when a badge is shown, e.g. "pending". */
  badgeLabel?: string
}

export interface SidebarGroup {
  label?: string
  items: SidebarItem[]
}

export function Sidebar({ groups, label = 'Main' }: { groups: SidebarGroup[]; label?: string }) {
  return (
    <nav aria-label={label} style={s.sidebar}>
      {groups.map((g, gi) => (
        <div key={g.label ?? gi} style={s.sidebarGroup}>
          {g.label && <div style={s.sidebarGroupLabel}>{g.label}</div>}
          {g.items.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              style={({ isActive }) => ({ ...s.navItem, ...(isActive ? s.navItemActive : {}) })}
              aria-label={item.badge ? `${item.label}, ${item.badge} ${item.badgeLabel ?? 'new'}` : undefined}
            >
              <Icon icon={item.icon} size={15} ariaHidden={true} />
              <span style={{ flex: 1 }}>{item.label}</span>
              {item.badge ? <span style={s.badge}>{item.badge}</span> : null}
            </NavLink>
          ))}
        </div>
      ))}
    </nav>
  )
}

// ── Sub-navigation (tabs rendered as links) ──────────────────────────────────

export function SubNav({ items, label }: { items: { to: string; label: string; end?: boolean }[]; label: string }) {
  return (
    <nav aria-label={label} style={s.subNav}>
      {items.map((i) => (
        <NavLink
          key={i.to}
          to={i.to}
          end={i.end}
          style={({ isActive }) => ({ ...s.subNavItem, ...(isActive ? s.subNavItemActive : {}) })}
        >
          {i.label}
        </NavLink>
      ))}
    </nav>
  )
}

// ── Master-detail ────────────────────────────────────────────────────────────

/** A list pane on the left and the selected item on the right. */
export function MasterDetail({ title, list, children }: { title: string; list: ReactNode; children: ReactNode }) {
  return (
    <div style={s.masterDetail}>
      <aside data-sidebar aria-label={title} style={s.listPane}>
        <div style={s.listHeader}><span style={s.listTitle}>{title}</span></div>
        <div style={s.listScroll}>{list}</div>
      </aside>
      <div style={s.detail}>{children}</div>
    </div>
  )
}

/** A small heading used between stacked lists inside a list pane. */
export function ListGroupHeading({ children }: { children: ReactNode }) {
  return <div style={s.listGroupHeading}>{children}</div>
}

const s: Record<string, CSSProperties> = {
  pageHeader: {
    display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 16,
    padding: '20px 24px 12px', flexShrink: 0,
  },
  pageTitle: { margin: 0, fontSize: 20, fontWeight: 700, color: ds.textPrimary, letterSpacing: '-0.2px' },
  pageDescription: { margin: '4px 0 0', fontSize: 13, color: ds.textSecondary, maxWidth: 640 },
  pageActions: { display: 'flex', gap: 8, alignItems: 'center', flexShrink: 0 },
  page: { flex: 1, overflowY: 'auto', display: 'flex', flexDirection: 'column' },
  section: { padding: '8px 24px 16px' },
  sectionHead: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 10 },
  sectionTitle: {
    margin: 0, fontSize: 11, fontWeight: 600, textTransform: 'uppercase',
    letterSpacing: '0.08em', color: ds.textSecondary,
  },
  empty: {
    flex: 1, display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center',
    gap: 10, padding: 32, textAlign: 'center', color: ds.textMuted,
  },
  emptyIcon: { opacity: 0.5 },
  emptyTitle: { fontSize: 15, fontWeight: 600, color: ds.textPrimary },
  emptyBody: { fontSize: 13, color: ds.textSecondary, maxWidth: 440, lineHeight: 1.5 },
  sidebar: {
    width: 212, flexShrink: 0, borderRight: '1px solid var(--ds-border)', background: ds.surface,
    padding: '12px 8px', display: 'flex', flexDirection: 'column', gap: 14, overflowY: 'auto',
  },
  sidebarGroup: { display: 'flex', flexDirection: 'column', gap: 2 },
  sidebarGroupLabel: {
    padding: '0 10px 4px', fontSize: 10, fontWeight: 600, textTransform: 'uppercase',
    letterSpacing: '0.08em', color: ds.textMuted,
  },
  navItem: {
    display: 'flex', alignItems: 'center', gap: 10, padding: '7px 10px', borderRadius: 6,
    fontSize: 13, fontWeight: 500, color: ds.textSecondary, textDecoration: 'none',
    transitionProperty: 'background-color, color', transitionDuration: '0.15s', transitionTimingFunction: 'ease',
  },
  navItemActive: { color: ds.accent, background: 'rgba(59,130,246,.15)' },
  badge: {
    minWidth: 18, padding: '0 5px', borderRadius: 9, background: ds.warning, color: '#111',
    fontSize: 11, fontWeight: 700, lineHeight: '18px', textAlign: 'center', fontVariantNumeric: 'tabular-nums',
  },
  subNav: { display: 'flex', gap: 4, padding: '0 24px', borderBottom: '1px solid var(--ds-border)', flexShrink: 0 },
  subNavItem: {
    padding: '8px 12px', fontSize: 13, fontWeight: 500, color: ds.textSecondary, textDecoration: 'none',
    borderBottom: '2px solid transparent', marginBottom: -1,
  },
  subNavItemActive: { color: ds.accent, borderBottom: '2px solid var(--ds-accent)' },
  masterDetail: { flex: 1, display: 'flex', overflow: 'hidden', minHeight: 0 },
  listPane: {
    width: 260, flexShrink: 0, borderRight: '1px solid var(--ds-border)', display: 'flex',
    flexDirection: 'column', overflow: 'hidden',
  },
  listHeader: { padding: '12px 14px 8px', borderBottom: '1px solid var(--ds-border)' },
  listTitle: {
    fontSize: 11, fontWeight: 600, textTransform: 'uppercase', letterSpacing: '0.08em', color: ds.textSecondary,
  },
  listScroll: { flex: 1, overflowY: 'auto' },
  detail: { flex: 1, overflow: 'hidden', display: 'flex', flexDirection: 'column', minWidth: 0 },
  listGroupHeading: {
    padding: '10px 14px 4px', fontSize: 10, fontWeight: 600, textTransform: 'uppercase',
    letterSpacing: '0.08em', color: ds.textMuted,
  },
}
