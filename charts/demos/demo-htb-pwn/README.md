# demo-htb-pwn — Autonomous HTB Red Team (MCP toolbelt)

A **privileged pwnbox pod** (HTB OpenVPN + a pentest toolbelt exposed as an **MCP
server**) is orchestrated by a **restricted** openai-compatible agent (Poolside LLM via the
model-router) that calls the tools over the cluster network. The agent is never
privileged — only the dedicated pwnbox pod is.

> ⚠️ **Real offensive testing against Hack The Box labs.** Only legal because HTB's
> ToS authorises pentesting inside its own lab VMs. Scope is strictly the HTB lab
> network carried by the VPN tunnel — the pwnbox pod is `privileged` with `/dev/net/tun`
> and egress locked to the HTB VPN endpoint; the agent is fully restricted. See
> `plans/blue-red-team-wargame.md` §16.

## Architecture

```
red-team namespace (label: agentorc.io/enable-privileged-pods=true  [chart-managed])
├─ Namespace  (chart creates this with the opt-in label)
├─ Secret htb-ovpn  ← your htbea.ovpn (createSecret or kubectl create secret)
├─ Deployment red-pwnbox   (privileged: NET_ADMIN + /dev/net/tun)
│     ├─ openvpn --config /etc/htb/htbea.ovpn  → tun0 (HTB lab)
│     └─ pwnbox-mcp-server.py  (MCP HTTP toolbelt: recon (masscan, subfinder, httpx,
│                                 nuclei, ffuf, …), web vuln (sqlmap/nikto/wpscan/…),
│                                 exploitation (searchsploit/fetch-exploit/run-exploit,
│                                 metasploit, hydra/john, …), AD (netexec/bloodhound/…),
│                                 cloud/forensics (trivy/checkov/gitleaks/yara/…)
│                                + shell-exploit / submit-findings)
├─ Service red-pwnbox :8080
├─ NetworkPolicy red-pwnbox-egress   (HTB VPN + DNS + K8s API only)
├─ NetworkPolicy red-commander-egress (Poolside + pwnbox svc + DNS + Redis + K8s API)
├─ MCPServer red-pwnbox-mcp  → child Tools red-pwnbox-mcp-* (allowedAgents: red-commander)
├─ Agent red-commander-agent (python:3.12-slim, openai-compatible, http mode, RESTRICTED)
└─ AgentDeployment red-commander (chat, warm pool, replicas:1)
```

The model-router (sidecar in the `red-commander` pods) discovers the MCP tools on the
pwnbox and runs the agentic tool-calling loop: LLM → `probe-target` → MCP → pwnbox
nmap over tun0 → result back to LLM → … → `submit-findings` / `_done`. Real tooling
runs **in the pwnbox pod over the tunnel**; the agent only reasons.

## Images (two variants)

| Variant | Dockerfile | Base | Size | When |
|---|---|---|---|---|
| **blackcart** (default) | `files/Dockerfile.pwnbox.blackcart` | `erdemozgen/blackcart` (BlackArch: nmap, rustscan, nuclei, httpx, subfinder, ffuf, naabu, katana, gobuster, feroxbuster, sqlmap, nikto, hydra, dirsearch, dalfox, metasploit, chisel, socat, gitleaks, …) | ~8GB | default `skaffold run` (needs a node sized for the large ingest) |
| **slim** (fallback) | `files/Dockerfile.pwnbox` | `python:3.12-slim-bookworm` + apt/release-binary/pip toolbelt (openvpn, nmap, masscan, sqlmap, nikto, hydra, whatweb, dirb, gobuster, ffuf, feroxbuster, subfinder, httpx, nuclei, gitleaks, amass, yara, netexec, smbmap, bloodhound-python, volatility3, checkov, trivy, ruby, perl, …; **searchsploit-lookup/exploitdb and metasploit/rustscan are blackcart-only** — the slim image ships no local ExploitDB (o-sec/exploitdb master is README-only), and `metasploit`/`rustscan` aren't in Debian; those tools report `TOOL MISSING` gracefully) | ~1.75GB | resource-constrained local `kind` where the 8GB blackcart ingest fails |

**Size note:** the default blackcart image is ~8GB. `kind load docker-image` ingests it
into the kind node's containerd — the node must have headroom (the earlier
`input/output error` was a storage/fullness issue during ingest, now resolved by sizing
the cluster storage). If local kind still can't ingest 8GB, build the slim variant:
```bash
# slim variant:
docker build -t red-pwnbox:slim -f charts/demos/demo-htb-pwn/files/Dockerfile.pwnbox .
kind load docker-image red-pwnbox:slim --name agent-orc-dev
# then point the chart at it: skaffold run -p demo-htb-pwn -s pwnbox.image.tag=slim
docker build -t red-pwnbox:latest -f charts/demos/demo-htb-pwn/files/Dockerfile.pwnbox.blackcart .
# deploy that image (set pwnbox.image.tag accordingly) into a cluster with a large node.

## Prerequisites

- agent-orc operator v0.2+ deployed (`skaffold dev -p dev`) with a Poolside API key.
- An HTB account + `.ovpn`.

## Quick start (local kind)

```bash
skaffold dev -p dev                                  # operator + poolside providers (leave running)
export AGENT_ORC_POOLSIDE_API_KEY="ps-..."
export AGENT_ORC_HTB_OVPN_CONFIG="$(cat ~/htb.ovpn)"      # or create the secret yourself (see below)
export AGENT_ORC_HTB_OVPN_ENDPOINT="nl.free.hackthebox.com:1194"
skaffold run -p demo-htb-pwn                         # builds slim red-pwnbox image, kind-loads, deploys to red-team
```

Drive an engagement (the agent autonomously calls the pwnbox MCP tools):
```bash
# Send a mission to the long-running red-commander deployment:
curl -s -X POST http://localhost:8080/api/deployments/red-team/red-commander/execute \
  -H 'Content-Type: application/json' \
  -d '{"input":"Begin engagement: enumerate 10.10.x.x (HTB lab via your tunnel), find a foothold, capture the user and root flags. Submit each flag via submit-findings, then _done."}'
