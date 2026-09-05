<script>
  import { untrack } from 'svelte'
  import { route, go } from './lib/router.svelte.js'
  import { session, refreshSession, initTheme, setTheme, signOut } from './lib/session.svelte.js'
  import { listJDCategories, listDocuments, setupState, stats as fetchStats, uploadDocument, getDemoMode, mintDemoSession, chatStatus } from './lib/api.js'
  import { hasCapability } from './lib/capabilities.js'
  import Icon from './lib/Icon.svelte'
  import Login from './routes/Login.svelte'
  import Dashboard from './routes/Dashboard.svelte'
  import Omnibox from './lib/Omnibox.svelte'
  import Lazy from './lib/Lazy.svelte'
  import BrandMark from './lib/BrandMark.svelte'
  import SetupReminder from './lib/SetupReminder.svelte'

  const documentRoutes = () => import('./lib/documentRoutes.js')
  const searchRoute = () => import('./lib/searchRoute.js')
  const organizeRoutes = () => import('./lib/organizeRoutes.js')
  const workflowRoutes = () => import('./lib/workflowRoutes.js')
  const configurationRoutes = () => import('./lib/configurationRoutes.js')
  const demoRoute = () => import('./lib/demoRoute.js')
  const bundled = (load, name) => () => load().then(m => ({ default: m[name] }))
  const lazyRoutes = {
    documents:   bundled(documentRoutes, 'Documents'),
    detail:      bundled(documentRoutes, 'DocumentDetail'),
    upload:      bundled(documentRoutes, 'Upload'),
    uploadBox:   bundled(documentRoutes, 'UploadBox'),
    search:      bundled(searchRoute, 'Search'),
    trash:       bundled(organizeRoutes, 'Trash'),
    views:       bundled(organizeRoutes, 'Views'),
    calendar:    bundled(organizeRoutes, 'Calendar'),
    tasks:       bundled(workflowRoutes, 'Tasks'),
    automations: bundled(workflowRoutes, 'Automations'),
    settings:    bundled(configurationRoutes, 'Settings'),
    setup:       bundled(configurationRoutes, 'Setup'),
    demo:        bundled(demoRoute, 'Demo'),
  }

  let mobileNavOpen = $state(false)
  let sidebarCollapsed = $state(false)
  let uploadOpen = $state(false)
  let uploadDialog = $state(null)
  let uploadReturnFocus = null
  let dragDepth = $state(0)   // window-level drop target (except on #/upload)
  const initials = $derived((session.user?.display_name || session.user?.email || '?')
    .split(/[\s@._-]+/).filter(Boolean).slice(0, 2).map(w => w[0].toUpperCase()).join('') || '?')
  const canShareViews = $derived(hasCapability(session.user, 'share_views'))
  const canUseArchiveChat = $derived(hasCapability(session.user, 'archive_chat') && session.user?.demo !== 'anon' && session.user?.demo !== 'scratch')
  const canReviewIntelligence = $derived(hasCapability(session.user, 'archive_intelligence') && session.user?.demo !== 'anon' && session.user?.demo !== 'scratch')
  let chatEnabled = $state(false)
  let chatStatusInfo = $state({ enabled: false, provider: '', local: false })
  let chatOpen = $state(false)
  let chatParked = $state(false)
  let ChatDrawer = $state(null)
  let chatRequest = $state({ id: 0, question: '' })
  let chatReturnFocus = $state(null)
  let chatRibbon = $state(null)
  let chatOpenedFromRibbon = $state(false)
  let visibleChatScope = $state(null)
  let jdTree = $state([])            // [{lo, name, categories:[…]}]
  let openAreas = $state(loadOpenAreas())
  let inboxCategory = $state(null)
  let taxonomyLoaded = $state(false)
  let taxonomyError = $state('')
  let inboxCount = $state(0)
  let recentDocs = $state(undefined)
  let recentError = $state('')
  let st = $state(null)                 // /api/stats/ snapshot
  let statsError = $state('')
  const filingTree = $derived(jdTree
    .map((area) => ({ ...area, categories: area.categories.filter((category) => !category.system) }))
    .filter((area) => area.categories.length))
  const hasFilingIndex = $derived(filingTree.length > 0)
  let setupNeeded = $state(false)
  let setupEngaged = $state(false)
  let setupReminderKey = ''
  const setupReminderSeconds = 48 * 60 * 60
  let demoMode = $state(false)
  let demoBannerDismissed = $state(loadDemoDismissed())
  function loadDemoDismissed() {
    try { return sessionStorage.getItem('suchi.demo.bannerDismissed') === '1' } catch { return false }
  }
  function dismissDemoBanner() {
    demoBannerDismissed = true
    try { sessionStorage.setItem('suchi.demo.bannerDismissed', '1') } catch {}
  }
  function setupDismissalKey(startedAt) {
    const userID = Number(session.user?.user_id || 0)
    return userID && startedAt ? `suchi.setup.reminder.dismissed.${userID}.${startedAt}` : ''
  }
  function setupReminderWasDismissed(key) {
    if (!key) return false
    try { return localStorage.getItem(key) === '1' } catch { return false }
  }
  function acknowledgeSetupReminder() {
    setupNeeded = false
    setupEngaged = true
    try { if (setupReminderKey) localStorage.setItem(setupReminderKey, '1') } catch {}
  }
  function dismissSetupReminder() {
    acknowledgeSetupReminder()
    notify('Setup reminder closed. Setup is always available in Settings.')
  }
  function openSetupFromReminder() {
    acknowledgeSetupReminder()
    mobileNavOpen = false
  }
  function handleSetupTaxonomyChanged() {
    acknowledgeSetupReminder()
    return loadTaxonomy()
  }
  let toast = $state('')
  let toastTimer
  let pollTimer

  export function notify(msg) {
    toast = msg
    clearTimeout(toastTimer)
    toastTimer = setTimeout(() => (toast = ''), 2400)
  }

  function loadOpenAreas() {
    try { return new Set(JSON.parse(localStorage.getItem('suchi.jd.open') || '[]')) } catch { return new Set() }
  }
  function toggleArea(lo) {
    openAreas.has(lo) ? openAreas.delete(lo) : openAreas.add(lo)
    openAreas = new Set(openAreas)
    try { localStorage.setItem('suchi.jd.open', JSON.stringify([...openAreas])) } catch {}
  }

  initTheme()

  // Demo credentials are cookies too; an existing scratch session takes
  // precedence over the stateless read cookie minted on each visit.
  getDemoMode()
    .then(async j => {
      if (j?.enabled) demoMode = true
      if (j?.enabled) {
        try { await mintDemoSession() } catch {}
      }
      await refreshSession()
      if (session.user) boot()
      // Preserve demo deep links; redirect only the first default-route visit.
      if (j?.enabled && session.user?.demo === 'anon' && (!location.hash || location.hash === '#/' || location.hash === '#/dashboard')) {
        try {
          if (sessionStorage.getItem('suchi.demo.landed') !== '1') {
            sessionStorage.setItem('suchi.demo.landed', '1')
            go('#/demo')
          }
        } catch {}
      }
    })
    .catch(async () => {
      await refreshSession()
      if (session.user) boot()
    })

  async function boot() {
    const user = session.user
    clearInterval(pollTimer)
    const categories = loadTaxonomy()
    // Keep a fresh-install reminder for 48 hours, until the admin opens it,
    // dismisses it, or chooses a filing tree. Server time prevents an old
    // browser-local acknowledgement leaking into a new installation.
    const setup = session.user?.role === 'admin'
      ? setupState().then(state => {
          if (session.user !== user) return
          const startedAt = Number(state?.started_at || 0)
          const withinWindow = !startedAt || Math.floor(Date.now() / 1000) < startedAt + setupReminderSeconds
          setupReminderKey = setupDismissalKey(startedAt)
          setupNeeded = !state?.completed_at && !state?.filing_tree_chosen && withinWindow && !setupReminderWasDismissed(setupReminderKey)
          setupEngaged = !setupNeeded
        }).catch(() => {})
      : Promise.resolve()
    const chat = pollChatStatus()
    await Promise.all([pollStats(), categories, setup, chat])
    if (session.user !== user) return
    pollTimer = setInterval(() => { pollStats(); pollChatStatus() }, 60_000)
  }

  async function loadTaxonomy() {
    const user = session.user
    taxonomyError = ''
    try {
      const cats = await listJDCategories()
      if (session.user !== user) return
      if (!cats?.results) return
      buildTree(cats.results)
      revealPendingInbox()
    } catch (ex) {
      if (session.user === user) taxonomyError = ex.message || 'Could not load the filing tree.'
    } finally {
      if (session.user === user) taxonomyLoaded = true
    }
  }

  function buildTree(cats) {
    const areas = new Map()
    inboxCategory = null
    for (const c of cats) {
      const lo = Number(c.area_code)
      if (!areas.has(lo)) areas.set(lo, { lo, name: c.area_name, categories: [] })
      areas.get(lo).categories.push(c)
      if (c.system || (st?.inbox_category_id ? c.id === st.inbox_category_id : /inbox/i.test(c.name))) inboxCategory = c
    }
    jdTree = [...areas.values()].sort((a, b) => a.lo - b.lo)
  }

  function revealPendingInbox() {
    if (inboxCount > 0 && inboxCategory) {
      const lo = Number(inboxCategory.area_code)
      if (!openAreas.has(lo)) toggleArea(lo)
    }
  }

  async function pollStats() {
    const user = session.user
    try {
      const result = await fetchStats()
      if (session.user !== user) return
      st = result
      statsError = ''
      inboxCount = st?.inbox_count ?? 0
      revealPendingInbox()
    } catch (ex) {
      if (session.user === user && !st) statsError = ex.message || 'Could not load archive status.'
    }
  }

  async function pollChatStatus() {
    const user = session.user
    if (!canUseArchiveChat) {
      chatEnabled = false
      chatStatusInfo = { enabled: false, provider: '', local: false }
      return
    }
    try {
      const result = await chatStatus()
      if (session.user !== user) return
      chatStatusInfo = result
      chatEnabled = !!chatStatusInfo?.enabled
    } catch {
      if (session.user !== user) return
      chatEnabled = false
      chatStatusInfo = { enabled: false, provider: '', local: false }
    }
  }

  async function loadRecentDocuments({ background = false } = {}) {
    const user = session.user
    if (!background) {
      recentDocs = undefined
      recentError = ''
    }
    try {
      const r = await listDocuments({ page_size: 6, ordering: '-created_at' })
      if (session.user !== user) return
      recentDocs = r?.results || []
    } catch (ex) {
      if (session.user === user && !background) {
        recentDocs = []
        recentError = ex.message || 'Could not load recent documents.'
      }
    }
  }

  function refreshVisibleData() {
    pollStats()
    if (page === 'dashboard') loadRecentDocuments({ background: true })
  }

  async function globalDrop(e) {
    const user = session.user
    e.preventDefault()
    dragDepth = 0
    if (page === 'upload' || !session.user) return   // upload page has its own zone
    const files = [...(e.dataTransfer?.files || [])]
    if (!files.length) return
    notify(`Uploading ${files.length} file${files.length === 1 ? '' : 's'}…`)
    let ok = 0, dup = 0, fail = 0
    for (const f of files) {
      if (session.user !== user) return
      try {
        const result = await uploadDocument(f)
        result?.deduplicated ? dup++ : ok++
      }
      catch { fail++ }
    }
    if (session.user !== user) return
    notify([ok && `${ok} uploaded`, dup && `${dup} duplicate${dup === 1 ? '' : 's'}`, fail && `${fail} failed`].filter(Boolean).join(' · '))
    refreshVisibleData()
  }

  function openUpload() {
    uploadReturnFocus = document.activeElement
    uploadOpen = true
    queueMicrotask(() => uploadDialog?.focus())
  }

  function closeUpload({ refresh = false } = {}) {
    if (!uploadOpen) return
    uploadOpen = false
    if (refresh) refreshVisibleData()
    queueMicrotask(() => uploadReturnFocus?.focus?.())
  }

  function onUploadKey(e) {
    if (e.key === 'Escape') {
      e.preventDefault()
      e.stopPropagation()
      closeUpload({ refresh: true })
      return
    }
    if (e.key !== 'Tab' || !uploadDialog) return
    const focusable = [...uploadDialog.querySelectorAll('button:not([disabled]), a[href], input:not([disabled]):not([type="hidden"]):not([hidden]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])')]
    if (!focusable.length) {
      e.preventDefault()
      uploadDialog.focus()
      return
    }
    const first = focusable[0]
    const last = focusable[focusable.length - 1]
    if (e.shiftKey && (document.activeElement === first || document.activeElement === uploadDialog)) {
      e.preventDefault()
      last.focus()
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault()
      first.focus()
    }
  }

  function onKey(e) {
    if (e.key === 'Escape') mobileNavOpen = false
  }

  function currentChatScope() {
    if (page === 'doc' && documentID) {
      return { label: 'Current document', document_ids: [Number(documentID)] }
    }
    if (visibleChatScope?.path === route.path) return visibleChatScope.scope
    return { label: 'All archive' }
  }

  function publishChatScope(scope) {
    visibleChatScope = { path: route.path, scope }
  }

  async function openArchiveChat(question = '', returnFocus = null, scope = currentChatScope()) {
    chatOpen = true
    chatParked = false
    chatOpenedFromRibbon = false
    chatReturnFocus = returnFocus
    chatRequest = { id: chatRequest.id + 1, question, scope }
    if (!ChatDrawer) {
      try { ChatDrawer = (await import('./lib/ArchiveChat.svelte')).default }
      catch { chatOpen = false; chatParked = false; notify('Could not open archive research') }
    }
  }

  function closeArchiveChat() {
    chatOpen = false
    chatParked = chatOpenedFromRibbon
    chatOpenedFromRibbon = false
  }

  function parkArchiveChat() {
    chatOpen = false
    chatParked = true
    chatOpenedFromRibbon = false
  }

  function resumeArchiveChat() {
    chatOpen = true
    chatParked = false
    chatOpenedFromRibbon = true
    chatReturnFocus = () => chatRibbon?.focus()
  }

  function askSelectedDocuments(ids) {
    openArchiveChat('', null, {
      label: `${ids.length} selected document${ids.length === 1 ? '' : 's'}`,
      document_ids: ids,
    })
  }

  async function handleSignOut() {
    await signOut()
    clearInterval(pollTimer)
    clearTimeout(toastTimer)
    jdTree = []
    inboxCategory = null
    taxonomyLoaded = false
    taxonomyError = ''
    inboxCount = 0
    st = null
    statsError = ''
    recentDocs = undefined
    recentError = ''
    setupNeeded = false
    setupEngaged = false
    setupReminderKey = ''
    mobileNavOpen = false
    uploadOpen = false
    dragDepth = 0
    toast = ''
    chatEnabled = false
    chatStatusInfo = { enabled: false, provider: '', local: false }
    chatOpen = false
    chatParked = false
    ChatDrawer = null
    chatRequest = { id: 0, question: '' }
    chatReturnFocus = null
    chatOpenedFromRibbon = false
    visibleChatScope = null
  }

  $effect(() => {
    route.path
    untrack(() => { mobileNavOpen = false; closeUpload(); visibleChatScope = null })
  })
  const page = $derived(route.parts[0] || 'dashboard')
  const documentID = $derived(/^\d+$/.test(route.parts[1] || '') && Number(route.parts[1]) > 0 ? route.parts[1] : '')
  const jdCategories = $derived(jdTree.flatMap((area) => area.categories))
  $effect(() => {
    if (session.user && page === 'dashboard') loadRecentDocuments()
  })
  const nav = $derived([
    { hash: '#/dashboard',   ico: 'gauge',    label: 'Dashboard',   key: 'dashboard' },
    { hash: '#/documents',   ico: 'docs',     label: 'Documents',   key: 'documents' },
    { hash: '#/inbox',       ico: 'inbox',    label: 'Inbox',       key: 'inbox' },
    { hash: '#/views',       ico: 'eye',      label: 'Views',       key: 'views' },
    ...(canReviewIntelligence
      ? [{ hash: '#/calendar', ico: 'calendar', label: 'Calendar', key: 'calendar' }]
      : []),
    { hash: '#/tasks',       ico: 'tasks',    label: 'Approvals',   key: 'tasks' },
    { hash: '#/automations', ico: 'zap',      label: 'Automations', key: 'automations' },
    { hash: '#/trash',       ico: 'trash',    label: 'Trash',       key: 'trash' },
  ])
  const PAGES = $derived([
    ...nav.map(n => ({ href: n.hash, label: n.label, ico: n.ico })),
    { href: '#/search',   label: 'Search results', ico: 'search' },
    { href: '#/settings', label: 'Settings', ico: 'settings' },
  ])
  const pageTitle = $derived(
    nav.find((item) => item.key === page)?.label || ({
      doc: 'Document',
      search: 'Search results',
      upload: 'Upload',
      settings: 'Settings',
      demo: 'Demo',
      setup: 'Setup',
    })[page] || 'Page not found'
  )
  $effect(() => {
    if (!session.user) return
    if (page === 'login') go('#/dashboard')
    if (page === 'settings' && route.query.get('tab') === 'archive' && session.user.role !== 'admin') {
      go('#/settings')
    }
    if (page === 'calendar' && !canReviewIntelligence) go('#/dashboard')
  })
  const COMMANDS = $derived([
    { label: 'Upload documents',       ico: 'upload', run: openUpload },
    { label: session.theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme',
      ico: session.theme === 'dark' ? 'sun' : 'moon',
      run: () => setTheme(session.theme === 'dark' ? 'light' : 'dark') },
    { label: 'Sign out', ico: 'out', run: handleSignOut },
  ])
