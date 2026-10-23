// SPDX-License-Identifier: AGPL-3.0-or-later

import { localeState, t } from './i18n.svelte.js'

const formatterCache = new Map()

const managedFieldLabelKeys = Object.freeze({
  zugferd: Object.freeze({
    invoice_number: 'customFields.invoiceNumber',
    invoice_date: 'customFields.invoiceDate',
    invoice_currency: 'customFields.invoiceCurrency',
    invoice_total_gross: 'customFields.invoiceTotalGross',
    invoice_total_net: 'customFields.invoiceTotalNet',
    invoice_seller: 'customFields.invoiceSeller',
    invoice_seller_vat: 'customFields.invoiceSellerVat',
    invoice_buyer: 'customFields.invoiceBuyer',
  }),
})

export function customFieldLabel(field) {
  const key = managedFieldLabelKeys[field?.managed_by]?.[field?.name]
  return key ? t(key) : String(field?.name || '')
}

export function formatDate(value, options = { year: 'numeric', month: 'short', day: 'numeric' }) {
  const date = value instanceof Date ? value : new Date(value)
  if (Number.isNaN(date.getTime())) return t('common.invalidDate')
  return formatter(`date:${JSON.stringify(options)}`, () => new Intl.DateTimeFormat(localeState.locale, options)).format(date)
}

// "today", "tomorrow", "in 3 days", "in 2 weeks", "in 4 months" for a whole-day distance.
export function formatDaysAway(days) {
  const relative = formatter('relative', () => new Intl.RelativeTimeFormat(localeState.locale, { numeric: 'auto' }))
  if (Math.abs(days) < 14) return relative.format(days, 'day')
  if (Math.abs(days) < 60) return relative.format(Math.round(days / 7), 'week')
  return relative.format(Math.round(days / 30), 'month')
}

export function formatNumber(value, options = {}) {
  return formatter(`number:${JSON.stringify(options)}`, () => new Intl.NumberFormat(localeState.locale, options)).format(value)
}

export function compareLocalized(left, right) {
  return formatter('collator', () => new Intl.Collator(localeState.locale, { numeric: true, sensitivity: 'base' })).compare(left, right)
}

export function plural(key, count, values = {}) {
  const category = formatter('plural', () => new Intl.PluralRules(localeState.locale)).select(count)
  const messageKey = `${key}.${category}`
  const parameters = { ...values, count }
  const message = t(messageKey, parameters)
  return message === messageKey ? t(`${key}.other`, parameters) : message
}

export function formatList(values, options = {}) {
  return formatter(`list:${JSON.stringify(options)}`, () => new Intl.ListFormat(localeState.locale, options)).format(values)
}

function formatter(key, create) {
  const fullKey = `${localeState.locale}:${key}`
  if (!formatterCache.has(fullKey)) formatterCache.set(fullKey, create())
  return formatterCache.get(fullKey)
}

export function fmtDate(unix) {
  if (!unix) return ''
  return formatDate(new Date(unix * 1000))
}
export function fmtBytes(n) {
  if (!n && n !== 0) return ''
  const units = ['B', 'KB', 'MB', 'GB']
  let i = 0
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++ }
  return `${formatNumber(n, { maximumFractionDigits: n < 10 && i > 0 ? 1 : 0 })} ${units[i]}`
}

// Product and format names stay untranslated; generic words use catalog keys.
// Covers the formats core/mimeutil ingests, including its aliases.
const FORMAT_LABELS = Object.freeze({
  'application/pdf': 'PDF',
  'application/epub+zip': 'EPUB',
  'application/epub': 'EPUB',
  'application/msword': 'Word',
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document': 'Word',
  'application/vnd.ms-word.document.macroenabled.12': 'Word',
  'application/vnd.ms-powerpoint': 'PowerPoint',
  'application/vnd.openxmlformats-officedocument.presentationml.presentation': 'PowerPoint',
  'application/vnd.ms-powerpoint.presentation.macroenabled.12': 'PowerPoint',
  'application/vnd.openxmlformats-officedocument.presentationml.slideshow': 'PowerPoint',
  'application/vnd.ms-powerpoint.slideshow.macroenabled.12': 'PowerPoint',
  'application/vnd.ms-excel': 'Excel',
  'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet': 'Excel',
  'application/vnd.ms-excel.sheet.macroenabled.12': 'Excel',
  'application/vnd.ms-excel.sheet.binary.macroenabled.12': 'Excel',
  'application/vnd.oasis.opendocument.text': 'OpenDocument Text',
  'application/vnd.oasis.opendocument.spreadsheet': 'OpenDocument Spreadsheet',
  'application/vnd.oasis.opendocument.presentation': 'OpenDocument Presentation',
  'application/rtf': 'RTF',
  'text/rtf': 'RTF',
  'text/csv': 'CSV',
  'text/markdown': 'Markdown',
  'text/html': 'HTML',
  'image/jpeg': 'JPEG',
  'image/jpg': 'JPEG',
  'image/pjpeg': 'JPEG',
  'image/png': 'PNG',
  'image/tiff': 'TIFF',
  'image/x-tiff': 'TIFF',
  'image/heic': 'HEIC',
  'image/heic-sequence': 'HEIC',
  'image/heif': 'HEIF',
  'image/heif-sequence': 'HEIF',
  'image/webp': 'WebP',
  'image/gif': 'GIF',
  'image/bmp': 'BMP',
  'image/x-bmp': 'BMP',
  'image/x-ms-bmp': 'BMP',
  'image/avif': 'AVIF',
  'image/svg+xml': 'SVG',
  'image/vnd.djvu': 'DjVu',
  'image/x-djvu': 'DjVu',
  'image/djvu': 'DjVu',
})

const FORMAT_LABEL_KEYS = Object.freeze({
  'message/rfc822': 'format.email',
  'message/global': 'format.email',
  'application/vnd.ms-outlook': 'format.outlookMessage',
  'application/x-outlook-msg': 'format.outlookMessage',
  'application/ms-outlook': 'format.outlookMessage',
  'text/plain': 'format.plainText',
})

// formatLabel names a MIME type for people. Unknown types fall back to their
// top-level family, then to the raw MIME, which is never wrong.
export function formatLabel(mime) {
  const normalized = String(mime || '').split(';')[0].trim().toLowerCase()
  if (!normalized) return ''
  if (FORMAT_LABELS[normalized]) return FORMAT_LABELS[normalized]
  if (FORMAT_LABEL_KEYS[normalized]) return t(FORMAT_LABEL_KEYS[normalized])
  if (normalized.startsWith('image/')) return t('format.image')
  if (normalized.startsWith('text/')) return t('format.text')
  return normalized
}

export const SENSITIVITY_OPTIONS = Object.freeze([
  Object.freeze({ value: 'public', get label() { return t('sensitivity.public') } }),
  Object.freeze({ value: 'internal', get label() { return t('sensitivity.internal') } }),
  Object.freeze({ value: 'confidential', get label() { return t('sensitivity.confidential') } }),
  Object.freeze({ value: 'restricted', get label() { return t('sensitivity.restricted') } }),
])

export function isHighSensitivity(value) {
  return value === 'confidential' || value === 'restricted'
}

export function sensitivityLabel(value) {
  return SENSITIVITY_OPTIONS.find((option) => option.value === value)?.label || t('common.unset')
}

export function sensDot(s) {
  return { confidential: 'danger', restricted: 'danger', internal: 'warn', public: 'ok' }[s] || ''
}
