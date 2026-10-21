// SPDX-License-Identifier: AGPL-3.0-or-later

// Source details for these kinds are the acquired file's name or relative
// path. Mailbox details name the account folder, never a file.
const FILE_DETAIL_KINDS = new Set(['upload', 'api', 'watched_folder', 'import'])

function splitPath(detail) {
  return (detail || '').trim().replaceAll('\\', '/').split('/')
}

// originalNames returns distinct file names in acquisition order. The API
// returns sources oldest first, so the first name belongs to the winning
// acquisition.
export function originalNames(sources) {
  const names = new Map()
  for (const source of sources || []) {
    if (!FILE_DETAIL_KINDS.has(source.kind)) continue
    const name = splitPath(source.detail).pop()
    if (name && !names.has(name)) names.set(name, source)
  }
  return [...names].map(([name, source]) => ({ name, source }))
}

// sourcePlace is where within a channel a source was seen. File names are
// shown with the original instead, so watched folders keep only the
// directory and uploads, API calls, and imports have no place.
export function sourcePlace(source) {
  const detail = source.detail?.trim() || ''
  if (source.kind === 'watched_folder') return splitPath(detail).slice(0, -1).join('/')
  if (source.kind !== 'mailbox') return ''
  const label = source.label?.trim() || ''
  if (!label) return detail
  if (detail === label) return ''
  const repeatedPrefix = `${label} / `
  return detail.startsWith(repeatedPrefix) ? detail.slice(repeatedPrefix.length) : detail
}

// sourceGroups collapses repeat observations through one channel (kind and
// label) into one entry, in first-seen order. Places are distinct and newest
// first, so folder renames fold under the current name.
export function sourceGroups(sources) {
  const groups = new Map()
  for (const source of sources || []) {
    const key = `${source.kind}\u0000${source.label || ''}`
    if (!groups.has(key)) groups.set(key, { kind: source.kind, label: source.label, places: [] })
    const group = groups.get(key)
    const place = sourcePlace(source)
    if (place && !group.places.includes(place)) group.places.unshift(place)
  }
  return [...groups.values()]
}
