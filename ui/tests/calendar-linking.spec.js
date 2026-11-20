// SPDX-License-Identifier: AGPL-3.0-or-later

import { expect, test } from '@playwright/test'

function isoDay(day) {
  const now = new Date()
  return `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}-${String(day).padStart(2, '0')}`
}

function dateEvent(id, day, title) {
  const date = isoDay(day)
  return {
    id, document_id: id, document_title: title, type: 'date', role: 'due', status: 'accepted',
    value: { date, precision: 'day' }, sort_value: date, reviewed_at: 1780000000,
  }
}

// Day 10 has one chip; day 12 has five, so two sit behind "more".
const events = [
  dateEvent(1, 10, 'Electricity bill'),
  ...[2, 3, 4, 5, 6].map(id => dateEvent(id, 12, `Statement ${id}`)),
]

test.beforeEach(async ({ page }, testInfo) => {
  test.skip(testInfo.project.name === 'mobile', 'hover linking needs a pointer')
  await page.route('**/api/**', route => {
    const path = new URL(route.request().url()).pathname
    let body = { results: [], count: 0 }
    if (path === '/api/demo/mode') body = { enabled: false }
    else if (path === '/api/whoami') body = { user_id: 1, email: 'admin@example.test', display_name: 'Admin', role: 'admin', capabilities: [] }
    else if (path === '/api/jd/systems') body = { introduced: false, default_system_code: '', results: [] }
    else if (path === '/api/admin/setup/state') body = { filing_tree_chosen: true }
    else if (path === '/api/intelligence/') body = { results: events, count: events.length }
    return route.fulfill({ json: body })
  })
  await page.goto('/#/calendar')
})

test('hovering a calendar chip highlights its agenda entry', async ({ page }) => {
  const agendaEntry = page.locator('.agenda-event', { hasText: 'Electricity bill' })
  await expect(agendaEntry).not.toHaveClass(/linked/)
  await page.locator('.day-events a', { hasText: 'Electricity bill' }).hover()
  await expect(agendaEntry).toHaveClass(/linked/)
  await page.mouse.move(0, 0)
  await expect(agendaEntry).not.toHaveClass(/linked/)
})

test('hovering an agenda entry highlights its chip, or its day when the chip is hidden', async ({ page }) => {
  await page.locator('.agenda-event', { hasText: 'Electricity bill' }).hover()
  await expect(page.locator('.day-events a', { hasText: 'Electricity bill' })).toHaveClass(/linked/)

  const hiddenDay = page.locator('.day', { has: page.locator('.day-more') })
  await expect(hiddenDay).not.toHaveClass(/linked/)
  await page.locator('.agenda-event', { hasText: 'Statement 6' }).hover()
  await expect(hiddenDay).toHaveClass(/linked/)
})
