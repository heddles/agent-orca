import type { ExpertState } from '../types'

interface Props {
  experts: ExpertState[]
  onExpertsChange: (experts: ExpertState[]) => void
}

export function ControlPanel({ experts, onExpertsChange }: Props) {
  const handleWeightChange = (name: string, weight: number) => {
    const updated = experts.map(e => 
      e.name === name ? { ...e, confidence: weight } : e
    )
    onExpertsChange(updated)
  }

  return (
    <div style={container}>
      <div style={header}>Controls</div>
      
      {experts.map(expert => (
        <div key={expert.name} style={controlRow}>
          <span style={label}>{expert.name}</span>
          <input
            type="range"
            min="0"
            max="100"
            value={expert.confidence}
            onChange={(e) => handleWeightChange(expert.name, parseInt(e.target.value))}
            style={slider}
          />
          <span style={value}>{expert.confidence}%</span>
        </div>
      ))}
      
      <button style={actionBtn}>Run Analysis</button>
      <button style={resetBtn}>Reset All</button>
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

const controlRow: React.CSSProperties = {
  display: 'flex',
  alignItems: 'center',
  gap: 8,
}

const label: React.CSSProperties = {
  width: 100,
  fontSize: 12,
  color: '#e2e8f0',
}

const slider: React.CSSProperties = {
  flex: 1,
  accentColor: '#3b82f6',
}

const value: React.CSSProperties = {
  width: 40,
  fontSize: 12,
  color: '#94a3b8',
  textAlign: 'right',
}

const actionBtn: React.CSSProperties = {
  padding: '10px 16px',
  background: '#3b82f6',
  color: '#fff',
  border: 'none',
  borderRadius: 6,
  fontSize: 13,
  fontWeight: 600,
  cursor: 'pointer',
  marginTop: 8,
}

const resetBtn: React.CSSProperties = {
  padding: '10px 16px',
  background: 'transparent',
  color: '#94a3b8',
  border: '1px solid #334155',
  borderRadius: 6,
  fontSize: 13,
  cursor: 'pointer',
}