# OIDC Login (interactive, tenant-driven)

agent-orca supports **interactive OIDC login** for browser access. Each federated
`TenantConfig` that carries a `clientSecretRef` (+ `redirectURI`) is a selectable
identity provider — so **each org logs in through its own IdP** (Google, Okta,
Keycloak, …). On success the operator mints a short-lived agent-orca-issued JWT
carried in an `HttpOnly` session cookie, validated by the **same** `requireAuth` /
`ExternalAuth` middleware used by bearer tokens. **RBAC is not implemented** — the
identity carries `UserID`/`Groups`/`Roles` so authorization can be added later as a
handler-side check.

> **Multi-tenant by design.** Per-tenant issuer/client_id/client_secret/redirect/
> claim mappings live on the `TenantConfig` CRD (in the tenant's `targetNamespace`),
> *not* operator-wide. A browser-facing `Bearer`/SA token does **not** grant UI
> access when login is enabled — the UI is never shown without a successful login.

## Surfaces

| Surface | Port | Login endpoint | Delivery |
|---------|------|----------------|----------|
| UI API (via UIProxy) | 8080 / 8083 | `/oauth/login`, `/oauth/callback`, `/oauth/logout` | `HttpOnly` session cookie |
| External Task API | 8084 | *(bearer only)* | `Authorization: Bearer <jwt>` |
| ACP API | 8000 | *(bearer only)* | `Authorization: Bearer <jwt>` |

The External/ACP APIs already reject any request without a valid bearer token; the
OIDC bearer tokens they accept come from a `federated` `TenantConfig` (issuer + JWKS
verification). Interactive *browser* login is a UI-only feature; programmatic callers
use `/oauth/token` (client_credentials) or a federated bearer JWT.

## How a browser logs in

```
Browser  -> /oauth/login                -> picker of federated (login-capable) tenants
Browser  -> /oauth/login?tenant=org-x   -> 302 to IdP (state carries tenant X)
Browser <-> IdP login (Google/Keycloak/…)
IdP      -> /oauth/callback?code=..&state=..  (state cookie checked; CSRF-safe)
operator  * state -> tenant X -> read X's client_secret from its targetNamespace
operator  * POST code+secret to IdP -> id_token
operator  * verify id_token (JWKS signature, iss, aud, exp, nonce) + email_verified
operator  * ResolveFederatedTenant(issuer, claims)  -> TenantIdentity (tenant/namespace/agents)
operator  * MintSessionForIdentity -> signed session JWT -> Set-Cookie: agentorca_session
Browser  -> /api/* (cookie sent; requireAuth validates it -> 200)
```

The `state` value (CSRF) carries the tenant name so the callback uses **that
tenant's** IdP and secret. NewProviderForTenant builds the `oidc.Provider` per
tenant at login time.

## Tenant config reference

```yaml
apiVersion: agentorca.agentorca.io/v1alpha1
kind: TenantConfig
metadata:
  name: google-oidc
  namespace: agent-orca-system
spec:
  authMode: federated
  targetNamespace: default          # where tasks run; also where the client secret lives
  federated:
    issuerURL: "https://accounts.google.com"
    clientID: "<your-oauth-web-client-id>"
    redirectURI: "http://localhost:8083/oauth/callback"
    clientSecretRef:
      name: oidc-client-secret       # Secret in spec.targetNamespace
      key: client-secret
    claimMappings:
      userid: email                  # email -> TenantIdentity.UserID (default: sub)
    # matchClaim/matchValue omitted => issuer-only trust (any verified login admitted).
    # Set matchClaim/matchValue to gate on a claim (e.g. repository_owner for GitHub,
    # hd for a Google Workspace domain, or an IdP "groups"/"orgs" claim).
  rateLimit:
    requestsPerMinute: 60
    concurrentRuns: 4
```

**Fields of note:**
- `targetNamespace` — scopes the user's tasks/agents; the OAuth client secret is
  read from here (or `clientSecretRef.namespace` if set).
- `clientSecretRef` — optional. **Present ⇒** this tenant drives interactive login
  (the operator becomes an OAuth client). **Absent ⇒** bearer-only federation
  (callers present an IdP-issued JWT; verified via the public JWKS — no secret).
- `redirectURI` — the callback URL registered at the IdP. In local dev this is
  `http://localhost:8083/oauth/callback` (skaffold forwards `:8083`). Behind TLS it's
  the public callback.
- `claimMappings` — which IdP claims map onto `TenantIdentity`. Default
  `sub`/`groups`/`email`; set `userid: email` for Google.

## Enabling (Helm)

```yaml
# values.yaml
operator:
  oidc:
    enabled: true
    cookieSecure: true          # true behind TLS; false for plain-HTTP local dev
```

```bash
helm upgrade --install agent-orca charts/agent-orca \
  --namespace agent-orca-system \
  --set operator.oidc.enabled=true \
  --set operator.oidc.cookieSecure=true
```

## Local dev (skaffold + your own IdP)

1. Start the dev cluster: `skaffold dev -p dev` (this also forwards `:8083`).
2. Deploy your IdP (e.g. Keycloak) and port-forward it yourself.
3. Create the OAuth client secret in the tenant namespace and a login-capable
   `TenantConfig` (example: `config/samples/dev-tenant-google.yaml`). A ready-made
   overlay + secret sample live at `charts/agent-orca/values-dev-oidc.yaml` and
   `config/samples/dev-oidc-secret.yaml`:
   ```bash
   helm upgrade agent-orca charts/agent-orca -n agent-orca-system \
     -f charts/agent-orca/values-dev-oidc.yaml     # enabled=true, cookieSecure=false
   kubectl apply -f config/samples/dev-tenant-google.yaml
   kubectl create secret generic oidc-client-secret -n default \
     --from-literal=client-secret='<your-idp-client-secret>'
   ```
4. Open `http://localhost:8080/oauth/login` (proxied by the UIProxy, which now
   forwards `/oauth/*`) and pick your tenant.

### Local dev topology note
- The **browser** reaches the callback at `http://localhost:8083/oauth/callback`
  (skaffold forward); that's the IdP's registered redirect URI.
- The **operator pod** (in kind) must reach the IdP for JWKS discovery + token
  introspection. With both on the same host, the IdP issuer is
  `http://host.docker.internal:<port>/…` so the kind pod can resolve it.
- If your cluster enforces a default-deny egress, the operator can't reach the IdP
  until you allow it. Enable the opt-in egress on the operator NetworkPolicy (dev
  default allows everything from the pod; narrow to the IdP CIDR in production):
  ```yaml
  operator:
    networkPolicy:
      oidcEgress:
        enabled: true        # set when the pod can't already reach the IdP
        cidrs: ["0.0.0.0/0"] # dev; restrict to the IdP host CIDR in production
  ```
  The policy also retains egress to the kube-apiserver (443) and cluster DNS (53)
  so TokenReview keeps working once egress is isolated.
- `cookieSecure: false` is required for `http://` (a `Secure` cookie is not sent
  over plain HTTP).
- **DNS from the pod, not just egress:** `test.keycloak.org` must resolve *inside*
  the kind node (e.g. via `/etc/hosts` on the node or a resolvable host). If it
  resolves on your laptop but not in the cluster, use a host-reachable issuer such
  as `http://host.docker.internal:<port>/realms/master` instead.

## Cookie model & the UIProxy (BFF)

The UIProxy (`cmd/ui-proxy/main.go`) reverse-proxies `/api/*` **and** `/oauth/*`.
Its `Director` strips only `Authorization` (to inject the SA token); `ReverseProxy`
forwards `Cookie`/`Set-Cookie` by default, so the session cookie round-trips. With
login enabled, `requireAuth` prefers the session cookie and **ignores** the SA token,
so the UI is gated on the OIDC login (not on cluster-internal proximity).

## Google OIDC bearer tokens (GitHub Actions style)

For programmatic callers, a federated bearer tenant also works. Google's OIDC
issuer is `https://token.actions.githubusercontent.com` (Actions), with the user's
login in the `actor` claim — see
[`config/samples/dev-tenant.yaml`](config/samples/dev-tenant.yaml) and
[enterprise-integration.md](enterprise-integration.md#github-oidc).

## Future RBAC design

This change is **authentication only**. `TenantIdentity` already carries
`UserID`/`Groups`/`Roles` (empty until a role mapper exists). A future layer can
resolve `Groups → Roles` from the IdP claim and gate handlers with
`if !slices.Contains(identity.Roles, "admin")` — no auth-path rewrite, since the
identity is already injected into context by `requireAuth` / `ExternalAuth.Middleware`.
