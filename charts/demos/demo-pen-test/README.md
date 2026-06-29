# Penetration Testing Demo

An autonomous penetration testing agent that discovers vulnerabilities, correlates them with CVEs, safely validates findings, and generates interactive executive reports.

## What it shows

- **MCP Apps UI**: Each security tool renders an interactive HTML dashboard in the agent-orc UI via sandboxed iframes
- **Multi-server orchestration**: CVE, network, vulnerability, and exploit validators work together
- **KnowledgeBase integration**: Findings persist in `pentest-findings-kb` for historical tracking
- **GuardrailPolicy**: Security controls prevent scanning unauthorized targets
- **Deterministic workflow**: `AgentWorkflow` orchestrates the penetration testing pipeline
- **Safe exploitation**: The exploit validator performs read-only evidence collection ONLY

## Security Warning

**This agent demonstrates penetration testing capabilities. ONLY scan systems you have explicit authorization to test.** The guardrail policy blocks internal IP ranges and destructive commands by default.

## Architecture

```
User (Chat UI)
    │  "Scan web-victim.example.com for vulnerabilities"
    ▼
┌─────────────────────────────────────────────────────────────────┐
│  pentest-orchestrator (AgentDeployment: pentest-chat)            │
│  tools: cve-mcp-*, network-mcp-*, vuln-mcp-*, exploit-mcp-*,   │
│         report-mcp-*                                             │
└──────────┬─────────────────────┬─────────────────────┬──────────┘
           │ MCP HTTP            │ MCP HTTP            │ MCP HTTP
           ▼                     ▼                     ▼
┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐
│  cve-mcp        │  │  network-mcp    │  │  vuln-mcp       │
│  (search,       │  │  (scan-target,  │  │  (scan-host)    │
│   details)      │  │   enumerate)    │  │                 │
└─────────────────┘  └─────────────────┘  └────────┬────────┘
                                                    │
                                                    ▼
                                            ┌─────────────────┐
                                            │  exploit-mcp    │
                                            │  (validate-vuln)│
                                            │  READ-ONLY      │
                                            └────────┬────────┘
                                                     │
                                                     ▼
                                            ┌─────────────────┐
                                            │  report-mcp     │
                                            │  (render-exec   │
                                            │   -summary)      │
                                            └─────────────────┘
                                                     │
                                                     ▼
                                          Interactive MCP Apps iframe
                                          - Risk score gauge
                                          - Findings by severity
                                          - Remediation matrix
```

## Deploy / Remove

```bash
# Skaffold
skaffold run -p demo-pen-test

# or Helm
helm install demo-pen-test charts/demos/demo-pen-test -n agent-orc-system

# Wait for the warm pod:
kubectl get pods -n agent-orc-system -l agentorc.io/deployment=pentest-chat -w
```

## Demo Targets

The network and vuln MCP servers include simulated targets for safe testing:

- `web-victim.example.com` - Web server with Apache Tomcat 8.5.19 (VULNERABLE)
- `api-victim.example.com` - API server with nginx 1.24.0 (check version)
- `db-victim.example.com` - Database server with MySQL/PostgreSQL (exposed ports)

**These are demonstration targets only - not real vulnerable systems.**

## Scenarios

### Scenario A — Full penetration test
> "Perform a penetration test on web-victim.example.com"

1. Agent calls `network-mcp-scan-target` → port scan results, iframe shows open ports
2. Agent calls `vuln-mcp-scan-host` → vulnerability findings, iframe shows severity matrix
3. Agent calls `cve-mcp-search-cve` for Apache Tomcat, `cve-mcp-get-cve-details` for CVE-2024-29944
4. Agent calls `exploit-mcp-validate-vuln` → safe validation, iframe shows evidence
5. Agent calls `report-mcp-render-executive-summary` → executive dashboard with risk gauge

### Scenario B — CVE research
> "What are the details of CVE-2024-45269?"

1. Agent calls `cve-mcp-get-cve-details` with CVE ID
2. CVE details iframe renders with CVSS, vector string, and references

### Scenario C — Specific port scan
> "Scan ports 22,80,443 on api-victim.example.com"

1. Agent calls `network-mcp-scan-target` with specific ports
2. Scan results iframe shows open/closed status with service detection

## Safety Controls

| Control | Implementation |
|---|---|
| GuardrailPolicy blocks internal IPs | `pentest-guardrails` blocks 10.x, 172.16-31.x, 192.168.x, localhost |
| GuardrailPolicy blocks destructive commands | Blocks `rm -rf`, `delete`, `meterpreter`, etc. |
| Exploit MCP is read-only | Only performs banner/version checks |
| MCP Apps sandboxed | `sandbox="allow-scripts"` with CSP `connect-src 'none'` |

## Resources Created

| Name | Kind | Purpose |
|---|---|---|
| `cve-mcp-server` | Deployment + Service | CVE database lookup MCP server |
| `cve-mcp` | MCPServer | CVE tools with MCP Apps enabled |
| `network-mcp-server` | Deployment + Service | Network scanning MCP server |
| `network-mcp` | MCPServer | Network tools with MCP Apps enabled |
| `vuln-mcp-server` | Deployment + Service | Vulnerability scanning MCP server |
| `vuln-mcp` | MCPServer | Vuln tools with MCP Apps enabled |
| `exploit-mcp-server` | Deployment + Service | Safe validation MCP server |
| `exploit-mcp` | MCPServer | Validation tools (READ-ONLY) |
| `report-mcp-server` | Deployment + Service | Executive report MCP server |
| `report-mcp` | MCPServer | Report rendering with MCP Apps |
| `cve-knowledge-kb` | KnowledgeBase | CVE knowledge for RAG |
| `pentest-findings-kb` | KnowledgeBase | Persistent findings storage |
| `pentest-orchestrator` | Agent | Combined security agent |
| `pentest-chat` | AgentDeployment | Chat endpoint |
| `pentest-guardrails` | GuardrailPolicy | Security controls |