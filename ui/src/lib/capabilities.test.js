// SPDX-License-Identifier: AGPL-3.0-or-later

import assert from 'node:assert/strict'
import test from 'node:test'
import { availableCapabilities, hasCapability, hasFeature } from './capabilities.js'

test('conjoins feature availability with role and member capability grants', () => {
  const enabled = { share_links: true }
  assert.equal(hasCapability({ role: 'admin', capabilities: [], features: enabled }, 'share_links'), true)
  assert.equal(hasCapability({ role: 'member', capabilities: ['share_links'], features: enabled }, 'share_links'), true)
  assert.equal(hasCapability({ role: 'member', capabilities: [], features: enabled }, 'share_links'), false)
  assert.equal(hasCapability({ role: 'admin', capabilities: [], features: {} }, 'share_links'), false)
  assert.equal(hasCapability({ role: 'member', capabilities: ['share_links'], features: {} }, 'share_links'), false)
  assert.equal(hasCapability({ role: 'admin', capabilities: [], features: {} }, 'share_views'), true)
})

test('requires an explicit true feature and hides disabled capability controls', () => {
  assert.equal(hasFeature({ features: { mobile_pairing: true } }, 'mobile_pairing'), true)
  assert.equal(hasFeature({ features: { mobile_pairing: false } }, 'mobile_pairing'), false)
  assert.equal(hasFeature({}, 'mobile_pairing'), false)

  const visible = availableCapabilities({
    features: { llm_connections: false, email_ingestion: true, share_links: false },
  }).map(capability => capability.key)
  assert.deepEqual(visible, ['archive_intelligence', 'mailboxes', 'share_views'])
})
