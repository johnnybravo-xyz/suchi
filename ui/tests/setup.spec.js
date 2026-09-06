import { expect, test } from '@playwright/test'

test('clears account data and rejects late reads after signing in as another user', async ({ page }) => {
  await mockAPI(page, {
    documentsCount: 731,
    setupCompletedAt: 1,
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
  await page.getByLabel('Email', { exact: true }).fill('second@example.test')
  await page.getByLabel('Password', { exact: true }).fill('test-password')
  await page.getByRole('button', { name: 'Sign in', exact: true }).click()
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
    const options = { setupCompletedAt: 1, filingTreeChosen: true, documents: [] }
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
    if (target === 'modal') {
      await expect(page.getByText('duplicate', { exact: true })).toBeVisible()
      await expect(page.locator('.dropveil')).toHaveCount(0)
      await page.getByRole('button', { name: 'Close upload' }).click()
    }
    await expect(page.getByRole('link', { name: /Dropped receipt.pdf/ })).toBeVisible()
    expect(uploads).toBe(1)
  })
}

for (const input of ['drop', 'picker']) {
  test('stops queued ' + input + ' uploads when the account changes', async ({ page }) => {
    await mockAPI(page, { setupCompletedAt: 1, filingTreeChosen: true })
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
  { id: 'solo', name: 'Solo', description: 'One person', areas: [] },
  { id: 'household', name: 'Household', description: 'A family', areas: [] },
  { id: 'freelance', name: 'Freelance', description: 'Client work', areas: [] },
  { id: 'smb_billing', name: 'Small business', description: 'Billing', areas: [] },
  { id: 'blank', name: 'Blank', description: 'Build your own', blank: true, areas: [] },
]

// Mirror the server's strict decoder so payload drift fails in the browser suite.
const llmInputFields = [
  'enabled', 'endpoint_url', 'model', 'api_key', 'clear_api_key', 'egress_ack',
  'confidence_threshold', 'date_auto_apply', 'archive_enabled',
  'archive_auto_threshold', 'archive_review_threshold',
]

function unexpectedFields(payload, allowed) {
  return Object.keys(payload).filter(key => !allowed.includes(key))
}

async function mockAPI(page, options = {}) {
  let taxonomyApplied = false
  let researchContextMode = options.researchContextMode || 'balanced'
  let trashDocuments = [...(options.trashDocuments || [])]
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
    const thumb = path.match(/^\/api\/documents\/(\d+)\/thumb\/?$/)
    const documentDetail = path.match(/^\/api\/documents\/(\d+)$/)
    const documentVersions = path.match(/^\/api\/documents\/(\d+)\/versions\/$/)
    const similarDocuments = path.match(/^\/api\/documents\/(\d+)\/similar$/)
    const documentAccess = path.match(/^\/api\/acls\/document\/(\d+)$/)
    const trashDocument = path.match(/^\/api\/trash\/(\d+)$/)
    const restoreDocument = path.match(/^\/api\/documents\/(\d+)\/restore$/)
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
    if (path === '/api/admin/settings/llm' && request.method() === 'PATCH' && options.researchContextSaveFailure) {
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
      email: options.demoSession ? 'visitor@demo.local' : 'admin@example.test',
      display_name: options.demoSession ? 'Demo visitor' : 'Admin',
      role: options.userRole || (options.demoSession ? 'member' : 'admin'),
      authn_by: options.demoSession ? 'demo' : 'local',
      build_version: options.buildVersion,
      build_revision: options.buildRevision,
      capabilities: options.capabilities ?? ['mailboxes'],
      ...(options.demoSession ? { demo: options.demoSession } : {}),
    }
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
    else if (path === '/api/presets/') body = { results: presets }
    else if (path === '/api/admin/setup/preset' && request.method() === 'POST') {
      taxonomyApplied = true
      body = { applied: true }
    }
    else if (path === '/api/admin/setup/state') body = {
      intent: '',
      recommended_preset: '',
      current_preset: options.currentPreset || '',
      filing_tree_chosen: options.filingTreeChosen ?? false,
      started_at: options.setupStartedAt ?? Math.floor(Date.now() / 1000),
      completed_at: options.setupCompletedAt ?? null,
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
      actions: [{ kind: 'assign_tags', name: 'Add tags', params: [{ name: 'tag_ids' }] }],
    }
    else if (path === '/api/admin/settings/llm') {
      if (request.method() === 'PATCH') {
        const payload = request.postDataJSON()
        const unexpected = unexpectedFields(payload, ['research_context_mode'])
        if (unexpected.length || Object.keys(payload).length !== 1 ||
            !['focused', 'balanced', 'detailed'].includes(payload.research_context_mode)) {
          await route.fulfill({
            status: 400,
            json: { code: 'bad_json', error: unexpected.length ? `unknown field ${unexpected[0]}` : 'invalid research context mode' },
          })
          return
        }
        options.researchContextRequests?.push(payload)
        researchContextMode = payload.research_context_mode
        body = { research_context_mode: researchContextMode }
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
        body = { saved: true, active: payload.enabled }
      } else {
        body = {
          enabled: options.llmEnabled ?? false,
          active: options.llmActive ?? false,
          endpoint_url: options.llmEndpoint || 'http://host.suchi.local:11434/v1',
          model: options.llmModel || 'qwen2.5:7b',
          has_api_key: options.llmHasAPIKey ?? false,
          egress_ack: options.llmEgressAck ?? false,
          confidence_threshold: 0.7,
          date_auto_apply: options.dateAutoApply ?? true,
          archive_enabled: true,
          archive_auto_threshold: 0.9,
          archive_review_threshold: 0.5,
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
            vars: { field: 'tag', value_id: 3, label: 'banking', confidence: 0.63, source: 'archive', based_on: [14, 15] },
          },
          {
            id: 10, run_id: 4, approval_id: 1, approval_name: 'document-change',
            doc_id: 17, doc_title: 'HDFC receipt.pdf', doc_jd_category_id: 8,
            doc_jd_category_code: 24, doc_jd_category_name: 'Receipts',
            doc_has_thumbnail: false, state_key: 'review', assignee: 'user:1',
            prompt: 'Review suggested document metadata', choices: ['apply', 'reject'],
            status: 'open', created_at: 1780100000,
            vars: { field: 'correspondent', value_id: 6, label: 'HDFC Bank', confidence: 0.63, source: 'archive', based_on: [14, 15] },
          },
        ],
      } : { counts: {}, results: [] }
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
    setupCompletedAt: Math.floor(Date.now() / 1000),
    filingTreeChosen: true,
  })
  await page.goto('/#/dashboard')

  await expect(page).toHaveURL(/#\/demo$/)
  await expect(page.getByRole('heading', { name: 'From a precise search to an answer with sources' })).toBeVisible()
  await expect(page.getByText('Rich query language', { exact: true })).toBeVisible()
  await expect(page.getByText('Archive research', { exact: true })).toBeVisible()
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

test('keeps fresh incomplete setup visible on the dashboard', async ({ page }) => {
  await mockAPI(page, { setupStartedAt: Math.floor(Date.now() / 1000) })
  await page.goto('/#/dashboard')

  const reminder = page.getByRole('complementary', { name: 'Setup wizard' })
  await expect(reminder.getByText('Choose your filing tree')).toBeVisible()
  await expect(reminder.getByText(/reopen Setup anytime from Settings/)).toBeVisible()
  await expect(reminder.getByRole('button', { name: 'Continue setup' })).toHaveAttribute('href', '#/setup')
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)

  if ((page.viewportSize()?.width || 0) > 860) {
    await expect(page.locator('.sidebar .setup-reminder')).toBeVisible()
  } else {
    const reminderBox = await reminder.boundingBox()
    const topbarBox = await page.locator('.topbar').boundingBox()
    expect(reminderBox.y).toBeGreaterThanOrEqual(topbarBox.y + topbarBox.height - 1)
  }

  await page.goto('/#/settings')
  const setupRow = page.getByRole('region', { name: 'Setup wizard' })
  await expect(setupRow.getByText('Setup is incomplete')).toBeVisible()
  await expect(setupRow.getByText('Choose a filing tree to finish the guided archive setup.')).toBeVisible()
  await expect(setupRow.getByRole('button', { name: 'Continue setup' })).toHaveAttribute('href', '#/setup')
  await page.goto('/#/dashboard')

  await reminder.getByRole('button', { name: 'Close setup reminder' }).click()
  await expect(reminder).toHaveCount(0)
  await expect(page.getByText('Setup reminder closed. Setup is always available in Settings.')).toBeVisible()
  await page.reload()
  await expect(page.getByRole('complementary', { name: 'Setup wizard' })).toHaveCount(0)

  await page.goto('/#/settings')
  await expect(setupRow).toHaveCount(0)
  await page.getByRole('link', { name: 'Archive configuration', exact: true }).click()
  await expect(page.getByRole('region', { name: 'Archive configuration' })).toBeVisible()
})

test('shows the dashboard setup reminder only to fresh incomplete admins', async ({ browser }) => {
  const cases = [
    { setupCompletedAt: Math.floor(Date.now() / 1000) },
    { filingTreeChosen: true },
    { setupStartedAt: Math.floor(Date.now() / 1000) - 49 * 60 * 60 },
    { userRole: 'member' },
  ]
  for (const options of cases) {
    const context = await browser.newContext()
    const page = await context.newPage()
    await mockAPI(page, options)
    await page.goto('/#/dashboard')
    await expect(page.getByRole('complementary', { name: 'Setup wizard' })).toHaveCount(0)
    await context.close()
  }
})

test('acknowledges the setup reminder when setup is opened', async ({ page }) => {
  await mockAPI(page, { setupStartedAt: Math.floor(Date.now() / 1000) })
  await page.goto('/#/dashboard')

  const reminder = page.getByRole('complementary', { name: 'Setup wizard' })
  await reminder.getByRole('button', { name: 'Continue setup' }).click()
  await expect(page).toHaveURL(/#\/setup$/)
  await expect(reminder).toHaveCount(0)

  await page.goto('/#/dashboard')
  await expect(page.getByRole('complementary', { name: 'Setup wizard' })).toHaveCount(0)
  await page.reload()
  await expect(page.getByRole('complementary', { name: 'Setup wizard' })).toHaveCount(0)

  await page.goto('/#/settings')
  await expect(page.getByText('Setup is incomplete')).toHaveCount(0)
  await page.getByRole('link', { name: 'Archive configuration', exact: true }).click()
  await expect(page.getByRole('region', { name: 'Archive configuration' })).toBeVisible()
})

test('opens Archive configuration after choosing a filing tree', async ({ page }) => {
  await mockAPI(page, {
    filingTreeChosen: true,
    currentPreset: 'solo',
    setupCompletedAt: null,
  })
  await page.goto('/#/settings?tab=archive')

  await expect(page.getByText('Setup is incomplete')).toHaveCount(0)
  const configuration = page.getByRole('region', { name: 'Archive configuration' })
  await expect(configuration).toBeVisible()
  await expect(configuration.getByRole('link', { name: /Filing tree/ }).last()).toBeVisible()
  await expect(configuration.getByRole('link', { name: /Email intake/ }).last()).toBeVisible()
  await expect(configuration.getByRole('link', { name: /Classification/ }).last()).toBeVisible()
  await expect(configuration.getByRole('link', { name: /OCR and backups/ }).last()).toBeVisible()
})

for (const userRole of ['admin', 'member']) {
  test(`shows the running build in ${userRole} settings`, async ({ page }) => {
    await mockAPI(page, {
      userRole, buildVersion: 'v0.1.0-beta.2', buildRevision: '1234567890ab',
      setupCompletedAt: 1, filingTreeChosen: true,
    })
    await page.goto('/#/settings')
    await expect(page.getByRole('contentinfo', { name: 'Suchi build' }))
      .toHaveText('Suchi v0.1.0-beta.2 · 1234567890ab')
    await page.route('**/api/whoami', route => route.fulfill({ json: {
      user_id: 1, email: 'admin@example.test', role: userRole, capabilities: [], build_version: 'dev',
    } }))
    await page.reload()
    await expect(page.getByRole('contentinfo', { name: 'Suchi build' })).toHaveText('Suchi dev')
  })
}

test('separates completed archive administration from account settings', async ({ page }, testInfo) => {
  const llmSettingsRequests = []
  await mockAPI(page, {
    setupCompletedAt: Math.floor(Date.now() / 1000),
    filingTreeChosen: true,
    currentPreset: 'household',
    llmSettingsRequests,
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
  const administration = configuration.getByRole('link', { name: /People and metadata/ }).last()
  await expect(administration).toHaveAttribute('href', '#/settings?tab=archive&section=users')
  await administration.click()
  await expect(page).toHaveURL(/#\/settings\?tab=archive&section=users$/)
  await expect(configuration.getByRole('button', { name: 'Users', exact: true })).toBeVisible()
  await expect(configuration.getByRole('button', { name: 'Groups', exact: true })).toBeVisible()
  await expect(configuration.getByRole('button', { name: 'Custom fields', exact: true })).toBeVisible()
  await expect(configuration.getByRole('button', { name: 'Taxonomy', exact: true })).toBeVisible()
  await configuration.getByRole('link', { name: 'Archive overview' }).click()
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
  await expect(page.getByRole('heading', { name: 'Classification' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Hosted endpoint' })).toBeVisible()
  const testConnection = page.getByRole('button', { name: 'Test connection' })
  await expect(page.getByRole('button', { name: 'Save model and options' })).toBeDisabled()
  await testConnection.click()
  await expect(page.getByText('Validated in 12 ms')).toBeVisible()
  await expect(page.getByText('Works without a model', { exact: true })).toBeVisible()
  await expect(page.getByText('Uses the configured model', { exact: true })).toBeVisible()
  const dateAutoApply = page.getByLabel('Add high-confidence dates to Calendar automatically')
  const saveOptions = page.getByRole('button', { name: 'Save model and options' })
  await expect(dateAutoApply).toBeChecked()
  const controlsAreOrdered = await page.evaluate(() => {
    const test = [...document.querySelectorAll('button')].find(node => node.textContent.trim() === 'Test connection')
    const dates = [...document.querySelectorAll('label')].find(node => node.textContent.includes('Add high-confidence dates'))
    const save = [...document.querySelectorAll('button')].find(node => node.textContent.trim() === 'Save model and options')
    return !!test && !!dates && !!save &&
      !!(test.compareDocumentPosition(dates) & Node.DOCUMENT_POSITION_FOLLOWING) &&
      !!(dates.compareDocumentPosition(save) & Node.DOCUMENT_POSITION_FOLLOWING)
  })
  expect(controlsAreOrdered).toBe(true)
  await dateAutoApply.scrollIntoViewIfNeeded()
  await page.screenshot({ path: `/tmp/suchi-date-setting-${testInfo.project.name}.png`, fullPage: true })
  await dateAutoApply.uncheck()
  await saveOptions.click()
  await expect.poll(() => llmSettingsRequests.length).toBe(1)
  expect(llmSettingsRequests[0].date_auto_apply).toBe(false)
  await expect(page.getByRole('button', { name: 'Finish setup' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: /Skip|Done|Defaults are fine/ })).toHaveCount(0)
  const overviewReload = page.waitForRequest((request) =>
    new URL(request.url()).pathname === '/api/admin/settings/preferences'
  )
  await configuration.getByRole('link', { name: 'Archive overview' }).click()
  await overviewReload
  await expect(page).toHaveURL(/#\/settings\?tab=archive$/)
})

test('separates model-free matching saves from validated model settings', async ({ page }) => {
  const llmSettingsRequests = []
  await mockAPI(page, {
    setupCompletedAt: Math.floor(Date.now() / 1000),
    filingTreeChosen: true,
    llmSettingsRequests,
    llmHasAPIKey: true,
  })
  await page.goto('/#/settings?tab=archive&section=llm')

  const saveMatching = page.getByRole('button', { name: 'Save matching options' })
  await saveMatching.click()
  await expect.poll(() => llmSettingsRequests.length).toBe(1)
  expect(llmSettingsRequests[0]).toMatchObject({
    enabled: false,
    archive_enabled: true,
    endpoint_url: 'http://host.suchi.local:11434/v1',
    model: 'qwen2.5:7b',
  })

  const saveModel = page.getByRole('button', { name: 'Save model and options' })
  const testConnection = page.getByRole('button', { name: 'Test connection' })
  await expect(saveModel).toBeDisabled()
  await testConnection.click()
  await expect(page.getByText('Validated in 12 ms')).toBeVisible()
  await expect(saveModel).toBeEnabled()

  await page.getByLabel('Endpoint URL').fill('http://localhost:11435/v1')
  await expect(page.getByText('Validated in 12 ms')).toHaveCount(0)
  await expect(saveModel).toBeDisabled()
  await testConnection.click()
  await expect(saveModel).toBeEnabled()
  await page.getByLabel('Model', { exact: true }).fill('qwen2.5:14b')
  await expect(page.getByText('Validated in 12 ms')).toHaveCount(0)
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
  const egress = page.getByLabel('This endpoint is not local. I acknowledge document text will leave this machine.')
  await egress.check()
  await testConnection.click()
  await expect(saveModel).toBeEnabled()
  await egress.uncheck()
  await expect(page.getByText('Validated in 12 ms')).toHaveCount(0)
  await expect(saveModel).toBeDisabled()
})

test('preserves an enabled model when saving archive matching', async ({ page }) => {
  const llmSettingsRequests = []
  await mockAPI(page, {
    setupCompletedAt: Math.floor(Date.now() / 1000),
    filingTreeChosen: true,
    llmSettingsRequests,
    llmEnabled: true,
    llmActive: true,
    llmEndpoint: 'https://models.example.test/v1',
    llmModel: 'archive-model',
    llmEgressAck: true,
  })
  await page.goto('/#/settings?tab=archive&section=llm')

  await page.getByLabel(/Apply a matching document's filing at/).fill('0.85')
  await page.getByRole('button', { name: 'Save matching options' }).click()
  await expect.poll(() => llmSettingsRequests.length).toBe(1)
  expect(llmSettingsRequests[0]).toMatchObject({
    enabled: true,
    endpoint_url: 'https://models.example.test/v1',
    model: 'archive-model',
    egress_ack: true,
    archive_auto_threshold: 0.85,
  })
})

test('loads Balanced research context and saves each bounded preset independently', async ({ page }) => {
  const researchContextRequests = []
  const llmSettingsRequests = []
  await mockAPI(page, {
    setupCompletedAt: Math.floor(Date.now() / 1000),
    filingTreeChosen: true,
    researchContextRequests,
    llmSettingsRequests,
  })
  await page.goto('/#/settings?tab=archive&section=llm')

  const research = page.getByRole('region', { name: 'Archive research configuration' })
  await expect(research.getByRole('radio', { name: /Balanced/ })).toBeChecked()
  await expect(research.locator('input[type="number"]')).toHaveCount(0)
  await expect(research.getByText('1 matching passage · up to 1,600 characters')).toBeVisible()
  await expect(research.getByText('3 matching passages · up to 4,800 characters')).toBeVisible()
  await expect(research.getByText(/may also include a separate document ending/)).toBeVisible()

  for (const mode of ['Focused', 'Balanced', 'Detailed']) {
    await research.getByRole('radio', { name: new RegExp(mode) }).check()
    await research.getByRole('button', { name: 'Save research context' }).click()
    await expect.poll(() => researchContextRequests.length).toBe(
      ['Focused', 'Balanced', 'Detailed'].indexOf(mode) + 1
    )
  }
  expect(researchContextRequests).toEqual([
    { research_context_mode: 'focused' },
    { research_context_mode: 'balanced' },
    { research_context_mode: 'detailed' },
  ])
  expect(llmSettingsRequests).toEqual([])
})

test('keeps research context out of model test, save, and disable payloads', async ({ page }) => {
  const llmSettingsRequests = []
  const llmTestRequests = []
  await mockAPI(page, {
    setupCompletedAt: Math.floor(Date.now() / 1000),
    filingTreeChosen: true,
    llmSettingsRequests,
    llmTestRequests,
  })
  await page.goto('/#/settings?tab=archive&section=llm')

  await page.getByRole('radio', { name: /Detailed/ }).check()
  await page.getByRole('button', { name: 'Test connection' }).click()
  await expect(page.getByText('Validated in 12 ms')).toBeVisible()
  await expect.poll(() => llmTestRequests.length).toBe(1)

  const enabledPayload = {
    enabled: true,
    endpoint_url: 'http://host.suchi.local:11434/v1',
    model: 'qwen2.5:7b',
    api_key: '',
    clear_api_key: false,
    egress_ack: false,
    confidence_threshold: 0.7,
    date_auto_apply: true,
    archive_enabled: true,
    archive_auto_threshold: 0.9,
    archive_review_threshold: 0.5,
  }
  expect(llmTestRequests[0]).toEqual(enabledPayload)

  await page.getByRole('button', { name: 'Save model and options' }).click()
  await expect.poll(() => llmSettingsRequests.length).toBe(1)
  expect(llmSettingsRequests[0]).toEqual(enabledPayload)

  await page.getByRole('button', { name: 'Disable model' }).click()
  await expect.poll(() => llmSettingsRequests.length).toBe(2)
  expect(llmSettingsRequests[1]).toEqual({ ...enabledPayload, enabled: false })
})

test('keeps research context editable when its standalone save fails', async ({ page }) => {
  await mockAPI(page, {
    setupCompletedAt: Math.floor(Date.now() / 1000),
    filingTreeChosen: true,
    researchContextMode: 'unexpected',
    researchContextSaveFailure: true,
    failureMessage: 'research context unavailable',
  })
  await page.goto('/#/settings?tab=archive&section=llm')

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
    setupCompletedAt: Math.floor(Date.now() / 1000),
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

test('keeps failed configuration reads out of editable forms', async ({ page }) => {
  await mockAPI(page, {
    setupCompletedAt: Math.floor(Date.now() / 1000),
    filingTreeChosen: true,
    failPaths: ['/api/admin/settings/llm'],
    failureMessage: 'classification settings unavailable',
  })
  await page.goto('/#/settings?tab=archive&section=llm')

  await expect(page.getByText('classification settings unavailable')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Retry' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Save model and options' })).toHaveCount(0)
})

test('distinguishes mailbox and saved-view failures from empty data', async ({ page }) => {
  await mockAPI(page, {
    failPaths: ['/api/email-accounts', '/api/saved_views/'],
    failureMessage: 'archive data unavailable',
  })
  await page.goto('/#/setup')
  await page.getByRole('button', { name: 'Email intake' }).click()
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

test('guides intent, filing tree, and LLM mode without exposing import', async ({ page }) => {
  await mockAPI(page)
  await page.goto('/#/setup')

  await expect(page.getByRole('heading', { name: 'What are you organizing?' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Import a file' })).toHaveCount(0)
  await page.getByRole('button', { name: /Personal/ }).click()
  await expect(page.getByText('Recommended for Personal')).toBeVisible()
  const filingTrees = page.locator('.preset-grid')
  await expect(filingTrees.getByText('Household', { exact: true })).toHaveCount(0)
  await page.getByRole('button', { name: 'Compare all filing trees' }).click()
  await expect(filingTrees.getByText('Household', { exact: true })).toBeVisible()

  await page.getByRole('button', { name: 'Classification' }).click()
  await expect(page.getByRole('button', { name: 'Local model' })).toBeVisible()
  await expect(page.locator('#l-confidence')).toHaveAttribute('min', '0.5')
  await expect(page.locator('#l-confidence')).toHaveAttribute('max', '0.95')
  await expect(page.locator('#l-confidence')).toHaveAttribute('step', '0.05')
  await page.getByRole('button', { name: 'Hosted endpoint' }).click()
  await page.locator('#l-url').fill('https://llm.example.test/v1')
  await expect(page.getByText(/I acknowledge document text will leave this machine/)).toBeVisible()
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

  await page.goto('/#/setup')
  const finish = page.getByRole('button', { name: 'Finish setup' })
  await expect(finish).toBeDisabled()
  await page.getByRole('button', { name: /Personal/ }).click()
  await page.getByRole('button', { name: 'Apply filing tree' }).click()

  await expect(finish).toBeEnabled()
  await page.goto('/#/dashboard')
  await expect(page.getByRole('complementary', { name: 'Setup wizard' })).toHaveCount(0)
  await expect(indexHeading).toHaveCount(1)
  if ((page.viewportSize()?.width || 0) <= 860) {
    await page.getByRole('button', { name: 'Open navigation' }).click()
  }
  await page.locator('.area-toggle').filter({ hasText: 'Life admin' }).click()
  await expect(page.locator('a[href="#/documents?jd=11"]')).toContainText('Identity')
  await expect(page.locator('.jd-tree a[href="#/documents?jd=49"]')).toHaveCount(0)

  await page.goto('/#/settings')
  await expect(page.getByText('Setup is incomplete')).toHaveCount(0)
  await page.getByRole('link', { name: 'Archive configuration', exact: true }).click()
  await expect(page.getByRole('region', { name: 'Archive configuration' })).toBeVisible()
})

test('uses the demo category database id in document links', async ({ page }) => {
  await mockAPI(page, {
    jdCategories: [{
      id: 6, code: 22, name: 'Finance and tax', area_code: 20, area_name: 'Money',
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
    jdCategories: [{ id: 6, area_code: 20, area_name: 'Finance', code: 22, name: 'Investments' }],
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
  await expect(page.getByText(/page 1 of 2/)).toBeVisible()
})

test('opens dashboard views through user-facing document routes', async ({ page }) => {
  const filters = {
    q: 'distribution advice',
    tags__id__in: '2',
    correspondents__id__in: '3',
    document_type__id: '4',
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
    jdCategories: [{ id: 6, code: 22, name: 'Investments', area_code: 20, area_name: 'Finance', is_area: false }],
  })
  await page.goto('/#/views')
  await page.getByRole('button', { name: 'New view' }).click()
  const dialog = page.getByRole('dialog', { name: 'Create a view' })
  await dialog.getByLabel('View name').fill('Private investments')
  await dialog.getByLabel('Query').fill('"distribution advice"')
  await dialog.getByLabel('Filing category').selectOption('6')
  await dialog.getByLabel('Sensitivity').selectOption('confidential')

  const saveRequest = page.waitForRequest(request => {
    return new URL(request.url()).pathname === '/api/saved_views/' && request.method() === 'POST'
  })
  await dialog.getByRole('button', { name: 'Save view' }).click()
  const payload = (await saveRequest).postDataJSON()
  expect(JSON.parse(payload.filter_json)).toEqual({
    q: '"distribution advice" jd:22 sensitivity:confidential',
  })
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
    setupCompletedAt: Math.floor(Date.now() / 1000),
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
    if (['/api/tags/', '/api/correspondents/', '/api/document_types/'].includes(url.pathname)) {
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
  await expect(page.getByText('No saved views yet')).toBeVisible()
  expect(facetRequests).toEqual([])

  const facets = page.waitForRequest(request => new URL(request.url()).pathname === '/api/tags/')
  await page.getByRole('button', { name: 'New view' }).click()
  await facets
})

test('defers automation facets until an empty workspace is edited', async ({ page }) => {
  const facetRequests = []
  page.on('request', request => {
    const path = new URL(request.url()).pathname
    if (['/api/tags/', '/api/correspondents/', '/api/document_types/'].includes(path)) facetRequests.push(path)
  })
  await mockAPI(page, { automations: [] })
  await page.goto('/#/automations')

  await expect(page.getByText('No automations yet.')).toBeVisible()
  expect(facetRequests).toEqual([])
  await page.getByRole('button', { name: 'New automation' }).click()
  await expect.poll(() => new Set(facetRequests).size).toBe(3)
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

test('shows shared views without offering to delete another users view', async ({ page }) => {
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
})

test('lets admins grant saved-view sharing to members', async ({ page }) => {
  await mockAPI(page, {
    setupCompletedAt: Math.floor(Date.now() / 1000),
    filingTreeChosen: true,
  })
  await page.goto('/#/settings?tab=archive&section=users')

  await expect(page.getByRole('switch', {
    name: 'Share saved views capability for member@example.test',
  })).toBeVisible()
})

test('offers Microsoft sign-in without exposing registration controls', async ({ page }) => {
  await mockAPI(page)
  await page.goto('/#/setup')

  await page.getByRole('button', { name: 'Email intake' }).click()
  await page.getByRole('button', { name: 'Add mailbox' }).click()
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

test('groups metadata reviews by document', async ({ page }) => {
  await mockAPI(page)

  await page.goto('/#/tasks')
  await expect(page.getByRole('link', { name: 'HDFC receipt.pdf', exact: true })).toBeVisible()
  await expect(page.getByText('24 Receipts', { exact: true })).toBeVisible()
  await expect(page.getByText('2 suggestions')).toBeVisible()
  await expect(page.getByText('Add “banking” tag?')).toBeVisible()
  await expect(page.getByText('Set correspondent to “HDFC Bank”?')).toBeVisible()
  await expect(page.getByText('document-change', { exact: true }).first()).toBeHidden()
  if ((page.viewportSize()?.width || 0) > 1050) {
    expect(Math.round((await page.locator('.approval-grid[data-approval-kind="workflow"]').boundingBox()).width)).toBeLessThanOrEqual(820)
  }
})

test('lays out more than two workflow approvals in the shared responsive grid', async ({ page }, testInfo) => {
  const approvalTasks = [17, 18, 19].map((docID, index) => ({
    id: 90 + index, run_id: 30 + index, approval_id: 8, approval_name: 'document-change',
    doc_id: docID, doc_title: `Review document ${index + 1}.pdf`, doc_has_thumbnail: false,
    state_key: 'review', assignee: 'user:1', prompt: 'Review suggested document metadata',
    choices: ['apply', 'reject'], status: 'open', created_at: 1780100000,
    vars: { field: 'tag', value_id: 40 + index, label: `review-${index + 1}`, confidence: 0.74, source: 'archive', based_on: [2] },
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

  await page.getByText('Details', { exact: true }).click()
  await expect(page.getByText('Affected documents')).toBeVisible()
  await expect(page.getByRole('link', { name: 'SBI account statement August 2026.pdf' })).toHaveAttribute('href', '#/doc/18')
  await expect(page.getByRole('link', { name: 'HDFC Infinia card statement August 2026.pdf' })).toHaveAttribute('href', '#/doc/20')
  await expect(page.getByText('and 3 more')).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
})

test('persists user and automation switches', async ({ page }) => {
  await mockAPI(page, {
    setupCompletedAt: Math.floor(Date.now() / 1000),
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
    setupCompletedAt: Math.floor(Date.now() / 1000),
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
    setupCompletedAt: Math.floor(Date.now() / 1000),
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
    setupCompletedAt: Math.floor(Date.now() / 1000),
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
    setupCompletedAt: Math.floor(Date.now() / 1000),
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
      setupCompletedAt: Math.floor(Date.now() / 1000),
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
    setupCompletedAt: Math.floor(Date.now() / 1000),
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

  await page.goto('/#/documents?q=needle&jd=6&document_ids=41,42&tags__id__in=5,8&correspondents__id__in=7&document_type__id=4&sensitivity=internal')
  await page.getByTitle('Added on or after').fill('2026-08-01')
  await page.getByTitle('Added on or before').fill('2026-08-30')
  await page.waitForTimeout(50)
  await ask('full scope')
  expect(chatRequests.at(-1).scope).toEqual({
    query: 'needle', document_ids: [41, 42], jd_category_id: 6,
    sensitivity: 'internal', document_type_id: 4, tag_ids: [5, 8],
    correspondent_ids: [7], created_at_gte: 1785542400,
    created_at_lte: 1788134399, language: '',
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
    document_type_id: 0, tag_ids: [], correspondent_ids: [],
    created_at_gte: null, created_at_lte: null, language: 'de',
  })
})

test('keeps modified research anchors in the current drawer session', async ({ page }) => {
  await mockAPI(page, {
    chatEnabled: true,
    chatRequests: [],
    setupCompletedAt: Math.floor(Date.now() / 1000),
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
    setupCompletedAt: Math.floor(Date.now() / 1000),
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
    setupCompletedAt: Math.floor(Date.now() / 1000),
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
    setupCompletedAt: Math.floor(Date.now() / 1000),
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
  await mockAPI(page, { setupCompletedAt: Math.floor(Date.now() / 1000), filingTreeChosen: true })
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
    setupCompletedAt: Math.floor(Date.now() / 1000),
    filingTreeChosen: true,
    intelligence: [
      {
        id: 71, document_id: 17, document_title: 'Lease agreement.pdf',
        document_has_thumbnail: false, type: 'date', role: 'renewal',
        value: { date: '2026-09-01', precision: 'day' }, sort_value: '2026-09-01',
        raw_text: 'September 1, 2026', evidence_text: 'The lease renews on September 1, 2026.',
        confidence: 0.94, status: 'pending',
      },
      {
        id: 72, document_id: 17, document_title: 'Lease agreement.pdf',
        document_has_thumbnail: false, type: 'date', role: 'issued',
        value: { date: '2025-08-12', precision: 'day' }, sort_value: '2025-08-12',
        raw_text: '12 August 2025', evidence_text: 'Signed on 12 August 2025.',
        confidence: 0.62, status: 'pending',
      },
      {
        id: 73, document_id: 18, document_title: 'Boarding pass.pdf',
        document_has_thumbnail: false, type: 'date', role: 'service',
        value: { date: '2026-08-06', precision: 'day' }, sort_value: '2026-08-06',
        raw_text: '06 Aug 2026', evidence_text: 'Date 06 Aug 2026',
        confidence: 0.95, status: 'pending',
      },
      {
        id: 74, document_id: 19, document_title: 'Restaurant receipt.pdf',
        document_has_thumbnail: false, type: 'date', role: 'issued',
        value: { date: '2025-03-01', precision: 'day' }, sort_value: '2025-03-01',
        raw_text: '3/1/25', evidence_text: 'Date: 3/1/25, 2:48 PM',
        confidence: 0.98, status: 'pending',
      },
    ],
  })
  await page.goto('/#/tasks')

  await expect(page.getByRole('heading', { name: 'Check dates before they reach Calendar' })).toBeVisible()
  await expect(page.getByText('Dates needing a quick check', { exact: true })).toBeVisible()
  expect(await page.getByText('Dates needing a quick check', { exact: true }).evaluate(element => getComputedStyle(element).textTransform)).toBe('none')
  await expect(page.getByText('3 of 4 dates selected')).toBeVisible()
  const reviewGrid = page.locator('.approval-grid[data-approval-kind="date"]')
  await expect(reviewGrid).toHaveClass(/approval-grid-many/)
  const columnCount = await reviewGrid.evaluate(element => getComputedStyle(element).gridTemplateColumns.split(' ').length)
  expect(columnCount).toBe((page.viewportSize()?.width || 0) > 1050 ? 2 : 1)
  const actions = page.getByRole('group', { name: 'Review selected dates' })
  await expect(actions).toBeInViewport()
  expect(await actions.evaluate(element => getComputedStyle(element).position)).toBe('sticky')
  await page.screenshot({ path: `/tmp/suchi-date-review-${testInfo.project.name}.png`, fullPage: true })
  await page.getByText('Document text: “Date: 3/1/25, 2:48 PM”').scrollIntoViewIfNeeded()
  await expect(actions).toBeInViewport()
  await page.getByLabel('Select all dates').check()
  await expect(page.getByText('4 of 4 dates selected')).toBeVisible()
  await page.getByRole('button', { name: 'Add 4 to Calendar' }).click()

  expect(intelligenceRequests).toContainEqual({
    action: 'resolve', candidate_ids: [71, 72, 73, 74], decision: 'accepted',
  })
  await expect(page.getByRole('heading', { name: 'Check dates before they reach Calendar' })).toHaveCount(0)
})

test('shows automatic and reviewed dates on the calendar', async ({ page }) => {
  const intelligenceQueries = []
  const now = new Date()
  const year = now.getFullYear()
  const month = String(now.getMonth() + 1).padStart(2, '0')
  const date = `${year}-${month}-14`
  await mockAPI(page, {
    setupCompletedAt: Math.floor(Date.now() / 1000),
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
      id: 82, document_id: 29, document_title: 'Automatic policy reminder',
      document_has_thumbnail: false, type: 'date', role: 'renewal',
      value: { date, precision: 'day' }, sort_value: date,
      raw_text: date, evidence_text: `Renewal starts on ${date}.`,
      confidence: 0.99, status: 'accepted', reviewed_at: null,
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
  await expect(page.getByText('Dates added automatically or approved in Approvals appear here. Each date links to the document it came from.')).toBeVisible()
  await expect(page.getByText('Home insurance renewal notice', { exact: true }).first()).toBeVisible()
  await expect(page.locator('.agenda-event').getByText('Expiry', { exact: true })).toBeVisible()
  await expect(page.locator('.agenda-event').filter({ hasText: 'Home insurance renewal notice' }).getByText('Reviewed', { exact: true })).toBeVisible()
  await expect(page.locator('.agenda-event').filter({ hasText: 'Automatic policy reminder' }).getByText('Automatic', { exact: true })).toBeVisible()
  await expect(page.getByRole('heading', { name: '2 of 650 dates' })).toBeVisible()
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
    chatEnabled: true, setupCompletedAt: 1, filingTreeChosen: true, intelligenceQueries,
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
  await mockAPI(page, { setupCompletedAt: 1, filingTreeChosen: true, intelligence: dates })
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
  const confidence = month.getByText('LLM Classifier confidence: 92%', { exact: true })
  await expect(confidence).toBeHidden()
  await expect(day.locator('summary')).toHaveText('Reviewed')
  await expect(month.locator('summary')).toHaveText('Automatic')

  const status = month.locator('summary')
  if (testInfo.project.use.hasTouch) await status.tap()
  else { await status.focus(); await page.keyboard.press('Enter') }
  await expect(month.locator('details')).toHaveAttribute('open', '')
  await expect(confidence).toBeVisible()
  await expect(month.locator('details p')).toHaveText('LLM Classifier confidence: 92%')
  await expect(page).toHaveURL(/#\/calendar\?document_ids=17,18,19$/)
  await month.getByRole('link', { name: 'Annual service schedule', exact: true }).click()
  await expect(page).toHaveURL(/#\/doc\/18$/)
})

test('keeps month-only and year-only dates out of calendar day cells', async ({ page }) => {
  await page.clock.setFixedTime(new Date('2026-01-15T12:00:00Z'))
  const intelligenceQueries = []
  await mockAPI(page, {
    setupCompletedAt: 1, filingTreeChosen: true, intelligenceQueries,
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
    setupCompletedAt: 1, filingTreeChosen: true, intelligenceQueries,
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
    setupCompletedAt: 1, filingTreeChosen: true, intelligenceQueries,
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
    setupCompletedAt: 1, filingTreeChosen: true, intelligenceQueries,
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
  await mockAPI(page, { setupCompletedAt: 1, filingTreeChosen: true })
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
    setupCompletedAt: 1, filingTreeChosen: true, userID: 7, userRole: 'member', capabilities: ['share_links'],
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
    setupCompletedAt: 1, filingTreeChosen: true, apiRequests, permanentDeleteRequests,
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
      setupCompletedAt: 1, filingTreeChosen: true, userID: 7, userRole: scenario.userRole,
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
    setupCompletedAt: Math.floor(Date.now() / 1000),
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
    setupCompletedAt: Math.floor(Date.now() / 1000),
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
