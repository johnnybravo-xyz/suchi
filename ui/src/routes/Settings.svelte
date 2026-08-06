<script>
  import { listTokens, createToken, deleteToken, listSavedViews, createSavedView, deleteSavedView, listTags, listCorrespondents, listDocumentTypes, listJDCategories, setupState } from '../lib/api.js'
  import { session } from '../lib/session.svelte.js'
  import { fmtDate } from '../lib/format.js'
  import Icon from '../lib/Icon.svelte'

  let { notify, setupPending = false } = $props()
  let tokens = $state([])
  let err = $state('')
  let newName = $state('')
  let minted = $state('')   // freshly created secret, shown once

  // --- setup wizard state (admin only; quiet on 403) ---
  let setup = $state(null)
  setupState().then(st => (setup = st)).catch(() => {})
  const setupDone = $derived(!!setup?.completed_at)

  // --- custom views ---
  let views = $state([])
  let facets = $state({ tags: [], correspondents: [], types: [], cats: [] })
  let nv = $state({ name: '', q: '', tag: '', corr: '', type: '', jd: '', sens: '' })

  async function loadViews() {
    try { const r = await listSavedViews(); views = (r?.results || r || []).sort((a, b) => a.position - b.position) } catch {}
  }
  async function loadFacets() {
    try {
      const [t, c, d, j] = await Promise.all([listTags(), listCorrespondents(), listDocumentTypes(), listJDCategories()])
      facets = { tags: t?.results || [], correspondents: c?.results || [], types: d?.results || [], cats: j?.results || [] }
    } catch {}
  }
  async function addView(e) {
    e.preventDefault()
    if (!nv.name.trim()) { notify?.('Give the view a name'); return }
    const filters = {}
    if (nv.q.trim()) filters.q = nv.q.trim()
    if (nv.tag) filters.tags__id__in = nv.tag
    if (nv.corr) filters.correspondents__id__in = nv.corr
    if (nv.type) filters.document_type__id = nv.type
    if (nv.jd) filters.jd_category_id = nv.jd
    if (nv.sens) filters.sensitivity = nv.sens
    try {
      await createSavedView({ name: nv.name.trim(), filter_json: JSON.stringify(filters), display: 'list', position: views.length })
      nv = { name: '', q: '', tag: '', corr: '', type: '', jd: '', sens: '' }
      notify?.('View saved — it is on the dashboard now')
      loadViews()
    } catch (ex) { notify?.(ex.message || 'Could not save the view') }
  }
  async function removeView(v) {
    if (!confirm(`Delete the view “${v.name}”?`)) return
    try { await deleteSavedView(v.id); views = views.filter(x => x.id !== v.id); notify?.('View deleted') }
    catch (ex) { notify?.(ex.message || 'Could not delete') }
  }
  loadViews(); loadFacets()

  async function load() {
    try {
      const res = await listTokens()
      tokens = res?.results || res || []
    } catch (ex) { err = ex.message || 'Could not load tokens.' }
  }

  async function mint(e) {
    e.preventDefault()
    try {
      const res = await createToken({ name: newName.trim() || 'api token' })
      minted = res?.token || ''
      newName = ''
      notify?.('Token created — copy it now, it is shown once')
      load()
    } catch (ex) { notify?.(ex.message || 'Could not create a token') }
  }

  async function revoke(t) {
    if (!confirm(`Revoke “${t.name}”? Anything using it stops working immediately.`)) return
    try { await deleteToken(t.id); tokens = tokens.filter(x => x.id !== t.id); notify?.('Token revoked') }
    catch (ex) { notify?.(ex.message || 'Could not revoke') }
  }

  load()
</script>

