# Security Policy

## Supported Versions

The project maintains a rolling support window. Bug fixes and security patches
are applied to the `main` branch and back-ported to the most recent minor
release line.

| Version     | Supported          |
|-------------|--------------------|
| `main`      | :white_check_mark: |
| `v0.x.*`    | :white_check_mark: (from first tagged release) |
| `< v0.1.0`  | :x:                |

We recommend always running the latest released tag. Release notes list all
known fixed CVEs in managed dependencies (k8s client-go, controller-runtime,
etc.).

## Reporting a Vulnerability

**Please do not open a public GitHub issue for security vulnerabilities.**

Report them privately instead:

- **GitHub Security Advisories (preferred):** open a confidential draft advisory
  using the "Report a vulnerability" button on the repo's
  **Security → Advisories** tab. This is the fastest route and keeps the
  discussion off the public issue tracker.

Include in your report:

1. A description of the vulnerability and impact.
2. Steps to reproduce (PoC, if possible) or the offending commit/file.
3. Whether the issue is disclosed publicly (CVD date / embargo, if any).
4. Your preferred credit reference (name / handle) and whether you want
   coordinated disclosure.

### Response Expectations

- We aim to **acknowledge receipt within 72 hours**.
- We will **triage and confirm** the issue within 7 days.
- For confirmed, high-severity issues we aim to cut a patch release within
  14 days of confirmation; lower-severity issues are fixed on the next regular
  release cadence.
- You will be notified when a fix ships and credited in the release notes
  (unless you request anonymity).

## Scope

In scope: vulnerabilities in code owned by this repository, including the
Kubernetes operator, the model-router / ui-proxy / mcp-ingester sidecars, the
`aoctl` CLI, the Python SDK, the Helm charts, and the embedded UI assets.

Out of scope: vulnerabilities that are only exploitable when a deployment is
misconfigured by the operator (e.g. exposing the UI API port publicly without
`NetworkPolicy`), or issues in upstream dependencies that are resolved simply by
upgrading `go.mod` / `npm` (those should use the appropriate upstream channels;
we will still bump the pinned dependency in response to a filed advisory).

## Hardening Notes

- The operator runs the reconciler as a non-root user inside the container
  (`USER 65532:65532` in `Dockerfile`), and Pod Security Standards are enforced
  via `internal/security/pod_security.go` with an opt-in namespace label for
  privileged agent pods (see the `AgentRuntime.securityContextOverride` docs).
- Cross-tenant isolation is enforced per-`TenantConfig`; report any bypass.
- The External Task API (`/oauth/token`, `/v1/tasks`) should always sit behind
  an ingress with TLS and the `NetworkPolicy` chart templates applied.