```

## Managing the HTB secret

Prefer creating it yourself (never in values/skaffold env):
```bash
kubectl -n red-team create secret generic htb-ovpn --from-file=htbea.ovpn=~/htb.ovpn
# then: skaffold run -p demo-htb-pwn -s openvpn.createSecret=false
```

## Observe

```bash
kubectl -n red-team get pods
kubectl -n red-team logs deploy/red-pwnbox -c pwnbox -f        # OpenVPN + MCP telemetry
kubectl -n red-team logs deploy/red-commander -c agent -f      # LLM tool calls
kubectl -n red-team get pods -l app=red-pwnbox -o jsonpath='{.items[0].spec.containers[0].securityContext}'
#   -> privileged:true, NET_ADMIN, runAsUser:0  + /dev/net/tun mounted
kubectl -n red-team get pods -l agentdeployment.agentorc.io=red-commander -o jsonpath='{.items[0].spec.containers[0].securityContext}'
#   -> restricted (runAsNonRoot, readOnlyRootFilesystem, DROP ALL)
```

## Troubleshooting

### KnowledgeBase (red-tactics) pod stuck / "listing collections … produced zero addresses"
The operator auto-deploys a Qdrant StatefulSet per KB and waits for its `/readyz`
readiness probe before creating collections (so it no longer error-churns against a
not-yet-ready pod). If it still won't come up:

1. **StorageClass** — kind needs a default SC to bind the Qdrant PVC (`qdrant-storage`).
   Check `kubectl get storageclass` and the PVC: `kubectl -n red-team get pvc`. On bare
   kind, enable a default SC or use `kind`’s hostPath provisioner.
2. **Memory** — Qdrant defaults to 512Mi req / **1Gi limit** (wirable). On a
   memory-constrained kind node it can OOM-restart before `/readyz`. Override it:
   ```yaml
   agent-orc-resources:
     knowledgeBases:
       - name: red-tactics
         vectorStore:
           storageSize: 1Gi
           resources: { requests: {memory: 512Mi, cpu: 100m}, limits: {memory: 2Gi} }
   ```
3. Inspect: `kubectl -n red-team describe pod -l agentorc.io/knowledgebase=red-tactics`
   (look for `OOMKilled`, `ImagePullBackOff`, or `unbound PVC`) and
   `kubectl -n red-team logs -l agentorc.io/knowledgebase=red-tactics`.

### pwnbox container exits immediately (exit code 1, "Starting OpenVPN…")
This was the upstream cause of the KB 143-churn: a crashing pwnbox Deployment never
becomes ready → skaffold's stabilise check fails → it uninstalls the release → the
operator GC's the KB StatefulSet (SIGTERM / exit 143). The entrypoint is now
**best-effort: retries OpenVPN 3×, prints `/tmp/openvpn.log`, and ALWAYS starts the MCP
server**, so the pod stays up and you can inspect the failure:
```bash
kubectl -n red-team logs deploy/red-pwnbox -c pwnbox -f   # shows the openvpn log tail
```
Common reasons `openvpn` exits 1:
- **Empty/invalid `.ovpn`** — the chart injects `AGENT_ORC_HTB_OVPN_CONFIG`; if unset, the
  `htb-ovpn` Secret value is empty. Prefer creating the secret directly:
  `kubectl -n red-team create secret generic htb-ovpn --from-file=htbea.ovpn=~/htb.ovpn`
  then `skaffold run -p demo-htb-pwn -s openvpn.createSecret=false`.
- **DNS** — the pwnbox egress NP allows DNS (53); confirm the OVPN `remote` hostname resolves.
- **Auth/TLS** — some HTB profiles need `auth-nocache` or bundled certs; fix the `.ovpn`.

### MCP server "BrokenPipeError" / scans return no result
Two things were fixed in `pwnbox-mcp-server.py`: the server is now **threaded**
(`ThreadingHTTPServer`) so a long `nmap` no longer blocks `/healthz`, and writes swallow
`BrokenPipeError`. **Deeper:** the model-router's `ToolExecutionTimeoutSec` now defaults to
**1h (3600s)**; the pwnbox server mirrors this — each tool's `timeout` arg is clamped into
`[1, TOOL_TIMEOUT=3600]` (default `DEFAULT_TOOL_TIMEOUT=300s`), so a tool that overruns its
requested window returns graceful partial output + a TIMEOUT note instead of being killed
mid-output. Keep scans targeted (one host + port list per call) so they finish well under
the window; tools also report "tun0 not up" clearly if the tunnel is down.
```

## Cleanup

```bash
helm uninstall demo-htb-pwn -n red-team
kubectl delete namespace red-team
docker rmi red-pwnbox 2>/dev/null
```
