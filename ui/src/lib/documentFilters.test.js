// SPDX-License-Identifier: AGPL-3.0-or-later

import assert from 'node:assert/strict'
import test from 'node:test'
import {
  canonicalSavedViewQuery,
  documentListHash,
  extractFieldPresence,
  fieldPresenceSelection,
  parseFieldPresenceSelection,
  parseSavedViewFilters,
} from './documentFilters.js'

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
    { q: '"distribution advice"', tag: '7', corr: '9', jd: '6', sens: 'internal', dateFrom: '2026-09-01', dateTo: '2026-09-30', dateRole: 'renewal' },
    {
      tags: [{ id: 7, name: 'income tax' }],
      correspondents: [{ id: 9, name: 'Bagmane "Prime"' }],
      categories: [{ id: 6, code: 22, name: 'Investments' }],
    },
  )
  assert.equal(
    query,
    '"distribution advice" tag:"income tax" from:"Bagmane \\"Prime\\"" jd:22 sensitivity:internal date:>=2026-09-01 date:<=2026-09-30 date-role:renewal',
  )
})

test('round-trips one custom-field presence control through the rich query', () => {
  const selection = fieldPresenceSelection('Payment "receipt"', true)
  assert.deepEqual(parseFieldPresenceSelection(selection), { name: 'Payment "receipt"', missing: true })
  assert.equal(
    canonicalSavedViewQuery({ q: 'type:invoice', fieldPresence: selection }),
    'type:invoice -has-field:"Payment \\"receipt\\""',
  )
  assert.deepEqual(
    extractFieldPresence('type:invoice -has-field:"Payment \\"receipt\\""'),
    { query: 'type:invoice', selection },
  )
})

test('serializes exact document snapshots without granting access', () => {
  assert.equal(documentListHash({ document_ids: [17, 28, 39] }), '#/documents?document_ids=17%2C28%2C39')
})
