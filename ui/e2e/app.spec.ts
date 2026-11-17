import { test, expect } from '@playwright/test'

test.describe('App smoke tests', () => {
  test('page loads and shows the app shell', async ({ page }) => {
    await page.goto('/')
    await expect(page.locator('#root')).toBeVisible()
  })

  test('sidebar shows resource tabs', async ({ page }) => {
    await page.goto('/')
    const nav = page.getByRole('navigation', { name: 'Main' })
    for (const name of ['Home', 'Agents', 'Runs', 'Approvals', 'Build', 'Evaluate', 'Operate', 'Settings']) {
      await expect(nav.getByRole('link', { name: new RegExp(`^${name}`) })).toBeVisible()
    }
  })

  test('clicking Agents shows the agent lists at /agents', async ({ page }) => {
    await page.goto('/')
    await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Agents' }).click()
    await expect(page).toHaveURL(/\/agents$/)
    await expect(page.getByPlaceholder(/filter by name/i).first()).toBeVisible()
  })
})

test.describe('Deployments', () => {
  test('deployments from the cluster appear in the sidebar', async ({ page, request }) => {
    // Discover what's actually deployed
    const res = await request.get('/api/deployments?namespace=default')
    expect(res.ok()).toBeTruthy()
    const deployments: { name: string }[] = await res.json()
    test.skip(deployments.length === 0, 'No deployments in cluster to test against')

    await page.goto('/agents')

    for (const dep of deployments) {
      await expect(page.getByText(dep.name)).toBeVisible({ timeout: 10_000 })
    }
  })

  test('clicking a deployment opens its detail view', async ({ page, request }) => {
    const res = await request.get('/api/deployments?namespace=default')
    const deployments: { name: string; agentRef: string }[] = await res.json()
    test.skip(deployments.length === 0, 'No deployments in cluster to test against')

    const dep = deployments[0]
    await page.goto('/agents')
    // Wait for the list item to appear, then click it
    const sidebarItem = page.locator('aside').getByText(dep.name).first()
    await expect(sidebarItem).toBeVisible({ timeout: 15_000 })
    await sidebarItem.click()
    await expect(page).toHaveURL(/\/agents\/published\//)
    // DeploymentView always renders a chat textarea
    await expect(page.getByPlaceholder('Send a message…')).toBeVisible({ timeout: 15_000 })
  })
})

test.describe('Runs', () => {
  test('runs tab shows runs from the cluster or empty state', async ({ page, request }) => {
    const res = await request.get('/api/runs?namespace=default')
    const runs: { name: string }[] = await res.json()

    await page.goto('/runs')

    if (runs.length === 0) {
      // First-run empty state (or "no runs found" when history is on but empty)
      await expect(page.getByText(/no runs yet|no runs found/i).first()).toBeVisible({ timeout: 15_000 })
    } else {
      // The table fetches runs async — wait for the first one to appear
      await expect(page.getByRole('button', { name: `View run ${runs[0].name}` })).toBeVisible({ timeout: 15_000 })
    }
  })

  test('clicking a run opens its detail view', async ({ page, request }) => {
    const res = await request.get('/api/runs?namespace=default')
    const runs: { name: string }[] = await res.json()
    test.skip(runs.length === 0, 'No runs in cluster to test against')

    const run = runs[0]
    await page.goto('/runs')
    // Wait for the table to populate, then open the run
    await page.getByRole('button', { name: `View run ${run.name}` }).click({ timeout: 15_000 })
    await expect(page).toHaveURL(new RegExp(`/runs/(live|history)/.*/${run.name}$`))
    // RunView detail header renders the spend chip inside <main>
    const main = page.locator('main')
    await expect(main.getByText(run.name)).toBeVisible({ timeout: 15_000 })
  })
})

test.describe('Filter', () => {
  test('filter input narrows the resource list', async ({ page, request }) => {
    const res = await request.get('/api/deployments?namespace=default')
    const deployments: { name: string }[] = await res.json()
    test.skip(deployments.length === 0, 'No deployments in cluster to test against')

    await page.goto('/agents')
    await expect(page.getByText(deployments[0].name).first()).toBeVisible({ timeout: 10_000 })

    // The first filter box belongs to the published agents list.
    const filter = page.getByPlaceholder(/filter by name/i).first()
    await filter.fill('zzz-does-not-exist')
    await expect(page.getByText(/no published agents match/i)).toBeVisible()
  })
})

test.describe('Navigation', () => {
  test('moving to another section clears the open agent', async ({ page, request }) => {
    const res = await request.get('/api/deployments?namespace=default')
    const deployments: { name: string }[] = await res.json()
    test.skip(deployments.length === 0, 'No deployments in cluster to test against')

    await page.goto('/agents')
    await page.locator('aside').getByText(deployments[0].name).first().click()

    const nav = page.getByRole('navigation', { name: 'Main' })
    await nav.getByRole('link', { name: 'Operate' }).click()
    await page.getByRole('navigation', { name: 'Operate' }).getByRole('link', { name: 'Workflows' }).click()
    await expect(page).toHaveURL(/\/operate\/workflows$/)
    await expect(page.getByText(/choose a workflow/i)).toBeVisible()
  })

  test('costs live under Operate and survive a reload', async ({ page }) => {
    await page.goto('/operate/costs')
    const costs = page.getByRole('navigation', { name: 'Operate' }).getByRole('link', { name: 'Costs' })
    await expect(costs).toHaveAttribute('aria-current', 'page')
    await page.reload()
    await expect(page).toHaveURL(/\/operate\/costs$/)
    await expect(costs).toHaveAttribute('aria-current', 'page')
  })

  test('an old ?tab= link is redirected to the new route', async ({ page }) => {
    await page.goto('/?tab=approvals')
    await expect(page).toHaveURL(/\/approvals$/)
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

test.describe('Home Dashboard', () => {
  test('home shows the system dashboard with cards', async ({ page }) => {
    await page.goto('/')
    await expect(page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Home' })).toBeVisible()
    await expect(page.getByText('AI Agent Orchestration Platform')).toBeVisible()
    await expect(page.getByText('Runs', { exact: true }).first()).toBeVisible()
    await expect(page.getByText('Published agents', { exact: true }).first()).toBeVisible()
  })

  test('there is no Marketplace page', async ({ page }) => {
    await page.goto('/marketplace')
    await expect(page).toHaveURL(/\/$/)
    await expect(page.getByText('Marketplace')).toHaveCount(0)
  })
})
