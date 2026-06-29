/** Hero output display card with copy support and markdown rendering. */
import { useState } from 'react'
import Markdown from 'react-markdown'
import remarkGfm from 'remark-gfm'

interface Props {
  output: string
  streaming?: boolean
  /** If true, render output as markdown. */
  markdown?: boolean
}

export function OutputCard({ output, streaming, markdown }: Props) {
  const [copied, setCopied] = useState(false)
  const [open, setOpen] = useState(true)

  const copy = () => {
    navigator.clipboard.writeText(output).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }

  return (
    <div style={s.card}>
      <div style={s.header} onClick={() => setOpen((o) => !o)}>
        <div style={s.title}>
          {streaming ? (
            <>
              <PulseDot />
              <span>Streaming output</span>
            </>
          ) : (
            <>
              <span style={s.star}>✦</span>
              <span>AI Output</span>
            </>
          )}
        </div>
        <div style={s.actions}>
          <button style={s.iconBtn} onClick={(e) => { e.stopPropagation(); copy() }} title="Copy output">
            {copied ? '✓' : '⎘'}
          </button>
          <span style={{ ...s.chevron, transform: open ? 'rotate(90deg)' : undefined }}>▶</span>
        </div>
      </div>
      {open && (
        <div style={{ ...s.body, color: streaming ? '#94a3b8' : '#f1f5f9' }}>
          {markdown ? (
            <div style={s.mdWrap} className="ao-md">
              <Markdown remarkPlugins={[remarkGfm]}>{output}</Markdown>
            </div>
          ) : (
            <span>
              {output}
              {streaming && <span style={s.cursor}>▌</span>}
            </span>
          )}
        </div>
      )}
    </div>
  )
}

function PulseDot() {
  return (
    <span style={{
      display: 'inline-block',
      width: 7,
      height: 7,
      borderRadius: '50%',
      background: '#3b82f6',
      animation: 'aoPulse 1.5s ease infinite',
    }} />
  )
}

const s: Record<string, React.CSSProperties> = {
  card: {
    background: '#1e293b',
    border: '1px solid #334155',
    borderRadius: 12,
    display: 'flex',
    flexDirection: 'column',
    maxHeight: '50vh',
  },
  header: {
    padding: '12px 16px',
    borderBottom: '1px solid #334155',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    cursor: 'pointer',
    userSelect: 'none',
  },
  title: {
    fontSize: 11,
    fontWeight: 600,
    textTransform: 'uppercase' as const,
    letterSpacing: '0.08em',
    color: '#94a3b8',
    display: 'flex',
    alignItems: 'center',
    gap: 8,
  },
  star: { fontSize: 14 },
  actions: { display: 'flex', gap: 6, alignItems: 'center' },
  chevron: {
    fontSize: 11,
    color: '#94a3b8',
    transition: 'transform 0.2s',
  },
  iconBtn: {
    width: 28,
    height: 28,
    borderRadius: 5,
    border: '1px solid #334155',
    background: 'none',
    color: '#94a3b8',
    cursor: 'pointer',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    fontSize: 13,
  },
  body: {
    padding: 20,
    fontSize: 14,
    lineHeight: 1.7,
    fontFamily: 'system-ui, sans-serif',
    whiteSpace: 'pre-wrap' as const,
    minHeight: 80,
    flex: 1,
    overflowY: 'auto' as const,
    wordBreak: 'break-word' as const,
  },
  mdWrap: {
    color: '#f1f5f9',
    fontSize: 14,
    lineHeight: 1.7,
    wordBreak: 'break-word' as const,
  },
  cursor: {
    display: 'inline-block',
    animation: 'aoPulse 1s infinite',
  },
}
