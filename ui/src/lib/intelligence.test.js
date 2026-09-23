// SPDX-License-Identifier: AGPL-3.0-or-later

import assert from 'node:assert/strict'
import test from 'node:test'
import { canReviewDate, formatIntelligenceDate, formatIntelligenceValue, groupCalendarEvents, intelligenceDateValue, reviewFailure } from './intelligence.js'

test('formats dates only to their recorded precision, including storage placeholders', () => {
  const fullDate = { year: 'numeric', month: 'short', day: 'numeric' }
  const caption = { ...fullDate, weekday: 'short' }
  for (const { precision, date, options } of [
    { precision: 'day', date: '2026-09-14', options: fullDate },
    { precision: undefined, date: '2026-09-14', options: fullDate },
    { precision: 'month', date: '2026-09-01', options: { year: 'numeric', month: 'short' } },
    { precision: 'year', date: '2026-01-01', options: { year: 'numeric' } },
  ]) {
    const candidate = { type: 'date', role: 'renewal', sort_value: date, value: { date, precision } }
    const dateValue = new Date(`${date}T00:00:00`)
    const expected = new Intl.DateTimeFormat(undefined, options).format(dateValue)
    assert.equal(intelligenceDateValue(candidate), date)
    assert.equal(formatIntelligenceDate(candidate), expected)
    assert.equal(formatIntelligenceValue(candidate), `${expected} · renewal`)
    assert.equal(formatIntelligenceDate(candidate, caption),
      new Intl.DateTimeFormat(undefined, precision === 'month' || precision === 'year' ? options : caption).format(dateValue))
  }
})

test('groups one compact calendar entry per document and exact day, ignoring partial dates', () => {
  const events = [
    { id: 14, document_id: 60, role: 'issued', value: { date: '2026-08-01', precision: 'month' } },
    { id: 15, document_id: 60, role: 'issued', value: { date: '2026-01-01', precision: 'year' } },
    { id: 16, document_id: 60, role: 'start', value: { date: '2026-08-03' } },
    { id: 17, document_id: 60, role: 'end', value: { date: '2026-08-31' } },
    { id: 18, document_id: 60, role: 'issued', value: { date: '2026-08-31' } },
    { id: 19, document_id: 61, role: 'issued', value: { date: '2026-08-31' } },
    { id: 20, document_id: 60, role: 'start', value: { date: '2026-08-01', precision: 'day' } },
    { id: 21, document_id: 62, role: 'issued', value: { date: '2026-09-01', precision: 'month' } },
  ]

  const grouped = groupCalendarEvents(events)
  assert.deepEqual(grouped.get('2026-08-03').map(event => event.id), [16])
  assert.deepEqual(grouped.get('2026-08-31').map(event => event.id), [17, 19])
  assert.deepEqual(grouped.get('2026-08-01').map(event => event.id), [20])
  assert.equal(grouped.has('2026-01-01'), false)
  assert.equal(grouped.has('2026-09-01'), false)
})

test('only fresh known pending dates can be selected, regardless of classifier confidence', () => {
  const candidate = { type: 'date', role: 'renewal', status: 'pending', source_current: true, reason: 'important_fact', confidence: 1 }
  assert.equal(canReviewDate(candidate), true)
  assert.equal(canReviewDate({ ...candidate, confidence: 0.1, reason: 'low_confidence' }), true)
  assert.equal(canReviewDate({ ...candidate, source_current: false }), false)
  assert.equal(canReviewDate({ ...candidate, source_current: undefined }), false)
  assert.equal(canReviewDate({ ...candidate, reason: 'evidence_mismatch' }), false)
  assert.equal(canReviewDate({ ...candidate, status: 'accepted' }), false)
  assert.equal(canReviewDate({ ...candidate, type: 'reminder' }), false)
  assert.equal(canReviewDate({ ...candidate, role: 'transfer' }), false)
})

test('review failures distinguish changed evidence, hidden sources, revoked access and unconfirmed offline results', () => {
  assert.match(reviewFailure({ code: 'stale_proposal', status: 409 }), /changed.*Nothing was applied/)
  assert.match(reviewFailure({ code: 'invalid_evidence', status: 409 }), /evidence cannot be verified/)
  assert.match(reviewFailure({ code: 'reveal_required', status: 403 }), /hidden.*reveal controls/)
  assert.match(reviewFailure({ code: 'forbidden', status: 403 }), /access is no longer available/)
  assert.match(reviewFailure(new TypeError('Failed to fetch')), /unconfirmed.*reconnect/)
  assert.match(reviewFailure({ code: 'no_task', status: 404 }), /no longer available to you/)
})
