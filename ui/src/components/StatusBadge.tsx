/** Reusable phase badge for AgentRun, AgentDeployment, and AgentWorkflow.
 * Uses a consistent icon per phase (redundant cue alongside color — better-accessibility §9). */
import { PHASE_STYLE } from '../lib/phaseColors'
import { StatusIcon } from '../lib/icons'

interface Props {
  phase: string
  /** Optional pulse animation for running states. */
  pulse?: boolean
}


// Phase display labels — no emoji, just clean text
const PHASE_LABEL: Record<string, string> = {
  Succeeded: 'Succeeded',
  Running: 'Running',
  Failed: 'Failed',
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
    fontVariantNumeric: 'tabular-nums',
    ...(PHASE_STYLE[phase] ?? { background: 'var(--ds-muted-bg)', color: 'var(--ds-text-secondary)' }),
  }

  const isRunning = phase === 'Running' || phase === 'Creating'
  const showPulse = pulse !== false && isRunning

  return (
    <span style={style}>
      {showPulse && isRunning ? <PulseDot /> : <StatusIcon phase={phase} spinning={showPulse && isRunning} />}
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
      background: 'var(--ds-accent)',
      animation: 'aoPulse 1.5s ease infinite',
      willChange: 'transform, opacity',
    }} />
  )
}
