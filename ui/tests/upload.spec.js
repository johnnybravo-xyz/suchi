import { expect, test } from '@playwright/test'

const png = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl6zksAAAAASUVORK5CYII='
const file = name => ({ name, mimeType: 'image/png', buffer: Buffer.from(png, 'base64') })

async function mockUpload(page, options = {}) {
  const uploads = []
  await page.route('**/api/**', async route => {
    const request = route.request()
    const url = new URL(request.url())
    let body = { results: [], count: 0 }
    if (url.pathname === '/api/whoami') body = { user_id: 1, email: 'reader@example.test', role: 'member', capabilities: [] }
    else if (url.pathname === '/api/demo/mode') body = { enabled: false }
    else if (url.pathname === '/api/documents/' && request.method() === 'POST') {
      uploads.push(request)
      if (options.upload) return options.upload(route, uploads.length)
      body = { id: 41 }
    } else if (url.pathname === '/api/documents/' && request.method() === 'GET' && url.searchParams.has('split_origin_id')) {
      expect(url.searchParams.get('split_origin_id')).toBe('41')
      if (options.children) return options.children(route)
    } else if (url.pathname === '/api/tasks/') {
      expect(url.searchParams.get('include')).toBe('jobs')
      expect(url.searchParams.get('doc_id')).toBe('41')
      expect(url.searchParams.get('kind')).toBe('post-')
      if (options.tasks) return options.tasks(route)
    } else if (url.pathname === '/api/documents/41') {
      if (options.document) return options.document(route)
      body = { id: 41, title: 'Scanned receipt', content: options.content ?? 'Receipt total 42', tags: [] }
    }
    return route.fulfill({ json: body })
  })
  await page.addInitScript(() => {
    window.previewURLs = []
    window.revokedURLs = []
    const create = URL.createObjectURL.bind(URL)
    const revoke = URL.revokeObjectURL.bind(URL)
    URL.createObjectURL = blob => { const url = create(blob); window.previewURLs.push(url); return url }
    URL.revokeObjectURL = url => { window.revokedURLs.push(url); revoke(url) }
  })
  return uploads
}

async function paste(page, selector = '.drop', type = 'image/png') {
  return page.evaluate(({ selector, type, png }) => {
    const transfer = new DataTransfer()
    const bytes = Uint8Array.from(atob(png), character => character.charCodeAt(0))
    transfer.items.add(new File([bytes], 'screenshot.png', { type }))
    const event = new ClipboardEvent('paste', { clipboardData: transfer, bubbles: true, cancelable: true })
    document.querySelector(selector).dispatchEvent(event)
    return event.defaultPrevented
  }, { selector, type, png })
}

test('previews pasted images on the upload page and sends only after confirmation', async ({ page }) => {
  const uploads = await mockUpload(page)
  await page.goto('/#/upload')
  await expect(page.locator('.drop')).toBeVisible()
  expect(await paste(page)).toBe(true)
  const preview = page.getByRole('region', { name: 'Pasted images' })
  await expect(preview.getByRole('img', { name: 'Preview of screenshot.png' })).toBeVisible()
  expect(uploads).toHaveLength(0)
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await preview.getByRole('button', { name: 'Upload image', exact: true }).click()
  await expect(page.getByText('Ready', { exact: true })).toBeVisible()
  await expect(preview).toHaveCount(0)
  expect(uploads).toHaveLength(1)
  expect(uploads[0].postDataBuffer().includes(Buffer.from(png, 'base64'))).toBe(true)
  expect(await page.evaluate(() => window.revokedURLs)).toEqual(await page.evaluate(() => window.previewURLs))
})

