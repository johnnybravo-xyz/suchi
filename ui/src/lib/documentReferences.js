// SPDX-License-Identifier: AGPL-3.0-or-later

// Returns an exact document ID, an error for an unsafe/invalid document URL,
// or null when the input should be treated as ordinary search text.
export function parseDocumentReference(input, origin) {
  const value = String(input || '').trim()
  if (!value) return null
  if (/^\d+$/.test(value)) {
    const id = Number(value)
    return Number.isSafeInteger(id) && id > 0 ? { id } : { error: 'Enter a valid document ID.' }
  }

  const looksLikeURL = /^https?:\/\//i.test(value) || value.startsWith('/') || value.startsWith('#')
  if (!looksLikeURL) return null
  let url
  try {
    url = new URL(value, origin)
  } catch {
    return { error: 'Enter a valid document URL.' }
  }
  if (url.origin !== origin) return { error: 'Only links from this archive can be used.' }
  const route = `${url.pathname}${url.hash}`
  const match = route.match(/(?:#\/doc\/|\/api\/documents\/)(\d+)(?:\b|\/)/)
  if (!match) return { error: 'That link does not identify a document.' }
  const id = Number(match[1])
  return Number.isSafeInteger(id) && id > 0 ? { id } : { error: 'Enter a valid document ID.' }
}
