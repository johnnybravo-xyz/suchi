// SPDX-License-Identifier: AGPL-3.0-or-later

import assert from 'node:assert/strict'
import test from 'node:test'
import { originalNames, sourceGroups } from './documentSources.js'

test('lists distinct original file names in acquisition order', () => {
  const sources = [
    { kind: 'api', label: 'Ritesh', detail: 'Scan.pdf' },
    { kind: 'mailbox', label: 'Inbox', detail: 'me / INBOX' },
    { kind: 'upload', label: 'Ritesh', detail: 'Scan.pdf' },
    { kind: 'watched_folder', label: 'Drop', detail: 'bills\\oct\\inv.pdf' },
  ]
  assert.deepEqual(originalNames(sources).map((entry) => entry.name), ['Scan.pdf', 'inv.pdf'])
  assert.deepEqual(originalNames([{ kind: 'mailbox', label: 'Inbox', detail: 'me / INBOX' }]), [])
})

test('folds repeat observations of one channel under its newest place', () => {
  const groups = sourceGroups([
    { kind: 'api', label: 'Ritesh', detail: 'a.pdf' },
    { kind: 'mailbox', label: 'Work', detail: 'me / Old' },
    { kind: 'api', label: 'Ritesh', detail: 'b.pdf' },
    { kind: 'mailbox', label: 'Work', detail: 'me / New' },
    { kind: 'watched_folder', label: 'Drop', detail: 'root.pdf' },
  ])
  assert.deepEqual(groups, [
    { kind: 'api', label: 'Ritesh', places: [] },
    { kind: 'mailbox', label: 'Work', places: ['me / New', 'me / Old'] },
    { kind: 'watched_folder', label: 'Drop', places: [] },
  ])
  assert.deepEqual(
    sourceGroups([{ kind: 'mailbox', label: 'Work', detail: 'Work / INBOX' }])[0].places,
    ['INBOX'],
  )
})