test('discards pasted previews and releases their URLs on navigation', async ({ page }) => {
  const uploads = await mockUpload(page)
  await page.goto('/#/upload')
  await expect(page.locator('.drop')).toBeVisible()
  await paste(page)
  await page.getByRole('button', { name: 'Discard', exact: true }).click()
  await expect(page.getByRole('region', { name: 'Pasted images' })).toHaveCount(0)
  await paste(page)
  await page.evaluate(() => { location.hash = '#/dashboard' })
  await expect(page.getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible()
  expect(await page.evaluate(() => window.revokedURLs)).toEqual(await page.evaluate(() => window.previewURLs))
  expect(uploads).toHaveLength(0)
})

test('leaves pastes outside upload and inside text inputs untouched', async ({ page }) => {
  const uploads = await mockUpload(page)
  await page.goto('/#/upload')
  await expect(page.locator('.drop')).toBeVisible()
  expect(await paste(page, '.topbar input')).toBe(false)
  expect(await paste(page, '.drop', 'text/plain')).toBe(false)
  await page.evaluate(() => {
    const input = document.createElement('textarea')
    input.id = 'paste-editor'
    document.querySelector('.drop').parentElement.append(input)
  })
  expect(await paste(page, '#paste-editor')).toBe(false)
  await expect(page.getByRole('region', { name: 'Pasted images' })).toHaveCount(0)
  expect(uploads).toHaveLength(0)
})

test('shows every selected file before the first upload finishes and preserves order', async ({ page }) => {
  let firstUpload
  await mockUpload(page, { upload: (route, index) => {
    if (index === 1) {
      firstUpload = route
      return
    }
    return route.fulfill({ json: { id: 41 } })
  } })
  await page.goto('/#/upload')
  await page.locator('input[type=file]').setInputFiles(['first.png', 'second.png', 'third.png'].map(file))
  const rows = page.locator('.queue-row')
  await expect(rows).toHaveCount(3)
  await expect(rows.nth(0)).toContainText('first.png')
  await expect(rows.nth(0)).toContainText('Uploading')
  await expect(rows.nth(1)).toContainText('second.png')
  await expect(rows.nth(1)).toContainText('Waiting')
  await expect(rows.nth(2)).toContainText('third.png')
  await expect(rows.nth(2)).toContainText('Waiting')
  await expect(page.getByRole('status')).toHaveText('0 of 3 finished')
  await firstUpload.fulfill({ json: { id: 41 } })
  await expect(page.getByRole('status')).toContainText('3 uploaded')
})

test('a global file drop opens the full upload queue', async ({ page }) => {
  await mockUpload(page)
  await page.goto('/#/dashboard')
  await expect(page.getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Upload documents', exact: true })).toBeEnabled()
  await page.evaluate(({ png }) => {
    const transfer = new DataTransfer()
    const bytes = Uint8Array.from(atob(png), character => character.charCodeAt(0))
    transfer.items.add(new File([bytes], 'dropped.png', { type: 'image/png' }))
    window.dispatchEvent(new DragEvent('drop', { dataTransfer: transfer, bubbles: true, cancelable: true }))
  }, { png })
  await expect(page).toHaveURL(/#\/upload/)
  await expect(page.locator('.queue-row')).toContainText('dropped.png')
  await expect(page.getByText('Ready', { exact: true })).toBeVisible()
})

test('retries a failed upload with the same idempotency key', async ({ page }) => {
  const uploads = await mockUpload(page, { upload: (route, index) => index === 1
    ? route.fulfill({ status: 503, json: { error: 'Temporarily unavailable' } })
    : route.fulfill({ json: { id: 41 } }) })
  await page.goto('/#/upload')
  await page.locator('input[type=file]').setInputFiles(file('retry.png'))
  await expect(page.getByText('Upload failed', { exact: true })).toBeVisible()
  const firstKey = uploads[0].headers()['idempotency-key']
  expect(firstKey).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
  await page.getByRole('button', { name: 'Retry', exact: true }).click()
  await expect(page.getByText('Ready', { exact: true })).toBeVisible()
  expect(uploads[1].headers()['idempotency-key']).toBe(firstKey)
})

test('reports uploaded, duplicate, restored, and failed files distinctly', async ({ page }) => {
  await mockUpload(page, { upload: (route, index) => {
    if (index === 4) return route.fulfill({ status: 413, json: { error: 'Too large' } })
    return route.fulfill({ json: { id: 41, deduplicated: index === 2, restored: index === 3 } })
  } })
  await page.goto('/#/upload')
  await page.locator('input[type=file]').setInputFiles(['new.png', 'duplicate.png', 'restored.png', 'large.png'].map(file))
  await expect(page.getByText('Ready', { exact: true })).toBeVisible()
  await expect(page.getByText('Already in the archive · Ready', { exact: true })).toBeVisible()
  await expect(page.getByText('Restored · Ready', { exact: true })).toBeVisible()
  await expect(page.getByText('Choose a smaller file or ask the administrator to raise the upload limit.', { exact: true })).toBeVisible()
  await expect(page.getByRole('status')).toHaveText('3 uploaded · 3 ready · 1 failed')
  await expect(page.getByRole('button', { name: 'Retry', exact: true })).toHaveCount(0)
  await expect(page.getByRole('link', { name: 'Open document', exact: true })).toHaveCount(3)
})

test('waits for processing to finish before reading the document', async ({ page }) => {
  let jobs = [{ kind: 'post-ingest', state: 'running' }]
  let documentReads = 0
  await mockUpload(page, {
    tasks: route => route.fulfill({ json: { results: jobs } }),
    document: route => {
      documentReads++
      return route.fulfill({ json: { id: 41, title: 'Blank scan', content: '   ', tags: [] } })
    },
  })
  await page.clock.install()
  await page.goto('/#/upload')
  await page.locator('input[type=file]').setInputFiles(file('blank.png'))
  await expect(page.getByText('Uploaded · Processing', { exact: true })).toBeVisible()
  expect(documentReads).toBe(0)
  jobs = [{ kind: 'post-classify', state: 'pending' }]
  await page.clock.runFor(2500)
  await expect(page.getByText('Uploaded · Processing', { exact: true })).toBeVisible()
  expect(documentReads).toBe(0)
  jobs = []
  await page.clock.runFor(2500)
  await expect(page.getByText('Ready · No text found', { exact: true })).toBeVisible()
  expect(documentReads).toBe(1)
})

test('shows documents created from a split scan instead of linking the retired batch', async ({ page }) => {
  await mockUpload(page, {
    document: route => route.fulfill({ json: { id: 41, title: 'Feeder batch', trashed_at: 1, tags: [] } }),
    children: route => route.fulfill({ json: {
      count: 2,
      results: [
        { id: 43, title: 'Second document', split_index: 2, archive_serial_number: 4022 },
        { id: 42, title: 'First document', split_index: 1, archive_serial_number: 4021 },
      ],
    } }),
  })
  await page.goto('/#/upload')
  await page.locator('input[type=file]').setInputFiles(file('batch.png'))
  await expect(page.getByText('Split into 2 documents', { exact: true })).toBeVisible()
  const results = page.getByRole('group', { name: 'Documents created from this scan' })
  await expect(results.getByRole('link')).toHaveCount(2)
  await expect(results.getByRole('link').nth(0)).toContainText('ASN 4021')
  await expect(results.getByRole('link').nth(0)).toContainText('First document')
  await expect(results.getByRole('link').nth(1)).toContainText('ASN 4022')
  await expect(page.getByRole('link', { name: 'Open document', exact: true })).toHaveCount(0)
})

test('keeps processing honest after automatic checks pause and allows checking again', async ({ page }) => {
  let jobs = [{ kind: 'post-ingest', state: 'running' }]
  let reads = 0
  await mockUpload(page, { tasks: route => { reads++; return route.fulfill({ json: { results: jobs } }) } })
  await page.clock.install()
  await page.goto('/#/upload')
  await page.locator('input[type=file]').setInputFiles(file('slow.png'))
  await expect.poll(() => reads).toBe(1)
  for (let check = 2; check <= 40; check++) {
    await page.clock.runFor(2500)
    await expect.poll(() => reads).toBe(check)
  }
  await expect(page.getByText('Uploaded · Processing', { exact: true })).toBeVisible()
  await expect(page.getByText('Automatic checks paused. Processing continues on the server.', { exact: true })).toBeVisible()
  await expect(page.getByText('Ready', { exact: true })).toHaveCount(0)
  jobs = []
  await page.getByRole('button', { name: 'Check again', exact: true }).click()
  await expect(page.getByText('Ready', { exact: true })).toBeVisible()
})

test('shows unavailable and failed processing without claiming completion', async ({ page }) => {
  let unavailable = true
  await mockUpload(page, { tasks: route => unavailable
    ? route.fulfill({ status: 503, json: { error: 'Offline' } })
    : route.fulfill({ json: { results: [{ kind: 'post-ingest', state: 'dead' }] } }) })
  await page.clock.install()
  await page.goto('/#/upload')
  await page.locator('input[type=file]').setInputFiles(file('failed.png'))
  await expect(page.getByText('Could not check processing status', { exact: true })).toBeVisible()
  unavailable = false
  await page.clock.runFor(2500)
  await expect(page.getByText('Processing needs attention', { exact: true })).toBeVisible()
  await expect(page.getByText('Ready', { exact: true })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Check again', exact: true })).toBeVisible()
})

for (const [holder, message] of [
  [{ status: 200, json: { id: 7, title: 'Loan closure certificate' } },
    'ASN 4021 (page 2) is already used by #7 “Loan closure certificate”. Change the label and scan it again.'],
  [{ status: 404, json: { error: 'not found' } },
    'ASN 4021 (page 2) is already in use. Change the label and scan it again.'],
]) {
  test(`explains a rejected ASN scan on the upload row (holder ${holder.status})`, async ({ page }) => {
    await mockUpload(page, { tasks: route => route.fulfill({ json: { results: [{
      kind: 'post-ingest',
      state: 'dead',
      last_error: 'jobs: terminal error; do not retry: archive_serial_number_conflict page=2 asn=4021 doc=7',
    }] } }) })
    await page.route('**/api/documents/7', route => route.fulfill(holder))
    await page.goto('/#/upload')
    await page.locator('input[type=file]').setInputFiles(file('batch.png'))
    await expect(page.getByText('Processing needs attention', { exact: true })).toBeVisible()
    await expect(page.getByRole('alert')).toHaveText(message)
  })
}

test('stops polling and ignores late document details after leaving upload', async ({ page }) => {
  await mockUpload(page)
  let pending
  await page.route('**/api/documents/41', route => { pending = route })
  await page.clock.install()
  await page.goto('/#/upload')
  await page.locator('input[type=file]').setInputFiles(file('private.png'))
  await expect.poll(() => !!pending).toBe(true)
  await page.evaluate(() => { location.hash = '#/dashboard' })
  await expect(page.getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible()
  await pending.fulfill({ json: { id: 41, title: 'Late private title', content: 'Private text' } })
  await page.clock.runFor(5000)
  await expect(page.getByText('Late private title', { exact: false })).toHaveCount(0)
  await expect(page.getByText('Ready', { exact: true })).toHaveCount(0)
})

test('continues an accepted file batch after leaving upload without showing late receipts', async ({ page }) => {
  let firstUpload
  const uploads = await mockUpload(page, { upload: (route, index) => {
    if (index === 1) { firstUpload = route; return }
    return route.fulfill({ json: { id: 41, deduplicated: true } })
  } })
  await page.goto('/#/upload')
  await page.locator('input[type=file]').setInputFiles([file('first.png'), file('second.png')])
  await expect.poll(() => !!firstUpload).toBe(true)
  await page.evaluate(() => { location.hash = '#/dashboard' })
  await expect(page.getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible()
  await firstUpload.fulfill({ json: { id: 41, deduplicated: true } })
  await expect.poll(() => uploads.length).toBe(2)
  await expect(page.getByText('duplicate', { exact: true })).toHaveCount(0)
})
