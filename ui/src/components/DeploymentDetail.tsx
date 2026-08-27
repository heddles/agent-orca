import { useEffect, useState } from 'react'
import { getDeployment, type AgentDeploymentDetail } from '../api/sse'

interface Props {
  namespace: string
  name: string
}

const PHASE_COLORS: Record<string, string> = {
  Creating: 'var(--ds-warning)',
  Running: 'var(--ds-success)',
  Failed: 'var(--ds-error)',
  Paused: 'var(--ds-text-muted)',
}

export function DeploymentDetail({ namespace, name }: Props) {
  const [detail, setDetail] = useState<AgentDeploymentDetail | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    const load = () =>
      getDeployment(namespace, name)
        .then(setDetail)
        .catch((e: Error) => setError(e.message))
    load()
    const id = setInterval(load, 5_000)
    return () => clearInterval(id)
  }, [namespace, name])

  if (error) return <div style={styles.error}>{error}</div>
  if (!detail) return <div style={styles.loading}>Loading...</div>

  return (
    <div style={styles.container}>
      <div style={styles.header}>
        <span style={styles.title}>{detail.name}</span>
        <span style={{
          ...styles.phase,
          background: PHASE_COLORS[detail.phase] ?? '#6b7280',
        }}>
          {detail.phase}
        </span>
      </div>

      <div style={styles.grid}>
        <div style={styles.card}>
          <div style={styles.cardLabel}>Agent</div>
          <div style={styles.cardValue}>{detail.agentRef}</div>
        </div>
        <div style={styles.card}>
          <div style={styles.cardLabel}>Namespace</div>
          <div style={styles.cardValue}>{detail.namespace}</div>
        </div>
        <div style={styles.card}>
          <div style={styles.cardLabel}>Ready Replicas</div>
          <div style={styles.cardValue}>{detail.readyReplicas} / {detail.availableReplicas}</div>
        </div>
        {detail.inputSourceType && (
          <div style={styles.card}>
            <div style={styles.cardLabel}>Input Source</div>
            <div style={styles.cardValue}>{detail.inputSourceType}</div>
          </div>
        )}
        <div style={styles.card}>
          <div style={styles.cardLabel}>Consecutive Failures</div>
          <div style={{
            ...styles.cardValue,
            color: detail.consecutiveFailures > 0 ? 'var(--ds-error)' : 'var(--ds-success)',
          }}>
            {detail.consecutiveFailures}
          </div>
        </div>
        {detail.lastUpdateTime && (
          <div style={styles.card}>
            <div style={styles.cardLabel}>Last Updated</div>
            <div style={styles.cardValue}>{new Date(detail.lastUpdateTime).toLocaleString()}</div>
          </div>
        )}
        {(detail.maxContextTokens ?? 0) > 0 && (
          <div style={styles.card}>
            <div style={styles.cardLabel}>Context</div>
            <div style={styles.cardValue}>
              {detail.contextUsedTokens?.toLocaleString() ?? 0} / {detail.maxContextTokens?.toLocaleString() ?? '—'} tokens
            </div>
          </div>
        )}
      </div>

      {detail.message && (
        <div style={styles.messageBox}>
          <div style={styles.cardLabel}>Status Message</div>
          <div style={styles.message}>{detail.message}</div>
        </div>
      )}
    </div>
  )
}

const styles: Record<string, React.CSSProperties> = {
  container: {
    padding: 24,
    height: '100%',
    overflowY: 'auto',
    fontFamily: 'system-ui, sans-serif',
  },
  header: {
    display: 'flex',
    alignItems: 'center',
    gap: 12,
    marginBottom: 24,
  },
  title: {
    fontSize: 20,
    fontWeight: 700,
    color: 'var(--ds-text-primary)',
  },
  phase: {
    fontSize: 11,
    padding: '3px 10px',
    borderRadius: 9999,
    color: '#fff',
    fontWeight: 700,
    textTransform: 'uppercase',
  },
  grid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(auto-fill, minmax(200px, 1fr))',
    gap: 12,
    marginBottom: 20,
  },
  card: {
    background: 'var(--ds-surface)',
    borderRadius: 8,
    padding: '14px 16px',
  },
  cardLabel: {
    fontSize: 11,
    fontWeight: 600,
    color: 'var(--ds-text-muted)',
    textTransform: 'uppercase',
    letterSpacing: '0.04em',
    marginBottom: 6,
  },
  cardValue: {
    fontSize: 15,
    fontWeight: 600,
    color: 'var(--ds-text-primary)',
  },
  messageBox: {
    background: 'var(--ds-surface)',
    borderRadius: 8,
    padding: '14px 16px',
  },
  message: {
    fontSize: 13,
    color: 'var(--ds-text-secondary)',
    lineHeight: 1.5,
  },
  loading: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    height: '100%',
    color: 'var(--ds-text-muted)',
    fontSize: 14,
  },
  error: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    height: '100%',
    color: 'var(--ds-error)',
    fontSize: 14,
  },
}
