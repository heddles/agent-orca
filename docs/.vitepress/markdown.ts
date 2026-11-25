import { existsSync, statSync } from 'node:fs'
import { dirname, relative, resolve, sep } from 'node:path'
import type { MarkdownRenderer } from 'vitepress'

// Docs link to source files with relative paths, which work on GitHub but lead
// nowhere on the site. Point every link that leaves docs/, or targets a file
// that is not a page, at that file on GitHub for the ref being published.
export function repoLinks(md: MarkdownRenderer, docsDir: string, repo: string, ref: string) {
  const repoRoot = resolve(docsDir, '..')

  md.core.ruler.push('repo-links', (state) => {
    const page: string = state.env.relativePath ?? ''

    for (const token of state.tokens.flatMap((t) => t.children ?? [])) {
      if (token.type !== 'link_open') continue
      const href = token.attrGet('href')
      if (!href || /^([a-z][a-z0-9+.-]*:|\/|#)/i.test(href)) continue

      const [path, anchor] = href.split('#')
      const target = resolve(docsDir, dirname(page), decodeURI(path))
      const inDocs = !relative(docsDir, target).startsWith('..')
      // Pages, including links VitePress will reject as dead, are left to it.
      if (inDocs && (path.endsWith('.md') || !existsSync(target))) continue

      const fromRoot = relative(repoRoot, target).split(sep).join('/')
      if (fromRoot.startsWith('..') || !existsSync(target)) {
        throw new Error(`${page}: broken link "${href}"`)
      }
      const kind = statSync(target).isDirectory() ? 'tree' : 'blob'
      token.attrSet('href', `${repo}/${kind}/${ref}/${fromRoot}${anchor ? `#${anchor}` : ''}`)
    }
  })
}

// Render ```mermaid fences as diagrams, as GitHub does.
export function mermaidFences(md: MarkdownRenderer) {
  const fence = md.renderer.rules.fence!
  md.renderer.rules.fence = (tokens, idx, options, env, self) => {
    const token = tokens[idx]
    if (token.info.trim() !== 'mermaid') return fence(tokens, idx, options, env, self)
    return `<Mermaid code="${encodeURIComponent(token.content)}" />`
  }
}

// VitePress compiles pages as Vue templates, so a literal {{ ... }}, common in
// docs about templating, would be evaluated and break the build. Code fences
// are already exempt; exempt inline code and prose too.
export function literalBraces(md: MarkdownRenderer) {
  const inlineCode = md.renderer.rules.code_inline!
  md.renderer.rules.code_inline = (tokens, idx, options, env, self) => {
    tokens[idx].attrSet('v-pre', '')
    return inlineCode(tokens, idx, options, env, self)
  }

  const text = md.renderer.rules.text!
  md.renderer.rules.text = (tokens, idx, options, env, self) => {
    const html = text(tokens, idx, options, env, self)
    return tokens[idx].content.includes('{{') ? `<span v-pre>${html}</span>` : html
  }
}

// Name heading anchors the way GitHub does, so a "page.md#section" link works
// both on GitHub and on the site.
export function githubSlug(heading: string): string {
  return heading
    .trim()
    .toLowerCase()
    .replace(/[^\p{L}\p{M}\p{N}\p{Pc} -]/gu, '')
    .replace(/ /g, '-')
}
