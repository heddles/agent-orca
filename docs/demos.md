# Demo Catalog

Seven self-contained demos, each deployed as an independent Helm chart under
`charts/demos/`. Every demo requires the agent-orc platform and model-providers to be
running first, but is otherwise independent — install only what you need, remove it
cleanly when done.

## Prerequisites (all demos)

### Path 1 — Skaffold (recommended for development)

```bash
# 1. Export API keys (model-providers chart reads these at deploy time)
export ANTHROPIC_API_KEY=sk-ant-...
export OPENAI_API_KEY=sk-...
export GOOGLE_API_KEY=AIza...

# 2. Start the platform (operator + model-router + UI) and deploy model-providers:
skaffold dev -p dev
#   Deploys: agent-orc, agent-orc-resources, model-providers
#   Starts local llama-cpp servers with Metal GPU acceleration (macOS)

# 3. Deploy any demo profile:
skaffold run -p demo-soc-triage
skaffold run -p demo-escalation-chain
# etc.
```

### Path 2 — Helm installs (CI / production-like)

Install in the same order Skaffold uses:

```bash
# 1. Platform operator + model-router + UI
helm install agent-orc charts/agent-orc -n agent-orc-system --create-namespace \
  --set operator.image.pullPolicy=IfNotPresent \
  --set webhook.certManager=true

# 2. Platform resources (llama-cpp, base agents)
helm install agent-orc-resources charts/agent-orc-resources -n agent-orc-system

# 3. Model providers + selectors (required by all demos)
helm install model-providers charts/model-providers -n agent-orc-system \
  --set providerSecrets[0].apiKey=$ANTHROPIC_API_KEY \
  --set providerSecrets[1].apiKey=$OPENAI_API_KEY \
  --set providerSecrets[2].apiKey=$GOOGLE_API_KEY

# 4. Deploy a demo
helm install demo-soc-triage charts/demos/demo-soc-triage -n agent-orc-system
```

