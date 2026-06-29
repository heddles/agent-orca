# UI Testing

The UI has three testing layers: unit tests, component tests, and end-to-end (E2E) tests.

## Unit and Component Tests

Unit and component tests use [Vitest](https://vitest.dev/) with [React Testing Library](https://testing-library.com/docs/react-testing-library/intro/) and run entirely in jsdom (no browser or cluster required).

### Running

```bash
# From the repo root
make test-ui

# Or from the ui/ directory
cd ui
npm test              # Single run
npm run test:watch    # Watch mode (re-runs on file changes)
npm run test:coverage # With coverage report
```

### Writing Tests

- Place test files next to the code they test with a `.test.ts` or `.test.tsx` suffix.
- Use `@testing-library/react` for rendering components and querying the DOM.
- Use `@testing-library/user-event` for simulating user interactions.
- Vitest globals (`describe`, `it`, `expect`) are available without imports, but explicit imports also work.

Example component test:

```tsx
import { render, screen } from '@testing-library/react'
import { StatusBadge } from './StatusBadge'

it('renders the phase label', () => {
  render(<StatusBadge phase="Succeeded" />)
  expect(screen.getByText('✓ Succeeded')).toBeInTheDocument()
})
```

Example unit test:

```ts
import { PHASE_COLOR } from './phaseColors'

it('maps Running to a hex color', () => {
  expect(PHASE_COLOR['Running']).toMatch(/^#[0-9a-f]{6}$/i)
})
```

## End-to-End Tests

E2E tests use [Playwright](https://playwright.dev/) and run against a deployed instance of the application. They exercise the full stack: UI served by Caddy, API proxied to the operator, and Kubernetes resources.

### Prerequisites

Install Playwright browsers (one-time):

```bash
cd ui
npx playwright install --with-deps chromium
```

### Running Locally Against a Kind Cluster

1. Start the dev cluster and deploy (if not already running):

   ```bash
   kind create cluster --name agent-orc-dev
   skaffold dev
   ```

   `skaffold dev` builds all images, deploys via Helm, and port-forwards the UI to http://localhost:8080 automatically. Leave it running in a dedicated terminal.

2. Run the tests:

   ```bash
   # From the repo root
   make test-ui-e2e

   # Or from the ui/ directory with a custom base URL
   cd ui
   BASE_URL=http://localhost:8080 npm run test:e2e
   ```

4. For interactive debugging:

   ```bash
   cd ui
   BASE_URL=http://localhost:8080 npm run test:e2e:ui
   ```

   This opens the Playwright UI, which lets you step through tests, view screenshots, and inspect the DOM.

### Writing E2E Tests

- Place test files in `ui/e2e/` with a `.spec.ts` suffix.
- Tests receive a `page` fixture — a real Chromium browser page.
- Use `page.goto('/')` to navigate (the `baseURL` from `playwright.config.ts` is prepended automatically).
- Prefer accessible locators (`getByRole`, `getByText`, `getByPlaceholder`) over CSS selectors.

Example:

```ts
import { test, expect } from '@playwright/test'

test('sidebar shows resource tabs', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByRole('button', { name: /runs/i })).toBeVisible()
})
```

### Configuration

Playwright configuration is in `ui/playwright.config.ts`. Key settings:

| Setting | Default | Override |
|---|---|---|
| Base URL | `http://localhost:8080` | `BASE_URL` env var |
| Browser | Chromium | Add projects in config |
| Timeout | 30s | `timeout` in config |
| Retries | 1 | `retries` in config |

## CI

Two GitHub Actions workflows run UI tests automatically:

- **test-ui.yml** — Runs `npm run build` and `npm test` (Vitest) on pushes and PRs that touch `ui/`.
- **test-ui-e2e.yml** — Creates a Kind cluster, deploys the app with Helm, and runs Playwright tests on all pushes and PRs.

Failed E2E runs upload Playwright traces as artifacts for debugging.
