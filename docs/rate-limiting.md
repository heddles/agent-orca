# Rate Limiting & Budget Enforcement

agent-orca enforces three per-tenant admission controls before a task is
accepted. These are configured on the `TenantConfig` CRD and enforced by the
`RateLimiter` in `internal/apiserver/rate_limit.go`.

## Configuration

Set limits on the `TenantConfig` spec:

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: TenantConfig
metadata:
  name: acme-corp
  namespace: agentorca-system
spec:
  authMode: issued
  issued:
    clientID: "acme-client"
    clientSecretRef:
      name: acme-credentials
      key: client-secret
  allowedNamespaces:
    - "tenant-acme"
  allowedAgents:
    - "support-bot"
  rateLimit:
    requestsPerMinute: 60      # token-bucket rate limit (429 when exceeded)
    concurrentRuns: 10         # max active runs in the namespace (429 when exceeded)
  budgetPerDayUSD: "100.00"    # daily spend ceiling (402 when exceeded)
```

| Field | Type | Default | Description |
|---|---|---|---|
| `rateLimit.requestsPerMinute` | int | 0 (unlimited) | Maximum task submissions per minute per tenant |
| `rateLimit.concurrentRuns` | int | 0 (unlimited) | Maximum AgentRuns in `Running` phase simultaneously |
| `budgetPerDayUSD` | string | "" (no limit) | Maximum daily LLM spend in USD |

## Enforcement order

Quotas are checked in this order before a task is accepted:

1. **Rate limit** (token-bucket) — `429 Too Many Requests` with `Retry-After`
2. **Concurrent runs** — `429 Too Many Requests` with `Retry-After`
3. **Daily budget** — `402 Payment Required`

If any check fails, the request is rejected before an `AgentRun` is created.

## HTTP responses

### 429 — Rate limit or concurrency exceeded

```http
HTTP/1.1 429 Too Many Requests
Content-Type: application/json
Retry-After: 30

{
  "code": "rate_limited",
  "message": "rate limit exceeded for tenant acme: 60 requests per minute"
}
```

The `Retry-After` header (in seconds) tells the client how long to wait before
retrying. It is only present on `429` responses.

### 402 — Budget exceeded

```http
HTTP/1.1 402 Payment Required
Content-Type: application/json

{
  "code": "budget_exceeded",
  "message": "daily budget of $100.00 exceeded for tenant acme"
}
```

## How quotas are scoped

- **Rate limit** — tracked per tenant name, in-memory token bucket with a
  burst capacity equal to `requestsPerMinute`.
- **Concurrent runs** — counted as `AgentRun` resources in the tenant's
  target namespace with `status.phase == Running`.
- **Budget** — summed from `AgentRun` status `costUSD` fields for runs
  created today (UTC) in the tenant's namespace.

## Setting limits via the Admin API

Operators can set limits when creating or updating a tenant through the
[Admin API](admin-api.md):

```bash
aoctl admin tenants create acme \
  --namespace tenant-acme \
  --client-id acme-client \
  --rpm 60 \
  --concurrent 10 \
  --budget 100.00
```