UI available at [http://localhost:8080](http://localhost:8080) (skaffold port-forwards automatically; for helm installs, run `kubectl port-forward svc/agent-orc-ui 8080:80 -n agent-orc-system`).

---

## Demo overview

| Chart | Deployment | What it shows | Required ModelSelector |
|---|---|---|---|
| `demo-mcp-apps` | `ops-advisor-chat` | MCP Apps · sandboxed iframe UI · `allowApps` gate · KV-cached HTML · multi-server | `default` |
| `demo-financial-analysis` | `financial-analysis-chat` | Interactive MCP Apps · RAG + MCP pipeline · Canvas chart with live sliders · multi-factor forecast | `default`, `embeddings` |
| `demo-soc-triage` | `soc-triage` | Multi-agent pipeline · MCP Apps · RAG · `_clarify` · sandboxed incident card | `default`, `embeddings` |
| `demo-escalation-chain` | `platform-deploys` | Child-run restrictions · `_fail` → `_clarify` escalation · continuation runs | `coding` |
| `demo-parallel-swarm` | `research-swarm` | Parallel child AgentRuns · per-agent spend · swarm synthesis | `default` |
| `demo-codebase-expert` | `codebase-expert` | GitHub MCP · RAG KnowledgeBase · live code search · `_rag_ingest` back-fill | `codebase-expert`, `codebase-expert-embeddings` + GitHub PAT |
| `demo-pen-test` | `pentest-chat` | Autonomous pentesting · CVE correlation · safe vulnerability validation · executive report dashboard | `default`, `embeddings` |

---

## MCP Apps — Sandboxed Iframe UI

An ops assistant backed by two live-data MCP servers. When a tool is called, agent-orc
fetches the server's HTML dashboard via `resources/read`, caches it in Redis, and renders it
as a sandboxed iframe directly below the tool result — no page navigation required.

**What it shows:**
- **MCP App rendering**: `_meta.ui.resourceUri` in `tools/list` tells agent-orc to fetch and
  cache the tool's HTML; the UI renders a `sandbox="allow-scripts"` iframe below each result
- **Multiple MCP servers**: two independent servers (`health-mcp` and `system-monitor`) each
  provide their own tools and HTML dashboards; the agent has access to all four tools
- **`spec.allowApps` gate**: MCP Apps are disabled by default; both servers have
  `allowApps: true` set explicitly — other servers are unaffected
- **KV caching**: HTML is cached per `(mcpServer, toolName)` with a 1-hour TTL; subsequent
  calls to the same tool skip the `resources/read` round-trip
- **Security model**: opaque-origin iframe (`sandbox` without `allow-same-origin`) +
  `connect-src 'none'` CSP — the app cannot read the parent frame or make outbound requests

```
User (chat UI)
    │  POST /api/deployments/agent-orc-system/ops-advisor-chat/execute
    ▼
┌────────────────────────────────────────────────────────┐
│  ops-advisor  (AgentDeployment: ops-advisor-chat)      │
│  tools: health-mcp-check-service-health                │
│          health-mcp-list-incidents                     │
│          system-monitor-get-system-info                │
│          system-monitor-get-system-stats               │
└──────────┬─────────────────────────────┬───────────────┘
           │ MCP HTTP transport          │ MCP HTTP transport
           ▼                             ▼
┌─────────────────────────┐  ┌─────────────────────────────┐
│  health-mcp-server      │  │  system-monitor-server      │
│  (Python, HTTP)         │  │  (Python, HTTP)             │
│                         │  │                             │
│  check-service-health   │  │  get-system-info            │
│  list-incidents         │  │  get-system-stats           │
│  resources/read         │  │  resources/read             │
│  → health://dashboard   │  │  → system://monitor         │
│  → health://incidents   │  │                             │
└─────────────────────────┘  └─────────────────────────────┘
          │                             │
          └──────────┬──────────────────┘
                     ▼
           Redis KV cache → sandboxed iframe
```

### Deploy / Remove

```bash
# Skaffold
skaffold run -p demo-mcp-apps

# or Helm
helm install demo-mcp-apps charts/demos/demo-mcp-apps -n agent-orc-system

# Wait for the warm pod:
kubectl get pods -n agent-orc-system -l agentorc.io/deployment=ops-advisor-chat -w

helm uninstall demo-mcp-apps -n agent-orc-system
```

### Scenarios

**Scenario A — System status overview** *(calls `check-service-health`, renders dashboard iframe)*
> What's the current system status?

1. Agent calls `health-mcp-check-service-health` with no filter
2. Router fetches `health://dashboard` HTML via `resources/read` and caches it in Redis
3. Text summary appears in the response; the health dashboard iframe renders below the tool result
4. The dashboard shows all 6 services — `payment-service` is deliberately degraded (↑ error rate, ↑ latency)

**Scenario B — Incident report** *(calls `list-incidents`, renders incidents iframe)*
> Any P1 incidents right now?

1. Agent calls `health-mcp-list-incidents` with `severity: P1`
2. An incidents dashboard iframe renders listing the open P1 with service, age, and description

**Scenario C — Single service drill-down** *(iframe on second call served from cache)*
> How is the payment service doing?

1. Agent calls `health-mcp-check-service-health` with `service: payment-service`
2. If Scenario A was run first, the iframe is served from the Redis KV cache (no `resources/read` call to the MCP server — confirm in router logs)

**Scenario D — Cluster resource overview** *(calls `get-system-stats`, renders system monitor iframe)*
> What's the cluster resource usage?

1. Agent calls `system-monitor-get-system-stats` with no filter
2. Router fetches `system://monitor` HTML via `resources/read` and caches it in Redis
3. Text summary shows per-node CPU, memory, and disk usage; the system monitor iframe renders below with progress bars and CPU sparklines
4. `node-2` is deliberately running hot (~90% CPU) to make the dashboard interesting

**Scenario E — Node drill-down** *(calls `get-system-info` for a specific node)*
> Tell me about node-2's hardware

1. Agent calls `system-monitor-get-system-info` with `node: node-2`
2. Returns static configuration: CPU model, core count, memory capacity, disk size, role, and availability zone

**Scenario F — Cross-server query** *(agent uses tools from both MCP servers)*
> Is any node running hot on CPU? Are there related incidents?

1. Agent calls `system-monitor-get-system-stats` to check resource usage
2. Agent calls `health-mcp-list-incidents` to correlate with open incidents
3. Both iframes render — system monitor shows the hot node, incidents timeline shows related alerts

### Iframe security properties

| Property | Value |
|---|---|
| `sandbox` | `allow-scripts` (no `allow-same-origin` → opaque origin) |
| `Content-Security-Policy` | `default-src 'none'; script-src 'unsafe-inline' 'unsafe-eval'; style-src 'unsafe-inline'; img-src data:; connect-src 'none'` |
| Parent-frame access | Blocked — opaque origin cannot read parent DOM, cookies, or localStorage |
| Outbound HTTP from iframe | Blocked — `connect-src 'none'` prevents all fetch/XHR/WebSocket |
| `allowApps` on other servers | `false` (default) — only `health-mcp` and `system-monitor` opt in |

### Resources

| Name | Kind | Purpose |
|---|---|---|
| `health-mcp-server-code` | ConfigMap | Python MCP server source (`files/server.py`) |
| `health-mcp-server` | Deployment + Service | Runs the health MCP HTTP server on port 8080 |
| `health-mcp` | MCPServer | HTTP transport; `allowApps: true`; tools: `check-service-health`, `list-incidents` |
| `system-monitor-server-code` | ConfigMap | Python MCP server source (`files/system-monitor-server.py`) |
| `system-monitor-server` | Deployment + Service | Runs the system monitor MCP HTTP server on port 8080 |
| `system-monitor` | MCPServer | HTTP transport; `allowApps: true`; tools: `get-system-info`, `get-system-stats` |
| `ops-advisor` | Agent | SRE assistant; calls all four tools; `openai-compatible` HTTP runtime |
| `ops-advisor-chat` | AgentDeployment | Chat endpoint; warm pool size 1 |

---

## Financial Impact Analysis

A single question about how an event impacts downstream markets triggers a multi-step
pipeline: RAG knowledge base search → market baseline fetch → multi-factor forecast with
an interactive MCP Apps iframe. The forecast dashboard renders a Canvas-based chart with
adjustable sliders — users can change assumptions (supply shock severity, duration, demand
growth, central bank stance, geopolitical risk) and see projected price trajectories for
correlated assets recalculate live in the browser.

**What it shows:**
- **RAG + MCP Apps pipeline**: agent searches the `financial-planning-kb` KnowledgeBase for
  correlations and historical precedents, fetches current market baselines via MCP, then runs
  a multi-factor forecast whose result populates an interactive iframe dashboard
- **Interactive MCP App**: `forecast-impact` tool declares `_meta.ui.resourceUri`; the
  dashboard includes 5 sliders, a multi-line Canvas chart with P10/P50/P90 confidence bands,
  a factor sensitivity heatmap, and 5-year outlook summary cards — all recalculating client-side
  via the postMessage V2 data injection protocol
- **Multi-factor modeling**: 5 correlated assets tracked over 40 quarters (10 years) with
  correlation dampening, decay functions, and policy response modeling
- **Knowledge-enriched forecasting**: agent system prompt enforces a 5-step workflow
  (`_rag_search` 2-3x → `fetch-market-context` → `forecast-impact` → written analysis)

```
User (chat UI)
    │  "How would a poor wheat harvest affect gold prices over 5 years?"
    │  POST /api/deployments/agent-orc-system/financial-analysis-chat/execute
    ▼
┌──────────────────────────────────────────────────────────────────────┐
│  financial-analyst  (AgentDeployment: financial-analysis-chat)        │
│                                                                      │
│  1. _rag_search × 2-3 (correlations, historical shocks, macro)      │
│  2. financial-mcp-fetch-market-context (baselines)                   │
│  3. financial-mcp-forecast-impact (enriched scenario)                │
│     └→ interactive Canvas dashboard iframe                           │
│  4. Written analysis with key findings                               │
└──────────┬───────────────────────┬───────────────────────────────────┘
           │ MCP HTTP              │ RAG query
           ▼                       ▼
┌──────────────────────────┐  ┌──────────────────────────┐
│  financial-mcp-server    │  │  financial-planning-kb   │
│  (Python, HTTP)          │  │  KnowledgeBase (Qdrant)  │
│                          │  │                          │
│  forecast-impact         │  │  commodity-correlations  │
│  → financial://forecast- │  │  historical-supply-shocks│
│    dashboard (iframe)    │  │  macroeconomic-factors   │
│                          │  └──────────────────────────┘
│  fetch-market-context    │
│  → financial://market-   │
│    context (iframe)      │
└──────────────────────────┘
           │
           ▼
  Redis KV cache → sandboxed iframe with live sliders
```

### Deploy / Remove

```bash
# Skaffold
skaffold run -p demo-financial-analysis

# or Helm
helm install demo-financial-analysis charts/demos/demo-financial-analysis -n agent-orc-system

# Wait for KB ingestion and warm pod:
kubectl get knowledgebase financial-planning-kb -n agent-orc-system -w   # → Ready
kubectl get pods -n agent-orc-system -l agentorc.io/deployment=financial-analysis-chat -w

helm uninstall demo-financial-analysis -n agent-orc-system
```

### Scenarios

**Scenario A — Agricultural supply shock → gold** *(full pipeline, interactive dashboard)*
> How would a poor wheat harvest in the US and Eastern Europe affect gold prices over the next 5 years?

1. Agent calls `_rag_search("wheat price correlations gold")` → commodity correlation matrix, transmission mechanisms
2. Agent calls `_rag_search("wheat supply shock case study")` → 2010 Russian export ban, 2022 Ukraine disruption
3. Agent calls `_rag_search("central bank reaction commodity inflation gold")` → monetary policy reaction functions
4. Agent calls `financial-mcp-fetch-market-context(["wheat", "corn", "crude_oil", "gold", "usd_index"])` → current baselines; **market context iframe** renders
5. Agent calls `financial-mcp-forecast-impact(scenario="...", primary_commodity="wheat", supply_shock_severity=60, ...)` → 16KB JSON forecast; **interactive dashboard iframe** renders with:
   - Multi-line Canvas chart: wheat (amber), corn (green), oil (blue), gold (yellow), USD (gray) trajectories over 10 years with P10-P90 confidence bands
   - 5 sliders: shock severity, duration, demand growth, central bank stance, geopolitical risk
   - Factor sensitivity heatmap: 5 factors × 5 assets
   - 5-year outlook cards with projected prices and percentage changes
6. Agent presents written analysis: scenario overview, key findings, historical parallel (2010 Russian wheat crisis), critical uncertainty (central bank response)
7. User adjusts sliders — chart and cards recalculate instantly in the browser

**Scenario B — Energy shock** *(different primary commodity)*
> What happens to food prices and inflation hedges if crude oil doubles over the next year?

1. Same pipeline, but `primary_commodity="crude_oil"` — correlated assets shift to crude_oil, wheat, corn, gold, usd_index
2. RAG searches focus on energy-agriculture linkages and fertilizer cost pass-through
3. Dashboard shows oil as primary with agricultural commodities as correlated impacts

**Scenario C — Exploring assumptions** *(slider interaction)*
> After Scenario A completes, ask: "What if the Fed turns hawkish instead?"

1. User clicks the **Hawkish** button in the dashboard — chart immediately recalculates
2. Gold trajectory flips from positive to negative (hawkish policy suppresses gold)
3. USD index rises, agricultural commodities moderate
4. User can also ask the agent to re-run the forecast with explicit parameters for a new text analysis

### Interactive dashboard controls

| Control | Range | Default | Effect |
|---|---|---|---|
| Supply Shock Severity | 0-100% | From forecast | Intensity of the supply disruption |
| Disruption Duration | 1-60 months | From forecast | How long the disruption persists |
| Demand Growth Rate | -5% to 10% | From forecast | Annual global demand growth assumption |
| Central Bank Stance | Dovish / Neutral / Hawkish | From forecast | Monetary policy response (dovish → weak USD, gold positive; hawkish → strong USD, gold negative) |
| Geopolitical Risk | 0-100 | From forecast | Risk premium multiplier (0 = calm, 100 = extreme) |

### Commodity data reference

Baseline prices (synthetic, deterministic) used by the MCP server:

| Commodity | Price | Unit | Volatility (30d) |
|---|---|---|---|
| wheat | 6.50 | $/bushel | 18.5% |
| corn | 4.80 | $/bushel | 16.2% |
| soybeans | 13.20 | $/bushel | 14.8% |
| crude_oil | 78.00 | $/barrel | 22.1% |
| gold | 2,050 | $/oz | 12.3% |
| silver | 24.50 | $/oz | 24.6% |
| usd_index | 104.0 | index | 6.8% |
| us_10yr | 4.25 | % yield | 8.2% |

### Knowledge base documents

| Document | Content |
|---|---|
| `commodity-correlations.txt` | Pairwise correlations (wheat-corn: 0.85, gold-USD: -0.65, etc.) and 7-step transmission mechanism timeline |
| `historical-supply-shocks.txt` | 3 case studies: 2010 Russian wheat export ban, 2022 Ukraine conflict, 2012 US drought — with price impacts and timelines |
| `macroeconomic-factors.txt` | Inflation transmission, central bank reaction functions (dovish/neutral/hawkish), geopolitical risk tiers, demand scenarios, gold-specific factor analysis |

### Resources

| Name | Kind | Purpose |
|---|---|---|
| `financial-planning-kb-content` | ConfigMap | Source documents for RAG ingestion (3 financial reference files) |
| `financial-planning-kb` | KnowledgeBase | Qdrant-backed vector store of commodity correlations, supply shocks, and macro factors |
| `financial-mcp-server-code` | ConfigMap | Python MCP server source (`files/server.py`) |
| `financial-mcp-server` | Deployment + Service | Runs the financial MCP HTTP server on port 8080 |
| `financial-mcp` | MCPServer | HTTP transport; `allowApps: true`; tools: `forecast-impact`, `fetch-market-context` |
| `financial-analyst` | Agent | Single agent with RAG + MCP tools; 5-step workflow system prompt |
| `financial-analysis-chat` | AgentDeployment | Chat-mode deployment |

---

## SOC Triage

A single security alert triggers a three-agent pipeline: enricher → risk assessor →
orchestrator. The orchestrator uses `_clarify` to get human approval before acting on
HIGH/CRITICAL findings and calls `render-incident-report` to display a styled incident
card as a sandboxed MCP Apps iframe. Every agent runs as a separate Kubernetes pod with
individual cost tracking.

**What it shows:**
- **Agent-as-tool pattern**: orchestrator calls enricher and risk assessor as tools; executor spawns child `AgentRun` objects
- **MCP Apps (HTTP transport)**: `soc-apps` MCPServer with `allowApps: true` serves `cmdb-user-lookup`, `cmdb-ip-lookup`, and `render-incident-report`; each tool declares `_meta.ui.resourceUri` so the UI renders a sandboxed HTML panel below the result
- **HTML overlays**: `cmdb-user-lookup` → personnel directory cards; `cmdb-ip-lookup` → IP threat intel panel; `render-incident-report` → severity-badged incident card (CRITICAL/HIGH/MEDIUM/LOW)
- **RAG**: `soc-enricher-agent` searches the `soc-runbooks` KnowledgeBase (Qdrant) via `_rag_search`
- **Human-in-the-loop**: orchestrator uses `_clarify` for HIGH/CRITICAL risk; runs enter `WaitingForInput`; LOW/MEDIUM close automatically

```
User (chat UI)
    │  POST /api/deployments/agent-orc-system/soc-triage/execute
    ▼
┌──────────────────────────────────────────────────────────────────────┐
│  soc-orchestrator-agent  (AgentDeployment: chat)                     │
│  tools: soc-enricher, soc-risk-assessor, render-incident-report,     │
│         _clarify                                                      │
└──────────┬─────────────────────┬────────────────────────┬────────────┘
           │ agent-as-tool       │ agent-as-tool           │ MCP HTTP
           ▼                     ▼                         ▼
┌────────────────────┐  ┌───────────────────────┐  ┌──────────────────────┐
│  soc-enricher-     │  │  soc-risk-assessor-   │  │  soc-apps MCPServer  │
│  agent             │  │  agent                │  │  render-incident-    │
│  tools:            │  │  no tools — reasoning │  │  report              │
│    cmdb-user-lookup│  └───────────────────────┘  │  → soc://incident    │
│    cmdb-ip-lookup  │                              │    iframe            │
│    _rag_search     │                              └──────────────────────┘
└─────────┬──────────┘
          │ MCP HTTP                    │ RAG query
          ▼                             ▼
┌────────────────────────────┐   ┌─────────────────┐
│  soc-apps MCPServer        │   │  soc-runbooks   │
│  cmdb-user-lookup          │   │  KnowledgeBase  │
│    → soc://cmdb iframe     │   │  (Qdrant)       │
│  cmdb-ip-lookup            │   └─────────────────┘
│    → soc://threat-intel    │
│      iframe                │
└────────────────────────────┘
```

### Deploy / Remove

```bash
# Skaffold
skaffold run -p demo-soc-triage

# or Helm
helm install demo-soc-triage charts/demos/demo-soc-triage -n agent-orc-system

# Wait for KB ingestion and warm pod:
kubectl get knowledgebase soc-runbooks -n agent-orc-system -w   # → Ready
kubectl get pods -n agent-orc-system -l agentorc.io/deployment=soc-triage -w

helm uninstall demo-soc-triage -n agent-orc-system
```

### Scenarios

**Scenario A — Data Exfiltration via Tor** *(HIGH risk, triggers `_clarify`, CMDB + threat-intel iframes)*
> Alert: User john.doe@acme.corp downloaded 3,847 records via /api/v2/export in 4 minutes. Source IP: 198.51.100.42. Timestamp: 2026-03-23T14:32:17Z

1. Enricher calls `cmdb-user-lookup("john.doe@acme.corp")` → Sales Engineer L3, no privileged access; **personnel card iframe** renders below the tool result
2. Enricher calls `cmdb-ip-lookup("198.51.100.42")` → `TOR_EXIT_NODE`, emergingthreats + abuseipdb; **IP threat intel iframe** renders
3. Enricher calls `_rag_search("bulk data export anomaly")` → matches **Data Exfiltration via API** runbook
4. Risk assessor scores **HIGH** — TOR + bulk export + IP outside all known corporate ranges
5. Orchestrator uses `_clarify` → type **yes** → full incident report
6. Orchestrator calls `render-incident-report` → **styled incident card iframe** renders (severity badge: HIGH, decision: approved)

**Scenario B — Service Account Interactive Login** *(CRITICAL risk, incident card iframe)*
> Alert: Interactive login detected for admin-svc@acme.corp from 203.0.113.99. Auth method: password. Timestamp: 2026-03-23T09:15:03Z

1. `admin-svc@acme.corp` → CI/CD Service Account, known IPs: `10.0.0.0/8` only; interactive login always anomalous; **personnel card iframe** renders
2. `203.0.113.99` → `THREAT_INTEL_FLAGGED`, Bulletproof Hosting LLC (RU), active C2 infrastructure; **IP threat intel iframe** renders
3. Risk assessor scores **CRITICAL** → orchestrator uses `_clarify`
4. After approval, `render-incident-report` renders **CRITICAL severity incident card**

**Scenario C — Legitimate Activity** *(LOW risk, no `_clarify`)*
> Alert: sarah.chen@acme.corp ran kubectl exec on pod payments-api-7d9f8b in namespace prod. Source IP: 10.10.44.21. Timestamp: 2026-03-23T11:02:50Z

1. `sarah.chen@acme.corp` → Sr. DevOps Engineer L5, has `prod-k8s-admin`, known IPs include `10.10.0.0/16`; **personnel card iframe** renders
2. `10.10.44.21` → `CORPORATE_INTERNAL` — activity consistent with role
3. Risk assessor scores **LOW** → orchestrator outputs report directly, no human prompt
4. `render-incident-report` renders **LOW severity incident card** (auto-closed)

### Audit trail

```bash
kubectl get agentrun -n agent-orc-system \
  -l agentorc.io/deployment=soc-triage \
  --sort-by=.metadata.creationTimestamp
```

### Resources

| Name | Kind | Purpose |
|---|---|---|
| `soc-runbooks-content` | ConfigMap | Source documents for RAG ingestion |
| `soc-runbooks` | KnowledgeBase | Qdrant-backed vector store of security runbooks |
| `soc-apps-server-code` | ConfigMap | Python HTTP MCP server source (`files/server.py`) |
| `soc-apps-server` | Deployment + Service | Runs the Python MCP HTTP server on port 8080 |
| `soc-apps` | MCPServer | HTTP transport; `allowApps: true`; tools: `cmdb-user-lookup`, `cmdb-ip-lookup`, `render-incident-report` |
| `soc-apps-cmdb-user-lookup` | Tool (MCP) | Employee registry — renders personnel card iframe |
| `soc-apps-cmdb-ip-lookup` | Tool (MCP) | IP threat intel classifier — renders threat intel iframe |
| `soc-apps-render-incident-report` | Tool (MCP) | Submits structured findings — renders incident card iframe |
| `soc-enricher` | Tool (agent) | Wraps `soc-enricher-agent` as a callable tool |
| `soc-risk-assessor` | Tool (agent) | Wraps `soc-risk-assessor-agent` as a callable tool |
| `soc-enricher-agent` | Agent | Enrichment sub-agent (CMDB + RAG) |
| `soc-risk-assessor-agent` | Agent | Risk scoring sub-agent (pure reasoning) |
| `soc-orchestrator-agent` | Agent | Orchestrator with `_clarify` for HIGH/CRITICAL + `render-incident-report` |
| `soc-triage` | AgentDeployment | Chat-mode deployment, warm pool size 1 |

### CMDB reference

| Email | Role | Privilege | Known IPs |
|---|---|---|---|
| john.doe@acme.corp | Sales Engineer L3 | none | 10.50.0.0/16, 192.168.1.0/24 |
| sarah.chen@acme.corp | Sr. DevOps Eng L5 | prod-k8s-admin, aws-iam-admin | 10.10.0.0/16, 192.168.10.0/24 |
| admin-svc@acme.corp | CI/CD Service Account | ci-pipeline-deployer | 10.0.0.0/8 only |
| carlos.martinez@acme.corp | Security Analyst L4 | splunk-admin, crowdstrike-admin | 10.20.0.0/16 |

| IP | Classification |
|---|---|
| 10.x.x.x | CORPORATE_INTERNAL |
| 192.168.x.x | CORPORATE_VPN |
| 198.51.100.42 | TOR_EXIT_NODE (emergingthreats + abuseipdb) |
| 198.51.100.17 | TOR_EXIT_NODE (emergingthreats) |
| 203.0.113.5 | THREAT_INTEL_FLAGGED (C2, spamhaus + abuseipdb) |
| 203.0.113.99 | THREAT_INTEL_FLAGGED (active C2, emergingthreats + Spamhaus DROP) |
| 35.x / 52.x / 54.x | AWS_CLOUD_HOSTING |

---

## Escalation Chain

A child agent deep in a pipeline hits a decision it isn't authorized to make. Instead of
guessing or failing silently, the conflict travels up the agent hierarchy to the human —
and the human's answer travels back down. Every step is a Kubernetes resource.

**What it shows:**
- **Structural child-run restrictions**: child agents have only `_done` and `_fail`; `_clarify`, `_handoff`, and `_spawn` are withheld by the controller at the CRD level — not by prompt
- **`_fail` → `_clarify` escalation**: child calls `_fail` with a conflict → executor returns it as a tool error → parent orchestrator (which *does* have `_clarify`) surfaces the question to the human
- **Continuation run**: after the human answers, a new `AgentRun` is created with `PriorRunRef` pointing to the paused run, restoring full conversation context
- **Kubernetes audit trail**: `kubectl get agentrun` shows orchestrator → child → continuation as distinct resources with individual phases and spend

```
User (chat UI)
    │  POST /api/deployments/agent-orc-system/platform-deploys/execute
    ▼
┌──────────────────────────────────────────────┐
│  deploy-orchestrator-agent  (AgentDeployment)│
│  tools: deploy-planner, _clarify             │
└──────────────────┬───────────────────────────┘
                   │ agent-as-tool (executor spawns AgentRun)
                   ▼
        ┌──────────────────────────┐
        │  deployment-planner-agent │
        │  tools: _done, _fail only │
        │  (child run, ParentRunRef │
        │   set by executor)        │
        └──────────────────────────┘
                   │
         conflict? │ calls _fail("payments-api v2.1.3 requires
                   │  auth-svc v2.0.0 — not certified. Options: A/B")
                   ▼
        tool error returned to orchestrator
                   │
                   ▼ calls _clarify → run enters WaitingForInput
        ┌──────────────────────────┐
        │  Human answers in UI     │
        └──────────────────────────┘
                   │
                   ▼ continuation AgentRun created
        orchestrator calls deploy-planner again with decision
                   │
                   ▼ child returns JSON plan
        orchestrator presents formatted deployment summary
```

### Prerequisites

Requires the `coding` ModelSelector from the model-providers chart. Local models
routinely hallucinate a successful deployment plan instead of calling `_fail`,
breaking the escalation story — ensure `ANTHROPIC_API_KEY` is set before deploying
model-providers.

### Deploy / Remove

```bash
# Skaffold
skaffold run -p demo-escalation-chain

# or Helm
helm install demo-escalation-chain charts/demos/demo-escalation-chain -n agent-orc-system

kubectl get pods -n agent-orc-system -l agentorc.io/deployment=platform-deploys -w

helm uninstall demo-escalation-chain -n agent-orc-system
```

### Scenarios

**Scenario A — Version Conflict** *(triggers escalation)*
> Deploy payments-api v2.1.3 to production using a canary strategy.

1. Orchestrator calls `deploy-planner(task="Deploy payments-api v2.1.3 ...")`
2. Child checks registry: v2.1.3 requires `auth-svc >= v2.0.0`, but v2.0.0 isn't certified
3. Child calls `_fail` → child run transitions to **Failed**
4. Executor returns failure as tool error to orchestrator
5. Orchestrator calls `_clarify` with options A (pin to v2.1.2) and B (hold for certification)
6. Type **A** → continuation run created → orchestrator re-calls planner with decision → plan returned

After the demo, inspect the audit trail:

```bash
# All three runs: orchestrator, failed child, continuation
kubectl get agentrun -n agent-orc-system \
  -l agentorc.io/deployment=platform-deploys \
  --sort-by=.metadata.creationTimestamp

# Orchestrator run shows childRunRefs and continuationRunRef
kubectl describe agentrun <orchestrator-run> -n agent-orc-system

# Failed child run shows failureReason
kubectl describe agentrun <child-run> -n agent-orc-system
```

**Scenario B — Clean deployment** *(no escalation)*
> Deploy inventory-svc v3.3.0 to production using a canary strategy.

No compatibility constraints. Child returns a plan immediately. Orchestrator presents the
summary with no `_clarify` call — the human is only interrupted when there is a genuine
decision to be made.

**Scenario C — Decision already provided**
> Deploy payments-api v2.1.3. We've decided to pin to v2.1.2 to stay compatible with the current auth-svc.

Orchestrator passes the engineer's decision directly to `deploy-planner`. Child recognizes
Option A is already chosen, skips `_fail`, returns a plan for v2.1.2.

### Resources

| Name | Kind | Purpose |
|---|---|---|
| `deploy-planner` | Tool (agent) | Wraps `deployment-planner-agent` as a callable tool |
| `deployment-planner-agent` | Agent | Child planner — checks registry, calls `_fail` on conflicts |
| `deploy-orchestrator-agent` | Agent | Orchestrator — calls planner tool, escalates via `_clarify` |
| `platform-deploys` | AgentDeployment | Chat-mode deployment, warm pool size 1 |

### Service registry reference

Hardcoded in `deployment-planner-agent` system prompt for demo reproducibility.

| Service | Current Version | Notes |
|---|---|---|
| payments-api | v2.1.0 | v2.1.3 blocked; v2.1.2 is latest safe version |
| auth-svc | v1.9.0 | v2.0.0 pending security certification (ETA 2026-04-15) |
| inventory-svc | v3.2.1 | No pending constraints |
| notification-svc | v1.4.0 | No pending constraints |

`payments-api >= v2.1.3` requires `auth-svc >= v2.0.0` (API v3 token claims). `auth-svc v2.0.0` is not yet certified → deployment of `payments-api v2.1.3` is blocked.

---

## Parallel Research Swarm

One research question spawns three specialized child `AgentRun` objects simultaneously.
Each runs independently with its own tools, cost, and audit entry. The orchestrator waits
for all three, then synthesizes a report. Total wall time is far less than sequential.

**What it shows:**
- **Parallel child `AgentRun` objects**: orchestrator calls all three tools in a single LLM turn; executor creates the child runs concurrently
- **Per-child `spendUSD`**: each child's cost is tracked independently — `kubectl get agentrun` shows individual spend per researcher
- **`ChildRunRefs[]`**: orchestrator `AgentRun.status` lists all three child runs
- **MCP stdio**: `cve-lookup` runs an inline Python MCP server with a hardcoded CVE database

```
User (chat UI)
    │  POST /api/deployments/agent-orc-system/research-swarm/execute
    ▼
┌────────────────────────────────────────────────────────────────┐
│  research-orchestrator-agent  (AgentDeployment: chat)          │
│  Calls all 3 tools in ONE LLM turn → executor runs in parallel │
└──────────┬───────────────────┬──────────────────┬─────────────┘
           │                   │                  │
           ▼                   ▼                  ▼
┌──────────────────┐  ┌──────────────────┐  ┌─────────────────────┐
│ security-        │  │ cve-checker-     │  │ codebase-analyst-   │
│ researcher-agent │  │ agent            │  │ agent               │
│ pure reasoning   │  │ tools:           │  │ pure reasoning      │
│                  │  │   cve-lookup MCP │  │                     │
└──────────────────┘  └──────────────────┘  └─────────────────────┘
           │                   │                  │
           └───────────────────┴──────────────────┘
                               │
                   orchestrator synthesizes report
```

### Deploy / Remove

```bash
# Skaffold
skaffold run -p demo-parallel-swarm

# or Helm
helm install demo-parallel-swarm charts/demos/demo-parallel-swarm -n agent-orc-system

kubectl get pods -n agent-orc-system -l agentorc.io/deployment=research-swarm -w

helm uninstall demo-parallel-swarm -n agent-orc-system
```

### Demo question

> What are the risks of migrating our auth service from JWT to PASETO tokens?

Watch `kubectl get agentrun` as three child runs appear simultaneously. The orchestrator's
`AgentRun.status.childRunRefs` lists all three. Total wall time ≈ max(individual times),
not sum.

```bash
# Watch child runs appear in real time
kubectl get agentrun -n agent-orc-system -w

# After completion — compare per-child spend
kubectl get agentrun -n agent-orc-system \
  -l agentorc.io/deployment=research-swarm \
  --sort-by=.metadata.creationTimestamp \
  -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,SPEND:.status.spendUSD'
```

### CVE database reference

Hardcoded in the `cve-lookup` MCP sidecar for demo reproducibility.

| Query | CVEs included |
|---|---|
| `jwt` | CVE-2022-21449 (ECDSA bypass), CVE-2015-9235 (alg:none), CVE-2018-0114 (key confusion) |
| `paseto` | No CVEs — PASETO v2/v4 designed to eliminate JWT attack classes |
| `oauth2` | CVE-2023-28107 (open redirect) |
| `session` | CVE-2011-2107 (session fixation) |

### Resources

| Name | Kind | Purpose |
|---|---|---|
| `cve-lookup` | Tool (MCP) | Inline Python CVE database sidecar |
| `security-researcher-tool` | Tool (agent) | Wraps `security-researcher-agent` |
| `cve-checker-tool` | Tool (agent) | Wraps `cve-checker-agent` |
| `codebase-analyst-tool` | Tool (agent) | Wraps `codebase-analyst-agent` |
| `security-researcher-agent` | Agent | Threat model + attack vector analysis |
| `cve-checker-agent` | Agent | Calls `cve-lookup`, returns structured CVE report |
| `codebase-analyst-agent` | Agent | Migration complexity + breaking changes assessment |
| `research-orchestrator-agent` | Agent | Orchestrator — issues all 3 calls in one turn |
| `research-swarm` | AgentDeployment | Chat-mode deployment, warm pool size 1 |

---

## Codebase Expert

A persistent expert agent for the agent-orc repository. The `agent-orc-codebase`
KnowledgeBase is pre-populated via MCP ingestion on deploy and re-synced hourly — the
agent can answer architecture questions from semantic search alone, then drill into live
code via GitHub MCP tools when precision matters. Files read via MCP are back-filled into
the KB via `_rag_ingest` so the vector store grows richer with each session.

**What it shows:**
- **MCP ingestion pipeline**: `KnowledgeBase.spec.ingestion.mcp` — controller discovers repo files via `get_file_contents`, fetches content, and chunks into Qdrant automatically at deploy and on the hourly sync interval
- **`_rag_search` + GitHub MCP combination**: agent starts with semantic retrieval, falls back to live `search_code` or `get_file_contents` when RAG results are insufficient
- **`_rag_ingest` back-fill**: after reading a file via MCP the agent calls `_rag_ingest` to persist it, keeping the vector store current without a full re-sync
- **Scoped agent**: system prompt enforces strict topic scope — questions outside agent-orc are politely refused

```
User (chat UI)
    │  POST /api/deployments/agent-orc-system/codebase-expert/execute
    ▼
┌──────────────────────────────────────────────────────────────────────┐
│  codebase-expert-agent  (AgentDeployment: chat, warm pool: 1)        │
│  tools: _rag_search, _rag_ingest, github-mcp-*                       │
│                                                                      │
│  1. _rag_search(agent-orc-codebase, query)  ─────────────────────┐  │
│  2. If needed: github-mcp-search-code / get-file-contents        │  │
│  3. _rag_ingest(new content read via MCP)   ←────────────────────┘  │
│                                                                      │
│  agent-orc-codebase KnowledgeBase (Qdrant, 10 Gi)                   │
│    populated at deploy via MCP ingestion  (up to 500 files)         │
│    re-synced every 3600 s                                            │
└──────────────────────────────────────────────────────────────────────┘
```

### Prerequisites

- GitHub Personal Access Token with `repo` (read) scope for `ci-agent-orc/agent-orc`
- `codebase-expert` and `codebase-expert-embeddings` ModelSelectors (deployed by this chart) require providers from the model-providers chart (`OPENAI_API_KEY` or `ANTHROPIC_API_KEY`)

### Deploy / Remove

```bash
# Skaffold (token read from AGENT_ORC_GITHUB_TOKEN env var)
export AGENT_ORC_GITHUB_TOKEN=ghp_YOUR_TOKEN
skaffold run -p demo-codebase-expert

# or Helm
helm install demo-codebase-expert charts/demos/demo-codebase-expert -n agent-orc-system \
  --set githubToken=ghp_YOUR_TOKEN

# Wait for the KnowledgeBase ingestion job and warm pod:
kubectl get knowledgebase agent-orc-codebase -n agent-orc-system -w   # → Ready
kubectl get pods -n agent-orc-system -l agentorc.io/deployment=codebase-expert -w

helm uninstall demo-codebase-expert -n agent-orc-system
```

### Running the demo

Chat endpoint: `POST /api/deployments/agent-orc-system/codebase-expert/execute`

**Scenario 1 — Architecture overview**

> `How does the model-router sidecar intercept tool calls?`

Expected: agent calls `_rag_search` first; synthesizes an answer from indexed source files
citing specific file paths (e.g. `internal/router/router.go`).

**Scenario 2 — Live code lookup**

> `Show me the AgentRun controller reconcile loop`

Expected: agent calls `_rag_search`, then `github-mcp-get-file-contents` for
`internal/controller/agentrun_controller.go`, then `_rag_ingest` to persist the read
content, then returns an annotated explanation.

**Scenario 3 — Recent changes**

> `What changed in the last 5 commits?`

Expected: agent calls `github-mcp-list-commits` and summarises the recent diff.

### Audit trail

```bash
# Each session = one AgentRun; inspect tool calls and spend
kubectl get agentrun -n agent-orc-system \
  -l agentorc.io/deployment=codebase-expert \
  --sort-by=.metadata.creationTimestamp

# Check KnowledgeBase ingestion status
kubectl describe knowledgebase agent-orc-codebase -n agent-orc-system
```

### Resources

| Name | Kind | Purpose |
|---|---|---|
| `github-mcp-token` | Secret | GitHub PAT (repo read scope) |
| `github-mcp` | MCPServer | GitHub MCP sidecar — auto-creates Tool CRs; used by agent and KB ingestion |
| `agent-orc-codebase` | KnowledgeBase | Qdrant-backed vector store; MCP-ingested from `ci-agent-orc/agent-orc` |
| `codebase-expert` | ModelSelector | Multi-provider rule-based selector (Opus, Sonnet, Gemini, GPT-4o) |
| `codebase-expert-embeddings` | ModelSelector | Embeddings selector (OpenAI `text-embedding-3-large`, Gemini fallback) |
| `codebase-expert-agent` | Agent | Expert agent with `_rag_search`, `_rag_ingest`, and GitHub MCP tools |
| `codebase-expert` | AgentDeployment | Chat endpoint, warm pool 1 |

---

## Penetration Testing

An autonomous penetration testing agent that discovers vulnerabilities, correlates them with known CVEs, safely validates exploitability, and generates interactive executive reports. Demonstrates security assessment capabilities with guardrails to prevent unauthorized scanning.

**What it shows:**
- **Multi-MCP orchestration**: CVE lookup, network scanning, vulnerability detection, exploit validation, and report rendering MCP servers work together
- **CVE correlation**: Vulnerability findings are matched against the simulated CVE knowledge base
- **Safe exploitation**: The exploit validator performs read-only evidence collection only — NO actual exploitation is performed
- **Interactive executive dashboard**: `report-mcp-render-executive-summary` creates a risk score gauge and severity matrix
- **GuardrailPolicy**: Security controls block scanning of internal IP ranges and destructive commands
- **MCP Apps**: Each security tool renders an interactive HTML dashboard via sandboxed iframe
- **KnowledgeBase**: Findings persist in `pentest-findings-kb` for historical tracking

```
User (chat UI)
    │  POST /api/deployments/agent-orc-system/pentest-chat/execute
    ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  pentest-orchestrator  (AgentDeployment: pentest-chat)                  │
│  tools: cve-mcp-*, network-mcp-*, vuln-mcp-*, exploit-mcp-*, report-mcp-*│
└──────────┬─────────────────────┬─────────────────────┬──────────────────┘
           │ MCP HTTP            │ MCP HTTP            │ MCP HTTP
           ▼                     ▼                     ▼
┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐
│  cve-mcp        │  │  network-mcp    │  │  vuln-mcp       │  │  report-mcp     │
│  (search,       │  │  (scan-target,  │  │  (scan-host)    │  │  (render-exec   │
│   details)      │  │   enumerate)    │  │                 │  │   -summary)     │
└─────────────────┘  └────────┬────────┘  └────────┬────────┘  └─────────────────┘
                              │                     │
                              ▼                     ▼
                    ┌─────────────────┐  ┌─────────────────┐
                    │  exploit-mcp    │  │  KnowledgeBases │
                    │  (validate-vuln)│  │  cve-knowledge, │
                    │  READ-ONLY      │  │  pentest-findings│
                    └─────────────────┘  └─────────────────┘
```

### Deploy / Remove

```bash
# Skaffold
skaffold run -p demo-pen-test

# or Helm
helm install demo-pen-test charts/demos/demo-pen-test -n agent-orc-system

# Wait for the warm pod:
kubectl get pods -n agent-orc-system -l agentorc.io/deployment=pentest-chat -w

helm uninstall demo-pen-test -n agent-orc-system
```

### Demo Targets

The network and vuln MCP servers include simulated targets for safe testing (demonstration only — not real vulnerable systems):

- `web-victim.example.com` — Web server with Apache Tomcat 8.5.19 (VULNERABLE to CVE-2024-29944)
- `api-victim.example.com` — API server with nginx 1.24.0
- `db-victim.example.com` — Database server with MySQL/PostgreSQL

### Scenarios

**Scenario A — Full penetration test**
> "Perform a penetration test on web-victim.example.com"

1. Agent calls `network-mcp-scan-target` → port scan results with service detection; **network scan iframe** renders
2. Agent calls `vuln-mcp-scan-host` → vulnerability findings; **vuln scan iframe** renders with severity matrix
3. Agent calls `cve-mcp-search-cve` for "Tomcat", then `cve-mcp-get-cve-details` for CVE-2024-29944
4. Agent calls `exploit-mcp-validate-vuln` → safe validation results; **validation iframe** shows exploit evidence
5. Agent calls `report-mcp-render-executive-summary` → **executive dashboard** with risk score gauge and findings

**Scenario B — CVE research**
> "What are the details of CVE-2024-45269?"

1. Agent calls `cve-mcp-get-cve-details` with CVE ID
2. **CVE details iframe** renders with CVSS score, vector, and references

**Scenario C — Specific port scan**
> "Scan ports 22,80,443 on api-victim.example.com"

1. Agent calls `network-mcp-scan-target` with custom port list
2. **Scan results iframe** shows open/closed status per port

### Security Controls

| Control | Implementation |
|---|---|
| GuardrailPolicy blocks internal IPs | Blocks 10.x, 172.16-31.x, 192.168.x, localhost, 127.0.0.1 |
| GuardrailPolicy blocks destructive commands | Blocks `rm -rf`, `delete`, `meterpreter`, `reverse shell`, etc. |
| Exploit MCP is read-only | Only performs banner/version checks — NO actual exploitation |
| MCP Apps sandboxed | `sandbox="allow-scripts"` with CSP `connect-src 'none'` |

### Resources

| Name | Kind | Purpose |
|---|---|---|
| `cve-mcp-server` | Deployment + Service | CVE database lookup MCP server |
| `cve-mcp` | MCPServer | CVE tools with MCP Apps enabled |
| `network-mcp-server` | Deployment + Service | Network scanning MCP server |
| `network-mcp` | MCPServer | Network tools with MCP Apps enabled |
| `vuln-mcp-server` | Deployment + Service | Vulnerability scanning MCP server |
| `vuln-mcp` | MCPServer | Vuln tools with MCP Apps enabled |
| `exploit-mcp-server` | Deployment + Service | Safe validation MCP server (READ-ONLY) |
| `exploit-mcp` | MCPServer | Validation tools |
| `report-mcp-server` | Deployment + Service | Executive report MCP server |
| `report-mcp` | MCPServer | Report rendering with MCP Apps |
| `cve-knowledge-kb` | KnowledgeBase | CVE knowledge for RAG |
| `pentest-findings-kb` | KnowledgeBase | Persistent findings storage |
| `pentest-orchestrator` | Agent | Combined security agent with guardrails |
| `pentest-chat` | AgentDeployment | Chat endpoint with warm pool |
| `pentest-guardrails` | GuardrailPolicy | Security controls for authorized scanning |