<div class="content-narrow" style="display:flex;flex-direction:column;gap:16px">
  {#if setup !== null}
    <div class="card" style={setupDone ? '' : 'border-color:color-mix(in srgb, var(--accent) 50%, var(--line));background:var(--tint)'}>
      <h3>Setup wizard</h3>
      {#if setupDone}
        <p class="sub" style="color:var(--muted);font-size:.86rem;margin:0 0 10px">
          Completed. You can revisit any step — filing tree, OCR languages, classification, email intake, backups — at any time; every step is independently re-runnable.
        </p>
        <a role="button" class="btn sm" href="#/setup">Revisit the wizard</a>
      {:else}
        <p class="sub" style="font-size:.9rem;margin:0 0 10px">
          <b>suchi hasn't been fully set up yet.</b> The wizard walks through the filing tree, OCR, classification, email intake, and backups — each step is optional and skippable, and nothing is blocked in the meantime.
        </p>
        <a role="button" class="btn primary" href="#/setup">Open the setup wizard</a>
      {/if}
    </div>
  {/if}

  <div class="card">
    <h3>Custom views</h3>
    <p class="sub" style="color:var(--muted);margin:0 0 12px;font-size:.84rem">
      A view is a saved filter that appears on your dashboard with a live count.
    </p>
    <form onsubmit={addView}>
      <div class="toolbar" style="margin-bottom:8px">
        <input class="input" style="flex:2;min-width:150px" placeholder="View name, e.g. Tax to review" bind:value={nv.name} />
        <input class="input" style="flex:2;min-width:150px" placeholder="Full-text query (optional)" bind:value={nv.q} />
      </div>
      <div class="toolbar" style="margin-bottom:10px">
        <select class="input" bind:value={nv.jd}><option value="">Any category</option>{#each facets.cats as c}<option value={c.id}>{c.code} {c.name}</option>{/each}</select>
        <select class="input" bind:value={nv.tag}><option value="">Any tag</option>{#each facets.tags as t}<option value={t.id}>{t.name}</option>{/each}</select>
        <select class="input" bind:value={nv.corr}><option value="">Any correspondent</option>{#each facets.correspondents as c}<option value={c.id}>{c.name}</option>{/each}</select>
        <select class="input" bind:value={nv.type}><option value="">Any type</option>{#each facets.types as t}<option value={t.id}>{t.name}</option>{/each}</select>
        <select class="input" bind:value={nv.sens}><option value="">Any sensitivity</option><option value="public">Public</option><option value="internal">Internal</option><option value="confidential">Confidential</option></select>
        <button class="btn primary sm">Save view</button>
      </div>
    </form>
    {#if views.length}
      <div class="index">
        {#each views as v (v.id)}
          <div class="irow">
            <span class="dot accent"></span>
            <span class="title grow">{v.name}</span>
            <span class="sub mono" style="font-size:.68rem">{v.filter_json}</span>
            <button class="btn sm danger" onclick={() => removeView(v)}><Icon name="trash" size={13} /></button>
          </div>
        {/each}
      </div>
    {/if}
  </div>

  <div class="card">
    <h3>Account</h3>
    <dl class="kv">
      <dt>Signed in as</dt><dd>{session.user?.email}</dd>
      <dt>Role</dt><dd><span class="pill">{session.user?.role}</span></dd>
      <dt>Auth</dt><dd>{session.user?.authn_by}</dd>
    </dl>
  </div>

  <div class="card">
    <h3>API tokens</h3>
    <p class="sub" style="color:var(--muted);margin:0 0 12px;font-size:.84rem">
      For mobile apps, agents, and <code>suchi mcp</code>. Sent as <code>Authorization: Token …</code>
    </p>
    {#if err}<div class="err">{err}</div>{/if}
    {#if minted}
      <div class="field">
        <label for="minted">New token — copy it now, it is not shown again</label>
        <input id="minted" class="input mono" style="font-size:.76rem" readonly value={minted}
               onclick={(e) => { e.target.select(); navigator.clipboard?.writeText(minted) }} />
      </div>
    {/if}
    <form class="toolbar" onsubmit={mint} style="margin-bottom:12px">
      <input class="input" style="flex:1" placeholder="Token name, e.g. phone, bravo-agent" bind:value={newName} />
      <button class="btn primary sm"><Icon name="plus" size={13} /> Create token</button>
    </form>
    {#if tokens.length}
      <div class="index">
        {#each tokens as t (t.id)}
          <div class="irow">
            <span class="dot"></span>
            <span class="title grow">{t.name}</span>
            <span class="sub">{t.created_at ? fmtDate(t.created_at) : ''}</span>
            <button class="btn sm danger" onclick={() => revoke(t)}>Revoke</button>
          </div>
        {/each}
      </div>
    {/if}
  </div>

  <div class="card">
    <h3>About this interface</h3>
    <p class="sub" style="color:var(--muted);font-size:.84rem;margin:0">
      Svelte SPA, zero runtime dependencies, talking to the same API the mobile apps use.
      Theme follows your system; the toggle in the sidebar overrides it.
    </p>
  </div>
</div>
