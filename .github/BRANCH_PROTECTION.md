# Branch Protection Rules

This document describes the branch protection rules that should be configured for the `main` branch.

## Required GitHub Settings

Navigate to: **Repository → Settings → Code and automation → Branches**

### Default Branch Protection Rule

| Setting | Value | Notes |
|---------|-------|-------|
| Branch name pattern | `main` | The primary development branch |
| Require a pull request before merging | ✅ Enabled | **This is the key rule to prevent direct commits** |
| Allow force pushes | ❌ Disabled | Prevents rewriting history |
| Allow deletions | ❌ Disabled | Prevents accidental deletion |

### Pull Request Requirements

Under "Require a pull request before merging":

| Setting | Value |
|---------|-------|
| Approvals required | 1 minimum |
| Dismiss stale pull request approvals when new commits are pushed | ✅ Enabled |
| Require review from CODEOWNERS | ✅ Enabled |

### Status Checks (Required for all PRs)

| Check | Workflow File |
|-------|---------------|
| `test` | `test.yml` |
| `lint` | `lint.yml` |
| `enforce-pr-to-main` | `enforce-pr-to-main.yml` |

### Additional Settings

| Setting | Value |
|---------|-------|
| Require branches to be up to date before merging | ✅ Enabled |
| Require signed commits | ✅ Enabled |

## Implementation

Two workflows work together to enforce PR-only policy:

1. **`enforce-pr-to-main.yml`** - Runs on any push to main and fails immediately, ensuring direct pushes are blocked
2. **`protection-rules.yml`** - Runs weekly or manually to verify protection rules are properly configured

## Custom Issue Types

Enable these in **Settings → Features → Issues**:

- ✅ Bug
- ✅ Feature
- ✅ Task
- ✅ Documentation

## Pull Request Template

Use `.github/PULL_REQUEST_TEMPLATE.md` as the template for all PRs.

## Branch Naming Convention

When creating feature branches:
- Create from: `main`
- Format: `type/short-description`
  - Examples: `feat/agent-workflow`, `fix/memory-leak`, `docs/api-reference`

---

## Enforcement Checklist

- [ ] Branch protection enabled for `main` branch
- [ ] Require PR before merge enabled
- [ ] Require status checks: `test`, `lint`, `enforce-pr-to-main`
- [ ] Require signed commits enabled
- [ ] Allow force pushes disabled
- [ ] Allow deletions disabled
- [ ] Minimum 1 approval required
- [ ] Dismiss stale approvals enabled
- [ ] CODEOWNERS review required