<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "./lib/i18n.svelte.js";
  import { systems, resetSystems, selectSystem, captureScope, scopeCurrent, scopedHash as filingHref } from './lib/systems.svelte.js'
  import { tick, untrack } from 'svelte'
  import { route, go, scrollMemory } from './lib/router.svelte.js'
  import { session, refreshSession, initTheme, setTheme, signOut } from './lib/session.svelte.js'
  import { refreshSystems, enterRoute } from './lib/system-routing.js'
  import { listJDCategories, listDocuments, setupState, stats as fetchStats, getDemoMode, mintDemoSession, chatStatus } from './lib/api-core.js'
  import { hasCapability } from './lib/capabilities.js'
  import Icon from './lib/Icon.svelte'
  import Omnibox from './lib/Omnibox.svelte'
  import Lazy from './lib/Lazy.svelte'
  import BrandMark from './lib/BrandMark.svelte'

  const lazyRoutes = {
    login:       () => import('./routes/Login.svelte'),
    dashboard:   () => import('./routes/Dashboard.svelte'),
    setupReminder: () => import('./lib/SetupReminder.svelte'),
    documents:   () => import('./routes/Documents.svelte'),
    detail:      () => import('./routes/DocumentDetail.svelte'),
    upload:      () => import('./lib/UploadBox.svelte'),
    search:      () => import('./routes/Search.svelte'),
    trash:       () => import('./routes/Trash.svelte'),
    views:       () => import('./routes/Views.svelte'),
    calendar:    () => import('./routes/Calendar.svelte'),
    tasks:       () => import('./routes/Tasks.svelte'),
    automations: () => import('./routes/Automations.svelte'),
    settings:    () => import('./routes/Settings.svelte'),
    account:     () => import('./routes/AccountSettings.svelte'),
    demo:        () => import('./routes/Demo.svelte'),
  }

  let mobileNavOpen = $state(false)
  let sidebarCollapsed = $state(false)
  let uploadFiles = $state([])
  let dragDepth = $state(0)   // window-level drop target (except on #/upload)
  const initials = $derived((session.user?.display_name || session.user?.email || '?')
    .split(/[\s@._-]+/).filter(Boolean).slice(0, 2).map(w => w[0].toUpperCase()).join('') || '?')
  const canShareViews = $derived(hasCapability(session.user, 'share_views'))
  const demoVisitor = $derived(session.user?.kind === 'demo-anon' || session.user?.kind === 'demo-scratch')
  const canUseArchiveChat = $derived(hasCapability(session.user, 'archive_chat') && !demoVisitor)
  const canReviewIntelligence = $derived(hasCapability(session.user, 'archive_intelligence') && !demoVisitor)
  const canViewCalendar = $derived(canReviewIntelligence || demoVisitor)
  let chatStatusInfo = $state({ enabled: false, provider: '', local: false })
  const chatEnabled = $derived(!!chatStatusInfo?.enabled)
  let chatOpen = $state(false)
  let chatParked = $state(false)
  let ChatDrawer = $state(null)
  let chatRequest = $state({ id: 0, question: '' })
  let chatReturnFocus = $state(null)
  let chatRibbon = $state(null)
  let chatOpenedFromRibbon = $state(false)
  let visibleChatScope = $state(null)
  let jdTree = $state([])            // [{lo, name, categories:[…]}]
  let openAreas = $state(new Set())
  let inboxCategory = $state(null)
  let taxonomyLoaded = $state(false)
  let taxonomyError = $state('')
  let inboxCount = $state(0)
  let recentDocs = $state(undefined)
  let recentError = $state('')
  let st = $state(null)                 // /api/stats/ snapshot
  let statsError = $state('')
  // Documents whose reading or model step has not finished. The peak is the
  // size of the current run, so the pill can say how far it has got; it
  // resets once the queue drains. Stats poll often enough to catch a watched
  // folder's run, and faster while one is in progress. Hidden tabs do not
  // poll; returning to the tab refreshes at once.
  const processing = $derived((st?.processing_reading ?? 0) + (st?.processing_classifying ?? 0))
  const processingActive = $derived(processing > 0)
  let processingPeak = $state(0)
  let statsPolling = $state(false)
  let tabVisible = $state(document.visibilityState === 'visible')
  $effect(() => {
    const update = () => {
      tabVisible = document.visibilityState === 'visible'
      if (tabVisible && statsPolling) pollStats()
    }
    document.addEventListener('visibilitychange', update)
    return () => document.removeEventListener('visibilitychange', update)
  })
  $effect(() => {
    if (!statsPolling || !tabVisible) return
    const timer = setInterval(pollStats, processingActive ? 5_000 : 15_000)
    return () => clearInterval(timer)
  })
  const filingTree = $derived(jdTree
    .map((area) => ({ ...area, categories: area.categories.filter((category) => !category.system) }))
    .filter((area) => area.categories.length))
  const hasFilingIndex = $derived(filingTree.length > 0)
  let setupNeeded = $state(false)
  let setupError = $state(false)
  let demoMode = $state(false)
  let demoBannerDismissed = $state(loadDemoDismissed())
  function loadDemoDismissed() {
    try { return sessionStorage.getItem('suchi.demo.bannerDismissed') === '1' } catch { return false }
  }
  function dismissDemoBanner() {
    demoBannerDismissed = true
    try { sessionStorage.setItem('suchi.demo.bannerDismissed', '1') } catch {}
  }
  function openSetupFromReminder() {
    mobileNavOpen = false
  }
  async function refreshSetupState(scope = captureScope()) {
    if (session.user?.role !== 'admin') {
      setupNeeded = false
      setupError = false
      return
    }
    try {
      const state = await setupState()
      if (!scopeCurrent(scope)) return
      setupNeeded = !state?.filing_tree_chosen
      setupError = false
    } catch {
      if (!scopeCurrent(scope)) return
      setupNeeded = true
      setupError = true
    }
  }
  async function handleArchiveTaxonomyChanged() {
    await Promise.all([refreshSetupState(), loadTaxonomy()])
  }
  let toast = $state('')
  let toastTimer
  let accountToast = $state('')
  let accountToastTimer
  let pollTimer

  const scopedNotify = $derived.by(() => {
    const scope = captureScope()
    return message => { if (scopeCurrent(scope)) notify(message) }
  })

  export function notify(msg) {
    toast = msg
    clearTimeout(toastTimer)
    toastTimer = setTimeout(() => (toast = ''), 2400)
  }

  function accountNotify(message) {
    accountToast = message
    clearTimeout(accountToastTimer)
    accountToastTimer = setTimeout(() => (accountToast = ''), 4000)
  }

  function consumeAccountNotice() {
    const url = new URL(location.href)
    if (!url.searchParams.has('account_notice')) return
    const notice = url.searchParams.get('account_notice')
    const messages = {
      identity_bound: translate("app.identityProviderConnected"),
      email_changed: translate("app.signInEmailChanged"),
      email_checked: translate("app.signInEmailIsUpToDate"),
    }
    url.searchParams.delete('account_notice')
    history.replaceState(history.state, '', `${url.pathname}${url.search}${url.hash}`)
    if (messages[notice]) accountNotify(messages[notice])
  }

  async function initializeSession() {
    await refreshSession()
    await tick()
    consumeAccountNotice()
  }

  function loadOpenAreas() {
    try { return new Set(JSON.parse(localStorage.getItem(`suchi.jd.open.${session.user?.user_id}.${systems.code || 'default'}`) || '[]')) } catch { return new Set() }
  }
  function toggleArea(lo) {
    openAreas.has(lo) ? openAreas.delete(lo) : openAreas.add(lo)
    openAreas = new Set(openAreas)
    try { localStorage.setItem(`suchi.jd.open.${session.user?.user_id}.${systems.code || 'default'}`, JSON.stringify([...openAreas])) } catch {}
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
      await initializeSession()
      // Preserve demo deep links; redirect only the first default-route visit.
      if (j?.enabled && session.user?.kind === 'demo-anon' && (!location.hash || location.hash === '#/' || location.hash === '#/dashboard')) {
        try {
          if (sessionStorage.getItem('suchi.demo.landed') !== '1') {
            sessionStorage.setItem('suchi.demo.landed', '1')
            go('#/demo')
          }
        } catch {}
      }
    })
    .catch(async () => {
      await initializeSession()
    })

  async function boot() {
    if (!systems.ready) return
    const scope = captureScope()
    clearInterval(pollTimer)
    const categories = loadTaxonomy()
    // Filing-tree selection is the only setup requirement. Keep its reminder
    // visible across navigation and reloads until the server reports a choice.
    const setup = refreshSetupState(scope)
    const chat = pollChatStatus()
    await Promise.all([pollStats(), categories, setup, chat])
    if (!scopeCurrent(scope)) return
    statsPolling = true
    pollTimer = setInterval(pollChatStatus, 60_000)
  }

  async function loadTaxonomy() {
    const scope = captureScope()
    taxonomyError = ''
    try {
      const cats = await listJDCategories()
      if (!scopeCurrent(scope)) return
      if (!cats?.results) return
      buildTree(cats.results)
      revealPendingInbox()
    } catch (ex) {
      if (scopeCurrent(scope)) taxonomyError = ex.message || translate("app.couldNotLoadTheFilingTree")
    } finally {
      if (scopeCurrent(scope)) taxonomyLoaded = true
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
    const scope = captureScope()
    try {
      const result = await fetchStats()
      if (!scopeCurrent(scope)) return
      st = result
      statsError = ''
      const remaining = (st?.processing_reading ?? 0) + (st?.processing_classifying ?? 0)
      processingPeak = remaining ? Math.max(processingPeak, remaining) : 0
      inboxCount = st?.inbox_count ?? 0
      revealPendingInbox()
    } catch (ex) {
      if (scopeCurrent(scope) && !st) statsError = ex.message || translate("app.couldNotLoadArchiveStatus")
    }
  }

  async function pollChatStatus() {
    const scope = captureScope()
    if (!canUseArchiveChat) {
      chatStatusInfo = { enabled: false, provider: '', local: false }
      return
    }
    try {
      const result = await chatStatus()
      if (!scopeCurrent(scope)) return
      chatStatusInfo = result
    } catch {
      if (!scopeCurrent(scope)) return
      chatStatusInfo = { enabled: false, provider: '', local: false }
    }
  }

  async function loadRecentDocuments({ background = false } = {}) {
    const scope = captureScope()
    if (!background) {
      recentDocs = undefined
      recentError = ''
    }
    try {
      const r = await listDocuments({ page_size: 6, ordering: '-created_at' })
      if (!scopeCurrent(scope)) return
      recentDocs = r?.results || []
    } catch (ex) {
      if (scopeCurrent(scope) && !background) {
        recentDocs = []
        recentError = ex.message || translate("app.couldNotLoadRecentDocuments")
      }
    }
  }

  function globalDrop(e) {
    const handled = e.defaultPrevented
    e.preventDefault()
    dragDepth = 0
    if (handled || page === 'upload' || !session.user || !scopeReady) return
    const files = [...(e.dataTransfer?.files || [])]
    if (files.length) openUpload(files)
  }

  function openUpload(files = []) {
    if (!session.user || !scopeReady) return
    uploadFiles = files
    go(filingHref('#/upload'))
  }

  function onKey(e) {
    if (e.key === 'Escape') mobileNavOpen = false
  }

  function currentChatScope() {
    if (page === 'doc' && documentID) {
      return { label: translate("app.thisDocument"), document_ids: [Number(documentID)] }
    }
    if (visibleChatScope?.path === route.path) return visibleChatScope.scope
    return { label: translate("app.allDocuments") }
  }

  function publishChatScope(scope) {
    visibleChatScope = { path: route.path, scope }
  }

  async function openArchiveChat(question = '', returnFocus = null, scope = currentChatScope()) {
    const requestScope = captureScope()
    chatOpen = true
    chatParked = false
    chatOpenedFromRibbon = false
    chatReturnFocus = returnFocus
    chatRequest = { id: chatRequest.id + 1, question, scope }
    if (!ChatDrawer) {
      try {
        const drawer = (await import('./lib/ArchiveChat.svelte')).default
        if (scopeCurrent(requestScope)) ChatDrawer = drawer
      } catch {
        if (scopeCurrent(requestScope)) { chatOpen = false; chatParked = false; notify(translate("app.couldNotOpenDocumentQuestions")) }
      }
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
      label: translate(ids.length === 1 ? "app.selectedDocumentCount.one" : "app.selectedDocumentCount.other", { count: ids.length }),
      document_ids: ids,
    })
  }

  async function handleSignOut() {
    await signOut()
    clearTimeout(accountToastTimer)
    accountToast = ''
    clearScopedState()
  }

  function clearScopedState() {
    clearInterval(pollTimer)
    statsPolling = false
    clearTimeout(toastTimer)
    jdTree = []
    inboxCategory = null
    taxonomyLoaded = false
    taxonomyError = ''
    inboxCount = 0
    st = null
    statsError = ''
    processingPeak = 0
    recentDocs = undefined
    recentError = ''
    setupNeeded = false
    setupError = false
    mobileNavOpen = false
    uploadFiles = []
    dragDepth = 0
    toast = ''
    chatStatusInfo = { enabled: false, provider: '', local: false }
    chatOpen = false
    chatParked = false
    ChatDrawer = null
    chatRequest = { id: 0, question: '' }
    chatReturnFocus = null
    chatOpenedFromRibbon = false
    visibleChatScope = null
    openAreas = loadOpenAreas()
  }

  $effect(() => {
    const user = session.user
    untrack(() => {
      resetSystems(user)
      clearScopedState()
      if (user) refreshSystems().catch(ex => {
        if (systems.user === user) systems.error = ex.message || translate("app.couldNotLoadFilingSystems")
      })
    })
  })
  $effect(() => {
    const user = session.user
    const loaded = systems.loaded
    const path = route.path
    const query = route.query
    if (user && loaded) untrack(() => { void enterRoute(path, query) })
  })
  $effect(() => {
    const generation = systems.generation
    const ready = systems.ready
    untrack(() => {
      clearScopedState()
      if (ready) boot()
    })
  })
  const scopeReady = $derived(systems.ready && systems.user === session.user &&
    (!systems.introduced || route.query.get('system') === systems.code))
  $effect(() => {
    const path = route.path
    const uploadRoute = route.parts[0] === 'upload'
    untrack(() => {
      mobileNavOpen = false
      if (visibleChatScope?.path !== path) visibleChatScope = null
      if (!uploadRoute) uploadFiles = []
    })
  })
  const page = $derived(route.parts[0] || 'dashboard')
  const documentID = $derived(/^\d+$/.test(route.parts[1] || '') && Number(route.parts[1]) > 0 ? route.parts[1] : '')
  const jdCategories = $derived(jdTree.flatMap((area) => area.categories))
  $effect(() => {
    if (session.user && scopeReady && page === 'dashboard') loadRecentDocuments()
  })
  const nav = $derived([
    { hash: '#/dashboard',   ico: 'gauge',    label: translate("app.dashboard"),   key: 'dashboard' },
    { hash: '#/documents',   ico: 'docs',     label: translate("app.documents"),   key: 'documents' },
    { hash: '#/inbox',       ico: 'inbox',    label: translate("app.inbox"),       key: 'inbox' },
    { hash: '#/tasks',       ico: 'tasks',    label: translate("app.approvals"),   key: 'tasks' },
    { hash: '#/views',       ico: 'eye',      label: translate("app.views"),       key: 'views' },
    ...(canViewCalendar
      ? [{ hash: '#/calendar', ico: 'calendar', label: translate("app.calendar"), key: 'calendar' }]
      : []),
    { hash: '#/automations', ico: 'zap',      label: translate("app.automations"), key: 'automations' },
    { hash: '#/trash',       ico: 'trash',    label: translate("app.trash"),       key: 'trash' },
  ])
  const PAGES = $derived([
    ...nav.map(n => ({ href: n.hash, label: n.label, ico: n.ico })),
    { href: '#/search',   label: translate("app.searchResults"), ico: 'search' },
    { href: '#/settings', label: translate("app.settings"), ico: 'settings' },
  ])
  const pageTitle = $derived(
    nav.find((item) => item.key === page)?.label || ({
      doc: translate("app.document"),
      search: translate("app.searchResults"),
      upload: translate("app.upload"),
      settings: translate("app.settings"),
      demo: translate("app.demo"),
    })[page] || translate("app.pageNotFound2")
  )
  $effect(() => {
    if (!session.user) return
    if (page === 'login') go('#/dashboard')
    if (page === 'settings' && route.query.get('tab') === 'archive' && session.user.role !== 'admin') {
      go('#/settings')
    }
    if (page === 'calendar' && !canViewCalendar) go('#/dashboard')
  })
  const COMMANDS = $derived([
    { label: translate("app.uploadDocuments"),       ico: 'upload', run: openUpload },
    { label: session.theme === 'dark' ? translate("app.switchToLightTheme") : translate("app.switchToDarkTheme"),
      ico: session.theme === 'dark' ? 'sun' : 'moon',
      run: () => setTheme(session.theme === 'dark' ? 'light' : 'dark') },
    { label: translate("app.signOut"), ico: 'out', run: handleSignOut },
  ])
