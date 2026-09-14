import assert from 'node:assert/strict'
import test from 'node:test'
// Transport-only tests run without Svelte compilation. Browser regressions
// exercise the reactive account/system lifecycle with the real compiled module.
globalThis.$state = value => value
const { askArchive, login, logout, uploadDocument, exportTaxonomy, cancelMobilePairing, getDemoMode } = await import('./api.js')
const { systems, resetSystems } = await import('./systems.svelte.js')
delete globalThis.$state

test('uses the API-provided error message', async () => {
  const originalFetch = globalThis.fetch
  globalThis.fetch = async () => new Response(JSON.stringify({
    code: 'oauth_failed',
    error: 'Microsoft did not complete sign-in',
  }), {
    status: 400,
    headers: { 'content-type': 'application/json' },
  })
  try {
    await assert.rejects(
      () => login('admin@example.test', 'wrong-password'),
      (err) => err.status === 400 && err.code === 'oauth_failed' &&
        err.message === 'Microsoft did not complete sign-in',
    )
  } finally {
    globalThis.fetch = originalFetch
  }
})

test('preserves JSON error codes despite missing or mislabeled content types', async () => {
  const originalFetch = globalThis.fetch
  try {
    for (const contentType of [null, 'text/plain', 'Application/JSON']) {
      globalThis.fetch = async () => {
        const response = new Response(JSON.stringify({
          code: 'invalid_provider_response', error: 'upstream service error',
        }), { status: 502 })
        if (contentType) response.headers.set('content-type', contentType)
        else response.headers.delete('content-type')
        return response
      }
      await assert.rejects(
        () => askArchive({ question: 'How much did I spend?' }),
        (err) => err.status === 502 && err.code === 'invalid_provider_response' &&
          err.message === 'upstream service error',
      )
    }
  } finally {
    globalThis.fetch = originalFetch
  }
})

test('does not expose a non-JSON gateway error body', async () => {
  const originalFetch = globalThis.fetch
  globalThis.fetch = async () => new Response('<html>private upstream details</html>', {
    status: 502, headers: { 'content-type': 'text/html' },
  })
  try {
    await assert.rejects(
      () => askArchive({ question: 'How much did I spend?' }),
      (err) => err.status === 502 && err.message === 'HTTP 502' && err.data === null,
    )
  } finally {
    globalThis.fetch = originalFetch
  }
})

test('concurrent demo uploads share one cookie-session upgrade', async () => {
  const originalFetch = globalThis.fetch
  let finishUpgrade
  const upgrade = new Promise(resolve => { finishUpgrade = resolve })
  let upgradeStarted
  const started = new Promise(resolve => { upgradeStarted = resolve })
  let upgrades = 0
  let uploads = 0
  let upgraded = false
  globalThis.fetch = async (path, options) => {
    assert.equal(options.headers?.Authorization, undefined)
    assert.equal(options.headers?.['X-Suchi-Demo-Token'], undefined)
    if (path === '/api/demo/session/upgrade') {
      upgrades++
      upgradeStarted()
      await upgrade
      upgraded = true
      return new Response(null, { status: 200 })
    }
    uploads++
    return new Response(JSON.stringify(upgraded ? { id: uploads } : { code: 'demo_upgrade_required' }), {
      status: upgraded ? 201 : 403, headers: { 'content-type': 'application/json' },
    })
  }
  try {
    const first = uploadDocument(new File(['first'], 'first.txt'))
    const second = uploadDocument(new File(['second'], 'second.txt'))
    await started
    assert.equal(upgrades, 1)
    finishUpgrade()
    const results = await Promise.all([first, second])
    assert.equal(results.length, 2)
    assert.equal(uploads, 4)
    assert.equal(upgrades, 1)
  } finally {
    globalThis.fetch = originalFetch
  }
})

test('taxonomy exports capture the filing system and preserve the requested text format', async () => {
  const originalFetch = globalThis.fetch
  resetSystems({ user_id: 1 })
  systems.code = 'S02'
  globalThis.fetch = async (path, options) => {
    assert.equal(path, '/api/admin/taxonomy/export?format=toml&skip_seeds=true&system=S02')
    assert.equal(options.credentials, 'same-origin')
    return new Response('system = "S02"\n', { headers: { 'content-type': 'text/plain' } })
  }
  try {
    assert.equal(await exportTaxonomy('toml', true), 'system = "S02"\n')
  } finally {
    globalThis.fetch = originalFetch
    resetSystems()
  }
})

test('taxonomy export rejects a late download as soon as logout starts', async () => {
  const originalFetch = globalThis.fetch
  let finishExport
  globalThis.fetch = path => path.startsWith('/api/admin/taxonomy/export')
    ? new Promise(resolve => { finishExport = resolve })
    : Promise.resolve(new Response(null, { status: 204 }))
  try {
    const pending = exportTaxonomy()
    await logout()
    finishExport(new Response('private taxonomy'))
    await assert.rejects(pending, err => err.name === 'AbortError')
  } finally {
    globalThis.fetch = originalFetch
  }
})

test('taxonomy export invalidates an unavailable system and preserves server validation errors', async () => {
  const originalFetch = globalThis.fetch
  resetSystems({ user_id: 1 })
  systems.code = 'S02'
  systems.ready = true
  globalThis.fetch = async () => new Response(JSON.stringify({
    code: 'system_unavailable', error: 'System unavailable',
  }), { status: 404 })
  try {
    await assert.rejects(exportTaxonomy(), err => err.code === 'system_unavailable' && err.message === 'System unavailable')
    assert.equal(systems.ready, false)
    assert.match(systems.error, /Ask an administrator/)
  } finally {
    globalThis.fetch = originalFetch
    resetSystems()
  }
})

test('background uploads and pairing cancellation cannot invalidate a different active system', async () => {
  const originalFetch = globalThis.fetch
  resetSystems({ user_id: 1 })
  systems.code = 'S02'
  systems.ready = true
  globalThis.fetch = async path => {
    assert.equal(new URL(path, 'https://archive.example.test').searchParams.get('system'), 'S01')
    return new Response(JSON.stringify({ code: 'system_unavailable', error: 'System unavailable' }), { status: 404 })
  }
  try {
    await assert.rejects(uploadDocument(new File(['old batch'], 'old.txt'), 'S01'), err => err.code === 'system_unavailable')
    await assert.rejects(cancelMobilePairing('old-code', 'S01'), err => err.code === 'system_unavailable')
    assert.equal(systems.code, 'S02')
    assert.equal(systems.ready, true)
    assert.equal(systems.error, '')
  } finally {
    globalThis.fetch = originalFetch
    resetSystems()
  }
})

test('public demo detection survives initialization of the filing context', async () => {
  const originalFetch = globalThis.fetch
  let finishMode
  globalThis.fetch = () => new Promise(resolve => { finishMode = resolve })
  try {
    const pending = getDemoMode()
    resetSystems()
    finishMode(new Response(JSON.stringify({ enabled: true })))
    assert.deepEqual(await pending, { enabled: true })
  } finally {
    globalThis.fetch = originalFetch
    resetSystems()
  }
})
