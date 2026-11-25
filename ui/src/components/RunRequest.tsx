/** What a run was asked to do, with the channel's wrapper folded away. */
import type { CSSProperties } from 'react'
import { DESIGN, ds } from '../lib/designSystem'
import { channelLabel, parseRunInput } from '../lib/runText'

export function RunRequest({ input }: { input: string }) {
  const { message, channel, sender, subject } = parseRunInput(input)
  return (
    <section style={s.panel} aria-label="Request">
      <h2 style={s.label}>Request</h2>
      {channel && (
        <p style={s.from}>
          From {channelLabel(channel)}{sender ? ` · sender ${sender}` : ''}{subject ? ` · ${subject}` : ''}
        </p>
      )}
      <div style={s.text}>{message || input}</div>
      {channel && message && (
        <details style={s.raw}>
          <summary style={s.summary}>Full input as the agent received it</summary>
          <pre style={s.pre}>{input}</pre>
        </details>
      )}
    </section>
  )
}

const s: Record<string, CSSProperties> = {
  panel: { background: ds.surface, border: '1px solid var(--ds-border)', borderRadius: DESIGN.radii.lg, padding: '12px 16px', flexShrink: 0 },
  label: { margin: '0 0 8px', fontSize: 11, fontWeight: 600, textTransform: 'uppercase', letterSpacing: '0.08em', color: ds.textSecondary },
  from: { margin: '0 0 6px', fontSize: 12, color: ds.textMuted },
  // Long scheduled prompts stay readable without pushing the answer away.
  text: { fontSize: 14, color: ds.textPrimary, lineHeight: 1.5, whiteSpace: 'pre-wrap', wordBreak: 'break-word', maxHeight: 220, overflowY: 'auto' },
  raw: { marginTop: 8 },
  summary: { fontSize: 12, color: ds.textSecondary, cursor: 'pointer' },
  pre: { margin: '8px 0 0', fontSize: 11, color: ds.textSecondary, whiteSpace: 'pre-wrap', wordBreak: 'break-word', fontFamily: DESIGN.font.mono },
}
