# Egress Sinks

When an `AgentRun` reaches a terminal phase (`Succeeded` or `Failed`), the
controller can publish the final result to an external message bus. This is
configured via `spec.egress` on the `AgentRun` CRD and is independent of
webhook callbacks — both can be configured simultaneously.

## Supported sinks

| Type | Provider | Delivery guarantee |
|---|---|---|
| `kafka` | Apache Kafka | At-least-once (producer acks = 1), keyed by run ID for ordering |
| `pubsub` | Google Cloud Pub/Sub | At-least-once |
| `redis` | Redis Streams | At-most-once (fire-and-forget) |

## Configuration

| Field | Type | Required | Description |
|---|---|---|---|
| `type` | string | yes | `kafka`, `pubsub`, or `redis` |
| `topic` | string | yes | Destination topic/stream name |
| `brokers` | []string | kafka only | Broker addresses (e.g. `["kafka:9092"]`) |
| `projectID` | string | pubsub only | GCP project ID |
| `address` | string | redis only | Redis connection address (e.g. `redis-master:6379`) |
| `secretRef` | SecretKeyRef | no | Secret containing sink credentials |
| `key` | string | no | Partition key (kafka only; defaults to run ID) |

## Secret key conventions

The `secretRef` Secret must be in the same namespace as the `AgentRun`.

| Sink | Required keys |
|---|---|
| Kafka | `sasl-username`, `sasl-password` (or `tls-cert`, `tls-key`, `tls-ca` for TLS) |
| Pub/Sub | `credentials` (service account JSON blob) |
| Redis | `password` |

## Examples

### Kafka

```yaml
apiVersion: agentorc.agentorc.io/v1alpha1
kind: AgentRun
metadata:
  name: hello-run
  namespace: tenant-acme
spec:
  agentRef: hello-agent
  input: "Say hello to the world"
  egress:
    type: kafka
    topic: agent-results
    brokers:
      - kafka-cluster-kafka-bootstrap.kafka:9092
    secretRef:
      name: kafka-creds
      key: sasl-password
      namespace: tenant-acme
```

### Pub/Sub

```yaml
spec:
  egress:
    type: pubsub
    topic: agent-results
    projectID: my-gcp-project
    secretRef:
      name: pubsub-creds
      key: credentials
      namespace: tenant-acme
```

### Redis

```yaml
spec:
  egress:
    type: redis
    topic: agent-results       # Redis stream name
    address: redis-master:6379
    secretRef:
      name: redis-creds
      key: password
      namespace: tenant-acme
```

## Published payload

The `EgressResult` payload is self-contained (no HATEOAS links) and includes:

| Field | Description |
|---|---|
| `runId` | AgentRun name |
| `agent` | Agent name invoked |
| `phase` | Terminal phase: `"Succeeded"` or `"Failed"` |
| `output` | Final output (truncated to 10KB) |
| `costUSD` | Total LLM cost for this run |

## Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `agentorc_egress_published_total` | Counter | `sink_type` | Successfully published results |
| `agentorc_egress_failed_total` | Counter | `sink_type` | Failed publishes |

## AgentDeployment input sources

Long-running `AgentDeployment` resources can receive input from different
sources. This is configured via `spec.inputSource`:

| Type | Description |
|---|---|
| `chat` (default) | API-driven — each message spawns an `AgentRun` |
| `queue` | Redis or other queue — polls for input messages |
| `pubsub` | Kafka or Pub/Sub — subscribes to an input topic |
| `loop` | Self-managed — the deployment manages its own input loop |

Example:

```yaml
apiVersion: agentorc.agentorc.io/v1alpha1
kind: AgentDeployment
metadata:
  name: support-bot
  namespace: tenant-acme
spec:
  agentRef: support-bot
  inputSource:
    type: pubsub
    config:
      provider: kafka
      brokers: "kafka:9092"
      topic: agent-input
```