</script>

<svelte:window onkeydown={onKey}
  ondragenter={(e) => { if (e.dataTransfer?.types?.includes('Files') && page !== 'upload') { e.preventDefault(); dragDepth++ } }}
  ondragleave={() => (dragDepth = Math.max(0, dragDepth - 1))}
  ondragover={(e) => { if (dragDepth > 0) e.preventDefault() }}
  ondrop={globalDrop} />

{#if dragDepth > 0}
  <div class="dropveil" aria-hidden="true">
    <div class="dropveil-inner"><b>Drop to upload</b><span>anywhere works — the pipeline takes it from here</span></div>
  </div>
{/if}

{#if !session.checked}
  <div class="login-wrap"><div class="skel" style="width:220px"></div></div>
{:else if !session.user}
  <Login onSignedIn={() => { boot(); go('#/dashboard') }} />
{:else}
  <div class="shell" class:sidebar-collapsed={sidebarCollapsed} inert={chatOpen || uploadOpen}>
    {#if mobileNavOpen}
      <button class="mobile-nav-veil" aria-label="Close navigation" onclick={() => (mobileNavOpen = false)}></button>
    {/if}
    <aside id="primary-navigation" class="sidebar" class:mobile-open={mobileNavOpen}>
      <a class="brand" href="#/dashboard" aria-label="suchi home">
        <BrandMark />
        <b>suchi</b>
      </a>

      {#if setupNeeded}
        <SetupReminder onClose={dismissSetupReminder} onContinue={openSetupFromReminder} />
      {/if}

      <nav class="nav">
        {#each nav as n}
          <a href={n.hash} class:on={page === n.key} onclick={() => (mobileNavOpen = false)}>
            <Icon name={n.ico} />{n.label}
            {#if n.key === 'inbox' && inboxCount > 0}<span class="badge">{inboxCount}</span>{/if}
            {#if n.key === 'tasks' && ((st?.pending_approvals ?? 0) + (canReviewIntelligence ? (st?.pending_intelligence ?? 0) : 0)) > 0}<span class="badge">{(st?.pending_approvals ?? 0) + (canReviewIntelligence ? (st?.pending_intelligence ?? 0) : 0)}</span>{/if}
          </a>
        {/each}
      </nav>

      {#if hasFilingIndex}
        <div class="side-head">Index</div>
        <nav class="jd-tree">
          {#each filingTree as area (area.lo)}
            {@const areaPending = inboxCategory && Number(inboxCategory.area_code) === area.lo ? inboxCount : 0}
            <button class="area-toggle" onclick={() => toggleArea(area.lo)}
                    aria-expanded={openAreas.has(area.lo)}>
              <span class="chev" class:open={openAreas.has(area.lo)}><Icon name="chev" size={12} /></span>
              <span class="code">{area.lo}–{area.lo + 9}</span>
              <span class="area">{area.name}</span>
              {#if areaPending > 0 && !openAreas.has(area.lo)}<span class="badge">{areaPending}</span>{/if}
            </button>
            {#if openAreas.has(area.lo)}
              {#each area.categories as c (c.id)}
                <a href={`#/documents?jd=${c.id}`} class:on={route.query.get('jd') == c.id} onclick={() => (mobileNavOpen = false)}>
                  <span class="code">{c.code}</span>{c.name}
                  {#if inboxCategory && c.id === inboxCategory.id && inboxCount > 0}<span class="badge">{inboxCount}</span>{/if}
                </a>
              {/each}
            {/if}
          {/each}
        </nav>
      {/if}

      <div class="side-user">
        <a class="side-user-btn" href="#/settings" aria-label="Profile & settings" onclick={() => (mobileNavOpen = false)}>
          <span class="avatar sm">
            {#if session.user?.avatar_url}<img src={session.user.avatar_url} alt="" />{:else}{initials}{/if}
          </span>
          <span class="side-user-meta">
            <b>{session.user?.display_name || session.user?.email || 'Account'}</b>
            <span>{session.user?.role || 'member'}</span>
          </span>
        </a>
        <button class="side-user-out" onclick={handleSignOut} title="Sign out" aria-label="Sign out">
          <Icon name="out" size={14} />
        </button>
      </div>
    </aside>

    <div class="main">
      <div class="topbar">
        <button class="btn desktop-menu" onclick={() => (sidebarCollapsed = !sidebarCollapsed)}
                aria-label={sidebarCollapsed ? 'Expand navigation' : 'Collapse navigation'}
                title={sidebarCollapsed ? 'Expand navigation' : 'Collapse navigation'}
                aria-controls="primary-navigation" aria-expanded={!sidebarCollapsed}>
          <Icon name="menu" size={17} />
        </button>
        <button class="btn mobile-menu" onclick={() => (mobileNavOpen = !mobileNavOpen)}
                aria-label={mobileNavOpen ? 'Close navigation' : 'Open navigation'} title="Navigation"
                aria-controls="primary-navigation" aria-expanded={mobileNavOpen}>
          <Icon name={mobileNavOpen ? 'x' : 'menu'} size={17} />
        </button>
        <h1>{pageTitle}</h1>
        <Omnibox pages={session.user?.role === 'admin'
          ? [...PAGES, { href: '#/settings?tab=archive', label: 'Archive configuration', ico: 'settings' }, { href: '#/settings?tab=archive&section=users', label: 'People and metadata', ico: 'shield' }]
          : PAGES} commands={COMMANDS} canAsk={chatEnabled && canUseArchiveChat} onAsk={openArchiveChat} />
        {#if chatParked && ChatDrawer}
          <button class="research-ribbon" bind:this={chatRibbon} onclick={resumeArchiveChat}
                  aria-label="Return to archive research" title="Return to archive research">
            <span class="research-ribbon-dot"></span>
            <Icon name="ask" size={14} />
            <span class="research-ribbon-label">Research active</span>
          </button>
        {/if}
        <button class="btn primary topbar-upload" onclick={openUpload} aria-label="Upload documents">
          <Icon name="upload" size={15} /><span>Upload</span>
        </button>
        {#if demoMode}
          <a class="btn sm" class:on={page === 'demo'} style="padding:8px 11px" href="#/demo"
             title="Open demo guide" aria-label="Open demo guide">
            <Icon name="help" size={15} />
          </a>
        {/if}
        <button class="btn sm" style="padding:8px 11px" onclick={() => setTheme(session.theme === 'dark' ? 'light' : 'dark')}
                title="Switch theme" aria-label={`Use ${session.theme === 'dark' ? 'light' : 'dark'} theme`}>
          <Icon name={session.theme === 'dark' ? 'sun' : 'moon'} size={15} />
        </button>
      </div>

      {#if setupNeeded && page === 'dashboard'}
        <SetupReminder placement="mobile" onClose={dismissSetupReminder} onContinue={openSetupFromReminder} />
      {/if}

      {#if demoMode && !demoBannerDismissed}
        <div class="setup-banner">
          <span><b>You're on the public demo.</b> Resets daily at 00:00 UTC — don't upload confidential documents.</span>
          <a role="button" class="btn sm" href="#/demo">Tour</a>
          <button class="btn sm" onclick={dismissDemoBanner}>Dismiss</button>
        </div>
      {/if}

      <div class="content">
        {#if page === 'dashboard'}<Dashboard {st} {statsError} {inboxCategory} {taxonomyLoaded} {taxonomyError} recent={recentDocs} {recentError} onRetryRecent={loadRecentDocuments} />
        {:else if page === 'documents'}<Lazy load={lazyRoutes.documents} props={{ notify, jdCategories, canAskArchive: chatEnabled && canUseArchiveChat, canReviewIntelligence, onAskDocuments: askSelectedDocuments, onScopeChange: publishChatScope }} />
        {:else if page === 'doc' && documentID}<Lazy load={lazyRoutes.detail} props={{ id: documentID, notify, jdCategories }} />
        {:else if page === 'inbox'}<Lazy load={lazyRoutes.documents} props={{ notify, inbox: inboxCategory, inboxMode: true, taxonomyLoaded, jdCategories, canAskArchive: chatEnabled && canUseArchiveChat, canReviewIntelligence, onAskDocuments: askSelectedDocuments, onScopeChange: publishChatScope }} />
        {:else if page === 'search'}<Lazy load={lazyRoutes.search} props={{ onScopeChange: publishChatScope }} />
        {:else if page === 'tasks'}<Lazy load={lazyRoutes.tasks} props={{ notify, onCount: pollStats, canReviewIntelligence }} />
        {:else if page === 'automations'}<Lazy load={lazyRoutes.automations} props={{ notify, readOnly: session.user?.role !== 'admin', jdCategories }} />
        {:else if page === 'upload'}<Lazy load={lazyRoutes.upload} props={{ notify, jdCategories }} />
        {:else if page === 'settings'}<Lazy load={lazyRoutes.settings} props={{ notify, initialTab: route.query.get('tab'), initialSection: route.query.get('section'), onTaxonomyChanged: loadTaxonomy, setupEngaged, onSetupEngaged: acknowledgeSetupReminder }} />
        {:else if page === 'trash'}<Lazy load={lazyRoutes.trash} props={{ notify }} />
        {:else if page === 'views'}<Lazy load={lazyRoutes.views} props={{ notify, canShare: canShareViews, startCreate: route.query.get('new') === '1', createQuery: route.query.get('q') || '', createDocumentIDs: route.query.get('ids') || '', jdCategories }} />
        {:else if page === 'calendar'}<Lazy load={lazyRoutes.calendar} props={{ initialDocumentIDs: route.query.get('document_ids') || '' }} />
        {:else if page === 'demo'}<Lazy load={lazyRoutes.demo} props={{ jdCategories }} />
        {:else if page === 'setup' && session.user?.role === 'admin'}<Lazy load={lazyRoutes.setup} props={{ notify, onTaxonomyChanged: handleSetupTaxonomyChanged, onDone: () => { acknowledgeSetupReminder(); go('#/dashboard') } }} />
        {:else}<div class="empty"><b>Page not found.</b><span>The address does not match a Suchi screen.</span><a href="#/dashboard">Back to the dashboard</a></div>
        {/if}
      </div>
    </div>
  </div>

  {#if uploadOpen}
    <div class="modal-veil" onclick={() => closeUpload({ refresh: true })} role="presentation">
      <!-- svelte-ignore a11y_click_events_have_key_events -->
      <div class="modal" bind:this={uploadDialog} onclick={(e) => e.stopPropagation()} onkeydown={onUploadKey}
           role="dialog" aria-modal="true" aria-label="Upload documents" tabindex="-1">
        <div class="modal-head">
          <h3>Upload</h3>
          <button class="btn sm" onclick={() => closeUpload({ refresh: true })}
                  title="Close upload" aria-label="Close upload"><Icon name="x" size={13} /></button>
        </div>
        <Lazy load={lazyRoutes.uploadBox} props={{ notify, jdCategories }} />
      </div>
    </div>
  {/if}

  {#if ChatDrawer}
    <ChatDrawer open={chatOpen} request={chatRequest} status={chatStatusInfo}
      canReviewIntelligence={canReviewIntelligence} onClose={closeArchiveChat}
      onPark={parkArchiveChat} onReturnFocus={chatReturnFocus} />
  {/if}

{/if}

{#if toast}<div class="toast">{toast}</div>{/if}
