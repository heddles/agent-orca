/** Reusable phase badge for AgentRun, AgentDeployment, and AgentWorkflow. */
import { PHASE_STYLE } from '../lib/phaseColors'

interface Props {
  phase: string
  /** Optional pulse animation for running states. */
  pulse?: boolean
}


const PHASE_LABEL: Record<string, string> = {
  Succeeded: '✓ Succeeded',
  Running: 'Running',
  Failed: '✕ Failed',
  Pending: 'Pending',
  Skipped: 'Skipped',
  Creating: 'Creating',
  Paused: 'Paused',
  Cancelled: 'Cancelled',
  HandedOff: 'Handed Off',
  WaitingForInput: 'Waiting for Input',
}

export function StatusBadge({ phase, pulse }: Props) {
  const style: React.CSSProperties = {
    display: 'inline-flex',
    alignItems: 'center',
    gap: 4,
    padding: '2px 8px',
    borderRadius: 4,
    fontSize: 11,
    fontWeight: 600,
    letterSpacing: '0.03em',
    ...(PHASE_STYLE[phase] ?? { background: 'rgba(148,163,184,.1)', color: '#94a3b8' }),
  }

  const isRunning = phase === 'Running' || phase === 'Creating'
  const showPulse = pulse !== false && isRunning

  return (
    <span style={style}>
      {showPulse && <PulseDot />}
      {PHASE_LABEL[phase] ?? phase}
    </span>
  )
}

function PulseDot() {
  return (
    <span style={{
      display: 'inline-block',
      width: 6,
      height: 6,
      borderRadius: '50%',
      background: '#3b82f6',
      animation: 'aoPulse 1.5s ease infinite',
    }} />
  )
}
