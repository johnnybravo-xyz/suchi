<script>
  import { route, go } from './lib/router.svelte.js'
  import { session, refreshSession, initTheme, setTheme, signOut } from './lib/session.svelte.js'
  import { listJDCategories, listTasks, listDocuments, setupState } from './lib/api.js'
  import Icon from './lib/Icon.svelte'
  import Palette from './lib/Palette.svelte'
  import Login from './routes/Login.svelte'
  import Dashboard from './routes/Dashboard.svelte'
  import Documents from './routes/Documents.svelte'
  import DocumentDetail from './routes/DocumentDetail.svelte'
  import SearchPage from './routes/Search.svelte'
  import Tasks from './routes/Tasks.svelte'
  import Automations from './routes/Automations.svelte'
  import Upload from './routes/Upload.svelte'
  import Settings from './routes/Settings.svelte'
  import Setup from './routes/Setup.svelte'

  let paletteOpen = $state(false)
  let drawerOpen = $state(false)
  let jdTree = $state([])            // [{lo, name, categories:[…]}]
  let openAreas = $state(loadOpenAreas())
  let inboxCategory = $state(null)
  let inboxCount = $state(0)
  let pendingTasks = $state([])
  let deadJobs = $state([])
  let recentDocs = $state([])
  let setupNeeded = $state(false)
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
  refreshSession().then(() => { if (session.user) boot() })

  async function boot() {
    try {
      const cats = await listJDCategories()
      if (cats?.results) buildTree(cats.results)
    } catch {}
    pollActivity()
    clearInterval(pollTimer)
    pollTimer = setInterval(pollActivity, 60_000)
    // Setup wizard: propose, never block. Admin-only endpoint; anyone
    // else 403s and we stay quiet. "Later" is remembered locally.
    if (session.user?.role === 'admin') {
      try {
        const st = await setupState()
        const dismissed = (() => { try { return localStorage.getItem('suchi.setup.dismissed') === '1' } catch { return false } })()
        setupNeeded = !st?.completed_at && !dismissed
      } catch {}
    }
  }

  function buildTree(cats) {
    const areas = new Map()
    for (const c of cats) {
      const lo = Number(c.area_code)
      if (!areas.has(lo)) areas.set(lo, { lo, name: c.area_name, categories: [] })
      areas.get(lo).categories.push(c)
      if (/inbox/i.test(c.name)) inboxCategory = c
    }
    jdTree = [...areas.values()].sort((a, b) => a.lo - b.lo)
  }

  async function pollActivity() {
    try {
      const [wf, jb] = await Promise.all([
        listTasks({ include: 'workflow', state: 'pending', limit: 50 }),
        listTasks({ include: 'jobs', state: 'dead', limit: 50 }),
      ])
      pendingTasks = wf?.results || wf || []
      deadJobs = jb?.results || jb || []
    } catch {}
    try {
      if (inboxCategory) {
        const r = await listDocuments({ jd_category_id: inboxCategory.id, page_size: 1 })
        inboxCount = r?.count ?? 0
        // pending work in a collapsed drawer should be visible: open that area once
        if (inboxCount > 0) {
          const lo = Number(inboxCategory.area_code)
          if (!openAreas.has(lo)) toggleArea(lo)
        }
      }
    } catch {}
    try {
      const r = await listDocuments({ page_size: 6, ordering: '-created_at' })
      recentDocs = r?.results || []
    } catch {}
  }

  function dismissSetup() {
    setupNeeded = false
    try { localStorage.setItem('suchi.setup.dismissed', '1') } catch {}
  }

  function onKey(e) {
    if ((e.metaKey || e.ctrlKey) && e.key === 'k') { e.preventDefault(); paletteOpen = !paletteOpen }
    if (e.key === 'Escape') { paletteOpen = false; drawerOpen = false }
  }

  const page = $derived(route.parts[0] || 'dashboard')
  const bellCount = $derived(pendingTasks.length + deadJobs.length)
  const nav = [
    { hash: '#/dashboard',   ico: 'gauge',  label: 'Dashboard',   key: 'dashboard' },
    { hash: '#/documents',   ico: 'docs',   label: 'Documents',   key: 'documents' },
    { hash: '#/inbox',       ico: 'inbox',  label: 'Inbox',       key: 'inbox' },
    { hash: '#/search',      ico: 'search', label: 'Search',      key: 'search' },
    { hash: '#/tasks',       ico: 'tasks',  label: 'Approvals',   key: 'tasks' },
    { hash: '#/automations', ico: 'zap',    label: 'Automations', key: 'automations' },
  ]
