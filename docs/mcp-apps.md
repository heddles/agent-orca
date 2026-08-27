# MCP Apps — Sandboxed Iframe UI

MCP Apps let an MCP server embed a rich HTML panel into the agent-orca UI alongside a tool's
text result. When a tool declares `_meta.ui.resourceUri` in the `tools/list` response, the
model-router fetches that HTML once, caches it, and the UI renders it in a sandboxed iframe
below the tool result — in both the RunView trace accordion and the DeploymentView chat window.

---

## How to Enable

Set `spec.allowApps: true` on the MCPServer CRD. It defaults to `false`.

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: MCPServer
metadata:
  name: my-mcp-server
spec:
  transport: http
  url: http://my-mcp-server.default.svc:8080
  allowApps: true   # opt-in; false by default
  tools:
    - name: my-tool
      description: "..."
```

The MCP server must implement `resources/read` and respond to the URI advertised in `_meta.ui.resourceUri`:

```json
// tools/list response (from MCP server)
{
  "tools": [{
    "name": "my-tool",
    "description": "...",
    "_meta": {
      "ui": { "resourceUri": "ui://my-tool" }
    }
  }]
}
```

```json
// resources/read request (sent by router sidecar)
{ "method": "resources/read", "params": { "uri": "ui://my-tool" } }

// resources/read response (from MCP server)
{
  "contents": [{ "uri": "ui://my-tool", "mimeType": "text/html", "text": "<html>...</html>" }]
}
```

---

## Data Flow

```
tools/list          FetchResource         SaveKV             GET /mcpapp/...   <iframe>
MCP server ──► router ──────────────────► Redis KV store ◄── apiserver ◄───── browser
               (on first tool call)        ttl=1h
