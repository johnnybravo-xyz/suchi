// SPDX-License-Identifier: AGPL-3.0-or-later

import { expect, test } from '@playwright/test'

const replacementFile = {
  name: 'corrected.pdf',
  mimeType: 'application/pdf',
  buffer: Buffer.from('%PDF-corrected'),
}

function detail(id, overrides = {}) {
  return {
    id,
    owner_id: 1,
    title: `Document ${id}`,
    content: '',
    content_source: 'server',
    original_blob: `blob-${id}`,
    original_size: 2048,
    mime_type: 'application/pdf',
    jd_category_id: 1,
    created_at: 1780000000 + id,
    added_at: 1780000000 + id,
    updated_at: 1780000000 + id,
    sources: [],
    tags: [],
    correspondents: [],
    custom_fields: [],
    ...overrides,
  }
}

async function installDetailAPI(page, handler) {
  await page.route('**/preview/**', route => route.fulfill({ contentType: 'text/html', body: '<p>Preview</p>' }))
  await page.route('**/api/**', async route => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    if (handler && await handler({ route, request, url, path })) return
    let body = { results: [], count: 0 }
    if (path === '/api/demo/mode') body = { enabled: false }
    else if (path === '/api/whoami') body = { user_id: 1, email: 'admin@example.test', display_name: 'Admin', role: 'admin', capabilities: [] }
    else if (path === '/api/jd/systems') body = { introduced: false, default_system_code: '', results: [] }
    else if (path === '/api/jd/categories/') body = { results: [] }
    else if (path === '/api/admin/setup/state') body = { filing_tree_chosen: true }
    else if (/^\/api\/acls\/document\/\d+$/.test(path)) body = { results: [], principals: [] }
    else if (/^\/api\/documents\/\d+\/similar$/.test(path)) body = { results: [] }
    else if (path === '/api/custom_fields/') body = { results: [] }
    else if (/^\/api\/documents\/\d+\/referenced-by\/$/.test(path)) body = { results: [], count: 0 }
    else if (/^\/api\/documents\/\d+\/versions\/$/.test(path)) body = { results: [], count: 0, head_id: null, can_upload: false }
    else if (/^\/api\/documents\/\d+$/.test(path)) body = detail(Number(path.split('/').at(-1)))
    return route.fulfill({ json: body })
  })
}

async function openVersions(page) {
  await page.getByRole('tab', { name: /^Versions/ }).click()
  return page.getByRole('tabpanel', { name: /^Versions/ })
}

