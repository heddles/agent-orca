import { test, expect } from '@playwright/test'

test.describe('App smoke tests', () => {
  test('page loads and shows the app shell', async ({ page }) => {
    await page.goto('/')
    await expect(page.locator('#root')).toBeVisible()
  })

  test('sidebar shows resource tabs', async ({ page }) => {
    await page.goto('/')
    await expect(page.getByRole('button', { name: /runs/i })).toBeVisible()
    await expect(page.getByRole('button', { name: /deployments/i })).toBeVisible()
    await expect(page.getByRole('button', { name: /workflows/i })).toBeVisible()
  })

  test('clicking a tab switches the resource list', async ({ page }) => {
    await page.goto('/')
    await page.getByRole('button', { name: /deployments/i }).click()
    await expect(page.getByPlaceholder(/filter by name/i)).toBeVisible()
  })
})

test.describe('Deployments', () => {
  test('deployments from the cluster appear in the sidebar', async ({ page, request }) => {
    // Discover what's actually deployed
    const res = await request.get('/api/deployments?namespace=default')
    expect(res.ok()).toBeTruthy()
    const deployments: { name: string }[] = await res.json()
    test.skip(deployments.length === 0, 'No deployments in cluster to test against')

    await page.goto('/')
    await page.getByRole('button', { name: /deployments/i }).click()

    for (const dep of deployments) {
      await expect(page.getByText(dep.name)).toBeVisible({ timeout: 10_000 })
    }
  })

  test('clicking a deployment opens its detail view', async ({ page, request }) => {
    const res = await request.get('/api/deployments?namespace=default')
    const deployments: { name: string; agentRef: string }[] = await res.json()
    test.skip(deployments.length === 0, 'No deployments in cluster to test against')

    const dep = deployments[0]
    await page.goto('/')
    await page.getByRole('button', { name: /deployments/i }).click()
    // Wait for the sidebar item to appear, then click it
    const sidebarItem = page.locator('aside').getByText(dep.name).first()
    await expect(sidebarItem).toBeVisible({ timeout: 15_000 })
    await sidebarItem.click()
    // DeploymentView always renders a chat textarea
    await expect(page.getByPlaceholder('Send a message…')).toBeVisible({ timeout: 15_000 })
  })
})

test.describe('Runs', () => {
  test('runs tab shows runs from the cluster or empty state', async ({ page, request }) => {
    const res = await request.get('/api/runs?namespace=default')
    const runs: { name: string }[] = await res.json()

    await page.goto('/')

    if (runs.length === 0) {
      // Empty state or "no runs found" may appear after the list loads
      await expect(page.getByText(/select a run|no runs found/i)).toBeVisible({ timeout: 15_000 })
    } else {
      // The sidebar fetches runs async — wait for the first one to appear
      await expect(page.locator('aside').getByText(runs[0].name)).toBeVisible({ timeout: 15_000 })
    }
  })

  test('clicking a run opens its detail view', async ({ page, request }) => {
    const res = await request.get('/api/runs?namespace=default')
    const runs: { name: string }[] = await res.json()
    test.skip(runs.length === 0, 'No runs in cluster to test against')

    const run = runs[0]
    await page.goto('/')
    // Wait for the sidebar to populate, then click
    const sidebarItem = page.locator('aside').getByText(run.name).first()
    await sidebarItem.click({ timeout: 15_000 })
    // RunView detail header renders spend chip with 💰 inside <main>
    const main = page.locator('main')
    await expect(main.getByText('💰')).toBeVisible({ timeout: 15_000 })
  })
})

test.describe('Filter', () => {
  test('filter input narrows the resource list', async ({ page, request }) => {
    const res = await request.get('/api/deployments?namespace=default')
    const deployments: { name: string }[] = await res.json()
    test.skip(deployments.length === 0, 'No deployments in cluster to test against')

    await page.goto('/')
    await page.getByRole('button', { name: /deployments/i }).click()
    await expect(page.getByText(deployments[0].name)).toBeVisible({ timeout: 10_000 })

    const filter = page.getByPlaceholder(/filter by name/i)
    await filter.fill('zzz-does-not-exist')
    await expect(page.getByText(/no deployments match/i)).toBeVisible()
  })
})

test.describe('Navigation', () => {
  test('switching tabs clears the main panel to empty state', async ({ page, request }) => {
    const res = await request.get('/api/deployments?namespace=default')
    const deployments: { name: string }[] = await res.json()
    test.skip(deployments.length === 0, 'No deployments in cluster to test against')

    await page.goto('/')
    await page.getByRole('button', { name: /deployments/i }).click()
    await page.locator('aside').getByText(deployments[0].name).first().click()

    // Switch to workflows tab — main panel should show the empty state
    await page.getByRole('button', { name: /workflows/i }).click()
    await expect(page.getByText(/select a workflow/i)).toBeVisible()
  })

  test('cost dashboard toggle works', async ({ page }) => {
    await page.goto('/')
    await page.getByTitle('Cost Dashboard').click()
    await expect(page.getByText(/cost/i)).toBeVisible({ timeout: 10_000 })
    await page.getByTitle('Cost Dashboard').click()
    await expect(page.getByText(/select a run/i)).toBeVisible()
  })
})

test.describe('API health', () => {
  test('system status endpoint is reachable', async ({ request }) => {
    const res = await request.get('/api/system/status')
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(body).toHaveProperty('stateConfigured')
  })

  test('runs API returns a list', async ({ request }) => {
    const res = await request.get('/api/runs?namespace=default')
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })

  test('deployments API returns a list', async ({ request }) => {
    const res = await request.get('/api/deployments?namespace=default')
    expect(res.ok()).toBeTruthy()
    const body = await res.json()
    expect(Array.isArray(body)).toBeTruthy()
  })
})
