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

export function formatArchiveDate(value, options = { year: 'numeric', month: 'short', day: 'numeric' }) {
  if (!value) return 'Unknown date'
  return new Date(`${value}T00:00:00`).toLocaleDateString(undefined, options)
}

export function formatIntelligenceValue(candidate) {
  if (candidate?.type === 'date') {
    return `${formatArchiveDate(intelligenceDateValue(candidate))} · ${candidate.role || 'date'}`
  }
  return candidate?.raw_text || candidate?.sort_value || candidate?.type || 'Intelligence candidate'
}
