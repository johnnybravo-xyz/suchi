const defaultLocale = 'en'

const dictionaries = Object.freeze({
  en: Object.freeze({
    'common.cancel': 'Cancel',
    'common.loading': 'Loading...',
    'common.save': 'Save',
  }),
})

export function normalizeLocale(locale = defaultLocale) {
  const value = String(locale).split(',')[0].split(';')[0].trim()
    .replaceAll('_', '-').toLowerCase()
  return value && value !== '*' ? value : defaultLocale
}

export function lookup(locale, key, values = {}, catalogs = dictionaries) {
  const normalized = normalizeLocale(locale)
  const base = normalized.split('-')[0]
  const message = catalogs[normalized]?.[key]
    ?? catalogs[base]?.[key]
    ?? catalogs[defaultLocale]?.[key]
    ?? key

  return message.replace(/\{([a-zA-Z][\w]*)\}/g, (placeholder, name) => (
    Object.hasOwn(values, name) ? String(values[name]) : placeholder
  ))
}

export function translator(locale, catalogs = dictionaries) {
  return (key, values) => lookup(locale, key, values, catalogs)
}
