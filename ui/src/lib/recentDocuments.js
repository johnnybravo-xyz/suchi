// SPDX-License-Identifier: AGPL-3.0-or-later

// Recently opened documents, kept in this browser per user and filing system
// for the Omnibox. Callers omit titles of high-sensitivity documents, and
// sign-out forgets the list.

const LIMIT = 8
const prefix = userID => `suchi.recentDocuments.${userID}.`
const storageKey = (userID, system) => `${prefix(userID)}${system || 'default'}`

export function recentDocuments(userID, system) {
  try {
    const items = JSON.parse(localStorage.getItem(storageKey(userID, system)) || '[]')
    return Array.isArray(items)
      ? items.filter(item => Number.isSafeInteger(item?.id) && item.id > 0 && typeof item.title === 'string')
      : []
  } catch {
    return []
  }
}

export function rememberDocument(userID, system, document) {
  if (!userID || !Number.isSafeInteger(document?.id)) return
  try {
    const items = [document, ...recentDocuments(userID, system).filter(item => item.id !== document.id)]
    localStorage.setItem(storageKey(userID, system), JSON.stringify(items.slice(0, LIMIT)))
  } catch {}
}

export function forgetRecentDocuments(userID) {
  try {
    for (const name of Object.keys(localStorage)) {
      if (name.startsWith(prefix(userID))) localStorage.removeItem(name)
    }
  } catch {}
}
