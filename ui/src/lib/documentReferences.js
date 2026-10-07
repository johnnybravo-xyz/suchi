// SPDX-License-Identifier: AGPL-3.0-or-later

// Returns an exact document ID, an error for an unsafe/invalid document URL,
// or null when the input should be treated as ordinary search text.
export function parseDocumentReference(input, origin, appPath = '/') {
  const value = String(input || '').trim()
  if (!value) return null
  if (/^\d+$/.test(value)) {
    const id = Number(value)
    return Number.isSafeInteger(id) && id > 0 ? { id } : { error: 'Enter a valid document ID.' }
  }

  const looksLikeURL = candidate =>
    /^https?:\/\//i.test(candidate) || candidate.startsWith('/') || candidate.startsWith('#')
  let candidate = value
  if (!looksLikeURL(candidate)) {
    try {
      candidate = decodeURIComponent(candidate)
    } catch {
      return null
    }
    if (!looksLikeURL(candidate)) return null
  }

  let base
  let url
  try {
    base = new URL(appPath, origin)
    url = new URL(candidate, base)
  } catch {
    return { error: 'Enter a valid document URL.' }
  }
  if (url.origin !== base.origin) return { error: 'Only links from this archive can be used.' }
  if (url.username || url.password) return { error: 'Enter a valid document URL.' }

  let pathname
  let basePathname
  let hash
  try {
    pathname = decodeURIComponent(url.pathname)
    basePathname = decodeURIComponent(base.pathname)
    hash = decodeURIComponent(url.hash)
  } catch {
    return { error: 'Enter a valid document URL.' }
  }

  let match = null
  if (!url.search && pathname === basePathname) {
    const route = hash.match(/^#\/doc\/([1-9]\d*)(?:\?(.+))?$/)
    if (route) {
      const params = new URLSearchParams(route[2] || '')
      const entries = [...params]
      if (!entries.length ||
          (entries.length === 1 && entries[0][0] === 'system' && /^[A-Z][0-9]{2}$/.test(entries[0][1]))) {
        match = route
      }
    }
  }
  if (!match && !url.search && !hash) {
    match = pathname.match(/^\/api\/documents\/([1-9]\d*)$/)
  }
  if (!match) return { error: 'That link does not identify a document.' }

  const id = Number(match[1])
  return Number.isSafeInteger(id) ? { id } : { error: 'Enter a valid document ID.' }
}
