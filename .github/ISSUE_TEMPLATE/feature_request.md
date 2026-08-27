name: "Feature request"
about: "Suggest an idea for agent-orca"
title: "[feat] "
labels: ["enhancement", "status: triage"]
assignees: []
body:
  - type: textarea
    id: problem
    attributes:
      label: "Problem"
      description: "What problem does this solve? What can't you do today that you want to be able to do?"
      placeholder: "I'm frustrated that ..."
    validations:
      required: true
  - type: textarea
    id: solution
    attributes:
      label: "Proposed solution"
      description: "How would you like it to work? Describe the API/CRD surface, CLI flag, or UI change if relevant."
    validations:
      required: true
  - type: textarea
    id: alternatives
    attributes:
      label: "Alternatives considered"
      description: "What else did you try or consider? Why doesn't it fit?"
    validations:
      required: false
  - type: input
    id: impact
    attributes:
      label: "Impact / users affected"
      description: "Rough sense of who this affects (e.g. operators, integrators via the ACP API, SDK consumers)."
      placeholder: "Integrators calling POST /agents/{name}/run"
    validations:
      required: false
  - type: checkboxes
    id: checks
    attributes:
      label: "Checks"
      options:
        - label: "I have searched the existing feature requests to avoid duplicates."
          required: true
