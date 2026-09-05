import assert from 'node:assert/strict'
import test from 'node:test'
import { askArchive, login, uploadDocument } from './api.js'

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
