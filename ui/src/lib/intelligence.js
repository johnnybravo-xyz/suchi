export const DATE_ROLES = Object.freeze([
  'issued', 'due', 'start', 'end', 'expiry', 'renewal', 'service', 'other',
])

export function intelligenceRoleLabel(value) {
  const role = String(value || '').trim()
  return role ? role.charAt(0).toUpperCase() + role.slice(1) : 'Date'
}

export function intelligenceDateValue(candidate) {
  return candidate?.value?.date || candidate?.sort_value || ''
}

const dateFormatters = new Map()

export function formatArchiveDate(value, options = { year: 'numeric', month: 'short', day: 'numeric' }) {
  if (!value) return 'Unknown date'
  const date = new Date(`${value}T00:00:00`)
  if (Number.isNaN(date.getTime())) return 'Invalid Date'
  // Calendar can format hundreds of dates; constructing Intl formatters dominates that render.
  const key = Object.entries(options).sort().map(([name, setting]) => `${name}:${setting}`).join('|')
  let formatter = dateFormatters.get(key)
  if (!formatter) {
    formatter = new Intl.DateTimeFormat(undefined, options)
    dateFormatters.set(key, formatter)
  }
  return formatter.format(date)
}

export function formatIntelligenceValue(candidate) {
  if (candidate?.type === 'date') {
    return `${formatArchiveDate(intelligenceDateValue(candidate))} · ${candidate.role || 'date'}`
  }
  return candidate?.raw_text || candidate?.sort_value || candidate?.type || 'Extracted fact'
}
