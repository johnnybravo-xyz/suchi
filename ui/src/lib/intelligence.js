// SPDX-License-Identifier: AGPL-3.0-or-later

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

// A score never selects or authorizes a candidate. Only the live review
// projection can make a known date available for explicit selection.
export function canReviewDate(candidate) {
  return candidate?.type === 'date' && DATE_ROLES.includes(candidate.role) &&
    candidate.status === 'pending' && candidate.source_current === true &&
    ['important_fact', 'low_confidence', 'review_first'].includes(candidate.reason)
}

export function reviewReason(reason) {
  return {
    review_first: 'New inferred metadata needs your review.',
    important_fact: 'Dates can affect important decisions. Check the source before adding this date.',
    low_confidence: 'The classifier confidence is below the review threshold. Check the source carefully.',
    source_changed: 'The source changed after this suggestion. Extract a fresh suggestion.',
    source_unavailable: 'The source cannot currently be verified.',
    human_changed: 'The current value changed after this suggestion. Review a fresh suggestion instead.',
    human_unverified: 'The current value could not be verified.',
    evidence_missing: 'Exact source evidence is unavailable.',
    evidence_mismatch: 'The evidence no longer matches the source.',
    invalid_candidate: 'This suggestion is not valid for review.',
    consequential_effect: 'This action is not supported by metadata review.',
    unsupported: 'This action is not supported here. Read-only.',
  }[reason] || 'This suggestion could not be verified for review.'
}

export function reviewFailure(error) {
  const code = typeof error === 'string' ? error : error?.code
  if (['stale_source', 'stale_proposal', 'source_changed'].includes(code)) {
    return 'The source or current value changed. Nothing was applied; refresh and extract a fresh suggestion.'
  }
  if (['conflict', 'review_conflict', 'human_changed', 'already_resolved'].includes(code)) {
    return 'This suggestion or its current value changed. Nothing was applied by this request; refresh before reviewing again.'
  }
  if (['invalid_evidence', 'evidence_mismatch', 'evidence_missing'].includes(code)) {
    return 'The source evidence cannot be verified. Nothing was applied; extract a fresh suggestion.'
  }
  if (['hidden', 'reveal_required', 'sensitive_reveal_required'].includes(code)) {
    return 'The source is hidden. Open the document and use its existing reveal controls before reviewing.'
  }
  if (['forbidden', 'unauthorized', 'interactive_required', 'interactive_session_required'].includes(code) || [401, 403].includes(error?.status)) {
    return 'Review access is no longer available. Sign in with an authorized interactive session and refresh.'
  }
  if (['not_found', 'no_task', 'system_unavailable', 'source_unavailable'].includes(code) || error?.status === 404) {
    return 'This source or suggestion is no longer available to you. Refresh the review list.'
  }
  if (['unsupported', 'unknown_action', 'unknown_handler'].includes(code)) {
    return 'This action is not supported here. It remains read-only.'
  }
  if (error?.name === 'TypeError' || error?.status === 503) {
    return 'The review service could not be reached. The result is unconfirmed; reconnect and refresh before trying again.'
  }
  return 'The decision could not be confirmed. Refresh before trying again.'
}
