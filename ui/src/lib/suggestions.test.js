// SPDX-License-Identifier: AGPL-3.0-or-later

import assert from 'node:assert/strict'
import test from 'node:test'
import { canApplySuggestion, filingSuggestions } from './suggestions.js'

const filing = (overrides = {}) => ({
  id: 7, doc_id: 42, approval_name: 'document-change', choices: ['apply', 'reject'],
  vars: { field: 'jd_category', proposed_id: 21, proposed_value: 'Banking', source_current: true, review_conflict: false, reason: 'review_first' },
  ...overrides,
})

test('applies only current, unconflicted suggestions waiting for review', () => {
  assert.equal(canApplySuggestion(filing()), true)
  assert.equal(canApplySuggestion(filing({ choices: ['reject'] })), false)
  assert.equal(canApplySuggestion(filing({ vars: { ...filing().vars, source_current: false } })), false)
  assert.equal(canApplySuggestion(filing({ vars: { ...filing().vars, review_conflict: true } })), false)
  assert.equal(canApplySuggestion(filing({ vars: { ...filing().vars, reason: 'automatic' } })), false)
})

test('keys applicable filing suggestions by document', () => {
  const suggestions = filingSuggestions([
    filing(),
    filing({ id: 8, doc_id: 43, vars: { ...filing().vars, field: 'tag' } }),
    filing({ id: 9, doc_id: 44, vars: { ...filing().vars, proposed_id: 0 } }),
  ])
  assert.deepEqual([...suggestions], [[42, { taskID: 7, categoryID: 21, label: 'Banking' }]])
})
