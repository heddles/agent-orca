# Installation

You can run agent-orca [locally from source](#run-it-locally) or
[install the Helm chart](#install-with-helm) on your own cluster.

## Run it locally

This builds agent-orca from your checkout and deploys it to a local
[kind](https://kind.sigs.k8s.io) cluster.

### Prerequisites

- Docker, running
- [Git LFS](https://git-lfs.com). The build needs a binary that is stored in LFS.
- kind, [Skaffold](https://skaffold.dev), Helm and `kubectl`
- An API key for a model provider: Anthropic, OpenAI, Google or OpenRouter

If you use [mise](https://mise.jdx.dev), `mise install` in the repository
installs the tools listed in `mise.toml`. Docker is not one of them.

### Start the platform

```bash
git clone https://github.com/heddles/agent-orca.git && cd agent-orca
export ANTHROPIC_API_KEY=sk-ant-...   # or OPENAI_API_KEY, GOOGLE_API_KEY, OPENROUTER_API_KEY
kind create cluster --name agent-orca-dev
kubectl config use-context kind-agent-orca-dev
skaffold dev -p dev
```

::: warning Check your context
`skaffold dev` deploys to your current `kubectl` context. If the kind cluster
already exists, switch to it first, so that you don't deploy to another cluster.
:::

Leave `skaffold dev` running. It rebuilds and redeploys when you change code,
and removes what it deployed when you stop it.

### Create your first agent

The platform starts without any agents. When Skaffold reports
`Watching for changes`, run this in a second terminal, from the repository:

```bash
kubectl --context kind-agent-orca-dev apply -f config/samples/quickstart.yaml
```

Open <http://localhost:8080> and chat with the `quickstart` agent. You can
ignore the tenant isolation warning that `kubectl` prints.

The default model selector lists several providers. The router moves past the
ones you gave no key, so its logs show authentication errors for them.

### What gets deployed

| Component | Notes |
|---|---|
| agent-orca operator and web UI | In `agent-orca-system`. UI sign-in with OIDC is off |
| Redis | Checkpoints, spend and token streams |
| CloudNativePG and PostgreSQL | Run archive and approvals |
| Model providers and selectors | From `charts/model-providers`, using your keys |
| Ollama | Local `nomic-embed-text` embeddings for [RAG](rag.md) |
| OpenShell gateway and Agent Sandbox | For [sandboxed agents](openshell-sandbox.md) |
| ingress-nginx | An Ingress for `agent-orca.local`, if you add that name to `/etc/hosts` |

Skaffold forwards these ports:

| Port | Surface |
|---|---|
| 8080 | Web UI. If the port is taken, Skaffold's output shows the one it used |
| 8000 | [ACP API](acp-api.md) |
| 8083 | UI API |
| 8084 | [External Task API](enterprise-integration.md#external-task-api) |

### Clean up

Stop `skaffold dev`, then delete the cluster:

```bash
kind delete cluster --name agent-orca-dev
```

Next, try a [demo](demos.md) or read the [development guide](development.md).

## Install with Helm

Each [release](https://github.com/heddles/agent-orca/releases) publishes the
chart and its images. Replace `0.16.0` below with the release you want.

### 1. Choose whether to use PostgreSQL

The chart expects PostgreSQL by default, but does not install it. **The operator
does not start until its database Secret exists.**

- **With PostgreSQL** (recommended): before you install, run a database, put its
  password in a Secret named `postgres-app` under the key `password`, in
  `agent-orca-system`, and set `database.dsn` if your database is not the chart's
  default address. The default is a database and user named `agentorca` at
  `postgres-rw.agent-orca-system`, which is what a
  [CloudNativePG](https://cloudnative-pg.io) cluster named `postgres` provides.
- **Without it** (for a trial): add `--set database.enabled=false` to the
  command below. Finished runs are not [archived](run-archival.md), and features
  that store data in PostgreSQL, such as approvals, don't work.

### 2. Install the chart

```bash
helm install agent-orca oci://ghcr.io/heddles/agent-orca/charts/agent-orca \
  --version 0.16.0 -n agent-orca-system --create-namespace
```

The chart installs the resource definitions and a bundled Redis. `helm upgrade`
does not update resource definitions, so check the upgrade notes before you
upgrade.

### 3. Add a model

Create a Secret with your key, then a `ModelProvider` and a `ModelSelector`.
This example uses Anthropic:

```bash
kubectl -n agent-orca-system create secret generic anthropic-api-key \
  --from-literal=api-key="$ANTHROPIC_API_KEY"
```

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: ModelProvider
metadata:
  name: claude
  namespace: agent-orca-system
spec:
  litellmModel: anthropic/claude-sonnet-5-5
  credentialsRef:
    name: anthropic-api-key
    key: api-key
---
apiVersion: agentorca.agentorca.io/v1alpha1
kind: ModelSelector
metadata:
  name: default
  namespace: agent-orca-system
spec:
  strategy: rule-based
  providers:
    - name: claude
      weight: 100
```

Save it as `model.yaml` and apply it with `kubectl apply -f model.yaml`.

### 4. Create an agent and open the UI

```bash
kubectl apply -f https://raw.githubusercontent.com/heddles/agent-orca/main/config/samples/quickstart.yaml
kubectl -n agent-orca-system port-forward svc/agent-orca-ui 8080:80
```

Open <http://localhost:8080> and chat with the `quickstart` agent.

::: warning Set up sign-in before you share the UI
Until you configure sign-in, anyone who can reach the UI Service can use it.
Don't expose it through an Ingress or load balancer first. See
[Authentication](auth.md) and [OIDC login](oauth-login.md).
:::

To mirror or pin images, see
[Container image references](development.md#container-image-references).
