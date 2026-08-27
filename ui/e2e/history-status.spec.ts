import { test, expect, type Response } from '@playwright/test'

/**
 * Resolve the cluster's truth via the (public) system-status endpoint so each
 * test can branch on whether run history is configured, rather than assuming a
 * fixed cluster state. The existing app.spec.ts suite makes the same
 * "empty -> skip" assumption; here we make it explicit and data-driven.
 */
interface StatusSnapshot {
  stateConfigured: boolean
  runHistoryConfigured: boolean
  subsystems: Array<{ name: string; status: string; message?: string }>
  modelProviders: Array<{ name: string; namespace: string; ready: boolean }>
  metrics?: Record<string, unknown>
}

async function fetchStatus(request: Parameters<typeof test>[0]['request']): Promise<StatusSnapshot | null> {
  let res: Response
  try {
    res = await request.get('/api/system/status')
  } catch {
    return null
  }
  if (!res.ok()) return null
  return (await res.json()) as StatusSnapshot
}

test.describe('System Status API contract', () => {
  test('system status exposes runHistoryConfigured and a postgres-archive subsystem', async ({ request }) => {
    const status = await fetchStatus(request)
    expect(status, 'system status should be reachable').not.toBeNull()
    expect(status).toHaveProperty('runHistoryConfigured')
    expect(status).toHaveProperty('stateConfigured')
    const subsystems = status!.subsystems.map((s) => s.name)
    expect(subsystems).toContain('postgres-archive')
  })

  test('model providers are namespace-dereferenced (no duplicate-name collision)', async ({ request }) => {
    const status = await fetchStatus(request)
    expect(status).not.toBeNull()
    if (status!.modelProviders.length === 0) {
      test.skip('No model providers in cluster to test namespace disambiguation')
    }
    for (const p of status!.modelProviders) {
      // Every provider entry must carry a namespace so same-named providers in
      // different namespaces are distinguishable (the reported bug).
      expect(typeof p.namespace).toBe('string')
    }
    // No two entries should share both name AND namespace.
    const keys = new Set<string>()
    for (const p of status!.modelProviders) {
      const key = `${p.namespace}/${p.name}`
      expect(keys.has(key), `duplicate provider key ${key}`).toBe(false)
      keys.add(key)
    }
  })
})

test.describe('History tab', () => {
  test('shows either a run list, an empty state, or an actionable not-configured banner', async ({ page, request }) => {
    const status = await fetchStatus(request)
    await page.goto('/')
    await page.getByRole('button', { name: /run history/i }).click()

    if (status && !status.runHistoryConfigured) {
      // PostgreSQL archival not wired → the UI must surface an actionable banner,
      // never a silent blank (the original bug).
      await expect(page.getByText(/Run history is not available/i)).toBeVisible({ timeout: 15_000 })
      await expect(page.getByText(/PostgreSQL archival store is not configured/i)).toBeVisible()
    } else if (status && status.runHistoryConfigured) {
      // Archive is wired — table header or a genuine empty/filtered state.
      await expect(page.getByText(/run/i, { exact: false })).toBeVisible({ timeout: 15_000 })
      // The table header row should render.
      await expect(page.getByRole('columnheader')).toBeVisible()
    } else {
      test.skip('System status unreachable — cannot determine history configuration')
    }
  })

  test('tolerates a runs:null response without crashing the root', async ({ page }) => {
    // Regression: an empty archive serializes `runs` as null (nil slice). The UI
    // previously did `[...page.runs]` on null → "can't access property
    // Symbol.iterator, t.runs is null", which unmounted the whole React root and
    // left `tab:"history"` stuck in sessionStorage (forcing a cache clear).
    const crashes: string[] = []
    page.on('pageerror', (e) => crashes.push(String(e)))

    await page.route('**/api/runs/history*', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ runs: null, total: 0, limit: 50, offset: 0 }),
      })
    })
    await page.route('**/api/system/status*', async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          stateConfigured: false, runHistoryConfigured: true, version: 'test',
          subsystems: [{ name: 'kubernetes-api', status: 'up', latencyMs: 1 }],
          modelProviders: [], metrics: { requestCount24h: 0, p95LatencyMs: 0, tokenThroughput: 0, egressPublished: 0, egressFailed: 0, mcpToolCalls: 0 },
        }),
      })
    })
    await page.route('**/api/system/alerts*', async (route) => {
      await route.fulfill({ status: 200, contentType: 'application/json', body: '[]' })
    })

    await page.goto('/')
    await page.getByRole('button', { name: /run history/i }).click()
    await page.waitForTimeout(1500)

    expect(crashes.filter((e) => /Symbol.iterator|runs is null/i.test(e)),
      'status page must not crash the root on a null runs array').toEqual([])
    await expect(page.getByText(/No historical runs found/i)).toBeVisible()
  })
})

