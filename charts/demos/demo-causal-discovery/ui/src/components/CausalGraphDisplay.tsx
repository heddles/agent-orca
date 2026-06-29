import { useEffect, useRef } from 'react'

interface Props {
  hypothesis: any
}

export function CausalGraphDisplay({ hypothesis }: Props) {
  const containerRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!hypothesis || !containerRef.current) return
    
    // Simple graph visualization without external libraries
    const container = containerRef.current
    container.innerHTML = ''
    
    const title = document.createElement('div')
    title.textContent = `Hypothesis: ${hypothesis.hypothesis_id || 'unknown'}`
    title.style.cssText = 'font-size: 14px; font-weight: 600; color: #e2e8f0; margin-bottom: 12px;'
    container.appendChild(title)
    
    if (hypothesis.graph && hypothesis.graph.nodes) {
      const graphContainer = document.createElement('div')
      graphContainer.style.cssText = 'display: flex; flex-wrap: wrap; gap: 8; padding: 12px; background: #0f172a; border-radius: 6px;'
      
      hypothesis.graph.nodes.forEach((node: any) => {
        const nodeEl = document.createElement('div')
        nodeEl.textContent = node.label
        nodeEl.style.cssText = 'padding: 8px 12px; background: #3b82f6; color: #fff; border-radius: 6px; font-size: 12px;'
        graphContainer.appendChild(nodeEl)
      })
      
      hypothesis.graph.edges?.forEach((edge: any) => {
        const edgeEl = document.createElement('div')
        edgeEl.textContent = `${edge.source} → ${edge.target}`
        edgeEl.style.cssText = 'padding: 6px 10px; background: #1e293b; border: 1px solid #334155; border-radius: 4px; font-size: 11px; color: #94a3b8;'
        graphContainer.appendChild(edgeEl)
      })
      
      container.appendChild(graphContainer)
    }
  }, [hypothesis])

  if (!hypothesis) {
    return (
      <div style={empty}>
        <div style={icon}>📊</div>
        <div style={text}>Enter an observational pattern to generate causal hypotheses</div>
      </div>
    )
  }

  return (
    <div style={container}>
      <div style={header}>Causal Graph Visualization</div>
      <div ref={containerRef} style={graphArea} />
    </div>
  )
}

const container: React.CSSProperties = {
  display: 'flex',
  flexDirection: 'column',
  height: '100%',
  gap: 12,
}

const header: React.CSSProperties = {
  fontSize: 13,
  fontWeight: 600,
  color: '#94a3b8',
  textTransform: 'uppercase',
  letterSpacing: '0.06em',
}

const graphArea: React.CSSProperties = {
  flex: 1,
  background: '#1e293b',
  border: '1px solid #334155',
  borderRadius: 8,
  padding: 16,
  overflow: 'auto',
}

const empty: React.CSSProperties = {
  flex: 1,
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  justifyContent: 'center',
  gap: 12,
  color: '#475569',
}

const icon: React.CSSProperties = {
  fontSize: 48,
  opacity: 0.4,
}

const text: React.CSSProperties = {
  fontSize: 14,
  textAlign: 'center',
}