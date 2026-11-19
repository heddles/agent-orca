import { test, expect } from '@playwright/test'

// Runs against a real cluster. Set BASE_URL and E2E_SESSION (an agent-orca
// session token for a throwaway tenant); skipped otherwise.
const session = process.env.E2E_SESSION
test.skip(!session, 'E2E_SESSION not set')

const pages = [
  '/', '/agents', '/agents/templates', '/agents/published/ui-e2e/e2e-helper', '/runs', '/approvals',
  '/build/new', '/build/ui-e2e/e2e-helper', '/evaluate', '/evaluate/1', '/evaluate/experiments',
  '/evaluate/experiments/1', '/operate/fleet', '/operate/schedules', '/operate/costs', '/settings/providers',
]

test.beforeEach(async ({ context, baseURL }) => {
  await context.addCookies([{ name: 'agentorca_session', value: session!, url: baseURL! }])
})

for (const path of pages) {
  test(`renders ${path} without failed API calls`, async ({ page }) => {
    const bad: string[] = []
    const errors: string[] = []
    page.on('response', (r) => { if (r.url().includes('/api/') && r.status() >= 400) bad.push(`${r.status()} ${r.url()}`) })
    page.on('pageerror', (e) => errors.push(e.message))
    await page.goto(path)
    await page.waitForLoadState('networkidle')
    await page.screenshot({ path: `/tmp/shots/${path.replace(/\W+/g, '_') || 'home'}.png` })
    await expect(page.locator('#root')).not.toBeEmpty()
    expect(errors).toEqual([])
    expect(bad).toEqual([])
  })
}
