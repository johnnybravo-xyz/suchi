import { DATE_ROLES, intelligenceRoleLabel } from './intelligence.js'

function dayBounds(event) {
  if (event?.type !== 'date' || event.status !== 'accepted' ||
      !Number.isSafeInteger(event.reviewed_at) || event.reviewed_at <= 0 ||
      !DATE_ROLES.includes(event.role) || event.value?.precision !== 'day' ||
      !Number.isSafeInteger(event.id) || event.id <= 0 ||
      !Number.isSafeInteger(event.document_id) || event.document_id <= 0) return null
  const value = event.value.date
  if (!/^\d{4}-\d{2}-\d{2}$/.test(value) || value.startsWith('0000-')) return null
  const date = new Date(`${value}T00:00:00Z`)
  if (!Number.isFinite(date.getTime()) || date.toISOString().slice(0, 10) !== value) return null
  date.setUTCDate(date.getUTCDate() + 1)
  if (date.getUTCFullYear() > 9999) return null
  return { start: value.replaceAll('-', ''), end: date.toISOString().slice(0, 10).replaceAll('-', '') }
}

export function canExportCalendarDate(event) {
  return dayBounds(event) !== null
}

function escapeText(value) {
  return String(value).replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/g, '')
    .replaceAll('\\', '\\\\').replace(/\r\n|\r|\n/g, '\\n').replaceAll(';', '\\;').replaceAll(',', '\\,')
}

function foldLine(value) {
  const encoder = new TextEncoder()
  let folded = ''
  let width = 0
  // RFC 5545 limits physical lines to 75 UTF-8 octets, including continuation whitespace.
  for (const character of value) {
    const size = encoder.encode(character).length
    if (width + size > 75) {
      folded += '\r\n '
      width = 1
    }
    folded += character
    width += size
  }
  return folded
}

export function calendarDateFile(event, appURL, now = new Date()) {
  const bounds = dayBounds(event)
  if (!bounds) throw new Error('Only reviewed dates with an exact day can be exported.')
  const documentURL = new URL(appURL)
  if (!['http:', 'https:'].includes(documentURL.protocol)) throw new Error('Calendar export requires an HTTP or HTTPS document URL.')
  documentURL.username = ''
  documentURL.password = ''
  documentURL.search = ''
  documentURL.hash = `/doc/${event.document_id}`
  const title = event.document_title || `Document #${event.document_id}`
  const stamp = now.toISOString().replace(/[-:]/g, '').replace(/\.\d{3}Z$/, 'Z')
  const lines = [
    'BEGIN:VCALENDAR',
    'VERSION:2.0',
    'PRODID:-//Suchi//Document dates//EN',
    'BEGIN:VEVENT',
    `UID:${escapeText(`${documentURL.origin}${documentURL.pathname}#suchi-date-${event.id}-${event.reviewed_at}`)}`,
    `DTSTAMP:${stamp}`,
    `DTSTART;VALUE=DATE:${bounds.start}`,
    `DTEND;VALUE=DATE:${bounds.end}`,
    `SUMMARY:${escapeText(`${intelligenceRoleLabel(event.role)}: ${title}`)}`,
    `DESCRIPTION:${escapeText(`Reviewed document date from Suchi.\n${documentURL.href}`)}`,
    `URL:${documentURL.href}`,
    'CLASS:PRIVATE',
    'TRANSP:TRANSPARENT',
    'END:VEVENT',
    'END:VCALENDAR',
  ]
  return {
    filename: `suchi-date-${event.id}-${event.value.date}.ics`,
    contents: lines.map(foldLine).join('\r\n') + '\r\n',
  }
}
