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

The controller-runtime manager exposes additional metrics on `:8443` (internal,
not the external surfaces). These include standard Kubernetes controller
metrics (reconciliation counts, errors, etc.).

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

```yaml
scrape_configs:
  - job_name: "agent-orca-external"
    static_configs:
      - targets: ["agent-orca-external.default.svc:8084"]
    metrics_path: /metrics
```

Both the External Task API (8084) and ACP API (8000) expose metrics on the
same path. Use the `server` label to distinguish them.
