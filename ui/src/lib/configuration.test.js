// SPDX-License-Identifier: AGPL-3.0-or-later

import assert from 'node:assert/strict'
import test from 'node:test'
import { archiveSettingsItems, archiveSettingsSectionAvailable } from './configuration.js'

test('derives archive settings inventory from deployment features', () => {
  const disabled = archiveSettingsItems({ features: {} }).map(item => item.name)
  assert.equal(disabled.includes('matching'), true)
  assert.equal(disabled.includes('mail'), false)
  assert.equal(disabled.includes('llm'), false)
  assert.equal(archiveSettingsSectionAvailable({ features: {} }, 'llm'), false)

  const enabled = archiveSettingsItems({
    features: { email_ingestion: true, llm_connections: true },
  }).map(item => item.name)
  assert.equal(enabled.includes('mail'), true)
  assert.equal(enabled.includes('llm'), true)
  assert.equal(archiveSettingsSectionAvailable({ features: { llm_connections: true } }, 'llm'), true)
})
