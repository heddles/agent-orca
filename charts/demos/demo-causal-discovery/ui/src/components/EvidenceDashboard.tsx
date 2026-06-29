import type { EvidenceItem } from '../types'

interface Props {
  evidence: EvidenceItem[]
}

export function EvidenceDashboard({ evidence }: Props) {
  if (evidence.length === 0) {
    return (
      <div style={empty}>
        <div style={header}>Evidence Dashboard</div>
        <div style={placeholder}>No evidence collected yet</div>
      </div>
    )
  }

  return (
    <div style={container}>
      <div style={header}>Evidence Dashboard ({evidence.length})</div>
      <div style={list}>
        {evidence.map(item => (
          <div key={item.id} style={card}>
            <div style={cardHeader}>
              <span style={source}>{item.source}</span>
              <span style={{
                ...confidenceBadge,
                background: item.confidence > 0.7 ? '#10b981' : item.confidence > 0.4 ? '#f59e0b' : '#ef4444'
              }}>
                {Math.round(item.confidence * 100)}%
              </span>
            </div>
            <div style={title}>{item.title}</div>
            <div style={content}>{item.content}</div>
            <div style={timestamp}>{item.timestamp}</div>
          </div>
        ))}
      </div>
    </div>
  )
}

const container: React.CSSProperties = {
  display: 'flex',
  flexDirection: 'column',
  gap: 12,
}

const header: React.CSSProperties = {
  fontSize: 13,
  fontWeight: 600,
  color: '#94a3b8',
  textTransform: 'uppercase',
  letterSpacing: '0.06em',
}

const list: React.CSSProperties = {
  display: 'flex',
  flexDirection: 'column',
  gap: 10,
  maxHeight: '100%',
  overflowY: 'auto' as const,
}

const card: React.CSSProperties = {
  padding: 12,
  background: '#1e293b',
  border: '1px solid #334155',
  borderRadius: 8,
  fontSize: 12,
}

const cardHeader: React.CSSProperties = {
  display: 'flex',
  justifyContent: 'space-between',
  alignItems: 'center',
  marginBottom: 8,
}

const source: React.CSSProperties = {
  fontSize: 11,
  color: '#64748b',
  textTransform: 'uppercase',
  letterSpacing: '0.06em',
}

const confidenceBadge: React.CSSProperties = {
  padding: '2px 8px',
  borderRadius: 4,
  fontSize: 11,
  fontWeight: 600,
  color: '#fff',
}

const title: React.CSSProperties = {
  fontSize: 13,
  fontWeight: 600,
  color: '#e2e8f0',
  marginBottom: 6,
}

const content: React.CSSProperties = {
  fontSize: 11,
  color: '#94a3b8',
  marginBottom: 8,
}

const timestamp: React.CSSProperties = {
  fontSize: 10,
  color: '#64748b',
}

const empty: React.CSSProperties = {
  display: 'flex',
  flexDirection: 'column',
  gap: 12,
}

const placeholder: React.CSSProperties = {
  padding: 20,
  textAlign: 'center',
  color: '#475569',
  fontSize: 13,
}