test('shows explicit revision state and retries an uncertain replacement with one key', async ({ page }) => {
  const keys = []
  let attempts = 0
  await installDetailAPI(page, async ({ route, request, path }) => {
    if (path === '/api/documents/42') {
      await route.fulfill({ json: detail(42, { title: 'Viewed revision' }) })
      return true
    }
    if (path === '/api/documents/44') {
      await route.fulfill({ json: detail(44, { title: 'Uploaded revision' }) })
      return true
    }
    if (path === '/api/documents/42/versions/' && request.method() === 'GET') {
      await route.fulfill({ json: {
        count: 2,
        head_id: 42,
        can_upload: true,
        results: [
          { id: 42, title: 'Viewed revision', created_at: 1780000042, is_head: true },
          { id: 41, title: 'Earlier revision', created_at: 1780000041, is_head: false },
        ],
      } })
      return true
    }
    if (path === '/api/documents/42/versions/' && request.method() === 'POST') {
      keys.push(request.headers()['idempotency-key'])
      attempts++
      if (attempts === 1) {
        await route.fulfill({ status: 500, json: { code: 'db_write', error: 'Upload response was interrupted' } })
      } else {
        await route.fulfill({ status: 201, json: { id: 44 } })
      }
      return true
    }
    if (path === '/api/documents/44/versions/' && request.method() === 'GET') {
      await route.fulfill({ json: {
        count: 3, head_id: 44, can_upload: true,
        results: [{ id: 44, title: 'Uploaded revision', created_at: 1780000044, is_head: true }],
      } })
      return true
    }
    return false
  })

  await page.goto('/#/doc/42')
  const similarTab = page.getByRole('tab', { name: /^Similar documents/ })
  await expect(similarTab).toHaveAttribute('aria-selected', 'true')
  await expect(page.getByRole('tabpanel', { name: /^Similar documents/ })).toContainText(
    "Nothing in the archive overlaps this document's vocabulary yet.",
  )
  await expect(page.getByRole('tab', { name: 'Versions 2' })).toBeVisible()
  const history = await openVersions(page)
  await expect(history.locator('.version-notice')).toHaveCount(0)
  await expect(history.getByText('Latest', { exact: true })).toBeVisible()
  await expect(history.getByText('Viewing', { exact: true })).toBeVisible()
  await expect(history.getByText('Earlier', { exact: true })).toBeVisible()
  await history.locator('input[type=file]').setInputFiles(replacementFile)
  await expect(history.getByText('corrected.pdf', { exact: true })).toBeVisible()
  await history.getByRole('button', { name: 'Upload replacement', exact: true }).click()
  await expect(history.getByText('Upload response was interrupted', { exact: true })).toBeVisible()
  await expect(history.getByText('corrected.pdf', { exact: true })).toBeVisible()
  await history.getByRole('button', { name: 'Retry upload', exact: true }).click()
  await expect(page).toHaveURL(/#\/doc\/44/)
  await expect(page.getByText('Uploaded revision', { exact: true }).first()).toBeVisible()
  expect(keys).toHaveLength(2)
  expect(keys[0]).toBeTruthy()
  expect(keys[1]).toBe(keys[0])
})

for (const failure of [
  {
    name: 'duplicate conflict',
    status: 409,
    body: { code: 'duplicate_version_blob', error: 'uploaded bytes already belong to a live document', existing_id: 77 },
    message: 'These bytes already belong to live document #77. Choose a different file.',
  },
  {
    name: 'version head conflict',
    status: 409,
    body: {
      code: 'version_head_changed',
      error: 'the document version head changed; reload and upload from the latest revision',
    },
    message: 'the document version head changed; reload and upload from the latest revision',
  },
  {
    name: 'permission loss',
    status: 403,
    body: { code: 'forbidden', error: 'forbidden' },
    message: 'You no longer have permission to upload a replacement.',
  },
  {
    name: 'oversized upload',
    status: 413,
    body: { code: 'too_large', error: 'too large' },
    message: 'This file is larger than the configured upload limit.',
  },
]) {
  test(`keeps the selected replacement after ${failure.name}`, async ({ page }) => {
    await installDetailAPI(page, async ({ route, request, path }) => {
      if (path === '/api/documents/42') {
        await route.fulfill({ json: detail(42) })
        return true
      }
      if (path === '/api/documents/42/versions/' && request.method() === 'GET') {
        await route.fulfill({ json: {
          count: 1, head_id: 42, can_upload: true,
          results: [{ id: 42, title: 'Document 42', created_at: 1780000042, is_head: true }],
        } })
        return true
      }
      if (path === '/api/documents/42/versions/' && request.method() === 'POST') {
        await route.fulfill({ status: failure.status, json: failure.body })
        return true
      }
      return false
    })

    await page.goto('/#/doc/42')
    const history = await openVersions(page)
    await history.locator('input[type=file]').setInputFiles(replacementFile)
    await history.getByRole('button', { name: 'Upload replacement', exact: true }).click()
    await expect(history.getByText(failure.message, { exact: true })).toBeVisible()
    await expect(history.getByText('corrected.pdf', { exact: true })).toBeVisible()
    await expect(history.getByRole('button', { name: 'Retry upload', exact: true })).toBeVisible()
  })
}
test('keeps outgoing and incoming document links in one compact tab', async ({ page }) => {
  let targetID = 70

  const writes = []
  let exactReads = 0
  await installDetailAPI(page, async ({ route, request, path }) => {
    if (path === '/api/documents/42' && request.method() === 'GET') {
      const custom_fields = [
        { field_id: 4, name: 'Amount', data_type: 'number', value: 12.5 },
      ]
      if (targetID) custom_fields.push({
        field_id: 9, name: 'Governing record', data_type: 'documentlink',
        value: { id: targetID, title: targetID === 70 ? 'Original record' : 'Replacement target', system_code: 'S02', jd_address: `S02.13.${targetID}`, created_at: 1780000000, is_latest: targetID !== 70 },
      })
      await route.fulfill({ json: detail(42, { title: 'Amendment', custom_fields }) })
      return true
    }
    if (path === '/api/documents/77' && request.method() === 'GET') {
      exactReads++
      await route.fulfill({ json: detail(77, { title: 'Replacement target', system_code: 'S02', jd_address: 'S02.13.77' }) })
      return true
    }
    if (path === '/api/custom_fields/') {
      await route.fulfill({ json: { results: [
        { id: 9, name: 'Governing record', data_type: 'documentlink' },
        { id: 10, name: 'Supporting record', data_type: 'documentlink' },
        { id: 11, name: 'Superseded by', data_type: 'documentlink' },
      ] } })
      return true
    }
    if (path === '/api/documents/42/referenced-by/') {
      await route.fulfill({ json: {
        count: 1,
        results: [{ id: 55, title: 'Follow-up note', system_code: 'S01', jd_address: 'S01.13.55', created_at: 1780000055, is_latest: false, field_id: 12, field_name: 'Previous agreement' }],
      } })
      return true
    }
    if (path === '/api/documents/42/custom_fields/9') {
      writes.push({ method: request.method(), body: request.postDataJSON?.() })
      if (request.method() === 'PUT') targetID = request.postDataJSON().value
      else targetID = 0
      await route.fulfill({ status: 204 })
      return true
    }
    return false
  })

  await page.goto('/#/doc/42')
  await expect(page.getByText('Amount', { exact: true })).toBeVisible()
  await expect(page.getByText('12.5', { exact: true })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Document references' })).toHaveCount(0)
  const linkedTab = page.getByRole('tab', { name: /^Linked documents/ })
  await expect(linkedTab).toContainText('2')
  await linkedTab.click()
  const linkedDocuments = page.getByRole('tabpanel', { name: /^Linked documents/ })
  const linksFrom = linkedDocuments.getByRole('region', { name: 'Links from this document' })
  const linksTo = linkedDocuments.getByRole('region', { name: 'Links to this document' })
  await expect(linksFrom.getByText('Original record', { exact: true })).toBeVisible()
  await expect(linksFrom.getByText('Earlier revision', { exact: true })).toBeVisible()
  await expect(linksFrom.getByText('Supporting record', { exact: true })).toHaveCount(0)
  await expect(linksTo.getByText('Follow-up note', { exact: true })).toBeVisible()
  await expect(linksTo.getByText('Earlier revision', { exact: true })).toBeVisible()
  await linkedTab.focus()
  await linkedTab.press('ArrowRight')
  await expect(page.getByRole('tab', { name: /^Versions/ })).toHaveAttribute('aria-selected', 'true')
  await linkedTab.click()

  await linksFrom.getByRole('button', { name: 'Change', exact: true }).click()
  const input = linksFrom.getByLabel('Governing record document')
  await input.fill('https://remote.example/#/doc/77')
  await linksFrom.getByRole('button', { name: 'Search', exact: true }).click()
  await expect(linksFrom.getByText('Only links from this archive can be used.', { exact: true })).toBeVisible()
  expect(exactReads).toBe(0)

  await input.fill('77')
  await linksFrom.getByRole('button', { name: 'Search', exact: true }).click()
  await expect(linksFrom.getByRole('button', { name: /Replacement target/ })).toBeVisible()
  await linksFrom.getByRole('button', { name: 'Use selected document', exact: true }).click()
  await expect(linksFrom.getByText('Replacement target', { exact: true })).toBeVisible()
  expect(writes[0]).toEqual({ method: 'PUT', body: { value: 77 } })

  await linksFrom.getByRole('button', { name: 'Clear', exact: true }).click()
  await expect(linksFrom.getByText('No links from this document.', { exact: true })).toBeVisible()
  await expect(linkedTab).toContainText('1')
  await linksFrom.getByRole('button', { name: 'Add link', exact: true }).click()
  const linkType = linksFrom.getByLabel('Link type')
  await expect(linkType.getByRole('option', { name: 'Supporting record' })).toHaveCount(1)
  await linkType.selectOption('9')
  await expect(linksFrom.getByLabel('Governing record document')).toBeVisible()
  expect(writes[1].method).toBe('DELETE')
})

test('ignores late history and backlink responses after exact-route navigation', async ({ page }) => {
  let oldVersions
  let oldBacklinks
  await installDetailAPI(page, async ({ route, request, path }) => {
    if (path === '/api/documents/42') {
      await route.fulfill({ json: detail(42, { title: 'First route' }) })
      return true
    }
    if (path === '/api/documents/43') {
      await route.fulfill({ json: detail(43, { title: 'Second route' }) })
      return true
    }
    if (path === '/api/documents/42/versions/' && request.method() === 'GET') {
      oldVersions = route
      return true
    }
    if (path === '/api/documents/42/referenced-by/') {
      oldBacklinks = route
      return true
    }
    return false
  })

  await page.goto('/#/doc/42')
  await expect(page.getByText('First route', { exact: true }).first()).toBeVisible()
  await expect.poll(() => Boolean(oldVersions && oldBacklinks)).toBe(true)
  await page.evaluate(() => { location.hash = '#/doc/43' })
  await expect(page.getByText('Second route', { exact: true }).first()).toBeVisible()
  await oldVersions.fulfill({ json: { count: 1, head_id: 99, can_upload: false, results: [{ id: 99, title: 'Late private revision', created_at: 1, is_head: true }] } })
  await oldBacklinks.fulfill({ json: { count: 1, results: [{ id: 98, title: 'Late private backlink', field_id: 1, field_name: 'Secret', is_latest: true }] } })
  await page.waitForTimeout(50)
  await expect(page.getByText('Late private revision', { exact: true })).toHaveCount(0)
  await expect(page.getByText('Late private backlink', { exact: true })).toHaveCount(0)
})
