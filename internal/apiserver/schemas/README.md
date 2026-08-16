# OpenAPI specs for the agent-orc external HTTP APIs

These documents are the **authoritative, machine-readable contract** for integrating
with agent-orc from external systems. They are embedded at build time into the
operator binary and served live at runtime:

| API | Port | Spec file | Live URL |
|-----|------|-----------|----------|
| External Task API | 8084 | `openapi-external.yaml` | `GET /openapi.json` |
| ACP API | 8000 | `openapi-acp.yaml` | `GET /openapi.json` |

> The UI chat-proxy API on port 8080 (documented in the README as
> `/api/deployments/.../execute`) is **not** the integration surface. Use port 8084
> for programmatic task submission, or port 8000 for ACP-compatible clients.

## Regenerating / validating

```bash
make openapi          # copies specs to ./openapi/ for IDE/editor consumption
make openapi-validate # validates the specs with redocly (if installed)
```

## Authentication

Every endpoint except `POST /oauth/token` requires a bearer token (JWT or
Kubernetes ServiceAccount token). See [docs/auth.md](../docs/auth.md) for the
three accepted modes: agent-orc-issued JWTs (via this `/oauth/token`
`client_credentials` exchange), federated OIDC tokens, and in-cluster K8s SA tokens.

```
Authorization: Bearer <token>
```
