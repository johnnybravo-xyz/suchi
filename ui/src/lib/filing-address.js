// SPDX-License-Identifier: AGPL-3.0-or-later

// Only introduced filing systems have addresses (SYS.AC); a bare category
// code stays ordinary search text, and `jd:21` filters by category.
export function isCanonicalFilingAddress(value) {
  return /^[A-Z][0-9]{2}\.[0-9]{2}$/.test(value)
}

export function resolvedFilingAddressHash(result) {
  return `#/documents?system=${encodeURIComponent(result.system_code)}&jd=${encodeURIComponent(result.jd_category_id)}`
}
