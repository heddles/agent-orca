/**
 * MarketplaceView — placeholder for the marketplace feature.
 * Coming soon!
 */
import React, { type CSSProperties } from 'react'
import { Icon, ICON } from '../lib/icons'

export function MarketplaceView() {
  return (
    <div style={s.root}>
      <div style={s.content}>
        <div style={s.icon}><Icon icon={ICON.marketplace} size={64} strokeWidth={1} /></div>
        <h2 style={s.title}>Marketplace</h2>
        <p style={s.subtitle}>
          Browse and install pre-built agents, tools, and MCP servers.
          <br />
          Coming soon!
        </p>
      </div>
    </div>
  )
}

const s: Record<string, CSSProperties> = {
  root: {
    flex: 1,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    padding: 32,
    background: 'var(--ds-bg)',
  },
  content: {
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'center',
    gap: 16,
    color: 'var(--ds-text-muted)',
    textAlign: 'center',
  },
  icon: {
    fontSize: 64,
    opacity: 0.3,
  },
  title: {
    fontSize: 24,
    fontWeight: 600,
    color: 'var(--ds-text-primary)',
    margin: 0,
  },
  subtitle: {
    fontSize: 14,
    color: 'var(--ds-text-secondary)',
    lineHeight: 1.6,
  },
}