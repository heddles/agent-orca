/**
 * The embeddable chat widget script (internal/apiserver/widget/v1.js), run in
 * jsdom with a mocked fetch: it mounts in a shadow root, talks only to its key's
 * endpoints, and puts every piece of text in the page as text.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import SRC from '../../../internal/apiserver/widget/v1.js?raw'

const KEY = 'wgt_0123456789abcdef0123456789abcdef'
const HOST = 'https://orca.example.com'

type Call = { method: string; url: string; headers: Record<string, string>; body: unknown }
let calls: Call[]
let replies: ((c: Call) => Response | undefined)[]

function load(attrs: Record<string, string> = { 'data-key': KEY }) {
  const el = document.createElement('script')
  el.src = `${HOST}/widget/v1.js`
  for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v)
  Object.defineProperty(document, 'currentScript', { value: el, configurable: true })
  ;(0, eval)(SRC)
}
const shadow = () => document.getElementById(`agentorca-widget-${KEY}`)?.shadowRoot ?? null
const q = (sel: string) => shadow()!.querySelector(sel) as HTMLElement

beforeEach(() => {
  vi.useFakeTimers()
  calls = []
  replies = []
  sessionStorage.clear()
  document.body.innerHTML = ''
  vi.stubGlobal('fetch', vi.fn((url: string, init?: RequestInit) => {
    const c: Call = { method: init?.method ?? 'GET', url, headers: (init?.headers ?? {}) as Record<string, string>, body: init?.body ? JSON.parse(String(init.body)) : undefined }
    calls.push(c)
    for (const r of replies) { const res = r(c); if (res) return Promise.resolve(res) }
    return Promise.resolve(new Response('{}', { status: 404 }))
  }))
})
afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals() })

const json = (b: unknown, status = 200) => new Response(JSON.stringify(b), { status })
const chat = (answer: Record<string, string>) => {
  replies.push((c) => (c.url.endsWith('/sessions') ? json({ token: 'tok-1' }) : undefined))
  replies.push((c) => (c.url.endsWith('/messages') ? json({ runName: 'run-1' }, 202) : undefined))
  replies.push((c) => (c.url.includes('/runs/run-1') ? json(answer) : undefined))
}
async function send(text: string) {
  const input = q('input') as HTMLInputElement
  input.value = text
  q('form').dispatchEvent(new Event('submit', { cancelable: true }))
  await vi.advanceTimersByTimeAsync(3000)
}

describe('chat widget script', () => {
  it('does nothing without a well-formed key, and mounts once', () => {
    const err = vi.spyOn(console, 'error').mockImplementation(() => {})
    load({})
    load({ 'data-key': 'wgt_short' })
    expect(document.body.children.length).toBe(0)
    expect(err).toHaveBeenCalled()
    load()
    load()
    expect(document.querySelectorAll(`#agentorca-widget-${KEY}`).length).toBe(1)
  })

  it('mounts in a shadow root, closed until the button is pressed', () => {
    load({ 'data-key': KEY, 'data-title': 'Ask us' })
    expect(shadow()).not.toBeNull()
    expect(q('.panel').classList.contains('open')).toBe(false)
    q('.btn').click()
    expect(q('.panel').classList.contains('open')).toBe(true)
    expect(q('.panel').getAttribute('aria-label')).toBe('Ask us')
    q('.close').click()
    expect(q('.panel').classList.contains('open')).toBe(false)
  })

  it('opens a session, sends the message and shows the answer, with only its key’s endpoints', async () => {
    chat({ phase: 'done', output: 'It shipped.' })
    load()
    q('.btn').click()
    await send('Where is my order?')
    expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
      `POST ${HOST}/widget/v1/${KEY}/sessions`,
      `POST ${HOST}/widget/v1/${KEY}/messages`,
      `GET ${HOST}/widget/v1/${KEY}/runs/run-1`,
    ])
    expect(calls[1].body).toEqual({ input: 'Where is my order?' })
    expect(calls[1].headers['X-Widget-Session']).toBe('tok-1')
    expect(calls[0].headers['X-Widget-Session']).toBeUndefined()
    const texts = Array.from(shadow()!.querySelectorAll('.msg')).map((m) => m.textContent)
    expect(texts).toContain('Where is my order?')
    expect(texts).toContain('It shipped.')
    expect(sessionStorage.getItem(`agentorca-widget-${KEY}`)).toBe('tok-1')
  })

  it('reuses the session for the next message', async () => {
    chat({ phase: 'done', output: 'ok' })
    load()
    await send('one')
    await send('two')
    expect(calls.filter((c) => c.url.endsWith('/sessions')).length).toBe(1)
    expect(calls.filter((c) => c.url.endsWith('/messages')).length).toBe(2)
  })

  it('shows the agent’s text as text, never as markup', async () => {
    chat({ phase: 'done', output: '<img src=x onerror="window.pwned=1"><b>hi</b>' })
    load()
    await send('<script>window.pwned=1</script>')
    expect(shadow()!.querySelector('img, b, script')).toBeNull()
    expect(Array.from(shadow()!.querySelectorAll('.msg')).map((m) => m.textContent)).toContain('<img src=x onerror="window.pwned=1"><b>hi</b>')
    expect((window as unknown as { pwned?: number }).pwned).toBeUndefined()
  })

  it('keeps polling while the agent works', async () => {
    let n = 0
    replies.push((c) => (c.url.endsWith('/sessions') ? json({ token: 't' }) : undefined))
    replies.push((c) => (c.url.endsWith('/messages') ? json({ runName: 'run-1' }, 202) : undefined))
    replies.push((c) => (c.url.includes('/runs/') ? json(++n < 3 ? { phase: 'running' } : { phase: 'done', output: 'finally' }) : undefined))
    load()
    const input = q('input') as HTMLInputElement
    input.value = 'slow one'
    q('form').dispatchEvent(new Event('submit', { cancelable: true }))
    await vi.advanceTimersByTimeAsync(10000)
    expect(n).toBe(3)
    expect(Array.from(shadow()!.querySelectorAll('.msg')).map((m) => m.textContent)).toContain('finally')
  })

  it('says so when the budget is spent, the page is refused or the rate is hit', async () => {
    for (const [status, text] of [[402, /daily limit/], [403, /not available on this page/], [429, /Too many messages/]] as const) {
      document.body.innerHTML = ''
      sessionStorage.clear()
      replies = [(c) => (c.url.endsWith('/sessions') ? json({ token: 't' }) : undefined), (c) => (c.url.endsWith('/messages') ? json({ error: 'x' }, status) : undefined)]
      load()
      await send('hello')
      expect(shadow()!.textContent).toMatch(text)
      expect(q('input').hasAttribute('disabled')).toBe(false)
    }
  })

  it('starts over with a new session when the old one has expired', async () => {
    sessionStorage.setItem(`agentorca-widget-${KEY}`, 'old')
    replies.push((c) => (c.url.endsWith('/messages') && c.headers['X-Widget-Session'] === 'old' ? json({ error: 'expired' }, 401) : undefined))
    load()
    await send('hi')
    expect(shadow()!.textContent).toMatch(/session ended/)
    expect(sessionStorage.getItem(`agentorca-widget-${KEY}`)).toBeNull()
  })

  it('never sends cookies or other credentials', async () => {
    chat({ phase: 'done', output: 'ok' })
    load()
    await send('hi')
    const init = (fetch as unknown as { mock: { calls: [string, RequestInit][] } }).mock.calls.map((c) => c[1])
    expect(init.every((i) => i.credentials === 'omit')).toBe(true)
  })
})
