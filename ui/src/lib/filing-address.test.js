// SPDX-License-Identifier: AGPL-3.0-or-later

import assert from 'node:assert/strict'
import test from 'node:test'
import { isCanonicalFilingAddress, resolvedFilingAddressHash } from './filing-address.js'

test('recognizes only canonical category filing addresses', () => {
  for (const value of ['S01.21', 'Z99.99']) {
    assert.equal(isCanonicalFilingAddress(value), true, value)
  }
  for (const value of ['', '21', '20.21', '40.49', '00.00', 'S01.2', 's01.21', 'S01.21.147', ' S01.21', 'S01.21 ']) {
    assert.equal(isCanonicalFilingAddress(value), false, value)
  }
})

test('routes resolved addresses to their category and system', () => {
  assert.equal(
    resolvedFilingAddressHash({ system_code: 'S01', jd_category_id: 121 }),
    '#/documents?system=S01&jd=121',
  )
})
