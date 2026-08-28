import assert from 'node:assert/strict'
import test from 'node:test'
import {
  SENSITIVITY_OPTIONS, isHighSensitivity, sensDot, sensitivityLabel,
} from './format.js'

test('keeps the sensitivity vocabulary and privacy posture aligned', () => {
  assert.deepEqual(
    SENSITIVITY_OPTIONS.map((option) => option.value),
    ['public', 'internal', 'confidential', 'restricted'],
  )
  assert.equal(isHighSensitivity('confidential'), true)
  assert.equal(isHighSensitivity('restricted'), true)
  assert.equal(isHighSensitivity('internal'), false)
  assert.equal(sensDot('restricted'), 'danger')
  assert.equal(sensitivityLabel('restricted'), 'Restricted')
  assert.equal(sensitivityLabel(''), 'Unset')
})
