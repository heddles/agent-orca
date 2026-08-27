name: "Report a bug"
about: "File a bug report for agent-orca"
title: "[bug] "
labels: ["bug", "status: triage"]
assignees: []
body:
  - type: markdown
    attributes:
      value: |
        Thanks for filing a bug! If this is a security vulnerability, please
        **do not** use this form — see our [Security Policy](../SECURITY.md)
        and report it via GitHub Security Advisories or email instead.
  - type: textarea
    id: description
    attributes:
      label: "Description"
      description: "A clear and concise description of the bug."
    validations:
      required: true
  - type: textarea
    id: steps
    attributes:
      label: "To Reproduce"
      description: "Steps to reproduce the behavior."
      placeholder: "1. `kubectl apply -f ...`\n2. `curl ...`\n3. See error ..."
    validations:
      required: true
  - type: textarea
    id: expected
    attributes:
      label: "Expected behavior"
      description: "What you expected to happen."
    validations:
      required: true
  - type: textarea
    id: actual
    attributes:
      label: "Actual behavior / error output"
      description: "What actually happened, including any error messages or log excerpts."
    validations:
      required: false
  - type: input
    id: version
    attributes:
      label: "agent-orca version / commit"
      description: "The tag or commit hash you're running (or `main`, or `skaffold dev`)."
      placeholder: "v0.1.0, or main@abc1234"
    validations:
      required: false
  - type: input
    id: kubernetes
    attributes:
      label: "Kubernetes version / environment"
      description: "e.g. kind v0.23 / k8s v1.33, or distro + version."
      placeholder: "kind + k8s v1.33"
    validations:
      required: false
  - type: checkboxes
    id: checks
    attributes:
      label: "Checks"
      options:
        - label: "I have searched the existing issues to make sure this is not a duplicate."
          required: true
        - label: "I have reproduced this against the latest `main` (or a recent release)."
          required: false