test.describe('Status page', () => {
  test('renders subsystem health cards and metrics', async ({ page, request }) => {
    const status = await fetchStatus(request)
    test.skip(!status, 'System status unreachable')

    await page.goto('/')
    await page.getByRole('button', { name: /system status/i }).click()

    await expect(page.getByRole('heading', { name: /System Status/i })).toBeVisible({ timeout: 15_000 })
    await expect(page.getByText(/Subsystem Health/i)).toBeVisible()

    // Each reported subsystem should surface as a card (progressive disclosure).
    for (const sub of status!.subsystems) {
      await expect(page.getByText(sub.name)).toBeVisible({ timeout: 10_000 })
    }

    // Metrics section should render when the backend reports metrics.
    if (status!.metrics) {
      await expect(page.getByText(/Metrics/i)).toBeVisible()
    }
  })

  test('model provider table shows a Namespace column and disambiguates same-named providers', async ({ page, request }) => {
    const status = await fetchStatus(request)
    test.skip(!status || status.modelProviders.length === 0, 'No model providers in cluster')

    await page.goto('/')
    await page.getByRole('button', { name: /system status/i }).click()
    await expect(page.getByText(/Model Providers/i)).toBeVisible({ timeout: 15_000 })

    // Stage C: the table gained a Namespace column header.
    await expect(page.getByRole('columnheader', { name: /Namespace/i })).toBeVisible()

    // Same-named providers in different namespaces must both be visible (the bug
    // previously collapsed them into one visually-identical row).
    const byName = new Map<string, number>()
    for (const p of status!.modelProviders) {
      byName.set(p.name, (byName.get(p.name) ?? 0) + 1)
    }
    for (const [name, count] of byName) {
      if (count > 1) {
        // The provider name appears in the document at least `count` times
        // (once per namespace), proving they are no longer collapsed.
        expect((await page.getByText(name).count()), `provider ${name} should appear ${count} times`).toBeGreaterThanOrEqual(count)
      }
    }
  })

  test('metrics render as graphs/heatmap, not cards; MCP card and Alerts are removed', async ({ page, request }) => {
    const status = await fetchStatus(request)
    test.skip(!status, 'System status unreachable')

    await page.goto('/')
    await page.getByRole('button', { name: /system status/i }).click()
    await expect(page.getByText(/Metrics/i)).toBeVisible({ timeout: 15_000 })

    // The redesign replaced card views with graphs + a latency heatmap, a
    // time-range selector, and egress tooltips.
    await expect(page.getByText('Egress Published')).toBeVisible()
    await expect(page.getByText('Egress Failed')).toBeVisible()
    await expect(page.getByText('Request latency')).toBeVisible() // heatmap heading
    await expect(page.getByRole('radiogroup', { name: /time range/i })).toBeVisible()
    await expect(page.getByRole('radio', { name: '24h' })).toBeVisible()

    // MCP/tool aggregate and the Alerts section are explicitly removed.
    expect(await page.locator('text=/MCP \\/ Tool Results/i').count()).toBe(0)
    expect(await page.locator('text=/^Alerts$/i').count()).toBe(0)
  })

  test('latency heatmap + time-range selector render from metric samples', async ({ page }) => {
    // Drives the new graph/heatmap viz with a mocked status payload carrying
    // several sampled records, and verifies the time-range selector re-fetches
    // with ?range=.
    const crashes: string[] = []
    page.on('pageerror', (e) => crashes.push(String(e)))

    let lastRange = ''
    await page.route('**/api/system/status*', async (route) => {
      lastRange = route.request().url()
      const samples = []
      for (let i = 0; i < 12; i++) {
        const t = Math.floor(Date.now() / 1000) - (12 - i) * 60
        samples.push({
          time: t,
          requestCount: i * 10,
          p50LatencyMs: 40 + i,
          p95LatencyMs: 90 + i * 3,
          p99LatencyMs: 180 + i * 5,
          egressPublished: i,
          egressFailed: i % 3,
          tokenThroughput: 0,
        })
      }
      const body = JSON.stringify({
        stateConfigured: true, runHistoryConfigured: true, version: 'x',
        subsystems: [{ name: 'kubernetes-api', status: 'up', latencyMs: 5 }],
        modelProviders: [{ name: 'a', namespace: 'default', ready: true, message: 'ok' }],
        metrics: {
          requestCount24h: 120, p50LatencyMs: 50, p95LatencyMs: 130, p99LatencyMs: 230,
          tokenThroughput: 0, egressPublished: 12, egressFailed: 1, samples,
        },
        alerts: [],
      })
      route.fulfill({ status: 200, contentType: 'application/json', body })
    })
    await page.route('**/api/system/alerts*', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: '[]' }))

    await page.goto('/')
    await page.getByRole('button', { name: /system status/i }).click()
    await expect(page.getByRole('radiogroup', { name: /time range/i })).toBeVisible({ timeout: 15000 })
    // Heatmap renders a row per percentile.
    await expect(page.locator('text=/^p50$/')).toBeVisible()
    await expect(page.locator('text=/^p95$/')).toBeVisible()
    await expect(page.locator('text=/^p99$/')).toBeVisible()
    // Egress panels render with their tooltips (info icon via title attr).
    await expect(page.getByText('Egress Published')).toBeVisible()
    await expect(page.locator('[title*="published to the configured egress sink"]')).toBeVisible()
    expect(crashes, 'status page must not crash rendering metrics/heatmap').toEqual([])

    // Selecting a different range re-fetches with ?range=.
    const beforeUrl = lastRange
    await page.getByRole('radio', { name: '1h' }).click()
    await page.waitForTimeout(800)
    expect(lastRange).not.toBe(beforeUrl)
    expect(lastRange).toMatch(/range=1h/)
  })

  test('model provider table is sortable and stable across refreshes', async ({ page }) => {
    // Regression for: (1) no sortable headers, (2) table "regenerating" — rows
    // reordering on every 5s poll because the backend list order is
    // non-deterministic. A deterministic default sort (namespace asc) keeps
    // rows in place and only content updates; clicking a header re-sorts.
    const crashes: string[] = []
    page.on('pageerror', (e) => crashes.push(String(e)))

    let wireOrder = 0
    const providersA = [
      { name: 'openai', namespace: 'tenant-a', ready: false, latencyMs: 400, message: 'slow' },
      { name: 'anthropic', namespace: 'default', ready: true, latencyMs: 120, message: 'ok' },
      { name: 'openai', namespace: 'default', ready: true, latencyMs: 80, message: 'ok' },
    ]
    const providersB = [...providersA].reverse() // different wire order, same logical data
    const bodies = [
      JSON.stringify({
        stateConfigured: true, runHistoryConfigured: true, version: 'x',
        subsystems: [{ name: 'kubernetes-api', status: 'up', latencyMs: 5 }],
        modelProviders: providersA,
        metrics: { requestCount24h: 0, p95LatencyMs: 0, tokenThroughput: 0, egressPublished: 0, egressFailed: 0, mcpToolCalls: 0 },
        alerts: [],
      }),
      JSON.stringify({
        stateConfigured: true, runHistoryConfigured: true, version: 'x',
        subsystems: [{ name: 'kubernetes-api', status: 'up', latencyMs: 5 }],
        modelProviders: providersB,
        metrics: { requestCount24h: 0, p95LatencyMs: 0, tokenThroughput: 0, egressPublished: 0, egressFailed: 0, mcpToolCalls: 0 },
        alerts: [],
      }),
    ]
    await page.route('**/api/system/status*', (route) => {
      const body = bodies[Math.min(wireOrder++, bodies.length - 1)]
      route.fulfill({ status: 200, contentType: 'application/json', body })
    })
    await page.route('**/api/system/alerts*', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: '[]' }))

    await page.goto('/')
    await page.getByRole('button', { name: /system status/i }).click()
    await expect(page.getByRole('columnheader', { name: /Namespace/i })).toBeVisible({ timeout: 15000 })

    // No render crash.
    expect(crashes, 'status page must not crash on provider render').toEqual([])

    const nsHeader = page.locator('th', { has: page.getByRole('button', { name: /Sort by Namespace/i }) })

    // Default sort is namespace ascending → the Namespace header button is aria-sort ascending.
    const nsBtn = nsHeader.getByRole('button', { name: /Sort by Namespace/i })
    await expect(nsBtn).toHaveAttribute('aria-sort', 'ascending')

    // Click Namespace to flip to descending.
    await nsBtn.click()
    await expect(nsBtn).toHaveAttribute('aria-sort', 'descending')

    // Snapshot the namespace cells in DOM order (should be namespace asc by default).
    const beforeRefresh = await page.locator('tr td code').allInnerTexts()

    // Force a re-fetch (simulating the 5s poll) with a DIFFERENT wire order.
    await page.getByRole('button', { name: /Refresh/i }).click()
    await page.waitForTimeout(1200)

    // Displayed namespace order must be unchanged (deterministic default sort),
    // proving the table doesn't "regenerate"/reorder on a fresh fetch.
    const afterRefresh = await page.locator('tr td code').allInnerTexts()
    expect(afterRefresh).toEqual(beforeRefresh)
  })

  test('model provider table allows re-sorting by latency', async ({ page }) => {
    const crashes: string[] = []
    page.on('pageerror', (e) => crashes.push(String(e)))
    const body = JSON.stringify({
      stateConfigured: true, runHistoryConfigured: true, version: 'x',
      subsystems: [{ name: 'kubernetes-api', status: 'up', latencyMs: 5 }],
      modelProviders: [
        { name: 'a', namespace: 'n1', ready: true, latencyMs: 400 },
        { name: 'a', namespace: 'n2', ready: true, latencyMs: 80 },
      ],
      metrics: { requestCount24h: 0, p95LatencyMs: 0, tokenThroughput: 0, egressPublished: 0, egressFailed: 0, mcpToolCalls: 0 },
      alerts: [],
    })
    await page.route('**/api/system/status*', (route) => route.fulfill({ status: 200, contentType: 'application/json', body }))
    await page.route('**/api/system/alerts*', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: '[]' }))

    await page.goto('/')
    await page.getByRole('button', { name: /system status/i }).click()
    await expect(page.getByRole('columnheader', { name: /Namespace/i })).toBeVisible({ timeout: 15000 })
    expect(crashes).toEqual([])

    const latBtn = page.getByRole('button', { name: /Sort by Latency/i })
    const thead = page.locator('thead tr')
    expect(await thead.locator('th').count(), 'header must start with 5 columns').toBe(5)
    await latBtn.click()
    // Reproduces a regression where duplicate React keys on the header buttons
    // caused each sort click to leave a ghost <th> behind, widening the table.
    expect(await thead.locator('th').count(), 'clicking a header must not add columns').toBe(5)
    // Latency now ascending → lowest latency (n2, 80ms) sorts before n1 (400ms).
    await expect(latBtn).toHaveAttribute('aria-sort', 'ascending')
    const nsCodes = await page.locator('td code').allInnerTexts()
    expect(nsCodes).toEqual(expect.arrayContaining(['n2', 'n1']))
    // n2 must precede n1 in the rendered order.
    const idx = nsCodes.indexOf('n2')
    expect(idx).toBeLessThan(nsCodes.indexOf('n1'))
  })

  test('model provider table is collapsible (expand/collapse toggle)', async ({ page }) => {
    const crashes: string[] = []
    page.on('pageerror', (e) => crashes.push(String(e)))
    const body = JSON.stringify({
      stateConfigured: true, runHistoryConfigured: true, version: 'x',
      subsystems: [{ name: 'kubernetes-api', status: 'up', latencyMs: 5 }],
      modelProviders: [
        { name: 'openai', namespace: 'default', ready: true, latencyMs: 80, message: 'ok' },
        { name: 'anthropic', namespace: 'default', ready: false, latencyMs: 200, message: 'degraded' },
      ],
      metrics: { requestCount24h: 1, p50LatencyMs: 10, p95LatencyMs: 20, p99LatencyMs: 30, tokenThroughput: 0, egressPublished: 0, egressFailed: 0 },
      alerts: [],
    })
    await page.route('**/api/system/status*', (route) => route.fulfill({ status: 200, contentType: 'application/json', body }))
    await page.route('**/api/system/alerts*', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: '[]' }))

    await page.goto('/')
    await page.getByRole('button', { name: /system status/i }).click()
    await expect(page.getByText(/Model Providers/i)).toBeVisible({ timeout: 15_000 })

    // Default: expanded. The table (Namespace columnheader) is visible.
    await expect(page.getByRole('columnheader', { name: /Namespace/i })).toBeVisible()
    const toggle = page.getByRole('button', { name: /collapse providers/i })

    // Collapse → table body hides, summary line remains.
    await toggle.click()
    await expect(page.getByRole('button', { name: /expand providers/i })).toBeVisible()
    expect(crashes).toEqual([])

    // Expand → table returns.
    await page.getByRole('button', { name: /expand providers/i }).click()
    await expect(page.getByRole('columnheader', { name: /Namespace/i })).toBeVisible()
  })

  test('model provider table is scrollable (not clipped) when providers exist', async ({ page, request }) => {
    const status = await fetchStatus(request)
    test.skip(!status || status.modelProviders.length === 0, 'No model providers in cluster')

    await page.goto('/')
    await page.getByRole('button', { name: /system status/i }).click()
    await expect(page.getByText(/Model Providers/i)).toBeVisible({ timeout: 15_000 })

    // The providers table (identified by its Namespace column) must live in a
    // container that allows scrolling — overflow:hidden would clip the
    // Namespace/Message columns and make them unreachable.
    const table = page.locator('table').filter({
      has: page.getByRole('columnheader', { name: /Namespace/i }),
    })
    await expect(table).toBeVisible()
    const overflowX = await table.evaluate((t) => {
      const parent = (t as HTMLElement).parentElement
      return parent ? getComputedStyle(parent).overflowX : 'hidden'
    })
    expect(overflowX, 'providers table container must not clip overflow with hidden').not.toBe('hidden')
  })
})

