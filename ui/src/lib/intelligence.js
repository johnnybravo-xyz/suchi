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

// The compact month grid shows documents, not every semantic role attached to
// the same date. Keep the first API-ordered event for each document and day.
// Month/year values contain sorting placeholders, not exact dates.
export function groupCalendarEvents(items) {
  const grouped = new Map()
  const seen = new Set()
  for (const event of items) {
    const precision = event.value?.precision
    if (precision === 'month' || precision === 'year') continue
    const date = intelligenceDateValue(event)
    const identity = `${date}:${event.document_id ?? `event-${event.id}`}`
    if (seen.has(identity)) continue
    seen.add(identity)
    if (!grouped.has(date)) grouped.set(date, [])
    grouped.get(date).push(event)
  }
  return grouped
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

export function formatIntelligenceDate(candidate, options) {
  const precision = candidate?.value?.precision
  if (precision === 'month') options = { year: 'numeric', month: 'short' }
  else if (precision === 'year') options = { year: 'numeric' }
  return formatArchiveDate(intelligenceDateValue(candidate), options)
}

export function formatIntelligenceValue(candidate) {
  if (candidate?.type === 'date') {
    return `${formatIntelligenceDate(candidate)} · ${candidate.role || 'date'}`
  }
  return candidate?.raw_text || candidate?.sort_value || candidate?.type || 'Extracted fact'
}
