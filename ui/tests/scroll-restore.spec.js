// SPDX-License-Identifier: AGPL-3.0-or-later

import { expect, test } from '@playwright/test'

function doc(id) {
  return {
    id, owner_id: 1, title: `Document ${id}`, content: '', original_blob: `blob-${id}`,
    original_size: 2048, mime_type: 'application/pdf', jd_category_id: 1,
    created_at: 1780000000 + id, added_at: 1780000000 + id, updated_at: 1780000000 + id,
    sources: [], tags: [], correspondents: [], custom_fields: [], notes: [],
  }
}

test('returning from a document restores the list scroll position', async ({ page }) => {
  const docs = Array.from({ length: 50 }, (_, index) => doc(index + 1))
  await page.route('**/preview/**', route => route.fulfill({ contentType: 'text/html', body: '<p>Preview</p>' }))
  await page.route('**/api/**', async route => {
    const path = new URL(route.request().url()).pathname
    let body = { results: [], count: 0 }
    if (path === '/api/demo/mode') body = { enabled: false }
    else if (path === '/api/whoami') body = { user_id: 1, email: 'admin@example.test', display_name: 'Admin', role: 'admin', capabilities: [] }
    else if (path === '/api/jd/systems') body = { introduced: false, default_system_code: '', results: [] }
    else if (path === '/api/admin/setup/state') body = { filing_tree_chosen: true }
    else if (path === '/api/documents/') {
      // Slow enough that the list is empty when Back first renders it.
      await new Promise(resolve => setTimeout(resolve, 300))
      body = { results: docs, count: docs.length }
    } else if (/^\/api\/acls\/document\/\d+$/.test(path)) body = { results: [], principals: [] }
    else if (/^\/api\/documents\/\d+\/versions\/$/.test(path)) body = { results: [], count: 0, head_id: null, can_upload: false }
    else if (/^\/api\/documents\/\d+$/.test(path)) body = doc(Number(path.split('/').at(-1)))
    return route.fulfill({ json: body })
  })

  await page.goto('/#/documents')
  const content = page.locator('.content')
  const target = page.getByRole('link', { name: 'Document 45', exact: true })
  await target.scrollIntoViewIfNeeded()
  const before = await content.evaluate(node => node.scrollTop)
  expect(before).toBeGreaterThan(200)

  await target.click()
  await expect(page).toHaveURL(/#\/doc\/45$/)
  await expect(content).toHaveJSProperty('scrollTop', 0)

  await page.goBack()
  await expect(page.getByRole('link', { name: 'Document 45', exact: true })).toBeVisible()
  await expect.poll(() => content.evaluate(node => node.scrollTop)).toBe(before)
})
