<script>
  import { route, go } from './lib/router.svelte.js'
  import { session, refreshSession, initTheme, setTheme, signOut } from './lib/session.svelte.js'
  import { listJDCategories, listTasks, listDocuments, setupState, stats as fetchStats, listEvents } from './lib/api.js'
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
  import Trash from './routes/Trash.svelte'
  import Locked from './routes/Locked.svelte'
  import Admin from './routes/Admin.svelte'
  import UploadBox from './lib/UploadBox.svelte'

  let paletteOpen = $state(false)
  let drawerOpen = $state(false)
  let umenuOpen = $state(false)
  let uploadOpen = $state(false)
  let lockedCount = $state(0)
  let dragDepth = $state(0)   // window-level drop target (except on #/upload)
  const initials = $derived((session.user?.display_name || session.user?.email || '?')
    .split(/[\s@._-]+/).filter(Boolean).slice(0, 2).map(w => w[0].toUpperCase()).join('') || '?')
  let jdTree = $state([])            // [{lo, name, categories:[…]}]
  let openAreas = $state(loadOpenAreas())
  let inboxCategory = $state(null)
  let inboxCount = $state(0)
  let pendingTasks = $state([])
  let deadJobs = $state([])
  let recentDocs = $state([])
  let st = $state(null)                 // /api/stats/ snapshot
  let events = $state([])               // activity feed rows (newest first)
  let seenEventID = $state(loadSeen())  // durable read cursor
  function loadSeen() { try { return Number(localStorage.getItem('suchi.events.seen') || 0) } catch { return 0 } }
  function markEventsRead() {
    if (events.length) seenEventID = Math.max(seenEventID, ...events.map(e => e.id))
    try { localStorage.setItem('suchi.events.seen', String(seenEventID)) } catch {}
  }
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
    try { st = await fetchStats() } catch {}
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
      if (st?.inbox_category_id ? c.id === st.inbox_category_id : /inbox/i.test(c.name)) inboxCategory = c
    }
    jdTree = [...areas.values()].sort((a, b) => a.lo - b.lo)
  }

  async function pollActivity() {
    try {
      st = await fetchStats()
      inboxCount = st?.inbox_count ?? 0
      if (inboxCount > 0 && inboxCategory) {
        const lo = Number(inboxCategory.area_code)
        if (!openAreas.has(lo)) toggleArea(lo)   // pending work must be visible
      }
    } catch {}
    try {
      const ev = await listEvents({ limit: 40 })
      events = (ev?.results || []).slice().reverse()   // newest first
    } catch {}
    // task lists still power the drawer rows + Approvals screen
    try {
      const [wf, jb] = await Promise.all([
        listTasks({ include: 'workflow', state: 'pending', limit: 50 }),
        listTasks({ include: 'jobs', state: 'dead', limit: 50 }),
      ])
      pendingTasks = wf?.results || wf || []
      deadJobs = jb?.results || jb || []
    } catch {}
    try {
      const r = await listDocuments({ page_size: 6, ordering: '-created_at' })
      recentDocs = r?.results || []
    } catch {}
    try {
      const { listPendingDecryption } = await import('./lib/api.js')
      const r = await listPendingDecryption()
      lockedCount = (r?.results || r || []).length
    } catch { lockedCount = 0 }
  }

  function dismissSetup() {
    setupNeeded = false
    try { localStorage.setItem('suchi.setup.dismissed', '1') } catch {}
  }

  async function globalDrop(e) {
    e.preventDefault()
    dragDepth = 0
    if (page === 'upload' || !session.user) return   // upload page has its own zone
    const files = [...(e.dataTransfer?.files || [])]
    if (!files.length) return
    notify(`Uploading ${files.length} file${files.length === 1 ? '' : 's'}…`)
    const { uploadDocument } = await import('./lib/api.js')
    let ok = 0, dup = 0, fail = 0
    for (const f of files) {
      try { await uploadDocument(f); ok++ }
      catch (ex) { ex.status === 409 ? dup++ : fail++ }
    }
    notify([ok && `${ok} uploaded`, dup && `${dup} duplicate${dup === 1 ? '' : 's'}`, fail && `${fail} failed`].filter(Boolean).join(' · '))
    pollActivity()
  }

  function onKey(e) {
    if ((e.metaKey || e.ctrlKey) && e.key === 'k') { e.preventDefault(); paletteOpen = !paletteOpen }
    if (e.key === 'Escape') { paletteOpen = false; drawerOpen = false; umenuOpen = false; uploadOpen = false }
  }

  $effect(() => { route.path; umenuOpen = false })
  const page = $derived(route.parts[0] || 'dashboard')
  const unseenEvents = $derived(events.filter(e => e.id > seenEventID).length)
  const bellCount = $derived((st?.pending_approvals ?? pendingTasks.length) + (st?.dead_jobs ?? deadJobs.length) + unseenEvents)
  const nav = [
    { hash: '#/dashboard',   ico: 'gauge',  label: 'Dashboard',   key: 'dashboard' },
    { hash: '#/documents',   ico: 'docs',   label: 'Documents',   key: 'documents' },
    { hash: '#/inbox',       ico: 'inbox',  label: 'Inbox',       key: 'inbox' },
    { hash: '#/search',      ico: 'search', label: 'Search',      key: 'search' },
    { hash: '#/tasks',       ico: 'tasks',  label: 'Approvals',   key: 'tasks' },
    { hash: '#/automations', ico: 'zap',    label: 'Automations', key: 'automations' },
  ]
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
        {#if lockedCount > 0}
          <a href="#/locked" class:on={page === 'locked'}>
            <Icon name="lock" />Locked<span class="badge">{lockedCount}</span>
          </a>
        {/if}
        {#if session.user?.role === 'admin'}
          <a href="#/admin" class:on={page === 'admin'}>
            <Icon name="shield" />Admin
          </a>
        {/if}
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
        <button class="btn primary" style="padding:8px 16px" onclick={() => (uploadOpen = true)}>
          <Icon name="upload" size={15} /> Upload
        </button>
        <button class="btn bell" onclick={() => { drawerOpen = !drawerOpen; if (drawerOpen) markEventsRead() }}
                aria-label={`Activity, ${bellCount} items needing attention`} title="Activity">
          <Icon name="bell" size={16} />
          {#if bellCount > 0}<span class="bell-dot">{bellCount}</span>{/if}
        </button>
        <button class="btn sm" style="padding:8px 11px" onclick={() => setTheme(session.theme === 'dark' ? 'light' : 'dark')} title="Switch theme">
          <Icon name={session.theme === 'dark' ? 'sun' : 'moon'} size={15} />
        </button>
        <div class="umenu-wrap">
          <button class="avatar" onclick={() => (umenuOpen = !umenuOpen)} aria-label="Account menu" aria-expanded={umenuOpen}>
            {#if session.user?.avatar_url}<img src={session.user.avatar_url} alt="" />{:else}{initials}{/if}
          </button>
          {#if umenuOpen}
            <div class="umenu" role="menu">
              <div class="who">
                <b>{session.user?.display_name || 'Account'}</b>
                <span>{session.user?.email}</span>
              </div>
              <a href="#/settings" onclick={() => (umenuOpen = false)} role="menuitem"><Icon name="user" size={14} /> Profile</a>
              <a href="#/settings" onclick={() => (umenuOpen = false)} role="menuitem"><Icon name="settings" size={14} /> Settings</a>
              <button onclick={signOut} role="menuitem"><Icon name="out" size={14} /> Sign out</button>
            </div>
          {/if}
        </div>
      </div>

      {#if setupNeeded && page !== 'settings'}
        <div class="setup-banner">
          <span><b>Finish setting up suchi.</b> The wizard covers your filing tree, OCR, classification, and backups — every step is optional and nothing is blocked meanwhile.</span>
          <a role="button" class="btn primary sm" href="#/setup">Open setup wizard</a>
          <button class="btn sm" onclick={dismissSetup}>Later</button>
        </div>
      {/if}

      <div class="content">
        {#if page === 'dashboard'}<Dashboard {notify} {st} {inboxCategory} recent={recentDocs} />
        {:else if page === 'documents'}<Documents {notify} />
        {:else if page === 'doc'}<DocumentDetail id={route.parts[1]} {notify} />
        {:else if page === 'inbox'}<Documents {notify} inbox={inboxCategory} />
        {:else if page === 'search'}<SearchPage />
        {:else if page === 'tasks'}<Tasks {notify} onCount={() => pollActivity()} />
        {:else if page === 'automations'}<Automations {notify} />
        {:else if page === 'upload'}<Upload {notify} />
        {:else if page === 'settings'}<Settings {notify} setupPending={setupNeeded} />
        {:else if page === 'trash'}<Trash {notify} />
        {:else if page === 'locked'}<Locked {notify} onChanged={() => pollActivity()} />
        {:else if page === 'admin'}<Admin {notify} />
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

        {#if events.length}
          <div class="side-head" style="padding-left:0">Activity</div>
          <div class="index">
            {#each events.slice(0, 12) as e (e.id)}
              {#if e.doc_id}
                <a class="irow" href={`#/doc/${e.doc_id}`} onclick={() => (drawerOpen = false)}>
                  <span class="dot" class:danger={e.kind.startsWith('job.')} class:warn={e.kind.startsWith('approval.')} class:accent={e.kind.startsWith('document.')}></span>
                  <span class="title grow" style="font-size:.84rem">{e.summary}</span>
                  <span class="sub">{new Date(e.created_at * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}</span>
                </a>
              {:else}
                <div class="irow">
                  <span class="dot" class:danger={e.kind.startsWith('job.')}></span>
                  <span class="title grow" style="font-size:.84rem">{e.summary}</span>
                  <span class="sub">{new Date(e.created_at * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}</span>
                </div>
              {/if}
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

  {#if uploadOpen}
    <div class="modal-veil" onclick={() => (uploadOpen = false)} role="presentation">
      <div class="modal" onclick={(e) => e.stopPropagation()} role="dialog" aria-label="Upload documents">
        <div class="modal-head">
          <h3>Upload</h3>
          <button class="btn sm" onclick={() => { uploadOpen = false; pollActivity() }}><Icon name="x" size={13} /></button>
        </div>
        <UploadBox {notify} />
      </div>
    </div>
  {/if}

  {#if paletteOpen}
    <Palette close={() => (paletteOpen = false)} />
  {/if}
{/if}

{#if toast}<div class="toast">{toast}</div>{/if}
