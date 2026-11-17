/**
 * The builder's test panel: pick an input, run the saved agent, and watch the
 * run (status, output and trace) next to the form.
 */
import { useEffect, useState } from 'react'
import { getAgentManifest, type AgentManifest } from '../api/catalog'
import { ds } from '../lib/designSystem'
import { field } from './BuilderParts'
import { RunForm } from './AgentPage'
import { RunView } from './RunView'

interface Props {
  namespace: string
  name: string
  /** False until the agent exists in the cluster. */
  saved: boolean
  /** True when the form differs from the saved agent. */
  unsavedChanges: boolean
  /** Changes with every save, so the panel reads the new version's input form. */
  version: string
}

export function BuilderTestPanel({ namespace, name, saved, unsavedChanges, version }: Props) {
  const [manifest, setManifest] = useState<AgentManifest | null>(null)
  const [error, setError] = useState('')
  const [runName, setRunName] = useState<string | null>(null)

  useEffect(() => {
    if (!saved) return
    let live = true
    getAgentManifest(namespace, name)
      .then((m) => { if (live) { setManifest(m); setError('') } })
      .catch((e: unknown) => { if (live) setError(e instanceof Error ? e.message : String(e)) })
    return () => { live = false }
  }, [saved, namespace, name, version])

  return (
    <section aria-labelledby="test-heading" style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
      <h2 id="test-heading" style={{ margin: 0, fontSize: 15, fontWeight: 600, color: ds.textPrimary }}>Try it</h2>
      <p style={field.help}>
        A test runs the last saved version of this agent, not what is on the screen.
        {saved && unsavedChanges && ' You have unsaved changes, so save first to test them.'}
      </p>
      {!saved ? (
        <p role="status" style={field.help}>Save the agent first. Then you can try it here.</p>
      ) : error ? (
        <p role="alert" style={field.error}>The test form could not be loaded: {error}</p>
      ) : !manifest ? (
        <p role="status" style={field.help}>Loading the test form…</p>
      ) : (
        <RunForm
          key={version}
          namespace={namespace}
          name={name}
          schema={manifest.input_schema}
          examples={manifest.example_inputs ?? []}
          onStarted={setRunName}
        />
      )}
      {runName && (
        <div aria-label="Test run" style={{ borderTop: '1px solid var(--ds-border)', paddingTop: 10 }}>
          <RunView key={runName} runId={runName} namespace={namespace} />
        </div>
      )}
    </section>
  )
}
