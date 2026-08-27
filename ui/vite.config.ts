/// <reference types="vitest" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/api': 'http://localhost:8080',
    },
  },
  build: {
    outDir: 'dist',
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: './src/test/setup.ts',
    exclude: ['e2e/**', 'node_modules/**'],
    coverage: {
      provider: 'v8',
      include: ['src/api/traceStream.ts', 'src/components/TraceAccordion.tsx', 'src/lib/designSystem.ts', 'src/lib/primitives.tsx'],
      thresholds: {
        lines: 80,
        branches: 78,
        functions: 80,
        statements: 80,
      },
    },
  },
})
