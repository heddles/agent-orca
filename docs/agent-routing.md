# Automatic agent routing

A person or a client can send a request without naming an agent, and Agent Orca picks one. By default the pick is made on CPU inside your cluster, by a small model deployed with the chart. Nothing leaves the cluster.

Routing never widens access. It picks only among agents the caller could already have named: in their namespaces, belonging to their tenant and on its `allowedAgents` list. The chosen agent runs with its own tools, approvals, guardrails and budget.

## Where you can use it

| Entry point | How |
|---|---|
| [Task API](enterprise-integration.md#external-task-api) | `POST /v1/tasks` with `"agent": "auto"` |
| [ACP API](acp-api.md) | `"agent_name": "auto"` on `POST /runs`, or `POST /agents/auto/run` |
| Editors such as Zed | `aoctl acp setup --editor zed` configures it. See the [CLI reference](aoctl-reference.md#aoctl-acp) |
| Web UI | Home, **Ask an agent**, **Let Agent Orca choose** |
| Slack | A channel of a [shared chat app](inbound-channels.md#automatic-places) that an administrator set to pick automatically |

An agent that is really called `auto` keeps its name: a request for `auto` then goes to it.

## How an agent is picked

1. A conversation keeps its agent, for as long as the caller may run it. An editor thread, an ACP session and a chat thread are conversations. Naming another agent moves the conversation.
2. If there is only one agent to pick, it is picked.
3. The router scores the agents. If it is at least 80% sure of one, that agent gets the request.
4. Otherwise the request goes to the **default agent**: the chat channel's, or else the tenant's.
5. With no default agent the caller may run, the router's first choice gets the request if it scored at least 50%. Below that the request is refused, with the names the caller may use.

**Set a default agent.** Without one, unclear requests are either refused or given to a specialist the router is not sure of.

### What it gets wrong

A request that **no agent should handle** is the weakest case. The router compares agents with each other, so one of them can score well for a request that fits none. In a trial with three agents (billing, on-call and helpdesk), "recommend a movie for tonight" went to the on-call agent at 90%, over the 80% bar, so the default agent never saw it. A higher `minConfidencePercent` sends more requests to the default agent. Whatever gets a request it cannot help with still runs under its own limits.

Accuracy depends mostly on what agents say about themselves. See the measurements in [`hack/dispatch-eval/bench`](https://github.com/heddles/agent-orca/tree/main/hack/dispatch-eval/bench).

## Help the router

Every agent is routable without setup. The router reads its name, the start of its system prompt, its tools and its knowledge bases. You get better picks by saying what the agent is for:

```yaml
spec:
  routing:
    description: Invoices, payments, refunds and subscription charges
    useWhen:                       # up to 20
      - a customer was charged twice or wants a refund
      - someone asks what an invoice line means
    examples:                      # up to 50
      - Why was I billed twice in March?
      - Can I get a VAT invoice for order 4411?
```

- **Examples** help the most, once **every** agent that can be picked has at least five. Until then nobody's examples are weighed by the local router, so that agents with examples do not crowd out the others.
- **`useWhen`** lines each describe one kind of request.
- A `description` replaces the system prompt, tools and knowledge bases on the agent's card.

These texts are redacted before use, but write them as text you are willing to send to your own model provider if you let a model decide.

To keep an agent from ever being picked, label it `agentorca.io/dispatch: "false"`. It can still be named, kept by a conversation, or set as the default agent. Templates are never picked.

## Tenant settings

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: TenantConfig
spec:
  dispatch:
    defaultAgent: helpdesk
    enabled: true                 # false turns routing off for the tenant
    minConfidencePercent: 80      # 50 to 100
    decider: local                # local | modelSelector
    # modelSelectorRef:
    #   name: dispatch-judge
    #   namespace: team-a         # default: the first of allowedNamespaces
    # escalate: false
```

| Setting | Request text leaves the cluster |
|---|---|
| `decider: local` (default) | No |
| `decider: local` with `escalate: true` | Only when the local router is unsure |
| `decider: modelSelector` | Every decision |

With `modelSelector` or `escalate`, a model of your ModelSelector decides, using your providers and credentials. It is more accurate and slower, and it costs money. It receives the request and the agents' descriptions, after redaction. Naming a `modelSelectorRef` alone sends nothing: one of the two settings must ask for it.

Decision calls are not made once the day's spend, on runs and decisions, reaches `budgetPerDayUSD`; the request then follows the unsure path. Decision spend does not yet count against starting new runs.

## Deploying the router

The chart deploys `orca-decide` by default: a small server and a [llama.cpp](https://github.com/ggml-org/llama.cpp) sidecar running the `gte-small` embedding model (MIT licence). The model ships as an image, so nothing is downloaded when a pod starts. A NetworkPolicy lets only the operator call it, and it calls nothing.

| | |
|---|---|
| Requests | 0.35 CPU, 256 MiB across both containers |
| Memory in use | About 100 to 125 MiB |
| Time for a decision | On two cores of a 2015 x86 CPU, about 14 ms for a short request and 45 ms for a long one (model time) |
| Text read | The first 64 tokens of a request (`decision.requestTokens`) |

```yaml
decision:
  enabled: false   # no local router
```

Without it, only tenants that set `decider: modelSelector`, or `escalate` with a selector, have automatic routing. Others get `auto_unavailable`.

After agents or access change, the router needs a moment to prepare them. The operator does this ahead of requests. A request that arrives first follows the unsure path.

## What is recorded

Each run picked this way carries the annotation `agentorca.io/dispatch`, and the API answers and the run page show it: the method, the confidence and the agents considered.

With PostgreSQL, each decision is also stored for 90 days: tenant, entry point, method, agents considered, probabilities, tokens, cost and time. **The request text is never stored there.**

Metrics: `agentorca_dispatch_decisions_total`, `agentorca_dispatch_duration_seconds`, `agentorca_dispatch_confidence` and `agentorca_dispatch_record_failures_total`.

## Not built yet

- Learning from corrections. Choosing another agent does not teach the router.
- Splitting one request across several agents.
- Using a signal for "no agent fits" other than low confidence.
