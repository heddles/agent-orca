# Contributing to agent-orca

First off: thank you for taking the time to contribute! ❤️

This document explains how to get your change into `main`. It focuses on
**process and conventions**. For environment setup, build/deploy commands, and
local debugging, see the [Development Guide](docs/development.md).

By participating in this project you agree to abide by the
[Code of Conduct](CODE_OF_CONDUCT.md).

---

## Table of Contents

- [How to Get Your Change Merged](#how-to-get-your-change-merged)
- [Branch Protection Rules](#branch-protection-rules)
- [Toolchain](#toolchain)
- [Quick Build / Test / Lint Reference](#quick-build--test--lint-reference)
- [Code Generation (the easy way to break CI)](#code-generation-the-easy-way-to-break-ci)
- [Code Style](#code-style)
- [Where to Start](#where-to-start)
- [Repository Layout at a Glance](#repository-layout-at-a-glance)

---

## How to Get Your Change Merged

1. **File an issue first** for anything non-trivial (bugs, security, API
   changes, CRD schema changes). This lets us converge on design before you
   write code and avoids rework. If you're unsure, open a [blank issue][blank]
   or ask in [discussions][discussions].
2. **Fork & branch.** Fork the repo, then branch from `main`:
   ```
   git checkout -b fix/my-short-description main
   ```
   Branch name prefixes we use (feel free to match them):
   `fix/`, `feat/`, `chore/`, `docs/`, `refactor/`, `perf/`, `test/`, `ci/`.
3. **Make the change**, keeping commits focused (one concept per commit is
   nicer to review than a 3000-line dump).
4. **Run the checks locally** (see [below](#quick-build--test--lint-reference))
   before pushing — especially `make generate` and `make fmt`, which are easy
   to skip and easy to get wrong.
5. **Push & open a PR** against `main`. The PR description should:
   - reference the issue it closes (`Closes #123`),
   - summarize *what* changed and *why* (the "why" matters more than the "how"),
   - show before/after for API/CRD changes,
   - note any migration steps operators need (e.g. `kubectl apply` ordering).
6. **CI must be green** on: `test.yml` (Go unit + envtest), `lint.yml`,
   `test-e2e.yml` (integration), and `test-ui.yml` (if you touched `ui/`).
   A maintainer will review + approve.

Reference-level changes to CRDs (`api/v1alpha1/*_types.go`) need an explicit
sign-off in the PR that the schema is backward-compatible (additive) or that a
[version bump + migration path](docs/crds.md) is documented. This is a
Kubernetes operator consumed by tenants; breaking CRD changes are rare and
reviewed carefully.

[blank]: https://github.com/heddles/agent-orca/issues/new/choose
[discussions]: https://github.com/heddles/agent-orca/discussions

---

## Branch Protection Rules

The `main` branch is protected. **Direct commits are not allowed** - all changes
must go through Pull Requests.

### Required Settings

| Setting | Value |
|---------|-------|
| Require a pull request before merging | ✅ Required |
| Require approvals | 1 minimum |
| Dismiss stale PR approvals on new commits | ✅ Enabled |
| Require review from CODEOWNERS | ✅ Enabled |
| Require status checks | ✅ Required |
| Require branches to be up to date | ✅ Enabled |
| Require signed commits | ✅ Enabled |
| Allow force pushes | ❌ Disabled |
| Allow deletions | ❌ Disabled |

### Required Status Checks

Before a PR can be merged, all of these must pass:

- `test.yml` - Go unit tests
- `lint.yml` - golangci-lint
- `enforce-pr-to-main.yml` - Branch protection enforcement
- `check-branch-naming.yml` - Branch naming convention

### Branch Naming Convention

When creating feature branches, use this pattern:

```
<type>/<short-description>
```

Valid types:
- `feat/` - New feature
- `fix/` - Bug fix  
- `docs/` - Documentation
- `chore/` - Maintenance
- `refactor/` - Code refactoring
- `test/` - Test changes
- `perf/` - Performance improvements
- `ci/` - CI/CD changes

Examples:
```
git checkout -b feat/add-rag-support main
git checkout -b fix/memory-leak-in-router main
git checkout -b docs/api-reference main
```

See `.github/BRANCH_PROTECTION.md` for complete documentation.

---

## Toolchain

The repo uses [`mise`](https://mise.jdx.dev) to pin the local dev toolchain. With
mise installed, one command provisions everything in the table below (except
Docker):

```bash
mise install                 # install/pin all dev tools
# optional: auto-activate tools on cd into this repo:
#   eval "$(mise activate zsh)"   # or bash/fish
```

You can also run the Makefile front doors as `mise run build|test|lint|run`,
which guarantees the pinned toolchain is active on `PATH`. The versions below
are what `.mise.toml` pins (with `latest` where the project documents the tool
as latest; bump with `mise use <tool>@latest`):

| Tool       | Version | Notes |
|------------|---------|-------|
| Go         | 1.25+   | `go.mod` pins `1.25.3` (required by `k8s.io/* v0.35`). `go-version-file: go.mod` is used in CI. |
| Node       | 20+     | Drives what's under `ui/` and `pkg/python/agentorca/`'s smoke tests. |
| GNU Make   | 4.x     | The Makefile is the front door to everything. |
| kind       | latest  | E2E and local dev clusters. |
| kubectl    | latest  | Cluster interaction / goldens in `test/e2e`. |
| helm       | 3.14+   | Chart rendering / `make install`. |
| skaffold   | latest  | Local dev loop (`skaffold dev`). |
| Docker     | latest  | Image builds for e2e/dev. Not managed by mise — install Docker Desktop (or the engine) separately. |

You do **not** need to install the Kubernetes code-gen tools by hand.
The
Makefile downloads `controller-gen`, `kustomize`, and `golangci-lint` into
`./bin/` on first use (see `hack/` and the `*Tool` targets at the bottom of the
Makefile), so a fresh checkout builds with just `mise install` + `make build`.

---

## Quick Build / Test / Lint Reference

All of these are `make` targets:

```bash
make build              # Builds all binaries (operator, model-router, mcp-ingester, ui-proxy, aoctl)
make test               # manifests + generate + fmt + vet, then Go unit/envtest suite
make test-e2e           # Integration suite against an isolated kind cluster (slow; ~5-8 min)
make test-ui            # UI unit + component tests (Vitest)
make test-ui-e2e        # Playwright browser tests (needs kind + built images)
make lint               # golangci-lint (configs under .golangci*)
make manifests          # Regenerate CRDs, RBAC, webhook manifests -> config/crd, config/rbac, charts/agent-orca/crds
make generate           # DeepCopy methods for CRD types
```

Useful one-liners from the repo root:
```bash
make manifests generate && make test        # "did my CRD change produce the right output?"
make fmt && make vet                        # quick local sanity before push
make lint-fix                               # auto-fix what golangci-lint can
cd ui && npm run typecheck                  # UI type-check
```

> The `test-e2e` target creates and destroys a dedicated kind cluster. **Never
> point it at a real cluster.** See `docs/development.md` §"E2E Tests Require an
> Isolated Kind Cluster".

---

## Code Generation (the easy way to break CI)

This is the landmine that bites most new contributors. The repo commits
generated artifacts, so **every** change to a CRD type in `api/v1alpha1/` *must*
be followed by regeneration, and the regenerated output must be in your PR:

```bash
make manifests   # regenerates config/crd/bases/*.yaml, config/rbac/role.yaml, charts/agent-orca/crds/*.yaml
make generate    # regenerates api/v1alpha1/zz_generated.deepcopy.go (+ any other zz_generated.*)
```

If you change `api/v1alpha1/*_types.go`, your PR will fail CI's diff check unless
these generated files are updated. The `make test` target already runs both
`manifests` and `generate` as dependencies, so running `make test` locally is the
fastest way to catch a stale generation before you push.

Likewise: editing `internal/apiserver/schemas/openapi-*.yaml` should be followed
by `make openapi`; editing Helm charts under `charts/` can be validated with
`make lint-demos`.

---

## Code Style

- **Go:** `gofmt` + `goimports` (enforced by CI). The project already pins
  `golangci-lint` v2 in `./bin`. Run `make fmt` before pushing.
  License headers follow `hack/boilerplate.go.txt` — every `.go` source file
  must start with the Apache 2.0 header (`/*
Copyright 2026.
...
*/`).
  Generated `zz_generated.*` files get the header automatically from
  `controller-gen --header-file`.
- **Helm/Go templates:** `helm lint` on each chart under `charts/`.
- **UI (TypeScript/React):** `cd ui && npm run typecheck && npm test`.
  Prettier/ESLint are configured under `ui/.eslintrc`/`ui/.prettierrc`.
- **Commits:** we lean toward [Conventional Commits](https://www.conventionalcommits.org/)
  (e.g. `feat(crd): add GuardrailPolicy.status`, `fix(runner): don't drop stream on 500`).
  This isn't hard-enforced yet, but it's what `release-drafter` consumes to
  auto-generate changelogs.

---

## Where to Start

Got 30 minutes and new here? Pick an issue tagged
[`good first issue`](https://github.com/heddles/agent-orca/issues?q=is%3Aopen+is%3Aissue+label%3A%22good+first+issue%22).
Typical starter topics:

- small bugfix in `aoctl` (`cmd/aoctl/`) or a doc gap,
- adding a golden test to `test/e2e/`,
- reviewing a `docs/` page for accuracy against current code.

If nothing jumps out, comment on a `good first issue` and a maintainer will
point you at a starter task.

---

## Repository Layout at a Glance

```
agent-orca/
├── cmd/                # Binaries: main (operator), aoctl, model-router, mcp-ingester, ui-proxy
├── api/v1alpha1/       # CRD types (+kubebuilder markers) + generated zz_*.deepcopy.go
├── internal/           # controllers, webhooks, apiserver, podbuilder, security, router, rag, mcp, ...
├── config/             # kustomize bases for CRDs, RBAC, webhook, manager, samples
├── charts/             # Helm charts: agent-orca, model-providers, agent-orca-resources, demos/*
├── test/               # test/e2e (integration), test/utils
├── ui/                 # React + Vite UI (Vitest + Playwright)
├── pkg/python/         # Python SDK (agentorca) + smoke tests
├── examples/           # terraform tenant example + agent-sdk-template
├── hack/               # dev scripts + license boilerplate.txt
├── docs/               # full documentation index
├── Dockerfile*         # builder images for each binary
├── skaffold.yaml       local dev loop (dev/demo profiles)
└── Makefile            single entry point for build/test/lint/codegen
```

Colima / Docker Desktop is assumed for `kind` and image builds. On macOS with
Apple Silicon, the skaffold `dev` profile runs `kind` under Docker — no extra
architecture config is needed (the `Dockerfile` multi-arches via `TARGETARCH`).

Happy hacking! 🚀