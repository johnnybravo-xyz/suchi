import assert from 'node:assert/strict'
import test from 'node:test'
import { canonicalSavedViewQuery, documentListHash, parseSavedViewFilters } from './documentFilters.js'

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

test('serializes new saved views to one stable query string', () => {
  const query = canonicalSavedViewQuery(
    { q: '"distribution advice"', tag: '7', corr: '9', type: '10', jd: '6', sens: 'internal' },
    {
      tags: [{ id: 7, name: 'income tax' }],
      correspondents: [{ id: 9, name: 'Bagmane "Prime"' }],
      types: [{ id: 10, name: 'statement' }],
      categories: [{ id: 6, code: 22, name: 'Investments' }],
    },
  )
  assert.equal(
    query,
    '"distribution advice" tag:"income tax" from:"Bagmane \\"Prime\\"" type:"statement" jd:22 sensitivity:internal',
  )
})
