<script>
  // Saved views as a first-class destination: create, share, reorder-by-name,
  // and jump straight into the filtered Documents list.
  import { listSavedViews, createSavedView, deleteSavedView,
           listTags, listCorrespondents, listDocumentTypes, listJDCategories } from '../lib/api.js'
  import Icon from '../lib/Icon.svelte'

  let { notify } = $props()
  let views = $state([])
  let loading = $state(true)
  let tags = $state([]), corrs = $state([]), types = $state([]), jdCats = $state([])
  let nv = $state({ name: '', q: '', tag: '', corr: '', type: '', jd: '', sens: '', shared: false })

  async function load() {
    loading = true
    try {
      const r = await listSavedViews()
      views = r?.results || r || []
    } catch (ex) { notify?.(ex.message || 'Could not load views') }
    finally { loading = false }
  }

  function href(v) {
    try {
      const f = JSON.parse(v.filter_json || '{}')
      const p = new URLSearchParams()
      for (const [k, val] of Object.entries(f)) if (val !== '' && val != null) p.set(k, val)
      return `#/documents?${p}`
    } catch { return '#/documents' }
  }

  async function create(e) {
    e.preventDefault()
    if (!nv.name.trim()) return
    const filters = {}
    if (nv.q) filters.q = nv.q
    if (nv.tag) filters.tags__id__in = nv.tag
    if (nv.corr) filters.correspondents__id__in = nv.corr
    if (nv.type) filters.document_type__id = nv.type
    if (nv.jd) filters.jd_category_id = nv.jd
    if (nv.sens) filters.sensitivity = nv.sens
    try {
      await createSavedView({ name: nv.name.trim(), filter_json: JSON.stringify(filters), display: 'list', position: views.length, shared: nv.shared })
      nv = { name: '', q: '', tag: '', corr: '', type: '', jd: '', sens: '', shared: false }
      notify?.('View saved')
      load()
    } catch (ex) { notify?.(ex.message || 'Could not save the view') }
  }

  async function remove(v) {
    if (!confirm(`Delete the view “${v.name}”?`)) return
    try { await deleteSavedView(v.id); views = views.filter(x => x.id !== v.id); notify?.('View deleted') }
    catch (ex) { notify?.(ex.message || 'Could not delete') }
  }

  load()
  listTags().then(r => (tags = r?.results || r || [])).catch(() => {})
  listCorrespondents().then(r => (corrs = r?.results || r || [])).catch(() => {})
  listDocumentTypes().then(r => (types = r?.results || r || [])).catch(() => {})
  listJDCategories().then(r => (jdCats = (r?.results || r || []).filter(c => !c.is_area))).catch(() => {})
</script>

<div class="content-narrow" style="max-width:860px">
  <p class="sub" style="color:var(--muted);margin:0 0 16px;font-size:.88rem">
    A view is a saved filter that lives here, on your dashboard, and (if shared)
    on everyone else's too.
  </p>

  <form class="card" style="margin-bottom:18px" onsubmit={create}>
    <h3>New view</h3>
    <div class="toolbar" style="margin:10px 0 0">
      <input class="input" style="flex:2;min-width:150px" placeholder="Name, e.g. Tax to review" bind:value={nv.name} required />
      <input class="input" style="flex:2;min-width:130px" placeholder="Search text (optional)" bind:value={nv.q} />
      <select class="input" bind:value={nv.jd}><option value="">Any category</option>{#each jdCats as c}<option value={c.id}>{c.code} {c.name}</option>{/each}</select>
    </div>
    <div class="toolbar" style="margin:10px 0 0">
      <select class="input" bind:value={nv.tag}><option value="">Any tag</option>{#each tags as t}<option value={t.id}>{t.name}</option>{/each}</select>
      <select class="input" bind:value={nv.corr}><option value="">Any correspondent</option>{#each corrs as c}<option value={c.id}>{c.name}</option>{/each}</select>
      <select class="input" bind:value={nv.type}><option value="">Any type</option>{#each types as t}<option value={t.id}>{t.name}</option>{/each}</select>
      <select class="input" bind:value={nv.sens}><option value="">Any sensitivity</option><option value="public">Public</option><option value="internal">Internal</option><option value="confidential">Confidential</option></select>
      <label class="wiz-check" style="margin:0" title="Visible on every user's dashboard"><input type="checkbox" bind:checked={nv.shared} /> Shared</label>
      <button class="btn primary sm">Save view</button>
    </div>
  </form>

  {#if loading}
    <div class="index">{#each Array(3) as _}<div class="irow"><div class="skel" style="width:50%"></div></div>{/each}</div>
  {:else if views.length === 0}
    <div class="empty"><Icon name="eye" size={50} /><b>No views yet.</b><span>Save a filter above for quick access to matching documents.</span></div>
  {:else}
    <div class="index">
      {#each views as v (v.id)}
        <a class="irow" href={href(v)}>
          <span class="dot"></span>
          <span class="title grow">{v.name}</span>
          {#if v.shared}<span class="pill ok">shared</span>{/if}
          <span class="sub mono" style="font-size:.66rem;max-width:220px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">{v.filter_json}</span>
          <button class="btn sm danger" onclick={(e) => { e.preventDefault(); remove(v) }} title="Delete view"><Icon name="trash" size={12} /></button>
        </a>
      {/each}
    </div>
  {/if}
</div>
