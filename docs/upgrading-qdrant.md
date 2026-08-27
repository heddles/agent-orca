# Upgrading Qdrant for KnowledgeBases

The agent-orca operator deploys a dedicated Qdrant StatefulSet for each
KnowledgeBase. Starting with this release, the operator **automatically manages
Qdrant version upgrades** using a sequential minor-version walk.

## How It Works

1. The operator derives its **target Qdrant version** from the compiled
   `github.com/qdrant/go-client` module version (or the Helm
   `qdrant.image.tag` override).
2. On each reconcile, it **probes the running Qdrant version** via the HTTP
   root endpoint (`GET /` on port 6333).
3. If the running version is behind the target, the operator walks through
   each intermediate minor version one step at a time:
   - Creates a **collection snapshot** before each step
   - Patches the StatefulSet container image to the next minor version
   - Waits for the pod to become ready
   - Re-probes the version and repeats until the target is reached

This is necessary because Qdrant only guarantees storage migration between
**consecutive** minor versions. Skipping minors (e.g. v1.12 → v1.17) can fail
or corrupt data.

## Monitoring Upgrade Progress

```bash
# Watch version and upgrade state
kubectl get knowledgebase my-kb -o jsonpath='{.status.qdrantVersion}'
kubectl get knowledgebase my-kb -o jsonpath='{.status.qdrantTargetVersion}'
kubectl get knowledgebase my-kb -o jsonpath='{.status.qdrantUpgradeState}'

# Check conditions
kubectl get knowledgebase my-kb -o jsonpath='{.status.conditions}' | jq .
```

Conditions set during upgrades:

| Condition | When |
|---|---|
| `QdrantUpgrading=True` | An upgrade step is in progress |
| `QdrantUpgradeAvailable=True` | A version gap exists but `autoUpgrade` is disabled |
| `QdrantUpgrading=False, Reason=UpgradeFailed` | A step failed; rolled back to previous version |

The `Qdrant` column in `kubectl get knowledgebase` shows the running version.

## Holding Upgrades

Set `autoUpgrade: false` on the KnowledgeBase spec to prevent automatic
upgrades. The operator will still report the version gap via the
`QdrantUpgradeAvailable` condition but will not modify the StatefulSet.

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: KnowledgeBase
metadata:
  name: my-kb
spec:
  vectorStore:
    autoUpgrade: false
  # ...
```

To resume automatic upgrades, set `autoUpgrade: true` (or remove the field;
the default is `true`).

## Failure Handling

If a Qdrant pod enters CrashLoopBackOff after an image update and remains
failing for 5 minutes, the operator:

1. Reverts the StatefulSet image to the previously running version
2. Sets `QdrantUpgradeState` to `Failed`
3. Sets condition `QdrantUpgrading=False` with reason `UpgradeFailed`

The operator will not retry automatically. To retry after investigating:

1. Fix the underlying issue (check pod logs, storage, etc.)
2. Toggle `autoUpgrade` from `false` to `true` on the KnowledgeBase spec

If a snapshot fails before the upgrade can begin, the state is also set to
`Failed` with a descriptive condition message. No image change is made.

## Manual Override

If you need to manage Qdrant versions independently:

1. Set `autoUpgrade: false` on the KnowledgeBase
2. Override the image via Helm or direct StatefulSet patch:

```bash
kubectl set image statefulset/kb-qdrant-my-kb \
  qdrant=qdrant/qdrant:v1.14.1 -n default
```

Remember to step through consecutive minor versions manually.

## Helm-Level Override

To set the target version for **all new** KnowledgeBases at operator deploy time:

```yaml
qdrant:
  image:
    tag: "v1.17.1"
```

The auto-upgrade mechanism uses this value as its target for existing
KnowledgeBases as well.
