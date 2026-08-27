/** Sandboxed iframe for rendering MCP App UIs, with postMessage data injection. */

import { useRef, useState, useEffect } from 'react'

/** Message sent from parent to iframe after each tool call (V2 postMessage bridge). */
interface MCPAppMessage {
  type: 'mcp-app-result'
  tool: string
  args: unknown
  result: string
}

interface Props {
  appUrl: string
  toolName: string
  /** Raw JSON arguments string from the LLM tool call. Injected into the iframe via postMessage. */
  toolArgs?: string
  /** Full (untruncated) tool result text. Injected into the iframe via postMessage. */
  toolResult?: string
  initialHeight?: number
}

export function MCPAppFrame({ appUrl, toolName, toolArgs, toolResult, initialHeight = 400 }: Props) {
  const iframeRef = useRef<HTMLIFrameElement>(null)
  const [loaded, setLoaded] = useState(false)

  // After the iframe loads (or whenever args/result change), push the tool data in via postMessage.
  // The iframe's JS listens for {type: "mcp-app-result"} and renders tool-specific content.
  // We use "*" as the target origin because the sandboxed iframe has an opaque origin.
  useEffect(() => {
    if (!loaded || !iframeRef.current?.contentWindow || !toolArgs) return
    let args: unknown
    try {
      args = JSON.parse(toolArgs)
    } catch {
      args = toolArgs
    }
    const msg: MCPAppMessage = {
      type: 'mcp-app-result',
      tool: toolName,
      args,
      result: toolResult ?? '',
    }
    iframeRef.current.contentWindow.postMessage(msg, '*')
  }, [loaded, toolArgs, toolResult, toolName])

  return (
    <iframe
      ref={iframeRef}
      src={appUrl}
      sandbox="allow-scripts"
      title={`App: ${toolName}`}
      onLoad={() => setLoaded(true)}
      style={{
        width: '100%',
        height: initialHeight,
        minHeight: 80,
        border: '1px solid var(--ds-border)',
        borderRadius: 6,
        display: 'block',
        marginTop: 8,
        background: 'var(--ds-bg)',
        resize: 'vertical',
        overflow: 'hidden',
      }}
    />
  )
}
