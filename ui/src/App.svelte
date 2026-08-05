<script>
  import { route, go } from './lib/router.svelte.js'
  import { session, refreshSession, initTheme, setTheme, signOut } from './lib/session.svelte.js'
  import { listJDCategories, listTasks } from './lib/api.js'
  import Icon from './lib/Icon.svelte'
  import Palette from './lib/Palette.svelte'
  import Login from './routes/Login.svelte'
  import Documents from './routes/Documents.svelte'
  import DocumentDetail from './routes/DocumentDetail.svelte'
  import SearchPage from './routes/Search.svelte'
  import Tasks from './routes/Tasks.svelte'
  import Automations from './routes/Automations.svelte'
  import Upload from './routes/Upload.svelte'
  import Settings from './routes/Settings.svelte'

  let paletteOpen = $state(false)
  let jdTree = $state([])          // [{code,name,categories:[{id,code,name}]}]
  let inboxCategory = $state(null) // jd category holding the review queue
  let pendingTasks = $state(0)
  let toast = $state('')
  let toastTimer

  export function notify(msg) {
    toast = msg
    clearTimeout(toastTimer)
    toastTimer = setTimeout(() => (toast = ''), 2400)
  }

  initTheme()
  refreshSession().then(() => { if (session.user) boot() })

  async function boot() {
    try {
      const cats = await listJDCategories()
      if (cats?.results) buildTree(cats.results)
    } catch {}
    try {
      const t = await listTasks({ include: 'workflow', state: 'pending', limit: 200 })
      pendingTasks = (t?.results || t || []).length
    } catch {}
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

  function onKey(e) {
    if ((e.metaKey || e.ctrlKey) && e.key === 'k') { e.preventDefault(); paletteOpen = !paletteOpen }
    if (e.key === 'Escape') paletteOpen = false
  }

  const page = $derived(route.parts[0] || 'documents')
  const nav = [
    { hash: '#/documents', ico: 'docs',   label: 'Documents', key: 'documents' },
    { hash: '#/inbox',     ico: 'inbox',  label: 'Inbox',     key: 'inbox' },
    { hash: '#/search',    ico: 'search', label: 'Search',    key: 'search' },
    { hash: '#/tasks',     ico: 'tasks',  label: 'Approvals', key: 'tasks' },
    { hash: '#/automations', ico: 'zap',  label: 'Automations', key: 'automations' },
    { hash: '#/upload',    ico: 'upload', label: 'Upload',    key: 'upload' },
  ]
</script>

<svelte:window onkeydown={onKey} />

{#if !session.checked}
  <div class="login-wrap"><div class="skel" style="width:220px"></div></div>
{:else if !session.user}
  <Login onSignedIn={() => { boot(); go('#/documents') }} />
{:else}
  <div class="shell">
    <aside class="sidebar">
      <a class="brand" href="#/documents" aria-label="suchi home">
        <svg viewBox="0 0 64 64" aria-hidden="true"><rect x="8" y="8" width="48" height="48" rx="8" fill="var(--manila)" stroke="currentColor" stroke-width="3.5"/><circle cx="17.5" cy="19" r="2.2" fill="currentColor"/><line x1="23" y1="19" x2="48" y2="19" stroke="currentColor" stroke-width="3.5" stroke-linecap="round"/><circle cx="17.5" cy="28" r="2.2" fill="currentColor"/><line x1="23" y1="28" x2="48" y2="28" stroke="currentColor" stroke-width="3.5" stroke-linecap="round"/><circle cx="16.8" cy="37.25" r="3.2" fill="var(--accent)"/><rect x="22.5" y="33.5" width="29.5" height="7.5" rx="3.75" fill="var(--accent)"/><circle cx="17.5" cy="46" r="2.2" fill="currentColor"/><line x1="23" y1="46" x2="48" y2="46" stroke="currentColor" stroke-width="3.5" stroke-linecap="round"/></svg>
        <b>suchi</b>
      </a>

      <nav class="nav">
        {#each nav as n}
          <a href={n.hash} class:on={page === n.key}>
            <Icon name={n.ico} />{n.label}
            {#if n.key === 'tasks' && pendingTasks > 0}<span class="badge">{pendingTasks}</span>{/if}
          </a>
        {/each}
      </nav>

      {#if jdTree.length}
        <div class="side-head">Index</div>
        <nav class="jd-tree">
          {#each jdTree as area}
            <a href={'#/documents'} class="area-row"><span class="code">{area.lo}–{area.lo + 9}</span><span class="area">{area.name}</span></a>
            {#each area.categories as c}
              <a href={`#/documents?jd=${c.id}`} class:on={route.query.get('jd') == c.id}>
                <span class="code">{c.code}</span>{c.name}
              </a>
            {/each}
          {/each}
        </nav>
      {/if}

      <div class="side-foot">
        <button class="btn sm" onclick={() => setTheme(session.theme === 'dark' ? 'light' : 'dark')} title="Switch theme">
          <Icon name={session.theme === 'dark' ? 'sun' : 'moon'} size={14} />
        </button>
        <a href="#/settings" class="btn sm" class:on={page === 'settings'} title="Settings"><Icon name="settings" size={14} /></a>
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
      </div>

      <div class="content">
        {#if page === 'documents'}<Documents {notify} />
        {:else if page === 'doc'}<DocumentDetail id={route.parts[1]} {notify} />
        {:else if page === 'inbox'}<Documents {notify} inbox={inboxCategory} />
        {:else if page === 'search'}<SearchPage />
        {:else if page === 'tasks'}<Tasks {notify} onCount={(n) => (pendingTasks = n)} />
        {:else if page === 'automations'}<Automations {notify} />
        {:else if page === 'upload'}<Upload {notify} />
        {:else if page === 'settings'}<Settings {notify} />
        {:else if page === 'login'}<Login onSignedIn={() => go('#/documents')} />
        {:else}<div class="empty">Nothing filed under <code>#{route.path}</code>. <a href="#/documents">Back to documents</a></div>
        {/if}
      </div>
    </div>
  </div>

  {#if paletteOpen}
    <Palette close={() => (paletteOpen = false)} />
  {/if}
{/if}

{#if toast}<div class="toast">{toast}</div>{/if}
