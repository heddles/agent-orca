/**
 * Ambient module declarations for static asset imports.
 *
 * The UI bundles with Vite, and `npm run build` runs `tsc && vite build`.
 * Without these declarations TypeScript reports
 * "Cannot find module './assets/logo.png' or its corresponding type
 * declarations." when we `import logoUrl from './assets/logo.png'`. Vite
 * resolves such imports to a built-time URL string; these declarations mirror
 * that contract so `tsc` is happy.
 *
 * Note: the project tsconfig sets `"types": ["vitest/globals"]`, which omits
 * Vite's bundled `vite/client` references, so we declare the asset modules we
 * actually use here rather than pulling in all of `vite/client`.
 */
declare module '*.png' {
  const src: string
  export default src
}
declare module '*.jpg' {
  const src: string
  export default src
}
declare module '*.jpeg' {
  const src: string
  export default src
}
declare module '*.svg' {
  const src: string
  export default src
}
declare module '*.gif' {
  const src: string
  export default src
}
declare module '*.webp' {
  const src: string
  export default src
}
