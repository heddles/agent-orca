# OpenAPI Specification

agent-orc publishes two machine-readable OpenAPI specifications that describe
every HTTP endpoint on its two external API surfaces. You can use them to
generate client SDKs, validate requests, or explore the API interactively.

## The two specs

| Spec | Surface | Port | Source file | Runtime URL |
|---|---|---|---|---|
| **External Task API** | Programmatic task submit/poll/stream/cancel | 8084 | `internal/apiserver/schemas/openapi-external.yaml` | `GET http://localhost:8084/openapi.json` |
| **ACP API** | Agent discovery + self-service run (ACP-compatible) | 8000 | `internal/apiserver/schemas/openapi-acp.yaml` | `GET http://localhost:8000/openapi.json` |

Both specs are served at runtime from the same in-process YAML files — the
`.json` endpoint is the YAML rendered as JSON, so there is never a
drift between the spec and the running server.

## Fetching at runtime

```bash
# External Task API (port 8084)
curl http://localhost:8084/openapi.json

# ACP API (port 8000)
curl http://localhost:8000/openapi.json
```

The `/openapi.json` endpoint is in the `publicPaths` set and does **not**
require authentication, so you can fetch it without a bearer token.

## Generating client SDKs

Use any OpenAPI generator (openapi-generator-cli, Swagger Codegen, or your
language's generator of choice):

```bash
npx --package=@openapitools/openapi-generator-cli openapi-generator-cli generate \
  -i internal/apiserver/schemas/openapi-external.yaml \
  -g go \
  -o ./sdk/external-go

npx --package=@openapitools/openapi-generator-cli openapi-generator-cli generate \
  -i internal/apiserver/schemas/openapi-acp.yaml \
  -g python \
  -o ./sdk/acp-python
```

The `aoctl` CLI is itself generated from the external spec types (see
[`aoctl-reference.md`](aoctl-reference.md)).

## Building & validating locally

```bash
# Copy specs to ./openapi/ for consumption by tooling
make openapi

# Validate the specs (requires redocly or swagger-cli)
make openapi-validate
```

## Spec maintenance

The specs live alongside the code in `internal/apiserver/schemas/`. They are
hand-maintained YAML (not auto-generated from Go types) so that the API
contract is explicit and reviewable in code review. When you add or change an
endpoint, update the corresponding `.yaml` file and run `make openapi-validate`
before committing.
