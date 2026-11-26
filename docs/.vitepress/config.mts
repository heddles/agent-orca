import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vitepress'
import { githubSlug, literalBraces, mermaidFences, repoLinks } from './markdown'
import { sidebar } from './sidebar'

const docsDir = fileURLToPath(new URL('..', import.meta.url))
const repo = 'https://github.com/heddles/agent-orca'
// The release being published, set by .github/workflows/docs.yml.
const ref = process.env.DOCS_REF || 'main'

export default defineConfig({
  title: 'agent-orca',
  description: 'A Kubernetes-native platform for deploying, managing and running AI agents.',
  lang: 'en-US',
  cleanUrls: true,
  sitemap: { hostname: 'https://docs.agentorca.org' },
  srcExclude: ['graphify-out/**'],
  ignoreDeadLinks: 'localhostLinks',
  head: [['link', { rel: 'icon', href: '/logo.png' }]],
  markdown: {
    anchor: { slugify: githubSlug },
    config(md) {
      repoLinks(md, docsDir, repo, ref)
      mermaidFences(md)
      literalBraces(md)
    },
  },
  themeConfig: {
    logo: '/logo.png',
    nav: [
      { text: ref, link: ref === 'main' ? `${repo}/tree/main` : `${repo}/releases/tag/${ref}` },
      { text: 'Changelog', link: `${repo}/blob/${ref}/CHANGELOG.md` },
    ],
    sidebar: sidebar(docsDir),
    outline: [2, 3],
    search: { provider: 'local' },
    socialLinks: [{ icon: 'github', link: repo }],
    editLink: { pattern: `${repo}/edit/main/docs/:path`, text: 'Edit this page on GitHub' },
    footer: { message: 'Released under the Apache 2.0 License.' },
  },
})
