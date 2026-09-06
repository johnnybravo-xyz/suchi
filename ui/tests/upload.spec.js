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
    } else if (url.pathname === '/api/tasks/') {
      expect(url.searchParams.get('include')).toBe('jobs')
      expect(url.searchParams.get('doc_id')).toBe('41')
      expect(url.searchParams.get('kind')).toBe('post-')
      if (options.tasks) return options.tasks(route)
    } else if (url.pathname === '/api/documents/41') {
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

for (const target of ['page', 'dialog']) {
  test(`previews pasted images in the upload ${target} and sends only after confirmation`, async ({ page }) => {
    const uploads = await mockUpload(page)
    await page.goto(target === 'page' ? '/#/upload' : '/#/dashboard')
    if (target === 'dialog') await page.getByRole('button', { name: 'Upload documents', exact: true }).click()
    await expect(page.locator('.drop')).toBeVisible()
    expect(await paste(page, target === 'dialog' ? '[role=dialog]' : '.drop')).toBe(true)
    const preview = page.getByRole('region', { name: 'Pasted images' })
    await expect(preview.getByRole('img', { name: 'Preview of screenshot.png' })).toBeVisible()
    expect(uploads).toHaveLength(0)
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    await preview.getByRole('button', { name: 'Upload image', exact: true }).click()
    await expect(page.getByText('Text extracted', { exact: true })).toBeVisible()
    await expect(preview).toHaveCount(0)
    expect(uploads).toHaveLength(1)
    expect(uploads[0].postDataBuffer().includes(Buffer.from(png, 'base64'))).toBe(true)
    expect(await page.evaluate(() => window.revokedURLs)).toEqual(await page.evaluate(() => window.previewURLs))
  })
}

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

test('reports uploaded, duplicate, restored, and failed files distinctly', async ({ page }) => {
  await mockUpload(page, { upload: (route, index) => {
    if (index === 4) return route.fulfill({ status: 413, json: { error: 'Too large' } })
    return route.fulfill({ json: { id: 41, deduplicated: index === 2, restored: index === 3 } })
  } })
  await page.goto('/#/upload')
  await page.locator('input[type=file]').setInputFiles(['new.png', 'duplicate.png', 'restored.png', 'large.png'].map(file))
  await expect(page.getByText('Uploaded', { exact: true })).toBeVisible()
  await expect(page.getByText('duplicate', { exact: true })).toBeVisible()
  await expect(page.getByText('Restored from Trash', { exact: true })).toBeVisible()
  await expect(page.getByText('Larger than the server allows', { exact: true })).toBeVisible()
  await expect(page.getByText('1 uploaded · 1 restored · 1 duplicate · 1 failed', { exact: true })).toBeVisible()
  await expect(page.getByRole('link', { name: 'Open', exact: true })).toHaveCount(3)
})

test('reports no text only after extraction and classification finish', async ({ page }) => {
  let jobs = [{ kind: 'post-ingest', state: 'running' }]
  await mockUpload(page, { content: '   ', tasks: route => route.fulfill({ json: { results: jobs } }) })
  await page.clock.install()
  await page.goto('/#/upload')
  await page.locator('input[type=file]').setInputFiles(file('blank.png'))
  await expect(page.getByText('Still processing…', { exact: true })).toBeVisible()
  await expect(page.getByText('No text found', { exact: true })).toHaveCount(0)
  jobs = [{ kind: 'post-classify', state: 'pending' }]
  await page.clock.runFor(2500)
  await expect(page.getByText('Still processing…', { exact: true })).toBeVisible()
  jobs = []
  await page.clock.runFor(2500)
  await expect(page.getByText('No text found', { exact: true })).toBeVisible()
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
  await expect(page.getByText('Still processing', { exact: true })).toBeVisible()
  await expect(page.getByText('Automatic checks paused. Server processing continues.', { exact: true })).toBeVisible()
  await expect(page.getByText('Text extracted', { exact: true })).toHaveCount(0)
  jobs = []
  await page.getByRole('button', { name: 'Check again', exact: true }).click()
  await expect(page.getByText('Text extracted', { exact: true })).toBeVisible()
})

test('shows unavailable and failed processing without claiming completion', async ({ page }) => {
  let unavailable = true
  await mockUpload(page, { tasks: route => unavailable
    ? route.fulfill({ status: 503, json: { error: 'Offline' } })
    : route.fulfill({ json: { results: [{ kind: 'post-ingest', state: 'dead' }] } }) })
  await page.clock.install()
  await page.goto('/#/upload')
  await page.locator('input[type=file]').setInputFiles(file('failed.png'))
  await expect(page.getByText('Processing status unavailable', { exact: true })).toBeVisible()
  unavailable = false
  await page.clock.runFor(2500)
  await expect(page.getByText('Processing needs attention', { exact: true })).toBeVisible()
  await expect(page.getByText('Text extracted', { exact: true })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Check again', exact: true })).toBeVisible()
})

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
  await expect(page.getByText('Text extracted', { exact: true })).toHaveCount(0)
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