test.describe('History → detail navigation', () => {
  test('selecting a history row opens a detail view with cost/context/input/output', async ({ page, request }) => {
    const res = await request.get('/api/runs/history?limit=1')
    // 503 => archive not configured → nothing to select.
    if (res.status() === 503) {
      test.skip('Run history archive not configured')
    }
    expect(res.ok()).toBeTruthy()
    const body: { runs: Array<{ name: string; namespace: string }> } = await res.json()
    if (body.runs.length === 0) {
      test.skip('No archived runs in cluster')
    }
    const run = body.runs[0]

    await page.goto('/')
    await page.getByRole('button', { name: /run history/i }).click()

    // Wait for the table then click the detail chevron within the target row.
    const row = page.locator('tr', { hasText: run.name })
    await expect(row).toBeVisible({ timeout: 15_000 })
    await row.getByRole('button', { name: /view/i }).first().click()

    // Detail view header renders the run name + cost/context chips.
    await expect(page.getByText(run.name, { exact: true })).toBeVisible({ timeout: 15_000 })
    await expect(page.locator('text=/Cost/i')).toBeVisible()
    await expect(page.locator('text=/Context/i')).toBeVisible()
    // Tabs for progressive disclosure of output/routing.
    await expect(page.getByRole('tab', { name: /Output/i })).toBeVisible()
    await expect(page.getByRole('tab', { name: /Model Router/i })).toBeVisible()
    // Back button returns to the history list (filter bar is always rendered,
    // even before the table data resolves).
    await page.getByRole('button', { name: /Back to run history/i }).click()
    await expect(page.getByLabel('Search runs')).toBeVisible({ timeout: 15_000 })
  })

  test('archived run detail has an Execution Trace tab with a reconstructed timeline', async ({ page }) => {
    // Mock the history list + the archived-run detail (with routing decisions,
    // a child run, and spend/context) and assert the Execution Trace tab builds
    // a chronological event timeline from the archived fields.
    const detail = {
      name: 'run-1', namespace: 'default', agentRef: 'my-agent', podName: 'agent-run-1',
      phase: 'Succeeded', output: 'Here is the summary.', spendUSD: '0.0123', restartCount: 1,
      startTime: '2026-01-01T12:00:00Z', completionTime: '2026-01-01T12:05:00Z',
      contextUsedTokens: 4200, maxContextTokens: 200000, resolvedModel: 'openai/gpt-4o',
      routingDecisions: [{
        model: 'openai/gpt-4o', provider: 'openai', strategy: 'rule-based',
        reason: 'cheapest capable', confidence: '0.9', timestamp: '2026-01-01T12:00:05Z',
      }],
      childRunRefs: ['child-run-1'], tools: ['weather-tool'], mcpServers: ['time-server'],
    }
    await page.route(/api\/runs\/history/, (route) => {
      const u = route.request().url()
      route.fulfill(u.includes('/history/default/run-1')
        ? { status: 200, contentType: 'application/json', body: JSON.stringify(detail) }
        : { status: 200, contentType: 'application/json', body: JSON.stringify({
            runs: [{ name: 'run-1', namespace: 'default', agentRef: 'my-agent', phase: 'Succeeded', spendUSD: '0.0123' }],
            total: 1, limit: 50, offset: 0,
          }) })
    })
    await page.route('**/api/system/status*', (route) => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ stateConfigured: true, runHistoryConfigured: true, version: 'x',
        subsystems: [{ name: 'kubernetes-api', status: 'up', latencyMs: 5 }], modelProviders: [],
        metrics: { requestCount24h: 1, p50LatencyMs: 10, p95LatencyMs: 20, p99LatencyMs: 30, tokenThroughput: 0, egressPublished: 0, egressFailed: 0 }, alerts: [] }),
    }))
    await page.route('**/api/system/alerts*', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: '[]' }))

    await page.goto('/')
    await page.getByRole('button', { name: /run history/i }).click()
    await expect(page.locator('tr', { hasText: 'run-1' })).toBeVisible({ timeout: 15_000 })
    await page.locator('tr', { hasText: 'run-1' }).getByRole('button', { name: /view/i }).first().click()

    // Execution Trace tab exists and reconstructs the timeline from archived data.
    await expect(page.getByRole('tab', { name: /Execution Trace/i })).toBeVisible({ timeout: 15_000 })
    await page.getByRole('tab', { name: /Execution Trace/i }).click()
    // Scope assertions to the trace panel: the same strings (run name, child
    // refs, spend) also appear in the always-visible meta cards above the tabs.
    const trace = page.locator('[aria-label="Execution Trace"]')
    await expect(trace.getByText(/Run started/i)).toBeVisible()
    await expect(trace.getByText(/Model routed/i)).toBeVisible()
    await expect(trace.locator('text=/openai\/gpt-4o via openai \/ rule-based/i')).toBeVisible()
    await expect(trace.getByText(/Child run spawned/i)).toBeVisible()
    await expect(trace.locator('text=child-run-1')).toBeVisible()
    await expect(trace.getByText(/Run completed/i)).toBeVisible()
    await expect(trace.locator('text=/Spend:/i')).toBeVisible()
    await expect(trace.locator('text=/tokens/i')).toBeVisible()
  })
})
