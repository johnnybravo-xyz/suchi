import assert from 'node:assert/strict'
import test from 'node:test'
import { USER_CAPABILITIES, hasCapability } from './capabilities.js'

test('keeps the frontend capability catalog unique', () => {
  const keys = USER_CAPABILITIES.map(capability => capability.key)
  assert.deepEqual(keys, ['archive_chat', 'archive_intelligence', 'mailboxes', 'share_links', 'share_views'])
  assert.equal(new Set(keys).size, keys.length)
  assert.equal(USER_CAPABILITIES.find(capability => capability.key === 'archive_intelligence')?.label, 'Manage document dates')
})

test('treats admins as implicitly capable and checks member grants', () => {
  assert.equal(hasCapability({ role: 'admin', capabilities: [] }, 'share_views'), true)
  assert.equal(hasCapability({ role: 'member', capabilities: ['share_views'] }, 'share_views'), true)
  assert.equal(hasCapability({ role: 'member', capabilities: [] }, 'share_views'), false)
})
