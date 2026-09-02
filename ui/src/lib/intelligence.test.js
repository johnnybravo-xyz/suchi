import assert from 'node:assert/strict'
import test from 'node:test'
import { DATE_ROLES, formatIntelligenceValue, groupCalendarEvents, intelligenceDateValue, intelligenceRoleLabel } from './intelligence.js'

test('keeps the date role vocabulary stable', () => {
  assert.deepEqual([...DATE_ROLES], ['issued', 'due', 'start', 'end', 'expiry', 'renewal', 'service', 'other'])
  assert.equal(intelligenceRoleLabel('renewal'), 'Renewal')
})

test('formats date intelligence from the generic value envelope', () => {
  const candidate = {
    type: 'date', role: 'renewal', sort_value: '2026-09-01',
    value: { date: '2026-09-01', precision: 'day' },
  }
  assert.equal(intelligenceDateValue(candidate), '2026-09-01')
  assert.match(formatIntelligenceValue(candidate), /2026.*renewal/i)
})

test('groups one compact calendar entry per document and day', () => {
  const events = [
    { id: 16, document_id: 60, role: 'start', value: { date: '2026-08-03' } },
    { id: 17, document_id: 60, role: 'end', value: { date: '2026-08-31' } },
    { id: 18, document_id: 60, role: 'issued', value: { date: '2026-08-31' } },
    { id: 19, document_id: 61, role: 'issued', value: { date: '2026-08-31' } },
  ]

  const grouped = groupCalendarEvents(events)
  assert.deepEqual(grouped.get('2026-08-03').map(event => event.id), [16])
  assert.deepEqual(grouped.get('2026-08-31').map(event => event.id), [17, 19])
})
