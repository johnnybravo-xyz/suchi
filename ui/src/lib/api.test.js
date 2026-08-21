import assert from 'node:assert/strict'
import test from 'node:test'
import { ApiError, req } from './api.js'

test('uses the canonical API error message', async () => {
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
      () => req('POST', '/api/email-accounts/oauth/complete', {}),
      (err) => err instanceof ApiError &&
        err.code === 'oauth_failed' &&
        err.message === 'Microsoft did not complete sign-in',
    )
  } finally {
    globalThis.fetch = originalFetch
  }
})
