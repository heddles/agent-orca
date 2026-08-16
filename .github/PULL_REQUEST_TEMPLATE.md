## Description

<!-- What does this PR do? Why? Link to an issue if there is one (Closes #123). -->

## Type of change

- [ ] Bug fix (non-breaking change that fixes an issue)
- [ ] New feature (non-breaking, additive change)
- [ ] Breaking change (API/CRD migration path required — describe below)
- [ ] Documentation update
- [ ] Refactor / tech debt
- [ ] Tests only

## CRD / API impact

- [ ] No CRD or External API surface changed
- [ ] Additive CRD change (new optional field, new enum value, new status field)
- [ ] Non-additive CRD change (removed/renamed field, changed type, required field added)
  - If yes: document the migration path and whether a version bump is required.

## Checklist

Before requesting review:

- [ ] `make manifests generate` run and regenerated output committed (if you
      touched `api/v1alpha1/*_types.go`).
- [ ] `make fmt` / `make vet` clean.
- [ ] `make test` passes locally (Go unit + envtest).
- [ ] If this touches `ui/`: `cd ui && npm test` passes; if it adds user-facing
      behavior, Playwright (`make test-ui-e2e`) was considered.
- [ ] New user-facing behavior is documented in `docs/` and/or `README.md`.
- [ ] `CODEOWNERS` coverage applies (CI will request the right reviewers).
- [ ] Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/)
      (this helps `release-drafter` build accurate changelogs).

## For maintainers

- [ ] Label: set to `release` if this should be included in the next changelog.