</script>

<svelte:window onkeydown={onKey}
  ondragenter={(e) => { if (scopeReady && e.dataTransfer?.types?.includes('Files') && page !== 'upload') { e.preventDefault(); dragDepth++ } }}
  ondragleave={() => (dragDepth = Math.max(0, dragDepth - 1))}
  ondragover={(e) => { if (dragDepth > 0) e.preventDefault() }}
  ondrop={globalDrop} />

{#if dragDepth > 0}
  <div class="dropveil" aria-hidden="true">
    <div class="dropveil-inner"><b>{translate("app.dropToUpload")}</b><span>{translate("app.suchiWillAddAndProcessEachFile")}</span></div>
  </div>
{/if}

{#if !session.checked}
  <div class="login-wrap"><div class="skel" style="width:220px"></div></div>
{:else if !session.user}
  <Lazy load={lazyRoutes.login} props={{ onSignedIn: () => { go('#/dashboard') } }} />
{:else}
  {#key systems.generation}
  <div class="shell" class:sidebar-collapsed={sidebarCollapsed} inert={chatOpen}>
    {#if mobileNavOpen}
      <button class="mobile-nav-veil" aria-label={translate("app.closeNavigation")} onclick={() => (mobileNavOpen = false)}></button>
    {/if}
    <aside id="primary-navigation" class="sidebar" class:mobile-open={mobileNavOpen}>
      <a class="brand" href={filingHref("#/dashboard")} aria-label={translate("app.suchiHome")}>
        <BrandMark />
        <b>suchi</b>
      </a>

      {#if systems.introduced}
        <div class="side-head">{translate("app.filingSystem")}</div>
        <div style="padding:8px 16px;overflow-wrap:anywhere">
          {#if systems.results.length > 1}
            <label>{translate("app.currentFilingSystem")}
              <select class="input" aria-label={translate("app.currentFilingSystem")} value={route.query.get('system') || ''}
                onchange={event => selectSystem(event.currentTarget.value)}>
                {#if !scopeReady}<option value="">{translate("app.chooseASystem")}</option>{/if}
                {#each systems.results as system (system.code)}
                  <option value={system.code}>{system.code} · {system.name}</option>
                {/each}
              </select>
            </label>
          {:else if scopeReady}
            <b>{systems.code} · {systems.results.find(item => item.code === systems.code)?.name}</b>
          {/if}
        </div>
      {/if}
      {#if setupNeeded && page !== 'dashboard'}
        <Lazy load={lazyRoutes.setupReminder} props={{ error: setupError, onRetry: refreshSetupState, onContinue: openSetupFromReminder }} />
      {/if}

      {#if scopeReady}
      <nav class="nav">
        {#each nav as n}
          <a href={filingHref(n.hash)} class:on={page === n.key} onclick={() => (mobileNavOpen = false)}>
            <Icon name={n.ico} />{n.label}
            {#if n.key === 'inbox' && inboxCount > 0}<span class="badge">{inboxCount}</span>{/if}
            {#if n.key === 'tasks' && ((st?.pending_approvals ?? 0) + (canReviewIntelligence ? (st?.pending_intelligence ?? 0) : 0)) > 0}<span class="badge">{(st?.pending_approvals ?? 0) + (canReviewIntelligence ? (st?.pending_intelligence ?? 0) : 0)}</span>{/if}
          </a>
        {/each}
      </nav>

      {#if hasFilingIndex}
        <div class="side-head">{translate("app.index")}</div>
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
                <a href={filingHref(`#/documents?jd=${c.id}`)} class:on={route.query.get('jd') == c.id} onclick={() => (mobileNavOpen = false)}>
                  <span class="code">{c.code}</span>{c.name}
                  {#if inboxCategory && c.id === inboxCategory.id && inboxCount > 0}<span class="badge">{inboxCount}</span>{/if}
                </a>
              {/each}
            {/if}
          {/each}
        </nav>
      {/if}
      {/if}

      <div class="side-user">
        <a class="side-user-btn" href={filingHref("#/settings")} aria-label={translate("app.profileSettings")} onclick={() => (mobileNavOpen = false)}>
          <span class="avatar sm">
            {#if session.user?.avatar_url}<img src={session.user.avatar_url} alt="" />{:else}{initials}{/if}
          </span>
          <span class="side-user-meta">
            <b>{session.user?.display_name || session.user?.email || translate("app.account")}</b>
            <span>{session.user?.role || 'member'}</span>
          </span>
        </a>
        <button class="side-user-out" onclick={handleSignOut} title={translate("app.signOut")} aria-label={translate("app.signOut")}>
          <Icon name="out" size={14} />
        </button>
      </div>
    </aside>

    <div class="main">
      <div class="topbar">
        <button class="btn desktop-menu" onclick={() => (sidebarCollapsed = !sidebarCollapsed)}
                aria-label={sidebarCollapsed ? translate("app.expandNavigation") : translate("app.collapseNavigation")}
                title={sidebarCollapsed ? translate("app.expandNavigation") : translate("app.collapseNavigation")}
                aria-controls="primary-navigation" aria-expanded={!sidebarCollapsed}>
          <Icon name="menu" size={17} />
        </button>
        <button class="btn mobile-menu" onclick={() => (mobileNavOpen = !mobileNavOpen)}
                aria-label={mobileNavOpen ? translate("app.closeNavigation") : translate("app.openNavigation")} title={translate("app.navigation")}
                aria-controls="primary-navigation" aria-expanded={mobileNavOpen}>
          <Icon name={mobileNavOpen ? 'x' : 'menu'} size={17} />
        </button>
        <h1 aria-label={pageTitle}>
          {pageTitle}
          {#if page !== 'tasks' && scopeReady && systems.introduced}
            <small class="topbar-page-meta">{systems.code} · {systems.results.find(item => item.code === systems.code)?.name}</small>
          {/if}
        </h1>
        {#if scopeReady}<Omnibox pages={session.user?.role === 'admin'
          ? [...PAGES, { href: '#/settings?tab=archive', label: translate("app.archiveConfiguration"), ico: 'settings' }, { href: '#/settings?tab=archive&section=users', label: translate("app.people"), ico: 'shield' }, { href: '#/settings?tab=archive&section=metadata', label: translate("app.metadata"), ico: 'tag' }]
          : PAGES} commands={COMMANDS} canAsk={chatEnabled && canUseArchiveChat} onAsk={openArchiveChat} />{/if}
        {#if chatParked && ChatDrawer}
          <button class="research-ribbon" bind:this={chatRibbon} onclick={resumeArchiveChat}
                  aria-label={translate("app.returnToYourDocumentQuestion")} title={translate("app.returnToYourDocumentQuestion")}>
            <span class="research-ribbon-dot"></span>
            <Icon name="ask" size={14} />
            <span class="research-ribbon-label">{translate("app.questionOpen")}</span>
          </button>
        {/if}
        {#if processingActive}
          <span class="processing-ribbon" role="status"
                aria-label={translate("app.processingProgress", { done: processingPeak - processing, total: processingPeak })}
                title={translate("app.processingDetail", { reading: st.processing_reading ?? 0, classifying: st.processing_classifying ?? 0 })}>
            <span class="processing-ribbon-dot" aria-hidden="true"></span>
            <span class="processing-ribbon-label">{translate("app.processingProgress", { done: processingPeak - processing, total: processingPeak })}</span>
            <span class="processing-ribbon-count" aria-hidden="true">{processing}</span>
          </span>
        {/if}
        <button class="btn primary topbar-upload" disabled={!scopeReady} onclick={openUpload} aria-label={translate("app.uploadDocuments")}>
          <Icon name="upload" size={15} /><span>{translate("app.upload")}</span>
        </button>
        {#if demoMode}
          <a class="btn sm" class:on={page === 'demo'} style="padding:8px 11px" href={filingHref("#/demo")}
             title={translate("app.openDemoGuide")} aria-label={translate("app.openDemoGuide")}>
            <Icon name="help" size={15} />
          </a>
        {/if}
        <button class="btn sm" style="padding:8px 11px" onclick={() => setTheme(session.theme === 'dark' ? 'light' : 'dark')}
          title={translate("app.switchTheme")} aria-label={translate(session.theme === 'dark' ? "app.switchToLightTheme" : "app.switchToDarkTheme")}>
          <Icon name={session.theme === 'dark' ? 'sun' : 'moon'} size={15} />
        </button>
      </div>



      {#if demoMode && !demoBannerDismissed}
        <div class="setup-banner">
          <span><b>{translate("app.youReOnThePublicDemo")}</b> {translate("app.resetsDailyAt0000UtcDon")}</span>
          <a role="button" class="btn sm" href={filingHref("#/demo")}>{translate("app.tour")}</a>
          <button class="btn sm" onclick={dismissDemoBanner}>{translate("app.dismiss")}</button>
        </div>
      {/if}

      <div class="content" use:scrollMemory>
        {#if !scopeReady}
          <div class="empty" role="status">
            {#if systems.error}
              <b>{translate("app.filingSystemUnavailable")}</b><span>{systems.error}</span>
            {:else if systems.loaded && systems.introduced && !systems.results.length}
              <b>{translate("app.accessRequired")}</b><span>{translate("app.anAdministratorMustGrantYouAccessTo")}</span>
            {:else}<span>{translate("app.loadingFilingSystems")}</span>{/if}
            {#if systems.error && systems.results.length === 1}
              <a class="btn" href={`#/dashboard?system=${systems.results[0].code}`}>{translate("app.openValueValue", { "code": systems.results[0].code, "name": systems.results[0].name })}</a>
            {/if}
            <span>{session.user.email}</span>
            <button class="btn" onclick={handleSignOut}>{translate("app.signOut")}</button>
          {#if page !== 'settings'}<a class="btn" href={filingHref('#/settings')}>{translate("app.myAccount")}</a>{/if}
          </div>
          {#if page === 'settings'}
            <Lazy load={lazyRoutes.account} props={{ notify: scopedNotify, accountNotify, profileOnly: true }} />
          {/if}
        {:else if page === 'dashboard'}<Lazy load={lazyRoutes.dashboard} props={{ st, statsError, inboxCategory, taxonomyLoaded, taxonomyError, recent: recentDocs, recentError, onRetryRecent: loadRecentDocuments, setupNeeded, setupError, onRetrySetup: refreshSetupState }} />
        {:else if page === 'documents'}<Lazy load={lazyRoutes.documents} props={{ notify: scopedNotify, jdCategories, canAskArchive: chatEnabled && canUseArchiveChat, canReviewIntelligence, onAskDocuments: askSelectedDocuments, onScopeChange: publishChatScope }} />
        {:else if page === 'doc' && documentID}<Lazy load={lazyRoutes.detail} props={{ id: documentID, notify: scopedNotify, jdCategories }} />
        {:else if page === 'inbox'}<Lazy load={lazyRoutes.documents} props={{ notify: scopedNotify, inbox: inboxCategory, inboxMode: true, taxonomyLoaded, jdCategories, canAskArchive: chatEnabled && canUseArchiveChat, canReviewIntelligence, onAskDocuments: askSelectedDocuments, onScopeChange: publishChatScope }} />
        {:else if page === 'search'}<Lazy load={lazyRoutes.search} props={{ onScopeChange: publishChatScope }} />
        {:else if page === 'tasks'}<Lazy load={lazyRoutes.tasks} props={{ notify: scopedNotify, onCount: pollStats, canReviewIntelligence, canManageJobs: session.user?.role === 'admin' }} />
        {:else if page === 'automations'}<Lazy load={lazyRoutes.automations} props={{ notify: scopedNotify, readOnly: session.user?.role !== 'admin', jdCategories }} />
        {:else if page === 'upload'}
          <div class="upload-wrap">
            <Lazy load={lazyRoutes.upload} props={{ initialFiles: uploadFiles, onInitialFilesConsumed: () => (uploadFiles = []) }} />
          </div>
        {:else if page === 'settings'}<Lazy load={lazyRoutes.settings} props={{ notify: scopedNotify, accountNotify, initialTab: route.query.get('tab'), initialSection: route.query.get('section'), initialPeople: route.query.get('people'), initialMetadata: route.query.get('metadata'), setupNeeded, setupError, onRetrySetup: refreshSetupState, onTaxonomyChanged: handleArchiveTaxonomyChanged }} />
        {:else if page === 'trash'}<Lazy load={lazyRoutes.trash} props={{ notify: scopedNotify }} />
        {:else if page === 'views'}<Lazy load={lazyRoutes.views} props={{ notify: scopedNotify, canShare: canShareViews, startCreate: route.query.get('new') === '1', createQuery: route.query.get('q') || '', createDocumentIDs: route.query.get('ids') || '', jdCategories }} />
        {:else if page === 'calendar'}
          {#key route.query.toString()}
            <Lazy load={lazyRoutes.calendar} props={{ query: route.query, demo: demoVisitor }} />
          {/key}
        {:else if page === 'demo'}<Lazy load={lazyRoutes.demo} props={{ jdCategories }} />
        {:else}<div class="empty"><b>{translate("app.pageNotFound")}</b><span>{translate("app.theAddressDoesNotMatchASuchi")}</span><a href={filingHref("#/dashboard")}>{translate("app.backToTheDashboard")}</a></div>
        {/if}
      </div>
    </div>
  </div>


  {#if ChatDrawer}
    <ChatDrawer open={chatOpen} request={chatRequest} status={chatStatusInfo}
      canReviewIntelligence={canReviewIntelligence} onClose={closeArchiveChat}
      onPark={parkArchiveChat} onReturnFocus={chatReturnFocus} />
  {/if}
  {/key}

{/if}

{#if accountToast}
  <div class="toast" role="status" aria-live="polite" aria-atomic="true">{accountToast}</div>
{:else if toast}
  <div class="toast" role="status" aria-live="polite" aria-atomic="true">{toast}</div>
{/if}
