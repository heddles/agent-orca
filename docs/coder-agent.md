# Coding agent in a sandbox

`coder-agent` (`cmd/coder-agent`) is a coding agent for [OpenShell sandboxes](openshell-sandbox.md).
It gives the model a shell and file tools over the sandbox workspace and works on the
run's input until the model calls `_done` or `_fail`. The tools run inside the sandbox as
[agent-run tools](agent-run-tools.md). Everything else goes through the run's model-router
as usual: LLM calls, the Agent's own tools, guardrails, safeguards and spend.

`config/samples/coder-agent.yaml` is a complete example: an Agent plus an AgentRun that
clones agent-orca, adds a unit test and runs it.

## The image

`ghcr.io/heddles/agent-orca/dev-sandbox` (`Dockerfile.dev-sandbox`) is the sandbox
workload. Its entrypoint is `coder-agent`, and it contains:

| Tool | Version |
|---|---|
| Go | the version in go.mod (golang:1.26 image) |
| Node.js, npm | 22 |
| Python, pip, venv | 3.11 (Debian bookworm) |
| git, make, gcc/g++, pkg-config, ripgrep, jq, curl | Debian bookworm |
| [mise](https://mise.jdx.dev) | pinned; installs other tools into the workspace at runtime |

OpenShell runs the workload as uid 1000. Only `/sandbox` (the workspace) and `/tmp` are
writable, so `HOME` and every cache and user-level install location live under
`/sandbox`:

- `GOPATH`, `GOMODCACHE`, `GOCACHE`
- npm's cache and global prefix
- pip's cache and `--user` installs (`PIP_USER=1`; Debian's PEP 668 guard is disabled
  because installs go to the workspace, not the system)
- mise's data directory

Their `bin` directories are on `PATH`.

Build and push a test image with `skaffold build -p dev-sandbox --platform linux/amd64`.
Releases publish it with the other images.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `AGENTORC_INPUT` | (set by the operator) | The task |
| `CODER_WORKSPACE` | `/sandbox` | The directory the file tools work in |
| `CODER_MAX_TURNS` | `300` | Maximum requests to the router |

## Agent settings

- `runtime.command: ["/usr/local/bin/coder-agent"]`. OpenShell runs the command, not the
  image's `ENTRYPOINT`.
- `runtime.localTools: true`, so the shell and file tools run in the sandbox.
- `runtime.sandbox.egress`: the hosts the agent may reach. Anything not listed is denied.
  The sample allows GitHub (clone over HTTPS), the Go module proxy and checksum database,
  `storage.googleapis.com`, npm and PyPI.
- `disableClarify: true`: nobody is there to answer a question.
- `AgentRun.spec.safeguards.maxRepeatedToolCalls`: a coding agent reruns the same build or
  test command after each edit, and the limit counts identical calls across the whole run.
  Set it high enough for that (the sample uses 10).

## Sizing

Measured on 2026-10-23 with a fresh clone of agent-orca in the image, limited to 2 CPUs
and 4 GiB of memory, with a disk-backed workspace and `/tmp`:

| Step | Time |
|---|---|
| `go mod download` | 13 s |
| `go build ./...` (cold) | 89 s |
| `go vet ./...` | 26 s |
| `go test` (router, openshell, pkg) | 13 s |

The workspace reached 3.4 GB: about 1.2 GB of modules and 1.9 GB of build cache. Go
also writes temporary build files to `/tmp`. Give the Agent at least `ephemeral-storage:
20Gi`, 4 GiB of memory (8 GiB as the limit) and 2 to 4 CPUs.

Notes from the measurement:

- A memory-backed `/sandbox` or `/tmp` (tmpfs) counts toward the memory limit, and the
  build was OOM-killed at 4 GiB. Keep both on disk.
- `go build ./...` in agent-orca needs `make ui-proxy-dist` first: `cmd/ui-proxy` embeds the
  built UI. The agent finds this out from the error; mention it in the task or in the
  repository's AGENTS.md to save it a turn.

## A dedicated, autoscaled node pool

Builds are heavy, so run sandboxes on their own nodes and keep everything else off
them. For example, with nodes labeled and tainted `openshell=coder-agent`:

```shell
kubectl label node <node> openshell=coder-agent
kubectl taint node <node> openshell=coder-agent:NoExecute
```

```yaml
# agent-orca chart values
openshell:
  placement:
    nodeSelector:
      openshell: coder-agent
    tolerations:
      - {key: openshell, operator: Equal, value: coder-agent, effect: NoExecute}
```

This needs the gateway's `allowDriverConfig` (see
[Placement](openshell-sandbox.md#placement)). Placement is applied to the sandbox's
workload pod. OpenShell's separate supervisor pod is small and may still schedule on
your regular nodes.

If the pool scales from zero, the cluster autoscaler must know the pool's labels and
taints from its node-group configuration. Otherwise it can't tell that a pending
sandbox would fit there, and never scales the pool up. Expect the first run after a
scale-up to wait a few minutes for the node, inside the AgentRun's `timeout`.
