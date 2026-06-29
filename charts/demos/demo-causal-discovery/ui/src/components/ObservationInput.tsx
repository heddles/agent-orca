import { useState } from 'react'

interface Props {
  sessionId: string | null
  onHypothesis: (hypothesis: any) => void
}

export function ObservationInput({ sessionId, onHypothesis }: Props) {
  const [input, setInput] = useState('')
  const [loading, setLoading] = useState(false)

  const handleSubmit = async () => {
    if (!input.trim() || !sessionId) return
    setLoading(true)
    
    try {
      const response = await fetch(`/api/causal/${sessionId}/analyze`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ pattern: input })
      })
      const result = await response.json()
      onHypothesis(result)
    } catch (err) {
      console.error('Analysis failed:', err)
    } finally {
      setLoading(false)
    }
    
    setInput('')
  }

  return (
    <div style={container}>
      <div style={label}>Observational Pattern</div>
      <textarea
        value={input}
        onChange={(e) => setInput(e.target.value)}
        placeholder="Describe what you observed: e.g., 'Higher vitamin D levels correlate with reduced respiratory infections'"
        style={textarea}
        rows={4}
      />
      <button onClick={handleSubmit} disabled={loading || !input.trim()} style={btn}>
        {loading ? 'Analyzing...' : 'Generate Hypotheses'}
      </button>
    </div>
  )
}

const container: React.CSSProperties = {
  display: 'flex',
  flexDirection: 'column',
  gap: 8,
}

const label: React.CSSProperties = {
  fontSize: 12,
  fontWeight: 600,
  color: '#94a3b8',
  textTransform: 'uppercase',
  letterSpacing: '0.06em',
}

const textarea: React.CSSProperties = {
  width: '100%',
  padding: 12,
  background: '#1e293b',
  border: '1px solid #334155',
  borderRadius: 6,
  color: '#e2e8f0',
  fontSize: 13,
  resize: 'vertical',
  outline: 'none',
}

const btn: React.CSSProperties = {
  padding: '10px 16px',
  background: '#3b82f6',
  color: '#ffffff',
  border: 'none',
  borderRadius: 6,
  fontSize: 13,
  fontWeight: 600,
  cursor: 'pointer',
}

const btnDisabled: React.CSSProperties = {
  ...btn,
  opacity: 0.5,
  cursor: 'not-allowed',
}