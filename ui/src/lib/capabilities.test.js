import assert from 'node:assert/strict'
import test from 'node:test'
import { hasCapability } from './capabilities.js'

test('treats admins as implicitly capable and checks member grants', () => {
  assert.equal(hasCapability({ role: 'admin', capabilities: [] }, 'share_views'), true)
  assert.equal(hasCapability({ role: 'member', capabilities: ['share_views'] }, 'share_views'), true)
  assert.equal(hasCapability({ role: 'member', capabilities: [] }, 'share_views'), false)
})
