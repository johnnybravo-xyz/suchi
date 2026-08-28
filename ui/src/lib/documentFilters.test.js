import assert from 'node:assert/strict'
import test from 'node:test'
import { documentListHash, parseSavedViewFilters } from './documentFilters.js'

test('keeps backend JD field names out of document routes', () => {
  const hash = documentListHash({ q: 'paris', jd_category_id: 6, sensitivity: 'internal' })
  assert.equal(hash, '#/documents?q=paris&jd=6&sensitivity=internal')
  assert.equal(hash.includes('jd_category_id'), false)
})

test('omits empty filters and a trailing question mark', () => {
  assert.equal(documentListHash({ q: '', jd_category_id: null }), '#/documents')
})

test('parses saved-view filters defensively', () => {
  assert.deepEqual(parseSavedViewFilters('{"q":"paris"}'), { q: 'paris' })
  assert.deepEqual(parseSavedViewFilters('not json'), {})
  assert.deepEqual(parseSavedViewFilters([]), {})
})
