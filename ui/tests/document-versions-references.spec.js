// SPDX-License-Identifier: AGPL-3.0-or-later

import { expect, test } from '@playwright/test'

const replacementFile = {
  name: 'corrected.pdf',
  mimeType: 'application/pdf',
  buffer: Buffer.from('%PDF-corrected'),
}

function onePagePDF(text) {
  const content = `BT /F1 24 Tf 72 720 Td (${text.replace(/[()\\]/g, '\\$&')}) Tj ET`
  const objects = [
    '<< /Type /Catalog /Pages 2 0 R >>',
    '<< /Type /Pages /Kids [3 0 R] /Count 1 >>',
    '<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>',
    `<< /Length ${Buffer.byteLength(content)} >>\nstream\n${content}\nendstream`,
    '<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>',
  ]
  let body = '%PDF-1.4\n'
  const offsets = [0]
  for (let index = 0; index < objects.length; index++) {
    offsets.push(Buffer.byteLength(body))
    body += `${index + 1} 0 obj\n${objects[index]}\nendobj\n`
  }
  const xref = Buffer.byteLength(body)
  body += `xref\n0 ${objects.length + 1}\n0000000000 65535 f \n`
  body += offsets.slice(1).map(offset => `${String(offset).padStart(10, '0')} 00000 n \n`).join('')
  body += `trailer\n<< /Size ${objects.length + 1} /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`
  return Buffer.from(body)
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
    mime_type: 'text/html',
    jd_category_id: 1,
    created_at: 1780000000 + id,
    added_at: 1780000000 + id,
    updated_at: 1780000000 + id,
    sources: [],
    tags: [],
    correspondents: [],
    custom_fields: [],
    notes: [],
    ...overrides,
  }
}

