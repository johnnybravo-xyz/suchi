import assert from 'node:assert/strict'
import test from 'node:test'
import { DATE_ROLES, formatIntelligenceValue, intelligenceDateValue, intelligenceRoleLabel } from './intelligence.js'

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
