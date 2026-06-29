# UIProxy

UIProxy is a small Go reverse proxy that sits between the internet and the operator's UI API.
It serves the pre-built React SPA and authenticates every API call to the operator using a
Kubernetes projected ServiceAccount token. Browsers connect to UIProxy with no authentication
required — OIDC/SAML can be layered on the Ingress later.

```
Browser (no auth)
  │
  ▼
UIProxy Pod
  ├── serves React SPA (embedded at build time)
  └── /api/* → operator :8083 with Authorization: Bearer <SA token>
                                │
                                ▼
                          Operator Pod
                          (TokenReview: audience agentorc/ui)
```

---

## Building

```bash
# 1. Build the React app and copy it into the proxy package
make build-ui-proxy

# The resulting binary is bin/ui-proxy
```

Or manually:

```bash
cd ui && npm run build
rm -rf cmd/ui-proxy/dist && cp -r ui/dist cmd/ui-proxy/dist
go build -o bin/ui-proxy ./cmd/ui-proxy
```

---

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `8080` | Port the proxy listens on |
| `--operator-addr` | `http://localhost:8083` | Upstream operator UI API |
| `--token-file` | `/var/run/secrets/agentorc/ui/token` | Path to the projected SA token |

---

## Kubernetes deployment

### 1. ServiceAccount

Create a dedicated ServiceAccount for the UIProxy. No RBAC rules are needed — the SA is
only used for identity proof via TokenReview.

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: agentorc-ui-proxy
  namespace: default
```

### 2. Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: agentorc-ui-proxy
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: agentorc-ui-proxy
  template:
    metadata:
      labels:
        app: agentorc-ui-proxy
    spec:
      serviceAccountName: agentorc-ui-proxy
      containers:
        - name: ui-proxy
          image: your-registry/agentorc-ui-proxy:latest
          args:
            - --operator-addr=http://agentorc-operator:8083
            - --token-file=/var/run/secrets/agentorc/ui/token
          ports:
            - containerPort: 8080
          volumeMounts:
            - name: ui-token
              mountPath: /var/run/secrets/agentorc/ui
              readOnly: true
      volumes:
        - name: ui-token
          projected:
            sources:
              - serviceAccountToken:
                  audience: agentorc/ui       # must match UITokenAudience in the operator
                  expirationSeconds: 900      # 15 minutes; kubelet auto-refreshes
                  path: token
```

### 3. Service and Ingress

```yaml
apiVersion: v1
kind: Service
metadata:
  name: agentorc-ui-proxy
  namespace: default
spec:
  selector:
    app: agentorc-ui-proxy
  ports:
    - port: 80
      targetPort: 8080
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: agentorc-ui
  namespace: default
  annotations:
    # Future: add OIDC/SAML here via oauth2-proxy annotation
spec:
  rules:
    - host: agentorc.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: agentorc-ui-proxy
                port:
                  number: 80
```

### 4. NetworkPolicy (recommended)

Restrict operator port 8083 to accept connections only from the UIProxy pod:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: operator-ui-api
  namespace: default
spec:
  podSelector:
    matchLabels:
      app: agentorc-operator   # adjust to match your operator pod label
  ingress:
    - ports:
        - port: 8083
      from:
        - podSelector:
            matchLabels:
              app: agentorc-ui-proxy
```

### 5. Enable auth on the operator

Deploy the operator with `--ui-auth-enabled=true`. Without this flag the operator accepts
unauthenticated requests on port 8083 (suitable for local development only).

---

## Local development (no proxy)

For local development, run the operator with auth disabled and use the Vite dev server:

```bash
# Terminal 1 — operator (auth off)
make run   # starts with --ui-auth-enabled=false by default

# Terminal 2 — React dev server (proxies /api to localhost:8083)
cd ui && npm run dev
```

The Vite dev server proxies `/api` to the operator directly. No proxy binary is needed.

---

## Token rotation

The kubelet automatically refreshes the projected SA token roughly every 12 minutes (the
token has a 15-minute expiry). The UIProxy background goroutine re-reads the token file
every 30 seconds, so at most 30 seconds of requests use a slightly older-but-still-valid
token. There is no service interruption during rotation.

---

## Adding OIDC/SAML later

Add an `oauth2-proxy` or similar sidecar/annotation to the Ingress. The UIProxy itself
does not change — it remains auth-free for browsers. The Ingress layer handles user
identity and can forward `X-Auth-User` / `X-Auth-Groups` headers to the UIProxy, which
the operator can then read for fine-grained access control.
