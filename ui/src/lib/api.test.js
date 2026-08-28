import assert from 'node:assert/strict'
import test from 'node:test'
import { login } from './api.js'

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