async function installDetailAPI(page, handler, preview = { contentType: 'text/html', body: '<p>Preview</p>' }) {
  await page.route('**/preview/**', route => route.fulfill(preview))
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
    else if (path === '/api/rendered_layouts/') body = { results: [] }
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

test('renders PDFs with the bundled offline viewer', async ({ page }) => {
  const pdf = onePagePDF('Suchi PDF preview')
  await installDetailAPI(page, async ({ route, path }) => {
    if (path !== '/api/documents/77') return false
    await route.fulfill({ json: detail(77, { mime_type: 'application/pdf' }) })
    return true
  }, {
    contentType: 'application/pdf',
    body: pdf,
    headers: { 'Accept-Ranges': 'bytes' },
  })

  await page.goto('/#/doc/77')
  const preview = page.locator('.preview')
  await expect(preview.getByRole('toolbar', { name: 'PDF controls' })).toBeVisible()
  await expect(preview.getByText('Page 1 of 1', { exact: true })).toBeVisible()
  await expect(preview.locator('iframe')).toHaveCount(0)
  const canvas = preview.locator('canvas')
  await expect(canvas).toBeVisible()
  await expect.poll(() => canvas.evaluate(node => node.width)).toBeGreaterThan(0)
  await expect.poll(() => canvas.evaluate(node => node.height)).toBeGreaterThan(0)
})

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
        await route.fulfill({ status: 500, json: { code: 'db_write', error: 'server error' } })
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
    'No similar documents found.',
  )
  await expect(page.getByRole('tab', { name: 'Versions 1' })).toBeVisible()
  const history = await openVersions(page)
  await expect(history.locator('.version-notice')).toHaveCount(0)
  await expect(history.getByText('Latest', { exact: true })).toBeVisible()
  await expect(history.getByText('Viewing', { exact: true })).toBeVisible()
  await expect(history.getByText('Earlier', { exact: true })).toBeVisible()
  await history.locator('input[type=file]').setInputFiles(replacementFile)
  await expect(history.getByText('corrected.pdf', { exact: true })).toBeVisible()
  await history.getByRole('button', { name: 'Upload replacement', exact: true }).click()
  await expect(history.getByText('The server could not complete the request.', { exact: true })).toBeVisible()
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
test('offers link type setup and counts only other revisions', async ({ page }) => {
  await installDetailAPI(page, async ({ route, request, path }) => {
    if (path === '/api/documents/42/versions/' && request.method() === 'GET') {
      await route.fulfill({ json: {
        count: 1,
        head_id: 42,
        can_upload: true,
        results: [{ id: 42, title: 'Document 42', created_at: 1780000042, is_head: true }],
      } })
      return true
    }
    return false
  })

  await page.goto('/#/doc/42')
  const contextTabs = page.getByRole('tablist', { name: 'Document context' })
  for (const name of ['Linked documents', 'Versions', 'Notes']) {
    const tab = contextTabs.getByRole('tab', { name, exact: true })
    await expect(tab).toBeVisible()
    await expect(tab.locator('.pill')).toHaveCount(0)
  }
  await contextTabs.getByRole('tab', { name: 'Linked documents', exact: true }).click()
  const linksFrom = page.getByRole('region', { name: 'This document links to' })
  await expect(linksFrom.getByRole('button', { name: 'Add link', exact: true })).toHaveCount(0)
  const createLinkField = linksFrom.getByRole('button', { name: 'Create Document link field' })
  await expect(createLinkField).toBeVisible()
  await expect(createLinkField).toHaveAttribute('href', /metadata=custom_fields/)
})

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
        value: { id: targetID, title: targetID === 70 ? 'Original record' : 'Replacement target', system_code: 'S02', jd_address: targetID === 70 ? '' : `S02.13.${targetID}`, created_at: 1780000000, is_latest: targetID !== 70 },
      })
      await route.fulfill({ json: detail(42, { title: 'Amendment', custom_fields }) })
      return true
    }
    if (path === '/api/documents/42/similar') {
      await route.fulfill({ json: {
        method: 'fts',
        results: [{ id: 71, title: 'Related receipt', created_at: 1780000071 }],
      } })
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
  const similarDocuments = page.getByRole('tabpanel', { name: 'Similar documents' })
  await expect(similarDocuments.getByText('Related receipt', { exact: true })).toBeVisible()
  await expect(similarDocuments.getByText('fts', { exact: true })).toHaveCount(0)
  await expect(page.getByText('Amount', { exact: true })).toBeVisible()
  await expect(page.getByText('12.5', { exact: true })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Document references' })).toHaveCount(0)
  const linkedTab = page.getByRole('tab', { name: /^Linked documents/ })
  await expect(linkedTab).toContainText('2')
  await linkedTab.click()
  const linkedDocuments = page.getByRole('tabpanel', { name: /^Linked documents/ })
  const linksFrom = linkedDocuments.getByRole('region', { name: 'This document links to' })
  const linksTo = linkedDocuments.getByRole('region', { name: 'Documents linking to this version' })
  await expect(linksFrom.getByText('Original record', { exact: true })).toBeVisible()
  await expect(linksFrom.getByText('#70', { exact: true })).toHaveCount(0)
  await expect(linksFrom.getByText('Earlier version', { exact: true })).toBeVisible()
  await expect(linksFrom.getByText('Supporting record', { exact: true })).toHaveCount(0)
  await expect(linksTo.getByText('Follow-up note', { exact: true })).toBeVisible()
  await expect(linksTo.getByText('Linked here as “Previous agreement”', { exact: true })).toBeVisible()
  await expect(linksTo.getByText('Earlier version', { exact: true })).toBeVisible()
  await linkedTab.focus()
  await linkedTab.press('ArrowRight')
  await expect(page.getByRole('tab', { name: /^Versions/ })).toHaveAttribute('aria-selected', 'true')
  await linkedTab.click()

  await linksFrom.getByRole('button', { name: 'Change link', exact: true }).click()
  const input = linksFrom.getByLabel('Governing record document')
  await input.fill('https://remote.example/#/doc/77')
  await linksFrom.getByRole('button', { name: 'Search', exact: true }).click()
  await expect(linksFrom.getByText('Only links from this archive can be used.', { exact: true })).toBeVisible()
  expect(exactReads).toBe(0)

  const encodedTargetURL = encodeURIComponent(`${new URL(page.url()).origin}/#/doc/77?system=S02`)
  await input.fill(encodedTargetURL)
  await linksFrom.getByRole('button', { name: 'Search', exact: true }).click()
  await expect(linksFrom.getByRole('button', { name: /Replacement target/ })).toBeVisible()
  expect(exactReads).toBe(1)
  await linksFrom.getByRole('button', { name: 'Use selected document', exact: true }).click()
  await expect(linksFrom.getByText('Replacement target', { exact: true })).toBeVisible()
  expect(writes[0]).toEqual({ method: 'PUT', body: { value: 77 } })

  await linksFrom.getByRole('button', { name: 'Remove link', exact: true }).click()
  await expect(linksFrom.getByText('This document does not link to another document yet.', { exact: true })).toBeVisible()
  await expect(linkedTab).toContainText('1')
  await linksFrom.getByRole('button', { name: 'Add link', exact: true }).click()
  const linkType = linksFrom.getByLabel('Link type')
  await expect(linkType.getByRole('option', { name: 'Supporting record' })).toHaveCount(1)
  await linkType.selectOption('9')
  await expect(linksFrom.getByLabel('Governing record document')).toBeVisible()
  expect(writes[1].method).toBe('DELETE')
})

