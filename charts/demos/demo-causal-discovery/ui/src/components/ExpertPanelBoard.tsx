import type { ExpertState } from '../types'

interface Props {
  experts: ExpertState[]
  onExpertsChange: (experts: ExpertState[]) => void
}

export function ExpertPanelBoard({ experts, onExpertsChange }: Props) {
  const toggleActive = (name: string) => {
    const updated = experts.map(e => 
      e.name === name ? { ...e, active: !e.active } : e
    )
    onExpertsChange(updated)
  }

  const updateConfidence = (name: string, value: number) => {
    const updated = experts.map(e => 
      e.name === name ? { ...e, confidence: value } : e
    )
    onExpertsChange(updated)
  }

  return (
    <div style={container}>
      <div style={header}>Domain Experts</div>
      <div style={grid}>
        {experts.map(expert => (
          <div key={expert.name} style={{
            ...card,
            ...(expert.active ? cardActive : {})
          }}>
            <div style={cardHeader}>
              <span style={expertName}>{expert.name}</span>
              <label style={toggle}>
                <input
                  type="checkbox"
                  checked={expert.active}
                  onChange={() => toggleActive(expert.name)}
                />
              </label>
            </div>
            <div style={confidenceBar}>
              <div style={{
                ...confidenceFill,
                width: `${expert.confidence}%`
              }} />
            </div>
            <div style={confidenceText}>Confidence: {expert.confidence}%</div>
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
  marginBottom: 4,
}

const grid: React.CSSProperties = {
  display: 'flex',
  flexDirection: 'column',
  gap: 10,
}

const card: React.CSSProperties = {
  padding: 12,
  background: '#1e293b',
  border: '1px solid #334155',
  borderRadius: 8,
  cursor: 'pointer',
}

const cardActive: React.CSSProperties = {
  borderColor: '#3b82f6',
  background: 'rgba(59,130,246,0.1)',
}

const cardHeader: React.CSSProperties = {
  display: 'flex',
  justifyContent: 'space-between',
  alignItems: 'center',
  marginBottom: 8,
}

const expertName: React.CSSProperties = {
  fontSize: 12,
  fontWeight: 600,
  color: '#e2e8f0',
}

const confidenceBar: React.CSSProperties = {
  width: '100%',
  height: 4,
  background: '#334155',
  borderRadius: 2,
  overflow: 'hidden',
  marginBottom: 6,
}

const confidenceFill: React.CSSProperties = {
  height: '100%',
  background: '#3b82f6',
  transition: 'width 0.3s',
}

const confidenceText: React.CSSProperties = {
  fontSize: 11,
  color: '#94a3b8',
}

const toggle: React.CSSProperties = {
  cursor: 'pointer',
}