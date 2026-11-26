import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import type { DefaultTheme } from 'vitepress'

// The menu. Each entry is a file in docs/ without ".md"; its label is the
// page's "# " heading. A page that is not listed here still appears, under
// "Other", so a new doc is never missing from the menu.
const groups: { text: string; pages: string[] }[] = [
  {
    text: 'Getting started',
    pages: [
      'installation', 'architecture', 'demos', 'local-model-selection', 'redis',
      'KNOWN_ISSUES',
    ],
  },
  {
    text: 'Building agents',
    pages: [
      'crds', 'agent-images', 'agentworkflow', 'rag', 'context-management',
      'revisions', 'schedules', 'evaluations', 'switchyard-routing',
      'coder-agent', 'claude-code-subscription',
    ],
  },
  {
    text: 'Tools and sandboxes',
    pages: [
      'openshell-sandbox', 'sandbox-shell', 'agent-run-tools',
      'local-terminal', 'filesystem-access', 'tool-side-effects', 'mcp-apps',
      'mcp-access-control', 'mcp-oauth',
    ],
  },
  {
    text: 'Integrations',
    pages: [
      'integrating', 'integrations', 'inbound-channels', 'chat-channels',
      'egress-sinks', 'enterprise-integration',
    ],
  },
  {
    text: 'Security and tenancy',
    pages: [
      'auth', 'oauth-login', 'tenant-isolation', 'tenant-migration',
      'serviceaccount-iam', 'hindsight-auth', 'ui-proxy',
    ],
  },
  {
    text: 'Operations',
    pages: [
      'observability', 'cost-tracking', 'run-budgets', 'rate-limiting',
      'run-archival', 'gitops-coexistence', 'upgrading-qdrant',
      'upgrading-v0.13',
    ],
  },
  {
    text: 'API and CLI reference',
    pages: [
      'acp-api', 'admin-api', 'agent-builder', 'openapi-spec',
      'aoctl-reference',
    ],
  },
  {
    text: 'Project and contributing',
    pages: [
      'development', 'testing-tools', 'ui-testing',
      'vector-database-selection', 'alternatives',
    ],
  },
]

function title(docsDir: string, page: string): string {
  const source = readFileSync(join(docsDir, `${page}.md`), 'utf8')
  const heading = source.match(/^# (.+)$/m)
  return heading ? heading[1].replace(/`/g, '').trim() : page
}

export function sidebar(docsDir: string): DefaultTheme.SidebarItem[] {
  const present = readdirSync(docsDir)
    .filter((f) => f.endsWith('.md') && f !== 'index.md')
    .map((f) => f.slice(0, -'.md'.length))
  const listed = new Set(groups.flatMap((g) => g.pages))

  const missing = [...listed].filter((p) => !present.includes(p))
  if (missing.length > 0) {
    throw new Error(`sidebar.ts lists pages that do not exist: ${missing.join(', ')}`)
  }

  const other = present.filter((p) => !listed.has(p)).sort()
  const all = other.length > 0 ? [...groups, { text: 'Other', pages: other }] : groups

  return all.map((g) => ({
    text: g.text,
    collapsed: true,
    items: g.pages.map((p) => ({ text: title(docsDir, p), link: `/${p}` })),
  }))
}
