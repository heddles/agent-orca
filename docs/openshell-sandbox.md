# Sandboxed agents (NVIDIA OpenShell)

An Agent with `spec.runtime.sandbox` runs its agent process in an
[NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell) sandbox instead of a plain
pod. The agent gets a real shell: it can run builds and tests and install the tools
it needs. The sandbox confines what it can touch:

- **Filesystem**: Landlock. The workdir and `/tmp` are writable; system paths are read-only.
- **Syscalls**: seccomp, with a non-root user and no capabilities.
- **Network**: every outbound connection goes through a supervisor that allows only the
  hosts you list, with HTTP method/path checks per host and optionally per binary.

The model-router is unchanged. It runs in its own pod (split-pod topology) and remains
the only path to LLMs, so routing, fallbacks, compaction, quotas and guardrails work
as before. Provider API keys never enter the sandbox.

## Prerequisites

1. **Agent Sandbox** controller and CRDs (the Kubernetes SIG project that OpenShell's
   Kubernetes driver uses):

   ```shell
   kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/latest/download/sandbox.yaml
   ```

2. **OpenShell gateway**, v0.1.5 (the version the operator's SDK is pinned to):

   ```shell
   helm upgrade --install openshell oci://ghcr.io/nvidia/openshell/helm-chart \
     --version 0.1.5 --namespace openshell --create-namespace \
     --set server.telemetryEnabled=false \
     --set server.drivers.kubernetes.allowDriverConfig=true
   ```

   `allowDriverConfig` lets gateway clients set Kubernetes pod settings on a sandbox
   (node selector, tolerations, runtime class). agent-orca uses it only for the
   operator-wide [placement](#placement), which only cluster admins set; Agents can't.
   Enable it when you configure placement. The operator should be the gateway's only
   client, because any client holding a gateway certificate gets the same power.
   Persistent-volume attachments still need the gateway's resource-admission labels.

3. **Kernel**: Landlock ABI v3+ (Linux 6.2+) on the nodes that run sandboxes. agent-orca
   sets `landlock.compatibility: hard_requirement`, so a sandbox on an older kernel fails
   rather than silently running without filesystem rules.

4. **CNI**: one that enforces NetworkPolicy (Calico, Cilium, ...). Both OpenShell's
   workload fence and agent-orca's router policies depend on it.

## Enable it in agent-orca

Give the operator a gateway client certificate. The OpenShell chart generates one in
`openshell-client-tls`; copy it to the agent-orca namespace:

```shell
kubectl get secret openshell-client-tls -n openshell -o json \
  | jq 'del(.metadata.namespace,.metadata.uid,.metadata.resourceVersion,.metadata.creationTimestamp,.metadata.ownerReferences)' \
  | kubectl apply -n agent-orca-system -f -
```

Then set the chart values:

```yaml
openshell:
  enabled: true
  gateway:
    address: openshell.openshell.svc.cluster.local:8080
    namespace: openshell
  workspace: default
  sandboxNamespace: openshell   # the gateway chart's server.sandboxNamespace
  tls:
    secretName: openshell-client-tls
```

For an OIDC gateway, set `openshell.tokenSecret.name` to a Secret holding a bearer token
instead of (or in addition to) the client certificate.

### Placement

By default, sandbox pods schedule wherever the cluster puts them. Builds and tests are
heavy, so give sandboxes their own node pool. Make sure they can't land on the
control plane: an untainted control-plane node is a valid target. Then point the
operator at the pool:

```yaml
openshell:
  placement:
    nodeSelector:
      agentorca.io/sandbox: "true"
    tolerations:
      - {key: agentorca.io/sandbox, operator: Exists, effect: NoSchedule}
    runtimeClassName: ""     # e.g. a gVisor or Kata RuntimeClass
    priorityClassName: ""
```

Placement applies to every sandboxed agent and requires the gateway's
`allowDriverConfig` (see Prerequisites). A gateway without it rejects every sandbox
while placement is set. Placement is operator configuration rather than an Agent field
because a toleration can admit a pod to any tainted node.

### Resources

An Agent's `spec.resources` becomes the sandbox workload's container resources, as for
an agent pod. CPU and memory requests default to their limits when unset, and other
resources such as `ephemeral-storage` are passed through.

Finally, opt in each namespace that may run sandboxed agents:

```shell
kubectl label namespace agents agentorca.io/enable-sandboxes=true
```

## Write a sandboxed Agent

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: Agent
metadata:
  name: coder
  namespace: agents
spec:
  modelSelectorRef: default
  guardrailPolicyRef: default        # required for sandboxed agents
  networkIsolation:
    splitPod: true                   # required: the router stays in its own pod
  runtime:
    ociRef: ghcr.io/acme/coding-agent:1.4
    command: ["python", "-m", "agent"]
    sandbox:
      provider: openshell
      enforcement: audit             # start in audit; switch to enforce when ready
      egress:
        - host: pypi.org             # pip index (read-only: GET/HEAD)
        - host: files.pythonhosted.org
        - host: github.com           # git clone over HTTPS POSTs, so read-write
          access: read-write
          binaries: [/usr/bin/git]   # also matches git's child helper processes
        - host: "*.githubusercontent.com"
      tools:                         # the Agent's tools are reachable by default
        excludeTools: [jira]         # ...except these Tools
        excludeHosts: [uploads.github.com]  # ...and these hosts, whichever tool declares them
      filesystem:
        readOnly: [/opt/venv]        # image paths outside the default set
```

| Field | Default | Meaning |
|---|---|---|
| `provider` | `openshell` | Sandbox runtime. Only `openshell` exists today. |
| `enforcement` | `audit` | `audit` logs HTTP method/path violations and lets them through. `enforce` blocks them. Hosts not listed in `egress` are always denied, in either mode. |
| `egress[].host` | — | Hostname, or a wildcard with at least three labels (`*.example.com`). |
| `egress[].ports` | `[443]` | Destination ports. |
| `egress[].access` | `read-only` | `read-only` (GET/HEAD/OPTIONS), `read-write` (adds POST/PUT/PATCH), or `full`. |
| `egress[].binaries` | any binary | Restrict the rule to these executables (absolute paths or globs). A rule matches the process that connects or any of its parents. List the real path, not a symlink. For scripts, list the interpreter. |
| `tools.inherit` | `true` | Add the Agent's tool endpoints to the sandbox egress (see below). Set `false` to keep tools reachable only through the model-router. |
| `tools.excludeTools` | — | Tool names from `spec.tools` whose endpoints are not added. Unknown names are rejected at admission. |
| `tools.excludeHosts` | — | Hosts that are not added from any tool. Exact match; does not filter `egress`. |
| `filesystem.readOnly` / `readWrite` | — | Extra paths on top of the defaults: the workdir, read-only `/usr`, `/lib`, `/etc`, `/app`, `/proc` and `/var/log`, and read-write `/tmp`. |

HTTPS to listed hosts is decrypted and checked by the supervisor. OpenShell injects its CA
into the usual trust variables (`SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `NODE_EXTRA_CA_CERTS`,
`GIT_SSL_CAINFO`, ...), so pip, npm, curl and git work unchanged.

### Tool endpoints

The agent's LLM tool calls always go through the model-router and are unaffected by
the sandbox. In addition, by default the agent process can reach its tools' endpoints
directly, for example to use a CLI or SDK against the same API. For each Tool in
`spec.tools` (builtins like `_clarify` excluded), the sandbox gets one rule,
`agentorca-tool-<name>`, allowing full HTTP access to:

- each `networkEgress` entry that names a host (TCP);
- the `networkEgress` of the MCPServer the tool was generated from;
- the URL of a remote (`http`/`sse`) MCP tool.

Port-only `networkEgress` entries are skipped, because sandbox rules are per host. Loopback
URLs are skipped too: they point at sidecars in the router pod. Stdio MCP tools and agent
tools have no endpoint.

### Authenticated tools

Remote MCP tools with static secret auth (`mcpConfig.auth.bearerToken`, `apiKey` or
`headers`) can be called directly from the sandbox with their credentials, without
the agent ever holding the secret. The operator registers each credential with the
OpenShell gateway as a provider, and the sandbox policy binds it to that tool's MCP
endpoint alone:

- The workload's environment gets an opaque placeholder per secret, named
  `AGENTORCA_<SOURCE>_TOKEN` for a bearer token, `AGENTORCA_<SOURCE>_API_KEY` for an
  API key, or `AGENTORCA_<SOURCE>_<HEADER>` for a custom header. `<SOURCE>` is the
  MCPServer the tool came from (its tools share one credential) or else the tool's
  name, upper-cased with non-alphanumerics replaced by `_`.
- When the agent sends a request carrying a placeholder to the tool's host and port,
  the supervisor substitutes the real value. Sent anywhere else, the request is
  refused with `403 credential_endpoint_mismatch` and logged. Exfiltrating the
  placeholder yields nothing.
- `AGENTORCA_TOOL_CREDENTIALS` describes them (no secrets), so an agent can build its
  MCP clients generically:

  ```json
  [{"tool":"github","url":"https://api.githubcopilot.com/mcp/",
    "headers":{"Authorization":"Bearer ${AGENTORCA_GITHUB_TOKEN}"}}]
  ```

  Expand `${VAR}` from the environment and send the header as-is.

Each run gets its own provider (`agentorca-run-<hash>`) holding the secret values, read
from the tool's Secrets when the sandbox starts. The provider is deleted when the run ends.
Each credential source also gets an endpointless provider profile
(`agentorca-tool-<hash>`) that declares its env var names. Profiles hold no secrets and
are shared across runs. The operator's gateway identity therefore needs permission to
import profiles and to create and delete providers.

Narrowing applies here too: excluding a tool (`excludeTools`) or its MCP host
(`excludeHosts`) also withholds its credential. If you list a credentialed tool's
host:port in `sandbox.egress` as well, the bound endpoint takes over that host:port, since
OpenShell does not allow conflicting endpoints for one destination.

Not covered yet: OAuth MCP tools (their access token rotates; OpenShell's refresh
support is the planned route), MCPServers that authenticate the caller with the
router's ServiceAccount token (`allowedAgents`), secrets in another namespace, and pod
tools' `secretRefs`. These remain usable through the model-router as always. A sandbox
can attach at most 32 credentialed tools.

Use `tools.inherit: false`, `tools.excludeTools` or `tools.excludeHosts` to narrow this.
They only affect what the sandbox can reach; the router still offers the tool to the
LLM.

### Admission rules

The Agent webhook rejects a sandboxed Agent unless:

- the namespace has `agentorca.io/enable-sandboxes=true`;
- `guardrailPolicyRef` is set;
- `networkIsolation.splitPod` is `true`;
- it sets neither `runtime.securityContextOverride` nor `runtime.secretRefs` (a sandbox
  cannot mount Secrets; give credentials to a Tool so the router holds them);
- `runtime.inputMode` is `env` (the sandbox accepts no inbound connections);
- `runtime.command` is set: OpenShell runs it as the sandbox's main process and ignores
  the image's `ENTRYPOINT` (an empty command creates an idle sandbox);
- `runtime.framework` is `openai-compatible` or `langgraph`. `autogen` and
  `semantic-kernel` need init containers and `shim` needs hostname rewrites; sandboxes
  support neither;
- `egress[].binaries` and `filesystem` paths are absolute, and `filesystem.readWrite`
  does not include `/`.

AgentDeployments do not support sandboxed Agents yet. Their pods would run without the
sandbox, so the controller marks such a deployment `Failed`. Run the agent as AgentRuns.

## How a run works

```
AgentRun ─► operator ─┬─ router Service + router pod + NetworkPolicies   (run namespace, unchanged)
                      └─ OpenShell gateway (Go SDK) ─► Sandbox CR ─► supervisor pod + workload pod
                                                                      (gateway's sandbox namespace)
workload ──► supervisor ──► router Service :8080/:8082      (always allowed)
                       └──► sandbox.egress hosts            (policy-checked)
```

1. `startRun` builds the router config, RBAC, token Secret, router Service and router pod
   exactly as in split-pod mode. Instead of the agent pod, it creates a sandbox through
   the gateway:
   - the Agent image is the workload, and `command` + `args` is the main process;
   - the environment is the agent pod's: `AGENTORC_*`, `OPENAI_BASE_URL`, `OPENAI_API_KEY`
     and framework variables. The gateway rejects line breaks in environment values, so a
     multi-line value (usually the task) is passed exactly as `<NAME>_B64` (base64), and
     `<NAME>` holds a one-line version. The Go SDK's `agent.Input()` reads the exact one;
   - the policy is compiled from `sandbox` and the Agent's tools (see `internal/openshell`);
   - static tool credentials are registered as providers and attached first (see
     [Authenticated tools](#authenticated-tools)).
2. Once the gateway reports the sandbox ID, the operator adds a NetworkPolicy
   `agentorca-router-<run>-sandbox`. It admits only that sandbox's supervisor pod
   (`openshell.ai/sandbox-id=<id>` in the sandbox namespace) to the router.
3. The run is `Running` while the sandbox provisions and runs. A main-process exit code
   of 0 makes it `Succeeded`; the output is read from the workload pod's `agent`
   container. A nonzero exit, a sandbox `Error` or a rejected policy counts as a pod
   failure: the operator deletes the sandbox and retries with a fresh one, up to
   `restartPolicy.maxRetries`.
4. When the run ends, the operator deletes the sandbox and the run's credential providers. The gateway keeps completed
   sandboxes until they are deleted, and they sit outside ownerReference GC. If the
   gateway is unreachable at that point, the operator records a `SandboxDeleteFailed`
   event. Each sandbox carries `agentorca.io/run` and `agentorca.io/run-namespace`
   labels, so leftovers can be found with `openshell sandbox list`.

`AgentRun.status.sandboxName` holds the gateway's name for the current sandbox.

## Who enforces what

| Layer | Enforced by | Scope |
|---|---|---|
| LLM egress, routing, quotas, guardrails | model-router | All LLM traffic, sandboxed or not |
| Router pod egress/ingress | Kubernetes NetworkPolicy (agent-orca) | Router pod |
| Workload → supervisor only | Kubernetes NetworkPolicy (OpenShell) | Sandbox workload pod |
| Per-host, per-binary, HTTP method/path egress | OpenShell supervisor | Sandbox workload |
| Filesystem | Landlock (OpenShell) | Sandbox workload |
| Syscalls and identity | seccomp, non-root (OpenShell) | Sandbox workload |
| `execve`/`ptrace`/`mount` kill | Tetragon (optional, agent-orca) | Pods in the run namespace, i.e. the router pod. The sandbox workload is not affected: running binaries is the point, and OpenShell confines them. |

## Limitations and follow-ups

These are deliberate scope limits of this first version, tracked against
`plans/openshell-first-class-integration.md`:

- **Router credential**: the per-run router token is passed as plain sandbox environment,
  so it is stored in the gateway's sandbox spec. It is audience-bound to the model-router
  and short-lived, and the policy only lets it reach the router. Moving it to an OpenShell
  provider so the workload sees only a placeholder is still to do (tool credentials
  already work this way).
- **No http input mode, no AgentDeployments, no warm sandboxes** (Phase 4).
- **No `SandboxPolicy` CRD yet.** Egress and filesystem extras are inline on the Agent. A
  shared, reusable policy CRD and TenantConfig defaults are a follow-up.
- **No OCSF decision stream into egress sinks/UI timeline yet** (Phase 3). Use
  `openshell logs <sandbox>` to see denied connections while tuning `audit` →
  `enforce`.
- **Not yet exercised on a live cluster.** The gateway interaction is covered by unit
  tests against the SDK's fake client. The Phase 0 cluster spike (CNI enforcement, cold
  start, Landlock on the node pool) still needs to run before production use.