```

1. **Discovery**: The router's MCP client parses `_meta.ui.resourceUri` from `tools/list` and stores it on the `Tool` struct as `AppResourceURI`. `AllowApps` is propagated from `MCPServer.spec.allowApps` through the `ServerConfig → Tool → ToolDefinition` chain.

2. **Fetch on first call**: In `callMCPTool()`, after the tool result returns, the router checks `td.AllowApps && td.AppResourceURI != ""`. On a cache miss it calls `mcpClient.FetchResource()` which sends a `resources/read` JSON-RPC to the MCP server and extracts the `text` field from the first content block.

3. **Cache**: HTML is stored in Redis via `store.SaveKV("mcpapp", "{serverName}/{toolName}", html, 1h)`. The cache is **per (server, tool)** — not per run, per session, or per call. All runs that use the same tool share the same cached HTML. The 1-hour TTL lets the cache pick up MCP server redeployments.

4. **App URL**: The router sets `appUrl = "/api/runs/{namespace}/{runName}/mcpapp/{serverName}/{toolName}"` and includes it in the `tool_result` SSE trace event alongside the text result.

5. **Serve**: When the iframe loads, the browser GETs from that URL. The apiserver reads the HTML from the KV store and serves it with security headers (see [Security](#security)).

6. **Render**: The React `MCPAppFrame` component renders a sandboxed `<iframe src={appUrl}>`. In RunView, the frame appears below the tool result text in the trace accordion. In DeploymentView chat, it appears below the assistant message.

---

## Design Choices

### `spec.allowApps` defaults to `false`

MCP servers that don't serve HTML need no changes. An operator explicitly opts each server in.
This limits the blast radius of a compromised MCP server — only servers you explicitly trust
can inject HTML into the UI.

### HTML is cached per (server, tool), not per call

The HTML returned by `resources/read` is expected to be the **app shell** — a static or nearly-
static UI that will later receive tool results via postMessage (V2). It is not expected to
change per-call. Caching per (server, tool) means one Redis entry per tool, regardless of
how many runs call it or how many times each run calls it.

The TTL is 1 hour. This is short enough to pick up a new server deployment within a reasonable
time, and long enough that a busy agent doesn't re-fetch on every call.

### 1 MB size limit before save

`SaveKV` enforces a 1 MB ceiling with zstd compression. HTML + inline JS/CSS for a real dashboard
compresses to well under 100 KB in practice. If a server returns a response larger than 1 MB
uncompressed, the router logs a warning and omits `appUrl` from the trace event — the tool
result is still shown as text, and no iframe appears.

### HTML is served through the apiserver, not the router

The router runs as a sidecar inside the agent pod behind a per-run NetworkPolicy. Its HTTP port is
not reachable from the browser. The apiserver is the public ingress point for the UI, so it is
the natural place to serve cached assets. The router writes to Redis; the apiserver reads from
Redis — the same pattern used for SSE trace streaming.

### `FetchResource` runs on the existing MCP connection

`resources/read` uses the same persistent connection the router already holds for `tools/list`
and tool calls. No new network path, no new port in the NetworkPolicy. This is why the 1-hour
cache is acceptable latency — the first call to a given tool pays the fetch cost (~50–200 ms);
all subsequent calls hit Redis at ~1 ms.

---

## Security

### `spec.allowApps` gate

`false` by default. If the MCPServer doesn't set `allowApps: true`, `FetchResource` is never
called and `appUrl` never appears in trace events. An operator must explicitly trust each server.

### Sandboxed iframe

```tsx
<iframe sandbox="allow-scripts" src={appUrl} />
```

`sandbox="allow-scripts"` without `allow-same-origin` gives the iframe an **opaque origin**.
It cannot read the parent frame's DOM, cookies, localStorage, or IndexedDB. It cannot navigate
the parent frame. It cannot submit forms. It can run JavaScript, which is required for
interactive dashboards.

### CSP (nonce-based script/style, `connect-src 'none'`)

The apiserver sets the following headers when serving MCP App HTML. Each response uses a fresh
**nonce**; [`injectMCPAppCSPNonces`](../internal/apiserver/mcpapp_csp.go) rewrites the cached HTML
so every `<script>` and `<style>` tag carries that nonce. That removes **`script-src
'unsafe-inline'`** (see GitHub security issue #10) while still allowing scripts that declare the
nonce. `style-src` keeps `'unsafe-inline'` for inline style attributes on elements.

```
Content-Type: text/html; charset=utf-8
X-Content-Type-Options: nosniff
Content-Security-Policy: default-src 'none'; script-src 'nonce-<random>' 'strict-dynamic'; style-src 'nonce-<random>' 'unsafe-inline'; img-src data: blob:; font-src data:; connect-src 'none'; frame-src 'none'; base-uri 'none'
```

`connect-src 'none'` blocks all outbound `fetch`, `XMLHttpRequest`, and WebSocket calls from
within the iframe. Even if the HTML is malicious, it cannot exfiltrate anything over the network.
This makes V1 safe even for apps served by partially-trusted MCP servers.

### Threat summary

| Threat | Mitigation |
|---|---|
| Malicious HTML reads parent frame | `sandbox` without `allow-same-origin` → opaque origin |
| HTML exfiltrates data via HTTP/WS | CSP `connect-src 'none'` |
| Compromised MCP server injects app | `spec.allowApps: false` by default |
| Parent-frame navigation / takeover | No `allow-top-navigation`, no `allow-forms` |
| Oversized payload fills Redis | 1 MB size check before `SaveKV` |
| postMessage injection from other origins | V1 has no postMessage listener |

---

## V2: postMessage Data Injection

V2 pushes each tool call's arguments and result into the cached iframe via `postMessage` immediately
after the tool returns. The iframe remains a static shell (served from cache as before) but can
now react to the specific query the LLM made and render tool-specific content.

### Protocol

After the tool call completes the router includes `toolArgs` and `toolResult` in the SSE
`tool_result` trace event (only when `appUrl` is also set). The React `MCPAppFrame` component
posts this into the iframe once it loads:

```typescript
// Sent by MCPAppFrame after iframe onLoad:
iframe.contentWindow.postMessage({
  type: "mcp-app-result",
  tool: "check-service-health",
  args: { service: "payment-service" },  // parsed from toolArgs
  result: "Service Health Summary...",   // full text result
}, "*")  // "*" because sandboxed iframes have an opaque origin
```

The iframe listens and re-renders:

```javascript
window.addEventListener("message", function(e) {
  if (e.source !== window.parent) return;  // only accept from parent frame
  var d = e.data;
  if (!d || d.type !== "mcp-app-result") return;
  // use d.args and d.result to update the UI
});
```

### Security

- **No CSP changes**: `postMessage` is not subject to `connect-src`. The iframe cannot initiate
  outbound network requests (still blocked by `connect-src 'none'`).
- **`event.source` validation**: The iframe checks `e.source === window.parent` before accepting
  any message, preventing injection from other origins.
- **Parent posts to `"*"`**: Necessary because the sandboxed iframe has an opaque origin. This
  is safe — the parent is the trusted React app, and the iframe cannot read back anything.
- **Data flows parent → iframe only**: The iframe cannot call tools or exfiltrate data. It is
  a passive display surface.

### Threat summary update

| Threat | Mitigation |
|---|---|
| postMessage injection from other origins | `e.source !== window.parent` check in iframe JS |
| Iframe exfiltrates data via postMessage reply | No `targetOrigin` listener in parent for iframe messages |
| Iframe makes network calls with injected data | CSP `connect-src 'none'` still in effect |

### V3: iframe-initiated tool calls (not yet implemented)

V3 would allow the iframe to request new tool calls via postMessage (iframe → parent → router).
This requires a separate security review: tool results could be sensitive, and the blast radius
of a compromised MCP server HTML would expand significantly. V3 will need a scoped relay
endpoint, strict request validation, and possible result redaction.

---

## Key Files

| File | Role |
|---|---|
| [api/v1alpha1/mcpserver_types.go](../api/v1alpha1/mcpserver_types.go) | `spec.allowApps` field on MCPServer CRD |
| [internal/mcp/client.go](../internal/mcp/client.go) | `Tool.AppResourceURI` from `_meta.ui.resourceUri`; `FetchResource()` |
| [internal/router/config.go](../internal/router/config.go) | `MCPServerConfig.AllowApps` and `ToolDefinition.AllowApps` + `AppResourceURI` |
| [internal/router/router.go](../internal/router/router.go) | `callMCPTool()` — fetch/cache HTML, set `appUrl` in trace event |
| [internal/controller/agentrun_controller.go](../internal/controller/agentrun_controller.go) | Copies `MCPServer.Spec.AllowApps` into `MCPServerConfig` |
| [internal/apiserver/uiapi.go](../internal/apiserver/uiapi.go) | `handleMCPApp()` — serves HTML from KV store with security headers |
| [internal/apiserver/mcpapp_csp.go](../internal/apiserver/mcpapp_csp.go) | Per-response CSP nonce + HTML rewrite for `<script>` / `<style>` |
| [ui/src/api/traceStream.ts](../ui/src/api/traceStream.ts) | `tool_result` event type extended with optional `appUrl` |
| [ui/src/components/MCPAppFrame.tsx](../ui/src/components/MCPAppFrame.tsx) | Sandboxed iframe component |
| [ui/src/components/TraceAccordion.tsx](../ui/src/components/TraceAccordion.tsx) | Renders `MCPAppFrame` below tool result in RunView |
| [ui/src/components/DeploymentView.tsx](../ui/src/components/DeploymentView.tsx) | Renders `MCPAppFrame` below tool result in chat |
| [ui/src/components/ConfigDetailView.tsx](../ui/src/components/ConfigDetailView.tsx) | Shows Apps enabled/disabled in MCPServer detail panel |
