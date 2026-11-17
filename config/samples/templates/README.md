# Agent templates

Ready-made agents that appear in the UI under **Agents, Templates**. People can
read them and see what they include. A template is kept in Git by the platform
team, so it is read only in the UI. Copying a template into a team's own
workspace is "Use template" (`POST /api/templates/{ns}/{name}/use`, see `docs/agent-builder.md`).

| Template | What it does | Based on |
|---|---|---|
| `financial-analysis` | Scenario analysis of commodity and market effects | `charts/demos/demo-financial-analysis` |
| `research-assistant` | Structured, sourced research brief | `charts/demos/demo-llm-research` |
| `on-call-triage` | First-minutes incident triage, asks before risky steps | `charts/demos/demo-soc-triage` |
| `security-alert-triage` | Scores a security alert, asks an analyst to approve | `charts/demos/demo-soc-triage` |
| `document-summarizer` | Summaries for a chosen audience and length | general starting point |
| `codebase-expert` | Senior-engineer chat for code questions and reviews | `charts/demos/demo-programming-agent` |
| `coder-agent` | Changes and tests a repository in an isolated sandbox | `config/samples/coder-agent.yaml` |

All of them use the `default` ModelSelector and the `openai-reference` runtime
image, and live in `agent-orca-system`, the default platform catalog namespace. They have no external dependencies, with
two exceptions: `coder-agent` needs OpenShell sandboxes enabled in its namespace
(see `docs/coder-agent.md`).

Using a template copies it into the team's namespace, and the references in a
template are same-namespace names. The namespace therefore needs a ModelSelector
named `default` (or the caller names another with `replace`, see
`docs/agent-builder.md`), and any KnowledgeBase, GuardrailPolicy or Tool a
template names outside its group. Without them "Use template" answers 422 and
creates nothing.

## How a template is marked

The catalog finds templates by labels and reads its text from annotations.

| Where | Key | Value | Meaning |
|---|---|---|---|
| Agent | `agentorca.io/template` | `"true"` | This Agent is a template. It is listed under Templates, not in the agent catalog. |
| Agent and companions | `agentorca.io/scope` | `platform` | Owned by the platform, readable by every tenant that may use the namespace, changeable by none of them. |
| Agent, AgentDeployment, Tools, GuardrailPolicy | `agentorca.io/template-group` | the template name | Ties the pieces of one template together. The details page lists the AgentDeployments and Tools in the group. |
| Agent | `agentorca.io/maintainer` (annotation) | text | Shown as "Maintained by" on the card. Not to be confused with `agentorca.io/owner`, which the UI API reserves for the source of truth (`ui` or `git`). |
| Agent | `agentorca.io/description` (annotation) | text | One or two plain sentences on what the agent does. Without it the start of the system prompt is shown. |
| Agent | `agentorca.io/example-inputs` (annotation) | JSON array of strings | Example inputs, shown on the card and as one-click suggestions on the run form. |
| Agent | `agentorca.io/input-schema` (annotation) | JSON Schema object | Shapes the run form. Without it the form is one free-text box. |

The same description, example and schema annotations work on any Agent, not only
templates, so a team can make its own agents friendly to use.

### Input schema

The form supports `string`, `number`, `integer`, `boolean`, `enum`, arrays of
strings, and objects nested one level deep, with `required`, `title`,
`description` (shown as help text), `default`, `minimum` and `maximum`. Anything
else is edited as JSON in that one field. When a form has more than the single
`input` text, the agent receives the filled-in values as a JSON object, so the
system prompt should say which fields to expect (the templates here do).

## Curating templates with GitOps

1. Keep the template files in the platform repository, for example under a
   `templates/` directory that Argo CD or Flux syncs to the template namespace.
2. Each template is one file with the Agent, plus the AgentDeployment (for chat)
   and any Tools or GuardrailPolicy it needs, all carrying the same
   `agentorca.io/template-group` label.
3. Review changes as pull requests. A merge updates the template for everyone;
   copies people already made are not changed.
4. Retire a template by deleting its files. Remove the `agentorca.io/template`
   label to keep the agent but stop listing it as a template.
5. Put the files in a platform catalog namespace: the operator's own namespace
   by default, or those listed in the Helm value `operator.platformNamespaces`
   (`PLATFORM_NAMESPACES`). There, every tenant can read objects labelled
   `agentorca.io/scope: platform` without claiming the namespace. Everything
   else in that namespace stays hidden from tenants. Tenants are never allowed
   to write `agentorca.io/scope: platform` themselves.
6. Templates cannot be run in place: the API answers 409 and says to use the
   template. This keeps tenants from starting runs in the platform namespace.

Apply them with `kubectl apply -f config/samples/templates/`. Run the checks
with `go test ./config/samples/templates/`: they decode every file strictly
against the API types, check the labels and annotations above, and, when envtest
binaries are available, create each object on a real API server with the CRDs
installed.
