/**
 * MarketplaceView — placeholder for the marketplace feature.
 * Coming soon!
 */
import React, { type CSSProperties } from 'react'

export function MarketplaceView() {
  return (
    <div style={s.root}>
      <div style={s.content}>
        <div style={s.icon}>🛍️</div>
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
    background: '#0f172a',
  },
  content: {
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'center',
    gap: 16,
    color: '#64748b',
    textAlign: 'center',
  },
  icon: {
    fontSize: 64,
    opacity: 0.3,
  },
  title: {
    fontSize: 24,
    fontWeight: 600,
    color: '#f1f5f9',
    margin: 0,
  },
  subtitle: {
    fontSize: 14,
    color: '#94a3b8',
    lineHeight: 1.6,
  },
}