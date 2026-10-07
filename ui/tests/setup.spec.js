import { expect, test } from '@playwright/test'

test('shows password-unlocked status in lists, grids and detail', async ({ page }) => {
  await mockAPI(page, {
    documents: [
      { id: 42, title: 'Unlocked statement', encryption_state: 'decrypted' },
      { id: 43, title: 'Ordinary document' },
      { id: 44, title: 'Locked document', encryption_state: 'encrypted' },
    ],
    documentDetails: { 42: { document: { encryption_state: 'decrypted' } } },
  })
  await page.goto('/#/documents')
  const status = page.getByRole('img', { name: 'Password unlocked', exact: true })
  await expect(status).toHaveCount(1)
  await expect(status).toHaveAttribute('title', /original remains password-protected/)
  await page.getByRole('button', { name: 'Grid', exact: true }).click()
  await expect(status).toHaveCount(1)
  await page.getByRole('link').filter({ hasText: 'Unlocked statement' }).click()
  await expect(status).toBeVisible()
  await expect(page.getByText('Password unlocked', { exact: true })).toBeVisible()
})

test('keeps the document refresh control aligned without duplicating Trash navigation', async ({ page }) => {
  await mockAPI(page, { filingTreeChosen: true })
  await page.goto('/#/documents')
  const toolbar = page.locator('.document-toolbar')
  await expect(toolbar.getByRole('link', { name: 'Open trash' })).toHaveCount(0)
  const refresh = await toolbar.getByRole('button', { name: 'Refresh documents' }).boundingBox()
  const viewToggle = await toolbar.locator('.seg').filter({ hasText: 'List' }).boundingBox()
  expect(Math.abs(refresh.height - viewToggle.height)).toBeLessThanOrEqual(1)
})

test('clears account data and rejects late reads after signing in as another user', async ({ page }) => {
  await mockAPI(page, {
    documentsCount: 731,

    filingTreeChosen: true,
    jdCategories: [{ id: 4, code: 11, name: 'Private estate plan', area_code: 10, area_name: 'Private affairs', system: false }],
  })
  let actor = 1
  let oldRead
  let oldIdentity
  let identityReads = 0
  await page.route('**/api/**', async route => {
    const path = new URL(route.request().url()).pathname
    if (path === '/api/logout') {
      actor = 0
      return route.fulfill({ status: 204 })
    }
    if (path === '/api/login') {
      actor = 2
      return route.fulfill({ json: { ok: true } })
    }
    if (path === '/api/whoami') {
      if (actor === 1 && ++identityReads > 1) {
        oldIdentity = route
        return
      }
      return route.fulfill({ json: { user_id: actor, email: 'second@example.test', display_name: 'Second user', role: 'member', capabilities: [] } })
    }
    if (path === '/api/documents/' && actor === 1) {
      oldRead = route
      return
    }
    if (actor === 2 && ['/api/documents/', '/api/jd/categories/', '/api/stats/'].includes(path)) {
      return route.fulfill({ status: 503, json: { error: path === '/api/documents/' ? 'Second account documents unavailable' : 'Second account metadata unavailable' } })
    }
    return route.fallback()
  })
  await page.goto('/#/dashboard')
  await expect(page.getByText('Private affairs', { exact: true })).toHaveCount(1)
  await expect(page.getByText('731', { exact: true })).toBeVisible()
  await expect.poll(() => !!oldRead).toBe(true)
  const navigation = page.getByRole('button', { name: 'Open navigation', exact: true })
  if (await navigation.isVisible()) await navigation.click()
  await page.getByRole('link', { name: 'Profile & settings', exact: true }).click()
  await page.getByRole('button', { name: 'Save profile', exact: true }).click()
  await expect.poll(() => !!oldIdentity).toBe(true)
  if (await navigation.isVisible()) await navigation.click()
  await page.getByRole('button', { name: 'Sign out', exact: true }).click()
  const loginForm = page.locator('form').filter({ has: page.getByRole('button', { name: 'Sign in', exact: true }) })
  await loginForm.getByLabel('Email', { exact: true }).fill('second@example.test')
  await loginForm.getByLabel('Password', { exact: true }).fill('test-password')
  await loginForm.getByRole('button', { name: 'Sign in', exact: true }).click()
  await expect(page.getByText('Second account documents unavailable', { exact: true })).toBeVisible()
  const finished = page.waitForEvent('requestfinished', request => request === oldRead.request())
  await oldRead.fulfill({ json: { count: 1, results: [{ id: 41, title: 'First account confidential title', created_at: 1780000000 }] } })
  await finished
  const identityFinished = page.waitForEvent('requestfinished', request => request === oldIdentity.request())
  await oldIdentity.fulfill({ json: { user_id: 1, email: 'first@example.test', display_name: 'First user', role: 'admin' } })
  await identityFinished
  await page.waitForTimeout(50)
  await expect(page.getByText('First user', { exact: true })).toHaveCount(0)
  await expect(page.getByText('First account confidential title', { exact: true })).toHaveCount(0)
  await expect(page.getByText('Private affairs', { exact: true })).toHaveCount(0)
  await expect(page.getByText('731', { exact: true })).toHaveCount(0)
  await expect(page.getByText('Second account documents unavailable', { exact: true })).toBeVisible()
})

for (const switchAccount of [false, true]) {
  test(`demo upgrade ${switchAccount ? 'cannot restore a signed-out session' : 'retries the original visitor upload'}`, async ({ page }) => {
    await mockAPI(page, { demoMode: true, demoSession: 'anon' })
    await page.addInitScript(() => {
      sessionStorage.setItem('suchi.demo.landed', '1')
    })
    let signedIn = false
    let upgrade
    const uploadCredentials = []
    await page.route('**/api/**', async route => {
      const request = route.request()
      const path = new URL(request.url()).pathname
      if (path === '/api/logout') return route.fulfill({ status: 204 })
      if (path === '/api/login') {
        signedIn = true
        return route.fulfill({ json: { ok: true } })
      }
      if (path === '/api/whoami' && signedIn) {
        return route.fulfill({ json: { user_id: 2, email: 'second@example.test', role: 'member', capabilities: [] } })
      }
      if (path === '/api/documents/' && request.method() === 'POST') {
        uploadCredentials.push(request.headers()['authorization'] || request.headers()['x-suchi-demo-token'] || 'cookie')
        if (uploadCredentials.length === 1) {
          return route.fulfill({ status: 403, json: { code: 'demo_upgrade_required' } })
        }
        return route.fulfill({ json: { id: 41, deduplicated: true } })
      }
      if (path === '/api/demo/session/upgrade') {
        upgrade = route
        return
      }
      return route.fallback()
    })
    await page.goto('/#/upload')
    await page.locator('input[type=file]').setInputFiles({
      name: 'first.pdf', mimeType: 'application/pdf', buffer: Buffer.from('%PDF-test-one'),
    })
    await expect.poll(() => !!upgrade).toBe(true)
    if (switchAccount) {
      const navigation = page.getByRole('button', { name: 'Open navigation', exact: true })
      if (await navigation.isVisible()) await navigation.click()
      await page.getByRole('button', { name: 'Sign out', exact: true }).click()
      await page.getByLabel('Email', { exact: true }).fill('second@example.test')
      await page.getByLabel('Password', { exact: true }).fill('test-password')
      await page.getByRole('button', { name: 'Sign in', exact: true }).click()
      await expect(page.getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible()
    }
    await upgrade.fulfill({ json: { kind: 'scratch' } })
    if (switchAccount) {
      await page.waitForTimeout(50)
      expect(await page.evaluate(() => localStorage.getItem('suchi.token'))).toBeNull()
      expect(uploadCredentials).toEqual(['cookie'])
      await expect(page.getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible()
    } else {
      await expect(page.getByText('duplicate', { exact: true })).toBeVisible()
      expect(uploadCredentials).toEqual(['cookie', 'cookie'])
      expect(await page.evaluate(() => localStorage.getItem('suchi.token'))).toBeNull()
    }
  })
}

for (const target of ['page', 'modal']) {
  test(`uploads each ${target} drop once and refreshes the document list`, async ({ page }) => {
    const options = { filingTreeChosen: true, documents: [] }
    await mockAPI(page, options)
    let uploads = 0
    await page.route('**/api/documents/', async route => {
      if (route.request().method() !== 'POST') return route.fallback()
      uploads++
      options.documents = [{ id: 41, title: 'Dropped receipt.pdf', created_at: 1780000000 }]
      return route.fulfill({ json: { id: 41, deduplicated: true } })
    })
    await page.goto('/#/documents')
    await expect(page.getByText('No documents match.', { exact: true })).toBeVisible()
    if (target === 'modal') {
      await page.getByRole('button', { name: 'Upload documents', exact: true }).click()
      await expect(page.getByRole('dialog', { name: 'Upload documents' }).locator('.drop')).toBeVisible()
    }
    await page.evaluate(target => {
      const transfer = new DataTransfer()
      transfer.items.add(new File(['%PDF-test-one'], 'receipt.pdf', { type: 'application/pdf' }))
      const dropTarget = target === 'modal' ? document.querySelector('.modal .drop') : window
      dropTarget.dispatchEvent(new DragEvent('dragenter', { dataTransfer: transfer, bubbles: true, cancelable: true }))
      dropTarget.dispatchEvent(new DragEvent('drop', { dataTransfer: transfer, bubbles: true, cancelable: true }))
    }, target)
    await expect(page.getByText('duplicate', { exact: true })).toBeVisible()
    await expect(page.locator('.dropveil')).toHaveCount(0)
    await page.getByRole('button', { name: 'Close upload' }).click()
    await expect(page.getByRole('link', { name: /Dropped receipt.pdf/ })).toBeVisible()
    expect(uploads).toBe(1)
  })
}

for (const input of ['drop', 'picker']) {
  test('stops queued ' + input + ' uploads when the account changes', async ({ page }) => {
    await mockAPI(page, { filingTreeChosen: true })
    let actor = 1
    let firstUpload
    const uploadActors = []
    await page.route('**/api/**', async route => {
      const path = new URL(route.request().url()).pathname
      if (path === '/api/logout') {
        actor = 0
        return route.fulfill({ status: 204 })
      }
      if (path === '/api/login') {
        actor = 2
        return route.fulfill({ json: { ok: true } })
      }
      if (path === '/api/whoami') {
        return route.fulfill({ json: { user_id: actor, email: 'second@example.test', display_name: 'Second user', role: 'member', capabilities: [] } })
      }
      if (path === '/api/documents/' && route.request().method() === 'POST') {
        uploadActors.push(actor)
        if (!firstUpload) {
          firstUpload = route
          return
        }
        return route.fulfill({ json: { id: 42, deduplicated: true } })
      }
      return route.fallback()
    })
    await page.goto(input === 'picker' ? '/#/upload' : '/#/dashboard')
    if (input === 'picker') {
      await page.locator('input[type=file]').setInputFiles([
        { name: 'first.pdf', mimeType: 'application/pdf', buffer: Buffer.from('%PDF-test-one') },
        { name: 'second.pdf', mimeType: 'application/pdf', buffer: Buffer.from('%PDF-test-two') },
      ])
    } else {
      await expect(page.getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible()
      await page.evaluate(() => {
        const transfer = new DataTransfer()
        transfer.items.add(new File(['%PDF-test-one'], 'first.pdf', { type: 'application/pdf' }))
        transfer.items.add(new File(['%PDF-test-two'], 'second.pdf', { type: 'application/pdf' }))
        window.dispatchEvent(new DragEvent('drop', { dataTransfer: transfer }))
      })
    }
    await expect.poll(() => !!firstUpload).toBe(true)
    if (input === 'drop') await page.getByRole('button', { name: 'Close upload' }).click()
    const navigation = page.getByRole('button', { name: 'Open navigation', exact: true })
    if (await navigation.isVisible()) await navigation.click()
    await page.getByRole('button', { name: 'Sign out', exact: true }).click()
    await page.getByLabel('Email', { exact: true }).fill('second@example.test')
    await page.getByLabel('Password', { exact: true }).fill('test-password')
    await page.getByRole('button', { name: 'Sign in', exact: true }).click()
    await expect(page.getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible()
    const finished = page.waitForEvent('requestfinished', request => request === firstUpload.request())
    await firstUpload.fulfill({ json: { id: 41, deduplicated: true } })
    await finished
    await page.waitForTimeout(50)
    expect(uploadActors).toEqual([1])
  })
}


const presets = [
  { id: 'solo', name: 'Solo', description: 'One person', set_ids: ['life-admin', 'finance', 'health', 'home'], areas: [{ code: 10, name: 'Life admin', category_count: 2, categories: [{ code: 11, name: 'Identity' }, { code: 14, name: 'Education & memberships' }] }] },
  { id: 'household', name: 'Household', description: 'A family', set_ids: ['family-admin', 'finance', 'health', 'home'], areas: [{ code: 10, name: 'Family admin', category_count: 2, categories: [{ code: 11, name: 'Family IDs' }, { code: 14, name: 'Memberships' }] }] },
  { id: 'freelance', name: 'Freelance', description: 'Client work', set_ids: ['clients', 'projects', 'business-finance', 'compliance', 'portfolio-marketing'], areas: [] },
  { id: 'smb_billing', name: 'Small business', description: 'Billing', set_ids: ['customers', 'vendors', 'finance-payroll', 'compliance', 'portfolio-marketing'], areas: [] },
  { id: 'blank', name: 'Blank', description: 'Build your own', blank: true, set_ids: [], areas: [] },
]

const filingSetCatalog = {
  max_sets: 5,
  sets: [
    { id: 'life-admin', name: 'Life admin', description: 'Identity, insurance, vehicles, education, and memberships', lane: 10, categories: [{ code: 11, name: 'Identity' }, { code: 14, name: 'Education & memberships' }] },
    { id: 'finance', name: 'Money', description: 'Investments, tax, credit, and receipts', lane: 20, categories: [{ code: 22, name: 'Investments' }] },
    { id: 'health', name: 'Health', description: 'Medical records', lane: 30, categories: [{ code: 31, name: 'Medical records' }] },
    { id: 'home', name: 'Home', description: 'Utilities, housing, and warranties', lane: 50, categories: [{ code: 51, name: 'Utilities' }, { code: 52, name: 'Housing' }] },
  ],
  recipes: Object.fromEntries(presets.map(preset => [preset.id, preset.set_ids])),
}

const taxonomyContent = `format = "suchi-taxonomy/v1"
id = "personal-records"
version = 2
name = "Personal records"
market = "global"
language = "en"
story = "Keep identity paperwork together."
[[areas]]
code = 10
name = "Life admin"
[[areas.categories]]
code = 11
name = "Identity"
description = "Passports and identity cards."
`

function taxonomyPreview(overrides = {}) {
  return {
    format: 'suchi-taxonomy/v1', preset_id: 'personal-records', preset_version: 2,
    name: 'Personal records', story: 'Keep identity paperwork together.',
    content_sha256: 'a'.repeat(64), state_hash: 'preview-state',
    mode: 'merge', areas_incoming: 1, categories_incoming: 1,
    categories_to_add: [11], collisions: [], keywords_to_seed: 0, automations_to_seed: 1,
    user_areas: [{ code: 10, name: 'Life admin', categories: [{ code: 11, name: 'Identity', description: 'Passports and identity cards.' }] }],
    generated_areas: [{ code: 40, name: 'System', categories: [{ code: 49, name: 'Inbox' }] }],
    rules_to_add: ['File identity'], rules_preserved: [{ name: 'Local starter', enabled: false }],
    rules_skipped: [], applied: false, index_refresh_pending: false,
    ...overrides,
  }
}

function presetChangePreview(payload, overrides = {}) {
  const preset = presets.find(candidate => candidate.id === payload.preset_id)
  const setIDs = payload.set_ids ?? preset?.set_ids ?? []
  const areas = payload.set_ids
    ? filingSetCatalog.sets.filter(set => setIDs.includes(set.id)).map(set => ({ code: set.lane, name: set.name, categories: set.categories }))
    : (preset?.areas || []).map(area => ({ code: area.code, name: area.name, categories: area.categories }))
  const collisions = (overrides.collisions || []).map(collision => ({
    ...collision,
    resolved: collision.resolved || !!payload.replacements?.[collision.code] ||
      Object.prototype.hasOwnProperty.call(payload.remaps || {}, collision.code),
    replace: collision.replace || !!payload.replacements?.[collision.code],
  }))
  return taxonomyPreview({
    ...overrides,
    preset_id: preset?.id || 'composed-v1-test',
    name: preset?.name || 'Custom filing tree',
    story: preset?.description || 'A filing tree composed from focused sets.',
    set_ids: setIDs,
    current_set_ids: overrides.current_set_ids || [],
    user_areas: areas,
    collisions,
  })
}

async function openTaxonomyImport(page) {
  await page.goto('/#/settings?tab=archive&section=filing-tree')
  await page.getByRole('button', { name: 'Import or export' }).click()
  await page.getByRole('button', { name: 'Import file' }).click()
  return page.getByRole('region', { name: 'Import taxonomy', exact: true })
}

test('taxonomy import previews a named tree and refreshes setup and sidebar after apply', async ({ page }) => {
  const requests = []
  await mockAPI(page, {
    jdCategoriesAfterPreset: [
      { id: 11, code: 11, name: 'Identity', area_code: 10, area_name: 'Life admin' },
      { id: 49, code: 49, name: 'Inbox', area_code: 40, area_name: 'System', system: true },
    ],
    taxonomyImport: async payload => {
      requests.push(payload)
      return taxonomyPreview({ applied: payload.apply, index_refresh_pending: payload.apply })
    },
  })
  const importer = await openTaxonomyImport(page)
  await importer.getByLabel('Choose taxonomy file').setInputFiles({
    name: 'personal.toml', mimeType: 'text/plain', buffer: Buffer.from(taxonomyContent),
  })
  await expect(importer.getByRole('combobox', { name: 'Serialization', exact: true })).toHaveValue('toml')
  await importer.getByRole('button', { name: 'Preview', exact: true }).click()
  await expect(importer.getByRole('heading', { name: /Personal records.*content revision 2/ })).toBeVisible()
  await expect(importer.getByText('Passports and identity cards.')).toBeVisible()
  await expect(importer.getByText('00–09 System index', { exact: true })).toBeVisible()
  await expect(importer.getByText('49 Inbox', { exact: true })).toBeVisible()
  await expect(importer.getByText('Local starter — disabled (stays disabled)', { exact: true })).toBeVisible()
  await expect(page.locator('.setup-reminder-settings')).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  await importer.getByRole('button', { name: 'Apply import', exact: true }).click()
  await expect(page.locator('.setup-reminder-settings')).toHaveCount(0)
  expect(requests[0]).toMatchObject({ content: taxonomyContent, format: 'toml', apply: false, skip_seeds: false, remaps: {} })
  expect(requests[1]).toMatchObject({ expected_state_hash: 'preview-state', apply: true, format: 'toml' })
  await page.goto('/#/dashboard')
  if ((page.viewportSize()?.width || 0) <= 860) await page.getByRole('button', { name: 'Open navigation' }).click()
  await page.locator('.area-toggle').filter({ hasText: 'Life admin' }).click()
  await expect(page.getByRole('link', { name: '11 Identity', exact: true })).toBeVisible()
})

test('taxonomy import renders server validation failures and never applies invalid input', async ({ page }) => {
  const requests = []
  await mockAPI(page, {
    taxonomyImport: async (payload, route) => {
      requests.push(payload)
      await route.fulfill({ status: 400, json: { code: 'invalid_taxonomy', error: 'areas[0].code: area 40 is reserved' } })
    },
  })
  const importer = await openTaxonomyImport(page)
  await importer.getByLabel('Paste content').fill('unsupported content')
  await importer.getByRole('combobox', { name: 'Serialization', exact: true }).selectOption('toml')
  await importer.getByRole('button', { name: 'Preview', exact: true }).click()
  await expect(importer.getByRole('alert')).toHaveText('areas[0].code: area 40 is reserved')
  await expect(importer.getByLabel('Paste content')).toHaveValue('unsupported content')
  await expect(importer.getByRole('button', { name: 'Apply import', exact: true })).toBeDisabled()
  expect(requests).toHaveLength(1)
  expect(requests[0].apply).toBe(false)
})

test('taxonomy import requires explicit previewed collision choices and retains them after invalidation', async ({ page }) => {
  const requests = []
  await mockAPI(page, {
    taxonomyImport: async payload => {
      requests.push(payload)
      const resolved = Object.hasOwn(payload.remaps, '11')
      return taxonomyPreview({
        collisions: [{ code: 11, existing: 'Old identity', incoming: 'Identity', proposed_code: 12, resolved }],
        categories_to_add: payload.remaps['11'] === 12 ? [12] : [],
        rules_to_add: [],
        rules_skipped: payload.remaps['11'] === 0 ? [{ name: 'File identity', reason: 'Depends on skipped category 11' }] : [],
        state_hash: `collision-${requests.length}`,
      })
    },
  })
  const importer = await openTaxonomyImport(page)
  await importer.getByLabel('Paste content').fill(taxonomyContent)
  await importer.getByRole('combobox', { name: 'Serialization', exact: true }).selectOption('toml')
  await importer.getByRole('button', { name: 'Preview', exact: true }).click()
  await expect(importer.getByLabel('Choice for 11')).toHaveValue('')
  await expect(importer.getByRole('button', { name: 'Apply import', exact: true })).toBeDisabled()
  expect(requests[0].remaps).toEqual({})
  await importer.getByLabel('Choice for 11').selectOption('12')
  await expect(importer.getByLabel('Choice for 11')).toHaveValue('12')
  await expect(importer.getByRole('button', { name: 'Apply import', exact: true })).toBeDisabled()
  await importer.getByRole('button', { name: 'Preview', exact: true }).click()
  await expect(importer.getByRole('button', { name: 'Apply import', exact: true })).toBeEnabled()
  expect(requests[1].remaps).toEqual({ 11: 12 })
  await importer.getByLabel('Choice for 11').selectOption('0')
  await importer.getByRole('button', { name: 'Preview', exact: true }).click()
  await expect(importer.getByText('File identity — Depends on skipped category 11')).toBeVisible()
  await importer.getByLabel('Include starter rules').uncheck()
  await expect(importer.getByLabel('Choice for 11')).toHaveValue('0')
  await expect(importer.getByRole('button', { name: 'Apply import', exact: true })).toBeDisabled()
  await importer.getByRole('button', { name: 'Preview', exact: true }).click()
  expect(requests[3]).toMatchObject({ remaps: { 11: 0 }, skip_seeds: true })
  await importer.getByRole('combobox', { name: 'Serialization', exact: true }).selectOption('huml')
  await expect(importer.getByRole('button', { name: 'Apply import', exact: true })).toBeDisabled()
  await importer.getByLabel('Paste content').fill(taxonomyContent + '\n')
  await expect(importer.getByLabel('Choice for 11')).toHaveCount(0)
})

test('taxonomy import preserves input after stale preview and binds apply to a fresh preview', async ({ page }) => {
  const requests = []
  let stale = true
  await mockAPI(page, {
    taxonomyImport: async (payload, route) => {
      requests.push(payload)
      if (payload.apply && stale) {
        stale = false
        await route.fulfill({ status: 409, json: { code: 'stale_preview', error: 'State changed' } })
        return
      }
      return taxonomyPreview({ state_hash: stale ? 'old-state' : 'new-state' })
    },
  })
  const importer = await openTaxonomyImport(page)
  await importer.getByLabel('Paste content').fill(taxonomyContent)
  await importer.getByRole('combobox', { name: 'Serialization', exact: true }).selectOption('toml')
  await importer.getByRole('button', { name: 'Preview', exact: true }).click()
  await importer.getByRole('button', { name: 'Apply import', exact: true }).click()
  await expect(importer.getByRole('alert')).toContainText('Preview again before applying')
  await expect(importer.getByLabel('Paste content')).toHaveValue(taxonomyContent)
  await expect(importer.getByRole('button', { name: 'Apply import', exact: true })).toBeDisabled()
  await importer.getByRole('button', { name: 'Preview', exact: true }).click()
  await importer.getByRole('button', { name: 'Apply import', exact: true }).click()
  await expect.poll(() => requests.length).toBe(4)
  expect(requests[1].expected_state_hash).toBe('old-state')
  expect(requests[3].expected_state_hash).toBe('new-state')
})

test('taxonomy import confirms blank trees in a native modal before applying', async ({ page }) => {
  const requests = []
  await mockAPI(page, {
    taxonomyImport: async payload => {
      requests.push(payload)
      return taxonomyPreview({ categories_incoming: 0, categories_to_add: [], user_areas: [], rules_to_add: [] })
    },
  })
  const importer = await openTaxonomyImport(page)
  await importer.getByLabel('Paste content').fill(taxonomyContent.split('[[areas]]')[0] + 'areas = []\n')
  await importer.getByRole('combobox', { name: 'Serialization', exact: true }).selectOption('toml')
  await importer.getByRole('button', { name: 'Preview', exact: true }).click()
  await importer.getByRole('button', { name: 'Apply import', exact: true }).click()
  const confirmation = page.getByRole('alertdialog', { name: 'Import a blank filing tree?' })
  await expect(confirmation).toBeVisible()
  expect(await confirmation.evaluate(element => element.matches(':modal'))).toBe(true)
  await page.keyboard.press('Escape')
  await expect(confirmation).toHaveCount(0)
  expect(requests).toHaveLength(1)
  await importer.getByRole('button', { name: 'Apply import', exact: true }).click()
  await confirmation.getByRole('button', { name: 'Apply blank import' }).click()
  await expect.poll(() => requests.length).toBe(2)
  expect(requests[1].apply).toBe(true)
})

test('taxonomy import ignores late file reads and reports unreadable or unsupported files', async ({ page }) => {
  await mockAPI(page)
  await page.addInitScript(() => {
    window.taxonomyReaders = []
    window.FileReader = class {
      readAsText() { window.taxonomyReaders.push(this) }
      abort() {}
    }
  })
  const importer = await openTaxonomyImport(page)
  const file = importer.getByLabel('Choose taxonomy file')
  await file.setInputFiles({ name: 'old.toml', mimeType: 'text/plain', buffer: Buffer.from('old') })
  await expect(importer.getByRole('status')).toHaveText('Reading file…')
  await importer.getByLabel('Paste content').fill('new pasted content')
  await page.evaluate(() => {
    const reader = window.taxonomyReaders[0]
    reader.result = 'late old content'
    reader.onload()
  })
  await expect(importer.getByLabel('Paste content')).toHaveValue('new pasted content')
  await file.setInputFiles({ name: 'broken.huml', mimeType: 'text/plain', buffer: Buffer.from('broken') })
  await page.evaluate(() => window.taxonomyReaders[1].onerror())
  await expect(importer.getByRole('alert')).toContainText('Could not read broken.huml')
  await expect(importer.getByRole('button', { name: 'Preview', exact: true })).toBeDisabled()
  await file.setInputFiles({ name: 'unsupported.yaml', mimeType: 'text/plain', buffer: Buffer.from('x') })
  await expect(importer.getByRole('alert')).toContainText('Choose a .huml or .toml file')
})

test('taxonomy import disables conflicting requests and discards a preview after navigation', async ({ page }) => {
  let pending
  await mockAPI(page, { taxonomyImport: async (_, route) => { pending = route } })
  const importer = await openTaxonomyImport(page)
  await importer.getByLabel('Paste content').fill(taxonomyContent)
  await importer.getByRole('combobox', { name: 'Serialization', exact: true }).selectOption('toml')
  await importer.getByRole('button', { name: 'Preview', exact: true }).click()
  await expect.poll(() => !!pending).toBe(true)
  await expect(importer.getByLabel('Paste content')).toBeDisabled()
  await expect(importer.getByRole('combobox', { name: 'Serialization', exact: true })).toBeDisabled()
  await expect(importer.getByLabel('Include starter rules')).toBeDisabled()
  await expect(page.getByRole('button', { name: 'Close', exact: true })).toBeDisabled()
  await page.goto('/#/dashboard')
  await pending.fulfill({ json: taxonomyPreview() }).catch(() => {})
  const nextImporter = await openTaxonomyImport(page)
  await expect(nextImporter.getByLabel('Paste content')).toHaveValue('')
  await expect(nextImporter.getByRole('button', { name: 'Apply import', exact: true })).toBeDisabled()
})

test('taxonomy settings offers explicit starter and tree-only exports without silent fallback', async ({ page }) => {
  const exports = []
  await mockAPI(page, { filingTreeChosen: true })
  await page.route('**/api/admin/taxonomy/export?**', async route => {
    const url = new URL(route.request().url())
    exports.push(Object.fromEntries(url.searchParams))
    if (url.searchParams.get('skip_seeds') === 'false') {
      await route.fulfill({ status: 400, json: { error: 'Disabled starter cannot be exported. Use tree-only export.' } })
    } else {
      await route.fulfill({ contentType: 'text/plain', body: taxonomyContent })
    }
  })
  await page.goto('/#/settings?tab=archive&section=filing-tree')
  await page.getByRole('button', { name: 'Import or export' }).click()
  await page.getByRole('combobox', { name: 'File format', exact: true }).selectOption('toml')
  await page.getByRole('button', { name: 'Export with starter rules' }).click()
  await expect(page.getByText('Disabled starter cannot be exported. Use tree-only export.', { exact: true })).toBeVisible()
  expect(exports).toEqual([{ format: 'toml', skip_seeds: 'false' }])
  const download = page.waitForEvent('download')
  await page.getByRole('button', { name: 'Export tree only' }).click()
  expect((await download).suggestedFilename()).toBe('archive-tree.toml')
  expect(exports[1]).toEqual({ format: 'toml', skip_seeds: 'true' })
  await page.getByRole('button', { name: 'Import file' }).click()
  await expect(page.getByRole('region', { name: 'Import taxonomy', exact: true })).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
})

// Mirror the server's strict decoder so payload drift fails in the browser suite.
const llmInputFields = [
  'subscription_provider', 'enabled', 'endpoint_url', 'model', 'api_key', 'clear_api_key', 'egress_ack',
  'confidence_threshold', 'archive_enabled', 'archive_review_threshold', 'archive_auto_threshold',
]

function unexpectedFields(payload, allowed) {
  return Object.keys(payload).filter(key => !allowed.includes(key))
}

async function mockAPI(page, options = {}) {
  let taxonomyApplied = false
  let researchContextMode = options.researchContextMode || 'balanced'
  let autoApply = options.autoApply
  let llmSettings = {
    enabled: options.llmEnabled ?? false,
    active: options.llmActive ?? false,
    endpoint_url: options.llmEndpoint ?? (options.subscriptionConnected ? '' : 'http://host.suchi.local:11434/v1'),
    model: options.llmModel || 'qwen2.5:7b',
    has_api_key: options.llmHasAPIKey ?? false,
    mode: options.subscriptionConnected ? 'subscription' : 'local',
    subscription_provider: 'openai_chatgpt',
    subscription_connected: options.subscriptionConnected ?? false,
    subscription_model: options.subscriptionModel || '',
    egress_ack: options.llmEgressAck ?? false,
    confidence_threshold: 0.7,
    archive_enabled: true,
    archive_review_threshold: 0.5,
    archive_auto_threshold: 0.9,
  }
  let trashDocuments = [...(options.trashDocuments || [])]
  let renderedLayouts = (options.renderedLayouts || []).map(layout => ({ ...layout }))
  const restoredDocuments = new Set()
  await page.route('**/preview/**', async route => {
    await route.fulfill({
      contentType: 'text/html',
      body: options.previewHTML || '<p>Document preview</p>',
    })
  })
  await page.route('**/api/**', async route => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    options.apiRequests?.push({ method: request.method(), path })
    if (options.demoSession && /^\/api\/(tokens|decryption-passwords|email-accounts|mobile\/pairing|users\/me)(\/|$)/.test(path)) {
      if (options.demoSession === 'scratch') {
        return route.fulfill({ status: 403, json: { code: 'token_route_forbidden', error: 'API tokens are not allowed on this route' } })
      }
      if (!['GET', 'HEAD', 'OPTIONS'].includes(request.method())) {
        return route.fulfill({ status: 403, json: { code: 'demo_upgrade_required', message: 'Anonymous demo sessions are read-only' } })
      }
      if (path.startsWith('/api/tokens')) {
        return route.fulfill({ status: 401, json: { code: 'unauthorized', error: 'auth required' } })
      }
      if (path.startsWith('/api/email-accounts')) {
        return route.fulfill({ status: 403, json: { code: 'forbidden', error: 'mailboxes not enabled for this user' } })
      }
      return route.fulfill({ json: { results: [] } })
    }
    const thumb = path.match(/^\/api\/documents\/(\d+)\/thumb\/?$/)
    const documentDetail = path.match(/^\/api\/documents\/(\d+)$/)
    const documentVersions = path.match(/^\/api\/documents\/(\d+)\/versions\/$/)
    const similarDocuments = path.match(/^\/api\/documents\/(\d+)\/similar$/)
    const documentAccess = path.match(/^\/api\/acls\/document\/(\d+)$/)
    const trashDocument = path.match(/^\/api\/trash\/(\d+)$/)
    const restoreDocument = path.match(/^\/api\/documents\/(\d+)\/restore$/)
    const savedView = path.match(/^\/api\/saved_views\/(\d+)$/)
    const renderedLayout = path.match(/^\/api\/rendered_layouts\/(\d+)$/)
    if (thumb && options.thumbnailFailures) {
      if (options.thumbnailFailures.includes(Number(thumb[1]))) {
        if (options.thumbnailDelay) {
          await new Promise(resolve => setTimeout(resolve, options.thumbnailDelay))
        }
        await route.fulfill({ status: 404, json: { code: 'no_thumb', error: 'no thumbnail' } })
      } else {
        await route.fulfill({
          contentType: 'image/svg+xml',
          body: '<svg xmlns="http://www.w3.org/2000/svg" width="30" height="38"><rect width="30" height="38" fill="#ddd"/></svg>',
        })
      }
      return
    }
    if (path === '/api/jd/categories/' && options.taxonomyFailure) {
      await route.fulfill({ status: 500, json: { error: 'taxonomy unavailable' } })
      return
    }
    if (path === '/api/admin/settings/llm' && request.method() === 'PATCH' &&
        (request.postDataJSON().auto_apply !== undefined ? options.applicationModeSaveFailure : options.researchContextSaveFailure)) {
      await route.fulfill({
        status: 500,
        json: { code: 'save_failed', error: options.failureMessage || 'research context unavailable' },
      })
      return
    }
    if (options.failPaths?.includes(path)) {
      await route.fulfill({
        status: options.failureStatus || 500,
        json: {
          code: options.failureCode,
          error: options.failureMessage || 'forced request failure',
        },
      })
      return
    }
    let body = { results: [], count: 0 }

    if (path === '/api/demo/mode') body = { enabled: !!options.demoMode }
    else if (path === '/api/whoami') body = {
      user_id: options.userID ?? (options.demoSession === 'anon' ? 0 : 1),
      email: options.userEmail || (options.demoSession ? 'visitor@demo.local' : 'admin@example.test'),
      display_name: options.demoSession ? 'Demo visitor' : 'Admin',
      role: options.userRole || (options.demoSession ? 'member' : 'admin'),
      authn_by: options.demoSession ? 'demo' : 'local',
      email_change_mode: options.emailChangeMode || (options.demoSession ? 'disabled' : 'password'),
      build_version: options.buildVersion,
      build_revision: options.buildRevision,
      capabilities: options.capabilities ?? (options.demoSession ? [] : ['mailboxes']),
      kind: options.demoSession ? `demo-${options.demoSession}` : 'user',
    }
    else if (path === '/api/jd/systems') body = options.systems || { introduced: false, default_system_code: '', results: [] }
    else if (path === '/api/stats/') body = {
      documents_total: options.documentsCount ?? options.documents?.length ?? 0,
      inbox_count: 0,
      pending_approvals: 0,
      dead_jobs: 0,
    }
    else if (path === '/api/jd/categories/') body = {
      results: taxonomyApplied && options.jdCategoriesAfterPreset
        ? options.jdCategoriesAfterPreset
        : (options.jdCategories || []),
    }
    else if (path === '/api/custom_fields/') body = { results: options.customFields || [] }
    else if (path === '/api/rendered_layouts/preview' && request.method() === 'POST') {
      const template = request.postDataJSON().template
      const usesASN = template.includes('{{ asn }}')
      body = {
        path: usesASN ? 'Legacy/4021/March electricity bill' : '10-19 Home/13 Utilities',
        uses_asn: usesASN,
      }
    }
    else if (path === '/api/rendered_layouts/' && request.method() === 'POST') {
      const input = request.postDataJSON()
      const id = Math.max(0, ...renderedLayouts.map(layout => layout.id)) + 1
      renderedLayouts.push({ id, name: input.name, path: input.path, uses_asn: input.path.includes('{{ asn }}') })
      body = { id }
    }
    else if (path === '/api/rendered_layouts/') body = { results: renderedLayouts }
    else if (renderedLayout && request.method() === 'PATCH') {
      const input = request.postDataJSON()
      renderedLayouts = renderedLayouts.map(layout => Number(layout.id) === Number(renderedLayout[1])
        ? { ...layout, ...input, uses_asn: (input.path ?? layout.path).includes('{{ asn }}') }
        : layout)
      body = { id: Number(renderedLayout[1]) }
    }
    else if (renderedLayout && request.method() === 'DELETE') {
      renderedLayouts = renderedLayouts.filter(layout => Number(layout.id) !== Number(renderedLayout[1]))
      await route.fulfill({ status: 204, body: '' })
      return
    }
    else if (path === '/api/presets/') body = { results: presets }
    else if (path === '/api/filing-sets/') body = filingSetCatalog
    else if (path === '/api/admin/taxonomy/import' && options.taxonomyImport) {
      const payload = request.postDataJSON()
      const result = await options.taxonomyImport(payload, route)
      if (!result) return
      if (result.applied) taxonomyApplied = true
      await route.fulfill({ json: result })
      return
    }
    else if (path === '/api/admin/setup/preset/preview' && request.method() === 'POST') {
      body = presetChangePreview(request.postDataJSON(), {
        collisions: options.presetCollisions || [],
        current_set_ids: options.currentSetIDs || [],
      })
    }
    else if (path === '/api/admin/setup/preset/apply' && request.method() === 'POST') {
      taxonomyApplied = true
      body = presetChangePreview(request.postDataJSON(), {
        collisions: options.presetCollisions || [], applied: true, index_refresh_pending: true,
      })
    }
    else if (path === '/api/admin/setup/state') body = {
      current_preset: options.currentPreset || '',
      current_set_ids: options.currentSetIDs || filingSetCatalog.recipes[options.currentPreset] || [],
      filing_tree_chosen: taxonomyApplied || (options.filingTreeChosen ?? false),
      started_at: options.setupStartedAt ?? Math.floor(Date.now() / 1000),
    }
    else if (path === '/api/admin/users') body = {
      results: [
        { id: 1, email: 'admin@example.test', display_name: 'Admin', role: 'admin', capabilities: [] },
        { id: 2, email: 'member@example.test', display_name: 'Member', role: 'member', disabled: false, capabilities: ['mailboxes'] },
      ],
    }
    else if (path === '/api/automations/') body = {
      results: options.automations ?? [{
        id: 7, name: 'Tag utility bills', enabled: true, order: 0,
        triggers: [{ type: 2 }],
        actions: [{ id: 1, type: 'assign_tags', params: { tag_ids: [] } }],
      }],
    }
    else if (path === '/api/automations/schema') body = {
      triggers: [{ code: 2, type: 'document_added', name: 'After a new document lands' }],
      actions: options.automationActions || [{ kind: 'assign_tags', name: 'Add tags', params: [{ name: 'tag_ids' }] }],
    }
    else if (path === '/api/admin/settings/llm') {
      if (request.method() === 'PATCH') {
        const payload = request.postDataJSON()
        const unexpected = unexpectedFields(payload, ['research_context_mode', 'auto_apply', 'subscription_model', 'subscription_provider', 'archive_enabled', 'archive_auto_threshold', 'archive_review_threshold'])
        const archivePatch = Object.keys(payload).some(key => key.startsWith('archive_'))
        const valid = archivePatch || typeof payload.auto_apply === 'boolean' ||
          ['focused', 'balanced', 'detailed'].includes(payload.research_context_mode) || typeof payload.subscription_model === 'string'
        if (unexpected.length || (!archivePatch && Object.keys(payload).length !== (payload.subscription_model ? 2 : 1)) || !valid) {
          await route.fulfill({
            status: 400,
            json: { code: 'bad_json', error: unexpected.length ? `unknown field ${unexpected[0]}` : 'invalid research context mode' },
          })
          return
        }
        if (archivePatch) {
          options.archiveMatchingRequests?.push(payload)
          llmSettings = { ...llmSettings, ...payload }
          body = { archive_enabled: llmSettings.archive_enabled, archive_auto_threshold: llmSettings.archive_auto_threshold, archive_review_threshold: llmSettings.archive_review_threshold }
        } else if (typeof payload.auto_apply === 'boolean') {
          options.applicationModeRequests?.push(payload)
          autoApply = payload.auto_apply
          body = { auto_apply: autoApply }
        } else if (typeof payload.subscription_model === 'string') {
          options.subscriptionModelRequests?.push(payload)
          llmSettings.subscription_model = payload.subscription_model
          body = { subscription_provider: payload.subscription_provider, subscription_model: payload.subscription_model }
        } else {
          options.researchContextRequests?.push(payload)
          researchContextMode = payload.research_context_mode
          body = { research_context_mode: researchContextMode }
        }
      } else if (request.method() === 'POST') {
        const payload = request.postDataJSON()
        const unexpected = unexpectedFields(payload, llmInputFields)
        if (unexpected.length) {
          await route.fulfill({
            status: 400,
            json: { code: 'bad_json', error: `unknown field ${unexpected[0]}` },
          })
          return
        }
        options.llmSettingsRequests?.push(payload)
        const { api_key, clear_api_key, ...saved } = payload
        llmSettings = {
          ...llmSettings, ...saved, active: saved.enabled,
          has_api_key: clear_api_key ? false : !!api_key || llmSettings.has_api_key,
        }
        body = { saved: true, active: payload.enabled }
      } else {
        body = {
          ...llmSettings,
          auto_apply: autoApply,
          research_context_mode: researchContextMode,
        }
      }
    }
    else if (path === '/api/admin/settings/llm/test') {
      const payload = request.postDataJSON()
      const unexpected = unexpectedFields(payload, llmInputFields)
      if (unexpected.length) {
        await route.fulfill({
          status: 400,
          json: { code: 'bad_json', error: `unknown field ${unexpected[0]}` },
        })
        return
      }
      options.llmTestRequests?.push(payload)
      body = {
        message: 'Classifier connection passed',
        result: { title: 'Connection test', confidence: 0.91, elapsed_ms: 12, tags: [] },
      }
    }
    else if (path === '/api/admin/settings/preferences') body = {
      backup_interval_hours: 24,
      ocr_languages: ['eng'],
    }
    else if (path === '/api/admin/settings/ingest') body = {
      fs_watch_dir: '',
      fs_watch_owner_email: '',
    }
    else if (path === '/api/documents/') {
      const query = new URL(request.url()).searchParams.get('q') || ''
      const response = options.documentsByQuery?.[query]
      if (response?.delay) await new Promise(resolve => setTimeout(resolve, response.delay))
      const documents = response?.documents ?? options.documents ?? []
      body = {
        count: response?.count ?? options.documentsCount ?? documents.length,
        results: documents,
      }
    }
    else if (path === '/api/search/') {
      const query = new URL(request.url()).searchParams.get('q') || ''
      const response = options.searchByQuery?.[query]
      if (response?.delay) await new Promise(resolve => setTimeout(resolve, response.delay))
      const results = response?.results || []
      body = { count: response?.count ?? results.length, results }
    }
    else if (path === '/api/autocomplete/') {
      const query = new URL(request.url()).searchParams.get('q') || ''
      options.autocompleteQueries?.push(query)
      const response = options.autocompleteByQuery?.[query]
      if (response?.delay) await new Promise(resolve => setTimeout(resolve, response.delay))
      body = { results: response?.results || [] }
    }
    else if (path === '/api/chat/status') body = {
      enabled: !!options.chatEnabled,
      provider: options.chatProvider || 'local.test',
      local: options.chatLocal ?? true,
    }
    else if (path === '/api/chat' && request.method() === 'POST') {
      const payload = request.postDataJSON()
      options.chatRequests?.push(payload)
      const response = typeof options.chatResponse === 'function'
        ? options.chatResponse(payload, options.chatRequests?.length || 1)
        : options.chatResponse
      if (response?.delay) await new Promise(resolve => setTimeout(resolve, response.delay))
      body = response || {
        answer: 'The archive supports this answer [1].',
        sources: [{ id: 17, title: 'Archive evidence.pdf', snippet: 'Supporting document text', sensitivity: 'internal' }],
        citations: [1],
        grounded: true,
        intelligence: { accepted: {}, pending: {} },
      }
    }
    else if (path === '/api/intelligence/schema') body = {
      types: [{ type: 'date', label: 'Dates', roles: ['issued', 'due', 'start', 'end', 'expiry', 'renewal', 'service', 'other'] }],
    }
    else if (path === '/api/intelligence/' && request.method() === 'GET') {
      const query = Object.fromEntries(new URL(request.url()).searchParams)
      options.intelligenceQueries?.push(query)
      body = typeof options.intelligence === 'function'
        ? options.intelligence(query)
        : { results: options.intelligence || [], count: options.intelligenceCount ?? options.intelligence?.length ?? 0 }
    }
    else if (path === '/api/intelligence/extract' && request.method() === 'POST') {
      const payload = request.postDataJSON()
      options.intelligenceRequests?.push({ action: 'extract', ...payload })
      body = {
        total: payload.document_ids?.length || 0,
        applied: payload.document_ids?.length || 0,
        results: (payload.document_ids || []).map(id => ({ id, ok: true })),
      }
    }
    else if (path === '/api/intelligence/resolve' && request.method() === 'POST') {
      const payload = request.postDataJSON()
      options.intelligenceRequests?.push({ action: 'resolve', ...payload })
      body = {
        total: payload.candidate_ids?.length || 0,
        applied: payload.candidate_ids?.length || 0,
        results: (payload.candidate_ids || []).map(id => ({ id, ok: true })),
      }
    }
    else if (path === '/api/languages/') body = { languages: [] }
    else if (path === '/api/saved_views/' && request.method() === 'POST' && options.savedViewCreateError) {
      await route.fulfill({
        status: 400,
        json: { code: 'invalid_filter', error: options.savedViewCreateError },
      })
      return
    }
    else if (path === '/api/saved_views/') body = { results: options.savedViews || [] }
    else if (savedView && request.method() === 'PATCH') {
      const view = options.savedViews?.find(view => view.id === Number(savedView[1]))
      if (!view || view.owner_id) {
        await route.fulfill({ status: 404, json: { error: 'no such saved view' } })
        return
      }
      Object.assign(view, request.postDataJSON())
      body = { id: view.id }
    }
    else if (path === '/api/trash/' && request.method() === 'DELETE') {
      options.emptyTrashRequests?.push({ count: trashDocuments.length })
      body = { purged: trashDocuments.length }
      trashDocuments = []
    }
    else if (path === '/api/trash/') {
      body = { count: trashDocuments.length, results: trashDocuments }
    }
    else if (trashDocument && request.method() === 'DELETE') {
      const documentID = Number(trashDocument[1])
      options.permanentDeleteRequests?.push(documentID)
      trashDocuments = trashDocuments.filter(document => document.id !== documentID)
      await route.fulfill({ status: 204 })
      return
    }
    else if (restoreDocument && request.method() === 'POST') {
      const documentID = Number(restoreDocument[1])
      options.restoreRequests?.push(documentID)
      restoredDocuments.add(documentID)
      trashDocuments = trashDocuments.filter(document => document.id !== documentID)
      await route.fulfill({ status: 204 })
      return
    }
    else if (path === '/api/email-accounts') body = {
      accounts: [
        {
          id: 1, owner_id: 1, name: 'Personal Outlook archive', provider: 'microsoft',
          username: 'admin@example.test', enabled: true, last_sync_at: Math.floor(Date.now() / 1000) - 240,
        },
        {
          id: 2, owner_id: 1, name: 'johnnybravo.xyz@protonmail.com via homelab bridge', provider: 'proton',
          username: 'johnnybravo.xyz@protonmail.com', enabled: true, last_sync_at: Math.floor(Date.now() / 1000) - 300,
        },
        {
          id: 3, owner_id: 1, name: 'Receipts and statements from Gmail', provider: 'gmail',
          host: 'imap.gmail.com', port: 993, use_tls: true, folder: 'INBOX',
          username: 'admin.archive@gmail.com', enabled: true, last_sync_at: Math.floor(Date.now() / 1000) - 60,
          intake_policy: { rules: [{ selection: 'all', content: 'email_and_files' }] },
        },
      ],
      capabilities: { microsoft_oauth: { ready: true, reason: '' } },
    }
    else if (path === '/api/email-accounts/3/preview') body = {
      inspected: 10,
      matched: 2,
      samples: [{ from: 'billing@example.com', subject: 'August invoice', attachments: ['invoice-august.pdf'] }],
    }
    else if (path === '/api/decryption-passwords/') body = {
      results: [
        {
          id: 1, label: 'Hathway broadband invoice password', last_used_at: 1780100000,
          last_used_doc_id: 31, last_used_doc_title: 'Hathway Broadband Internet Tax Invoice - August 2026.pdf',
        },
        {
          id: 2, label: 'Axis Bank card statement', last_used_at: 1780000000,
          last_used_doc_id: 32, last_used_doc_title: 'Axis Bank Atlas Credit Card Statement ending 0194.pdf',
        },
      ],
    }
    else if (path === '/api/tokens/') body = request.method() === 'POST'
      ? { token: 'new-token-secret', name: 'Readonly tablet', scopes: 'documents:read' }
      : { results: [{ id: 1, name: 'Archive search', scopes: 'documents:read', created_at: 1780100000 }] }
    else if (path === '/api/tasks/') {
      const include = new URL(request.url()).searchParams.get('include')
      options.taskIncludes?.push(include)
      body = include === 'approvals' ? {
        counts: { approvals_open: options.approvalTasks?.length || 2 },
        results: [],
        approval_tasks: options.approvalTasks || [
          {
            id: 9, run_id: 3, approval_id: 1, approval_name: 'document-change',
            doc_id: 17, doc_title: 'HDFC receipt.pdf', doc_jd_category_id: 8,
            doc_jd_category_code: 24, doc_jd_category_name: 'Receipts',
            doc_has_thumbnail: false, state_key: 'review', assignee: 'user:1',
            prompt: 'Review suggested document metadata', choices: ['apply', 'reject'],
            status: 'open', created_at: 1780100000,
            vars: { field: 'tag', proposed_value: 'banking', current_value: 'receipt', confidence: 0.63, source: 'archive', sources: [{ document_id: 14, title: 'Bank statement' }], reason: 'review_first', policy_version: 'review-first-v1', source_current: true, review_conflict: false },
          },
          {
            id: 10, run_id: 4, approval_id: 1, approval_name: 'document-change',
            doc_id: 17, doc_title: 'HDFC receipt.pdf', doc_jd_category_id: 8,
            doc_jd_category_code: 24, doc_jd_category_name: 'Receipts',
            doc_has_thumbnail: false, state_key: 'review', assignee: 'user:1',
            prompt: 'Review suggested document metadata', choices: ['apply', 'reject'],
            status: 'open', created_at: 1780100000,
            vars: { field: 'correspondent', proposed_value: 'HDFC Bank', current_value: '', confidence: 0.63, source: 'archive', sources: [{ document_id: 14, title: 'Bank statement' }], reason: 'review_first', policy_version: 'review-first-v1', source_current: true, review_conflict: false },
          },
        ],
      } : { counts: { dead: options.deadJobs?.length || 0 }, results: options.deadJobs || [] }
    }
    else if (documentDetail) {
      const documentID = Number(documentDetail[1])
      const response = options.documentDetails?.[documentID]
      if (response?.delay) await new Promise(resolve => setTimeout(resolve, response.delay))
      body = {
        id: documentID,
        title: documentID === 42 ? 'Electricity bill' : `Document ${documentID}`,
        content: options.documentContent || '',
        original_blob: 'abc',
        original_size: 2048,
        mime_type: options.documentMime || 'application/pdf',
        sensitivity: options.documentSensitivity || '',
        jd_category_id: 1,
        created_at: 1780000000,
        added_at: 1780100000,
        updated_at: 1780100000,
        source_mtime: 1779900000,
        sources: [
          { kind: 'mailbox', label: 'user@example.test', detail: 'user@example.test / INBOX', observed_at: 1780100000 },
          { kind: 'mailbox', label: 'Personal Outlook', detail: 'archive@example.test / Receipts', observed_at: 1780150000 },
          { kind: 'upload', label: 'Admin', detail: 'bill.pdf', observed_at: 1780200000 },
        ],
        tags: [],
        correspondents: [],
        ...options.trashDocuments?.find(document => document.id === documentID),
        ...response?.document,
        ...(restoredDocuments.has(documentID) ? { trashed_at: null, deletes_at: null } : {}),
      }
    }
    else if (documentVersions) body = { results: [] }
    else if (similarDocuments) body = { results: [] }
    else if (documentAccess) body = { results: [], principals: [] }

    await route.fulfill({ json: body })
  })
}

test('guides first-time demo visitors and keeps the help launcher available', async ({ page }) => {
  await mockAPI(page, {
    demoMode: true,
    demoSession: 'anon',
    capabilities: [],
    chatEnabled: true,

    filingTreeChosen: true,
  })
  await page.goto('/#/dashboard')

  await expect(page).toHaveURL(/#\/demo$/)
  await expect(page.getByRole('heading', { name: 'From a precise search to dates with sources' })).toBeVisible()
  await expect(page.getByText('Rich query language', { exact: true })).toBeVisible()
  await expect(page.getByText('Archive research · Available on your own installation', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Ask the archive' })).toHaveCount(0)
  const queryLink = page.getByRole('link', { name: /Run the guided query/ })
  await expect(queryLink).toHaveAttribute('href', '#/search?q=from%3A%22Northstar%20Cloud%22%20tag%3Arenewal')

  await queryLink.click()
  await expect(page).toHaveURL(/#\/search\?q=from%3A%22Northstar%20Cloud%22%20tag%3Arenewal$/)
  await page.getByRole('link', { name: 'Open demo guide' }).click()
  await expect(page).toHaveURL(/#\/demo$/)

  await page.goto('/#/dashboard')
  await expect(page).toHaveURL(/#\/dashboard$/)
  await page.getByRole('link', { name: 'Open demo guide' }).click()
  await expect(page).toHaveURL(/#\/demo$/)
})

for (const demoSession of ['anon', 'scratch']) {
  test(`demo Calendar is browsable without model access (${demoSession})`, async ({ page }) => {
    const intelligenceQueries = []
    await mockAPI(page, {
      demoMode: true, demoSession, capabilities: [], chatEnabled: true,
      filingTreeChosen: true, intelligenceQueries,
      intelligence: ['2026-10-31', '2027-11-30'].map((date, index) => ({
        id: index + 1, document_id: 42, document_title: 'Northstar renewal',
        type: 'date', role: 'renewal', status: 'accepted', extractor: 'demo-corpus',
        value: { date, precision: 'day' }, sort_value: date, evidence_text: `Renews on ${date}`,
      })),
    })
    await page.goto('/#/demo')
    await page.getByRole('link', { name: 'Browse document dates' }).click()
    await expect(page.getByRole('heading', { name: 'Calendar', exact: true, level: 2 })).toBeVisible()
    await expect(page.getByRole('heading', { name: '2 dates', exact: true })).toBeVisible()
    await expect(page.getByText('Demo example', { exact: true })).toHaveCount(2)
    await expect(page.getByText(/LLM Classifier confidence/)).toHaveCount(0)
    await expect(page.getByRole('button', { name: 'Ask the archive' })).toHaveCount(0)
    expect(intelligenceQueries.at(-1)).not.toHaveProperty('sort_from')
    expect(intelligenceQueries.at(-1)).not.toHaveProperty('sort_to')
    await page.reload()
    await expect(page.getByRole('heading', { name: '2 dates', exact: true })).toBeVisible()
    await page.getByRole('link', { name: 'Northstar renewal', exact: true }).first().click()
    await expect(page).toHaveURL(/#\/doc\/42$/)
  })
}

test('keeps incomplete archive setup visible across navigation and reloads', async ({ page }) => {
  await mockAPI(page, { setupStartedAt: Math.floor(Date.now() / 1000) })
  await page.goto('/#/dashboard')

  const reminder = (page.viewportSize()?.width || 0) > 860
    ? page.locator('.sidebar .setup-reminder')
    : page.locator('.main > .setup-reminder-mobile')
  await expect(reminder.getByText('Choose your filing tree')).toBeVisible()
  await expect(reminder.getByText('Choose how documents are organized to finish archive setup.')).toBeVisible()
  await expect(reminder.getByRole('button', { name: 'Continue setup' })).toHaveAttribute('href', '#/settings?tab=archive&section=filing-tree')
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)

  if ((page.viewportSize()?.width || 0) > 860) {
    await expect(page.locator('.sidebar .setup-reminder')).toBeVisible()
  } else {
    const reminderBox = await reminder.boundingBox()
    const topbarBox = await page.locator('.topbar').boundingBox()
    expect(reminderBox.y).toBeGreaterThanOrEqual(topbarBox.y + topbarBox.height - 1)
  }

  await page.goto('/#/settings')
  const setupRow = page.locator('.settings-body .setup-reminder-settings')
  await expect(setupRow.getByText('Choose your filing tree')).toBeVisible()
  await expect(setupRow.getByRole('button', { name: 'Continue setup' })).toHaveAttribute('href', '#/settings?tab=archive&section=filing-tree')
  const setupActionClearance = await setupRow.evaluate(element => {
    const body = element.closest('.settings-body').getBoundingClientRect()
    const action = element.querySelector('[role="button"]').getBoundingClientRect()
    return body.right - action.right
  })
  expect(setupActionClearance).toBeGreaterThanOrEqual(20)
  await setupRow.getByRole('button', { name: 'Continue setup' }).click()
  await expect(page.getByRole('heading', { name: 'Filing tree', exact: true })).toBeVisible()
  const archiveLayout = await page.locator('.settings-body').evaluate(element => {
    const body = element.getBoundingClientRect()
    const slot = element.querySelector('.archive-slot').getBoundingClientRect()
    return { overflowY: getComputedStyle(element).overflowY, bodyBottom: body.bottom, slotBottom: slot.bottom }
  })
  expect(archiveLayout.overflowY).toBe('hidden')
  expect(archiveLayout.slotBottom).toBeLessThanOrEqual(archiveLayout.bodyBottom + 1)
  await page.goto('/#/dashboard')

  await expect(reminder).toBeVisible()
  await page.reload()
  await expect(reminder).toBeVisible()

  await page.goto('/#/settings')
  await expect(setupRow).toBeVisible()
  await page.getByRole('link', { name: 'Archive configuration', exact: true }).click()
  await expect(page.getByRole('region', { name: 'Archive configuration' })).toBeVisible()
})

test('shows the dashboard setup reminder only to incomplete admins', async ({ browser }) => {
  const cases = [
    { filingTreeChosen: true },
    { userRole: 'member' },
  ]
  for (const options of cases) {
    const context = await browser.newContext()
    const page = await context.newPage()
    await mockAPI(page, options)
    await page.goto('/#/dashboard')
    await expect(page.locator('.setup-reminder')).toHaveCount(0)
    await context.close()
  }
})

test('keeps setup-state failures visible and retries them', async ({ page }) => {
  const options = { failPaths: ['/api/admin/setup/state'], failureMessage: 'Setup status unavailable.' }
  await mockAPI(page, options)
  await page.goto('/#/dashboard')

  const reminder = (page.viewportSize()?.width || 0) > 860
    ? page.locator('.sidebar .setup-reminder')
    : page.locator('.main > .setup-reminder-mobile')
  await expect(reminder.getByText('Archive setup status unavailable')).toBeVisible()
  await expect(reminder.getByRole('button', { name: 'Continue setup' })).toBeVisible()

  options.failPaths.length = 0
  await reminder.getByRole('button', { name: 'Retry' }).click()
  await expect(reminder.getByText('Choose your filing tree')).toBeVisible()
})

test('does not recognize the removed setup route', async ({ page }) => {
  await mockAPI(page, { filingTreeChosen: true })
  await page.goto('/#/setup')

  await expect(page.getByText('Page not found.', { exact: true })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Archive configuration' })).toHaveCount(0)
})

test('opens Archive configuration after choosing a filing tree', async ({ page }) => {
  await mockAPI(page, {
    filingTreeChosen: true,
    currentPreset: 'solo',
  })
  await page.goto('/#/settings?tab=archive')

  await expect(page.locator('.setup-reminder')).toHaveCount(0)
  const configuration = page.getByRole('region', { name: 'Archive configuration' })
  await expect(configuration).toBeVisible()
  await expect(configuration.getByRole('link', { name: /Filing tree/ }).last()).toBeVisible()
  await expect(configuration.getByRole('link', { name: /Email intake/ }).last()).toBeVisible()
  await expect(configuration.getByRole('link', { name: /Classification/ }).last()).toBeVisible()
  await expect(configuration.getByRole('link', { name: /Folder layouts/ }).last()).toBeVisible()
  await expect(configuration.getByRole('link', { name: /OCR and backups/ }).last()).toBeVisible()
  const advanced = configuration.locator('.archive-rail .rail-group').filter({ hasText: 'Advanced' })
  await expect(advanced.getByRole('link').nth(0)).toHaveText('Folder layouts')
  await expect(advanced.getByRole('link').nth(1)).toHaveText('OCR and backups')
})

for (const userRole of ['admin', 'member']) {
  test(`shows the running build in ${userRole} settings`, async ({ page }) => {
    await mockAPI(page, {
      userRole, buildVersion: 'v0.1.0-beta.2', buildRevision: '1234567890ab',
      filingTreeChosen: true,
    })
    await page.goto('/#/settings')
    await expect(page.getByRole('contentinfo', { name: 'Suchi build' }))
      .toHaveText('Suchi v0.1.0-beta.2')
    await expect(page.getByText('1234567890ab', { exact: false })).toHaveCount(0)
    await page.route('**/api/whoami', route => route.fulfill({ json: {
      user_id: 1, email: 'admin@example.test', role: userRole, capabilities: [], build_version: 'dev',
    } }))
    await page.reload()
    await expect(page.getByRole('contentinfo', { name: 'Suchi build' })).toHaveText('Suchi dev · revision unavailable')
  })
}

test('keeps the running revision in Settings only', async ({ page }) => {
  await mockAPI(page, {
    buildVersion: 'v0.1.0-beta.2-dev', buildRevision: '1234567890ab.dirty',
    filingTreeChosen: true,
  })
  await page.goto('/#/dashboard')
  await expect(page.getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible()
  const navigation = page.getByRole('button', { name: 'Open navigation', exact: true })
  if (await navigation.isVisible()) await navigation.click()
  await expect(page.getByText('1234567890ab.dirty', { exact: false })).toHaveCount(0)
  await page.getByRole('link', { name: 'Profile & settings', exact: true }).click()
  await expect(page.getByRole('contentinfo', { name: 'Suchi build' }))
    .toHaveText('Suchi v0.1.0-beta.2-dev · 1234567890ab.dirty')
})

test('keeps archive layout and version footer fixed while scrolling', async ({ page }) => {
  await mockAPI(page, { buildVersion: 'dev', filingTreeChosen: true })
  const users = Array.from({ length: 12 }, (_, index) => ({
    id: index + 2, email: `layout-${index + 1}@example.test`,
    display_name: `Layout user ${index + 1}`, role: 'member', capabilities: [],
  }))
  await page.route('**/api/admin/users', route => route.fulfill({ json: { results: users } }))
  await page.goto('/#/settings?tab=archive&section=automations')
  const frame = page.getByRole('region', { name: 'Archive configuration' })
  const pane = frame.locator('.archive-content')
  await expect(frame.getByRole('button', { name: 'Open automations', exact: true })).toBeVisible()
  await expect(page.getByRole('contentinfo', { name: 'Suchi build' })).toBeVisible()
  await page.evaluate(() => document.fonts.ready)

  const measure = () => frame.evaluate(element => {
    const content = element.querySelector('.archive-content')
    const footer = document.querySelector('.build-info').getBoundingClientRect()
    const ancestors = []
    for (let parent = element.parentElement; parent; parent = parent.parentElement) ancestors.push(parent)
    return {
      frameHeight: element.getBoundingClientRect().height,
      footerY: footer.y,
      footerBottom: footer.bottom,
      navigationY: element.querySelector('.archive-sidebar').getBoundingClientRect().y,
      navigationScrollTop: element.querySelector('.archive-rail').scrollTop,
      paneScrollTop: content.scrollTop,
      paneOverflow: content.scrollHeight - content.clientHeight,
      outerScrollTops: ancestors.map(parent => parent.scrollTop),
      outerOverflow: Math.max(...ancestors.map(parent => parent.scrollHeight - parent.clientHeight)),
    }
  })
  const short = await measure()
  await frame.getByRole('navigation', { name: 'Archive settings sections' })
    .getByRole('link', { name: 'People', exact: true }).click()
  await expect(frame.getByText('Layout user 12', { exact: true })).toBeVisible()
  const long = await measure()
  expect(long.frameHeight).toBeCloseTo(short.frameHeight, 0)
  expect(long.footerY).toBeCloseTo(short.footerY, 0)
  expect(long.paneOverflow).toBeGreaterThan(0)
  expect(long.outerOverflow).toBeLessThanOrEqual(1)
  expect(long.outerScrollTops).toEqual(short.outerScrollTops)
  expect(long.footerBottom).toBeLessThanOrEqual(page.viewportSize().height)

  const box = await pane.boundingBox()
  await page.mouse.move(box.x + box.width - 24, box.y + box.height / 2)
  await page.mouse.wheel(0, 500)
  await expect.poll(async () => (await measure()).paneScrollTop).toBeGreaterThan(long.paneScrollTop)
  const scrolled = await measure()
  expect(scrolled.frameHeight).toBeCloseTo(short.frameHeight, 0)
  expect(scrolled.footerY).toBeCloseTo(short.footerY, 0)
  expect(scrolled.navigationY).toBeCloseTo(long.navigationY, 0)
  expect(scrolled.navigationScrollTop).toBe(long.navigationScrollTop)
  expect(scrolled.outerScrollTops).toEqual(long.outerScrollTops)
  expect(scrolled.outerOverflow).toBeLessThanOrEqual(1)
})

test('separates completed archive administration from account settings', async ({ page }) => {
  await mockAPI(page, {

    filingTreeChosen: true,
    currentPreset: 'household',
  })
  await page.goto('/#/settings')

  await expect(page.getByRole('navigation', { name: 'Settings areas' })).toBeVisible()
  await expect(page.locator('.sidebar .nav').getByRole('link', { name: 'Admin' })).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Archive configuration' })).toHaveCount(0)
  await expect(page.getByRole('heading', { name: 'Mailboxes' })).toHaveCount(0)
  await page.getByRole('link', { name: 'Archive configuration', exact: true }).click()

  await expect(page).toHaveURL(/#\/settings\?tab=archive$/)
  const configuration = page.getByRole('region', { name: 'Archive configuration' })
  await expect(configuration).toBeVisible()
  await expect(configuration.getByRole('heading', { name: 'Processing' })).toBeVisible()
  const filingTree = configuration.getByRole('link', { name: /Filing tree/ }).last()
  await expect(filingTree).toHaveAttribute('href', '#/settings?tab=archive&section=filing-tree')
  await filingTree.click()
  await expect(page).toHaveURL(/#\/settings\?tab=archive&section=filing-tree$/)
  await expect(configuration.getByRole('button', { name: 'Ready-made', exact: true })).toBeVisible()
  const administration = configuration.getByRole('link', { name: 'People', exact: true }).last()
  await administration.click()
  await expect(page).toHaveURL(/#\/settings\?tab=archive&section=users$/)
  await expect(configuration.getByRole('button', { name: 'Users', exact: true })).toBeVisible()
  await expect(configuration.getByRole('button', { name: 'Groups', exact: true })).toBeVisible()
  await expect(configuration.getByRole('button', { name: 'Metadata', exact: true })).toHaveCount(0)
  await expect(configuration.getByRole('link', { name: 'Metadata', exact: true }).last()).toHaveAttribute('href', '#/settings?tab=archive&section=metadata')
  await configuration.getByLabel('Configuration content', { exact: true }).getByRole('link', { name: 'Overview', exact: true }).click()
  await expect(page).toHaveURL(/#\/settings\?tab=archive$/)
  const automations = configuration.getByRole('link', { name: /Automations/ }).last()
  await expect(automations).toHaveAttribute('href', '#/settings?tab=archive&section=automations')
  await automations.click()
  await expect(page).toHaveURL(/#\/settings\?tab=archive&section=automations$/)
  const openAutomations = configuration.getByRole('button', { name: 'Open automations' })
  await expect(openAutomations).toHaveAttribute('href', '#/automations')
  await openAutomations.click()
  await expect(page).toHaveURL(/#\/automations$/)
  await page.goto('/#/settings?tab=archive')
  const classification = configuration.getByRole('link', { name: /Classification/ }).last()
  await expect(classification).toHaveAttribute('href', '#/settings?tab=archive&section=llm')
  await classification.click()

  await expect(page).toHaveURL(/#\/settings\?tab=archive&section=llm$/)
  await expect(page.getByRole('heading', { name: 'Suggestions', exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Set up model', exact: true }).first().click()
  await expect(page.getByRole('button', { name: 'Hosted endpoint' })).toBeVisible()
  await page.getByRole('button', { name: 'Cancel', exact: true }).click()
  const overviewReload = page.waitForRequest((request) =>
    new URL(request.url()).pathname === '/api/admin/settings/preferences'
  )
  await configuration.getByLabel('Configuration content', { exact: true }).getByRole('link', { name: 'Overview', exact: true }).click()
  await overviewReload
  await expect(page).toHaveURL(/#\/settings\?tab=archive$/)
})

test('saves application mode independently and retains only saved choices after reload', async ({ page }) => {
  const applicationModeRequests = []
  const llmSettingsRequests = []
  const llmTestRequests = []
  await mockAPI(page, {
    filingTreeChosen: true,
    applicationModeRequests, llmSettingsRequests, llmTestRequests,
  })
  await page.goto('/#/settings?tab=archive&section=llm')
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByRole('button', { name: 'Change', exact: true }).click()
  const applyAuto = page.getByRole('radio', { name: /Apply when confident/ })
  const applyReview = page.getByRole('radio', { name: /Review first/ })
  const localThreshold = page.getByLabel(/Minimum confidence to apply automatically/)
  const modelThreshold = page.getByLabel(/Minimum model confidence to apply automatically/)
  await expect(applyAuto).toBeChecked()
  await localThreshold.fill('0.95')
  await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await modelThreshold.fill('0.8')
  await page.getByLabel('Model', { exact: true }).fill('unsaved-model')
  await page.getByLabel('API key (blank for local)').fill('unsaved-key')
  await page.getByRole('button', { name: 'Cancel', exact: true }).click()
  await page.getByRole('radio', { name: /Detailed/ }).check()
  await applyReview.check()
  await expect(localThreshold).toBeDisabled()
  await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await expect(modelThreshold).toBeDisabled()
  if (await page.getByRole('dialog').count()) await page.getByRole('button', { name: 'Cancel', exact: true }).click()
  await page.getByRole('button', { name: 'Save application mode' }).click()
  await expect.poll(() => applicationModeRequests.length).toBe(1)
  await expect(page.getByText('Titles, dates, tags and Archive research. Off.', { exact: true })).toBeVisible()
  if (!await page.getByRole('dialog').count()) await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await expect(page.getByLabel('Model', { exact: true })).toHaveValue('unsaved-model')
  await expect(page.getByLabel('API key (blank for local)')).toHaveValue('unsaved-key')
  await expect(page.getByRole('radio', { name: /Detailed/ })).toBeChecked()
  await expect(localThreshold).toHaveValue('0.95')
  if (!await page.getByRole('dialog').count()) await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await expect(modelThreshold).toHaveValue('0.8')
  expect(llmSettingsRequests).toEqual([])
  expect(llmTestRequests).toEqual([])

  await page.reload()
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByRole('button', { name: 'Change', exact: true }).click()
  await expect(applyReview).toBeChecked()
  await expect(localThreshold).toBeDisabled()
  await expect(localThreshold).toHaveValue('0.9')
  if (!await page.getByRole('dialog').count()) await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await expect(modelThreshold).toHaveValue('0.7')
  if (!await page.getByRole('dialog').count()) await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await expect(page.getByLabel('Model', { exact: true })).toHaveValue('qwen2.5:7b')
  await expect(page.getByRole('radio', { name: /Balanced/ })).toBeChecked()
  if (await page.getByRole('dialog').count()) await page.getByRole('button', { name: 'Cancel', exact: true }).click()
  await applyAuto.check()
  if (await page.getByRole('dialog').count()) await page.getByRole('button', { name: 'Cancel', exact: true }).click()
  await page.getByRole('button', { name: 'Save application mode' }).click()
  await expect.poll(() => applicationModeRequests.length).toBe(2)
  await page.reload()
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByRole('button', { name: 'Change', exact: true }).click()
  await expect(applyAuto).toBeChecked()
  await expect(localThreshold).toBeEnabled()
})

test('keeps application mode editable after a failed save without changing persisted behavior', async ({ page }) => {
  await page.setViewportSize({ width: 360, height: 800 })
  await mockAPI(page, {
    filingTreeChosen: true, autoApply: false,
    applicationModeSaveFailure: true, failureMessage: 'application mode unavailable',
  })
  await page.goto('/#/settings?tab=archive&section=llm')
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByRole('button', { name: 'Change', exact: true }).click()
  const applyAuto = page.getByRole('radio', { name: /Apply when confident/ })
  const applyReview = page.getByRole('radio', { name: /Review first/ })
  await expect(applyReview).toBeChecked()
  if (await page.getByRole('dialog').count()) await page.getByRole('button', { name: 'Cancel', exact: true }).click()
  await applyAuto.check()
  if (await page.getByRole('dialog').count()) await page.getByRole('button', { name: 'Cancel', exact: true }).click()
  await page.getByRole('button', { name: 'Save application mode' }).click()
  await expect(page.getByText('application mode unavailable')).toBeVisible()
  await expect(applyAuto).toBeChecked()
  await expect(applyAuto).toBeEnabled()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  await page.reload()
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByRole('button', { name: 'Change', exact: true }).click()
  await expect(applyReview).toBeChecked()
})

test('model and matching saves preserve independent drafts and the persisted application mode', async ({ page }) => {
  const llmSettingsRequests = []
  const archiveMatchingRequests = []
  await mockAPI(page, { filingTreeChosen: true, llmSettingsRequests, archiveMatchingRequests })
  await page.goto('/#/settings?tab=archive&section=llm')
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByRole('button', { name: 'Change', exact: true }).click()
  const applyAuto = page.getByRole('radio', { name: /Apply when confident/ })
  const applyReview = page.getByRole('radio', { name: /Review first/ })
  const localThreshold = page.getByLabel(/Minimum confidence to apply automatically/)
  const modelThreshold = page.getByLabel(/Minimum model confidence to apply automatically/)
  await localThreshold.fill('0.95')
  await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await modelThreshold.fill('0.8')
  await page.getByRole('button', { name: 'Cancel', exact: true }).click()
  await page.getByRole('radio', { name: /Detailed/ }).check()
  await applyReview.check()
  await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await page.getByRole('button', { name: 'Test connection' }).click()
  await page.getByRole('button', { name: 'Enable model' }).click()
  await expect(page.getByText('Active', { exact: true })).toBeVisible()
  await expect(page.getByText('Model enabled', { exact: true })).toBeVisible()
  await expect(applyReview).toBeChecked()
  await expect(localThreshold).toHaveValue('0.95')
  await expect(page.getByRole('radio', { name: /Detailed/ })).toBeChecked()

  await page.getByRole('button', { name: 'Manage' }).click()
  await page.getByLabel('Model', { exact: true }).fill('unsaved-model')
  if (await page.getByRole('dialog').count()) await page.getByRole('button', { name: 'Cancel', exact: true }).click()
  await page.getByRole('button', { name: 'Save matching options' }).click()
  await expect.poll(() => archiveMatchingRequests.length).toBe(1)
  expect(llmSettingsRequests).toHaveLength(1)
  if (!await page.getByRole('dialog').count()) await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await expect(page.getByLabel('Model', { exact: true })).toHaveValue('unsaved-model')
  await expect(applyReview).toBeChecked()
  if (await page.getByRole('dialog').count()) await page.getByRole('button', { name: 'Cancel', exact: true }).click()
  await page.getByRole('button', { name: 'Save research context' }).click()
  await expect(page.getByText('Research context saved', { exact: true })).toBeVisible()
  await expect(applyReview).toBeChecked()
  await page.reload()
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByRole('button', { name: 'Change', exact: true }).click()
  await expect(applyAuto).toBeChecked()
  await expect(localThreshold).toHaveValue('0.95')
  if (!await page.getByRole('dialog').count()) await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await expect(modelThreshold).toHaveValue('0.8')
  await expect(page.getByRole('radio', { name: /Detailed/ })).toBeChecked()
  if (!await page.getByRole('dialog').count()) await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await expect(page.getByLabel('Model', { exact: true })).toHaveValue('qwen2.5:7b')
  await expect(page.getByText('Active', { exact: true })).toBeVisible()
})

test('keeps the similar-document switch contained and interactive', async ({ page }) => {
  await mockAPI(page, { filingTreeChosen: true })
  await page.goto('/#/settings?tab=archive&section=llm')
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByRole('button', { name: 'Change', exact: true }).click()

  const toggle = page.getByRole('switch', { name: 'Offer filing suggestions from similar documents' })
  await expect(toggle).toBeChecked()
  const box = await toggle.boundingBox()
  expect(Math.round(box.width)).toBe(38)
  expect(Math.round(box.height)).toBe(22)
  await toggle.click()
  await expect(toggle).not.toBeChecked()
})

test('separates model-free matching saves from validated model settings', async ({ page }) => {
  const llmSettingsRequests = []
  const archiveMatchingRequests = []
  await mockAPI(page, {

    filingTreeChosen: true,
    llmSettingsRequests, archiveMatchingRequests,
    llmHasAPIKey: true,
  })
  await page.goto('/#/settings?tab=archive&section=llm')
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByRole('button', { name: 'Change', exact: true }).click()

  const saveMatching = page.getByRole('button', { name: 'Save matching options' })
  await page.getByRole('switch', { name: 'Offer filing suggestions from similar documents' }).click()
  await saveMatching.click()
  await expect.poll(() => archiveMatchingRequests.length).toBe(1)
  expect(llmSettingsRequests).toEqual([])
  expect(archiveMatchingRequests[0]).toEqual({ archive_enabled: false, archive_review_threshold: 0.5, archive_auto_threshold: 0.9 })

  await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  const saveModel = page.getByRole('button', { name: 'Enable model' })
  const testConnection = page.getByRole('button', { name: 'Test connection' })
  await expect(saveModel).toBeDisabled()
  await testConnection.click()
  await expect(saveModel).toBeEnabled()

  await page.getByLabel('Endpoint URL').fill('http://localhost:11435/v1')
  await expect(saveModel).toBeDisabled()
  await testConnection.click()
  await expect(saveModel).toBeEnabled()
  await page.getByLabel('Model', { exact: true }).fill('qwen2.5:14b')
  await expect(saveModel).toBeDisabled()
  await testConnection.click()
  await expect(saveModel).toBeEnabled()
  await page.getByLabel('API key (blank for local)').fill('replacement-key')
  await expect(saveModel).toBeDisabled()
  await testConnection.click()
  await expect(saveModel).toBeEnabled()
  await page.getByLabel('Clear the saved API key when saving. Config-file and environment keys are unchanged.').check()
  await expect(saveModel).toBeDisabled()
  await page.getByRole('button', { name: 'Hosted endpoint' }).click()
  await page.getByLabel('Endpoint URL').fill('https://models.example.test/v1')
  await page.getByLabel('Model', { exact: true }).fill('hosted-model')
  const egress = page.getByLabel('This endpoint is not local. I acknowledge document text will leave this machine.')
  await egress.check()
  await testConnection.click()
  await expect(saveModel).toBeEnabled()
  await egress.uncheck()
  await expect(saveModel).toBeDisabled()
})

test('preserves an enabled model when saving archive matching', async ({ page }) => {
  const llmSettingsRequests = []
  const archiveMatchingRequests = []
  await mockAPI(page, {

    filingTreeChosen: true,
    llmSettingsRequests, archiveMatchingRequests,
    llmEnabled: true,
    llmActive: true,
    llmEndpoint: 'https://models.example.test/v1',
    llmModel: 'archive-model',
    llmEgressAck: true,
  })
  await page.goto('/#/settings?tab=archive&section=llm')
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByRole('button', { name: 'Change', exact: true }).click()

  await page.getByLabel(/Minimum confidence to suggest for review/).fill('0.9')
  await expect(page.getByRole('button', { name: 'Save matching options' })).toBeDisabled()
  await expect(page.getByRole('alert')).toBeVisible()
  await page.getByLabel(/Minimum confidence to suggest for review/).fill('0.85')
  if (await page.getByRole('dialog').count()) await page.getByRole('button', { name: 'Cancel', exact: true }).click()
  await page.getByRole('button', { name: 'Save matching options' }).click()
  await expect.poll(() => archiveMatchingRequests.length).toBe(1)
  expect(llmSettingsRequests).toEqual([])
  await page.reload()
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByRole('button', { name: 'Change', exact: true }).click()
  await expect(page.getByLabel(/Minimum confidence to suggest for review/)).toHaveValue('0.85')
  await page.getByRole('button', { name: 'Manage', exact: true }).click()
  await expect(page.getByLabel('Endpoint URL')).toHaveValue('https://models.example.test/v1')
  if (!await page.getByRole('dialog').count()) await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await expect(page.getByLabel('Model', { exact: true })).toHaveValue('archive-model')
  await expect(page.getByLabel('This endpoint is not local. I acknowledge document text will leave this machine.')).toBeChecked()
  await expect(page.getByText('Active', { exact: true })).toBeVisible()
})

test('persists research context independently without enabling the model', async ({ page }) => {
  const researchContextRequests = []
  const llmSettingsRequests = []
  await mockAPI(page, {

    filingTreeChosen: true,
    researchContextRequests,
    llmSettingsRequests,
  })
  await page.goto('/#/settings?tab=archive&section=llm')
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByRole('button', { name: 'Change', exact: true }).click()

  const research = page.getByRole('region', { name: 'Archive research configuration' })
  await expect(research.getByRole('radio', { name: /Balanced/ })).toBeChecked()

  await research.getByRole('radio', { name: /Detailed/ }).check()
  await research.getByRole('button', { name: 'Save research context' }).click()
  await expect.poll(() => researchContextRequests.length).toBe(1)
  await page.reload()
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByRole('button', { name: 'Change', exact: true }).click()
  await expect(research.getByRole('radio', { name: /Detailed/ })).toBeChecked()
  await expect(page.getByText('Titles, dates, tags and Archive research. Off.', { exact: true })).toBeVisible()
  expect(llmSettingsRequests).toEqual([])
})

test('keeps research context out of model test, save, and disable payloads', async ({ page }) => {
  const llmSettingsRequests = []
  const llmTestRequests = []
  await mockAPI(page, {

    filingTreeChosen: true,
    llmSettingsRequests,
    llmTestRequests,
  })
  await page.goto('/#/settings?tab=archive&section=llm')
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByRole('button', { name: 'Change', exact: true }).click()

  await page.getByRole('radio', { name: /Detailed/ }).check()
  await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await page.getByRole('button', { name: 'Test connection' }).click()
  await expect.poll(() => llmTestRequests.length).toBe(1)


  await page.getByRole('button', { name: 'Enable model' }).click()
  await expect.poll(() => llmSettingsRequests.length).toBe(1)
  expect(llmSettingsRequests[0].enabled).toBe(true)
  await expect(page.getByRole('radio', { name: /Detailed/ })).toBeChecked()
  await expect(page.getByText('Active', { exact: true })).toBeVisible()

  await page.getByRole('button', { name: 'Manage', exact: true }).click()
  await page.getByRole('button', { name: 'Disable model' }).click()
  await expect.poll(() => llmSettingsRequests.length).toBe(2)
  expect(llmSettingsRequests[1].enabled).toBe(false)
  await expect(page.getByRole('radio', { name: /Detailed/ })).toBeChecked()
  await expect(page.getByText('Titles, dates, tags and Archive research. Off.', { exact: true })).toBeVisible()
  for (const payload of [...llmTestRequests, ...llmSettingsRequests]) {
    expect(payload).not.toHaveProperty('research_context_mode')
  }
  await page.reload()
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByRole('button', { name: 'Change', exact: true }).click()
  await expect(page.getByRole('radio', { name: /Balanced/ })).toBeChecked()
})

test('keeps research context editable when its standalone save fails', async ({ page }) => {
  await mockAPI(page, {

    filingTreeChosen: true,
    researchContextMode: 'unexpected',
    researchContextSaveFailure: true,
    failureMessage: 'research context unavailable',
  })
  await page.goto('/#/settings?tab=archive&section=llm')
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByRole('button', { name: 'Change', exact: true }).click()

  const research = page.getByRole('region', { name: 'Archive research configuration' })
  await expect(research.getByRole('radio', { name: /Balanced/ })).toBeChecked()
  await research.getByRole('radio', { name: /Detailed/ }).check()
  await research.getByRole('button', { name: 'Save research context' }).click()
  await expect(page.getByText('research context unavailable')).toBeVisible()
  await expect(research.getByRole('radio', { name: /Detailed/ })).toBeChecked()
})

test('mounts only the selected settings surface', async ({ page }) => {
  const requestedPaths = []
  page.on('request', request => requestedPaths.push(new URL(request.url()).pathname))
  await mockAPI(page, {

    filingTreeChosen: true,
  })
  await page.goto('/#/settings?tab=archive&section=users')

  await expect(page.getByRole('button', { name: 'Users', exact: true })).toBeVisible()
  expect(requestedPaths).not.toContain('/api/tokens/')
  expect(requestedPaths).not.toContain('/api/decryption-passwords/')
  expect(requestedPaths).not.toContain('/api/groups/')
  expect(requestedPaths).not.toContain('/api/custom_fields/')
  expect(requestedPaths).not.toContain('/api/tags/')

  const groupsRequest = page.waitForRequest(request => new URL(request.url()).pathname === '/api/groups/')
  await page.getByRole('button', { name: 'Groups', exact: true }).click()
  await groupsRequest
  await expect(page.getByText(/No groups yet/)).toBeVisible()
  expect(requestedPaths).not.toContain('/api/custom_fields/')
  expect(requestedPaths).not.toContain('/api/tags/')
})

test('refreshes intake owners after user creation in Archive configuration', async ({ page }) => {
  await mockAPI(page, { filingTreeChosen: true })
  const users = [{ id: 1, email: 'admin@example.test', display_name: 'Admin', role: 'admin' }]
  await page.route('**/api/admin/users', async route => {
    if (route.request().method() === 'POST') {
      users.push({ ...route.request().postDataJSON(), id: 2 })
      return route.fulfill({ json: { id: 2 } })
    }
    return route.fulfill({ json: { results: users } })
  })
  await page.goto('/#/settings?tab=archive&section=sources')
  const owner = page.getByRole('combobox', { name: 'Documents from it belong to', exact: true })
  await expect(owner).toHaveValue('admin@example.test')
  await page.getByRole('link', { name: 'People', exact: true }).click()
  const form = page.getByRole('form', { name: 'Create a user', exact: true })
  await form.getByLabel('Email', { exact: true }).fill('morgan@example.test')
  await form.getByLabel('Display name', { exact: true }).fill('Morgan')
  await form.getByLabel('Password', { exact: true }).fill('safe-test-password')
  await form.getByRole('button', { name: 'Create user', exact: true }).click()
  await page.getByRole('link', { name: 'Watched folder', exact: true }).click()
  await expect(owner.locator('option[value="morgan@example.test"]')).toHaveCount(1)
  await expect(owner).toHaveValue('admin@example.test')
})

test('keeps failed configuration reads out of editable forms', async ({ page }) => {
  await mockAPI(page, {

    filingTreeChosen: true,
    failPaths: ['/api/admin/settings/llm'],
    failureMessage: 'classification settings unavailable',
  })
  await page.goto('/#/settings?tab=archive&section=llm')

  await expect(page.getByText('classification settings unavailable')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Retry' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Enable model' })).toHaveCount(0)
  await expect(page.getByRole('radio', { name: /Apply when confident/ })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Save application mode' })).toHaveCount(0)
})

test('distinguishes mailbox and saved-view failures from empty data', async ({ page }) => {
  await mockAPI(page, {
    filingTreeChosen: true,
    failPaths: ['/api/email-accounts', '/api/saved_views/'],
    failureMessage: 'archive data unavailable',
  })
  await page.goto('/#/settings?tab=archive&section=mail')
  await expect(page.getByText('archive data unavailable')).toBeVisible()
  await expect(page.getByText('No mailboxes connected.')).toHaveCount(0)

  await page.evaluate(() => { location.hash = '#/views' })
  await expect(page.getByText('Could not load saved views', { exact: true })).toBeVisible()
  await expect(page.getByText('No saved views yet')).toHaveCount(0)
})

test('distinguishes a recent-document failure from an empty archive', async ({ page }) => {
  await mockAPI(page, {
    failPaths: ['/api/documents/'],
    failureMessage: 'recent documents unavailable',
  })
  await page.goto('/#/dashboard')

  await expect(page.getByText('recent documents unavailable')).toBeVisible()
  await expect(page.getByText('Nothing here yet.')).toHaveCount(0)
})

test('does not report healthy pipeline metrics when status fails', async ({ page }) => {
  await mockAPI(page, {
    failPaths: ['/api/stats/'],
    failureMessage: 'archive status unavailable',
  })
  await page.goto('/#/dashboard')

  await expect(page.locator('.metrics').getByText('archive status unavailable')).toHaveCount(2)
  await expect(page.getByText('pipeline healthy')).toHaveCount(0)
  await expect(page.getByText('none pending')).toHaveCount(0)
})

test('keeps capable member mailboxes in account settings', async ({ page }) => {
  await mockAPI(page, { userRole: 'member', capabilities: ['mailboxes'] })
  await page.goto('/#/settings')
  await expect(page.getByRole('heading', { name: 'Mailboxes' })).toBeVisible()
})

test('offers ready-made trees, focused sets, and file tools without blocking other settings', async ({ page }) => {
  await mockAPI(page)
  await page.goto('/#/settings?tab=archive&section=filing-tree')

  await expect(page.getByRole('heading', { name: 'Filing tree', exact: true })).toBeVisible()
  const filingTrees = page.locator('.preset-grid')
  await expect(filingTrees.getByText('Household', { exact: true })).toBeVisible()
  await expect(filingTrees.getByText('Blank', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Build your own' }).click()
  await expect(page.getByRole('button', { name: /Life admin/ })).toBeVisible()
  await expect(page.getByText('Choose at most one set in each numbered lane.')).toBeVisible()
  await page.getByRole('button', { name: 'Import or export' }).click()
  await expect(page.getByRole('button', { name: 'Folder layouts' })).toHaveCount(0)
  await expect(page.getByRole('heading', { name: 'Import a taxonomy file' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Export this filing tree' })).toBeVisible()
  await page.getByRole('link', { name: 'Classification', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Suggestions' })).toBeVisible()
  await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await expect(page.getByRole('button', { name: 'Local model', exact: true })).toBeVisible()
})

test('creates, previews, and deletes Advanced folder layouts', async ({ page }) => {
  await mockAPI(page, {
    renderedLayouts: [{
      id: 4, name: 'Bills by year',
      path: 'Bills/{{ created_year }}/{{ title }}', uses_asn: false,
    }],
  })
  await page.goto('/#/settings?tab=archive&section=folder-layouts')

  await expect(page.getByRole('heading', { name: 'Folder layouts' })).toBeVisible()
  await expect(page.getByText('Bills by year', { exact: true })).toBeVisible()
  await expect(page.getByText('10-19 Home/13 Utilities', { exact: true })).toBeVisible()

  await page.getByLabel('Layout name').fill('Imported folders')
  await page.getByLabel('Path template').fill('Legacy/{{ asn }}/{{ title }}')
  await expect(page.getByText('Legacy/4021/March electricity bill', { exact: true })).toBeVisible()
  await expect(page.getByText('Uses previous archive number', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Create layout' }).click()

  const saved = page.getByRole('region', { name: 'Saved layouts' })
  await expect(saved.getByText('Imported folders', { exact: true })).toBeVisible()
  const imported = saved.locator('.layout-row').filter({ hasText: 'Imported folders' })
  await imported.getByRole('button', { name: 'Delete' }).click()
  const confirmation = page.getByRole('alertdialog', { name: 'Delete folder layout?' })
  await expect(confirmation).toBeVisible()
  await confirmation.getByRole('button', { name: 'Delete layout' }).click()
  await expect(saved.getByText('Imported folders', { exact: true })).toHaveCount(0)
})

test('centers custom automations ahead of collapsed filing-tree rules', async ({ page }) => {
  await mockAPI(page, {
    automations: [
      {
        id: 18, name: 'Route tax records', enabled: true, order: 0,
        triggers: [{ type: 2 }],
        actions: [{ id: 1, type: 'assign_tags', params: { tag_ids: [] } }],
      },
      {
        id: 19, name: 'Built-in utility filing', enabled: true, order: 1, preset_slug: 'household',
        triggers: [{ type: 2 }],
        actions: [{ id: 2, type: 'assign_tags', params: { tag_ids: [] } }],
      },
    ],
  })
  await page.goto('/#/automations')

  await expect(page.getByRole('button', { name: 'New automation' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Your automations' })).toBeVisible()
  await expect(page.getByText('Route tax records', { exact: true })).toBeVisible()
  await expect(page.locator('#automation-19')).toHaveCount(0)

  const customPanel = page.locator('.automations-panel')
  await expect(customPanel.locator('.panel-head .chip')).toHaveText('1')
  const builtIns = page.locator('.builtins')
  await expect.poll(async () => {
    const [customBox, builtInBox] = await Promise.all([customPanel.boundingBox(), builtIns.boundingBox()])
    return customBox && builtInBox ? builtInBox.y > customBox.y + customBox.height : false
  }).toBe(true)

  await page.getByRole('button', { name: /Built-in automations/ }).click()
  await expect(page.locator('#automation-19 .title').filter({ hasText: 'Built-in utility filing' })).toBeVisible()
})

test('uses named folder-layout and typed custom-field automation controls', async ({ page }) => {
  await mockAPI(page, {
    renderedLayouts: [{ id: 4, name: 'Bills by year', path: 'Bills/{{ created_year }}/{{ title }}', uses_asn: false }],
    customFields: [
      { id: 12, name: 'Review status', data_type: 'select', extra_data: JSON.stringify({ choices: ['Needs review', 'Approved'] }) },
      { id: 13, name: 'Invoice amount', data_type: 'number', extra_data: '{}' },
    ],
    automationActions: [
      { kind: 'assign_storage_path', name: 'Assign folder layout', params: [{ name: 'storage_path_id', type: 'id', target_kind: 'rendered_layout' }] },
      { kind: 'assign_owner', name: 'Assign owner', params: [{ name: 'owner_id', type: 'id', target_kind: 'user' }] },
      { kind: 'assign_custom_field', name: 'Set custom field', params: [{ name: 'field_id', type: 'id', target_kind: 'custom_field' }, { name: 'value', type: 'value' }] },
    ],
    automations: [{
      id: 19, name: 'Prepare imported bills', enabled: true, order: 0,
      triggers: [{ type: 2 }],
      actions: [
        { id: 1, type: 'assign_storage_path', params: { storage_path_id: 4 } },
        { id: 2, type: 'assign_custom_field', params: { field_id: 12, value: 'Needs review' } },
        { id: 3, type: 'assign_owner', params: { owner_id: 2 } },
      ],
    }],
  })
  await page.goto('/#/automations')

  const row = page.locator('#automation-19')
  await expect(row.getByText('Assign folder layout → Bills by year', { exact: true })).toBeVisible()
  await expect(row.getByText('Set Review status → Needs review', { exact: true })).toBeVisible()
  await expect(row.getByText('Set owner → Member · member@example.test', { exact: true })).toBeVisible()
  await row.getByRole('button', { name: 'Edit' }).click()

  await expect(page.getByRole('combobox', { name: 'Folder layout' })).toHaveValue('4')
  await expect(page.getByRole('combobox', { name: 'Owner' })).toHaveValue('2')
  const fields = page.getByRole('combobox', { name: 'Custom field', exact: true })
  await expect(fields).toHaveValue('12')
  await expect(page.getByRole('combobox', { name: 'Custom field value' })).toHaveValue('Needs review')
  await fields.selectOption('13')
  await expect(page.getByRole('spinbutton', { name: 'Custom field value' })).toBeVisible()
})

test('reviews category mappings and applies the operator choice', async ({ page }) => {
  const requests = []
  page.on('request', request => {
    const path = new URL(request.url()).pathname
    if (path.startsWith('/api/admin/setup/preset/')) requests.push({ path, body: request.postDataJSON() })
  })
  await mockAPI(page, {
    filingTreeChosen: true,
    currentPreset: 'freelance',
    currentSetIDs: presets.find(preset => preset.id === 'freelance').set_ids,
    presetCollisions: [{
      code: 22, existing: 'Briefs & plans', incoming: 'Orders & contracts',
      proposed_code: 26, resolved: false, suggested_replace: true,
      live_documents: 4, trashed_documents: 1,
    }],
  })
  await page.goto('/#/settings?tab=archive&section=filing-tree')
  await page.locator('.preset-grid').getByText('Small business', { exact: true }).click()
  await page.getByRole('button', { name: 'Review change' }).click()

  const review = page.locator('.review')
  await expect(review).toBeFocused()
  await expect(review.locator('.hash')).toHaveCount(0)
  await expect.poll(() => review.evaluate(element => {
    const top = element.getBoundingClientRect().top
    return top >= 0 && top < window.innerHeight
  })).toBe(true)

  const mapping = page.getByRole('group', { name: /22.*Briefs & plans.*Orders & contracts/ })
  await expect(mapping.getByText('4 live · 1 in Trash')).toBeVisible()
  await expect(mapping.getByRole('radio', { name: /Replace meaning in place.*recommended/ })).toBeChecked()
  await mapping.getByRole('radio', { name: /Add as 26/ }).check()
  await page.getByRole('button', { name: 'Apply reviewed change' }).click()
  await expect(page.getByText('Filing tree updated.', { exact: false })).toBeVisible()

  const apply = requests.find(request => request.path.endsWith('/apply'))
  expect(apply.body.remaps).toEqual({ 22: 26 })
})

test('requires review and a successful apply before Blank satisfies archive setup', async ({ page }) => {
  const options = {
    failPaths: ['/api/admin/setup/preset/apply'],
    failureMessage: 'Could not apply the filing tree.',
  }
  await mockAPI(page, options)
  const openBlankReview = async () => {
    await page.goto('/#/settings?tab=archive&section=filing-tree')
    await page.locator('.preset-grid').getByText('Blank', { exact: true }).click()
    const review = page.getByRole('button', { name: 'Review change' })
    await expect(review).toBeDisabled()
    await page.getByLabel('I understand that documents will remain in Inbox until I add categories.').check()
    await review.click()
    return page.getByRole('button', { name: 'Apply reviewed change' })
  }

  await (await openBlankReview()).click()
  await expect(page.getByText(options.failureMessage, { exact: true })).toBeVisible()
  await page.goto('/#/dashboard')
  await expect(page.locator('.setup-reminder').filter({ visible: true })).toHaveCount(1)

  options.failPaths.length = 0
  await (await openBlankReview()).click()
  await expect(page.getByText('Filing tree updated.', { exact: false })).toBeVisible()
  await page.goto('/#/dashboard')
  await expect(page.locator('.setup-reminder')).toHaveCount(0)
})

test('retries filing-tree workspace loading without hiding other sections', async ({ page }) => {
  const options = {
    failPaths: ['/api/admin/setup/state'],
    failureMessage: 'Filing-tree settings unavailable.',
  }
  await mockAPI(page, options)
  await page.goto('/#/settings?tab=archive&section=filing-tree')
  await expect(page.getByText(options.failureMessage, { exact: true })).toBeVisible()
  await expect(page.getByRole('link', { name: 'People', exact: true })).toBeVisible()
  await expect(page.getByRole('link', { name: 'Metadata', exact: true })).toBeVisible()

  options.failPaths.length = 0
  await page.getByRole('region', { name: 'Configuration content' })
    .getByRole('button', { name: 'Retry', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Ready-made', exact: true })).toBeVisible()
})

test('keeps the filing index neutral until a preset is applied', async ({ page }) => {
  const inbox = {
    id: 49, code: 49, name: 'Inbox', area_code: 40, area_name: 'System', system: true,
  }
  await mockAPI(page, {
    jdCategories: [inbox],
    jdCategoriesAfterPreset: [
      { id: 11, code: 11, name: 'Identity', area_code: 10, area_name: 'Life admin' },
      inbox,
    ],
  })
  await page.goto('/#/dashboard')

  const indexHeading = page.locator('.side-head').filter({ hasText: /^Index$/ })
  await expect(indexHeading).toHaveCount(0)
  await expect(page.locator('.nav a[href="#/inbox"]')).toHaveCount(1)

  await page.goto('/#/settings?tab=archive&section=filing-tree')
  await page.getByRole('button', { name: 'Review change' }).click()
  await page.getByRole('button', { name: 'Apply reviewed change' }).click()

  await page.goto('/#/dashboard')
  await expect(page.locator('.setup-reminder')).toHaveCount(0)
  await expect(indexHeading).toHaveCount(1)
  if ((page.viewportSize()?.width || 0) <= 860) {
    await page.getByRole('button', { name: 'Open navigation' }).click()
  }
  await page.locator('.area-toggle').filter({ hasText: 'Life admin' }).click()
  await expect(page.locator('a[href="#/documents?jd=11"]')).toContainText('Identity')
  await expect(page.locator('.jd-tree a[href="#/documents?jd=49"]')).toHaveCount(0)

  await page.goto('/#/settings')
  await expect(page.locator('.setup-reminder')).toHaveCount(0)
  await page.getByRole('link', { name: 'Archive configuration', exact: true }).click()
  await expect(page.getByRole('region', { name: 'Archive configuration' })).toBeVisible()
})

test('uses the demo category database id in document links', async ({ page }) => {
  await mockAPI(page, {
    jdCategories: [{
      id: 6, code: 22, name: 'Money and tax', area_code: 20, area_name: 'Money',
    }],
  })
  await page.goto('/#/demo')

  const link = page.getByRole('link', { name: /Browse the Johnny Decimal tree/ })
  await expect(link).toHaveAttribute('href', '#/documents?jd=6')
})

test('does not request a document for an invalid detail route', async ({ page }) => {
  const invalidRequests = []
  page.on('request', request => {
    if (new URL(request.url()).pathname.includes('/api/documents/not-a-number')) {
      invalidRequests.push(request.url())
    }
  })
  await mockAPI(page)
  await page.goto('/#/doc/not-a-number')

  await expect(page.getByText('Page not found.')).toBeVisible()
  expect(invalidRequests).toEqual([])
})

test('does not show all documents when the inbox category is unavailable', async ({ page }) => {
  const documentRequests = []
  page.on('request', request => {
    if (new URL(request.url()).pathname === '/api/documents/') documentRequests.push(request.url())
  })
  await mockAPI(page, { taxonomyFailure: true })
  await page.goto('/#/inbox')

  await expect(page.getByText('The inbox is unavailable.')).toBeVisible()
  expect(documentRequests).toEqual([])
})

test('loads the filing tree once for every archive screen', async ({ page }) => {
  let taxonomyRequests = 0
  page.on('request', request => {
    if (new URL(request.url()).pathname === '/api/jd/categories/') taxonomyRequests++
  })
  await mockAPI(page, {
    jdCategories: [{ id: 6, area_code: 20, area_name: 'Money', code: 22, name: 'Investments' }],
  })

  await page.goto('/#/documents')
  await expect(page.getByText('No documents match.')).toBeVisible()
  await page.evaluate(() => { location.hash = '#/views' })
  await expect(page.getByRole('heading', { name: 'Shortcuts into the archive' })).toBeVisible()
  await page.evaluate(() => { location.hash = '#/automations' })
  await expect(page.getByText('Tag utility bills')).toBeVisible()
  await page.evaluate(() => { location.hash = '#/upload' })
  await expect(page.getByText('Drop documents here')).toBeVisible()
  await page.evaluate(() => { location.hash = '#/doc/42' })
  await expect(page.getByRole('heading', { name: 'Electricity bill' })).toBeVisible()

  expect(taxonomyRequests).toBe(1)
})

test('keeps the newest document filter response', async ({ page }) => {
  await mockAPI(page, {
    documentsByQuery: {
      slow: {
        delay: 300,
        documents: [{ id: 41, title: 'Old response', created_at: 1780000000, tags: [] }],
      },
      fast: {
        documents: [{ id: 42, title: 'Current response', created_at: 1780000000, tags: [] }],
      },
    },
  })

  const slowRequest = page.waitForRequest(request => {
    const url = new URL(request.url())
    return url.pathname === '/api/documents/' && url.searchParams.get('q') === 'slow'
  })
  await page.goto('/#/documents?q=slow')
  await slowRequest
  await page.evaluate(() => { location.hash = '#/documents?q=fast' })

  await expect(page.getByText('Current response')).toBeVisible()
  await page.waitForTimeout(350)
  await expect(page.getByText('Old response')).toHaveCount(0)
})

test('keeps the newest document detail response', async ({ page }) => {
  await mockAPI(page, {
    documentDetails: {
      42: { delay: 300, document: { title: 'Old detail' } },
      43: { document: { title: 'Current detail' } },
    },
  })

  const oldRequest = page.waitForRequest(request => new URL(request.url()).pathname === '/api/documents/42')
  await page.goto('/#/doc/42')
  await oldRequest
  await page.evaluate(() => { location.hash = '#/doc/43' })

  await expect(page.getByRole('heading', { name: 'Current detail' })).toBeVisible()
  await page.waitForTimeout(350)
  await expect(page.getByRole('heading', { name: 'Old detail' })).toHaveCount(0)
})

test('runs a changed search once and keeps its newest response', async ({ page }) => {
  const queries = []
  page.on('request', request => {
    const url = new URL(request.url())
    if (url.pathname === '/api/search/') queries.push(url.searchParams.get('q'))
  })
  await mockAPI(page, {
    searchByQuery: {
      paris: {
        delay: 300,
        results: [{ id: 41, title: 'Old Paris result', created_at: 1780000000 }],
      },
      london: {
        results: [{ id: 42, title: 'Current London result', created_at: 1780000000 }],
      },
    },
  })

  const firstRequest = page.waitForRequest(request => {
    const url = new URL(request.url())
    return url.pathname === '/api/search/' && url.searchParams.get('q') === 'paris'
  })
  await page.goto('/#/search?q=paris')
  await firstRequest
  await page.getByPlaceholder('Search text or use jd:, tag:, from:…').fill('london')
  await page.getByRole('button', { name: 'Search', exact: true }).click()

  await expect(page.getByText('Current London result')).toBeVisible()
  await page.waitForTimeout(350)
  await expect(page.getByText('Old Paris result')).toHaveCount(0)
  expect(queries).toEqual(['paris', 'london'])
})

test('clearing an applied search cancels the request and leaves search usable', async ({ page }) => {
  await mockAPI(page, {
    searchByQuery: {
      paris: {
        delay: 1000,
        results: [{ id: 41, title: 'Canceled Paris result', created_at: 1780000000 }],
      },
      london: {
        results: [{ id: 42, title: 'Current London result', created_at: 1780000000 }],
      },
    },
  })
  const firstRequest = page.waitForRequest(request => {
    const url = new URL(request.url())
    return url.pathname === '/api/search/' && url.searchParams.get('q') === 'paris'
  })
  await page.goto('/#/search?q=paris')
  const request = await firstRequest
  const canceled = page.waitForEvent('requestfailed', failed => failed === request)
  const input = page.getByPlaceholder('Search text or use jd:, tag:, from:…')
  const submit = page.getByRole('button', { name: 'Search', exact: true })
  await input.fill('')
  await submit.click()
  await canceled
  await expect(page).toHaveURL(/#\/search$/)
  await expect(page.getByText('Canceled Paris result')).toHaveCount(0)

  await submit.click()
  await input.fill('london')
  await submit.click()
  await expect(page.getByText('Current London result')).toBeVisible()
})


test('resets document pagination when route filters change', async ({ page }) => {
  await mockAPI(page, {
    documentsCount: 100,
    documents: [{ id: 42, title: 'Electricity bill', created_at: 1780000000, tags: [] }],
  })
  await page.goto('/#/documents')

  const secondPage = page.waitForRequest(request => {
    const url = new URL(request.url())
    return url.pathname === '/api/documents/' && url.searchParams.get('page') === '2'
  })
  await page.getByRole('button', { name: /Next/ }).click()
  await secondPage

  const filteredFirstPage = page.waitForRequest(request => {
    const url = new URL(request.url())
    return url.pathname === '/api/documents/' &&
      url.searchParams.get('page') === '1' && url.searchParams.get('jd_category_id') === '6'
  })
  await page.evaluate(() => { location.hash = '#/documents?jd=6' })
  await filteredFirstPage
  await expect(page.getByText(/Page 1 of 2/)).toBeVisible()
})

test('filters Documents by active share links', async ({ page }) => {
  await mockAPI(page, {
    documents: [{ id: 42, title: 'Shared receipt', created_at: 1780000000, tags: [] }],
  })
  await page.goto('/#/documents?q=receipt&page=2')

  const filtered = page.waitForRequest(request => {
    const url = new URL(request.url())
    return url.pathname === '/api/documents/' &&
      url.searchParams.get('q') === 'receipt' &&
      url.searchParams.get('share_link') === 'active'
  })
  await page.getByRole('button', { name: 'Shared by me' }).click()
  await filtered
  await expect(page).toHaveURL(/#\/documents\?q=receipt&share_link=active$/)

  await page.getByRole('button', { name: 'Shared by me' }).click()
  await expect(page).toHaveURL(/#\/documents\?q=receipt$/)
})

test('keeps full-width search above one wide-screen options row', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop', 'wide-screen layout regression')
  await page.setViewportSize({ width: 1568, height: 720 })
  await mockAPI(page)
  await page.goto('/#/documents')

  const searchRow = page.getByRole('group', { name: 'Document search' })
  const optionsRow = page.getByRole('group', { name: 'Document options' })
  const displayActions = optionsRow.getByRole('group', { name: 'Document display' })
  const search = searchRow.locator('input[type="search"]')
  const sort = displayActions.getByRole('combobox', { name: 'Sort documents' })
  const viewToggle = displayActions.locator('.seg')
  const refresh = displayActions.getByRole('button', { name: 'Refresh documents' })
  const sharedByMe = optionsRow.getByRole('button', { name: 'Shared by me' })
  const dateRange = optionsRow.getByRole('button', { name: 'Any date', exact: true })
  const heights = await Promise.all([search, sort, viewToggle, refresh, sharedByMe, dateRange]
    .map(async selector => Math.round((await selector.boundingBox()).height)))
  expect(new Set(heights).size).toBe(1)
  await expect(page.getByRole('link', { name: 'Manage tags' })).toHaveCount(0)

  const [searchBox, searchRowBox, firstFilter, dateBox, viewBox, refreshBox, sortBox] = await Promise.all([
    search.boundingBox(),
    searchRow.boundingBox(),
    optionsRow.getByRole('combobox').first().boundingBox(),
    dateRange.boundingBox(),
    viewToggle.boundingBox(),
    refresh.boundingBox(),
    sort.boundingBox(),
  ])
  expect(Math.abs(searchBox.width - searchRowBox.width)).toBeLessThanOrEqual(1)
  const centers = [firstFilter, dateBox, viewBox, refreshBox, sortBox]
    .map(box => box.y + box.height / 2)
  expect(Math.max(...centers) - Math.min(...centers)).toBeLessThanOrEqual(1)
  expect(viewBox.x - dateBox.x - dateBox.width).toBeGreaterThan(24)
  expect(refreshBox.x).toBeGreaterThan(viewBox.x)
  expect(sortBox.x).toBeGreaterThan(refreshBox.x)
  expect(firstFilter.y).toBeGreaterThan(searchBox.y)
})

test('document pagination survives detail navigation, history and reload', async ({ page }, testInfo) => {
  await mockAPI(page, { documentsCount: 743 })
  await page.route('**/api/documents/?*', route => {
    const params = new URL(route.request().url()).searchParams
    const current = Number(params.get('page') || 1)
    return route.fulfill({ json: {
      count: 743,
      results: [{ id: 42, title: `Receipt on page ${current}`, created_at: 1780000000, tags: [] }],
    } })
  })
  await page.goto('/#/documents?q=receipt&ordering=title')
  const pager = page.getByRole('navigation', { name: 'Document pages' })
  await pager.getByRole('link', { name: 'Page 3', exact: true }).click()
  await expect(page).toHaveURL(/q=receipt&ordering=title&page=3$/)
  await expect(page.getByText('Receipt on page 3', { exact: true })).toBeVisible()
  const documentLink = page.getByText('Receipt on page 3', { exact: true })
  if (testInfo.project.name === 'mobile') await documentLink.tap()
  else await documentLink.click()
  await expect(page).toHaveURL(/#\/doc\/42$/)
  await page.goBack()
  await expect(page.getByText('Receipt on page 3', { exact: true })).toBeVisible()
  await expect(pager.getByRole('link', { name: 'Page 3', exact: true })).toHaveAttribute('aria-current', 'page')
  await page.reload()
  await expect(page.getByText('Receipt on page 3', { exact: true })).toBeVisible()
  await pager.getByRole('link', { name: 'Page 15', exact: true }).click()
  await expect(page.getByText('Receipt on page 15', { exact: true })).toBeVisible()
  await expect(pager.getByRole('button', { name: 'Next page' })).toBeDisabled()
  await page.goBack()
  await expect(page.getByText('Receipt on page 3', { exact: true })).toBeVisible()
  await page.goForward()
  await expect(page.getByText('Receipt on page 15', { exact: true })).toBeVisible()
  await pager.getByRole('link', { name: 'Page 1', exact: true }).click()
  await expect(page).toHaveURL(/q=receipt&ordering=title$/)
  await expect(pager.getByRole('button', { name: 'Previous page' })).toBeDisabled()
  // A middle page exercises both ellipses on narrow screens.
  if (testInfo.project.name === 'mobile') await page.setViewportSize({ width: 320, height: 740 })
  await page.goto('/#/documents?page=8')
  await expect(pager.getByRole('link', { name: 'Page 8', exact: true })).toHaveAttribute('aria-current', 'page')
  expect(await pager.getByRole('link').allTextContents()).toEqual(['1', '7', '8', '9', '15'])
  const bounds = await pager.locator('.page-links').boundingBox()
  const container = await pager.boundingBox()
  expect(bounds.x).toBeGreaterThanOrEqual(container.x)
  expect(bounds.x + bounds.width).toBeLessThanOrEqual(container.x + container.width)
  await pager.screenshot({ path: testInfo.outputPath('document-pagination.png') })
})

test('document pagination preserves dates and resets on filter or sort changes', async ({ page }) => {
  await mockAPI(page, {
    documentsCount: 743,
    documents: [{ id: 42, title: 'Receipt', created_at: 1780000000, tags: [] }],
  })
  await page.goto('/#/documents?page=3')
  await page.getByRole('button', { name: 'Any date', exact: true }).click()
  const dateFrom = page.getByLabel('Added on or after', { exact: true })
  await expect(dateFrom).toHaveAttribute('type', 'text')
  await expect(dateFrom).toHaveAttribute('placeholder', 'yyyy-mm-dd')
  await expect(page.getByRole('button', { name: 'Choose added on or after' })).toBeVisible()
  await dateFrom.fill('2026-09-01')
  await dateFrom.press('Tab')
  await expect(page).toHaveURL(/#\/documents\?created_at__gte=1788220800$/)
  await page.getByLabel('Added on or before', { exact: true }).fill('2026-09-30')
  await page.getByLabel('Added on or before', { exact: true }).press('Tab')
  await page.getByRole('button', { name: 'Done', exact: true }).click()
  const pager = page.getByRole('navigation', { name: 'Document pages' })
  await pager.getByRole('link', { name: 'Page 3', exact: true }).click()
  await page.reload()
  const activeDateRange = page.getByRole('button', { name: '2026-09-01 – 2026-09-30', exact: true })
  await expect(activeDateRange).toBeVisible()
  await activeDateRange.click()
  await expect(page.getByLabel('Added on or after', { exact: true })).toHaveValue('2026-09-01')
  await expect(page.getByLabel('Added on or before', { exact: true })).toHaveValue('2026-09-30')
  await page.getByRole('button', { name: 'Done', exact: true }).click()
  await expect(pager).toContainText('Page 3 of 15')
  const sorted = page.waitForRequest(request => {
    const url = new URL(request.url())
    return url.pathname === '/api/documents/' && url.searchParams.get('page') === '1'
      && url.searchParams.get('ordering') === 'title'
      && url.searchParams.get('created_at__gte') === '1788220800'
      && url.searchParams.get('created_at__lte') === '1790812799'
  })
  await page.getByLabel('Sort documents').selectOption('title')
  await sorted
  await expect(pager).toContainText('Page 1 of 15')
  await page.goBack()
  await expect(pager).toContainText('Page 3 of 15')
  await page.getByRole('button', { name: '2026-09-01 – 2026-09-30', exact: true }).click()
  await page.getByRole('button', { name: 'Clear dates' }).click()
  await expect(page).not.toHaveURL(/created_at__(?:gte|lte)=/)
  await expect(page.getByRole('button', { name: 'Any date', exact: true })).toBeVisible()
})

test('document pagination corrects invalid and vanished pages without trapping history', async ({ page }) => {
  const options = { documentsCount: 101, documents: [{ id: 42, title: 'Receipt', created_at: 1780000000, tags: [] }] }
  await mockAPI(page, options)
  await page.goto('/#/documents?page=3')
  const pager = page.getByRole('navigation', { name: 'Document pages' })
  await expect(pager).toContainText('Page 3 of 3')
  options.documentsCount = 100
  await page.getByRole('button', { name: 'Refresh documents', exact: true }).click()
  await expect(page).toHaveURL(/#\/documents\?page=2$/)
  await expect(pager).toContainText('Page 2 of 2')
  await pager.getByRole('link', { name: 'Page 1', exact: true }).click()
  await page.goto('/#/documents?page=999')
  await expect(page).toHaveURL(/#\/documents\?page=2$/)
  await page.goBack()
  await expect(page).toHaveURL(/#\/documents$/)
  for (const value of ['-1', 'abc', '2.5']) {
    await page.goto(`/#/documents?page=${value}`)
    await expect(page).toHaveURL(/#\/documents$/)
    await expect(pager).toContainText('Page 1 of 2')
  }
})

test('inbox pagination keeps its scope through detail and browser Back', async ({ page }) => {
  await mockAPI(page, {
    documentsCount: 150,
    jdCategories: [{ id: 49, code: 49, name: 'Inbox', area_code: 40, system: true }],
    documents: [{ id: 42, title: 'Unfiled receipt', created_at: 1780000000, tags: [] }],
  })
  await page.goto('/#/inbox')
  const thirdPage = page.waitForRequest(request => {
    const url = new URL(request.url())
    return url.pathname === '/api/documents/' && url.searchParams.get('page') === '3'
      && !!url.searchParams.get('jd_category_id')
  })
  await page.getByRole('link', { name: 'Page 3', exact: true }).click()
  await thirdPage
  await expect(page).toHaveURL(/#\/inbox\?page=3$/)
  await page.getByText('Unfiled receipt', { exact: true }).click()
  await expect(page).toHaveURL(/#\/doc\/42$/)
  await page.goBack()
  await expect(page.getByRole('navigation', { name: 'Document pages' })).toContainText('Page 3 of 3')
})

test('opens dashboard views through user-facing document routes', async ({ page }) => {
  const filters = {
    q: 'distribution advice',
    tags__id__in: '2',
    correspondents__id__in: '3',
    jd_category_id: '6',
    sensitivity: 'confidential',
    ordering: 'title',
  }
  await mockAPI(page, {
    savedViews: [{
      id: 8,
      name: '22 Investments',
      filter_json: JSON.stringify(filters),
      position: 0,
      shared: false,
    }],
  })
  await page.goto('/#/dashboard')

  await expect(page.getByRole('link', { name: 'New view' })).toHaveAttribute('href', '#/views?new=1')
  const view = page.locator('.views a.view').filter({ hasText: '22 Investments' })
  await expect(view).toHaveCount(1)
  const href = await view.getAttribute('href')
  const linkParams = new URLSearchParams(href.split('?')[1])
  for (const [key, value] of Object.entries(filters)) {
    if (key === 'jd_category_id') continue
    expect(linkParams.get(key)).toBe(value)
  }
  expect(linkParams.get('jd')).toBe(filters.jd_category_id)
  expect(linkParams.has('jd_category_id')).toBe(false)

  const documentRequest = page.waitForRequest(request => {
    const url = new URL(request.url())
    return url.pathname === '/api/documents/' &&
      url.searchParams.get('jd_category_id') === filters.jd_category_id &&
      url.searchParams.get('q') === filters.q
  })
  await view.click()
  const requestParams = new URL((await documentRequest).url()).searchParams
  for (const [key, value] of Object.entries(filters)) {
    expect(requestParams.get(key)).toBe(value)
  }
  await expect(page).toHaveURL(/#\/documents\?.*jd=6/)
  expect(page.url()).not.toContain('jd_category_id')
})

test('starts view creation from the dashboard action', async ({ page }) => {
  await mockAPI(page)
  await page.goto('/#/dashboard')

  await expect(page.getByText('No saved views yet.')).toBeVisible()
  await page.getByRole('link', { name: 'New view' }).click()
  await expect(page).toHaveURL(/#\/views\?new=1$/)
  await expect(page.getByRole('dialog', { name: 'Create a view' })).toBeVisible()
  await expect(page.getByLabel('View name')).toBeFocused()
})

test('stores new saved views as one canonical query', async ({ page }) => {
  await mockAPI(page, {
    jdCategories: [{ id: 6, code: 22, name: 'Investments', area_code: 20, area_name: 'Money', is_area: false }],
    customFields: [{ id: 12, name: 'Payment receipt', data_type: 'documentlink' }],
  })
  await page.goto('/#/views')
  await page.getByRole('button', { name: 'New view' }).click()
  const dialog = page.getByRole('dialog', { name: 'Create a view' })
  await dialog.getByLabel('View name').fill('Private investments')
  await dialog.getByLabel('Query').fill('"distribution advice"')
  await dialog.getByLabel('Filing category').selectOption('6')
  await dialog.getByLabel('Sensitivity').selectOption('confidential')
  await dialog.getByLabel('Custom field value').selectOption({ label: 'Missing value — Payment receipt' })
  await expect(dialog.getByLabel('Document date from', { exact: true })).toHaveAttribute('placeholder', 'yyyy-mm-dd')
  await dialog.getByLabel('Document date from', { exact: true }).fill('2026-01-01')
  await dialog.getByLabel('Document date from', { exact: true }).press('Tab')
  await dialog.getByLabel('Document date to', { exact: true }).fill('2026-12-31')
  await dialog.getByLabel('Document date to', { exact: true }).press('Tab')

  const saveRequest = page.waitForRequest(request => {
    return new URL(request.url()).pathname === '/api/saved_views/' && request.method() === 'POST'
  })
  await dialog.getByRole('button', { name: 'Save view' }).click()
  const payload = (await saveRequest).postDataJSON()
  expect(JSON.parse(payload.filter_json)).toEqual({
    q: '"distribution advice" jd:22 -has-field:"Payment receipt" sensitivity:confidential date:>=2026-01-01 date:<=2026-12-31',
  })
})

test('reopens custom-field presence views and keeps browser navigation stable', async ({ page }) => {
  const query = 'type:invoice -has-field:"Payment receipt"'
  await mockAPI(page, {
    customFields: [{ id: 12, name: 'Payment receipt', data_type: 'documentlink' }],
    savedViews: [{ id: 9, name: 'Invoices missing receipts', filter_json: JSON.stringify({ q: query }), shared: false }],
  })
  await page.goto('/#/views')
  await page.getByRole('button', { name: 'Edit Invoices missing receipts' }).click()
  const dialog = page.getByRole('dialog', { name: 'Edit view' })
  await expect(dialog.getByLabel('Query')).toHaveValue('type:invoice')
  await expect(dialog.getByLabel('Custom field value')).toHaveValue(
    JSON.stringify({ name: 'Payment receipt', missing: true }),
  )
  await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()

  const documentRequest = page.waitForRequest(request => {
    const url = new URL(request.url())
    return url.pathname === '/api/documents/' && url.searchParams.get('q') === query
  })
  await page.getByRole('link', { name: /Invoices missing receipts/ }).click()
  await documentRequest
  await expect(page).toHaveURL(/#\/documents\?q=/)
  await page.goBack()
  await expect(page).toHaveURL(/#\/views$/)
  await page.goForward()
  await expect(page).toHaveURL(/#\/documents\?q=/)
})

test('keeps an invalid saved view open with the server error', async ({ page }) => {
  await mockAPI(page, {
    savedViewCreateError: 'filter key q: no tag value matches "missing" at byte 0',
  })
  await page.goto('/#/views')
  await page.getByRole('button', { name: 'New view' }).click()
  const dialog = page.getByRole('dialog', { name: 'Create a view' })
  await dialog.getByLabel('View name').fill('Missing tag')
  await dialog.getByLabel('Query').fill('tag:missing')
  await dialog.getByRole('button', { name: 'Save view' }).click()

  await expect(dialog).toBeVisible()
  await expect(dialog.getByRole('alert')).toHaveText('filter key q: no tag value matches "missing" at byte 0')
  await expect(dialog.getByRole('button', { name: 'Save view' })).toBeEnabled()
})

test('edits saved views in place and keeps drafts after a rejected save', async ({ page }) => {
  const options = {
    savedViews: [{ id: 8, name: 'Investments', filter_json: '{"q":"jd:22"}', shared: true }],
    failPaths: ['/api/saved_views/8'],
    failureStatus: 409,
    failureMessage: 'a saved view with that name already exists',
  }
  await mockAPI(page, options)
  await page.goto('/#/views')
  await page.getByRole('button', { name: 'Edit Investments', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Edit view' })
  await expect(dialog.getByLabel('View name')).toBeFocused()
  await expect(dialog.getByLabel('Query')).toHaveValue('jd:22')
  await expect(dialog.getByLabel('Share this view')).toBeChecked()
  await dialog.getByLabel('View name').fill('Unsaved name')
  await dialog.getByLabel('Query').fill('discarded')
  await page.keyboard.press('Escape')
  await expect(page.getByRole('link', { name: /Investments/ })).toBeVisible()
  await page.getByRole('button', { name: 'New view' }).click()
  await expect(page.getByRole('dialog').getByLabel('View name')).toHaveValue('')
  await expect(page.getByRole('dialog').getByLabel('Query')).toHaveValue('')
  await page.getByRole('button', { name: 'Cancel', exact: true }).click()

  await page.getByRole('button', { name: 'Edit Investments', exact: true }).click()
  await expect(dialog.getByLabel('View name')).toHaveValue('Investments')
  await expect(dialog.getByLabel('Query')).toHaveValue('jd:22')
  await dialog.getByLabel('View name').fill('Private investments')
  await dialog.getByLabel('Query').fill('jd:22 -is:trash')
  await dialog.getByLabel('Share this view').uncheck()
  await dialog.getByRole('button', { name: 'Save changes' }).click()
  await expect(dialog.getByRole('alert')).toHaveText(options.failureMessage)
  await expect(dialog.getByLabel('View name')).toHaveValue('Private investments')
  await expect(dialog.getByLabel('Query')).toHaveValue('jd:22 -is:trash')
  options.failPaths.length = 0
  await dialog.getByRole('button', { name: 'Save changes' }).click()
  await expect(dialog).toHaveCount(0)
  await page.reload()
  await expect(page.getByRole('button', { name: 'Edit Investments', exact: true })).toHaveCount(0)
  const view = page.getByRole('link', { name: /Private investments/ })
  await expect(view).not.toContainText('Shared')
  await view.click()
  expect(new URLSearchParams(page.url().split('?')[1]).get('q')).toBe('jd:22 -is:trash')
})

test('editing legacy saved views preserves multi-value scopes and ordering without vocabularies', async ({ page }) => {
  const filters = {
    q: 'invoice', tags__id__in: [2, 7], correspondents__id__in: [3, 9],
    jd_category_id: 6, sensitivity: 'internal', ordering: 'title',
  }
  await mockAPI(page, {
    userRole: 'member', capabilities: [],
    savedViews: [{ id: 8, name: 'Legacy scope', filter_json: JSON.stringify(filters) }],
    failPaths: ['/api/tags/', '/api/correspondents/'],
  })
  await page.goto('/#/views')
  await page.getByRole('button', { name: 'Edit Legacy scope' }).click()
  const dialog = page.getByRole('dialog', { name: 'Edit view' })
  await expect(dialog.getByLabel('Tag', { exact: true })).toHaveValue('2,7')
  await expect(dialog.getByLabel('Correspondent', { exact: true })).toHaveValue('3,9')
  await expect(dialog.getByLabel('Filing category')).toHaveValue('6')
  await expect(dialog.getByLabel('Share this view')).toHaveCount(0)
  await dialog.getByLabel('View name').fill('Retained scope')
  await dialog.getByRole('button', { name: 'Save changes' }).click()
  const view = page.getByRole('link', { name: /Retained scope/ })
  await expect(view).toBeVisible()
  const params = new URLSearchParams((await view.getAttribute('href')).split('?')[1])
  for (const [key, value] of Object.entries(filters)) {
    expect(params.get(key === 'jd_category_id' ? 'jd' : key)).toBe(String(value))
  }

  await page.getByRole('button', { name: 'Edit Retained scope' }).click()
  await dialog.getByLabel('Filing category').selectOption('')
  await dialog.getByLabel('Sensitivity').selectOption('restricted')
  await dialog.getByRole('button', { name: 'Save changes' }).click()
  await expect(dialog).toHaveCount(0)
  await view.click()
  const updated = new URLSearchParams(page.url().split('?')[1])
  expect(updated.has('jd')).toBe(false)
  expect(updated.has('sensitivity')).toBe(false)
  expect(updated.get('q')).toBe('invoice sensitivity:restricted')
  expect(updated.get('tags__id__in')).toBe('2,7')
  expect(updated.get('correspondents__id__in')).toBe('3,9')
  expect(updated.get('ordering')).toBe('title')
})

test('renaming a saved view snapshot retains its exact document scope', async ({ page }) => {
  await mockAPI(page, {
    savedViews: [{ id: 8, name: 'Research snapshot', filter_json: '{"document_ids":[17,42],"ordering":"title"}' }],
  })
  await page.goto('/#/views')
  await page.getByRole('button', { name: 'Edit Research snapshot' }).click()
  const dialog = page.getByRole('dialog', { name: 'Edit view' })
  await expect(dialog.getByText('Exact research snapshot')).toBeVisible()
  await expect(dialog.getByLabel('Query')).toHaveCount(0)
  await dialog.getByLabel('View name').fill('Renewal evidence')
  await dialog.getByRole('button', { name: 'Save changes' }).click()
  await page.getByRole('link', { name: /Renewal evidence/ }).click()
  const params = new URLSearchParams(page.url().split('?')[1])
  expect(params.get('document_ids')).toBe('17,42')
  expect(params.get('ordering')).toBe('title')
})

test('loads recent dashboard documents once per navigation', async ({ page }) => {
  let recentRequests = 0
  page.on('request', request => {
    const url = new URL(request.url())
    if (url.pathname === '/api/documents/' && url.searchParams.get('page_size') === '6') {
      recentRequests++
    }
  })
  await mockAPI(page)
  await page.goto('/#/dashboard')
  await expect(page.getByText('Nothing here yet.')).toBeVisible()
  await expect.poll(() => recentRequests).toBe(1)
  await page.waitForTimeout(150)
  expect(recentRequests).toBe(1)

  await page.goto('/#/views')
  await page.goto('/#/dashboard')
  await expect.poll(() => recentRequests).toBe(2)
  await page.waitForTimeout(150)
  expect(recentRequests).toBe(2)
})

test('keeps recent dashboard documents inside the mobile content column', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== 'mobile', 'mobile layout regression')
  await mockAPI(page, {

    filingTreeChosen: true,
    documents: [{
      id: 42,
      title: 'A deliberately long example document title that must shrink inside the recent list',
      jd_category_code: 22,
      jd_category_name: 'Example category',
      sensitivity: 'internal',
      created_at: 1780000000,
      tags: [],
    }],
  })
  await page.goto('/#/dashboard')
  await expect(page.locator('.dash-grid a.irow').filter({ hasText: 'A deliberately long example' })).toBeVisible()

  const widths = await page.locator('.content').evaluate((content) => ({
    client: content.clientWidth,
    scroll: content.scrollWidth,
  }))
  expect(widths.scroll).toBeLessThanOrEqual(widths.client)
})

test('limits dashboard count requests and defers empty-view facets', async ({ page }) => {
  const countRequests = []
  const facetRequests = []
  page.on('request', request => {
    const url = new URL(request.url())
    if (url.pathname === '/api/documents/' && url.searchParams.get('page_size') === '1') {
      countRequests.push(url.toString())
    }
    if (['/api/tags/', '/api/correspondents/'].includes(url.pathname)) {
      facetRequests.push(url.pathname)
    }
  })

  await mockAPI(page, {
    savedViews: Array.from({ length: 6 }, (_, index) => ({
      id: index + 1,
      name: `View ${index + 1}`,
      filter_json: JSON.stringify({ q: `query ${index + 1}` }),
      position: index,
      shared: false,
    })),
  })
  await page.goto('/#/dashboard')
  await expect(page.getByRole('link', { name: 'View all 6 saved views' })).toBeVisible()
  await expect.poll(() => countRequests.length).toBe(4)

  await page.unrouteAll({ behavior: 'wait' })
  await mockAPI(page)
  await page.goto('/#/views')
  const viewCount = page.locator('.views-panel .panel-count')
  await expect(viewCount).toHaveText('0')
  await expect(viewCount).not.toHaveClass(/chip/)
  expect(facetRequests).toEqual([])

  const facets = page.waitForRequest(request => new URL(request.url()).pathname === '/api/tags/')
  await page.getByRole('button', { name: 'New view' }).click()
  await facets
})

test('defers automation facets until an empty workspace is edited', async ({ page }) => {
  const facetRequests = []
  page.on('request', request => {
    const path = new URL(request.url()).pathname
    if (['/api/tags/', '/api/correspondents/'].includes(path)) facetRequests.push(path)
  })
  await mockAPI(page, { automations: [] })
  await page.goto('/#/automations')

  const automationCount = page.locator('.automations-panel .panel-count')
  await expect(automationCount).toHaveText('0')
  await expect(automationCount).not.toHaveClass(/chip/)
  expect(facetRequests).toEqual([])
  await page.getByRole('button', { name: 'New automation' }).click()
  await expect.poll(() => new Set(facetRequests).size).toBe(2)
})

test('keeps saved views ahead of the creation form', async ({ page }) => {
  await mockAPI(page, {
    savedViews: [{
      id: 8,
      name: 'Private investments',
      filter_json: JSON.stringify({ q: 'distribution advice', sensitivity: 'confidential' }),
      position: 0,
      shared: false,
    }],
  })
  await page.goto('/#/views')

  await expect(page.getByRole('link', { name: /Private investments/ })).toBeVisible()
  await expect(page.getByText('Search: “distribution advice”')).toBeVisible()
  await expect(page.getByText('Confidential', { exact: true })).toBeVisible()
  await expect(page.getByRole('dialog', { name: 'Create a view' })).toHaveCount(0)

  await page.getByRole('button', { name: 'New view' }).click()
  const dialog = page.getByRole('dialog', { name: 'Create a view' })
  await expect(dialog).toBeVisible()
  await expect(dialog.getByLabel('View name')).toBeFocused()
  await expect(dialog.getByLabel('Share this view')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(dialog).toHaveCount(0)
})

test('gates saved-view sharing for members by capability', async ({ page }) => {
  const options = { userRole: 'member', capabilities: [] }
  await mockAPI(page, options)
  await page.goto('/#/views')
  await page.getByRole('button', { name: 'New view' }).click()
  await expect(page.getByRole('dialog').getByLabel('Share this view')).toHaveCount(0)

  options.capabilities = ['share_views']
  await page.reload()
  await page.getByRole('button', { name: 'New view' }).click()
  await expect(page.getByRole('dialog').getByLabel('Share this view')).toBeVisible()
})

test('shows shared views without offering to change another users view', async ({ page }) => {
  await mockAPI(page, {
    userRole: 'member',
    capabilities: [],
    savedViews: [{
      id: 8,
      owner_id: 2,
      name: 'Shared tax review',
      filter_json: JSON.stringify({ q: 'tax' }),
      position: 0,
      shared: true,
    }],
  })

  const sharedRequest = page.waitForRequest(request => {
    const url = new URL(request.url())
    return url.pathname === '/api/saved_views/' && url.searchParams.get('include') === 'shared'
  })
  await page.goto('/#/dashboard')
  await sharedRequest
  await expect(page.locator('.views').getByRole('link', { name: /Shared tax review/ })).toBeVisible()

  await page.goto('/#/views')
  await expect(page.getByRole('link', { name: /Shared tax review/ })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Delete Shared tax review' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Edit Shared tax review' })).toHaveCount(0)
})

test('lets admins grant saved-view sharing to members without showing failed grants', async ({ page }) => {
  await mockAPI(page, {

    filingTreeChosen: true,
  })
  let rejectGrant = true
  await page.route('**/api/admin/users/2', route => route.fulfill(rejectGrant
    ? { status: 403, json: { error: 'Capability change denied' } }
    : { json: { ok: true } }))
  await page.goto('/#/settings?tab=archive&section=users')
  const member = page.locator('.user-entry').filter({ hasText: 'member@example.test' })
  await member.locator('summary').click()
  const sharing = member.getByRole('switch', { name: 'Share saved views capability for member@example.test' })
  await expect(sharing).toBeVisible()
  await expect(sharing).not.toBeChecked()
  await sharing.click()
  await expect(page.getByText('Capability change denied', { exact: true })).toBeVisible()
  await expect(sharing).not.toBeChecked()
  rejectGrant = false
  await sharing.click()
  await expect(sharing).toBeChecked()
})

test('prevents self-disable while allowing other users to be disabled and enabled', async ({ page }) => {
  await mockAPI(page, { filingTreeChosen: true })
  const changes = []
  await page.route('**/api/admin/users/*', route => {
    changes.push({ path: new URL(route.request().url()).pathname, body: route.request().postDataJSON() })
    return route.fulfill({ status: 204 })
  })
  await page.goto('/#/settings?tab=archive&section=users')
  const self = page.getByRole('switch', { name: 'User admin@example.test active', exact: true })
  const member = page.getByRole('switch', { name: 'User member@example.test active', exact: true })
  await expect(self).toBeChecked()
  await expect(self).toBeDisabled()
  await member.click()
  await expect(member).not.toBeChecked()
  await expect(self).toBeChecked()
  await member.click()
  await expect(member).toBeChecked()
  expect(changes).toEqual([
    { path: '/api/admin/users/2', body: { disabled: true } },
    { path: '/api/admin/users/2', body: { disabled: false } },
  ])
})

test('does not store hidden member grants when creating an administrator', async ({ page }) => {
  await mockAPI(page, { filingTreeChosen: true })
  await page.goto('/#/settings?tab=archive&section=users')
  const form = page.getByRole('form', { name: 'Create a user', exact: true })
  await form.getByLabel('Email', { exact: true }).fill('new-admin@example.test')
  await form.getByLabel('Display name', { exact: true }).fill('New admin')
  await form.getByLabel('Password', { exact: true }).fill('safe-test-password')
  await form.locator('summary').click()
  const sharing = form.getByRole('checkbox', { name: /^Share saved views/ })
  await sharing.check()
  await expect(sharing).toBeChecked()
  await form.getByRole('combobox', { name: 'Role', exact: true }).selectOption('admin')
  await expect(sharing).toHaveCount(0)
  const creation = page.waitForRequest(request =>
    request.method() === 'POST' && new URL(request.url()).pathname === '/api/admin/users')
  await form.getByRole('button', { name: 'Create user', exact: true }).click()
  expect((await creation).postDataJSON()).toMatchObject({ role: 'admin', capabilities: [] })
})


test('offers Microsoft sign-in without exposing registration controls', async ({ page }) => {
  await mockAPI(page, { filingTreeChosen: true })
  await page.goto('/#/settings?tab=archive&section=mail')

  await page.getByRole('button', { name: 'Add mailbox' }).click()
  await expect(page.getByLabel('Sync mail from', { exact: true })).toHaveAttribute('placeholder', 'yyyy-mm-ddThh:mm')
  await page.locator('#ma-provider').selectOption('microsoft')
  await expect(page.getByText('Auth method', { exact: true })).toHaveCount(0)
  await expect(page.getByLabel('Password')).toHaveCount(0)
  const microsoftSignIn = page.getByRole('button', { name: 'Microsoft sign-in', exact: true })
  await expect(microsoftSignIn).toBeVisible()
  await expect(microsoftSignIn).toContainText('Sign in with Microsoft')

  await page.locator('#ma-provider').selectOption('gmail')
  await expect(page.getByLabel('App password')).toBeVisible()
  await expect(page.getByText('Use a Google app password, not your regular Google password.')).toBeVisible()
  await expect(page.getByRole('link', { name: 'Google App Passwords' })).toHaveAttribute('href', 'https://myaccount.google.com/apppasswords')
  await expect(page.getByRole('button', { name: 'Microsoft sign-in', exact: true })).toHaveCount(0)

  await page.locator('#ma-provider').selectOption('icloud')
  await expect(page.getByText('Use an Apple app-specific password, not your Apple Account password.')).toBeVisible()
  await expect(page.getByRole('link', { name: 'Apple Account' })).toHaveAttribute('href', 'https://account.apple.com/')

  await page.goto('/#/settings')
  await expect(page.getByTitle('Microsoft sign-in settings')).toHaveCount(0)
})

test('shows every document source and the source date', async ({ page }) => {
  await mockAPI(page)
  await page.goto('/#/doc/42')

  await expect(page.getByText('Sources', { exact: true })).toBeVisible()
  await expect(page.getByText('user@example.test', { exact: true })).toBeVisible()
  await expect(page.getByText(/· INBOX/)).toBeVisible()
  await expect(page.getByText(/user@example\.test.*user@example\.test/)).toHaveCount(0)
  await expect(page.getByText('Personal Outlook', { exact: true })).toBeVisible()
  await expect(page.getByText(/archive@example\.test \/ Receipts/)).toBeVisible()
  await expect(page.getByText('Uploaded by Admin', { exact: true })).toBeVisible()
  await expect(page.getByText(/bill\.pdf/)).toBeVisible()
  await expect(page.getByText('first seen', { exact: true })).toBeVisible()
  await expect(page.getByText('Source date', { exact: true })).toBeVisible()
  await expect(page.getByText('Created', { exact: true })).toHaveCount(0)
})

test('keeps wide document detail within one viewport-height row', async ({ page }) => {
  test.skip((page.viewportSize()?.width || 0) <= 1000, 'wide layout only')
  await mockAPI(page, { documentContent: 'Extracted document text. '.repeat(400) })
  await page.goto('/#/doc/42')
  await expect(page.getByRole('heading', { name: 'Electricity bill' })).toBeVisible()

  const layout = await page.locator('.document-detail-grid').evaluate(element => {
    const preview = element.querySelector('.preview').getBoundingClientRect()
    const sidebar = element.querySelector('.detail-sidebar')
    const sidebarBox = sidebar.getBoundingClientRect()
    return {
      bottom: element.getBoundingClientRect().bottom,
      previewBottom: preview.bottom,
      sidebarBottom: sidebarBox.bottom,
      sidebarClientHeight: sidebar.clientHeight,
      sidebarScrollHeight: sidebar.scrollHeight,
    }
  })
  expect(layout.bottom).toBeLessThanOrEqual((page.viewportSize()?.height || 0) + 1)
  expect(Math.abs(layout.previewBottom - layout.sidebarBottom)).toBeLessThan(1)
  expect(layout.sidebarScrollHeight).toBeGreaterThan(layout.sidebarClientHeight)
})

test('protects restricted previews like confidential documents', async ({ page }) => {
  await mockAPI(page, {
    documentSensitivity: 'restricted',
    documentContent: 'Account number 1234',
  })
  await page.goto('/#/doc/42')

  const preview = page.locator('.preview')
  await expect(preview.getByText('Restricted')).toBeVisible()
  await expect(preview.locator('iframe')).toHaveCount(0)
  await expect(page.locator('.extracted')).toHaveAttribute('aria-hidden', 'true')
  await expect(page.getByLabel('Sensitivity')).toHaveValue('restricted')

  await preview.getByRole('button', { name: 'Reveal preview' }).click()
  await expect(preview.locator('iframe')).toHaveAttribute('src', '/preview/42?reveal=1')
  await expect(page.locator('.extracted')).toHaveAttribute('aria-hidden', 'false')
})

for (const clipboard of ['available', 'rejected']) {
  test(`reads and copies full extracted text with ${clipboard} clipboard`, async ({ page }) => {
    const content = 'First line\n' + 'A clear line of text. '.repeat(180) + '\nFinal line beyond preview'
    await mockAPI(page, { documentContent: content, documentSensitivity: 'confidential' })
    await page.addInitScript(clipboard => {
      window.copiedText = []
      Object.defineProperty(navigator, 'clipboard', {
        configurable: true,
        value: { async writeText(value) {
          if (clipboard === 'rejected') throw new DOMException('Denied', 'NotAllowedError')
          window.copiedText.push(value)
        } },
      })
    }, clipboard)
    await page.goto('/#/doc/42')
    const text = page.getByRole('region', { name: 'Extracted text', exact: true })
    await expect(text.getByRole('button', { name: 'Copy text', exact: true })).toHaveCount(0)
    await expect(text).not.toContainText('Final line beyond preview')
    await text.getByRole('button', { name: 'Reveal', exact: true }).click()
    await expect(text).not.toContainText('Final line beyond preview')
    await text.getByRole('button', { name: 'Read all', exact: true }).click()
    await expect(text.getByText(content, { exact: true })).toBeVisible()
    await text.getByRole('button', { name: 'Show less', exact: true }).click()
    await expect(text).not.toContainText('Final line beyond preview')
    await text.getByRole('button', { name: 'Copy text', exact: true }).click()
    if (clipboard === 'available') {
      await expect(text.getByRole('status')).toHaveText('Text copied')
      expect(await page.evaluate(() => window.copiedText)).toEqual([content])
    } else {
      const field = text.getByLabel('Full extracted text for copying', { exact: true })
      await expect(field).toHaveValue(content)
      await field.click()
      expect(await field.evaluate(input => input.value.slice(input.selectionStart, input.selectionEnd))).toBe(content)
      await expect(text.getByRole('status')).toHaveText('Select the text below and copy it manually.')
    }
    await page.getByRole('button', { name: 'Hide', exact: true }).click()
    await expect(text.getByRole('button', { name: 'Copy text', exact: true })).toHaveCount(0)
    await expect(text.getByRole('textbox')).toHaveCount(0)
    await expect(text).not.toContainText('Final line beyond preview')
    await page.evaluate(() => { location.hash = '#/doc/41' })
    await expect(text.getByRole('button', { name: 'Reveal', exact: true })).toBeVisible()
    await expect(text.getByRole('status')).toHaveCount(0)
  })
}

test('opens a private document QR without creating a share or copying automatically', async ({ page }, testInfo) => {
  await mockAPI(page, { userRole: 'member', capabilities: [] })
  await page.addInitScript(() => {
    window.copiedLinks = []
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { async writeText(value) { window.copiedLinks.push(value) } },
    })
  })
  const shareRequests = []
  await page.route('**/api/share_links/**', route => {
    shareRequests.push(route.request().url())
    return route.fulfill({ status: 403, json: { error: 'Not allowed' } })
  })
  await page.goto('/#/doc/42')
  const open = page.getByRole('button', { name: 'Open on my phone', exact: true })
  await open.click()
  const dialog = page.getByRole('dialog', { name: 'Open on my phone', exact: true })
  await expect(dialog).toBeVisible()
  await expect(dialog).toContainText('This does not create a public share link.')
  await expect(dialog).toContainText('This address only works on this computer.')
  const link = dialog.getByLabel('Document link', { exact: true })
  await expect(link).toHaveValue('http://127.0.0.1:5173/app/#/doc/42')
  const qr = dialog.getByRole('img', { name: 'QR code for the displayed link' })
  await expect(qr).toBeVisible()
  await qr.screenshot({ path: testInfo.outputPath('document-qr.png') })
  expect(await page.evaluate(() => window.copiedLinks)).toEqual([])
  expect(shareRequests).toEqual([])
  await dialog.getByRole('button', { name: 'Copy link', exact: true }).click()
  await expect(dialog.getByRole('status')).toHaveText('Link copied')
  expect(await page.evaluate(() => window.copiedLinks)).toEqual(['http://127.0.0.1:5173/app/#/doc/42'])
  await page.keyboard.press('Escape')
  await expect(dialog).toHaveCount(0)
  await expect(open).toBeFocused()
  await open.click()
  await page.evaluate(() => { location.hash = '#/doc/41' })
  await expect(dialog).toHaveCount(0)
})

test('keeps a private document link selectable when clipboard access fails', async ({ page }) => {
  await mockAPI(page)
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: undefined })
  })
  await page.goto('/#/doc/42')
  await page.getByRole('button', { name: 'Open on my phone', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Open on my phone', exact: true })
  await dialog.getByRole('button', { name: 'Copy link', exact: true }).click()
  await expect(dialog.getByRole('status')).toHaveText('Select the link and copy it manually.')
  const link = dialog.getByLabel('Document link', { exact: true })
  await link.click()
  expect(await link.evaluate(input => input.selectionEnd - input.selectionStart)).toBe((await link.inputValue()).length)
})

test('reopens an existing password-protected share QR and clears it on revocation', async ({ page }) => {
  await mockAPI(page)
  let deleted = false
  const writes = []
  await page.route('**/api/share_links/**', route => {
    const request = route.request()
    if (request.method() === 'DELETE') {
      deleted = true
      return route.fulfill({ status: 204 })
    }
    if (request.method() !== 'GET') writes.push(request.method())
    return route.fulfill({ json: { results: deleted ? [] : [{
      id: 7, doc_ids: [42], public_url: 'https://archive.example.test/s/protected', has_passwd: true,
    }] } })
  })
  await page.goto('/#/doc/42')
  await page.getByRole('button', { name: 'Share', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Share document' })
  await expect(dialog.getByText('password', { exact: true })).toBeVisible()
  await dialog.getByRole('button', { name: 'Show link', exact: true }).click()
  await expect(dialog.getByLabel('Share link', { exact: true })).toHaveValue('https://archive.example.test/s/protected')
  await dialog.getByRole('button', { name: 'Show QR code', exact: true }).click()
  await expect(dialog.getByRole('img', { name: 'QR code for the displayed link' })).toBeVisible()
  expect(writes).toEqual([])
  await dialog.getByRole('button', { name: 'Revoke', exact: true }).click()
  await expect(dialog.getByLabel('Share link', { exact: true })).toHaveCount(0)
  await expect(dialog.getByRole('img', { name: 'QR code for the displayed link' })).toHaveCount(0)
  expect(deleted).toBe(true)
})

test('edits document tags inline and preserves tags when a change is refused', async ({ page }, testInfo) => {
  await mockAPI(page)
  const tags = [
    { id: 1, slug: 'airtel', name: 'Airtel' },
    { id: 2, slug: 'receipt', name: 'Receipt' },
    { id: 3, slug: 'payment', name: 'Payment' },
  ]
  const writes = []
  const pages = []
  let assigned = ['airtel', 'receipt']
  let refuse = false
  await page.route('**/api/tags/**', route => {
    const pageNumber = Number(new URL(route.request().url()).searchParams.get('page') || 1)
    pages.push(pageNumber)
    return route.fulfill({ json: { results: pageNumber === 1 ? tags.slice(0, 2) : tags.slice(2), next: pageNumber === 1 ? '/api/tags/?page=2' : null } })
  })
  await page.route('**/api/documents/42', route => route.fulfill({ json: {
    id: 42, title: 'Airtel payment receipt', mime_type: 'application/pdf', tags: assigned, languages: 'en',
  } }))
  await page.route('**/api/documents/bulk_edit', route => {
    const body = route.request().postDataJSON()
    writes.push(body)
    const tag = tags.find(tag => tag.id === body.parameters.tag_id)
    if (!refuse) assigned = body.method === 'add_tag' ? [...assigned, tag.slug] : assigned.filter(slug => slug !== tag.slug)
    return route.fulfill({ json: { results: [{ id: 42, ok: !refuse, ...(refuse ? { code: 'forbidden' } : {}) }] } })
  })
  await page.goto('/#/doc/42')
  const row = page.locator('.document-tags')
  await expect(row.getByRole('button', { name: 'Edit tags' })).toBeVisible()
  await expect(row.getByRole('link', { name: 'Manage tags' })).toHaveCount(0)
  await row.getByRole('button', { name: 'Edit tags' }).click()
  await expect(row.getByRole('link', { name: 'Manage tags' })).toBeVisible()
  await expect(row.getByRole('button', { name: 'Remove tag airtel' })).toBeEnabled()
  expect(pages).toEqual([1, 2])
  const picker = row.getByRole('combobox', { name: 'Tag to add' })
  await picker.fill('pay')
  await expect(row.getByRole('listbox', { name: 'Tag to add' }).getByRole('option')).toHaveCount(1)
  await picker.press('ArrowDown')
  await expect(picker).toHaveAttribute('aria-activedescendant', 'document-tag-options-0')
  await picker.press('Enter')
  await expect(row.getByRole('button', { name: 'Remove tag payment' })).toBeEnabled()
  await row.getByRole('button', { name: 'Remove tag receipt' }).click()
  await expect(row.getByRole('button', { name: 'Remove tag receipt' })).toHaveCount(0)
  expect(writes).toEqual([
    { documents: [42], method: 'add_tag', parameters: { tag_id: 3 } },
    { documents: [42], method: 'remove_tag', parameters: { tag_id: 2 } },
  ])
  await row.scrollIntoViewIfNeeded()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.screenshot({ path: testInfo.outputPath('document-tag-editor.png'), fullPage: true })
  refuse = true
  await row.getByRole('button', { name: 'Remove tag airtel' }).click()
  await expect(row.getByRole('alert')).toHaveText('You do not have permission to edit this document.')
  await expect(row.getByRole('button', { name: 'Remove tag airtel' })).toBeEnabled()
  await row.getByRole('button', { name: 'Done', exact: true }).click()
  await page.reload()
  await expect(row.locator('.pill')).toHaveText(['airtel', 'payment'])
})

test('keeps a new document behind Reveal when an earlier sensitivity save finishes', async ({ page }) => {
  await mockAPI(page, { documentDetails: {
    42: { document: { sensitivity: 'internal', title: 'Earlier document' } },
    43: { document: { sensitivity: 'restricted', title: 'Restricted document', content: 'Restricted extracted text' } },
  } })
  let pending
  await page.route('**/api/documents/42', route => route.request().method() === 'PATCH'
    ? (pending = route)
    : route.fallback())
  await page.goto('/#/doc/42')
  await page.getByRole('combobox', { name: 'Sensitivity', exact: true }).selectOption('public')
  await expect.poll(() => !!pending).toBe(true)
  await page.evaluate(() => { location.hash = '#/doc/43' })
  await expect(page.getByRole('combobox', { name: 'Sensitivity', exact: true })).toHaveValue('restricted')
  const finished = page.waitForEvent('requestfinished', request => request === pending.request())
  await pending.fulfill({ json: { ok: true } })
  await finished
  await paintSettled(page)
  await expect(page.getByRole('combobox', { name: 'Sensitivity', exact: true })).toHaveValue('restricted')
  await expect(page.locator('.extracted')).toHaveAttribute('aria-hidden', 'true')
  await expect(page.locator('.preview iframe')).toHaveCount(0)
})

for (const action of [
  { name: 'trash', method: 'DELETE', path: '/api/documents/42', button: 'Trash', confirm: 'Move to trash' },
  { name: 'restore', method: 'POST', path: '/api/documents/42/restore', button: 'Restore', trashed: true },
  { name: 'permanent deletion', method: 'DELETE', path: '/api/trash/42', button: 'Delete permanently', confirm: 'Delete permanently', trashed: true },
]) {
  test(`ignores late ${action.name} completion after opening another document`, async ({ page }) => {
    const apiRequests = []
    const now = Math.floor(Date.now() / 1000)
    await mockAPI(page, { apiRequests, documentDetails: {
      42: { document: { title: 'Earlier document', owner_id: 1,
        ...(action.trashed ? { trashed_at: now - 86400, deletes_at: now + 86400 } : {}) } },
      43: { document: { title: 'Current document' } },
    } })
    let pending
    await page.route(`**${action.path}`, route => route.request().method() === action.method
      ? (pending = route)
      : route.fallback())
    await page.goto('/#/doc/42')
    await page.getByRole('button', { name: action.button, exact: true }).click()
    if (action.confirm) await page.getByRole('alertdialog').getByRole('button', { name: action.confirm, exact: true }).click()
    await expect.poll(() => !!pending).toBe(true)

    await page.evaluate(() => { location.hash = '#/doc/43' })
    await expect(page.getByRole('heading', { name: 'Current document', exact: true })).toBeVisible()
    await page.getByTitle('Rename', { exact: true }).click()
    const draft = page.locator('.detail form input')
    await draft.fill('Unsaved current title')
    const finished = page.waitForEvent('requestfinished', request => request === pending.request())
    await pending.fulfill({ status: 204 })
    await finished
    await paintSettled(page)

    await expect(page).toHaveURL(/#\/doc\/43$/)
    await expect(draft).toHaveValue('Unsaved current title')
    expect(apiRequests.filter(request => request.method === 'GET' && request.path === '/api/documents/43')).toHaveLength(1)
  })
}

test('adds the first document tag and recovers from a tag-list error', async ({ page }) => {
  await mockAPI(page)
  let fail = true
  const writes = []
  await page.route('**/api/tags/**', route => fail
    ? route.fulfill({ status: 503, json: { error: 'Tags temporarily unavailable' } })
    : route.fulfill({ json: { results: [{ id: 3, slug: 'payment', name: 'Payment' }], next: null } }))
  await page.route('**/api/documents/bulk_edit', route => {
    writes.push(route.request().postDataJSON())
    return route.fulfill({ json: { results: [{ id: 42, ok: true }] } })
  })
  await page.goto('/#/doc/42')
  const row = page.locator('.document-tags')
  await expect(row).toContainText('No tags')
  await row.getByRole('button', { name: 'Edit tags' }).click()
  await expect(row.getByRole('alert')).toHaveText('Tags temporarily unavailable')
  fail = false
  await row.getByRole('button', { name: 'Reload tags' }).click()
  const picker = row.getByRole('combobox', { name: 'Tag to add' })
  await picker.fill('payment')
  await row.getByRole('option', { name: /Payment/ }).click()
  await expect(row.locator('.pill')).toHaveText(['payment'])
  await expect(row.getByText('No tags', { exact: true })).toHaveCount(0)
  expect(writes).toEqual([{ documents: [42], method: 'add_tag', parameters: { tag_id: 3 } }])
})

test('ignores a late document tag save after navigating to another document', async ({ page }) => {
  await mockAPI(page)
  let pending
  await page.route('**/api/tags/**', route => route.fulfill({ json: {
    results: [{ id: 3, slug: 'payment', name: 'Payment' }], next: null,
  } }))
  await page.route('**/api/documents/bulk_edit', route => { pending = route })
  await page.goto('/#/doc/42')
  const row = page.locator('.document-tags')
  await row.getByRole('button', { name: 'Edit tags' }).click()
  const picker = row.getByRole('combobox', { name: 'Tag to add' })
  await picker.fill('payment')
  await picker.press('ArrowDown')
  await picker.press('Enter')
  await expect.poll(() => !!pending).toBe(true)
  await expect(picker).toBeDisabled()
  await page.evaluate(() => { location.hash = '#/doc/41' })
  await expect(page.getByTitle('Rename')).toContainText('Document 41')
  const finished = page.waitForEvent('requestfinished', request => request === pending.request())
  await pending.fulfill({ json: { results: [{ id: 42, ok: true }] } })
  await finished
  await row.getByRole('button', { name: 'Edit tags' }).click()
  await expect(row.getByRole('combobox', { name: 'Tag to add' })).toBeEnabled()
  await expect(row).toContainText('No tags')
  await expect(row.locator('.pill')).toHaveCount(0)
})

test('document tag picker preserves a refused add for retry and closes without a write', async ({ page }) => {
  await mockAPI(page)
  await page.route('**/api/tags/**', route => route.fulfill({ json: {
    results: [{ id: 3, name: 'Payment', slug: 'payment' }], next: null,
  } }))
  const writes = []
  let refuse = true
  await page.route('**/api/documents/bulk_edit', route => {
    writes.push(route.request().postDataJSON())
    return route.fulfill({ json: { results: [{ id: 42, ok: !refuse, code: refuse ? 'forbidden' : '' }] } })
  })
  await page.goto('/#/doc/42')
  const row = page.locator('.document-tags')
  await row.getByRole('button', { name: 'Edit tags' }).click()
  const picker = row.getByRole('combobox', { name: 'Tag to add' })
  await picker.fill('pay')
  await picker.press('Escape')
  await expect(picker).toHaveAttribute('aria-expanded', 'false')
  expect(writes).toHaveLength(0)
  await picker.click()
  await picker.press('ArrowDown')
  await picker.press('Enter')
  await expect(row.getByRole('alert')).toContainText('permission')
  await expect(picker).toHaveValue('pay')
  await expect(row.locator('.pill')).toHaveCount(0)
  refuse = false
  await picker.press('ArrowDown')
  await picker.press('Enter')
  await expect(row.locator('.pill')).toHaveText(['payment'])
  expect(writes).toHaveLength(2)
})

test('document edit refreshes a classifier review tag after metadata and tag writes', async ({ page }) => {
  await mockAPI(page)
  let tags = ['needs-review']
  const edits = []
  const reads = []
  await page.route('**/api/documents/42', route => {
    if (route.request().method() === 'PATCH') {
      edits.push(route.request().postDataJSON())
      tags = []
      return route.fulfill({ json: { id: 42 } })
    }
    reads.push(tags.slice())
    return route.fulfill({ json: {
      id: 42, title: 'Review document', tags, mime_type: 'application/pdf', original_size: 20,
      created_at: 1780000000,
    } })
  })
  await page.route('**/api/tags/**', route => route.fulfill({ json: {
    results: [{ id: 9, name: 'Other label', slug: 'other' }], next: null,
  } }))
  const writes = []
  await page.route('**/api/documents/bulk_edit', route => {
    writes.push(route.request().postDataJSON())
    tags = ['other']
    return route.fulfill({ json: { results: [{ id: 42, ok: true }] } })
  })
  await page.goto('/#/doc/42')
  const row = page.locator('.document-tags')
  await expect(row.locator('.pill')).toHaveText(['needs-review'])
  await page.getByRole('combobox', { name: 'Sensitivity' }).selectOption('internal')
  await expect(row.locator('.pill')).toHaveCount(0)
  expect(edits).toEqual([{ sensitivity: 'internal' }])
  expect(reads).toEqual([['needs-review'], []])

  tags = ['needs-review']
  await page.reload()
  await row.getByRole('button', { name: 'Edit tags' }).click()
  const picker = row.getByRole('combobox', { name: 'Tag to add' })
  await picker.fill('other')
  await picker.press('ArrowDown')
  await picker.press('Enter')
  await expect(row.locator('.pill')).toHaveText(['other'])
  expect(writes).toEqual([{ documents: [42], method: 'add_tag', parameters: { tag_id: 9 } }])
})

test('bulk tag assignment retries partial failure without dropping the selection', async ({ page }) => {
  const documents = [
    { id: 42, title: 'First invoice', created_at: 1780000000, tags: [] },
    { id: 43, title: 'Second invoice', created_at: 1780000000, tags: [] },
  ]
  await mockAPI(page, { documents })
  const pages = []
  await page.route('**/api/tags/**', route => {
    const number = Number(new URL(route.request().url()).searchParams.get('page') || 1)
    pages.push(number)
    return route.fulfill({ json: { results: number === 1
      ? [{ id: 1, name: 'First', slug: 'first' }]
      : [{ id: 9, name: 'Later Page Tag', slug: 'later' }],
    next: number === 1 ? '/api/tags/?page=2' : null } })
  })
  const writes = []
  await page.route('**/api/documents/bulk_edit', route => {
    writes.push(route.request().postDataJSON())
    if (writes.length === 1) {
      documents[0].tags = ['later']
      return route.fulfill({ json: { applied: 1, results: [{ id: 42, ok: true }, { id: 43, ok: false, code: 'forbidden' }] } })
    }
    documents[1].tags = ['later']
    return route.fulfill({ json: { applied: 2, results: [{ id: 42, ok: true }, { id: 43, ok: true }] } })
  })
  await page.goto('/#/documents')
  await page.getByLabel('Select First invoice').check()
  await page.getByLabel('Select Second invoice').check()
  const bar = page.locator('.bulkbar')
  await bar.getByRole('button', { name: 'Add tags' }).click()
  const picker = bar.getByRole('combobox', { name: 'Tag for selection' })
  await picker.fill('later')
  await expect(picker).toHaveAttribute('aria-expanded', 'true')
  await expect(bar.getByRole('option', { name: /Later Page Tag/ })).toBeVisible()
  await picker.press('ArrowDown')
  await picker.press('Enter')
  await expect(bar).toContainText(/1.*failed/)
  await expect(picker).toHaveValue('later')
  await expect(bar).toContainText('2 selected')
  expect(writes[0]).toEqual({ documents: [42, 43], method: 'add_tag', parameters: { tag_id: 9 } })
  expect(pages).toContain(2)
  await expect(page.getByText('later', { exact: true }).first()).toBeVisible()
  await picker.press('ArrowDown')
  await picker.press('Enter')
  await expect(bar).toHaveCount(0)
  expect(writes).toHaveLength(2)
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
})

test('bulk tag assignment is available to members but not in Inbox', async ({ page }) => {
  await mockAPI(page, { userRole: 'member', documents: [{ id: 42, title: 'Member document', created_at: 1780000000 }] })
  await page.goto('/#/documents')
  await page.getByLabel('Select Member document').check()
  await expect(page.locator('.bulkbar').getByRole('button', { name: 'Add tags' })).toBeVisible()
  await page.goto('/#/inbox')
  await expect(page.locator('.bulkbar').getByRole('button', { name: 'Add tags' })).toHaveCount(0)
})

test('tag management link navigates to the searchable Settings catalog', async ({ page }) => {
  await mockAPI(page)
  await page.route('**/api/tags/**', route => route.fulfill({ json: {
    results: [{ id: 5, name: 'Receipt', slug: 'receipt' }], next: null,
  } }))
  await page.goto('/#/doc/42')
  const documentTags = page.locator('.document-tags')
  await expect(documentTags.getByRole('link', { name: 'Manage tags' })).toHaveCount(0)
  await documentTags.getByRole('button', { name: 'Edit tags' }).click()
  await documentTags.getByRole('link', { name: 'Manage tags' }).click()
  await expect(page).toHaveURL(/#\/settings\?tab=archive&section=metadata&metadata=tags$/)
  await expect(page.getByRole('combobox', { name: 'Metadata type' })).toHaveValue('tags')
  await expect(page.getByRole('textbox', { name: 'Rename Receipt' })).toBeVisible()
  await page.reload()
  await expect(page.getByRole('combobox', { name: 'Metadata type' })).toHaveValue('tags')
  const archiveNavigation = page.getByRole('navigation', { name: 'Archive settings sections' })
  await archiveNavigation.getByRole('link', { name: 'People', exact: true }).click()
  await expect(page).toHaveURL(/section=users$/)
  const peopleNav = page.getByRole('navigation', { name: 'People' })
  await expect(peopleNav.getByRole('button', { name: 'Users' })).toBeVisible()
  await expect(peopleNav.getByRole('button', { name: 'Groups' })).toBeVisible()
  await archiveNavigation.getByRole('link', { name: 'Metadata', exact: true }).click()
  await page.getByRole('combobox', { name: 'Metadata type' }).selectOption('correspondents')
  await expect(page).toHaveURL(/section=metadata&metadata=correspondents$/)
  await page.goBack()
  await expect(page.getByRole('combobox', { name: 'Metadata type' })).toHaveValue('tags')
})

test('settings tag management confirms atomic selection across searches', async ({ page }) => {
  await mockAPI(page)
  let tags = Array.from({ length: 501 }, (_, index) => ({
    id: index + 1, name: `Generic tag ${index + 1}`, slug: `generic-${index + 1}`,
  }))
  tags[0] = { id: 1, name: 'Parent Filing Tag', slug: 'parent-filing', child_count: 1 }
  tags[500] = { id: 501, name: 'Child Later Tag', slug: 'archived-later', parent_id: 1 }
  const pages = []
  const deletes = []
  let fail = true
  await page.route('**/api/tags/**', route => {
    const request = route.request()
    if (request.method() === 'DELETE') {
      deletes.push(request.postDataJSON())
      if (fail) return route.fulfill({ status: 503, json: { error: 'Catalog temporarily unavailable' } })
      tags = tags.filter(tag => !request.postDataJSON().ids.includes(tag.id))
      return route.fulfill({ status: 204 })
    }
    const number = Number(new URL(request.url()).searchParams.get('page') || 1)
    pages.push(number)
    return route.fulfill({ json: {
      results: tags.slice((number - 1) * 500, number * 500),
      next: number * 500 < tags.length ? `/api/tags/?page=${number + 1}` : null,
    } })
  })
  await page.goto('/#/settings?tab=archive&section=metadata&metadata=tags')
  await expect(page.getByRole('textbox', { name: 'Rename Child Later Tag' })).toBeVisible()
  expect(pages).toContain(2)
  const search = page.getByRole('searchbox', { name: 'Search tags' })
  await expect(page.getByRole('button', { name: 'Select matching' })).toBeDisabled()
  await search.fill('Parent Filing')
  await page.getByRole('button', { name: 'Select matching' }).click()
  await search.fill('archived-later')
  await page.getByRole('button', { name: 'Select matching' }).click()
  await expect(page.getByRole('button', { name: 'Delete selected' })).toBeEnabled()
  await page.getByRole('button', { name: 'Delete selected' }).click()
  let dialog = page.getByRole('alertdialog')
  await expect(dialog).toContainText(/2 tags/)
  await expect(dialog).toContainText(/child tags/)
  await expect(dialog).toContainText(/cannot be undone/)
  await dialog.getByRole('button', { name: 'Cancel' }).click()
  expect(deletes).toHaveLength(0)
  await page.getByRole('button', { name: 'Delete selected' }).click()
  await page.keyboard.press('Escape')
  await expect(page.getByRole('alertdialog')).toHaveCount(0)
  expect(deletes).toHaveLength(0)
  await page.getByRole('button', { name: 'Delete selected' }).click()
  dialog = page.getByRole('alertdialog')
  await dialog.getByRole('button', { name: 'Delete tags' }).click()
  await expect(page.getByRole('alert')).toContainText('Catalog temporarily unavailable')
  await expect(page.getByRole('textbox', { name: 'Rename Child Later Tag' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Delete selected' })).toBeEnabled()
  expect(deletes).toEqual([{ ids: [1, 501] }])
  fail = false
  await page.getByRole('button', { name: 'Delete selected' }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: 'Delete tags' }).click()
  await expect(page.getByRole('textbox', { name: 'Rename Child Later Tag' })).toHaveCount(0)
  await search.fill('parent-filing')
  await expect(page.getByRole('textbox', { name: 'Rename Parent Filing Tag' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Delete selected' })).toBeDisabled()
  expect(deletes).toEqual([{ ids: [1, 501] }, { ids: [1, 501] }])
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.getByRole('combobox', { name: 'Metadata type' }).selectOption('correspondents')
  await expect(page.getByRole('button', { name: 'Delete selected' })).toHaveCount(0)
})

test('settings tag row deletion uses the confirmed catalog endpoint', async ({ page }) => {
  await mockAPI(page)
  const deletes = []
  let exists = true
  await page.route('**/api/tags/**', route => {
    if (route.request().method() === 'DELETE') {
      deletes.push(route.request().postDataJSON())
      exists = false
      return route.fulfill({ status: 204 })
    }
    return route.fulfill({ json: {
      results: exists ? [{ id: 9, name: 'Individual tag', slug: 'individual' }] : [], next: null,
    } })
  })
  await page.goto('/#/settings?tab=archive&section=metadata&metadata=tags')
  await page.getByRole('button', { name: 'Delete Individual tag' }).click()
  const dialog = page.getByRole('alertdialog')
  await expect(dialog).toContainText('document')
  await dialog.getByRole('button', { name: 'Delete tags' }).click()
  await expect(page.getByRole('textbox', { name: 'Rename Individual tag' })).toHaveCount(0)
  expect(deletes).toEqual([{ ids: [9] }])
})

test('settings tag management ignores a late delete after switching metadata', async ({ page }) => {
  await mockAPI(page)
  let pending
  await page.route('**/api/tags/**', route => route.request().method() === 'DELETE'
    ? (pending = route)
    : route.fulfill({ json: { results: [{ id: 7, name: 'Old tag', slug: 'old' }], next: null } }))
  await page.goto('/#/settings?tab=archive&section=metadata&metadata=tags')
  await page.getByRole('button', { name: 'Delete Old tag' }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: 'Delete tags' }).click()
  await expect.poll(() => !!pending).toBe(true)
  await page.evaluate(() => { location.hash = '#/settings?tab=archive&section=metadata&metadata=correspondents' })
  await expect(page.getByRole('combobox', { name: 'Metadata type' })).toHaveValue('correspondents')
  const finished = page.waitForEvent('requestfinished', request => request === pending.request())
  await pending.fulfill({ status: 204 })
  await finished
  await paintSettled(page)
  await expect(page.getByRole('combobox', { name: 'Metadata type' })).toHaveValue('correspondents')
  await expect(page.getByText(/1 tag deleted/)).toHaveCount(0)
})

test('shows share controls only with the share-links capability', async ({ page }) => {
  const documents = [{
    id: 42, title: 'Electricity bill', mime_type: 'application/pdf',
    created_at: 1780000000, sensitivity: '', tags: [],
  }]
  await mockAPI(page, { userRole: 'member', capabilities: [], documents })
  await page.goto('/#/documents')

  await page.getByLabel('Select Electricity bill').check()
  await expect(page.locator('.bulkbar').getByRole('button', { name: 'Share' })).toHaveCount(0)
  await page.goto('/#/doc/42')
  await expect(page.locator('.toolbar').getByRole('button', { name: 'Share' })).toHaveCount(0)

  await page.unrouteAll({ behavior: 'wait' })
  await mockAPI(page, { userRole: 'member', capabilities: ['share_links'], documents })
  await page.reload()
  await expect(page.locator('.toolbar').getByRole('button', { name: 'Share' })).toBeVisible()
})

test('does not show a previous document share link while the next list is loading', async ({ page }) => {
  await mockAPI(page)
  let reads = 0
  let pending
  await page.route('**/api/share_links/', route => {
    if (++reads === 1) return route.fulfill({ json: { results: [{
      id: 7, doc_ids: [42], public_url: 'https://archive.example.test/s/first-document-bearer',
    }] } })
    pending = route
  })
  await page.goto('/#/doc/42')
  await page.getByRole('button', { name: 'Share', exact: true }).click()
  await expect(page.getByText('https://archive.example.test/s/first-document-bearer', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Close sharing', exact: true }).click()
  await page.evaluate(() => { location.hash = '#/doc/41' })
  await expect(page.getByTitle('Rename', { exact: true })).toContainText('Document 41')
  await page.getByRole('button', { name: 'Share', exact: true }).click()
  await expect.poll(() => !!pending).toBe(true)
  await expect(page.getByText('https://archive.example.test/s/first-document-bearer', { exact: true })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Show link', exact: true })).toHaveCount(0)
  await pending.fulfill({ json: { results: [] } })
})

for (const screen of ['list', 'detail']) {
  for (const clipboard of ['unavailable', 'rejected', 'available']) {
    test(`keeps ${screen} share links usable when clipboard is ${clipboard}`, async ({ page }) => {
      const documents = [{ id: 42, title: 'Electricity bill', created_at: 1780000000, tags: [] }]
      await mockAPI(page, { userRole: 'member', capabilities: ['share_links'], documents })
      await page.addInitScript(clipboard => {
        window.copiedLinks = []
        Object.defineProperty(navigator, 'clipboard', {
          configurable: true,
          value: clipboard === 'unavailable' ? undefined : {
            async writeText(text) {
              if (clipboard === 'rejected') throw new DOMException('Clipboard denied', 'NotAllowedError')
              window.copiedLinks.push(text)
            },
          },
        })
      }, clipboard)
      const url = 'https://archive.example.test/s/shared-token'
      const creations = []
      await page.route('**/api/share_links/', async route => {
        if (route.request().method() === 'POST') {
          creations.push(route.request().postDataJSON())
          return route.fulfill({ json: { id: 7, doc_ids: [42], public_url: url } })
        }
        return route.fulfill({ json: { results: creations.length ? [{ id: 7, doc_ids: [42], public_url: url }] : [] } })
      })
      await page.goto(screen === 'list' ? '/#/documents' : '/#/doc/42')
      if (screen === 'list') {
        await page.getByLabel('Select Electricity bill').check()
        await page.locator('.bulkbar').getByRole('button', { name: 'Share', exact: true }).click()
      } else {
        await page.locator('.toolbar').getByRole('button', { name: 'Share', exact: true }).click()
        await page.getByRole('button', { name: 'Create & copy' }).click()
      }
      const panel = screen === 'list'
        ? page.getByRole('group', { name: 'Created share link' })
        : page.getByRole('dialog', { name: 'Share document' })
      const link = panel.getByRole('textbox', { name: 'Share link', exact: true })
      await expect(link).toHaveValue(url)
      await expect(page.locator('.toast')).toHaveText(clipboard === 'available'
        ? 'Share link copied'
        : 'Share link ready. Select the link and copy it manually.')
      expect(creations).toHaveLength(1)
      expect(creations[0].doc_ids).toEqual([42])
      await panel.getByRole('button', { name: 'Show QR code', exact: true }).click()
      await expect(panel.getByRole('img', { name: 'QR code for the displayed link' })).toBeVisible()
      expect(creations).toHaveLength(1)
      await link.click()
      expect(await link.evaluate(input => input.value.slice(input.selectionStart, input.selectionEnd))).toBe(url)
      await page.evaluate(() => {
        Object.defineProperty(navigator, 'clipboard', {
          configurable: true,
          value: { async writeText(text) { window.copiedLinks.push(text) } },
        })
      })
      await panel.getByRole('button', { name: 'Copy link', exact: true }).click()
      await expect(page.locator('.toast')).toHaveText('Share link copied')
      expect(await page.evaluate(() => window.copiedLinks)).toEqual(clipboard === 'available' ? [url, url] : [url])
      expect(creations).toHaveLength(1)
    })
  }
}

for (const screen of ['list', 'detail']) {
  test(`ignores late ${screen} share creation after leaving its account or document`, async ({ page }) => {
    await mockAPI(page, {
      userRole: 'member', capabilities: ['share_links'],
      documents: [{ id: 42, title: 'Electricity bill', created_at: 1780000000, tags: [] }],
    })
    await page.addInitScript(() => {
      window.copiedLinks = []
      Object.defineProperty(navigator, 'clipboard', {
        value: { async writeText(text) { window.copiedLinks.push(text) } },
      })
    })
    let pendingShare
    await page.route('**/api/share_links/', async route => {
      if (route.request().method() !== 'POST') return route.fallback()
      pendingShare = route
    })
    await page.goto(screen === 'list' ? '/#/documents' : '/#/doc/42')
    if (screen === 'list') {
      await page.getByLabel('Select Electricity bill').check()
      await page.locator('.bulkbar').getByRole('button', { name: 'Share', exact: true }).click()
    } else {
      await page.locator('.toolbar').getByRole('button', { name: 'Share', exact: true }).click()
      await page.getByRole('button', { name: 'Create & copy' }).click()
    }
    await expect.poll(() => !!pendingShare).toBe(true)
    if (screen === 'list') {
      const navigation = page.getByRole('button', { name: 'Open navigation', exact: true })
      if (await navigation.isVisible()) await navigation.click()
      await page.getByRole('button', { name: 'Sign out', exact: true }).click()
      await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeVisible()
    } else {
      await page.evaluate(() => { location.hash = '#/doc/43' })
      await expect(page.getByText('Document 43', { exact: true })).toBeVisible()
    }
    const finished = page.waitForEvent('requestfinished', request => request === pendingShare.request())
    await pendingShare.fulfill({ json: { public_url: 'https://archive.example.test/s/late-link' } })
    await finished
    expect(await page.evaluate(() => window.copiedLinks)).toEqual([])
    await expect(page.getByRole('textbox', { name: 'Share link', exact: true })).toHaveCount(0)
    await expect(page.getByRole('dialog', { name: 'Share document' })).toHaveCount(0)
  })
}

test('keeps list rows stable when a thumbnail is missing', async ({ page }) => {
  const document = {
    title: 'Stable document row', created_at: 1780000000, sensitivity: '',
    jd_category_code: 49, jd_category_name: 'Inbox', jd_area_name: 'System', tags: [],
  }
  await mockAPI(page, {
    documents: [{ ...document, id: 2 }],
    thumbnailFailures: [2], thumbnailDelay: 1000,
  })
  await page.goto('/#/documents')

  const row = page.locator('.irow.hoverable')
  const thumb = row.locator('.rthumb')
  await expect(row).toHaveCount(1)
  await expect(thumb).not.toHaveClass(/none/)
  const [rowBefore, thumbBefore] = await Promise.all([row.boundingBox(), thumb.boundingBox()])
  await expect(thumb).toHaveClass(/none/)
  const [rowAfter, thumbAfter] = await Promise.all([row.boundingBox(), thumb.boundingBox()])
  expect(rowAfter).toEqual(rowBefore)
  expect(thumbAfter).toEqual(thumbBefore)
})

test('previews archived email bodies inline', async ({ page }) => {
  await mockAPI(page, {
    documentMime: 'message/rfc822',
    previewHTML: '<!doctype html><html><body><h1>Distribution advice</h1></body></html>',
  })
  await page.goto('/#/doc/42')

  const preview = page.getByTitle('Document preview')
  await expect(preview).toBeVisible()
  await expect(page.getByText('No inline preview for this format')).toHaveCount(0)
  await expect(preview.contentFrame().getByRole('heading', { name: 'Distribution advice' })).toBeVisible()
})

test('saves the display name through supported profile fields and keeps email read-only', async ({ page }) => {
  const pageErrors = []
  page.on('pageerror', error => pageErrors.push(error.message))
  page.on('console', message => {
    if (message.text().includes('derived_inert')) pageErrors.push(message.text())
  })
  await mockAPI(page)
  const changes = []
  await page.route('**/api/users/me', route => {
    const body = route.request().postDataJSON()
    changes.push(body)
    if ('email' in body) return route.fulfill({ status: 400, json: {
      code: 'email_change_unsupported', error: 'Email changes require a dedicated flow.',
    } })
    return route.fulfill({ json: { display_name: body.display_name } })
  })
  await page.route('**/api/whoami', route => changes.length
    ? route.fulfill({ json: { kind: 'user', user_id: 1, email: 'admin@example.test',
      display_name: changes.at(-1).display_name, role: 'admin', capabilities: [] } })
    : route.fallback())
  await page.goto('/#/settings')
  const profile = page.getByRole('region', { name: 'Profile', exact: true })
  await profile.getByLabel('Display name', { exact: true }).fill('  Updated name  ')
  await profile.getByRole('button', { name: 'Save profile', exact: true }).click()
  await expect(profile.getByLabel('Display name', { exact: true })).toHaveValue('Updated name')
  await expect(profile.getByLabel('Email', { exact: true })).toHaveValue('admin@example.test')
  await expect(profile.getByLabel('Email', { exact: true })).toHaveAttribute('readonly', '')
  expect(changes).toEqual([{ display_name: 'Updated name' }])
  expect(pageErrors).toEqual([])
})

test('changes a password-managed sign-in email through focused reauthentication', async ({ page }) => {
  const options = {
    userEmail: 'admin@example.test',
    emailChangeMode: 'password',
  }
  await mockAPI(page, options)
  const requests = []
  let pendingChange
  await page.route('**/api/users/me/email', route => {
    const body = route.request().postDataJSON()
    requests.push(body)
    if (requests.length === 1) {
      return route.fulfill({
        status: 401,
        json: { code: 'reauthentication_failed', error: 'Current password is incorrect.' },
      })
    }
    options.userEmail = body.email
    pendingChange = route
  })

  await page.goto('/#/settings')
  const profile = page.getByRole('region', { name: 'Profile', exact: true })
  await expect(profile.getByText('Sign-in: Password', { exact: true })).toBeVisible()
  await profile.getByRole('button', { name: 'Change email', exact: true }).click()
  let dialog = page.getByRole('dialog', { name: 'Change sign-in email', exact: true })
  await expect(dialog).toContainText('does not send a confirmation or recovery email')
  await expect(dialog).toContainText('Your account, documents, and permissions stay the same.')
  await expect(dialog).toContainText('Every other browser session will be signed out.')

  await dialog.getByLabel('New email', { exact: true }).fill('not-an-email')
  await dialog.getByLabel('Confirm new email', { exact: true }).fill('not-an-email')
  await dialog.getByLabel('Current password', { exact: true }).fill('invalid-email-secret')
  await dialog.getByRole('button', { name: 'Change sign-in email', exact: true }).click()
  await expect(dialog.getByRole('alert')).toHaveText('Enter a valid email address in both fields.')
  await expect(dialog.getByLabel('Current password', { exact: true })).toHaveValue('')
  expect(requests).toEqual([])

  await dialog.getByLabel('New email', { exact: true }).fill('changed@example.test')
  await dialog.getByLabel('Confirm new email', { exact: true }).fill('different@example.test')
  await dialog.getByLabel('Current password', { exact: true }).fill('secret that must clear')
  await dialog.getByRole('button', { name: 'Change sign-in email', exact: true }).click()
  await expect(dialog.getByRole('alert')).toHaveText('The email addresses must match exactly.')
  await expect(dialog.getByLabel('Current password', { exact: true })).toHaveValue('')
  expect(requests).toEqual([])

  await dialog.getByLabel('Confirm new email', { exact: true }).fill('changed@example.test')
  await dialog.getByLabel('Current password', { exact: true }).fill('wrong password')
  await dialog.getByRole('button', { name: 'Change sign-in email', exact: true }).click()
  await expect(dialog.getByRole('alert')).toHaveText('Current password is incorrect.')
  await expect(dialog.getByLabel('Current password', { exact: true })).toHaveValue('')
  await expect(dialog.getByLabel('New email', { exact: true })).toHaveValue('changed@example.test')

  await dialog.getByLabel('Current password', { exact: true }).fill('close-secret')
  await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()
  await profile.getByRole('button', { name: 'Change email', exact: true }).click()
  dialog = page.getByRole('dialog', { name: 'Change sign-in email', exact: true })
  await expect(dialog.getByLabel('New email', { exact: true })).toHaveValue('')
  await expect(dialog.getByLabel('Current password', { exact: true })).toHaveValue('')

  await dialog.getByLabel('New email', { exact: true }).fill('changed@example.test')
  await dialog.getByLabel('Confirm new email', { exact: true }).fill('changed@example.test')
  await dialog.getByLabel('Current password', { exact: true }).fill('correct password')
  await dialog.getByRole('button', { name: 'Change sign-in email', exact: true }).click()
  await expect.poll(() => !!pendingChange).toBe(true)
  await expect(dialog.getByLabel('New email', { exact: true })).toBeDisabled()
  await expect(dialog.getByRole('button', { name: 'Close email change', exact: true })).toBeDisabled()
  await pendingChange.fulfill({ status: 204 })

  await expect(dialog).toHaveCount(0)
  await expect(profile.getByLabel('Email', { exact: true })).toHaveValue('changed@example.test')
  await expect(page.locator('.toast[role="status"]')).toHaveText('Sign-in email changed')
  expect(requests).toEqual([
    { email: 'changed@example.test', current_password: 'wrong password' },
    { email: 'changed@example.test', current_password: 'correct password' },
  ])
})

test('clears an open email-change secret when the account changes', async ({ page }) => {
  await mockAPI(page, { emailChangeMode: 'password' })
  let identityReads = 0
  let pendingIdentity
  await page.route('**/api/whoami', route => {
    if (++identityReads === 1) return route.fallback()
    pendingIdentity = route
  })
  await page.goto('/#/settings')
  await page.getByRole('button', { name: 'Save profile', exact: true }).click()
  await expect.poll(() => !!pendingIdentity).toBe(true)
  await page.getByRole('button', { name: 'Change email', exact: true }).click()
  let dialog = page.getByRole('dialog', { name: 'Change sign-in email', exact: true })
  await dialog.getByLabel('New email', { exact: true }).fill('private@example.test')
  await dialog.getByLabel('Confirm new email', { exact: true }).fill('private@example.test')
  await dialog.getByLabel('Current password', { exact: true }).fill('account-one-secret')
  await pendingIdentity.fulfill({ json: {
    kind: 'user', user_id: 2, email: 'second@example.test', display_name: 'Second user',
    role: 'member', capabilities: [], email_change_mode: 'password',
  } })
  await expect(dialog).toHaveCount(0)
  await page.getByRole('button', { name: 'Change email', exact: true }).click()
  dialog = page.getByRole('dialog', { name: 'Change sign-in email', exact: true })
  await expect(dialog.getByLabel('New email', { exact: true })).toHaveValue('')
  await expect(dialog.getByLabel('Current password', { exact: true })).toHaveValue('')
})

test('uses provider reauthentication for OIDC-managed email and consumes fixed notices', async ({ page }) => {
  await mockAPI(page, { emailChangeMode: 'oidc' })
  const syncRequests = []
  await page.route('**/oidc/email-change', route => {
    syncRequests.push({
      method: route.request().method(),
      origin: route.request().headers().origin,
    })
    return route.fulfill({
      status: 302,
      headers: { location: '/?account_notice=email_changed#/settings' },
    })
  })
  await page.goto('/#/settings')
  const profile = page.getByRole('region', { name: 'Profile', exact: true })
  await expect(profile.getByText('Sign-in: Identity provider', { exact: true })).toBeVisible()
  await expect(profile.getByText(/identity provider manages this address/i)).toBeVisible()
  await expect(profile.getByRole('button', { name: 'Change email', exact: true })).toHaveCount(0)
  await profile.getByRole('button', { name: 'Sync from identity provider', exact: true }).click()
  await expect(page.locator('.toast[role="status"]')).toHaveText('Sign-in email changed')
  await expect(page).toHaveURL(/\/#\/settings$/)
  expect(new URL(page.url()).searchParams.has('account_notice')).toBe(false)
  expect(syncRequests).toEqual([{ method: 'POST', origin: 'http://127.0.0.1:5173' }])

  for (const [notice, message] of [
    ['identity_bound', 'Identity provider connected'],
    ['email_checked', 'Sign-in email is up to date'],
  ]) {
    await page.goto(`/?account_notice=${notice}#/settings`)
    await expect(page.locator('.toast[role="status"]')).toHaveText(message)
    expect(new URL(page.url()).searchParams.has('account_notice')).toBe(false)
    expect(new URL(page.url()).hash).toBe('#/settings')
  }

  await page.goto('/?account_notice=untrusted-detail#/settings')
  await expect(page.locator('.toast[role="status"]')).toHaveCount(0)
  expect(new URL(page.url()).searchParams.has('account_notice')).toBe(false)
  expect(new URL(page.url()).hash).toBe('#/settings')
})

test('keeps disabled account email read-only without a mutation path', async ({ page }) => {
  await mockAPI(page, { emailChangeMode: 'disabled' })
  await page.goto('/#/settings')
  const profile = page.getByRole('region', { name: 'Profile', exact: true })
  await expect(profile.getByText('Sign-in: Managed account', { exact: true })).toBeVisible()
  await expect(profile.getByText('Sign-in email changes are not available for this account.')).toBeVisible()
  await expect(profile.getByRole('button', { name: 'Change email', exact: true })).toHaveCount(0)
  await expect(profile.getByRole('button', { name: 'Sync from identity provider', exact: true })).toHaveCount(0)
})

test('keeps the email-change dialog inside an enlarged mobile viewport', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await mockAPI(page, {
    emailChangeMode: 'password',
    userEmail: 'account-with-a-long-address@example.test',
  })
  await page.goto('/#/settings')
  await page.evaluate(() => { document.documentElement.style.fontSize = '24px' })
  await page.getByRole('button', { name: 'Change email', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Change sign-in email', exact: true })
  await expect(dialog).toBeVisible()
  const layout = await dialog.evaluate(element => {
    const box = element.getBoundingClientRect()
    return {
      documentWidth: document.documentElement.scrollWidth,
      viewportWidth: window.innerWidth,
      left: box.left,
      right: box.right,
      scrollWidth: element.scrollWidth,
      clientWidth: element.clientWidth,
    }
  })
  expect(layout.documentWidth).toBeLessThanOrEqual(layout.viewportWidth)
  expect(layout.left).toBeGreaterThanOrEqual(0)
  expect(layout.right).toBeLessThanOrEqual(layout.viewportWidth)
  expect(layout.scrollWidth).toBeLessThanOrEqual(layout.clientWidth)
})

test('refreshes the profile photo from the versioned avatar URL after upload', async ({ page }) => {
  const pageErrors = []
  page.on('pageerror', error => pageErrors.push(error.message))
  page.on('console', message => {
    if (message.text().includes('derived_inert')) pageErrors.push(message.text())
  })
  await mockAPI(page)
  let avatarVersion = 'old-pixels'
  const avatarRequests = []
  await page.route('**/api/whoami', route => route.fulfill({ json: {
    kind: 'user', user_id: 1, email: 'admin@example.test', display_name: 'Admin',
    role: 'admin', capabilities: [], avatar_url: `/api/users/1/avatar?v=${avatarVersion}`,
  } }))
  const pixels = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aD1sAAAAASUVORK5CYII=', 'base64')
  await page.route('**/api/users/1/avatar?*', route => {
    avatarRequests.push(new URL(route.request().url()).searchParams.get('v'))
    return route.fulfill({ contentType: 'image/png', body: pixels })
  })
  await page.route('**/api/users/me/avatar', route => {
    avatarVersion = 'new-pixels'
    return route.fulfill({ status: 204 })
  })
  await page.goto('/#/settings')
  const profile = page.getByRole('region', { name: 'Profile', exact: true })
  await expect(profile.locator('img')).toHaveAttribute('src', '/api/users/1/avatar?v=old-pixels')
  await expect.poll(() => avatarRequests.includes('old-pixels')).toBe(true)
  await profile.locator('input[type=file]').setInputFiles({ name: 'avatar.png', mimeType: 'image/png', buffer: pixels })
  await expect(profile.locator('img')).toHaveAttribute('src', '/api/users/1/avatar?v=new-pixels')
  await expect.poll(() => avatarRequests.includes('new-pixels')).toBe(true)
  expect(pageErrors).toEqual([])
})

for (const demoSession of ['anon', 'scratch']) {
  test(`keeps ${demoSession} demo account settings read-only without unavailable requests`, async ({ page }) => {
    const apiRequests = []
    await mockAPI(page, { demoMode: true, demoSession, capabilities: [], apiRequests })
    await page.goto('/#/settings')
    const profile = page.getByRole('region', { name: 'Profile', exact: true })
    await expect(profile).toBeVisible()
    await expect(profile.getByLabel('Display name', { exact: true })).toHaveAttribute('readonly', '')
    await expect(profile.getByLabel('Email', { exact: true })).toHaveAttribute('readonly', '')
    for (const name of ['Save profile', 'Change photo', 'Change email', 'Sync from identity provider', 'Pair mobile app', 'Create token', 'Delete saved password']) {
      await expect(page.getByRole('button', { name, exact: true })).toHaveCount(0)
    }
    for (const name of ['Mobile app', 'API tokens', 'Saved decryption passwords', 'Mailboxes']) {
      await expect(page.getByRole('heading', { name, exact: true })).toHaveCount(0)
    }
    await page.evaluate(() => window.dispatchEvent(new Event('focus')))
    await paintSettled(page)
    expect(apiRequests.filter(request => /^\/api\/(tokens|decryption-passwords|email-accounts|mobile\/pairing|users\/me)(\/|$)/.test(request.path))).toEqual([])

    // The fixture must reject these operations just like the assembled server.
    const denied = await page.evaluate(async () => {
      const requests = [
        ['GET', '/api/tokens/'], ['GET', '/api/email-accounts'],
        ['PATCH', '/api/users/me'], ['POST', '/api/users/me/avatar'],
        ['POST', '/api/users/me/email'], ['POST', '/api/mobile/pairing'],
        ['POST', '/api/tokens/'], ['DELETE', '/api/decryption-passwords/1'],
      ]
      return Promise.all(requests.map(async ([method, path]) => {
        const response = await fetch(path, { method })
        return { status: response.status, code: (await response.json()).code }
      }))
    })
    expect(denied).toEqual(demoSession === 'scratch'
      ? Array(8).fill({ status: 403, code: 'token_route_forbidden' })
      : [{ status: 401, code: 'unauthorized' }, { status: 403, code: 'forbidden' },
        ...Array(6).fill({ status: 403, code: 'demo_upgrade_required' })])
  })
}

test('names scoped tokens and account vault records', async ({ page }) => {
  await mockAPI(page)
  await page.goto('/#/settings')

  await expect(page.getByLabel('Token name')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Read only' })).toHaveAttribute('aria-pressed', 'true')
  await expect(page.getByText('Archive search')).toBeVisible()
  await expect(page.getByTitle('documents:read')).toHaveText('Read only')
  await expect(page.getByText('Authorization: Token <token>', { exact: true })).toBeVisible()
  await page.getByRole('heading', { name: 'Saved decryption passwords' }).scrollIntoViewIfNeeded()
  await expect(page.getByText('Axis Bank Atlas Credit Card Statement ending 0194.pdf')).toBeVisible()
  await page.getByLabel('Token name').fill('Readonly tablet')
  await page.getByRole('button', { name: 'Read & write' }).click()
  const created = page.waitForRequest(request =>
    request.method() === 'POST' && new URL(request.url()).pathname === '/api/tokens/')
  await page.getByRole('button', { name: 'Create token' }).click()
  expect((await created).postDataJSON()).toEqual({
    name: 'Readonly tablet', scopes: 'documents:read,documents:write',
  })
  await expect(page.locator('#minted')).toHaveValue('new-token-secret')
})


test('connected mobile apps refresh after pairing and remain visible on return', async ({ page }) => {
  await mockAPI(page)
  let rows = [{ id: 1, user_id: 1, name: 'Archive script', scopes: 'documents:read', created_at: 1780100000 }]
  await page.route('**/api/tokens/', route => route.fulfill({ json: { results: rows } }))
  await page.route('**/api/mobile/pairing', route => {
    if (route.request().method() === 'DELETE') return route.fulfill({ status: 204 })
    return route.fulfill({ status: 201, json: {
      name: 'My phone', code: 'a'.repeat(64), pairing_url: 'suchi://pair?pending',
      expires_at: Math.floor(Date.now() / 1000) + 300, qr_data_url: 'data:image/png;base64,',
    } })
  })
  await page.goto('/#/settings')
  const mobile = page.getByRole('region', { name: 'Mobile app', exact: true })
  const tokens = page.getByRole('region', { name: 'API tokens', exact: true })
  await expect(mobile.getByText('No connected mobile apps. Pair the app to add one here.')).toBeVisible()
  await mobile.getByRole('button', { name: 'Pair mobile app', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Pair mobile app', exact: true })
  await dialog.getByRole('button', { name: 'Generate QR code' }).click()
  await expect(dialog.locator('#pairing-link')).toHaveValue('suchi://pair?pending')
  await expect(mobile.locator('.token-row')).toHaveCount(0)
  // The phone exchanges the code in a separate client while the browser prompt remains open.
  rows = [
    { id: 2, user_id: 1, source: 'mobile_pairing', name: 'My phone', scopes: 'documents:read,documents:write', created_at: 1780100000 },
    ...rows,
  ]
  await expect(mobile.locator('.token-row')).toHaveCount(1, { timeout: 8000 })
  await dialog.getByRole('button', { name: 'Done', exact: true }).click()
  await expect(mobile.getByText('My phone', { exact: true })).toBeVisible()
  await expect(mobile.getByText('Not used yet', { exact: true })).toBeVisible()
  await expect(tokens.getByText('Archive script', { exact: true })).toBeVisible()
  await expect(tokens.getByText('My phone', { exact: true })).toHaveCount(0)
  await page.evaluate(() => { location.hash = '#/documents' })
  await expect(mobile).toHaveCount(0)
  await page.evaluate(() => { location.hash = '#/settings' })
  await expect(mobile.getByText('My phone', { exact: true })).toBeVisible()
  await page.reload()
  await expect(mobile.getByText('My phone', { exact: true })).toBeVisible()
})

test('connected mobile apps show only owned pairing entries and support retry and revoke', async ({ page }) => {
  await mockAPI(page)
  const phoneName = 'My iPhone with a long descriptive device name for this archive'
  let rows = [
    { id: 2, user_id: 1, source: 'mobile_pairing', name: phoneName, scopes: 'documents:read,documents:write', created_at: 1780100000, last_used_at: 1780200000 },
    { id: 3, user_id: 2, source: 'mobile_pairing', name: 'Other account phone', scopes: 'documents:read,documents:write', created_at: 1780100000 },
    { id: 4, user_id: 1, name: 'Suchi mobile', scopes: 'documents:read', created_at: 1780100000 },
  ]
  let reads = 0
  let revokes = 0
  await page.route('**/api/tokens/', route => ++reads === 1
    ? route.fulfill({ status: 503, json: { error: 'Connected apps unavailable' } })
    : route.fulfill({ json: { results: rows } }))
  await page.route('**/api/tokens/2', route => {
    if (++revokes === 1) return route.fulfill({ status: 503, json: { error: 'Revoke failed. Try again.' } })
    rows = rows.filter(row => row.id !== 2)
    return route.fulfill({ status: 204 })
  })
  await page.goto('/#/settings')
  const mobile = page.getByRole('region', { name: 'Mobile app', exact: true })
  const tokens = page.getByRole('region', { name: 'API tokens', exact: true })
  await expect(mobile.getByRole('alert')).toContainText('Connected apps unavailable')
  await expect(mobile.getByText('No connected mobile apps.', { exact: false })).toHaveCount(0)
  await mobile.getByRole('button', { name: 'Retry', exact: true }).click()
  await expect(mobile.getByText(phoneName, { exact: true })).toBeVisible()
  await expect(mobile.locator('.row-date')).toContainText('Connected')
  await expect(mobile.locator('.row-date')).toContainText('Last used')
  await expect(mobile.getByText('Other account phone', { exact: true })).toHaveCount(0)
  await expect(mobile.getByText('Suchi mobile', { exact: true })).toHaveCount(0)
  await expect(tokens.getByText('Other account phone', { exact: true })).toBeVisible()
  await expect(tokens.getByText('Suchi mobile', { exact: true })).toBeVisible()
  await page.addStyleTag({ content: 'html { font-size: 24px !important; }' })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  page.on('dialog', dialog => dialog.accept())
  await mobile.getByRole('button', { name: 'Revoke', exact: true }).click()
  await expect(page.getByText('Revoke failed. Try again.', { exact: true })).toBeVisible()
  await expect(mobile.getByText(phoneName, { exact: true })).toBeVisible()
  await mobile.getByRole('button', { name: 'Revoke', exact: true }).click()
  await expect(mobile.locator('.token-row')).toHaveCount(0)
  await expect(mobile.getByText('No connected mobile apps. Pair the app to add one here.')).toBeVisible()
  await page.reload()
  await expect(mobile.locator('.token-row')).toHaveCount(0)
})

test('connected mobile apps ignore a late list from the previous account', async ({ page }) => {
  await mockAPI(page)
  let identityReads = 0
  let tokenReads = 0
  let oldRead
  await page.route('**/api/whoami', route => ++identityReads === 1 ? route.fallback() : route.fulfill({ json: {
    user_id: 2, email: 'second@example.test', display_name: 'Second user', role: 'member', capabilities: [], authn_by: 'local',
  } }))
  await page.route('**/api/tokens/', route => {
    if (++tokenReads === 1) { oldRead = route; return }
    return route.fulfill({ json: { results: [
      { id: 2, user_id: 2, source: 'mobile_pairing', name: 'Second account phone', scopes: 'documents:read,documents:write', created_at: 1780100000 },
    ] } })
  })
  await page.goto('/#/settings')
  await expect.poll(() => !!oldRead).toBe(true)
  await page.getByRole('button', { name: 'Save profile', exact: true }).click()
  const mobile = page.getByRole('region', { name: 'Mobile app', exact: true })
  await expect(mobile.getByText('Second account phone', { exact: true })).toBeVisible()
  await oldRead.fulfill({ json: { results: [
    { id: 1, user_id: 1, source: 'mobile_pairing', name: 'Previous private phone', scopes: 'documents:read,documents:write', created_at: 1780100000 },
  ] } })
  await expect(page.getByText('Previous private phone', { exact: true })).toHaveCount(0)
  await expect(mobile.getByText('Second account phone', { exact: true })).toBeVisible()
})

for (const clipboard of ['available', 'denied']) {
  test(`pairs mobile with a private expiring QR and ${clipboard} clipboard fallback`, async ({ page }) => {
    await mockAPI(page)
    await page.addInitScript(denied => {
      Object.defineProperty(navigator, 'clipboard', { configurable: true, value: {
        writeText: async text => {
          if (denied) throw new Error('Clipboard denied')
          window.copiedPairing = text
        },
      } })
    }, clipboard === 'denied')
    const code = 'a'.repeat(64)
    const pairingURL = `suchi://pair?v=1&server=https%3A%2F%2Farchive.example.test&code=${code}`
    const requests = []
    await page.route('**/api/mobile/pairing', async route => {
      requests.push({ method: route.request().method(), body: route.request().postDataJSON() })
      if (route.request().method() === 'DELETE') return route.fulfill({ status: 204 })
      return route.fulfill({ status: 201, json: {
        pairing_url: pairingURL, code, name: 'My phone', expires_at: Math.floor(Date.now() / 1000) + 300,
        qr_data_url: 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII=',
      } })
    })
    await page.goto('/#/settings')
    await page.getByRole('button', { name: 'Pair mobile app', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: 'Pair mobile app', exact: true })
    await expect(dialog).toBeVisible()
    expect(requests).toEqual([])
    await dialog.getByLabel('Device name').fill('My phone')
    await dialog.getByRole('button', { name: 'Generate QR code' }).click()
    await expect(dialog.getByAltText('Scan this QR code in the Suchi app to pair with this archive')).toBeVisible()
    await expect(dialog.getByText(/Expires in .* Usable once/)).toBeVisible()
    await expect(dialog.getByText(/document read and write access/)).toBeVisible()
    await expect(dialog.getByText(/sign in manually/)).toBeVisible()
    const link = dialog.getByLabel('Can’t scan? Paste this pairing link in the app.')
    await expect(link).toHaveValue(pairingURL)
    await dialog.getByRole('button', { name: 'Copy pairing link' }).click()
    await expect(dialog.getByRole('status')).toHaveText(clipboard === 'denied'
      ? 'Select the pairing link below and copy it manually.' : 'Pairing link copied.')
    if (clipboard === 'available') expect(await page.evaluate(() => window.copiedPairing)).toBe(pairingURL)
    await link.click()
    expect(await link.evaluate(input => input.selectionEnd - input.selectionStart)).toBe(pairingURL.length)
    await dialog.getByRole('button', { name: 'Done', exact: true }).click()
    await expect(dialog).toHaveCount(0)
    await expect.poll(() => requests).toEqual([
      { method: 'POST', body: { name: 'My phone' } },
      { method: 'DELETE', body: { code } },
    ])
  })
}

test('retries failed mobile pairing and removes expired QR secrets', async ({ page }) => {
  await mockAPI(page)
  let attempts = 0
  await page.route('**/api/mobile/pairing', async route => {
    if (route.request().method() === 'DELETE') return route.fulfill({ status: 204 })
    if (++attempts === 1) return route.fulfill({ status: 503, json: { message: 'Pairing unavailable. Try again.' } })
    return route.fulfill({ status: 201, json: {
      pairing_url: 'suchi://pair?expired-secret', code: 'b'.repeat(64),
      expires_at: Math.floor(Date.now() / 1000) - 1, qr_data_url: 'data:image/png;base64,',
    } })
  })
  await page.goto('/#/settings')
  await page.getByRole('button', { name: 'Pair mobile app', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Pair mobile app', exact: true })
  await dialog.getByRole('button', { name: 'Generate QR code' }).click()
  await expect(dialog.getByRole('alert')).toHaveText('Pairing unavailable. Try again.')
  await dialog.getByRole('button', { name: 'Generate QR code' }).click()
  await expect(dialog.getByRole('status')).toHaveText('This code has expired. Generate a new code to pair another device.')
  await expect(dialog.locator('img')).toHaveCount(0)
  await expect(dialog.locator('#pairing-link')).toHaveCount(0)
  await expect(dialog.getByRole('button', { name: 'Generate new code' })).toBeEnabled()
  await page.keyboard.press('Escape')
  await expect(dialog).toHaveCount(0)
})

test('cancels mobile codes that finish after closing the prompt', async ({ page }) => {
  await mockAPI(page)
  let pending
  let cancelled
  await page.route('**/api/mobile/pairing', async route => {
    if (route.request().method() === 'DELETE') {
      cancelled = route.request().postDataJSON()
      return route.fulfill({ status: 204 })
    }
    pending = route
  })
  await page.goto('/#/settings')
  await page.getByRole('button', { name: 'Pair mobile app', exact: true }).click()
  await page.getByRole('button', { name: 'Generate QR code' }).click()
  await expect.poll(() => !!pending).toBe(true)
  await page.getByRole('button', { name: 'Close mobile pairing' }).click()
  const code = 'c'.repeat(64)
  await pending.fulfill({ status: 201, json: {
    code, pairing_url: 'suchi://pair?late-secret', expires_at: Math.floor(Date.now() / 1000) + 300,
  } })
  await expect.poll(() => cancelled).toEqual({ code })
  await expect(page.getByRole('dialog', { name: 'Pair mobile app', exact: true })).toHaveCount(0)
  await expect(page.getByText('suchi://pair?late-secret')).toHaveCount(0)
})

for (const refreshedUserID of [1, 2]) {
  test(`clears mobile pairing secrets when the session refreshes as user ${refreshedUserID}`, async ({ page }) => {
    await mockAPI(page)
    let identityReads = 0
    let pendingIdentity
    let creates = 0
    const code = 'd'.repeat(64)
    const pairingURL = `suchi://pair?v=1&server=https%3A%2F%2Farchive.example.test&code=${code}`
    await page.route('**/api/whoami', async route => {
      if (++identityReads === 1) return route.fallback()
      pendingIdentity = route
    })
    await page.route('**/api/mobile/pairing', async route => {
      if (route.request().method() === 'DELETE') return route.fulfill({ status: 204 })
      creates++
      return route.fulfill({ status: 201, json: {
        code, pairing_url: pairingURL, expires_at: Math.floor(Date.now() / 1000) + 300,
        qr_data_url: 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII=',
      } })
    })
    await page.goto('/#/settings')
    await page.getByRole('button', { name: 'Save profile', exact: true }).click()
    await expect.poll(() => !!pendingIdentity).toBe(true)
    await page.getByRole('button', { name: 'Pair mobile app', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: 'Pair mobile app', exact: true })
    await dialog.getByRole('button', { name: 'Generate QR code' }).click()
    await expect(dialog.locator('#pairing-link')).toHaveValue(pairingURL)

    await pendingIdentity.fulfill({ json: {
      user_id: refreshedUserID, email: 'refreshed@example.test', display_name: 'Refreshed user',
      role: 'member', capabilities: [], authn_by: 'local',
    } })
    await expect(dialog).toHaveCount(0)
    await expect(page.locator('#pairing-link')).toHaveCount(0)
    await expect(page.getByRole('button', { name: 'Copy pairing link' })).toHaveCount(0)
    await page.getByRole('button', { name: 'Pair mobile app', exact: true }).click()
    await expect(dialog).toBeVisible()
    await expect(dialog.locator('#pairing-link')).toHaveCount(0)
    await expect(dialog.locator('img')).toHaveCount(0)
    expect(creates).toBe(1)
  })
}

test('route changes cancel and clear an open mobile pairing code', async ({ page }) => {
  await mockAPI(page)
  const code = 'e'.repeat(64)
  let cancelled
  await page.route('**/api/mobile/pairing', async route => {
    if (route.request().method() === 'DELETE') {
      cancelled = route.request().postDataJSON()
      return route.fulfill({ status: 204 })
    }
    return route.fulfill({ status: 201, json: {
      code, pairing_url: 'suchi://pair?route-secret', expires_at: Math.floor(Date.now() / 1000) + 300,
      qr_data_url: 'data:image/png;base64,',
    } })
  })
  await page.goto('/#/settings')
  await page.getByRole('button', { name: 'Pair mobile app', exact: true }).click()
  await page.getByRole('button', { name: 'Generate QR code' }).click()
  await expect(page.locator('#pairing-link')).toHaveValue('suchi://pair?route-secret')
  await page.evaluate(() => { location.hash = '#/documents' })
  await expect(page.getByRole('dialog', { name: 'Pair mobile app', exact: true })).toHaveCount(0)
  await expect.poll(() => cancelled).toEqual({ code })
  await expect(page.locator('#pairing-link')).toHaveCount(0)
})

test('groups metadata reviews by document', async ({ page }) => {
  await mockAPI(page)

  await page.goto('/#/tasks')
  await expect(page.getByRole('link', { name: 'HDFC receipt.pdf', exact: true })).toBeVisible()
  await expect(page.getByText('24 Receipts', { exact: true })).toBeVisible()
  await expect(page.getByText('2 suggestions')).toBeVisible()
  await expect(page.getByText('Add “banking” tag?')).toBeVisible()
  await expect(page.getByText('Set correspondent to “HDFC Bank”?')).toBeVisible()
  await expect(page.getByText('document-change', { exact: true })).toHaveCount(0)
  await expect(page.getByText('Details', { exact: true })).toHaveCount(0)
  await expect(page.getByText('user:1', { exact: true })).toHaveCount(0)
  if ((page.viewportSize()?.width || 0) > 1050) {
    expect(Math.round((await page.locator('.approval-grid[data-approval-kind="workflow"]').boundingBox()).width)).toBeLessThanOrEqual(820)
  }
})

test('keeps filing reviews compact and explains sources on demand', async ({ page }) => {
  await mockAPI(page, {
    approvalTasks: [{
      id: 11, run_id: 5, approval_id: 1, approval_name: 'document-change',
      doc_id: 233, doc_title: 'Mobile App Document Scan Queue Screen',
      doc_jd_category_id: 49, doc_jd_category_code: 49, doc_jd_category_name: 'Inbox',
      doc_has_thumbnail: false, state_key: 'review', assignee: 'user:1',
      prompt: 'Review suggested category', choices: ['apply', 'reject'],
      status: 'open', created_at: 1780100000,
      vars: {
        field: 'jd_category', current_value: 'Inbox', proposed_value: 'Receipts',
        confidence: 0.76, source: 'archive', reason: 'review_first',
        policy_version: 'review-first-v1', source_current: true, review_conflict: false,
        sources: [
          { document_id: 14, title: 'Swiggy Tax Invoice' },
          { document_id: 15, title: 'H&M Online Shopping Tax Invoice' },
        ],
      },
    }],
  })

  await page.goto('/#/tasks')
  const review = page.locator('.decision-row')
  await expect(review.getByText('File under “Receipts”?', { exact: true })).toBeVisible()
  await expect(review.locator('.metadata-transition')).toHaveText(/Inbox\s*→\s*Receipts/)
  await expect(review.getByText('Current', { exact: true })).toHaveCount(0)
  await expect(review.getByText('Proposed', { exact: true })).toHaveCount(0)
  await expect(review.getByText('Producer detail', { exact: true })).toHaveCount(0)

  await expect(review.getByText('Why this was suggested', { exact: true })).toBeVisible()
  await expect(review.getByText('Archive match', { exact: true })).toBeVisible()
  const rationale = review.getByText('Similar documents (2)', { exact: true })
  await expect(rationale).toBeVisible()
  await expect(review.getByRole('link', { name: 'Swiggy Tax Invoice' })).not.toBeVisible()
  await rationale.click()
  await expect(review.getByRole('link', { name: 'Swiggy Tax Invoice' })).toBeVisible()
  await expect(review.getByRole('link', { name: 'H&M Online Shopping Tax Invoice' })).toBeVisible()
  await expect(review.getByRole('button', { name: 'File document', exact: true })).toBeVisible()
  await expect(review.getByRole('button', { name: 'Dismiss', exact: true })).toBeVisible()
})

test('shows dead-job recovery only to administrators', async ({ page }) => {
  const deadJobs = [{
    id: 41, kind: 'post-ingest', doc_id: 17, state: 'dead', attempts: 4,
    last_error: 'document is unavailable', updated_at: 1780100000,
  }]
  const adminIncludes = []
  await mockAPI(page, { deadJobs, taskIncludes: adminIncludes })
  await page.goto('/#/tasks')
  await expect(page.getByRole('heading', { name: 'Dead jobs — needs attention' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Retry', exact: true })).toBeVisible()
  expect(adminIncludes).toContain('jobs')

  await page.unrouteAll({ behavior: 'wait' })
  const memberIncludes = []
  await mockAPI(page, { userRole: 'member', deadJobs, taskIncludes: memberIncludes })
  await page.reload()
  await expect(page.getByRole('link', { name: 'HDFC receipt.pdf', exact: true })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Dead jobs — needs attention' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Retry', exact: true })).toHaveCount(0)
  expect(memberIncludes).toContain('approvals')
  expect(memberIncludes).not.toContain('jobs')
})

test('keeps stale metadata visible, unknown actions read-only, and queued decisions distinct from application', async ({ page }) => {
  const known = {
    id: 31, approval_name: 'document-change', doc_id: 17, doc_title: 'Original title',
    choices: ['apply', 'reject'], prompt: 'Review title',
    vars: { field: 'title', current_value: 'Original title', proposed_value: 'Reviewed title',
      source_current: true, review_conflict: false, reason: 'review_first', confidence: 0.99 },
  }
  await mockAPI(page, { approvalTasks: [known, {
    ...known, id: 32, vars: { ...known.vars, field: 'transfer_money', proposed_value: 'Unknown effect' },
  }] })
  let stale = true
  let resolutions = 0
  await page.route('**/api/approvals/tasks/31/resolve*', async route => {
    resolutions++
    return stale
      ? route.fulfill({ status: 409, json: { code: 'stale_proposal' } })
      : route.fulfill({ status: 204 })
  })
  await page.goto('/#/tasks')
  const titleReview = page.locator('.decision-row').filter({ hasText: 'Change title to' })
  await expect(titleReview.getByLabel('Current Original title; proposed Reviewed title', { exact: true })).toBeVisible()
  const unknown = page.locator('.decision-row').filter({ hasText: 'Unsupported action' })
  await expect(unknown.getByRole('button')).toHaveCount(0)
  await titleReview.getByRole('button', { name: 'Change title' }).click()
  await expect(titleReview.getByRole('alert')).toContainText('Nothing was applied')
  await expect(titleReview.getByRole('button', { name: 'Change title' })).toBeDisabled()
  expect(resolutions).toBe(1)
  stale = false
  await page.getByRole('button', { name: 'Refresh reviews' }).click()
  await titleReview.getByRole('button', { name: 'Change title' }).click()
  await expect(page.locator('.review-toolbar [role="status"]')).toContainText('queued, not yet applied')
  await expect(titleReview).toHaveCount(0)
  await expect(unknown.getByRole('button')).toHaveCount(0)
})

test('lays out more than two workflow approvals in the shared responsive grid', async ({ page }, testInfo) => {
  const approvalTasks = [17, 18, 19].map((docID, index) => ({
    id: 90 + index, run_id: 30 + index, approval_id: 8, approval_name: 'document-change',
    doc_id: docID, doc_title: `Review document ${index + 1}.pdf`, doc_has_thumbnail: false,
    state_key: 'review', assignee: 'user:1', prompt: 'Review suggested document metadata',
    choices: ['apply', 'reject'], status: 'open', created_at: 1780100000,
    vars: { field: 'tag', proposed_value: `review-${index + 1}`, current_value: '', confidence: 0.74, source: 'archive', reason: 'review_first', source_current: true, review_conflict: false },
  }))
  await mockAPI(page, { approvalTasks })
  await page.goto('/#/tasks')

  const grid = page.locator('.approval-grid[data-approval-kind="workflow"]')
  await expect(grid).toHaveClass(/approval-grid-many/)
  await expect(grid.locator('.approval-card')).toHaveCount(3)
  const boxes = await grid.locator('.approval-card').evaluateAll(cards => cards.map(card => {
    const box = card.getBoundingClientRect()
    return { x: Math.round(box.x), width: Math.round(box.width) }
  }))
  if ((page.viewportSize()?.width || 0) > 1050) {
    expect(boxes[0].x).not.toBe(boxes[1].x)
    expect(Math.abs(boxes[0].width - boxes[1].width)).toBeLessThanOrEqual(1)
  } else {
    expect(new Set(boxes.map(box => box.x)).size).toBe(1)
  }
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  await page.screenshot({ path: `/tmp/suchi-workflow-approvals-${testInfo.project.name}.png`, fullPage: true })
})

test('shows affected document titles in rescan details', async ({ page }) => {
  await mockAPI(page, {
    approvalTasks: [{
      id: 11, run_id: 5, approval_id: 2, approval_name: 'rescan-proposal',
      state_key: 'review', assignee: 'role:admin',
      prompt: 'Pipeline rescan available',
      choices: ['approve_all', 'approve_sample', 'dismiss'],
      status: 'open', created_at: 1780100000,
      vars: {
        kind: 'content', current_version: 2, stale_count: 5,
        target_documents: [
          { id: 18, title: 'SBI account statement August 2026.pdf' },
          { id: 20, title: 'HDFC Infinia card statement August 2026.pdf' },
        ],
      },
    }],
  })
  await page.goto('/#/tasks')

  await page.getByText('Affected documents', { exact: true }).click()
  await expect(page.getByText('Text extraction has improved. 5 documents can be updated.', { exact: true })).toBeVisible()
  await expect(page.getByText(/can be updated to v\d/)).toHaveCount(0)
  await expect(page.getByText('Affected documents')).toBeVisible()
  await expect(page.getByRole('link', { name: 'SBI account statement August 2026.pdf' })).toHaveAttribute('href', '#/doc/18')
  await expect(page.getByRole('link', { name: 'HDFC Infinia card statement August 2026.pdf' })).toHaveAttribute('href', '#/doc/20')
  await expect(page.getByText('and 3 more')).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
})

test('persists user and automation switches', async ({ page }) => {
  await mockAPI(page, {

    filingTreeChosen: true,
  })

  await page.goto('/#/settings?tab=archive&section=users')
  const userSwitch = page.getByRole('switch', { name: 'User member@example.test active' })
  await expect(userSwitch).toHaveAttribute('aria-checked', 'true')
  const userPatch = page.waitForRequest(request =>
    request.method() === 'PATCH' && new URL(request.url()).pathname === '/api/admin/users/2')
  await userSwitch.click()
  expect((await userPatch).postDataJSON()).toEqual({ disabled: true })
  await expect(userSwitch).toHaveAttribute('aria-checked', 'false')

  await page.goto('/#/automations')
  const automationSwitch = page.getByRole('switch', { name: 'Tag utility bills enabled' })
  await expect(automationSwitch).toHaveAttribute('aria-checked', 'true')
  const automationPatch = page.waitForRequest(request =>
    request.method() === 'PATCH' && new URL(request.url()).pathname === '/api/automations/7')
  await automationSwitch.click()
  expect((await automationPatch).postDataJSON()).toEqual({ enabled: false })
  await expect(automationSwitch).toHaveAttribute('aria-checked', 'false')
})

test('edits mailbox intake on a narrow screen', async ({ page }) => {
  await mockAPI(page, {

    filingTreeChosen: true,
  })

  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto('/#/settings?tab=archive&section=mail')
  await expect(page.getByRole('heading', { name: 'Email intake' })).toBeVisible()
  await page.getByRole('button', { name: 'Edit' }).nth(2).click()
  const mailboxSwitch = page.getByRole('switch', { name: 'Poll this mailbox' })
  await expect(mailboxSwitch).toHaveAttribute('aria-checked', 'true')
  await mailboxSwitch.click()
  const firstRule = page.getByRole('group', { name: 'Rule 1' })
  await expect(firstRule.getByRole('button', { name: 'Remove rule 1' })).toBeDisabled()
  await firstRule.getByRole('button', { name: 'With files' }).click()
  await firstRule.getByRole('button', { name: 'Files only' }).click()
  await page.getByRole('button', { name: 'Add rule' }).click()
  await expect(page.getByText('OR', { exact: true })).toBeVisible()
  await expect(page.getByText('If rules overlap, Email and files wins.')).toBeVisible()
  const secondRule = page.getByRole('group', { name: 'Rule 2' })
  await expect(secondRule.getByRole('button', { name: 'Matching', exact: true })).toHaveAttribute('aria-pressed', 'true')
  await page.getByRole('button', { name: 'Preview matches' }).click()
  await expect(page.getByText('Rule 2: add at least one matching condition.')).toBeVisible()
  await secondRule.getByLabel('Subject contains').fill('distribution advice')
  await secondRule.getByRole('button', { name: 'Email and files' }).click()
  const previewRequest = page.waitForRequest(request =>
    request.method() === 'POST' && new URL(request.url()).pathname === '/api/email-accounts/3/preview')
  await page.getByRole('button', { name: 'Preview matches' }).click()
  expect((await previewRequest).postDataJSON().intake_policy).toEqual({
    rules: [
      { selection: 'files', content: 'files_only' },
      { selection: 'matching', content: 'email_and_files', subject_terms: 'distribution advice' },
    ],
  })
  await expect(page.getByText('2 of 10 new messages match')).toBeVisible()
  await expect(page.getByText('August invoice')).toBeVisible()
  await page.getByText('Advanced', { exact: true }).click()
  await expect(page.getByLabel('Host')).toHaveValue('imap.gmail.com')
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  const mailboxPatch = page.waitForRequest(request =>
    request.method() === 'PATCH' && new URL(request.url()).pathname === '/api/email-accounts/3')
  await page.getByRole('button', { name: 'Save changes' }).click()
  const mailboxBody = (await mailboxPatch).postDataJSON()
  expect(mailboxBody.enabled).toBe(false)
  expect(mailboxBody.intake_policy).toEqual({
    rules: [
      { selection: 'files', content: 'files_only' },
      { selection: 'matching', content: 'email_and_files', subject_terms: 'distribution advice' },
    ],
  })
})

test('keeps search separate from scoped archive research and saves exact sources', async ({ page }, testInfo) => {
  const chatRequests = []
  const autocompleteQueries = []
  await mockAPI(page, {
    chatEnabled: true,
    chatRequests,
    autocompleteQueries,

    filingTreeChosen: true,
    chatResponse: {
      answer: 'The lease renews in September [1].',
      sources: [{ id: 17, title: 'Lease agreement.pdf', snippet: 'The renewal date is September 1.', sensitivity: 'internal' }],
      citations: [1],
      grounded: true,
      intelligence: { accepted: {}, pending: {} },
    },
  })

  await page.goto('/#/dashboard')
  const omnibox = page.getByLabel('Search or run a command')
  await expect(page.getByRole('button', { name: 'Ask the archive' })).toBeVisible()
  expect(await page.evaluate(() => performance.getEntriesByType('resource').some(entry => entry.name.includes('ArchiveChat')))).toBe(false)

  await omnibox.fill('tag:renewal')
  await page.waitForTimeout(250)
  expect(autocompleteQueries).toEqual([])
  await omnibox.fill('lease renewal')
  await omnibox.press('Enter')
  await expect(page).toHaveURL(/#\/search\?q=lease%20renewal$/)
  expect(chatRequests).toHaveLength(0)

  await page.goto('/#/dashboard')
  await omnibox.fill('When does the lease renew?')
  await page.getByRole('button', { name: 'Ask the archive' }).click()
  await expect(page.getByRole('dialog', { name: 'Archive research' })).toBeVisible()
  if ((page.viewportSize()?.width || 0) > 640) {
    await expect.poll(async () => Math.round((await page.getByRole('dialog', { name: 'Archive research' }).boundingBox()).width)).toBe(520)
    await expect.poll(async () => {
      const box = await page.getByRole('dialog', { name: 'Archive research' }).boundingBox()
      return Math.round(box.x + box.width)
    }).toBe(page.viewportSize().width)
  } else {
    await expect.poll(async () => Math.round((await page.getByRole('dialog', { name: 'Archive research' }).boundingBox()).x)).toBe(0)
    await expect.poll(async () => Math.round((await page.getByRole('dialog', { name: 'Archive research' }).boundingBox()).width)).toBe(page.viewportSize().width)
  }
  await page.screenshot({ path: `/tmp/suchi-archive-chat-${testInfo.project.name}.png`, fullPage: true })
  await expect(page.getByText('The lease renews in September')).toBeVisible()
  await expect(page.getByText('Ask questions about your documents.')).toBeVisible()
  await expect(page.getByText('Grounded answer', { exact: true })).toHaveCount(0)
  const citation = page.getByRole('link', { name: 'Open cited document 1: Lease agreement.pdf' })
  await expect(citation).toHaveAttribute('href', '#/doc/17')
  await citation.click()
  await expect(page).toHaveURL(/#\/doc\/17$/)
  await expect(page.getByRole('listbox')).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Return to archive research' })).toBeVisible()
  await page.screenshot({ path: `/tmp/suchi-archive-chat-ribbon-${testInfo.project.name}.png`, fullPage: true })
  expect(chatRequests).toHaveLength(1)
  expect(chatRequests[0]).toMatchObject({
    question: 'When does the lease renew?',
    include_sensitive: false,
    history: [],
    context_source_ids: [],
    scope: { query: '', document_ids: [], jd_category_id: 0 },
  })
  expect(await page.evaluate(() => performance.getEntriesByType('resource').some(entry => entry.name.includes('ArchiveChat')))).toBe(true)

  await page.getByRole('button', { name: 'Return to archive research' }).click()
  await expect(page.getByRole('dialog', { name: 'Archive research' })).toBeVisible()
  await expect(page.getByText('The lease renews in September')).toBeVisible()
  await page.getByTitle('Close', { exact: true }).click()
  const resumedRibbon = page.getByRole('button', { name: 'Return to archive research' })
  await expect(resumedRibbon).toBeVisible()
  await expect(resumedRibbon).toBeFocused()
  await resumedRibbon.click()
  await page.getByRole('link', { name: /Save retrieved documents as a view/ }).click()
  await expect(page).toHaveURL(/#\/views\?new=1&ids=17$/)
  await expect(page.getByRole('dialog', { name: 'Create a view' })).toBeVisible()
  await expect(page.getByText('Exact research snapshot')).toBeVisible()
  await page.getByRole('button', { name: 'Close create view' }).click()
  await expect(page).toHaveURL(/#\/views$/)
  await expect(page.getByRole('button', { name: 'Return to archive research' })).toBeVisible()

  await page.goto('/#/dashboard')
  await page.getByRole('button', { name: 'Return to archive research' }).click()
  await page.getByRole('link', { name: /Save retrieved documents as a view/ }).click()
  await expect(page).toHaveURL(/#\/views\?new=1&ids=17$/)
  await expect(page.getByRole('dialog', { name: 'Create a view' })).toBeVisible()
  await page.getByRole('button', { name: 'Close create view' }).click()
  await expect(page).toHaveURL(/#\/views$/)

  await page.goto('/#/dashboard')
  await page.getByRole('button', { name: 'Return to archive research' }).click()
  await expect(page.getByText('The lease renews in September')).toBeVisible()
  const source = page.getByRole('link', { name: 'Open source 1: Lease agreement.pdf' })
  await expect(source).toHaveAttribute('href', '#/doc/17')
  await source.click()
  await expect(page).toHaveURL(/#\/doc\/17$/)
  await expect(page.getByRole('listbox')).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Return to archive research' })).toBeVisible()

  await page.getByRole('button', { name: 'Return to archive research' }).click()
  const sensitive = page.getByLabel('Include Confidential and Restricted')
  await sensitive.check()
  await page.getByRole('button', { name: 'Clear' }).click()
  await expect(page.getByText('Start with a question, not a search query')).toBeVisible()
  await expect(sensitive).not.toBeChecked()
})

test('supports cancellation, focus return, and the full-screen mobile research desk', async ({ page }) => {
  await mockAPI(page, {
    chatEnabled: true,
    chatRequests: [],

    filingTreeChosen: true,
    chatResponse: { delay: 5000, answer: 'Too late', sources: [] },
  })
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto('/#/dashboard')
  const omnibox = page.getByLabel('Search or run a command')
  await omnibox.fill('slow question')
  await page.getByRole('button', { name: 'Ask the archive' }).click()
  const dialog = page.getByRole('dialog', { name: 'Archive research' })
  await expect(page.getByText('Reading the documents…')).toBeVisible()
  await expect(dialog).toBeVisible()
  await expect.poll(async () => (await dialog.boundingBox()).x).toBe(0)
  await expect.poll(async () => Math.round((await dialog.boundingBox()).width)).toBe(390)
  await page.getByRole('button', { name: 'Cancel' }).click()
  await expect(page.getByText('Request canceled.')).toBeVisible()
  await expect(page.getByRole('textbox', { name: 'Question', exact: true })).toHaveValue('slow question')
  await page.getByRole('textbox', { name: 'Question', exact: true }).press('Escape')
  await expect(dialog).toBeHidden()
  await expect(omnibox).toBeFocused()
  await expect(page.getByRole('listbox')).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Return to archive research' })).toHaveCount(0)
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
})

test('closes archive research outside without reopening the Omnibox menu', async ({ page }) => {
  await mockAPI(page, {
    chatEnabled: true,

    filingTreeChosen: true,
  })
  await page.setViewportSize({ width: 1280, height: 800 })
  await page.goto('/#/dashboard')
  const omnibox = page.getByLabel('Search or run a command')
  await omnibox.fill('outside close')
  await page.getByRole('button', { name: 'Ask the archive' }).click()
  const dialog = page.getByRole('dialog', { name: 'Archive research' })
  await expect(dialog).toBeVisible()
  await page.locator('.research-veil').click({ position: { x: 200, y: 200 } })
  await expect(dialog).toBeHidden()
  await expect(omnibox).toBeFocused()
  await expect(page.getByRole('listbox')).toHaveCount(0)
})

for (const [code, message] of Object.entries({
  invalid_provider_response: 'The model returned an answer without valid citations. Try again.',
  provider_response_truncated: 'The model reached its output limit before finishing. Try a narrower question or another model.',
})) {
  test(`explains ${code} without exposing provider details`, async ({ page }) => {
    await mockAPI(page, {
      chatEnabled: true,
      failPaths: ['/api/chat'],
      failureStatus: 502,
      failureCode: code,
      failureMessage: 'upstream service error',

      filingTreeChosen: true,
    })
    await page.goto('/#/dashboard')
    const omnibox = page.getByLabel('Search or run a command')
    await omnibox.fill('How much did I spend?')
    await page.getByRole('button', { name: 'Ask the archive' }).click()
    await expect(page.getByText(message)).toBeVisible()
    await expect(page.getByText('upstream service error')).toHaveCount(0)
  })
}

test('publishes exact Inbox, Documents, and Search scopes to archive research', async ({ page }) => {
  const chatRequests = []
  const inbox = { id: 9, area_code: 10, area_name: 'Intake', code: 10, name: 'Inbox', system: true }
  await mockAPI(page, {
    chatEnabled: true,
    chatRequests,
    jdCategories: [inbox],

    filingTreeChosen: true,
  })

  async function ask(question) {
    await page.getByRole('button', { name: 'Ask the archive' }).click()
    const composer = page.getByRole('textbox', { name: 'Question', exact: true })
    await composer.fill(question)
    await composer.press('Enter')
    await expect(page.getByText('The archive supports this answer')).toBeVisible()
    await composer.press('Escape')
  }

  await page.goto('/#/inbox')
  await expect(page.getByText('Inbox zero.')).toBeVisible()
  await ask('inbox scope')
  expect(chatRequests.at(-1).scope).toMatchObject({ jd_category_id: 9, query: '' })

  await page.goto('/#/documents?q=needle&jd=6&document_ids=41,42&tags__id__in=5,8&correspondents__id__in=7&sensitivity=internal&share_link=active')
  await page.getByRole('button', { name: 'Any date', exact: true }).click()
  await page.getByLabel('Added on or after', { exact: true }).fill('2026-08-01')
  await page.getByLabel('Added on or before', { exact: true }).fill('2026-08-30')
  await page.getByLabel('Added on or before', { exact: true }).press('Tab')
  await page.waitForTimeout(50)
  await ask('full scope')
  expect(chatRequests.at(-1).scope).toEqual({
    query: 'needle', document_ids: [41, 42], jd_category_id: 6,
    sensitivity: 'internal', tag_ids: [5, 8],
    correspondent_ids: [7], created_at_gte: 1785542400,
    created_at_lte: 1788134399, language: '', share_link: 'active',
  })

  await page.goto('/#/search?q=lease&lang=de')
  await expect(page.getByText('Nothing matched.')).toBeVisible()
  await ask('search scope')
  expect(chatRequests.at(-1).scope).toMatchObject({ query: 'lease', language: 'de' })

  await page.getByLabel('Search or run a command').press('Escape')
  await page.getByPlaceholder('Search text or use jd:, tag:, from:…').fill('')
  await page.getByRole('button', { name: 'Search', exact: true }).click()
  await expect(page).toHaveURL(/#\/search\?lang=de$/)
  await ask('cleared search scope')
  expect(chatRequests.at(-1).scope).toEqual({
    query: '', document_ids: [], jd_category_id: 0, sensitivity: '',
    tag_ids: [], correspondent_ids: [],
    created_at_gte: null, created_at_lte: null, language: 'de',
  })
})

test('keeps modified research anchors in the current drawer session', async ({ page }) => {
  await mockAPI(page, {
    chatEnabled: true,
    chatRequests: [],

    filingTreeChosen: true,
  })
  await page.goto('/#/dashboard')
  await page.getByRole('button', { name: 'Ask the archive' }).click()
  const composer = page.getByRole('textbox', { name: 'Question', exact: true })
  await composer.fill('show the evidence')
  await composer.press('Enter')
  const dialog = page.getByRole('dialog', { name: 'Archive research' })
  await expect(page.getByRole('link', { name: 'Open cited document 1: Archive evidence.pdf' })).toBeVisible()

  async function modifiedClick(locator, init) {
    await locator.evaluate((element, eventInit) => {
      element.addEventListener('click', event => event.preventDefault(), { once: true })
      element.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, ...eventInit }))
    }, init)
    await expect(dialog).toBeVisible()
    await expect(page).toHaveURL(/#\/dashboard$/)
    await expect(page.getByRole('button', { name: 'Return to archive research' })).toHaveCount(0)
  }

  await modifiedClick(page.getByRole('link', { name: 'Open cited document 1: Archive evidence.pdf' }), { button: 0, ctrlKey: true })
  await modifiedClick(page.getByRole('link', { name: 'Open source 1: Archive evidence.pdf' }), { button: 1 })
  await modifiedClick(page.getByRole('link', { name: /Save retrieved documents as a view/ }), { button: 0, shiftKey: true })
})

test('bounds long archive research transcripts', async ({ page }) => {
  const chatRequests = []
  await mockAPI(page, {
    chatEnabled: true,
    chatRequests,

    filingTreeChosen: true,
  })
  await page.goto('/#/dashboard')
  await page.getByRole('button', { name: 'Ask the archive' }).click()
  const composer = page.getByRole('textbox', { name: 'Question', exact: true })
  for (let turn = 1; turn <= 21; turn++) {
    await composer.fill(`bounded question ${turn}`)
    await composer.press('Enter')
    await expect.poll(() => chatRequests.length).toBe(turn)
    await expect(composer).toBeEnabled()
  }
  await expect(page.locator('.research-turn')).toHaveCount(20)
  await expect(page.getByText('bounded question 1', { exact: true })).toHaveCount(0)
  await expect(page.getByText('bounded question 21', { exact: true })).toBeVisible()
})

test('resets research boundaries and carries only bounded cited follow-up context', async ({ page }) => {
  const chatRequests = []
  await mockAPI(page, {
    chatEnabled: true,
    chatRequests,

    filingTreeChosen: true,
    chatResponse: (_payload, call) => call === 1 ? {
      answer: `${'界'.repeat(4200)} [2]`,
      sources: [
        { id: 70, title: 'Uncited.pdf', snippet: 'extra', sensitivity: 'public' },
        { id: 71, title: 'Cited.pdf', snippet: 'evidence', sensitivity: 'internal' },
      ],
      citations: [2], grounded: true, intelligence: { accepted: {}, pending: {} },
    } : undefined,
  })

  await page.goto('/#/dashboard')
  await page.getByRole('button', { name: 'Ask the archive' }).click()
  let composer = page.getByRole('textbox', { name: 'Question', exact: true })
  await composer.fill('first question')
  await composer.press('Enter')
  await expect(page.getByRole('link', { name: 'Open source 2: Cited.pdf' })).toBeVisible()
  await composer.fill('follow up')
  await composer.press('Enter')
  await expect.poll(() => chatRequests.length).toBe(2)
  expect(chatRequests[1].history).toEqual([])
  expect(chatRequests[1].context_source_ids).toEqual([71])

  const sensitive = page.getByLabel('Include Confidential and Restricted')
  await sensitive.check()
  await sensitive.uncheck()
  await expect(page.getByText('Start with a question, not a search query')).toBeVisible()
  await expect(sensitive).not.toBeChecked()

  await composer.fill('same scope survives close')
  await composer.press('Enter')
  await expect.poll(() => chatRequests.length).toBe(3)
  await page.getByTitle('Close', { exact: true }).click()
  await page.getByRole('button', { name: 'Ask the archive' }).click()
  await expect(page.getByText('same scope survives close')).toBeVisible()
  await page.getByTitle('Close', { exact: true }).click()

  await page.goto('/#/doc/42')
  await page.getByRole('button', { name: 'Ask the archive' }).click()
  await expect(page.getByText('Start with a question, not a search query')).toBeVisible()
  await expect(page.getByLabel('Include Confidential and Restricted')).not.toBeChecked()
})

test('clearing an active research request invalidates it without restoring text', async ({ page }) => {
  await mockAPI(page, {
    chatEnabled: true,
    chatRequests: [],

    filingTreeChosen: true,
    chatResponse: { delay: 1000, answer: 'late answer', sources: [], citations: [], grounded: false },
  })
  await page.goto('/#/dashboard')
  await page.getByRole('button', { name: 'Ask the archive' }).click()
  const composer = page.getByRole('textbox', { name: 'Question', exact: true })
  await composer.fill('clear this request')
  await composer.press('Enter')
  await page.getByRole('button', { name: 'Clear' }).click()
  await expect(page.getByText('Start with a question, not a search query')).toBeVisible()
  await expect(composer).toHaveValue('')
  await page.waitForTimeout(1100)
  await expect(page.getByText('late answer')).toHaveCount(0)
  await expect(page.getByText('Request canceled.')).toHaveCount(0)
})

test('makes the upload modal inert, focused, trapped, and dismissible', async ({ page }) => {
  await mockAPI(page, { filingTreeChosen: true })
  await page.goto('/#/dashboard')
  const opener = page.getByRole('button', { name: 'Upload documents' })
  await opener.focus()
  await opener.click()
  const dialog = page.getByRole('dialog', { name: 'Upload documents' })
  await expect(dialog).toBeVisible()
  await expect(dialog).toBeFocused()
  await expect(page.locator('.shell')).toHaveAttribute('inert', '')
  await page.keyboard.press(process.platform === 'darwin' ? 'Meta+k' : 'Control+k')
  await expect(page.getByLabel('Search or run a command')).not.toBeFocused()
  const close = page.getByRole('button', { name: 'Close upload' })
  const drop = dialog.getByRole('button', { name: 'Upload documents' })
  await expect(drop).toBeVisible()
  await close.focus()
  await page.keyboard.press('Shift+Tab')
  await expect(drop).toBeFocused()
  await page.keyboard.press('Tab')
  await expect(close).toBeFocused()
  await page.keyboard.press('Escape')
  await expect(dialog).toHaveCount(0)
  await expect(opener).toBeFocused()
  await expect(page.locator('.shell')).not.toHaveAttribute('inert', '')
})

test('reviews dates in a responsive grid with visible actions', async ({ page }, testInfo) => {
  const intelligenceRequests = []
  await mockAPI(page, {
    intelligenceRequests,
    approvalTasks: [],

    filingTreeChosen: true,
    intelligence: [
      {
        id: 71, document_id: 17, document_title: 'Lease agreement.pdf',
        document_has_thumbnail: false, type: 'date', role: 'renewal',
        value: { date: '2026-09-01', precision: 'day' }, sort_value: '2026-09-01',
        raw_text: 'September 1, 2026', evidence_text: 'The lease renews on September 1, 2026.',
        confidence: 0.94, status: 'pending', source_current: true, reason: 'important_fact',
      },
      {
        id: 72, document_id: 17, document_title: 'Lease agreement.pdf',
        document_has_thumbnail: false, type: 'date', role: 'issued',
        value: { date: '2025-08-12', precision: 'day' }, sort_value: '2025-08-12',
        raw_text: '12 August 2025', evidence_text: 'Signed on 12 August 2025.',
        confidence: 0.62, status: 'pending', source_current: true, reason: 'low_confidence',
      },
      {
        id: 73, document_id: 18, document_title: 'Boarding pass.pdf',
        document_has_thumbnail: false, type: 'date', role: 'service',
        value: { date: '2026-08-06', precision: 'day' }, sort_value: '2026-08-06',
        raw_text: '06 Aug 2026', evidence_text: 'Date 06 Aug 2026',
        confidence: 0.95, status: 'pending', source_current: true, reason: 'important_fact',
      },
      {
        id: 74, document_id: 19, document_title: 'Restaurant receipt.pdf',
        document_has_thumbnail: false, type: 'date', role: 'issued',
        value: { date: '2025-03-01', precision: 'day' }, sort_value: '2025-03-01',
        raw_text: '3/1/25', evidence_text: 'Date: 3/1/25, 2:48 PM',
        confidence: 0.98, status: 'pending', source_current: true, reason: 'important_fact',
      },
    ],
  })
  await page.goto('/#/tasks')

  await expect(page.getByRole('heading', { name: 'Check dates before they reach Calendar' })).toBeVisible()
  await expect(page.getByText('0 of 4 dates selected')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Add 0 to Calendar' })).toBeDisabled()
  const reviewGrid = page.locator('.approval-grid[data-approval-kind="date"]')
  await expect(reviewGrid).toHaveClass(/approval-grid-many/)
  const columnCount = await reviewGrid.evaluate(element => getComputedStyle(element).gridTemplateColumns.split(' ').length)
  expect(columnCount).toBe((page.viewportSize()?.width || 0) > 1050 ? 2 : 1)
  const actions = page.getByRole('group', { name: 'Review selected dates' })
  await expect(actions).toBeInViewport()
  expect(await actions.evaluate(element => getComputedStyle(element).position)).toBe('sticky')
  await page.screenshot({ path: `/tmp/suchi-date-review-${testInfo.project.name}.png`, fullPage: true })
  await page.getByText('“Date: 3/1/25, 2:48 PM”', { exact: true }).scrollIntoViewIfNeeded()
  await expect(actions).toBeInViewport()
  await page.getByLabel('Select all dates').check()
  await expect(page.getByText('4 of 4 dates selected')).toBeVisible()
  await page.getByRole('button', { name: 'Add 4 to Calendar' }).click()

  expect(intelligenceRequests).toContainEqual({
    action: 'resolve', candidate_ids: [71, 72, 73, 74], decision: 'accepted',
  })
  await expect(page.getByRole('heading', { name: 'Check dates before they reach Calendar' })).toHaveCount(0)
})

test('does not select stale or unknown facts and preserves failed date decisions for review', async ({ page }) => {
  const candidate = {
    id: 81, document_id: 17, document_title: 'Renewal notice',
    type: 'date', role: 'renewal', status: 'pending', value: { date: '2026-09-20' },
    evidence_text: 'Renewal date: 20 September 2026', source_current: true,
    reason: 'important_fact', confidence: 1,
  }
  await mockAPI(page, {
    approvalTasks: [], filingTreeChosen: true,
    intelligence: [
      candidate,
      { ...candidate, id: 82, source_current: false, reason: 'source_changed' },
      { ...candidate, id: 83, type: 'transfer' },
    ],
  })
  await page.route('**/api/intelligence/resolve*', route => route.fulfill({
    json: { results: [{ id: 81, ok: false, code: 'stale_source' }] },
  }))
  await page.goto('/#/tasks')
  const candidates = page.locator('.intelligence-candidate')
  await expect(candidates.nth(1).getByRole('checkbox')).toBeDisabled()
  await expect(candidates.nth(2).getByRole('checkbox')).toBeDisabled()
  await page.getByLabel('Select all dates').check()
  await expect(page.getByText('1 of 3 dates selected')).toBeVisible()
  await page.getByRole('button', { name: 'Add 1 to Calendar' }).click()
  await expect(candidates.nth(0).getByRole('alert')).toContainText('Nothing was applied')
  await expect(candidates).toHaveCount(3)
  await expect(candidates.nth(0).getByRole('checkbox')).toBeDisabled()
  await expect(page.locator('.review-toolbar [role="status"]')).toContainText('No dates were changed')
})

test('distinguishes automatic dates from reviewed and previously accepted history', async ({ page }) => {
  const intelligenceQueries = []
  const now = new Date()
  const year = now.getFullYear()
  const month = String(now.getMonth() + 1).padStart(2, '0')
  const date = `${year}-${month}-14`
  await mockAPI(page, {

    filingTreeChosen: true,
    intelligenceQueries,
    intelligenceCount: 650,
    intelligence: [{
      id: 81, document_id: 28, document_title: 'Home insurance renewal notice',
      document_has_thumbnail: false, type: 'date', role: 'expiry',
      value: { date, precision: 'day' }, sort_value: date,
      raw_text: date, evidence_text: `Cover expires on ${date}.`,
      confidence: 0.97, status: 'accepted', reviewed_at: 1780200000,
    }, {
      id: 82, document_id: 29, document_title: 'Legacy policy reminder',
      document_has_thumbnail: false, type: 'date', role: 'renewal',
      value: { date, precision: 'day' }, sort_value: date,
      raw_text: date, evidence_text: `Renewal starts on ${date}.`,
      confidence: 0.99, status: 'accepted', reviewed_at: null,
    }, {
      id: 83, document_id: 30, document_title: 'Automatic policy reminder',
      document_has_thumbnail: false, type: 'date', role: 'renewal',
      value: { date, precision: 'day' }, sort_value: date,
      confidence: 0.99, status: 'accepted', reviewed_at: null, reviewed_by: null,
      policy_version: 'threshold-auto-v1', reason: 'confidence_threshold',
    }],
    savedViews: [{
      id: 4, name: 'Quarterly tax review',
      filter_json: '{"q":"invoice","sensitivity":"confidential"}',
      display: 'list', position: 0, created_at: 0, updated_at: 0,
    }],
  })
  await page.goto('/#/calendar')

  await expect(page.getByRole('heading', { name: 'Calendar', level: 2, exact: true })).toBeVisible()
  await expect(page.getByText('Dates from your documents', { exact: true })).toBeVisible()
  await expect(page.getByText('Home insurance renewal notice', { exact: true }).first()).toBeVisible()
  await expect(page.locator('.agenda-event').getByText('Expiry', { exact: true })).toBeVisible()
  await expect(page.locator('.agenda-event').filter({ hasText: 'Home insurance renewal notice' }).getByText('Reviewed', { exact: true })).toBeVisible()
  await expect(page.locator('.agenda-event').filter({ hasText: 'Legacy policy reminder' }).getByText('Previously accepted', { exact: true })).toBeVisible()
  await expect(page.locator('.agenda-event').filter({ hasText: 'Automatic policy reminder' }).getByText('Automatic', { exact: true })).toBeVisible()
  await expect(page.locator('.agenda-event').filter({ hasText: 'Automatic policy reminder' }).getByText('Add to calendar', { exact: true })).toHaveCount(0)
  await expect(page.locator('.agenda-event').filter({ hasText: 'Legacy policy reminder' }).getByText('Add to calendar', { exact: true })).toHaveCount(0)
  await expect(page.getByRole('heading', { name: '3 of 650 dates' })).toBeVisible()
  await expect(page.getByText('Showing the first 500. Narrow the view or date role to see the rest.')).toBeVisible()
  const viewSelect = page.getByLabel('Document view')
  await expect(viewSelect).toContainText('Quarterly tax review')
  await expect(viewSelect).toHaveValue('')
  expect(intelligenceQueries.some(query => !('view_id' in query))).toBe(true)

  await viewSelect.selectOption('4')
  await expect.poll(() => intelligenceQueries.at(-1)).toMatchObject({ view_id: '4', type: 'date', status: 'accepted' })
  expect(intelligenceQueries.at(-1)).not.toHaveProperty('q')
  expect(intelligenceQueries.at(-1)).not.toHaveProperty('sensitivity')
})

test('downloads a reviewed exact-day calendar event only after disclosing its contents', async ({ page }) => {
  const apiRequests = []
  const event = {
    id: 81, document_id: 28, document_title: 'Home insurance renewal',
    type: 'date', role: 'renewal', status: 'accepted', reviewed_at: 1780200000,
    value: { date: '2028-02-29', precision: 'day' }, evidence_text: 'Private policy quote',
  }
  await mockAPI(page, {
    filingTreeChosen: true, apiRequests,
    intelligence: [
      event,
      { ...event, id: 82, document_title: 'Automatic policy', reviewed_at: null, policy_version: 'threshold-auto-v1' },
      { ...event, id: 83, document_title: 'Month-only policy', value: { date: '2028-02-01', precision: 'month' } },
      { ...event, id: 84, document_title: 'Year-only policy', value: { date: '2028-01-01', precision: 'year' } },
      { ...event, id: 85, document_title: 'Legacy policy', reviewed_at: null },
    ],
  })
  await page.goto('/#/calendar?document_ids=28')
  const entry = page.locator('.agenda-event').filter({ hasText: 'Home insurance renewal' })
  const downloads = []
  page.on('download', download => downloads.push(download))
  await expect(page.getByText('Add to calendar', { exact: true })).toHaveCount(1)
  await expect(entry.getByRole('button', { name: 'Download .ics' })).toBeHidden()
  await entry.getByText('Add to calendar', { exact: true }).click()
  await expect(entry.getByText(/synced calendar shares those details with your calendar provider/)).toBeVisible()
  expect(downloads).toHaveLength(0)
  const requestCount = apiRequests.length
  const pendingDownload = page.waitForEvent('download')
  await entry.getByRole('button', { name: 'Download .ics' }).click()
  const download = await pendingDownload
  expect(download.suggestedFilename()).toBe('suchi-date-81-2028-02-29.ics')
  const stream = await download.createReadStream()
  const chunks = []
  for await (const chunk of stream) chunks.push(chunk)
  const contents = Buffer.concat(chunks).toString('utf8').replace(/\r\n[ \t]/g, '')
  expect(contents).toContain('DTSTART;VALUE=DATE:20280229\r\nDTEND;VALUE=DATE:20280301\r\n')
  expect(contents).toContain('SUMMARY:Renewal: Home insurance renewal\r\n')
  expect(contents).toContain(`URL:${new URL(page.url()).origin}/#/doc/28\r\n`)
  expect(contents).not.toContain('Private policy quote')
  expect(apiRequests.slice(requestCount)).toEqual([])
  await expect(entry.getByRole('link', { name: 'Home insurance renewal' })).toHaveAttribute('href', '#/doc/28')
  await expect(page).toHaveURL(/#\/calendar\?document_ids=28$/)
})

test('opens research calendar links across all years and replaces stale calendar filters', async ({ page }) => {
  await page.clock.setFixedTime(new Date('2026-09-15T12:00:00Z'))
  const intelligenceQueries = []
  const dates = [
    {
      document_id: 17, document_title: 'Archived lease', role: 'expiry',
      value: { date: '2024-02-14', precision: 'day' }, reviewed_at: 1708041600,
      evidence_text: 'The lease expired on 14 February 2024.',
    },
    {
      document_id: 18, document_title: 'Future policy', role: 'renewal',
      value: { date: '2028-11-03', precision: 'day' }, reviewed_at: null,
      evidence_text: 'Policy renews on 3 November 2028.',
    },
    { document_id: 19, document_title: 'Different research document', role: 'issued', value: { date: '2031-07-19' } },
    { document_id: 99, document_title: 'Unrelated current policy', role: 'expiry', value: { date: '2026-09-14' } },
  ].map((event, index) => ({
    ...event, id: index + 1, type: 'date', status: 'accepted', confidence: 0.99,
    evidence_text: event.evidence_text || `Recorded date: ${event.value.date}.`,
  }))
  await mockAPI(page, {
    chatEnabled: true, filingTreeChosen: true, intelligenceQueries,
    savedViews: [{ id: 4, name: 'Current insurance', filter_json: '{"document_ids":"99"}' }],
    intelligence: query => {
      const ids = query.document_ids?.split(',').map(Number)
      const results = dates.filter(event =>
        (!ids || ids.includes(event.document_id)) &&
        (!query.view_id || event.document_id === 99) &&
        (!query.role || event.role === query.role) &&
        (!query.sort_from || event.value.date >= query.sort_from) &&
        (!query.sort_to || event.value.date <= query.sort_to))
      return { results, count: results.length }
    },
    chatResponse: {
      answer: 'The archive has dates in separate years [1] [2].',
      sources: [{ id: 17, title: 'Archived lease' }, { id: 18, title: 'Future policy' }],
      citations: [1, 2], grounded: true, intelligence: { accepted: { date: 2 }, pending: {} },
    },
  })
  await page.goto('/#/calendar')
  await page.getByLabel('Document view').selectOption('4')
  await page.getByLabel('Date role').selectOption('expiry')
  await expect.poll(() => intelligenceQueries.at(-1)).toMatchObject({ view_id: '4', role: 'expiry' })
  await expect(page.locator('.agenda-event').getByText('Unrelated current policy', { exact: true })).toBeVisible()

  await page.getByLabel('Search or run a command').fill('Find the dates in these documents')
  await page.getByRole('button', { name: 'Ask the archive' }).click()
  const action = page.getByRole('link', { name: /Open 2 calendar dates/ })
  await expect(action).toHaveAttribute('href', '#/calendar?document_ids=17,18')
  await action.click()
  await expect(page).toHaveURL(/#\/calendar\?document_ids=17,18$/)
  await expect(page.getByRole('dialog', { name: 'Archive research' })).toBeHidden()
  await expect(page.getByText('Dates from 2 selected documents · All months and years', { exact: true })).toBeVisible()
  await expect(page.locator('.month-grid')).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Next month' })).toHaveCount(0)
  await expect(page.getByLabel('Document view')).toHaveCount(0)
  await expect(page.getByLabel('Date role')).toHaveValue('')
  await expect(page.locator('.agenda-event')).toHaveCount(2)
  await expect(page.locator('.agenda-event').filter({ hasText: 'Archived lease' })).toContainText('2024')
  await expect(page.locator('.agenda-event').filter({ hasText: 'Future policy' })).toContainText('2028')
  await expect(page.locator('.agenda-event').filter({ hasText: 'Unrelated current policy' })).toHaveCount(0)
  expect(intelligenceQueries.at(-1)).toMatchObject({ document_ids: '17,18', status: 'accepted', type: 'date', page: '1', page_size: '500' })
  for (const key of ['view_id', 'role', 'sort_from', 'sort_to']) expect(intelligenceQueries.at(-1)).not.toHaveProperty(key)

  await page.getByLabel('Date role').selectOption('expiry')
  await expect(page.locator('.agenda-event')).toHaveCount(1)
  const filteredResearchURL = page.url()
  await page.evaluate(() => { location.hash = '#/calendar?document_ids=19' })
  await expect(page.getByLabel('Date role')).toHaveValue('')
  await expect(page.locator('.agenda-event')).toHaveCount(1)
  await expect(page.locator('.agenda-event')).toContainText('Different research document')
  await expect(page.locator('.agenda-event')).toContainText('2031')
  expect(intelligenceQueries.at(-1)).toMatchObject({ document_ids: '19', page: '1' })
  expect(intelligenceQueries.at(-1)).not.toHaveProperty('role')

  await page.goBack()
  await expect(page).toHaveURL(filteredResearchURL)
  await expect(page.locator('.agenda-event')).toHaveCount(1)
  await expect(page.getByLabel('Date role')).toHaveValue('expiry')
  const fullCalendar = page.getByRole('link', { name: 'Open full calendar', exact: true })
  await expect(fullCalendar).toHaveAttribute('href', '#/calendar')
  await fullCalendar.click()
  await expect(page).toHaveURL(/#\/calendar$/)
  await expect(page.locator('.month-grid')).toBeVisible()
  await expect(page.getByLabel('Document view')).toHaveValue('')
  await expect(page.getByLabel('Date role')).toHaveValue('')
  await expect(page.locator('.agenda-event')).toHaveCount(1)
  await expect(page.locator('.agenda-event')).toContainText('Unrelated current policy')
  expect(intelligenceQueries.at(-1)).toMatchObject({ sort_from: '2026-09-01', sort_to: '2026-09-30' })
  expect(intelligenceQueries.at(-1)).not.toHaveProperty('document_ids')
})

test('keeps calendar date precision and explains model confidence without navigating', async ({ page }, testInfo) => {
  const dates = [
    {
      id: 91, document_id: 17, document_title: 'Archived lease', role: 'expiry',
      value: { date: '2024-02-14', precision: 'day' }, reviewed_at: 1708041600,
      confidence: 0.97, evidence_text: 'The lease expired on 14 February 2024.',
    },
    {
      id: 92, document_id: 18, document_title: 'Annual service schedule', role: 'service',
      value: { date: '2026-06-01', precision: 'month' }, reviewed_at: null,
      confidence: 0.92, evidence_text: 'The next service is due in June 2026; no day is specified.',
    },
    {
      id: 93, document_id: 19, document_title: 'Long-term coverage plan', role: 'renewal',
      value: { date: '2028-01-01', precision: 'year' }, reviewed_at: 1780200000,
      confidence: 0.88, evidence_text: 'Coverage renews in 2028; no month or day is specified.',
    },
  ].map(event => ({ ...event, type: 'date', status: 'accepted' }))
  await mockAPI(page, { filingTreeChosen: true, intelligence: dates })
  await page.goto('/#/calendar?document_ids=17,18,19')
  await expect(page.locator('.agenda-event')).toHaveCount(3)
  const day = page.locator('.agenda-event').filter({ hasText: 'Archived lease' })
  const month = page.locator('.agenda-event').filter({ hasText: 'Annual service schedule' })
  const year = page.locator('.agenda-event').filter({ hasText: 'Long-term coverage plan' })
  await expect(day).toContainText('2024')
  await expect(day.locator('.agenda-date')).toContainText('14')
  await expect(month.locator('.agenda-copy > small')).toHaveText('Jun 2026')
  await expect(month.locator('.agenda-date')).not.toContainText(/\b0?1\b/)
  await expect(month).not.toContainText(/\b0?1\b/)
  await expect(month).not.toContainText(/\b(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun)\b/)
  await expect(year.locator('.agenda-copy > small')).toHaveText('2028')
  await expect(year.locator('.agenda-date')).not.toContainText(/Jan|\b0?1\b/)
  await expect(year).not.toContainText(/\bJan(?:uary)?\b|\b0?1\b/)
  await expect(year).not.toContainText(/\b(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun)\b/)
  await expect(month.getByText('month', { exact: true })).toHaveCount(0)
  await expect(year.getByText('year', { exact: true })).toHaveCount(0)
  const confidence = month.locator('.date-details p')
  await expect(confidence).toBeHidden()

  const status = month.locator('.date-details summary')
  if (testInfo.project.use.hasTouch) await status.tap()
  else { await status.focus(); await page.keyboard.press('Enter') }
  await expect(confidence).toBeVisible()
  await expect(page).toHaveURL(/#\/calendar\?document_ids=17,18,19$/)
  await month.getByRole('link', { name: 'Annual service schedule', exact: true }).click()
  await expect(page).toHaveURL(/#\/doc\/18$/)
})

test('keeps month-only and year-only dates out of calendar day cells', async ({ page }) => {
  await page.clock.setFixedTime(new Date('2026-01-15T12:00:00Z'))
  const intelligenceQueries = []
  await mockAPI(page, {
    filingTreeChosen: true, intelligenceQueries,
    intelligence: [
      { id: 94, document_id: 20, document_title: 'Day-specific notice', value: { date: '2026-01-14', precision: 'day' } },
      { id: 95, document_id: 21, document_title: 'Monthly service plan', value: { date: '2026-01-01', precision: 'month' } },
      { id: 96, document_id: 22, document_title: 'Yearly coverage plan', value: { date: '2026-01-01', precision: 'year' } },
    ].map(event => ({ ...event, type: 'date', role: 'due', status: 'accepted', confidence: 0.95 })),
  })
  await page.goto('/#/calendar')
  await expect(page.locator('.agenda-event')).toHaveCount(3)
  const grid = page.locator('.month-grid')
  await expect(grid.locator('a[href="#/doc/20"]')).toBeVisible()
  await expect(grid.locator('a[href="#/doc/21"], a[href="#/doc/22"]')).toHaveCount(0)
  await expect(grid.locator('.day.has-events')).toHaveCount(1)
  await expect(page.locator('.agenda-event').filter({ hasText: 'Monthly service plan' }).locator('.agenda-copy > small')).toHaveText('Jan 2026')
  await expect(page.locator('.agenda-event').filter({ hasText: 'Yearly coverage plan' })).toContainText('2026')
  expect(intelligenceQueries.at(-1)).toMatchObject({ sort_from: '2026-01-01', sort_to: '2026-01-31' })
})

test('opens a full day agenda and preserves its month and filters across navigation', async ({ page }, testInfo) => {
  await page.clock.setFixedTime(new Date('2026-09-15T12:00:00Z'))
  const intelligenceQueries = []
  const dates = [
    { document_id: 17, document_title: 'Home insurance renewal', reviewed_at: 1767225600, evidence_text: 'Home insurance expires on 1 January 2026.' },
    { document_id: 18, document_title: 'Car insurance policy', evidence_text: 'The vehicle policy expires on 1 January 2026.' },
    { document_id: 19, document_title: 'Storage rental agreement', evidence_text: 'The storage agreement expires on 1 January 2026.' },
    { document_id: 20, document_title: 'Equipment protection cover', evidence_text: 'Equipment cover expires on 1 January 2026.' },
    { document_id: 21, document_title: 'Month-only service plan', value: { date: '2026-01-01', precision: 'month' }, evidence_text: 'The service plan expires in January 2026; no day is specified.' },
    { document_id: 22, document_title: 'Year-only coverage plan', value: { date: '2026-01-01', precision: 'year' }, evidence_text: 'Coverage expires in 2026; no month or day is specified.' },
    { document_id: 23, document_title: 'Next-day expiry', value: { date: '2026-01-02', precision: 'day' }, evidence_text: 'This policy expires on 2 January 2026.' },
    { document_id: 24, document_title: 'Different date role', role: 'issued' },
    { document_id: 25, document_title: 'Outside the selected view', view_id: 5 },
  ].map((event, index) => ({
    id: index + 1, type: 'date', status: 'accepted', role: 'expiry', view_id: 4,
    confidence: 0.92, reviewed_at: null, value: { date: '2026-01-01', precision: 'day' }, ...event,
  }))
  await mockAPI(page, {
    filingTreeChosen: true, intelligenceQueries,
    savedViews: [{ id: 4, name: 'Household policies', filter_json: '{"q":"policy"}' }],
    intelligence: query => {
      const results = dates.filter(event =>
        (!query.view_id || String(event.view_id) === query.view_id) &&
        (!query.role || event.role === query.role) &&
        (!query.precision || event.value.precision === query.precision) &&
        (!query.sort_from || event.value.date >= query.sort_from) &&
        (!query.sort_to || event.value.date <= query.sort_to))
      return { results, count: results.length }
    },
  })
  await page.goto('/#/calendar?month=2025-12')
  await page.getByLabel('Document view').selectOption('4')
  await page.getByLabel('Date role').selectOption('expiry')
  await page.getByRole('button', { name: 'Next month', exact: true }).click()
  await expect(page.getByRole('button', { name: 'January 2026', exact: true })).toBeVisible()
  await expect(page.locator('.agenda-event')).toHaveCount(7)
  await expect(page.getByLabel('Document view')).toHaveValue('4')
  expect(intelligenceQueries.at(-1)).toMatchObject({ view_id: '4', role: 'expiry', sort_from: '2026-01-01', sort_to: '2026-01-31' })
  const monthURL = page.url()
  const dayLink = page.getByRole('link', { name: 'Open agenda for Jan 1, 2026', exact: true })
  const dayCell = page.locator('.day').filter({ has: dayLink })
  await expect(dayCell.locator('a[href^="#/doc/"]')).toHaveCount(3)
  await expect(dayCell.getByRole('link', { name: '+1 more', exact: true })).toBeVisible()
  if (testInfo.project.use.hasTouch) await dayCell.click({ position: { x: 4, y: 4 } })
  else { await dayLink.focus(); await page.keyboard.press('Enter') }
  await expect(page.locator('.month-grid')).toHaveCount(0)
  await expect(page.locator('.agenda-event')).toHaveCount(4)
  await expect(page.getByLabel('Document view')).toHaveValue('4')
  expect(Object.fromEntries(new URLSearchParams(page.url().split('?')[1]))).toEqual({ month: '2026-01', date: '2026-01-01', view_id: '4', role: 'expiry' })
  expect(intelligenceQueries.at(-1)).toMatchObject({
    type: 'date', status: 'accepted', precision: 'day', view_id: '4', role: 'expiry',
    sort_from: '2026-01-01', sort_to: '2026-01-01', page: '1', page_size: '500',
  })
  await expect(page.locator('.agenda-event').filter({ hasText: 'Equipment protection cover' })).toBeVisible()
  await expect(page.locator('.agenda-event').filter({ hasText: /Month-only|Year-only|Next-day|Different date role|Outside the selected view/ })).toHaveCount(0)
  const automatic = page.locator('.agenda-event').filter({ hasText: 'Car insurance policy' })
  await automatic.locator('summary').click()
  await expect(automatic.getByText('LLM Classifier confidence: 92%', { exact: true })).toBeVisible()
  const dayURL = page.url()
  await page.reload()
  await expect(page).toHaveURL(dayURL)
  await expect(page.locator('.agenda-event')).toHaveCount(4)
  await expect(page.getByLabel('Document view')).toHaveValue('4')
  await expect(page.getByLabel('Date role')).toHaveValue('expiry')
  await page.goBack()
  await expect(page).toHaveURL(monthURL)
  await expect(dayLink).toBeVisible()
  await expect(page.getByLabel('Document view')).toHaveValue('4')
  await expect(page.getByLabel('Date role')).toHaveValue('expiry')
  await dayCell.getByRole('link', { name: '+1 more', exact: true }).click()
  await expect(page.locator('.agenda-event')).toHaveCount(4)
  await page.getByRole('link', { name: 'Back to January 2026', exact: true }).click()
  await expect(page).toHaveURL(monthURL)
  await page.locator('.month-grid a[href="#/doc/17"]').click()
  await expect(page).toHaveURL(/#\/doc\/17$/)
  await page.goBack()
  await expect(page).toHaveURL(monthURL)
  await expect(dayLink).toBeVisible()
})

test('pages a directly linked day agenda and resets its page when the role changes', async ({ page }) => {
  const intelligenceQueries = []
  const dates = Array.from({ length: 501 }, (_, index) => ({
    id: index + 1, document_id: index + 100, document_title: `Policy ${index + 1}`,
    type: 'date', status: 'accepted', role: index % 2 ? 'renewal' : 'expiry', confidence: 0.99,
    value: { date: '2026-01-01', precision: 'day' }, evidence_text: `Policy ${index + 1} changes on 1 January 2026.`,
  }))
  await mockAPI(page, {
    filingTreeChosen: true, intelligenceQueries,
    intelligence: query => {
      const rows = dates.filter(event => (!query.role || event.role === query.role) &&
        (!query.sort_from || event.value.date >= query.sort_from) &&
        (!query.sort_to || event.value.date <= query.sort_to))
      const offset = (Number(query.page || 1) - 1) * Number(query.page_size)
      return { count: rows.length, results: rows.slice(offset, offset + Number(query.page_size)) }
    },
  })
  await page.goto('/#/calendar?date=2026-01-01')
  await expect(page.locator('.month-grid')).toHaveCount(0)
  await expect(page.locator('.agenda-event')).toHaveCount(500)
  await expect(page.getByRole('button', { name: 'Previous', exact: true })).toBeDisabled()
  await page.getByRole('button', { name: 'Next', exact: true }).click()
  await expect(page.locator('.agenda-event')).toHaveCount(1)
  await expect(page.locator('.agenda-event')).toContainText('Policy 501')
  await expect(page.getByRole('button', { name: 'Next', exact: true })).toBeDisabled()
  expect(intelligenceQueries.at(-1)).toMatchObject({ precision: 'day', sort_from: '2026-01-01', sort_to: '2026-01-01', page: '2', page_size: '500' })
  await page.getByLabel('Date role').selectOption('expiry')
  await expect(page.locator('.agenda-event')).toHaveCount(251)
  expect(intelligenceQueries.at(-1)).toMatchObject({ role: 'expiry', precision: 'day', sort_from: '2026-01-01', sort_to: '2026-01-01', page: '1' })
  await expect(page.getByRole('navigation', { name: 'Date pages' })).toHaveCount(0)
  await page.getByRole('link', { name: 'Back to January 2026', exact: true }).click()
  await expect(page.locator('.month-grid')).toBeVisible()
  await expect(page.getByLabel('Date role')).toHaveValue('expiry')
  await page.getByRole('link', { name: 'Open agenda for Jan 2, 2026', exact: true }).click()
  await expect(page.getByText('No dates this day', { exact: true })).toBeVisible()
  await expect(page.locator('.month-grid')).toHaveCount(0)
  expect(intelligenceQueries.at(-1)).toMatchObject({ role: 'expiry', precision: 'day', sort_from: '2026-01-02', sort_to: '2026-01-02', page: '1' })
})

test('pages all scoped calendar dates and resets pagination when the role changes', async ({ page }) => {
  const intelligenceQueries = []
  let shrinkCount = false
  const dates = Array.from({ length: 501 }, (_, index) => ({
    id: index + 1, document_id: 17, document_title: 'Multi-year document',
    type: 'date', status: 'accepted', role: index % 2 ? 'renewal' : 'expiry', confidence: 0.99,
    value: { date: `${2024 + Math.floor(index / 336)}-${String(Math.floor(index / 28) % 12 + 1).padStart(2, '0')}-${String(index % 28 + 1).padStart(2, '0')}` },
    evidence_text: `Date occurrence ${index + 1}.`,
  }))
  await mockAPI(page, {
    filingTreeChosen: true, intelligenceQueries,
    intelligence: query => {
      let rows = dates.filter(event => !query.role || event.role === query.role)
      if (shrinkCount) rows = rows.slice(0, 1)
      const offset = (Number(query.page || 1) - 1) * Number(query.page_size)
      return { count: rows.length, results: rows.slice(offset, offset + Number(query.page_size)) }
    },
  })
  await page.goto('/#/calendar?document_ids=17')
  await expect(page.locator('.agenda-event')).toHaveCount(500)
  await expect(page.getByRole('button', { name: 'Previous', exact: true })).toBeDisabled()
  await page.getByRole('button', { name: 'Next', exact: true }).click()
  await expect(page.locator('.agenda-event')).toHaveCount(1)
  await expect(page.locator('.agenda-event')).toContainText('Date occurrence 501.')
  expect(intelligenceQueries.at(-1)).toMatchObject({ page: '2' })
  await expect(page.getByRole('button', { name: 'Next', exact: true })).toBeDisabled()
  await page.getByRole('button', { name: 'Previous', exact: true }).click()
  await expect(page.locator('.agenda-event')).toHaveCount(500)
  await page.getByRole('button', { name: 'Next', exact: true }).click()
  await expect(page.locator('.agenda-event')).toHaveCount(1)
  await page.getByLabel('Date role').selectOption('expiry')
  await expect(page.locator('.agenda-event')).toHaveCount(251)
  expect(intelligenceQueries.at(-1)).toMatchObject({ role: 'expiry', page: '1' })
  await page.getByLabel('Date role').selectOption('')
  await expect(page.locator('.agenda-event')).toHaveCount(500)
  const beforeShrink = intelligenceQueries.length
  shrinkCount = true
  await page.getByRole('button', { name: 'Next', exact: true }).click()
  await expect(page.locator('.agenda-event')).toHaveCount(1)
  await expect(page.locator('.agenda-event')).toContainText('Date occurrence 1.')
  expect(intelligenceQueries.slice(beforeShrink).map(query => query.page)).toEqual(['2', '1'])
  await expect(page.getByRole('navigation', { name: 'Date pages' })).toHaveCount(0)
  for (const query of intelligenceQueries) {
    expect(query).toMatchObject({ document_ids: '17', status: 'accepted', type: 'date', page_size: '500' })
    expect(query).not.toHaveProperty('sort_from')
    expect(query).not.toHaveProperty('sort_to')
  }
})

test('shows scoped calendar loading and retry before its empty state', async ({ page }) => {
  await mockAPI(page, { filingTreeChosen: true })
  let pending
  await page.route('**/api/intelligence/?*', route => { pending = route })
  await page.goto('/#/calendar?document_ids=17')
  await expect.poll(() => !!pending).toBe(true)
  await expect(page.locator('.agenda-skeleton').first()).toBeVisible()
  await expect(page.locator('.agenda-empty')).toHaveCount(0)
  await pending.fulfill({ status: 503, json: { error: 'Date service unavailable' } })
  await expect(page.getByText('Date service unavailable', { exact: true })).toBeVisible()
  await expect(page.locator('.agenda-event')).toHaveCount(0)
  pending = undefined
  await page.getByRole('button', { name: 'Retry', exact: true }).click()
  await expect.poll(() => !!pending).toBe(true)
  await pending.fulfill({ json: { results: [], count: 0 } })
  await expect(page.getByText('No matching dates', { exact: true })).toBeVisible()
  await expect(page.locator('.agenda-empty')).not.toContainText('this month')
  await expect(page.getByText('Date service unavailable', { exact: true })).toHaveCount(0)
  await expect(page.locator('.month-grid')).toHaveCount(0)
})

test('opens a readable Trash document without overlapping actions and restores its live controls', async ({ page }, testInfo) => {
  const now = Math.floor(Date.now() / 1000)
  const title = 'Household insurance renewal and equipment protection schedule for September 2026.pdf'
  const apiRequests = []
  const restoreRequests = []
  await mockAPI(page, {
    filingTreeChosen: true, userID: 7, userRole: 'member', capabilities: ['share_links'],
    apiRequests, restoreRequests,
    documentContent: 'Policy number 1234. Equipment and household cover renewal schedule.',
    trashDocuments: [{
      id: 31, owner_id: 7, title, mime_type: 'application/pdf', original_size: 2048,
      sensitivity: 'restricted', trashed_at: now - 86400, deletes_at: now + 29 * 86400,
    }],
  })
  await page.goto('/#/trash')
  const row = page.locator('.trash-row').filter({ hasText: title })
  const documentLink = row.locator('.trash-document')
  const actions = row.locator('.trash-actions')
  await expect(documentLink).toHaveAttribute('href', '#/doc/31')
  await expect(documentLink).toContainText(title)
  await expect(documentLink.getByRole('button')).toHaveCount(0)
  await expect(actions.getByRole('button', { name: 'Restore', exact: true })).toBeVisible()
  await expect(actions.getByRole('button', { name: 'Delete permanently', exact: true })).toBeVisible()
  const linkBox = await documentLink.boundingBox()
  const actionsBox = await actions.boundingBox()
  const rowBox = await row.boundingBox()
  const viewport = page.viewportSize()
  expect(rowBox.x + rowBox.width).toBeLessThanOrEqual(viewport.width)
  if (viewport.width <= 700) expect(actionsBox.y).toBeGreaterThanOrEqual(linkBox.y + linkBox.height)
  else expect(actionsBox.x).toBeGreaterThanOrEqual(linkBox.x + linkBox.width)
  expect(await documentLink.evaluate(element => element.scrollWidth <= element.clientWidth)).toBe(true)
  await page.screenshot({ path: testInfo.outputPath('trash-list.png'), fullPage: true })
  if (testInfo.project.use.hasTouch) await documentLink.tap()
  else { await documentLink.focus(); await page.keyboard.press('Enter') }
  await expect(page).toHaveURL(/#\/doc\/31$/)
  await expect(page.getByRole('heading', { name: title, exact: true })).toBeVisible()
  const trashNotice = page.getByRole('region', { name: 'Trashed document', exact: true })
  await expect(trashNotice.getByRole('heading', { name: 'Document in Trash', exact: true })).toBeVisible()
  await expect(trashNotice).toContainText('Read-only. Deletes permanently')
  await expect(trashNotice).toContainText('Restore to make changes.')
  await expect(page.getByRole('link', { name: 'Back to Trash', exact: true })).toHaveAttribute('href', '#/trash')
  await expect(page.getByRole('link', { name: 'Download', exact: true })).toHaveAttribute('href', '/download/31')
  await expect(page.getByTitle('Rename', { exact: true })).toHaveCount(0)
  await expect(page.locator('.detail select')).toHaveCount(0)
  for (const name of ['Edit', 'Share', 'Access', 'Trash']) {
    await expect(page.getByRole('button', { name, exact: true })).toHaveCount(0)
  }
  await expect(page.getByRole('button', { name: 'Edit tags', exact: true })).toHaveCount(0)
  const preview = page.locator('.preview')
  await expect(preview.locator('iframe')).toHaveCount(0)
  await expect(page.locator('.extracted')).toHaveAttribute('aria-hidden', 'true')
  await preview.getByRole('button', { name: 'Reveal preview', exact: true }).click()
  await expect(preview.locator('iframe')).toHaveAttribute('src', '/preview/31?reveal=1')
  await expect(page.locator('.extracted')).toHaveAttribute('aria-hidden', 'false')
  const liveOnlyPaths = ['/api/documents/31/versions/', '/api/documents/31/similar', '/api/acls/document/31']
  expect(apiRequests.filter(request => liveOnlyPaths.includes(request.path))).toEqual([])
  await page.screenshot({ path: testInfo.outputPath('trashed-document.png'), fullPage: true })
  await page.getByRole('button', { name: 'Restore', exact: true }).click()
  await expect(page.getByTitle('Rename', { exact: true })).toBeVisible()
  await expect(page.getByLabel('Sensitivity')).toHaveValue('restricted')
  await expect(page.getByRole('button', { name: 'Edit', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Share', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Restore', exact: true })).toHaveCount(0)
  await expect(trashNotice).toHaveCount(0)
  await expect(page).toHaveURL(/#\/doc\/31$/)
  expect(restoreRequests).toEqual([31])
  await expect.poll(() => apiRequests.map(request => request.path)).toEqual(expect.arrayContaining(liveOnlyPaths))
})

test('confirms permanent deletion from Trash detail and retains the document after an error', async ({ page }) => {
  const apiRequests = []
  const permanentDeleteRequests = []
  const failPaths = ['/api/trash/31']
  const now = Math.floor(Date.now() / 1000)
  await mockAPI(page, {
    filingTreeChosen: true, apiRequests, permanentDeleteRequests,
    failPaths, failureMessage: 'Storage is unavailable; try again.',
    trashDocuments: [{ id: 31, owner_id: 2, title: 'Old insurance notice', mime_type: 'application/pdf', trashed_at: now - 86400, deletes_at: now + 86400 }],
  })
  await page.goto('/#/doc/31')
  await expect(page.getByRole('button', { name: 'Restore', exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Delete permanently', exact: true }).click()
  const dialog = page.getByRole('alertdialog', { name: 'Delete permanently?', exact: true })
  await expect(dialog).toContainText('Old insurance notice')
  await expect(dialog).toContainText('This cannot be undone.')
  await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()
  await expect(dialog).toHaveCount(0)
  expect(apiRequests.filter(request => request.method === 'DELETE')).toEqual([])
  await page.getByRole('button', { name: 'Delete permanently', exact: true }).click()
  await dialog.getByRole('button', { name: 'Delete permanently', exact: true }).click()
  await expect(page.getByText('Storage is unavailable; try again.', { exact: true })).toBeVisible()
  await expect(dialog).toBeVisible()
  await expect(page).toHaveURL(/#\/doc\/31$/)
  failPaths.length = 0
  await dialog.getByRole('button', { name: 'Delete permanently', exact: true }).click()
  await expect(page).toHaveURL(/#\/trash$/)
  await expect(page.getByText('Trash is empty.', { exact: true })).toBeVisible()
  expect(permanentDeleteRequests).toEqual([31])
  expect(apiRequests.filter(request => request.path === '/api/trash/31' && request.method === 'DELETE')).toHaveLength(2)
})

for (const scenario of [
  { name: 'its retention deadline has passed', userRole: 'admin', ownerID: 7, expiresIn: -1 },
  { name: 'the reader is not its owner', userRole: 'member', ownerID: 8, expiresIn: 86400 },
]) {
  test(`does not offer Restore in Trash detail when ${scenario.name}`, async ({ page }) => {
    const now = Math.floor(Date.now() / 1000)
    await mockAPI(page, {
      filingTreeChosen: true, userID: 7, userRole: scenario.userRole,
      trashDocuments: [{ id: 31, owner_id: scenario.ownerID, title: 'Read-only trashed document', mime_type: 'application/pdf', trashed_at: now - 31 * 86400, deletes_at: now + scenario.expiresIn }],
    })
    await page.goto('/#/doc/31')
    await expect(page.getByRole('heading', { name: 'Read-only trashed document', exact: true })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Restore', exact: true })).toHaveCount(0)
    await expect(page.getByRole('link', { name: 'Back to Trash', exact: true })).toBeVisible()
  })
}

test('confirms permanent Trash deletion before removing rows', async ({ page }) => {
  const permanentDeleteRequests = []
  const emptyTrashRequests = []
  const trashedAt = Math.floor(Date.now() / 1000) - (2 * 24 * 60 * 60)
  await mockAPI(page, {

    filingTreeChosen: true,
    permanentDeleteRequests,
    emptyTrashRequests,
    trashDocuments: [{
      id: 31,
      title: 'Old electricity bill',
      mime_type: 'application/pdf',
      original_size: 2048,
      created_at: trashedAt - 100,
      trashed_at: trashedAt,
      deletes_at: trashedAt + (30 * 24 * 60 * 60),
    }, {
      id: 32,
      title: 'Old insurance notice',
      mime_type: 'application/pdf',
      original_size: 4096,
      created_at: trashedAt - 200,
      trashed_at: trashedAt,
      deletes_at: trashedAt + (30 * 24 * 60 * 60),
    }, {
      id: 33,
      title: 'Expired tax notice',
      mime_type: 'application/pdf',
      original_size: 1024,
      created_at: trashedAt - (31 * 24 * 60 * 60),
      trashed_at: trashedAt - (31 * 24 * 60 * 60),
      deletes_at: trashedAt - (24 * 60 * 60),
    }],
  })
  await page.goto('/#/trash')

  await expect(page.getByText('Documents are permanently deleted 30 days after being moved to Trash. Restore puts one back where it was filed.')).toBeVisible()
  await expect(page.getByText(/Deletes permanently/).first()).toBeVisible()
  await expect(page.getByRole('button', { name: /Empty trash/ })).toBeEnabled()
  const expiredRow = page.locator('.irow').filter({ hasText: 'Expired tax notice' })
  await expect(expiredRow.getByRole('button', { name: 'Restore' })).toHaveCount(0)

  await page.getByRole('button', { name: 'Delete permanently' }).first().click()
  let dialog = page.getByRole('alertdialog', { name: 'Delete permanently?' })
  await expect(dialog).toContainText('any share link containing it will be revoked')
  await expect(dialog.getByRole('button', { name: 'Cancel' })).toBeFocused()
  await page.locator('button').filter({ hasText: 'Empty trash' }).first().evaluate(button => button.focus())
  await expect(dialog.getByRole('button', { name: 'Cancel' })).toBeFocused()
  for (let step = 0; step < 5; step++) {
    await page.keyboard.press('Tab')
    // Native dialogs may yield to browser chrome, but never to the inert page.
    expect(await dialog.evaluate(element => document.activeElement === document.body || element.contains(document.activeElement))).toBe(true)
  }
  await dialog.getByRole('button', { name: 'Cancel' }).focus()
  await page.keyboard.press('Escape')
  await expect(dialog).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Delete permanently' }).first()).toBeFocused()
  await expect(page.getByText('Old electricity bill', { exact: true })).toBeVisible()
  expect(permanentDeleteRequests).toEqual([])

  await page.getByRole('button', { name: 'Delete permanently' }).first().click()
  dialog = page.getByRole('alertdialog', { name: 'Delete permanently?' })
  await dialog.getByRole('button', { name: 'Delete permanently' }).click()
  await expect(page.getByText('Old electricity bill', { exact: true })).toHaveCount(0)
  expect(permanentDeleteRequests).toEqual([31])

  await page.getByRole('button', { name: 'Empty trash' }).click()
  dialog = page.getByRole('alertdialog', { name: 'Empty Trash?' })
  await expect(dialog).toContainText('All 2 documents you can see in Trash')
  await dialog.getByRole('button', { name: 'Empty trash' }).click()
  await expect(page.getByText('Trash is empty.')).toBeVisible()
  await expect(page.getByRole('button', { name: /Empty trash/ })).toBeDisabled()
  expect(emptyTrashRequests).toEqual([{ count: 2 }])
})

test('navigation button toggles the sidebar at each breakpoint', async ({ page }) => {
  await mockAPI(page, {

    filingTreeChosen: true,
  })
  await page.goto('/#/dashboard')

  const sidebar = page.locator('#primary-navigation')
  if ((page.viewportSize()?.width || 0) <= 860) {
    await expect(sidebar).toBeHidden()
    const open = page.getByRole('button', { name: 'Open navigation' })
    await expect(open).toHaveAttribute('aria-expanded', 'false')
    await open.click()
    await expect(sidebar).toBeVisible()
    await expect(page.getByRole('button', { name: 'Close navigation' }).last()).toHaveAttribute('aria-expanded', 'true')
    await page.locator('.mobile-nav-veil').click({ position: { x: 350, y: 100 } })
    await expect(sidebar).toBeHidden()
  } else {
    await expect(sidebar).toBeVisible()
    const collapse = page.getByRole('button', { name: 'Collapse navigation' })
    await expect(collapse).toHaveAttribute('aria-expanded', 'true')
    await collapse.click()
    await expect(sidebar).toBeHidden()
    const expand = page.getByRole('button', { name: 'Expand navigation' })
    await expect(expand).toHaveAttribute('aria-expanded', 'false')
    await expand.click()
    await expect(sidebar).toBeVisible()
  }
})

// These cases exercise compiled browser lifetimes. Mocked API responses do not
// establish server-side membership, ACL or credential isolation.
const filingCabinets = [
  { code: 'S01', name: 'First cabinet', is_default: true },
  { code: 'S02', name: 'Second cabinet', is_default: false },
]

async function mockFilingSystems(page, overrides = {}) {
  const options = {
    filingTreeChosen: true,
    systems: { introduced: true, default_system_code: 'S01', results: filingCabinets },
    documentDetails: {
      147: { document: { system_code: 'S01', jd_address: 'S01.13.147', title: 'First private record' } },
      148: { document: { system_code: 'S02', jd_address: 'S02.13.148', title: 'Second private record' } },
    },
    ...overrides,
  }
  await mockAPI(page, options)
  const requests = []
  await page.route('**/api/**', async route => {
    const request = route.request()
    const url = new URL(request.url())
    const code = url.searchParams.get('system')
    requests.push({ path: url.pathname, system: code, method: request.method() })
    if (request.method() !== 'GET') return route.fallback()
    if (url.pathname === '/api/documents/') return route.fulfill({ json: {
      count: 1, results: [code === 'S02'
        ? { id: 148, title: 'Second private record', system_code: 'S02', jd_address: 'S02.13.148' }
        : { id: 147, title: 'First private record', system_code: 'S01', jd_address: 'S01.13.147' }],
    } })
    if (url.pathname === '/api/jd/categories/') return route.fulfill({ json: {
      results: [{ id: code === 'S02' ? 213 : 113, code: 13, name: `${code || 'Original'} filing`,
        area_code: 10, area_name: `${code || 'Original'} records`, system: false }],
    } })
    if (url.pathname === '/api/stats/') return route.fulfill({ json: {
      documents_total: code === 'S02' ? 19 : 731, inbox_count: 0, pending_approvals: 0, dead_jobs: 0,
    } })
    return route.fallback()
  })
  return { options, requests }
}

async function chooseFilingSystem(page, code) {
  const select = page.getByRole('combobox', { name: 'Current filing system', exact: true })
  if (!await select.isVisible()) await page.getByRole('button', { name: 'Open navigation', exact: true }).click()
  await select.selectOption(code)
  await expect(page).toHaveURL(new RegExp(`system=${code}`))
}

async function paintSettled(page) {
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))
}

test('tag management preserves the selected filing system on entry and reload', async ({ page }) => {
  await mockFilingSystems(page)
  const tagSystems = []
  await page.route('**/api/tags/**', route => {
    tagSystems.push(new URL(route.request().url()).searchParams.get('system'))
    return route.fulfill({ json: {
      results: [{ id: 7, name: 'Scoped tag', slug: 'scoped' }], next: null,
    } })
  })
  await page.goto('/#/doc/148?system=S02')
  const documentTags = page.locator('.document-tags')
  await expect(documentTags.getByRole('link', { name: 'Manage tags' })).toHaveCount(0)
  await documentTags.getByRole('button', { name: 'Edit tags' }).click()
  await documentTags.getByRole('link', { name: 'Manage tags' }).click()
  await expect(page).toHaveURL(/#\/settings\?tab=archive&section=metadata&metadata=tags&system=S02$/)
  await expect(page.getByRole('combobox', { name: 'Metadata type' })).toHaveValue('tags')
  await page.reload()
  await expect(page.getByRole('textbox', { name: 'Rename Scoped tag' })).toBeVisible()
  expect(tagSystems).toEqual(['S02', 'S02', 'S02'])
})

test('filing systems selects an S02-only member before any collection reads and retains it on reload', async ({ page }) => {
  const { requests } = await mockFilingSystems(page, {
    userRole: 'member',
    systems: { introduced: true, default_system_code: '', results: [filingCabinets[1]] },
  })
  await page.goto('/#/documents')
  await expect(page).toHaveURL(/#\/documents\?system=S02$/)
  await expect(page.getByText('Second private record', { exact: true })).toBeVisible()
  await expect(page.getByRole('combobox', { name: 'Current filing system' })).toHaveCount(0)
  await expect(page.getByRole('link').filter({ hasText: 'Second private record' })).toHaveAttribute('href', '#/doc/148?system=S02')
  await page.reload()
  await expect(page.getByText('Second private record', { exact: true })).toBeVisible()
  const scoped = requests.filter(item => ['/api/documents/', '/api/jd/categories/', '/api/stats/'].includes(item.path))
  expect(scoped.length).toBeGreaterThan(0)
  expect(scoped.every(item => item.system === 'S02')).toBe(true)
  await expect(page.getByText('First private record', { exact: true })).toHaveCount(0)
})

test('filing systems never substitutes the default for an explicit unavailable route', async ({ page }) => {
  const { requests } = await mockFilingSystems(page, {
    userRole: 'member',
    systems: { introduced: true, default_system_code: '', results: [filingCabinets[1]] },
  })
  await page.goto('/#/documents?system=S01')
  await expect(page.getByText('Filing system unavailable', { exact: true })).toBeVisible()
  await expect(page).toHaveURL(/system=S01$/)
  expect(requests.filter(item => ['/api/documents/', '/api/jd/categories/', '/api/stats/'].includes(item.path))).toEqual([])
  await expect(page.getByText('Second private record', { exact: true })).toHaveCount(0)
  await page.reload()
  await expect(page.getByText('Filing system unavailable', { exact: true })).toBeVisible()
  await expect(page).toHaveURL(/system=S01$/)
})

test('filing systems refuses dropped uploads until the selected system is available', async ({ page }) => {
  const { requests } = await mockFilingSystems(page)
  await page.goto('/#/documents?system=S99')
  await expect(page.getByText('Filing system unavailable', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Upload documents', exact: true })).toBeDisabled()
  await page.evaluate(() => {
    const dataTransfer = new DataTransfer()
    dataTransfer.items.add(new File(['private content'], 'private.txt', { type: 'text/plain' }))
    window.dispatchEvent(new DragEvent('dragenter', { dataTransfer, bubbles: true, cancelable: true }))
    window.dispatchEvent(new DragEvent('drop', { dataTransfer, bubbles: true, cancelable: true }))
  })
  await paintSettled(page)
  await expect(page.getByRole('dialog', { name: 'Upload documents', exact: true })).toHaveCount(0)
  expect(requests.filter(item => item.path === '/api/documents/' && item.method === 'POST')).toEqual([])
})

test('filing systems derives a legacy numeric deep link intrinsically, then scopes document and blob links', async ({ page }) => {
  const { requests } = await mockFilingSystems(page)
  await page.goto('/#/doc/148')
  await expect(page).toHaveURL(/#\/doc\/148\?system=S02$/)
  await expect(page.getByLabel('Filing address', { exact: true })).toHaveValue('S02.13.148')
  expect(requests.filter(item => item.path === '/api/documents/148').map(item => item.system)).toEqual([null, 'S02'])
  expect(requests.filter(item => item.path === '/api/jd/categories/').every(item => item.system === 'S02')).toBe(true)
  await expect(page.getByRole('link', { name: 'Download', exact: true })).toHaveAttribute('href', '/download/148?system=S02')
  await page.getByRole('button', { name: 'Open on my phone', exact: true }).click()
  await expect(page.getByLabel('Document link', { exact: true })).toHaveValue(/#\/doc\/148\?system=S02$/)
})

test('filing systems does not reload document detail when the sidebar tree arrives', async ({ page }) => {
  const { requests } = await mockFilingSystems(page)
  let tree
  await page.route('**/api/jd/categories/?*', route => { tree = route })
  await page.goto('/#/doc/148?system=S02')
  await expect(page.getByLabel('Filing address', { exact: true })).toHaveValue('S02.13.148')
  await page.getByRole('button', { name: 'Edit tags', exact: true }).click()
  await expect(page.getByRole('combobox', { name: 'Tag to add', exact: true })).toBeVisible()
  await expect.poll(() => !!tree).toBe(true)
  const finished = page.waitForEvent('requestfinished', request => request === tree.request())
  await tree.fulfill({ json: { results: [{ id: 213, code: 13, name: 'Second filing', area_code: 10, area_name: 'Records' }] } })
  await finished
  await paintSettled(page)
  expect(requests.filter(item => item.path === '/api/documents/148')).toHaveLength(1)
  await expect(page.getByRole('combobox', { name: 'Tag to add', exact: true })).toBeVisible()
})

test('filing systems preserves cabinet history and resets selected documents', async ({ page }) => {
  await mockFilingSystems(page)
  await page.goto('/#/documents?system=S01')
  await page.getByRole('checkbox', { name: 'Select First private record', exact: true }).check()
  await chooseFilingSystem(page, 'S02')
  await expect(page.getByText('Second private record', { exact: true })).toBeVisible()
  await page.goBack()
  await expect(page).toHaveURL(/#\/documents\?system=S01$/)
  await expect(page.getByRole('checkbox', { name: 'Select First private record', exact: true })).not.toBeChecked()
  await page.goForward()
  await expect(page).toHaveURL(/system=S02$/)
  await page.reload()
  await expect(page.getByText('Second private record', { exact: true })).toBeVisible()
  await expect(page.getByText('First private record', { exact: true })).toHaveCount(0)
})

test('filing systems rejects delayed old tree, counters and recent documents after switching', async ({ page }) => {
  await mockFilingSystems(page)
  const pending = new Map()
  await page.route('**/api/**', route => {
    const url = new URL(route.request().url())
    if (url.searchParams.get('system') === 'S01' && ['/api/documents/', '/api/jd/categories/', '/api/stats/'].includes(url.pathname)) {
      pending.set(url.pathname, [...(pending.get(url.pathname) || []), route])
      return
    }
    return route.fallback()
  })
  await page.goto('/#/dashboard?system=S01')
  await expect.poll(() => pending.size).toBe(3)
  await chooseFilingSystem(page, 'S02')
  await expect(page.getByText('Second private record', { exact: true })).toBeVisible()
  for (const [path, routes] of pending) {
    const json = path === '/api/stats/' ? { documents_total: 731 } : path === '/api/jd/categories/'
      ? { results: [{ id: 113, code: 13, name: 'Foreign secret category', area_code: 10, area_name: 'Foreign secret tree' }] }
      : { count: 1, results: [{ id: 147, title: 'Foreign delayed receipt' }] }
    for (const route of routes) await route.fulfill({ json })
  }
  await paintSettled(page)
  await expect(page.getByText('19', { exact: true })).toBeVisible()
  await expect(page.getByText('Second private record', { exact: true })).toBeVisible()
  await expect(page.getByText(/Foreign delayed receipt|Foreign secret tree|Foreign secret category/)).toHaveCount(0)
  await expect(page.getByText('731', { exact: true })).toHaveCount(0)
})

test('filing systems keeps a continuing upload batch bound to its starting system without new-scope receipts', async ({ page }) => {
  await mockFilingSystems(page)
  const destinations = []
  let first
  await page.route('**/api/documents/?*', route => {
    if (route.request().method() !== 'POST') return route.fallback()
    destinations.push(new URL(route.request().url()).searchParams.get('system'))
    if (destinations.length === 1) { first = route; return }
    return route.fulfill({ json: { id: 902, system_code: 'S01', jd_address: 'S01.49.902' } })
  })
  await page.goto('/#/upload?system=S01')
  await page.locator('input[type=file]').setInputFiles([
    { name: 'old-first.txt', mimeType: 'text/plain', buffer: Buffer.from('one') },
    { name: 'old-second.txt', mimeType: 'text/plain', buffer: Buffer.from('two') },
  ])
  await expect.poll(() => !!first).toBe(true)
  await chooseFilingSystem(page, 'S02')
  await expect(page.getByText('Second private record', { exact: true })).toBeVisible()
  await first.fulfill({ json: { id: 901, system_code: 'S01', jd_address: 'S01.49.901' } })
  await expect.poll(() => destinations).toEqual(['S01', 'S01'])
  await paintSettled(page)
  await expect(page.getByText(/old-first|old-second|S01\.49\.90/)).toHaveCount(0)
  await expect(page.getByText('Second private record', { exact: true })).toBeVisible()
})

test('filing systems resolves exact addresses but never treats a stale filing address as a search query', async ({ page }) => {
  const { requests } = await mockFilingSystems(page)
  await page.route('**/api/jd/resolve?*', route => {
    const address = new URL(route.request().url()).searchParams.get('address')
    return address === 'S02.13.148'
      ? route.fulfill({ json: { id: 148, system_code: 'S02', jd_address: address } })
      : route.fulfill({ status: 404, json: { code: 'not_found', error: 'Address unavailable' } })
  })
  await page.goto('/#/documents?system=S01')
  const omni = page.getByRole('searchbox', { name: 'Search or run a command' })
  await omni.fill('S02.13.148')
  await omni.press('Enter')
  await expect(page).toHaveURL(/#\/doc\/148\?system=S02$/)
  await expect(page.getByLabel('Filing address', { exact: true })).toHaveValue('S02.13.148')
  await page.evaluate(() => { location.hash = '#/search?system=S02&q=S02.12.148' })
  await expect(page.getByText('Address unavailable', { exact: true })).toBeVisible()
  expect(requests.filter(item => item.path === '/api/search/')).toEqual([])
})

test('filing systems first named Apply invalidates unnamed reads and selects the imported system', async ({ page }) => {
  const options = {
    systems: { introduced: false, default_system_code: '', results: [] },
    taxonomyImport: async payload => {
      if (payload.apply) options.systems = { introduced: true, default_system_code: 'S01', results: filingCabinets }
      return taxonomyPreview({ applied: payload.apply, system_code: 'S02', system_name: 'Second cabinet',
        system_created: true, systems_introduced: true, existing_system_code: payload.existing_system_code || '' })
    },
  }
  await mockAPI(page, options)
  let oldRecent
  await page.route('**/api/documents/**', route => {
    const url = new URL(route.request().url())
    if (!url.searchParams.has('system') && url.pathname === '/api/documents/') { oldRecent = route; return }
    return route.fulfill({ json: { count: 1, results: [{ id: 148, title: 'New cabinet record', jd_address: 'S02.13.148' }] } })
  })
  await page.goto('/#/dashboard')
  await expect.poll(() => !!oldRecent).toBe(true)
  await expect(page.getByRole('combobox', { name: 'Current filing system' })).toHaveCount(0)
  await page.evaluate(() => { location.hash = '#/settings?tab=archive&section=filing-tree' })
  await page.getByRole('button', { name: 'Import or export' }).click()
  await page.getByRole('button', { name: 'Import file' }).click()
  const importer = page.getByRole('region', { name: 'Import taxonomy', exact: true })
  await importer.getByLabel('Paste content', { exact: true }).fill(`system = \"S02\"\n${taxonomyContent}`)
  await importer.getByRole('combobox', { name: 'Serialization', exact: true }).selectOption('toml')
  await importer.getByRole('button', { name: 'Preview', exact: true }).click()
  await expect(importer.getByRole('group', { name: 'First system import destination' })).toBeVisible()
  await importer.getByRole('radio', { name: /Create separately/ }).check()
  await importer.getByLabel('Existing archive code', { exact: true }).fill('S01')
  await expect(importer.getByRole('button', { name: 'Apply import', exact: true })).toBeDisabled()
  await importer.getByRole('button', { name: 'Preview', exact: true }).click()
  await importer.getByRole('button', { name: 'Apply import', exact: true }).click()
  await expect(page).toHaveURL(/#\/dashboard\?system=S02$/)
  await oldRecent.fulfill({ json: { count: 1, results: [{ id: 147, title: 'Obsolete unnamed receipt' }] } })
  await paintSettled(page)
  await expect(page.getByText('New cabinet record', { exact: true })).toBeVisible()
  await expect(page.getByText('Obsolete unnamed receipt', { exact: true })).toHaveCount(0)
  await expect(importer).toHaveCount(0)
})

test('filing systems membership editing preserves complete and inactive grants across directory pages', async ({ page }) => {
  await mockFilingSystems(page)
  const directory = Array.from({ length: 42 }, (_, i) => ({
    id: i + 1, email: `user${i + 1}@example.test`, role: i === 0 ? 'admin' : 'member', disabled: i === 40,
  }))
  let saved
  await page.route('**/api/admin/users', route => route.fulfill({ json: { results: directory } }))
  await page.route('**/api/admin/jd/systems/S01/members', route => {
    if (route.request().method() === 'PUT') saved = route.request().postDataJSON()
    return route.fulfill({ json: saved || { user_ids: [2, 22, 41] } })
  })
  await page.goto('/#/settings?tab=archive&system=S01')
  const panel = page.getByRole('region', { name: 'Filing system access', exact: true })
  await expect(panel.getByRole('checkbox', { name: 'System access for user1@example.test', exact: true })).toBeDisabled()
  await panel.getByRole('checkbox', { name: 'System access for user3@example.test', exact: true }).check()
  await panel.getByRole('button', { name: 'Next members' }).click()
  await expect(panel.getByRole('checkbox', { name: 'System access for user22@example.test', exact: true })).toBeChecked()
  await panel.getByRole('button', { name: 'Next members' }).click()
  await expect(panel.getByRole('checkbox', { name: 'System access for user41@example.test', exact: true })).toBeChecked()
  await panel.getByRole('button', { name: 'Save memberships' }).click()
  await expect.poll(() => saved).toEqual({ user_ids: [2, 3, 22, 41] })
  await expect(panel.getByRole('checkbox', { name: 'System access for user41@example.test', exact: true })).toBeChecked()
})

test('filing systems with no accessible cabinet offers only account controls and signout', async ({ page }) => {
  const { requests } = await mockFilingSystems(page, {
    userRole: 'member', systems: { introduced: true, default_system_code: '', results: [] },
  })
  await page.goto('/#/documents')
  await expect(page.getByText(/Ask an administrator to grant access/)).toBeVisible()
  await page.getByRole('link', { name: 'My account', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Save profile', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Pair mobile app', exact: true })).toHaveCount(0)
  expect(requests.filter(item => ['/api/documents/', '/api/tokens/', '/api/decryption-passwords/', '/api/jd/categories/'].includes(item.path))).toEqual([])
  await page.getByRole('button', { name: 'Sign out', exact: true }).last().click()
  await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeVisible()
})

test('filing systems cancels a late pairing in its captured cabinet and never restores its secret after a switch', async ({ page }) => {
  await mockFilingSystems(page)
  let pending
  const cancellations = []
  await page.route('**/api/mobile/pairing?*', route => {
    if (route.request().method() === 'POST') { pending = route; return }
    cancellations.push({ code: route.request().postDataJSON().code, system: new URL(route.request().url()).searchParams.get('system') })
    return route.fulfill({ status: 204 })
  })
  await page.goto('/#/settings?system=S01')
  await page.getByRole('button', { name: 'Pair mobile app', exact: true }).click()
  await page.getByRole('button', { name: 'Generate QR code', exact: true }).click()
  await expect.poll(() => !!pending).toBe(true)
  // A native modal makes the switcher inert; history/navigation still changes scope.
  await page.evaluate(() => { location.hash = '#/settings?system=S02' })
  await expect(page.getByRole('dialog', { name: 'Pair mobile app', exact: true })).toHaveCount(0)
  await pending.fulfill({ json: { code: 'old-pairing-code', pairing_url: 'suchi://pair?old-secret', expires_at: Math.floor(Date.now() / 1000) + 300 } })
  await expect.poll(() => cancellations).toEqual([{ code: 'old-pairing-code', system: 'S01' }])
  await page.getByRole('button', { name: 'Pair mobile app', exact: true }).click()
  await expect(page.locator('#pairing-link')).toHaveCount(0)
  await expect(page.getByText('suchi://pair?old-secret')).toHaveCount(0)
})

test('filing systems ignores a late share response and its clipboard effects after switching', async ({ page }) => {
  await mockFilingSystems(page)
  await page.addInitScript(() => {
    window.shareCopies = []
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: {
      writeText: async text => { window.shareCopies.push(text) },
    } })
  })
  let pending
  await page.route('**/api/share_links/?*', route => {
    if (route.request().method() === 'POST') { pending = route; return }
    return route.fulfill({ json: { results: [] } })
  })
  await page.goto('/#/doc/147?system=S01')
  await page.getByRole('button', { name: 'Share', exact: true }).click()
  await page.getByRole('button', { name: 'Create & copy', exact: true }).click()
  await expect.poll(() => !!pending).toBe(true)
  expect(new URL(pending.request().url()).searchParams.get('system')).toBe('S01')
  await page.evaluate(() => { location.hash = '#/doc/148?system=S02' })
  await expect(page.getByLabel('Filing address', { exact: true })).toHaveValue('S02.13.148')
  await pending.fulfill({ json: { id: 9, public_url: 'https://archive.example.test/s/foreign-secret' } })
  await paintSettled(page)
  await expect(page.getByRole('dialog', { name: 'Share document' })).toHaveCount(0)
  await expect(page.getByText(/foreign-secret/)).toHaveCount(0)
  expect(await page.evaluate(() => window.shareCopies)).toEqual([])
})

for (const completed of [false, true]) {
  test(`filing systems discards ${completed ? 'the completed OAuth handoff' : 'a pending OAuth completion'} on switch`, async ({ page }) => {
    await mockFilingSystems(page)
    const submitted = []
    let completion
    await page.route('**/api/email-accounts/**', route => {
      const url = new URL(route.request().url())
      if (url.pathname.endsWith('/oauth/start')) return route.fulfill({ json: {
        flow_handle: 'first-flow', user_code: 'FIRST-CODE', verification_url: 'https://microsoft.com/devicelogin',
        expires_at: Math.floor(Date.now() / 1000) + 300,
      } })
      if (url.pathname.endsWith('/oauth/complete')) {
        completion = route
        if (!completed) return
        return route.fulfill({ json: { ok: true, username: 'first-flow@example.test', oauth_account_id: 'first-account', sealed_secret_b64: 'first-handoff' } })
      }
      return route.fallback()
    })
    await page.route('**/api/email-accounts?*', route => {
      if (route.request().method() !== 'POST') return route.fallback()
      submitted.push({ body: route.request().postDataJSON(), system: new URL(route.request().url()).searchParams.get('system') })
      return route.fulfill({ json: { id: 12 } })
    })
    await page.goto('/#/settings?tab=archive&section=mail&system=S01')
    await page.getByRole('button', { name: 'Add mailbox', exact: true }).click()
    await page.locator('#ma-provider').selectOption('microsoft')
    await page.locator('#ma-oauth-btn').click()
    await expect.poll(() => !!completion).toBe(true)
    expect(new URL(completion.request().url()).searchParams.get('system')).toBe('S01')
    if (completed) await expect(page.getByText('Signed in as first-flow@example.test', { exact: true })).toBeVisible()
    await page.evaluate(() => { location.hash = '#/settings?tab=archive&section=mail&system=S02' })
    await expect(page.getByRole('dialog', { name: 'Sign in with Microsoft', exact: true })).toHaveCount(0)
    if (!completed) await completion.fulfill({ json: { ok: true, username: 'late@example.test', sealed_secret_b64: 'late-handoff' } }).catch(() => {})
    await page.getByRole('button', { name: 'Add mailbox', exact: true }).click()
    await expect(page.locator('#ma-user')).toHaveValue('')
    await expect(page.getByText(/Signed in as first-flow|late@example/)).toHaveCount(0)
    await page.locator('#ma-name').fill('Second cabinet mailbox')
    await page.locator('#ma-owner').selectOption('1')
    await page.locator('#ma-provider').selectOption('gmail')
    await page.locator('#ma-user').fill('second@example.test')
    await page.locator('#ma-pw').fill('new-app-password')
    await page.getByRole('button', { name: 'Create mailbox', exact: true }).click()
    await expect.poll(() => submitted.length).toBe(1)
    expect(submitted[0].system).toBe('S02')
    expect(submitted[0].body.username).toBe('second@example.test')
    expect(submitted[0].body).not.toHaveProperty('sealed_secret_b64')
    expect(submitted[0].body).not.toHaveProperty('oauth_account_id')
  })
}

test('filing systems destroys parked research and excludes old source IDs and history from the next question', async ({ page }) => {
  const chatRequests = []
  await mockFilingSystems(page, {
    chatEnabled: true, chatRequests,
    chatResponse: (_, number) => ({
      answer: number === 1 ? 'First cabinet confidential answer [1].' : 'Second cabinet answer [1].',
      sources: [{ id: number === 1 ? 147 : 148, title: number === 1 ? 'First evidence' : 'Second evidence', snippet: 'Evidence', sensitivity: 'internal' }],
      citations: [1], grounded: true, intelligence: { accepted: {}, pending: {} },
    }),
  })
  await page.goto('/#/documents?system=S01')
  await page.getByRole('searchbox', { name: 'Search or run a command' }).fill('First private question')
  await page.getByRole('button', { name: 'Ask the archive', exact: true }).click()
  await expect(page.getByText('First cabinet confidential answer', { exact: false })).toBeVisible()
  await page.getByRole('link', { name: 'Open source 1: First evidence', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Return to archive research', exact: true })).toBeVisible()
  await chooseFilingSystem(page, 'S02')
  await expect(page.getByRole('button', { name: 'Return to archive research', exact: true })).toHaveCount(0)
  await page.getByRole('searchbox', { name: 'Search or run a command' }).fill('Second question')
  await page.getByRole('button', { name: 'Ask the archive', exact: true }).click()
  await expect(page.getByText('Second cabinet answer', { exact: false })).toBeVisible()
  expect(chatRequests).toHaveLength(2)
  expect(chatRequests[1].history).toEqual([])
  expect(chatRequests[1].context_source_ids).toEqual([])
  await expect(page.getByText(/First private question|First cabinet confidential answer|First evidence/)).toHaveCount(0)
})

test('filing systems does not copy the selected system into a prefixed import body and drops its preview on switch', async ({ page }) => {
  const calls = []
  await mockFilingSystems(page, { taxonomyImport: async payload => {
    calls.push(payload)
    return taxonomyPreview({ system_code: 'S02', system_name: 'Second cabinet', system_created: false, systems_introduced: false })
  } })
  await page.goto('/#/settings?tab=archive&section=filing-tree&system=S01')
  await page.getByRole('button', { name: 'Import or export' }).click()
  await page.getByRole('button', { name: 'Import file', exact: true }).click()
  const importer = page.getByRole('region', { name: 'Import taxonomy', exact: true })
  await importer.getByLabel('Paste content', { exact: true }).fill(`system = \"S02\"\n${taxonomyContent}`)
  await importer.getByRole('button', { name: 'Preview', exact: true }).click()
  await expect(importer.getByText('S02 · Second cabinet', { exact: true })).toBeVisible()
  expect(calls[0]).not.toHaveProperty('target_system')
  await expect(importer.getByRole('group', { name: 'First system import destination' })).toHaveCount(0)
  await chooseFilingSystem(page, 'S02')
  await expect(importer).toHaveCount(0)
  await page.evaluate(() => { location.hash = '#/settings?tab=archive&section=filing-tree&system=S01' })
  await page.getByRole('button', { name: 'Import or export' }).click()
  await page.getByRole('button', { name: 'Import file', exact: true }).click()
  await expect(importer.getByLabel('Paste content', { exact: true })).toHaveValue('')
  await expect(importer.getByRole('button', { name: 'Apply import', exact: true })).toBeDisabled()
})

test('filing systems never shares an in-flight taxonomy GET between accounts with the same system URL', async ({ page }) => {
  await mockFilingSystems(page)
  let actor = 1
  let oldTree
  await page.route('**/api/**', route => {
    const url = new URL(route.request().url())
    if (url.pathname === '/api/logout') { actor = 0; return route.fulfill({ status: 204 }) }
    if (url.pathname === '/api/login') { actor = 2; return route.fulfill({ json: { ok: true } }) }
    if (url.pathname === '/api/whoami') return route.fulfill({ json: {
      user_id: actor, email: `actor${actor}@example.test`, role: 'member', capabilities: [],
    } })
    if (url.pathname === '/api/jd/categories/') {
      if (actor === 1) { oldTree = route; return }
      return route.fulfill({ json: { results: [{
        id: 213, code: 13, name: 'New account category', area_code: 10, area_name: 'New account tree',
      }] } })
    }
    return route.fallback()
  })
  await page.goto('/#/documents?system=S01')
  await expect.poll(() => !!oldTree).toBe(true)
  const openNav = page.getByRole('button', { name: 'Open navigation', exact: true })
  if (await openNav.isVisible()) await openNav.click()
  await page.getByRole('button', { name: 'Sign out', exact: true }).click()
  await page.getByLabel('Email', { exact: true }).fill('actor2@example.test')
  await page.getByLabel('Password', { exact: true }).fill('password')
  await page.getByRole('button', { name: 'Sign in', exact: true }).click()
  await expect(page.locator('.area-toggle').filter({ hasText: 'New account tree' })).toHaveCount(1)
  await oldTree.fulfill({ json: { results: [{
    id: 113, code: 13, name: 'Old account category', area_code: 10, area_name: 'Old account tree',
  }] } })
  await paintSettled(page)
  await expect(page.locator('.area-toggle').filter({ hasText: 'New account tree' })).toHaveCount(1)
  await expect(page.getByText(/Old account tree|Old account category/)).toHaveCount(0)
})

test('filing systems clears an active surface on system_unavailable instead of falling back', async ({ page }) => {
  const { requests } = await mockFilingSystems(page)
  let denied = false
  await page.route('**/api/documents/?*', route => denied
    ? route.fulfill({ status: 404, json: { code: 'system_unavailable', error: 'System unavailable' } })
    : route.fallback())
  await page.goto('/#/documents?system=S02')
  await expect(page.getByText('Second private record', { exact: true })).toBeVisible()
  denied = true
  await page.getByRole('button', { name: 'Refresh documents', exact: true }).click()
  await expect(page.getByText('Filing system unavailable', { exact: true })).toBeVisible()
  await expect(page.getByText('Second private record', { exact: true })).toHaveCount(0)
  await expect(page.getByText('S02 records', { exact: true })).toHaveCount(0)
  await expect(page).toHaveURL(/system=S02$/)
  expect(requests.filter(item => item.path === '/api/documents/').every(item => item.system === 'S02')).toBe(true)
})

test('filing systems renames a display name without changing its code or document address', async ({ page }) => {
  const { options } = await mockFilingSystems(page)
  const changes = []
  await page.route('**/api/admin/jd/systems/S01/members', route => route.fulfill({ json: { user_ids: [2] } }))
  await page.route('**/api/admin/jd/systems/S01', route => {
    changes.push(route.request().postDataJSON())
    options.systems = { ...options.systems, results: options.systems.results.map(system =>
      system.code === 'S01' ? { ...system, name: changes.at(-1).name } : system) }
    return route.fulfill({ json: options.systems.results[0] })
  })
  await page.goto('/#/settings?tab=archive&system=S01')
  const panel = page.getByRole('region', { name: 'Filing system access', exact: true })
  await panel.getByRole('textbox', { name: 'System name', exact: true }).fill('')
  await expect(panel.getByRole('button', { name: 'Save system name', exact: true })).toBeDisabled()
  await panel.getByRole('textbox', { name: 'System name', exact: true }).fill('Renamed firm')
  await panel.getByRole('button', { name: 'Save system name', exact: true }).click()
  await expect(page.locator('.topbar h1')).toContainText('S01 · Renamed firm')
  expect(changes).toEqual([{ name: 'Renamed firm' }])
  await expect(page).toHaveURL(/system=S01$/)
  await page.evaluate(() => { location.hash = '#/doc/147?system=S01' })
  await expect(page.getByLabel('Filing address', { exact: true })).toHaveValue('S01.13.147')
})

test('Account subscription connects with device code and requires egress and model test', async ({ page }) => {
  const llmTestRequests = []
  await mockAPI(page, { filingTreeChosen: true, llmHasAPIKey: true, llmTestRequests })
  const actions = []
  await page.route('**/api/admin/settings/llm/subscriptions/openai_chatgpt/*', route => {
    const action = new URL(route.request().url()).pathname.split('/').at(-1)
    if (action === 'models') return route.fulfill({ json: { models: [{ id: 'subscription-model', name: 'Subscription model' }, { id: 'other-model', name: 'Other model' }] } })
    actions.push(action)
    return route.fulfill({ json: action === 'start'
      ? { user_code: 'ABCD-1234', verification_url: 'https://auth.openai.com/codex/device', interval: 1 }
      : { connected: action !== 'disconnect' } })
  })
  await page.goto('/#/settings?tab=archive&section=llm')
  await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await page.getByLabel('Clear the saved API key when saving. Config-file and environment keys are unchanged.').check()
  await page.getByRole('button', { name: 'Account subscription', exact: true }).click()
  await expect(page.getByRole('combobox', { name: 'Provider', exact: true })).toHaveValue('openai_chatgpt')
  await expect(page.getByLabel('Endpoint URL')).toHaveCount(0)
  await expect(page.getByLabel('API key (blank for local)')).toHaveCount(0)
  await expect(page.getByRole('combobox', { name: 'Model', exact: true })).toBeDisabled()
  const testConnection = page.getByRole('button', { name: 'Test connection', exact: true })
  const saveModel = page.getByRole('button', { name: 'Enable model', exact: true })
  await expect(testConnection).toBeDisabled()
  await page.getByRole('button', { name: 'Connect ChatGPT', exact: true }).click()
  await expect(page.getByText('ABCD-1234', { exact: true })).toBeVisible()
  await expect(page.getByRole('link', { name: 'ChatGPT device login' })).toHaveAttribute('href', 'https://auth.openai.com/codex/device')
  await expect(page.getByText('ChatGPT connected', { exact: true })).toBeVisible()
  await expect(page.getByRole('combobox', { name: 'Model', exact: true })).toHaveValue('subscription-model')
  await expect(page.getByRole('combobox', { name: 'Model', exact: true }).locator('option')).toHaveCount(2)
  await expect(testConnection).toBeDisabled()
  await page.getByLabel('This endpoint is not local. I acknowledge document text will leave this machine.').check()
  await expect(testConnection).toBeEnabled()
  await expect(saveModel).toBeDisabled()
  await testConnection.click()
  await expect(saveModel).toBeEnabled()
  expect(llmTestRequests[0]).toMatchObject({ subscription_provider: 'openai_chatgpt', endpoint_url: '', model: 'subscription-model', api_key: '', clear_api_key: false, egress_ack: true })
  await page.getByRole('combobox', { name: 'Model', exact: true }).selectOption('other-model')
  await expect(saveModel).toBeDisabled()
  await page.getByRole('button', { name: 'Disconnect ChatGPT', exact: true }).click()
  await expect(page.getByText('ChatGPT not connected', { exact: true })).toBeVisible()
  await expect(testConnection).toBeDisabled()
  await expect(saveModel).toBeDisabled()
  expect(actions).toEqual(['start', 'poll', 'disconnect'])
})

test('Account subscription reloads saved models and retries catalog failures', async ({ page }) => {
  await mockAPI(page, { filingTreeChosen: true })
  await page.route('**/api/admin/settings/llm', route => route.fulfill({ json: {
    enabled: true, active: true, mode: 'subscription', subscription_provider: 'openai_chatgpt', endpoint_url: '',
    model: 'saved-model', egress_ack: true, subscription_connected: true,
  } }))
  let reads = 0
  await page.route('**/api/admin/settings/llm/subscriptions/openai_chatgpt/models', route => {
    if (++reads === 1) return route.fulfill({ status: 503, json: { error: 'Catalog temporarily unavailable' } })
    return route.fulfill({ json: { models: [{ id: 'first-model', name: 'First model' }, { id: 'saved-model', name: 'Saved model' }] } })
  })
  await page.goto('/#/settings?tab=archive&section=llm')
  await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  const model = page.getByRole('combobox', { name: 'Model', exact: true })
  await expect(page.getByRole('alert')).toContainText('Catalog temporarily unavailable')
  await expect(model).toBeDisabled()
  await expect(page.getByRole('button', { name: 'Test connection', exact: true })).toBeDisabled()
  await page.getByRole('button', { name: 'Refresh models', exact: true }).click()
  await expect(model).toHaveValue('saved-model')
  await model.selectOption('first-model')
  await page.getByRole('button', { name: 'Refresh models', exact: true }).click()
  await expect(model).toHaveValue('first-model')
  await expect(page.getByRole('button', { name: 'Test connection', exact: true })).toBeEnabled()
})

test('Account subscription discards a catalog that arrives after switching providers', async ({ page }) => {
  await mockAPI(page, { filingTreeChosen: true })
  await page.route('**/api/admin/settings/llm', route => route.fulfill({ json: {
    mode: 'subscription', subscription_provider: 'openai_chatgpt', endpoint_url: '', model: 'saved-model', subscription_connected: true,
  } }))
  let held
  await page.route('**/api/admin/settings/llm/subscriptions/openai_chatgpt/models', route => { held = route })
  await page.goto('/#/settings?tab=archive&section=llm')
  await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await expect.poll(() => !!held).toBe(true)
  await page.getByRole('button', { name: 'Local model', exact: true }).click()
  await page.getByRole('textbox', { name: 'Model', exact: true }).fill('local-model')
  await held.fulfill({ json: { models: [{ id: 'late-model', name: 'Late model' }] } })
  await expect(page.getByRole('textbox', { name: 'Model', exact: true })).toHaveValue('local-model')
  await expect(page.getByRole('combobox', { name: 'Model', exact: true })).toHaveCount(0)
})


test('Account subscription saves model selection across page reload without activating it', async ({ page }) => {
  const modelRequests = []
  const settingsRequests = []
  const testRequests = []
  await mockAPI(page, {
    filingTreeChosen: true, subscriptionConnected: true,
    llmEndpoint: '', llmModel: 'first-model',
    subscriptionModelRequests: modelRequests, llmSettingsRequests: settingsRequests, llmTestRequests: testRequests,
  })
  await page.route('**/api/admin/settings/llm/subscriptions/openai_chatgpt/models', route => route.fulfill({ json: {
    models: [{ id: 'first-model', name: 'First model' }, { id: 'chosen-model', name: 'Chosen model' }],
  } }))
  await page.goto('/#/settings?tab=archive&section=llm')
  await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  const model = page.getByRole('combobox', { name: 'Model', exact: true })
  await expect(model).toHaveValue('first-model')
  await model.selectOption('chosen-model')
  await expect(page.getByText('Model selection saved. Test and enable it to use this model.', { exact: true })).toBeVisible()
  expect(modelRequests).toEqual([{ subscription_provider: 'openai_chatgpt', subscription_model: 'chosen-model' }])
  expect(settingsRequests).toEqual([])
  expect(testRequests).toEqual([])
  await page.reload()
  await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await expect(model).toHaveValue('chosen-model')
  await expect(page.getByText('Titles, dates, tags and Archive research. Off.', { exact: true })).toBeVisible()
  await expect(page.getByLabel('This endpoint is not local. I acknowledge document text will leave this machine.')).not.toBeChecked()
})


test('Account subscription save errors do not block another provider', async ({ page }) => {
  await mockAPI(page, { filingTreeChosen: true, subscriptionConnected: true,
    llmEndpoint: '', llmModel: 'first-model' })
  await page.route('**/api/admin/settings/llm', route => {
    if (route.request().method() === 'PATCH') return route.fulfill({ status: 503, json: { error: 'Selection save unavailable' } })
    return route.fallback()
  })
  await page.route('**/api/admin/settings/llm/subscriptions/openai_chatgpt/models', route => route.fulfill({ json: {
    models: [{ id: 'first-model', name: 'First model' }, { id: 'other-model', name: 'Other model' }],
  } }))
  await page.goto('/#/settings?tab=archive&section=llm')
  await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await page.getByRole('combobox', { name: 'Model', exact: true }).selectOption('other-model')
  await expect(page.getByRole('alert')).toContainText('Selection save unavailable')
  await page.getByRole('button', { name: 'Local model', exact: true }).click()
  await page.getByRole('textbox', { name: 'Model', exact: true }).fill('local-model')
  await expect(page.getByRole('button', { name: 'Test connection', exact: true })).toBeEnabled()
})

test('Account subscription allows local matching saves after disconnect without changing model settings', async ({ page }) => {
  const archiveMatchingRequests = []
  const llmSettingsRequests = []
  await mockAPI(page, { filingTreeChosen: true, subscriptionConnected: true,
    llmEnabled: true, llmActive: true, llmModel: 'saved-model', llmEgressAck: true,
    archiveMatchingRequests, llmSettingsRequests })
  await page.route('**/api/admin/settings/llm/subscriptions/openai_chatgpt/models', route => route.fulfill({ json: {
    models: [{ id: 'saved-model', name: 'Saved model' }],
  } }))
  await page.route('**/api/admin/settings/llm/subscriptions/openai_chatgpt/disconnect', route => route.fulfill({ json: { connected: false } }))
  await page.goto('/#/settings?tab=archive&section=llm')
  await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  await expect(page.getByRole('combobox', { name: 'Model', exact: true })).toHaveValue('saved-model')
  await page.getByRole('button', { name: 'Disconnect ChatGPT', exact: true }).click()
  await expect(page.getByText('ChatGPT not connected', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Cancel', exact: true }).click()
  await page.getByRole('button', { name: 'Adjust', exact: true }).click()
  await page.getByLabel(/Minimum confidence to suggest for review/).fill('0.6')
  await page.getByRole('button', { name: 'Save matching options', exact: true }).click()
  await expect(page.getByText('Similar-document matching saved', { exact: true })).toBeVisible()
  expect(archiveMatchingRequests).toEqual([{ archive_enabled: true, archive_review_threshold: 0.6, archive_auto_threshold: 0.9 }])
  expect(llmSettingsRequests).toEqual([])
  await page.getByRole('button', { name: 'Set up model', exact: true }).first().click()
  await expect(page.getByRole('button', { name: 'Test connection', exact: true })).toBeDisabled()
  await expect(page.getByRole('button', { name: 'Enable model', exact: true })).toBeDisabled()
})

for (const [mode, endpoint] of [['Hosted endpoint', 'https://models.example.com/v1'], ['Local model', 'http://127.0.0.1:11434/v1']]) {
  test(`Account subscription preserves ${mode} draft across mode round trips`, async ({ page }) => {
    const settingsRequests = []
    await mockAPI(page, { filingTreeChosen: true, llmEnabled: true, llmActive: true,
      llmEndpoint: endpoint, llmModel: 'my-model', llmSettingsRequests: settingsRequests })
    await page.goto('/#/settings?tab=archive&section=llm')
    await page.getByRole('button', { name: 'Manage', exact: true }).first().click()
    const endpointInput = page.getByLabel('Endpoint URL', { exact: true })
    const model = page.getByRole('textbox', { name: 'Model', exact: true })
    await expect(endpointInput).toHaveValue(endpoint)
    await page.getByRole('button', { name: 'Account subscription', exact: true }).click()
    await page.getByRole('button', { name: mode, exact: true }).click()
    await expect(endpointInput).toHaveValue(endpoint)
    await expect(model).toHaveValue('my-model')
    // Unsaved edits survive too, and disabling sends the restored draft.
    await endpointInput.fill(endpoint + '/draft')
    await model.fill('draft-model')
    await page.getByRole('button', { name: 'Account subscription', exact: true }).click()
    await page.getByRole('button', { name: mode, exact: true }).click()
    await expect(endpointInput).toHaveValue(endpoint + '/draft')
    await expect(model).toHaveValue('draft-model')
    await page.getByRole('button', { name: 'Disable model', exact: true }).click()
    await expect.poll(() => settingsRequests.length).toBe(1)
    expect(settingsRequests[0]).toMatchObject({ enabled: false, endpoint_url: endpoint + '/draft', model: 'draft-model' })
  })
}

test('Account subscription restores saved model after disconnect and reconnect', async ({ page }) => {
  const settingsRequests = []
  const testRequests = []
  await mockAPI(page, { filingTreeChosen: true, subscriptionConnected: true,
    llmModel: 'saved-model', subscriptionModel: 'saved-model',
    llmSettingsRequests: settingsRequests, llmTestRequests: testRequests })
  await page.route('**/api/admin/settings/llm/subscriptions/openai_chatgpt/*', route => {
    const action = new URL(route.request().url()).pathname.split('/').at(-1)
    return route.fulfill({ json: action === 'models'
      ? { models: [{ id: 'first-model', name: 'First model' }, { id: 'saved-model', name: 'Saved model' }] }
      : action === 'start'
        ? { user_code: 'ABCD-1234', verification_url: 'https://auth.openai.com/codex/device', interval: 1 }
        : { connected: action !== 'disconnect' } })
  })
  await page.goto('/#/settings?tab=archive&section=llm')
  await page.getByRole('button', { name: /^(Set up model|Manage)$/ }).first().click()
  const model = page.getByRole('combobox', { name: 'Model', exact: true })
  await expect(model).toHaveValue('saved-model')
  await page.getByRole('button', { name: 'Disconnect ChatGPT', exact: true }).click()
  await expect(page.getByText('ChatGPT not connected', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Connect ChatGPT', exact: true }).click()
  await expect(model).toHaveValue('saved-model')
  await expect(page.getByRole('button', { name: 'Enable model', exact: true })).toBeDisabled()
  expect(settingsRequests).toEqual([])
  expect(testRequests).toEqual([])
})
