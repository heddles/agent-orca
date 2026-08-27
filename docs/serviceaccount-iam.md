# ServiceAccount & IAM Identity

## How the stable ServiceAccount works

The `Agent` controller creates a single, long-lived ServiceAccount per Agent resource named `agentorca-agent-<agent-name>`. This SA is **agent-scoped, not run-scoped** — it persists across all AgentRuns and AgentDeployments that reference that Agent, which is what makes cloud IAM bindings stable.

```
Agent "hello-agent"
  └─ ServiceAccount "agentorca-agent-hello-agent"  ← created once, lives forever
       ├─ AgentRun "hello-run-1"    pod runs as this SA
       ├─ AgentRun "hello-run-2"    pod runs as this SA
       └─ AgentDeployment "hello"   deployment runs as this SA
```

## SA name derivation

Both the `AgentReconciler` (creator) and the execution controllers (consumers) derive the SA name using the same deterministic function:

```go
// internal/security/rbac.go
func AgentSAName(agentName string) string {
    return "agentorca-agent-" + agentName
}
```

Both `AgentRunReconciler` and `AgentDeploymentReconciler` call this at reconcile time:

```go
saName := security.AgentSAName(agent.Name)
if agent.Spec.ServiceAccountRef != nil {
    saName = agent.Spec.ServiceAccountRef.Name  // BYO SA override
}
```

The `saName` is then used in **three places** per execution:

| Usage | Code location | Purpose |
|---|---|---|
| `pod.Spec.ServiceAccountName` | `agentrun_controller.go:768` | Pod identity — controls which K8s RBAC and cloud IAM applies |
| `CoreV1().ServiceAccounts(...).CreateToken(...)` | `agentrun_controller.go:632` | Mints the projected token the model-router uses for TokenReview |
| `RouterConfig.AgentSAName` | `agentrun_controller.go:427` | Model-router validates incoming requests come from this SA |

## BYOS (Bring Your Own ServiceAccount)

If you need the Agent to use a pre-existing SA (e.g. one already bound to a cloud IAM role), set `spec.serviceAccountRef`:

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: Agent
metadata:
  name: my-agent
spec:
  serviceAccountRef:
    name: my-existing-sa   # operator will not create or manage this SA
  ...
```

When `serviceAccountRef` is set, the `AgentReconciler` skips SA creation entirely and records the provided name in `status.serviceAccountName`. The execution controllers use it the same way.

## Cloud identity federation

The `AgentReconciler` annotates the managed SA based on `spec.cloudAuth`:

### GCP Workload Identity

```yaml
spec:
  cloudAuth:
    gcp:
      serviceAccountEmail: my-agent@my-project.iam.gserviceaccount.com
```

Adds annotation to the SA:
```
iam.gke.io/gcp-service-account: my-agent@my-project.iam.gserviceaccount.com
```

Bind on the GCP side:
```bash
gcloud iam service-accounts add-iam-policy-binding \
  my-agent@my-project.iam.gserviceaccount.com \
  --role roles/iam.workloadIdentityUser \
  --member "serviceAccount:PROJECT.svc.id.goog[NAMESPACE/agentorca-agent-my-agent]"
```

### AWS IRSA

```yaml
spec:
  cloudAuth:
    aws:
      roleARN: arn:aws:iam::123456789012:role/my-agent-role
```

Adds annotation:
```
eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/my-agent-role
```

Trust policy on the role must allow the SA:
```json
{
  "Effect": "Allow",
  "Principal": {
    "Federated": "arn:aws:iam::123456789012:oidc-provider/oidc.eks.REGION.amazonaws.com/id/CLUSTER_ID"
  },
  "Action": "sts:AssumeRoleWithWebIdentity",
  "Condition": {
    "StringEquals": {
      "oidc.eks.REGION.amazonaws.com/id/CLUSTER_ID:sub": "system:serviceaccount:NAMESPACE:agentorca-agent-my-agent"
    }
  }
}
```

### Azure Workload Identity

```yaml
spec:
  cloudAuth:
    azure:
      clientID: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
```

Adds annotation:
```
azure.workload.identity/client-id: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
```

## Tool-level cloud identity override

Tools can specify their own `cloudAuth`, which is independent of the Agent's SA. When a tool pod runs with `executionMode: pod`, the tool pod gets its own cloud identity — useful when a tool needs different IAM permissions from the agent.

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: Tool
metadata:
  name: gcs-reader
spec:
  cloudAuth:
    gcp:
      serviceAccountEmail: gcs-reader@my-project.iam.gserviceaccount.com
```

## Potential gotcha: status vs derived name

The execution controllers derive the SA name via `security.AgentSAName(agent.Name)` directly — they do **not** read `agent.Status.ServiceAccountName`. This means:

- If `AgentSAName()` were ever changed, running controllers and the Agent controller would diverge until redeployed together.
- There is no risk of a race where a run starts before the Agent controller has written status: the formula is purely deterministic from the Agent's name.

In practice this is safe as long as `internal/security/rbac.go:AgentSAName` is treated as a stable contract.

## Summary

```
spec.cloudAuth (Agent CRD)
    └─ AgentReconciler annotates SA "agentorca-agent-<name>"
         └─ Cloud provider maps annotation → IAM role/binding
              └─ AgentRunReconciler / AgentDeploymentReconciler
                   set pod.spec.serviceAccountName = "agentorca-agent-<name>"
                        └─ Pod inherits cloud identity automatically
```

Your cloud IAM bindings target `agentorca-agent-<agent-name>` and they will be honoured by every AgentRun and AgentDeployment that references that Agent.