</script>

<svelte:window onkeydown={onKey} />

{#if !session.checked}
  <div class="login-wrap"><div class="skel" style="width:220px"></div></div>
{:else if !session.user}
  <Login onSignedIn={() => { boot(); go('#/dashboard') }} />
{:else}
  <div class="shell">
    <aside class="sidebar">
      <a class="brand" href="#/dashboard" aria-label="suchi home">
        <svg viewBox="0 0 64 64" aria-hidden="true"><rect x="8" y="8" width="48" height="48" rx="8" fill="var(--manila)" stroke="currentColor" stroke-width="3.5"/><circle cx="17.5" cy="19" r="2.2" fill="currentColor"/><line x1="23" y1="19" x2="48" y2="19" stroke="currentColor" stroke-width="3.5" stroke-linecap="round"/><circle cx="17.5" cy="28" r="2.2" fill="currentColor"/><line x1="23" y1="28" x2="48" y2="28" stroke="currentColor" stroke-width="3.5" stroke-linecap="round"/><circle cx="16.8" cy="37.25" r="3.2" fill="var(--accent)"/><rect x="22.5" y="33.5" width="29.5" height="7.5" rx="3.75" fill="var(--accent)"/><circle cx="17.5" cy="46" r="2.2" fill="currentColor"/><line x1="23" y1="46" x2="48" y2="46" stroke="currentColor" stroke-width="3.5" stroke-linecap="round"/></svg>
        <b>suchi</b>
      </a>

      <nav class="nav">
        {#each nav as n}
          <a href={n.hash} class:on={page === n.key}>
            <Icon name={n.ico} />{n.label}
            {#if n.key === 'inbox' && inboxCount > 0}<span class="badge">{inboxCount}</span>{/if}
            {#if n.key === 'tasks' && pendingTasks.length > 0}<span class="badge">{pendingTasks.length}</span>{/if}
          </a>
        {/each}
      </nav>

      {#if jdTree.length}
        <div class="side-head">Index</div>
        <nav class="jd-tree">
          {#each jdTree as area (area.lo)}
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
                <a href={`#/documents?jd=${c.id}`} class:on={route.query.get('jd') == c.id}>
                  <span class="code">{c.code}</span>{c.name}
                  {#if inboxCategory && c.id === inboxCategory.id && inboxCount > 0}<span class="badge">{inboxCount}</span>{/if}
                </a>
              {/each}
            {/if}
          {/each}
        </nav>
      {/if}

      <div class="side-foot">
        <button class="btn sm" onclick={() => setTheme(session.theme === 'dark' ? 'light' : 'dark')} title="Switch theme">
          <Icon name={session.theme === 'dark' ? 'sun' : 'moon'} size={14} />
        </button>
        <a href="#/settings" class="btn sm" title="Settings"><Icon name="settings" size={14} /></a>
        <button class="btn sm" onclick={signOut} title="Sign out"><Icon name="out" size={14} /></button>
      </div>
    </aside>

    <div class="main">
      <div class="topbar">
        <h1>
          {#if page === 'doc'}Document
          {:else if page === 'tasks'}Approvals
          {:else}{page[0].toUpperCase() + page.slice(1)}{/if}
        </h1>
        <button class="searchbox" onclick={() => (paletteOpen = true)}>
          <Icon name="search" size={14} /> Search or jump to <kbd>⌘K</kbd>
        </button>
        <a role="button" class="btn primary" href="#/upload" style="padding:8px 16px">
          <Icon name="upload" size={15} /> Upload
        </a>
        <button class="btn bell" onclick={() => (drawerOpen = !drawerOpen)}
                aria-label={`Activity, ${bellCount} items needing attention`} title="Activity">
          <Icon name="bell" size={16} />
          {#if bellCount > 0}<span class="bell-dot">{bellCount}</span>{/if}
        </button>
      </div>

      {#if setupNeeded && page !== 'settings'}
        <div class="setup-banner">
          <span><b>Finish setting up suchi.</b> The wizard covers your filing tree, OCR, classification, and backups — every step is optional and nothing is blocked meanwhile.</span>
          <a role="button" class="btn primary sm" href="#/setup">Open setup wizard</a>
          <button class="btn sm" onclick={dismissSetup}>Later</button>
        </div>
      {/if}

      <div class="content">
        {#if page === 'dashboard'}<Dashboard {notify} {inboxCategory} {inboxCount} pending={pendingTasks.length} dead={deadJobs.length} recent={recentDocs} />
        {:else if page === 'documents'}<Documents {notify} />
        {:else if page === 'doc'}<DocumentDetail id={route.parts[1]} {notify} />
        {:else if page === 'inbox'}<Documents {notify} inbox={inboxCategory} />
        {:else if page === 'search'}<SearchPage />
        {:else if page === 'tasks'}<Tasks {notify} onCount={() => pollActivity()} />
        {:else if page === 'automations'}<Automations {notify} />
        {:else if page === 'upload'}<Upload {notify} />
        {:else if page === 'settings'}<Settings {notify} setupPending={setupNeeded} />
        {:else if page === 'setup'}<Setup {notify} onDone={() => { setupNeeded = false; go('#/dashboard') }} />
        {:else if page === 'login'}<Login onSignedIn={() => go('#/dashboard')} />
        {:else}<div class="empty">Nothing filed under <code>#{route.path}</code>. <a href="#/dashboard">Back to the dashboard</a></div>
        {/if}
      </div>
    </div>
  </div>

  {#if drawerOpen}
    <div class="drawer-veil" onclick={() => (drawerOpen = false)} role="presentation">
      <aside class="ndrawer" onclick={(e) => e.stopPropagation()} aria-label="Activity">
        <div class="ndrawer-head">
          <h3>Activity</h3>
          <button class="btn sm" onclick={() => (drawerOpen = false)}><Icon name="x" size={13} /></button>
        </div>

        {#if pendingTasks.length}
          <div class="side-head" style="padding-left:0">Waiting on you</div>
          <div class="index">
            {#each pendingTasks.slice(0, 8) as t (t.id)}
              <a class="irow" href="#/tasks" onclick={() => (drawerOpen = false)}>
                <span class="dot warn"></span>
                <span class="title grow">{t.title || t.kind || `Task #${t.id}`}</span>
              </a>
            {/each}
          </div>
        {/if}

        {#if deadJobs.length}
          <div class="side-head" style="padding-left:0">Failed — needs attention</div>
          <div class="index">
            {#each deadJobs.slice(0, 8) as j (j.id)}
              <a class="irow" href="#/tasks" onclick={() => (drawerOpen = false)}>
                <span class="dot danger"></span>
                <span class="title grow mono" style="font-size:.8rem">{j.kind}</span>
                {#if j.doc_id}<span class="sub">doc #{j.doc_id}</span>{/if}
              </a>
            {/each}
          </div>
        {/if}

        <div class="side-head" style="padding-left:0">Recently added</div>
        <div class="index">
          {#each recentDocs as d (d.id)}
            <a class="irow" href={`#/doc/${d.id}`} onclick={() => (drawerOpen = false)}>
              <span class="dot accent"></span>
              <span class="title grow">{d.title || `Document #${d.id}`}</span>
              {#if d.jd_category_code}<span class="chip">{d.jd_category_code}</span>{/if}
            </a>
          {:else}
            <div class="irow"><span class="sub">Nothing yet — <a href="#/upload">upload something</a>.</span></div>
          {/each}
        </div>

        {#if !pendingTasks.length && !deadJobs.length}
          <p class="sub" style="color:var(--muted);font-size:.8rem;margin-top:12px">Nothing needs you. The archive is running itself.</p>
        {/if}
      </aside>
    </div>
  {/if}

  {#if paletteOpen}
    <Palette close={() => (paletteOpen = false)} />
  {/if}
{/if}

{#if toast}<div class="toast">{toast}</div>{/if}