test('shows only populated typed metadata and keeps compact add and edit controls', async ({ page }) => {
  const writes = []
  const notes = [{
    id: 9, user_id: 1, author: 'Admin', note: 'Check the tariff',
    created_at: 1780000100, updated_at: 1780000100, can_edit: true,
  }]
  let invoiceAmount = 125
  await installDetailAPI(page, async ({ route, request, path }) => {
    if (path === '/api/documents/42' && request.method() === 'GET') {
      await route.fulfill({ json: detail(42, {
        title: 'Electricity bill',
        custom_fields: [
          {
            field_id: 11, name: 'Invoice amount', data_type: 'number',
            extra_data: {}, value: invoiceAmount, display_value: String(invoiceAmount),
          },
          {
            field_id: 13, name: 'invoice_seller', data_type: 'text', managed_by: 'zugferd',
            extra_data: {}, value: 'Example Energy', display_value: 'Example Energy',
          },
        ],
        notes,
      }) })
      return true
    }
    if (path === '/api/custom_fields/') {
      await route.fulfill({ json: { results: [
        { id: 11, name: 'Invoice amount', data_type: 'number', extra_data: '{}' },
        { id: 12, name: 'invoice_currency', data_type: 'text', extra_data: '{}', managed_by: 'zugferd' },
        { id: 13, name: 'invoice_seller', data_type: 'text', extra_data: '{}', managed_by: 'zugferd' },
        { id: 14, name: 'Warranty end', data_type: 'date', extra_data: '{}' },
      ] } })
      return true
    }
    if (path === '/api/documents/42/custom_fields/11' && request.method() === 'PUT') {
      const body = request.postDataJSON()
      writes.push({ kind: 'field', body })
      invoiceAmount = body.value
      await route.fulfill({ status: 204, body: '' })
      return true
    }
    if (path === '/api/documents/42/notes/' && request.method() === 'POST') {
      const body = request.postDataJSON()
      writes.push({ kind: 'note', body })
      await route.fulfill({ status: 201, json: {
        id: 10, user_id: 1, author: 'Admin', note: body.note,
        created_at: 1780000200, updated_at: 1780000200, can_edit: true,
      } })
      return true
    }
    return false
  })

  await page.goto('/#/doc/42')
  await expect(page.locator('dt').filter({ hasText: /^invoice_currency$/ })).toHaveCount(0)
  await expect(page.locator('dt').filter({ hasText: /^invoice_seller$/ })).toHaveCount(0)
  await expect(page.locator('dt').filter({ hasText: /^Invoice seller$/ })).toHaveCount(1)
  await expect(page.getByText('Not set', { exact: true })).toHaveCount(0)
  await expect(page.locator('dl.kv > dt').last()).toHaveText('Custom fields')
  const customFieldsRow = page.locator('dt').filter({ hasText: /^Custom fields$/ }).locator('xpath=following-sibling::dd[1]')
  await expect(customFieldsRow.getByRole('combobox', { name: 'Add custom field' })).toHaveCount(0)
  await customFieldsRow.getByRole('button', { name: 'Edit custom fields' }).click()
  const addField = customFieldsRow.getByRole('combobox', { name: 'Add custom field' })
  await expect(addField.getByRole('option', { name: 'Invoice currency' })).toHaveCount(0)
  await expect(addField.getByRole('option', { name: 'Warranty end' })).toHaveCount(1)
  await addField.selectOption('14')
  const warrantyRow = page.locator('dt').filter({ hasText: /^Warranty end$/ }).locator('xpath=following-sibling::dd[1]')
  await expect(warrantyRow.getByLabel('Warranty end')).toBeVisible()
  await warrantyRow.getByRole('button', { name: 'Cancel' }).click()
  await expect(customFieldsRow.getByRole('combobox', { name: 'Add custom field' })).toHaveCount(0)

  const fieldRow = page.locator('dt').filter({ hasText: /^Invoice amount$/ }).locator('xpath=following-sibling::dd[1]')
  await fieldRow.getByRole('button', { name: 'Edit' }).click()
  await fieldRow.getByLabel('Invoice amount').fill('250.5')
  await fieldRow.getByRole('button', { name: 'Save' }).click()
  await expect(fieldRow.getByText('250.5', { exact: true })).toBeVisible()

  await expect(page.getByRole('tab', { name: 'Notes 1' })).toBeVisible()
  const contextTabs = page.getByRole('tablist', { name: 'Document context' }).getByRole('tab')
  await expect(contextTabs.last()).toHaveAccessibleName('Notes 1')
  await page.getByRole('tab', { name: 'Notes 1' }).click()
  const notesCard = page.getByRole('region', { name: 'Document notes' })
  await expect(notesCard.getByText('Check the tariff', { exact: true })).toBeVisible()
  await notesCard.getByLabel('Add a note').fill('Call the utility')
  await notesCard.getByRole('button', { name: 'Add note' }).click()
  await expect(notesCard.getByText('Call the utility', { exact: true })).toBeVisible()
  await expect(page.getByRole('tab', { name: 'Notes 2' })).toBeVisible()

  await expect(page.getByRole('combobox', { name: 'Folder layout' })).toHaveCount(0)
  await expect(page.getByText('Folder layout', { exact: true })).toHaveCount(0)
  expect(writes).toEqual([
    { kind: 'field', body: { value: 250.5 } },
    { kind: 'note', body: { note: 'Call the utility' } },
  ])
  await expect(page.getByText('Previous archive number', { exact: true })).toHaveCount(0)
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
