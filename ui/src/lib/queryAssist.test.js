import assert from 'node:assert/strict'
import test from 'node:test'
import { createQueryAssistant } from './queryAssist.js'

async function waitFor(predicate) {
  for (let attempt = 0; attempt < 40; attempt++) {
    if (predicate()) return
    await new Promise((resolve) => setTimeout(resolve, 5))
  }
  assert.fail('timed out waiting for autocomplete')
}

test('aborts a superseded autocomplete request', async () => {
  const originalFetch = globalThis.fetch
  const calls = []
  globalThis.fetch = (url, options = {}) => new Promise((resolve, reject) => {
    const call = { url: String(url), signal: options.signal, resolve }
    calls.push(call)
    options.signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')), { once: true })
  })

  const suggestions = []
  const assistant = createQueryAssistant((next) => suggestions.push(next), { delay: 0 })
  try {
    assistant.update('first')
    await waitFor(() => calls.length === 1)
    assistant.update('second')
    await waitFor(() => calls.length === 2)

    assert.equal(calls[0].signal.aborted, true)
    calls[1].resolve(new Response(JSON.stringify({
      results: [{ query: 'second result', value: 'second result' }],
    }), { headers: { 'content-type': 'application/json' } }))
    await waitFor(() => suggestions.length === 1)
    assert.deepEqual(suggestions[0], [{ query: 'second result', value: 'second result' }])
  } finally {
    assistant.dispose()
    globalThis.fetch = originalFetch
  }
})
