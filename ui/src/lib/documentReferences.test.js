// SPDX-License-Identifier: AGPL-3.0-or-later

import assert from 'node:assert/strict'
import test from 'node:test'
import { parseDocumentReference } from './documentReferences.js'

const origin = 'https://archive.example'

test('parses exact document IDs and same-instance links', () => {
  assert.deepEqual(parseDocumentReference('42', origin), { id: 42 })
  assert.deepEqual(parseDocumentReference('https://archive.example/#/doc/73', origin), { id: 73 })
  assert.deepEqual(parseDocumentReference('/api/documents/91', origin), { id: 91 })
})

test('leaves ordinary text for authorized search', () => {
  assert.equal(parseDocumentReference('March receipt', origin), null)
})

test('rejects remote and non-document URLs before any request', () => {
  assert.deepEqual(parseDocumentReference('https://remote.example/#/doc/42', origin), {
    error: 'Only links from this archive can be used.',
  })
  assert.deepEqual(parseDocumentReference('https://archive.example/settings', origin), {
    error: 'That link does not identify a document.',
  })
})
