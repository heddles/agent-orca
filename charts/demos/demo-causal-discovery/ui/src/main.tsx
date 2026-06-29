import { StrictMode, useState, useEffect } from 'react'
import { createRoot } from 'react-dom/client'
import { ObservationInput } from './components/ObservationInput'
import { ExpertPanelBoard } from './components/ExpertPanelBoard'
import { CausalGraphDisplay } from './components/CausalGraphDisplay'
import { EvidenceDashboard } from './components/EvidenceDashboard'
import { ControlPanel } from './components/ControlPanel'

function App() {
  const [sessionId, setSessionId] = useState<string | null>(null)
  const [hypothesis, setHypothesis] = useState<any>(null)
  const [experts, setExperts] = useState<ExpertState[]>([
    { name: 'epidemiology', active: true, confidence: 0, evidenceCount: 0, lastUpdate: '' },
    { name: 'economics', active: false, confidence: 0, evidenceCount: 0, lastUpdate: '' },
    { name: 'neuroscience', active: false, confidence: 0, evidenceCount: 0, lastUpdate: '' },
  ])
  const [evidence, setEvidence] = useState<EvidenceItem[]>([])

  useEffect(() => {
    // Generate session ID on load
    setSessionId('cd-' + Math.random().toString(36).slice(2, 10))
  }, [])

  return (
    <div style={layout.root}>
      {/* Header */}
      <header style={layout.header}>
        <h1 style={layout.title}>🔬 Causal Discovery Microscope</h1>
      </header>

      {/* Main content */}
      <div style={layout.content}>
        {/* Left panel - Input and Controls */}
        <aside style={layout.left}>
          <ObservationInput sessionId={sessionId} onHypothesis={setHypothesis} />
          <ControlPanel experts={experts} onExpertsChange={setExperts} />
        </aside>

        {/* Center - Causal Graph */}
        <main style={layout.center}>
          <CausalGraphDisplay hypothesis={hypothesis} />
        </main>

        {/* Right - Experts and Evidence */}
        <aside style={layout.right}>
          <ExpertPanelBoard experts={experts} onExpertsChange={setExperts} />
          <EvidenceDashboard evidence={evidence} />
        </aside>
      </div>
    </div>
  )
}

export interface ExpertState {
  name: string
  active: boolean
  confidence: number
  evidenceCount: number
  lastUpdate: string
}

export interface EvidenceItem {
  id: string
  source: string
  title: string
  confidence: number
  timestamp: string
  content: string
}

const layout: Record<string, React.CSSProperties> = {
  root: {
    display: 'flex',
    flexDirection: 'column',
    height: '100vh',
    background: '#0f172a',
    color: '#e2e8f0',
    fontFamily: 'system-ui, -apple-system, sans-serif',
  },
  header: {
    height: 52,
    background: '#1e293b',
    borderBottom: '1px solid #334155',
    display: 'flex',
    alignItems: 'center',
    padding: '0 20px',
    flexShrink: 0,
  },
  title: {
    fontSize: 16,
    fontWeight: 600,
    color: '#f1f5f9',
  },
  content: {
    display: 'flex',
    flex: 1,
    overflow: 'hidden',
  },
  left: {
    width: 300,
    flexShrink: 0,
    borderRight: '1px solid #334155',
    padding: 16,
    overflowY: 'auto' as const,
    display: 'flex',
    flexDirection: 'column',
    gap: 16,
  },
  center: {
    flex: 1,
    padding: 16,
    overflow: 'hidden',
  },
  right: {
    width: 340,
    flexShrink: 0,
    borderLeft: '1px solid #334155',
    padding: 16,
    overflowY: 'auto' as const,
    display: 'flex',
    flexDirection: 'column',
    gap: 16,
  },
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>
)