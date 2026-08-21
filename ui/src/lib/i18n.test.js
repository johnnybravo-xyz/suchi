import assert from 'node:assert/strict'
import test from 'node:test'
import { lookup, normalizeLocale, translator } from './i18n.js'

const catalogs = {
  en: { greeting: 'Hello, {name}', save: 'Save' },
  de: { save: 'Speichern' },
  'de-ch': { greeting: 'Gruezi, {name}' },
}

test('normalizes browser locale tags', () => {
  assert.equal(normalizeLocale(' DE_ch '), 'de-ch')
  assert.equal(normalizeLocale(''), 'en')
  assert.equal(lookup('en', 'common.save'), 'Save')
})

test('falls back through exact, base, English, and key', () => {
  assert.equal(lookup('de-CH', 'greeting', { name: 'Ria' }, catalogs), 'Gruezi, Ria')
  assert.equal(lookup('de-AT', 'save', {}, catalogs), 'Speichern')
  assert.equal(lookup('fr', 'save', {}, catalogs), 'Save')
  assert.equal(lookup('fr', 'unknown', {}, catalogs), 'unknown')
})

test('interpolates known values and preserves missing placeholders', () => {
  const t = translator('en', catalogs)
  assert.equal(t('greeting', { name: 'Asha' }), 'Hello, Asha')
  assert.equal(t('greeting'), 'Hello, {name}')
})
