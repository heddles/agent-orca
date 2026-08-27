# Observability

agent-orca exposes standard Kubernetes-style probes, Prometheus metrics, and
structured audit logging on every external HTTP surface.

## Health probes

All probes are unauthenticated (`publicPaths` in `internal/apiserver/auth.go`).

| Endpoint | Port | Purpose |
|---|---|---|
| `GET /healthz` | 8084, 8000 | Liveness — returns 200 if the server process is alive |
| `GET /readyz` | 8084, 8000 | Readiness — checks K8s API connectivity and Redis (if configured) |
| `GET /version` | 8084, 8000 | Returns build version, commit, and uptime |
| `GET /metrics` | 8084, 8000 | Prometheus-format metrics |

```bash
curl http://localhost:8084/healthz
curl http://localhost:8084/readyz
curl http://localhost:8084/version
curl http://localhost:8084/metrics
```

## Prometheus metrics

Metrics are served from a dedicated registry (`externalReg` in
`internal/apiserver/metrics.go`) so they don't collide with the
controller-runtime manager metrics on `:8443`.

### HTTP API metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `agentorca_external_requests_total` | Counter | `server`, `path`, `method`, `status` | Total HTTP requests to external API surfaces |
| `agentorca_external_request_duration_seconds` | Histogram | `server`, `path` | Request latency distribution |
| `agentorca_external_auth_failures_total` | Counter | `server` | Failed authentication attempts |

### Egress metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `agentorca_egress_published_total` | Counter | `sink_type` | Successfully published results to egress sinks |
| `agentorca_egress_failed_total` | Counter | `sink_type` | Failed egress publishes |

### Controller metrics

The controller-runtime manager exposes additional metrics on `:8080` in the
Helm chart (`--metrics-bind-address=:8080 --metrics-secure=false`; kubebuilder
scaffold default is `:8443` over HTTPS) — these are server-internal and not on
the external API surfaces. They include standard Kubernetes controller metrics
(reconciliation counts, errors, workqueue depth, etc.). These are the metrics
served by the `<release>-metrics` Service and scraped by the chart's
`ServiceMonitor`.

## Audit logging

Every authenticated external API request is written to the operator log as a
structured JSON line with the key `external_api_request`. The log entry
includes:

| Field | Description |
|---|---|
| `tenant` | Tenant name from the authenticated JWT |
| `namespace` | Tenant target namespace |
| `method` | HTTP method |
| `path` | Request path |
| `status` | HTTP response status code |
| `latency_ms` | Request processing time in milliseconds |

Example log line:

```json
{"time":"2024-01-01T12:00:00Z","level":"INFO","msg":"external_api_request","tenant":"acme","namespace":"tenant-acme","method":"POST","path":"/v1/tasks","status":201,"latency_ms":42}
```

## Scraping with Prometheus

agent-orca ships a dedicated `Service` exposing the operator's
controller-runtime metrics on port **8080** (`<release>-metrics`), plus
Prometheus-format `/metrics` endpoints on the External Task API (**8084**) and
the ACP API (**8000**). Use the `server` label to distinguish the API metrics.

You do **not** need a separate external service to collect these metrics — they
are plain Prometheus/OpenMetrics HTTP endpoints. The only question is how a
scraper discovers and reaches them. In-cluster Prometheus (via the Prometheus
Operator) is the supported default; Datadog and any OpenMetrics consumer can scrape
the *same* Service/Pod endpoints with no code changes.

### Prometheus Operator (`ServiceMonitor` / `PodMonitor`)

When the `monitoring.coreos.com` CRDs are installed (the project's `charts/cluster`
and `cloudnative-pg` charts already depend on them if they are enabled). The `agent-orca` Helm chart
renders the following when `metrics.serviceMonitor.enabled` and `metrics.modelRouter.podMonitor.enabled` are both `true`:

- a `ServiceMonitor` scraping the operator metrics `Service` (port 8080), and
- a `PodMonitor` scraping every model-router sidecar on port 9091.

Toggle them in `values.yaml` under `metrics.`:

```bash
helm upgrade agent-orca ./charts/agent-orca \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.modelRouter.podMonitor.enabled=true
# or disable the whole metrics feature (Service + annotations + monitors):
#   --set metrics.enabled=false
```

### Reaching agent-pod metrics behind the NetworkPolicy

The model-router `PodMonitor` scrapes agent pods by PodIP on `:9091`. Each
AgentRun's `NetworkPolicy` is deny-by-default, so agent-orca opens `:9091` only
from namespaces labeled `metrics: enabled` — the same convention already used
for the operator's own metrics port (8080). Label your scraper namespace:

```bash
kubectl label namespace prometheus-operator metrics=enabled
```

### Datadog / external scrapers

Point a Datadog OpenMetrics integration at the **same** targets:

- Operator metrics: `http://<release>-metrics.<namespace>.svc:8080/metrics`
- model-router metrics: discover pods with the `metrics` container port (9091)
  via the Datadog Kubernetes/Agent integration, or set a pod-level
  `prometheus.io/scrape: "true"` check.

The `prometheus.io/scrape`, `prometheus.io/port`, and `prometheus.io/path`
annotations are added to the operator pod by default (`metrics.enabled=true`),
so Datadog auto-discovery works without a ServiceMonitor.

### Static scrape config (plain Prometheus, no Operator)

```yaml
scrape_configs:
  - job_name: "agent-orca-operator"
    static_configs:
      - targets: ["<release>-metrics.<namespace>.svc:8080"]
    metrics_path: /metrics
  - job_name: "agent-orca-external"
    static_configs:
      - targets: ["<release>-internal-api.<namespace>.svc:8084"]
    metrics_path: /metrics
```

Both the External Task API (8084) and ACP API (8000) expose metrics on the
same path. Use the `server` label to distinguish them.
