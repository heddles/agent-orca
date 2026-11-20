/*
 * agent-orca chat widget v1.
 *
 *   <script src="https://HOST/widget/v1.js" data-key="wgt_..." async></script>
 *
 * Optional attributes: data-title (the header text), data-open="true" (start
 * open). The page holds no credential of the platform: the key is public, and
 * the server only answers it for the origins its owner allowed. Every piece of
 * text from the agent or the visitor is put in the page with textContent, never
 * as HTML. The chat lives in a shadow root, so the page's styles do not reach
 * it and its styles do not leak out.
 */
(function () {
  'use strict'
  var script = document.currentScript
  if (!script || !script.src) return
  var key = script.getAttribute('data-key') || ''
  if (!/^wgt_[0-9a-f]{32}$/.test(key)) { console.error('agent-orca widget: data-key is missing or malformed'); return }
  if (document.getElementById('agentorca-widget-' + key)) return
  var base = new URL(script.src).origin + '/widget/v1/' + key
  var title = script.getAttribute('data-title') || 'Chat'
  var STORE = 'agentorca-widget-' + key
  var POLL_MS = 1200
  var POLL_LIMIT = 150

  var token = ''
  var busy = false
  try { token = sessionStorage.getItem(STORE) || '' } catch (e) { token = '' }

  var host = document.createElement('div')
  host.id = 'agentorca-widget-' + key
  host.style.cssText = 'position:fixed;right:20px;bottom:20px;z-index:2147483000;'
  var root = host.attachShadow({ mode: 'open' })

  var style = document.createElement('style')
  style.textContent = [
    ':host{all:initial}',
    '*{box-sizing:border-box;font-family:system-ui,-apple-system,Segoe UI,Roboto,sans-serif}',
    '.btn{width:52px;height:52px;border-radius:26px;border:0;background:#2b59c3;color:#fff;font-size:22px;cursor:pointer;box-shadow:0 2px 10px rgba(0,0,0,.3)}',
    '.panel{display:none;flex-direction:column;width:340px;max-width:calc(100vw - 24px);height:480px;max-height:calc(100vh - 100px);background:#fff;color:#1a1a1a;border:1px solid #d0d4da;border-radius:12px;box-shadow:0 6px 24px rgba(0,0,0,.25);overflow:hidden}',
    '.panel.open{display:flex}',
    '.head{display:flex;align-items:center;justify-content:space-between;padding:10px 12px;background:#2b59c3;color:#fff;font-size:14px;font-weight:600}',
    '.close{background:transparent;border:0;color:#fff;font-size:18px;cursor:pointer}',
    '.log{flex:1;overflow-y:auto;padding:10px;display:flex;flex-direction:column;gap:8px;font-size:13px}',
    '.msg{max-width:85%;padding:7px 10px;border-radius:10px;white-space:pre-wrap;overflow-wrap:anywhere}',
    '.me{align-self:flex-end;background:#e8eefc}',
    '.bot{align-self:flex-start;background:#f1f2f4}',
    '.note{align-self:center;color:#6b7280;font-size:12px}',
    '.err{align-self:center;color:#b42318;font-size:12px}',
    'form{display:flex;gap:6px;padding:8px;border-top:1px solid #e3e5e8}',
    'input{flex:1;padding:8px;border:1px solid #c4c8ce;border-radius:8px;font-size:13px}',
    'form button{padding:8px 12px;border:0;border-radius:8px;background:#2b59c3;color:#fff;font-size:13px;cursor:pointer}',
    'form button[disabled]{opacity:.5;cursor:default}'
  ].join('')
  root.appendChild(style)

  var btn = el('button', 'btn', '💬')
  btn.type = 'button'
  btn.setAttribute('aria-label', 'Open chat: ' + title)
  var panel = el('div', 'panel')
  panel.setAttribute('role', 'dialog')
  panel.setAttribute('aria-label', title)
  var head = el('div', 'head')
  head.appendChild(el('span', '', title))
  var close = el('button', 'close', '×')
  close.type = 'button'
  close.setAttribute('aria-label', 'Close chat')
  head.appendChild(close)
  var log = el('div', 'log')
  log.setAttribute('role', 'log')
  log.setAttribute('aria-live', 'polite')
  var form = el('form')
  var input = el('input')
  input.type = 'text'
  input.maxLength = 4000
  input.placeholder = 'Type a message'
  input.setAttribute('aria-label', 'Message')
  var send = el('button', '', 'Send')
  send.type = 'submit'
  form.appendChild(input)
  form.appendChild(send)
  panel.appendChild(head)
  panel.appendChild(log)
  panel.appendChild(form)
  root.appendChild(panel)
  root.appendChild(btn)
  document.body.appendChild(host)

  function el(tag, cls, text) {
    var e = document.createElement(tag)
    if (cls) e.className = cls
    if (text) e.textContent = text
    return e
  }
  function add(cls, text) {
    var m = el('div', 'msg ' + cls, text)
    log.appendChild(m)
    log.scrollTop = log.scrollHeight
    return m
  }
  function setOpen(open) {
    panel.classList.toggle('open', open)
    btn.style.display = open ? 'none' : ''
    if (open) input.focus(); else btn.focus()
  }
  function setBusy(b) { busy = b; send.disabled = b; input.disabled = b }

  function call(method, path, body) {
    var headers = { 'Content-Type': 'application/json' }
    if (token) headers['X-Widget-Session'] = token
    return fetch(base + path, { method: method, headers: headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: 'omit' })
  }
  function fail(res) {
    if (res.status === 402) return 'This chat has reached its daily limit. Please try again tomorrow.'
    if (res.status === 429) return 'Too many messages right now. Please try again in a moment.'
    if (res.status === 403 || res.status === 404) return 'This chat is not available on this page.'
    return 'Something went wrong. Please try again.'
  }
  function session() {
    if (token) return Promise.resolve(true)
    return call('POST', '/sessions').then(function (res) {
      if (!res.ok) { add('err', fail(res)); return false }
      return res.json().then(function (b) {
        token = b.token
        try { sessionStorage.setItem(STORE, token) } catch (e) { /* private mode: the chat works until the page closes */ }
        return true
      })
    })
  }
  function forget() { token = ''; try { sessionStorage.removeItem(STORE) } catch (e) { /* ignore */ } }

  function poll(runName, bubble, n) {
    if (n > POLL_LIMIT) { bubble.textContent = 'The agent is taking too long. Please try again.'; setBusy(false); return }
    setTimeout(function () {
      call('GET', '/runs/' + encodeURIComponent(runName)).then(function (res) {
        if (res.status === 401) { forget(); bubble.textContent = 'The chat session ended. Send your message again.'; setBusy(false); return }
        if (!res.ok) { bubble.textContent = fail(res); setBusy(false); return }
        return res.json().then(function (b) {
          if (b.phase === 'running') { poll(runName, bubble, n + 1); return }
          bubble.textContent = b.phase === 'done' ? (b.output || '(no answer)') : (b.error || 'The agent could not answer.')
          log.scrollTop = log.scrollHeight
          setBusy(false)
          input.focus()
        })
      }).catch(function () { bubble.textContent = 'The connection was lost. Please try again.'; setBusy(false) })
    }, POLL_MS)
  }

  function submit(text) {
    setBusy(true)
    add('me', text)
    var bubble = add('bot', '…')
    session().then(function (ok) {
      if (!ok) { bubble.remove(); setBusy(false); return }
      return call('POST', '/messages', { input: text }).then(function (res) {
        if (res.status === 401) { forget(); bubble.textContent = 'The chat session ended. Send your message again.'; setBusy(false); return }
        if (!res.ok) { bubble.textContent = fail(res); setBusy(false); return }
        return res.json().then(function (b) { poll(b.runName, bubble, 0) })
      })
    }).catch(function () { bubble.textContent = 'The connection was lost. Please try again.'; setBusy(false) })
  }

  btn.addEventListener('click', function () { setOpen(true) })
  close.addEventListener('click', function () { setOpen(false) })
  panel.addEventListener('keydown', function (e) { if (e.key === 'Escape') setOpen(false) })
  form.addEventListener('submit', function (e) {
    e.preventDefault()
    var text = input.value.trim()
    if (!text || busy) return
    input.value = ''
    submit(text)
  })
  add('note', 'Ask a question to get started.')
  if (script.getAttribute('data-open') === 'true') setOpen(true)
})()
