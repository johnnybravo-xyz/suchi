// SPDX-License-Identifier: AGPL-3.0-or-later

import assert from 'node:assert/strict'
import test from 'node:test'
import { parseDocumentReference } from './documentReferences.js'

const origin = 'https://archive.example'
const appPath = '/app/'

test('parses exact document IDs and canonical same-instance links', () => {
  assert.deepEqual(parseDocumentReference('42', origin, appPath), { id: 42 })
  assert.deepEqual(parseDocumentReference('https://archive.example/app/#/doc/73', origin, appPath), { id: 73 })
  assert.deepEqual(parseDocumentReference('#/doc/74?system=S02', origin, appPath), { id: 74 })
  assert.deepEqual(parseDocumentReference('/api/documents/91', origin, appPath), { id: 91 })
})

test('preserves encoded canonical document URLs', () => {
  const encoded = encodeURIComponent('https://archive.example/app/#/doc/73?system=S02')
  assert.deepEqual(parseDocumentReference(encoded, origin, appPath), { id: 73 })
  assert.deepEqual(parseDocumentReference('https://archive.example/app/#%2Fdoc%2F74%3Fsystem%3DS02', origin, appPath), { id: 74 })
  assert.deepEqual(parseDocumentReference('%2Fapi%2Fdocuments%2F91', origin, appPath), { id: 91 })
})

test('leaves ordinary text and non-URL schemes for authorized search', () => {
  assert.equal(parseDocumentReference('March receipt', origin, appPath), null)
  assert.equal(parseDocumentReference('javascript:alert(1)', origin, appPath), null)
})

test('rejects remote document URLs before any request', () => {
  for (const value of [
    'https://remote.example/#/doc/42',
    '//remote.example/#/doc/42',
    'https://archive.example@remote.example/#/doc/42',
    encodeURIComponent('https://remote.example/#/doc/42'),
  ]) {
    assert.deepEqual(parseDocumentReference(value, origin, appPath), {
      error: 'Only links from this archive can be used.',
    })
  }
})

test('rejects credentials and non-canonical same-instance routes', () => {
  const invalid = [
    'https://remote.example@archive.example/app/#/doc/42',
    'https://archive.example/settings',
    'https://archive.example/other/#/doc/42',
    'https://archive.example/other/api/documents/42',
    'https://archive.example/api/documents/42%2Fevil',
    'https://archive.example/app/#/doc/42/anything',
    'https://archive.example/app/#/doc/42?system=S02&next=%2Fsettings',
    'https://archive.example/app/#/doc/42?system=invalid',
  ]
  for (const value of invalid) {
    assert.ok(parseDocumentReference(value, origin, appPath)?.error, value)
  }
})
