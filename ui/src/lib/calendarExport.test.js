import assert from 'node:assert/strict'
import test from 'node:test'
import { calendarDateFile, canExportCalendarDate } from './calendarExport.js'
import { DATE_ROLES } from './intelligence.js'

const reviewedDate = {
  id: 81, document_id: 28, document_title: 'Home insurance renewal',
  type: 'date', role: 'renewal', status: 'accepted', reviewed_at: 1780200000,
  value: { date: '2028-02-29', precision: 'day' },
  evidence_text: 'Private source quote is not included in the export.',
}
const now = new Date('2026-09-06T12:34:56.789Z')
const archiveURL = 'https://archive.example.test/suchi/#/calendar?document_ids=28'
const unfold = value => value.replace(/\r\n[ \t]/g, '')

test('exports one all-day reviewed fact with a private document link and UTC stamp', () => {
  const { filename, contents } = calendarDateFile(reviewedDate, archiveURL, now)
  assert.equal(filename, 'suchi-date-81-2028-02-29.ics')
  assert.deepEqual(unfold(contents).split('\r\n'), [
    'BEGIN:VCALENDAR',
    'VERSION:2.0',
    'PRODID:-//Suchi//Document dates//EN',
    'BEGIN:VEVENT',
    'UID:https://archive.example.test/suchi/#suchi-date-81-1780200000',
    'DTSTAMP:20260906T123456Z',
    'DTSTART;VALUE=DATE:20280229',
    'DTEND;VALUE=DATE:20280301',
    'SUMMARY:Renewal: Home insurance renewal',
    'DESCRIPTION:Reviewed document date from Suchi.\\nhttps://archive.example.test/suchi/#/doc/28',
    'URL:https://archive.example.test/suchi/#/doc/28',
    'CLASS:PRIVATE',
    'TRANSP:TRANSPARENT',
    'END:VEVENT',
    'END:VCALENDAR',
    '',
  ])
  assert.equal(contents.includes(reviewedDate.evidence_text), false)
})

test('exports every known role but never automatic, unresolved, malformed or partial dates', () => {
  for (const role of DATE_ROLES) assert.equal(canExportCalendarDate({ ...reviewedDate, role }), true)
  for (const changes of [
    { status: 'pending' }, { status: 'rejected' }, { reviewed_at: null }, { reviewed_at: undefined },
    { reviewed_at: '1780200000' }, { reviewed_at: -1 },
    { type: 'amount' }, { role: 'unknown' }, { id: 0 }, { id: 1.5 }, { document_id: -1 },
    { value: { date: '2028-02-01', precision: 'month' } },
    { value: { date: '2028-01-01', precision: 'year' } },
    { value: { date: '2028-02-29' } },
    { value: { precision: 'day' }, sort_value: '2028-02-29' },
    ...['2027-02-29', '2028-02-30', '2028-13-01', '2028-00-00', '2028-2-29', '0000-01-01', '9999-12-31', '2028-02-29\r\nBEGIN:VEVENT'].map(date => ({ value: { date, precision: 'day' } })),
  ]) {
    const event = { ...reviewedDate, ...changes }
    assert.equal(canExportCalendarDate(event), false, JSON.stringify(changes))
    assert.throws(() => calendarDateFile(event, archiveURL, now), /Only reviewed dates with an exact day/)
  }
})

test('computes exclusive day ends across leap, century, month and year boundaries', () => {
  for (const [date, expectedEnd] of [
    ['2028-02-28', '20280229'], ['2027-02-28', '20270301'],
    ['2000-02-28', '20000229'], ['1900-02-28', '19000301'],
    ['2026-04-30', '20260501'], ['2026-12-31', '20270101'],
    ['2026-03-08', '20260309'], ['2026-11-01', '20261102'], ['0099-12-31', '01000101'],
  ]) {
    const event = { ...reviewedDate, value: { date, precision: 'day' } }
    assert.ok(calendarDateFile(event, archiveURL, now).contents.includes(`DTEND;VALUE=DATE:${expectedEnd}\r\n`), date)
  }
})

test('escapes calendar text and folds UTF-8 lines without splitting characters', () => {
  const title = 'Invoice, policy; folder\\name\r\nBEGIN:VEVENT\rline\nnext\u0000 ' + 'नेपाल 😀 Versicherung '.repeat(12)
  const { contents } = calendarDateFile({ ...reviewedDate, document_title: title }, archiveURL, now)
  for (const line of contents.split('\r\n')) assert.ok(new TextEncoder().encode(line).length <= 75)
  assert.equal(contents.replaceAll('\r\n', '').includes('\n'), false)
  assert.equal(contents.replaceAll('\r\n', '').includes('\r'), false)
  assert.equal(new TextDecoder('utf-8', { fatal: true }).decode(new TextEncoder().encode(contents)), contents)
  const lines = unfold(contents).split('\r\n')
  assert.equal(lines.filter(line => line === 'BEGIN:VEVENT').length, 1)
  assert.ok(lines.includes('SUMMARY:Renewal: Invoice\\, policy\\; folder\\\\name\\nBEGIN:VEVENT\\nline\\nnext ' + 'नेपाल 😀 Versicherung '.repeat(12)))
})

test('keeps event identity stable but distinct across archives and facts, without credentials', () => {
  const uid = (event, url, time = now) => unfold(calendarDateFile(event, url, time).contents).split('\r\n').find(line => line.startsWith('UID:'))
  assert.equal(uid(reviewedDate, archiveURL), uid({ ...reviewedDate, document_title: 'Renamed' }, archiveURL, new Date('2027-01-01T00:00:00Z')))
  assert.notEqual(uid(reviewedDate, archiveURL), uid({ ...reviewedDate, id: 82 }, archiveURL))
  assert.notEqual(uid(reviewedDate, archiveURL), uid({ ...reviewedDate, reviewed_at: 1780200001 }, archiveURL))
  assert.notEqual(uid(reviewedDate, archiveURL), uid(reviewedDate, 'https://other.example.test/suchi/'))
  assert.notEqual(uid(reviewedDate, archiveURL), uid(reviewedDate, 'https://archive.example.test/other/'))
  const { contents } = calendarDateFile(reviewedDate, 'https://name:secret@archive.example.test/suchi/?token=private#/calendar', now)
  assert.equal(unfold(contents).includes('URL:https://archive.example.test/suchi/#/doc/28'), true)
  assert.equal(/name|secret|token=|private#/.test(contents), false)
  assert.throws(() => calendarDateFile(reviewedDate, 'javascript:alert(1)', now), /HTTP or HTTPS/)
})
