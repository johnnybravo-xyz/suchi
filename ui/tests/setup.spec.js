import { expect, test } from '@playwright/test'

const presets = [
  { id: 'solo', name: 'Solo', description: 'One person', areas: [] },
  { id: 'household', name: 'Household', description: 'A family', areas: [] },
  { id: 'freelance', name: 'Freelance', description: 'Client work', areas: [] },
  { id: 'smb_billing', name: 'Small business', description: 'Billing', areas: [] },
  { id: 'blank', name: 'Blank', description: 'Build your own', blank: true, areas: [] },
]

async function mockAPI(page, options = {}) {
  let taxonomyApplied = false
  await page.route('**/preview/**', async route => {
    await route.fulfill({
      contentType: 'text/html',
      body: options.previewHTML || '<p>Document preview</p>',
    })
  })
  await page.route('**/api/**', async route => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    const thumb = path.match(/^\/api\/documents\/(\d+)\/thumb\/?$/)
    const documentDetail = path.match(/^\/api\/documents\/(\d+)$/)
    const documentVersions = path.match(/^\/api\/documents\/(\d+)\/versions\/$/)
    const similarDocuments = path.match(/^\/api\/documents\/(\d+)\/similar$/)
    const documentAccess = path.match(/^\/api\/acls\/document\/(\d+)$/)
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
    if (options.failPaths?.includes(path)) {
      await route.fulfill({ status: 500, json: { error: options.failureMessage || 'forced request failure' } })
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
    else if (path === '/api/admin/settings/llm') body = {
      enabled: false,
      active: false,
      endpoint_url: 'http://host.suchi.local:11434/v1',
      model: 'qwen2.5:7b',
      confidence_threshold: 0.7,
      archive_enabled: true,
      archive_auto_threshold: 0.9,
      archive_review_threshold: 0.5,
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
      options.intelligenceQueries?.push(Object.fromEntries(new URL(request.url()).searchParams))
      body = { results: options.intelligence || [], count: options.intelligence?.length || 0 }
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
        ...response?.document,
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
  await expect(page.getByRole('heading', { name: 'From a precise query to a grounded answer' })).toBeVisible()
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

test('separates completed archive administration from account settings', async ({ page }) => {
  await mockAPI(page, {
    setupCompletedAt: Math.floor(Date.now() / 1000),
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
  await expect(page.getByRole('button', { name: 'Finish setup' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: /Skip|Done|Defaults are fine/ })).toHaveCount(0)
  const overviewReload = page.waitForRequest((request) =>
    new URL(request.url()).pathname === '/api/admin/settings/preferences'
  )
  await configuration.getByRole('link', { name: 'Archive overview' }).click()
  await overviewReload
  await expect(page).toHaveURL(/#\/settings\?tab=archive$/)
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
  await expect(page.getByRole('button', { name: 'Save classifier' })).toHaveCount(0)
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

test('keeps the newest command-palette suggestions', async ({ page }) => {
  await mockAPI(page, {
    autocompleteByQuery: {
      'tag:o': { delay: 300, results: [{ value: 'Old suggestion', kind: 'tag', query: 'tag:old' }] },
      'tag:n': { results: [{ value: 'Current suggestion', kind: 'tag', query: 'tag:new' }] },
    },
  })
  await page.goto('/#/dashboard')

  const input = page.getByRole('searchbox', { name: 'Search or run a command' })
  const oldRequest = page.waitForRequest(request => {
    const url = new URL(request.url())
    return url.pathname === '/api/autocomplete/' && url.searchParams.get('q') === 'tag:o'
  })
  await input.fill('tag:o')
  await oldRequest
  await input.fill('tag:n')

  await expect(page.getByText('Current suggestion')).toBeVisible()
  await page.waitForTimeout(350)
  await expect(page.getByText('Old suggestion')).toHaveCount(0)
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
  await expect(page.getByRole('button', { name: 'Jump to source 1' })).toBeVisible()
  expect(chatRequests).toHaveLength(1)
  expect(chatRequests[0]).toMatchObject({
    question: 'When does the lease renew?',
    include_sensitive: false,
    history: [],
    context_source_ids: [],
    scope: { query: '', document_ids: [], jd_category_id: 0 },
  })
  expect(await page.evaluate(() => performance.getEntriesByType('resource').some(entry => entry.name.includes('ArchiveChat')))).toBe(true)

  await page.getByRole('link', { name: /Save source set/ }).click()
  await expect(page).toHaveURL(/#\/views\?new=1&ids=17$/)
  await expect(page.getByRole('dialog', { name: 'Create a view' })).toBeVisible()
  await expect(page.getByText('Exact research snapshot')).toBeVisible()
  await page.getByRole('button', { name: 'Close create view' }).click()
  await expect(page).toHaveURL(/#\/views$/)

  await page.goto('/#/dashboard')
  await page.getByRole('button', { name: 'Ask the archive' }).click()
  await page.getByRole('link', { name: /Save source set/ }).click()
  await expect(page).toHaveURL(/#\/views\?new=1&ids=17$/)
  await expect(page.getByRole('dialog', { name: 'Create a view' })).toBeVisible()
  await page.getByRole('button', { name: 'Close create view' }).click()
  await expect(page).toHaveURL(/#\/views$/)

  await page.goto('/#/dashboard')
  await page.getByRole('button', { name: 'Ask the archive' }).click()
  await expect(page.getByText('The lease renews in September')).toBeVisible()
  const source = page.getByRole('link', { name: 'Open source 1: Lease agreement.pdf' })
  await expect(source).toHaveAttribute('href', '#/doc/17')
  await source.click()
  await expect(page).toHaveURL(/#\/doc\/17$/)

  await page.getByRole('button', { name: 'Ask the archive' }).click()
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
  await expect(dialog).toBeVisible()
  await expect.poll(async () => (await dialog.boundingBox()).x).toBe(0)
  await expect.poll(async () => Math.round((await dialog.boundingBox()).width)).toBe(390)
  await page.getByRole('button', { name: 'Cancel' }).click()
  await expect(page.getByText('Request canceled.')).toBeVisible()
  await expect(page.getByRole('textbox', { name: 'Question', exact: true })).toHaveValue('slow question')
  await page.getByRole('textbox', { name: 'Question', exact: true }).press('Escape')
  await expect(dialog).toBeHidden()
  await expect(omnibox).toBeFocused()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
})

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
  await composer.press('Escape')
  await page.getByRole('button', { name: 'Ask the archive' }).click()
  await expect(page.getByText('same scope survives close')).toBeVisible()
  await composer.press('Escape')

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

test('suppresses the global Omnibox shortcut behind modal dialogs', async ({ page }) => {
  await mockAPI(page, { setupCompletedAt: Math.floor(Date.now() / 1000), filingTreeChosen: true })
  await page.goto('/#/dashboard')
  await page.getByRole('button', { name: 'Upload documents' }).click()
  await expect(page.getByRole('dialog', { name: 'Upload documents' })).toBeVisible()
  await page.keyboard.press(process.platform === 'darwin' ? 'Meta+k' : 'Control+k')
  await expect(page.getByLabel('Search or run a command')).not.toBeFocused()
})

test('bulk-validates generic intelligence candidates by document', async ({ page }) => {
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
    ],
  })
  await page.goto('/#/tasks')

  await expect(page.getByRole('heading', { name: 'Intelligence review' })).toBeVisible()
  await expect(page.getByText('Sep 1, 2026 · renewal')).toBeVisible()
  await expect(page.getByText('1 selected')).toBeVisible()
  await page.getByRole('checkbox', { name: 'Select every candidate from Lease agreement.pdf' }).check()
  await page.getByRole('button', { name: 'Accept 2' }).click()

  expect(intelligenceRequests).toContainEqual({
    action: 'resolve', candidate_ids: [71, 72], decision: 'accepted',
  })
  await expect(page.getByRole('heading', { name: 'Intelligence review' })).toHaveCount(0)
})

test('shows only accepted date intelligence on the calendar', async ({ page }) => {
  const intelligenceQueries = []
  const now = new Date()
  const year = now.getFullYear()
  const month = String(now.getMonth() + 1).padStart(2, '0')
  const date = `${year}-${month}-14`
  await mockAPI(page, {
    setupCompletedAt: Math.floor(Date.now() / 1000),
    filingTreeChosen: true,
    intelligenceQueries,
    intelligence: [{
      id: 81, document_id: 28, document_title: 'Home insurance renewal notice',
      document_has_thumbnail: false, type: 'date', role: 'expiry',
      value: { date, precision: 'day' }, sort_value: date,
      raw_text: date, evidence_text: `Cover expires on ${date}.`,
      confidence: 0.97, status: 'accepted',
    }],
    savedViews: [{
      id: 4, name: 'Quarterly tax review',
      filter_json: '{"q":"invoice","sensitivity":"confidential"}',
      display: 'list', position: 0, created_at: 0, updated_at: 0,
    }],
  })
  await page.goto('/#/calendar')

  await expect(page.getByRole('heading', { name: 'Calendar' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'Home insurance renewal notice', exact: true })).toBeVisible()
  await expect(page.locator('.agenda-event').getByText('Expiry', { exact: true })).toBeVisible()
  const viewSelect = page.getByLabel('Document view')
  await expect(viewSelect).toContainText('Quarterly tax review')
  await expect(viewSelect).toHaveValue('')
  expect(intelligenceQueries.some(query => !('view_id' in query))).toBe(true)

  const scopedRequest = page.waitForRequest(request => {
    const url = new URL(request.url())
    return url.pathname === '/api/intelligence/' && url.searchParams.get('view_id') === '4'
  })
  await viewSelect.selectOption('4')
  await scopedRequest
  expect(intelligenceQueries.at(-1)).toMatchObject({ view_id: '4', type: 'date', status: 'accepted' })
  expect(intelligenceQueries.at(-1)).not.toHaveProperty('q')
  expect(intelligenceQueries.at(-1)).not.toHaveProperty('sensitivity')
})
