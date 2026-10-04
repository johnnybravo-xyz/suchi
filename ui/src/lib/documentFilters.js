// SPDX-License-Identifier: AGPL-3.0-or-later

// Saved views persist API filter names. Browser routes keep their shorter,
// user-facing names and translate back at the Documents boundary.
export function documentListHash(filters = {}) {
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(filters)) {
    if (value === '' || value == null) continue
    params.set(key === 'jd_category_id' ? 'jd' : key, String(value))
  }
  const query = params.toString()
  return `#/documents${query ? `?${query}` : ''}`
}

export function parseSavedViewFilters(raw = '{}') {
  try {
    const filters = typeof raw === 'string' ? JSON.parse(raw) : raw
    return filters && typeof filters === 'object' && !Array.isArray(filters) ? filters : {}
  } catch {
    return {}
  }
}

function quotedQueryValue(value) {
  return `"${String(value).replaceAll('\\', '\\\\').replaceAll('"', '\\"')}"`
}

export function fieldPresenceSelection(name, missing = false) {
  return name ? JSON.stringify({ name: String(name), missing: !!missing }) : ''
}

export function parseFieldPresenceSelection(value) {
  try {
    const parsed = JSON.parse(value)
    return parsed && typeof parsed.name === 'string' && parsed.name
      ? { name: parsed.name, missing: !!parsed.missing }
      : null
  } catch {
    return null
  }
}

export function extractFieldPresence(query = '') {
  const text = String(query)
  const match = /(^|\s)(-?)has-field:(?:"((?:\\.|[^"\\])*)"|([^\s]+))(?=\s|$)/i.exec(text)
  if (!match) return { query: text, selection: '' }
  const name = (match[3] ?? match[4] ?? '').replace(/\\(.)/g, '$1')
  return {
    query: (text.slice(0, match.index) + text.slice(match.index + match[0].length)).trim(),
    selection: fieldPresenceSelection(name, match[2] === '-'),
  }
}

export function canonicalSavedViewQuery(draft, { tags = [], correspondents = [], types = [], categories = [] } = {}) {
  const parts = []
  const text = String(draft?.q || '').trim()
  if (text) parts.push(text)

  const tag = tags.find((item) => String(item.id) === String(draft?.tag))
  if (tag) parts.push(`tag:${quotedQueryValue(tag.name)}`)
  const correspondent = correspondents.find((item) => String(item.id) === String(draft?.corr))
  if (correspondent) parts.push(`from:${quotedQueryValue(correspondent.name)}`)
  const type = types.find((item) => String(item.id) === String(draft?.type))
  if (type) parts.push(`type:${quotedQueryValue(type.name)}`)
  const category = categories.find((item) => String(item.id) === String(draft?.jd))
  if (category) parts.push(`jd:${category.code}`)
  const fieldPresence = parseFieldPresenceSelection(draft?.fieldPresence)
  if (fieldPresence) {
    parts.push(`${fieldPresence.missing ? '-' : ''}has-field:${quotedQueryValue(fieldPresence.name)}`)
  }
  if (draft?.sens) parts.push(`sensitivity:${draft.sens}`)
  if (draft?.dateFrom) parts.push(`date:>=${draft.dateFrom}`)
  if (draft?.dateTo) parts.push(`date:<=${draft.dateTo}`)
  if (draft?.dateRole) parts.push(`date-role:${draft.dateRole}`)
  return parts.join(' ')
}
