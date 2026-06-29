# Redis Setup

Redis is an optional but strongly recommended dependency for agent-orc. Without it, two features are silently degraded:

| Feature | Without Redis | With Redis |
|---------|--------------|------------|
| Cost tracking | Always shows $0 | Accurate per-run USD spend |
| Crash recovery | Conversation lost on pod crash | Resumes from last checkpoint |

The operator logs a warning at startup when Redis is not configured, and the UI displays a persistent banner.

## How it works

The model-router sidecar (running inside every agent pod) writes conversation state and spend data to Redis after each LLM response. The operator controller reads spend back from Redis to populate `AgentRun.status.spendUSD`. On pod crash and restart, the new sidecar loads the checkpoint from Redis and resumes the conversation from where it left off.

Redis must be reachable by:
- The operator/controller (reads spend, sets CRD status)
- Every agent pod's model-router sidecar (writes checkpoints and spend)

Redis does **not** need to be inside the cluster — an external managed Redis (e.g. AWS ElastiCache, GCP Memorystore, Azure Cache for Redis, Upstash) works fine as long as it is network-accessible from the operator and agent pods.

## Configuration

Set two environment variables on the operator Deployment:

```yaml
env:
  - name: STATE_BACKEND
    value: redis
  - name: REDIS_URL
    valueFrom:
      secretKeyRef:
        name: agent-orc-redis
        key: url
```

`REDIS_URL` must be a valid Redis connection URL:

```
redis://:<password>@<host>:<port>/<db>
rediss://:<password>@<host>:<port>/<db>   # TLS
```

The operator propagates `STATE_BACKEND` and `REDIS_URL` into the `router-config.json` ConfigMap mounted into every agent pod, so the model-router sidecar automatically uses the same Redis instance.

## Storing credentials

Create a Kubernetes Secret in the same namespace as the operator:

```bash
kubectl create secret generic agent-orc-redis \
  --namespace agent-orc-system \
  --from-literal=url='redis://:<password>@redis.example.com:6379/0'
```

For TLS connections (recommended for external Redis):

```bash
kubectl create secret generic agent-orc-redis \
  --namespace agent-orc-system \
  --from-literal=url='rediss://:<password>@redis.example.com:6380/0'
```

Reference the secret in the operator Deployment as shown above. The operator never exposes the URL to agent workloads — it only passes the `STATE_BACKEND` type and URL into the sidecar config, which runs with a restricted pod security profile.

## Using a managed Redis service

### AWS ElastiCache (Valkey/Redis OSS)

1. Create a Serverless cache or a cluster-mode disabled replication group.
2. Enable in-transit encryption (TLS). Use `rediss://` scheme.
3. If auth tokens are enabled, include the token in the URL as the password: `rediss://:TOKEN@primary-endpoint:6379/0`.
4. Ensure the operator's node group / VPC security group can reach the ElastiCache endpoint on port 6379/6380.

### GCP Memorystore

1. Create a Memorystore for Redis instance. Enable AUTH and in-transit encryption.
2. The instance IP is only reachable from within the VPC. Use VPC-native GKE clusters or configure Private Service Connect.
3. URL format: `rediss://:AUTH_STRING@INSTANCE_IP:6378/0`.

### Upstash (serverless, no infrastructure)

1. Create a database at [upstash.com](https://upstash.com). Enable TLS (default).
2. Copy the `UPSTASH_REDIS_URL` from the console — it already includes credentials.
3. Store it as the `url` key in the Secret above.

## Key TTL

Checkpoint keys expire automatically. The default TTL is 6 hours beyond the AgentRun's active window. There is no manual key management required. Completed run data is not kept indefinitely — once the key expires, the AgentRun CRD status already holds the final spend value, so no data is lost for reporting purposes.

## RBAC

Redis credentials never leave the operator namespace. Agent pods receive only the opaque `router-config.json` ConfigMap, which contains the Redis URL. If you want to avoid agent pods having any visibility into the Redis URL, you can set up a Redis proxy (e.g. Envoy, Twemproxy) and give agent pods the proxy address instead.
