<script>
  import { listSavedViews, createSavedView, deleteSavedView,
           listTags, listCorrespondents, listDocumentTypes } from '../lib/api.js'
  import { canonicalSavedViewQuery, documentListHash, parseSavedViewFilters } from '../lib/documentFilters.js'
  import { SENSITIVITY_OPTIONS, sensitivityLabel } from '../lib/format.js'
  import { DATE_ROLES, intelligenceRoleLabel } from '../lib/intelligence.js'
  import Icon from '../lib/Icon.svelte'
  import { go } from '../lib/router.svelte.js'

  let { notify, canShare = false, startCreate = false, createQuery = '', createDocumentIDs = '', jdCategories = [] } = $props()
  let views = $state([])
  let loading = $state(true)
  let loadError = $state('')
  let saving = $state(false)
  let saveError = $state('')
  let createOpen = $state(false)
  let startCreateHandled = $state(false)
  let nameInput = $state()
  let tags = $state([]), corrs = $state([]), types = $state([])
  const filingCategories = $derived(jdCategories.filter((category) => !category.is_area))
  let facetsPromise
  let facetsError = $state('')
  let loadVersion = 0
  let nv = $state(emptyView())

  function emptyView() {
    return { name: '', q: '', tag: '', corr: '', type: '', jd: '', sens: '', dateFrom: '', dateTo: '', dateRole: '', ids: [], shared: false }
  }

  function parseDocumentIDs(value) {
    return [...new Set(String(value || '').split(',')
      .map(id => Number(id.trim()))
      .filter(id => Number.isInteger(id) && id > 0))].slice(0, 100)
  }

  async function load() {
    const version = ++loadVersion
    loading = true
    loadError = ''
    try {
      const r = await listSavedViews({ include: 'shared' })
      if (version !== loadVersion) return
      const raw = r?.results || r || []
      views = raw.map(view => ({ ...view, filters: parseSavedViewFilters(view.filter_json) }))
      if (views.length) loadFacets()
    } catch (ex) {
      if (version === loadVersion) loadError = ex.message || 'Could not load saved views.'
    } finally {
      if (version === loadVersion) loading = false
    }
  }

  function href(v) {
    return documentListHash(v.filters)
  }

  function nameFor(items, id, fallback, format = (item) => item.name) {
    const item = items.find((candidate) => String(candidate.id) === String(id))
    return item ? format(item) : fallback
  }

  function filterSummary(filters) {
    const summary = []
    if (Array.isArray(filters.document_ids) && filters.document_ids.length) {
      summary.push(`${filters.document_ids.length} saved documents`)
    }
    if (filters.q) summary.push(`Search: “${filters.q}”`)
    if (filters.jd_category_id) summary.push(nameFor(filingCategories, filters.jd_category_id, 'Category', (c) => `${c.code} ${c.name}`))
    if (filters.tags__id__in) summary.push(`Tag: ${nameFor(tags, filters.tags__id__in, 'Selected tag')}`)
    if (filters.correspondents__id__in) summary.push(nameFor(corrs, filters.correspondents__id__in, 'Selected correspondent'))
    if (filters.document_type__id) summary.push(nameFor(types, filters.document_type__id, 'Selected type'))
    if (filters.sensitivity) summary.push(sensitivityLabel(filters.sensitivity))
    if (filters.ordering) summary.push(filters.ordering === 'title' ? 'Title order' : filters.ordering === '-created_at' ? 'Newest first' : 'Custom order')
    return summary.length ? summary : ['All documents']
  }

  function openCreate() {
    if (!nv.ids.length) loadFacets()
    saveError = ''
    createOpen = true
    queueMicrotask(() => nameInput?.focus())
  }

  function loadFacets() {
    if (facetsPromise) return facetsPromise
    facetsError = ''
    facetsPromise = Promise.allSettled([
      listTags().then((r) => (tags = r?.results || r || [])),
      listCorrespondents().then((r) => (corrs = r?.results || r || [])),
      listDocumentTypes().then((r) => (types = r?.results || r || [])),
    ]).then((results) => {
      if (results.some((result) => result.status === 'rejected')) {
        facetsError = 'Some filters could not be loaded.'
        facetsPromise = null
      }
    })
    return facetsPromise
  }

  function closeCreate() {
    if (saving) return
    createOpen = false
    saveError = ''
    nv = emptyView()
    if (startCreate) go('#/views')
  }

  async function create(e) {
    e.preventDefault()
    if (!nv.name.trim() || saving) return

    const query = nv.ids.length ? '' : canonicalSavedViewQuery(nv, {
      tags,
      correspondents: corrs,
      types,
      categories: filingCategories,
    })
    const filters = nv.ids.length ? { document_ids: nv.ids } : (query ? { q: query } : {})

    saveError = ''
    saving = true
    try {
      await createSavedView({
        name: nv.name.trim(),
        filter_json: JSON.stringify(filters),
        display: 'list',
        position: views.filter((view) => !view.owner_id).length,
        shared: canShare && nv.shared,
      })
      nv = emptyView()
      createOpen = false
      if (startCreate) go('#/views')
      notify?.('View saved')
      await load()
    } catch (ex) { saveError = ex.message || 'Could not save the view.' }
    finally { saving = false }
  }

  async function remove(v) {
    if (!confirm(`Delete the view “${v.name}”?`)) return
    try {
      await deleteSavedView(v.id)
      views = views.filter((x) => x.id !== v.id)
      notify?.('View deleted')
    } catch (ex) { notify?.(ex.message || 'Could not delete') }
  }

  $effect(() => {
    if (!startCreate) {
      startCreateHandled = false
    } else if (!startCreateHandled) {
      startCreateHandled = true
      nv.q = createQuery
      nv.ids = parseDocumentIDs(createDocumentIDs)
      openCreate()
    }
  })
  load()
</script>

<div class="views-page">
  <header class="views-intro">
    <div>
      <span class="eyebrow">Saved searches</span>
      <h2>Shortcuts into the archive</h2>
      <p>Keep the document filters you return to. Open a view to pick up exactly where you left off.</p>
    </div>
    <button class="btn primary new-view" onclick={openCreate}>
      <Icon name="plus" size={15} /> New view
    </button>
  </header>

  <section class="views-panel" aria-labelledby="saved-views-heading">
    <div class="panel-head">
      <div>
        <h3 id="saved-views-heading">Saved views</h3>
        <span aria-live="polite">{loading ? 'Loading' : `${views.length} ${views.length === 1 ? 'view' : 'views'}`}</span>
      </div>
      <span class="panel-hint">Select a view to open its matching documents</span>
    </div>

    {#if loading}
      <div class="view-list" aria-label="Loading saved views">
        {#each Array(3) as _}
          <div class="view-row loading-row">
            <span class="view-mark skeleton-mark"></span>
            <div class="loading-copy">
              <div class="skel" style="width:38%"></div>
              <div class="skel" style="width:68%"></div>
            </div>
          </div>
        {/each}
      </div>
    {:else if loadError}
      <div class="empty views-empty">
        <b>Could not load saved views</b>
        <span>{loadError}</span>
        <button class="btn sm" onclick={load}><Icon name="refresh" size={13} /> Retry</button>
      </div>
    {:else if views.length === 0}
      <div class="empty views-empty">
        <span class="empty-mark"><Icon name="eye" size={25} /></span>
        <b>No saved views yet</b>
      </div>
    {:else}
      <div class="view-list">
        {#each views as v (v.id)}
          <div class="view-row" class:shared-view={!!v.owner_id}>
            <a class="view-link" href={href(v)}>
              <span class="view-mark"><Icon name="eye" size={17} /></span>
              <span class="view-copy">
                <span class="view-name">
                  <strong>{v.name}</strong>
                  {#if v.shared}<span class="pill ok" title={v.owner_id ? `Shared by user #${v.owner_id}` : 'Shared with every user'}>Shared</span>{/if}
                </span>
                <span class="filter-summary">
                  {#each filterSummary(v.filters) as filter}
                    <span>{filter}</span>
                  {/each}
                </span>
              </span>
              <span class="open-view" aria-hidden="true"><Icon name="chev" size={15} /></span>
            </a>
            {#if !v.owner_id}
              <button class="delete-view" onclick={() => remove(v)} title={`Delete ${v.name}`} aria-label={`Delete ${v.name}`}>
                <Icon name="trash" size={14} />
              </button>
            {/if}
          </div>
        {/each}
      </div>
    {/if}
  </section>
</div>

{#if createOpen}
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <div class="modal-veil" onclick={closeCreate} role="presentation">
    <div class="modal create-modal" onclick={(e) => e.stopPropagation()} onkeydown={(e) => { if (e.key === 'Escape') closeCreate() }} role="dialog" aria-modal="true" aria-labelledby="new-view-title" tabindex="-1">
      <form onsubmit={create}>
      <div class="modal-head create-head">
        <div>
          <h3 id="new-view-title">Create a view</h3>
          <p>Choose the documents this shortcut should open.</p>
        </div>
        <button type="button" class="btn sm" onclick={closeCreate} disabled={saving} title="Close" aria-label="Close create view"><Icon name="x" size={13} /></button>
      </div>

      <div class="field name-field">
        <label for="view-name">View name</label>
        <input id="view-name" class="input" placeholder="e.g. Tax documents to review" bind:this={nameInput} bind:value={nv.name} required />
      </div>

      {#if nv.ids.length}
        <div class="snapshot-note">
          <span class="snapshot-mark"><Icon name="docs" size={18} /></span>
          <span>
            <strong>Exact research snapshot</strong>
            <small>This view keeps these {nv.ids.length} documents together. Access is checked whenever it opens.</small>
          </span>
        </div>
      {:else}
        <div class="field search-field">
          <label for="view-search">Query <span>Optional</span></label>
          <input id="view-search" class="input" placeholder='Text or filters, e.g. tag:tax -is:trash' bind:value={nv.q} />
        </div>

        <div class="filter-heading">
          <span>Filters</span>
          <small>Leave any field open to include everything</small>
        </div>
        {#if facetsError}
          <div class="err facet-error">
            <span>{facetsError}</span>
            <button type="button" class="btn sm" onclick={loadFacets}>Retry</button>
          </div>
        {/if}
        <div class="filter-grid">
          <div class="field">
            <label for="view-category">Filing category</label>
            <select id="view-category" class="input" bind:value={nv.jd}>
              <option value="">Any category</option>
              {#each filingCategories as c}<option value={c.id}>{c.code} {c.name}</option>{/each}
            </select>
          </div>
          <div class="field">
            <label for="view-tag">Tag</label>
            <select id="view-tag" class="input" bind:value={nv.tag}>
              <option value="">Any tag</option>
              {#each tags as t}<option value={t.id}>{t.name}</option>{/each}
            </select>
          </div>
          <div class="field">
            <label for="view-correspondent">Correspondent</label>
            <select id="view-correspondent" class="input" bind:value={nv.corr}>
              <option value="">Any correspondent</option>
              {#each corrs as c}<option value={c.id}>{c.name}</option>{/each}
            </select>
          </div>
          <div class="field">
            <label for="view-type">Document type</label>
            <select id="view-type" class="input" bind:value={nv.type}>
              <option value="">Any type</option>
              {#each types as t}<option value={t.id}>{t.name}</option>{/each}
            </select>
          </div>
          <div class="field">
            <label for="view-sensitivity">Sensitivity</label>
            <select id="view-sensitivity" class="input" bind:value={nv.sens}>
              <option value="">Any sensitivity</option>
              {#each SENSITIVITY_OPTIONS as option (option.value)}
                <option value={option.value}>{option.label}</option>
              {/each}
            </select>
          </div>
          <div class="field">
            <label for="view-date-from">Document date from</label>
            <input id="view-date-from" class="input" type="date" bind:value={nv.dateFrom} />
          </div>
          <div class="field">
            <label for="view-date-to">Document date to</label>
            <input id="view-date-to" class="input" type="date" bind:value={nv.dateTo} />
          </div>
          <div class="field">
            <label for="view-date-role">Document date role</label>
            <select id="view-date-role" class="input" bind:value={nv.dateRole}>
              <option value="">Every role</option>
              {#each DATE_ROLES as role}
                <option value={role}>{intelligenceRoleLabel(role)}</option>
              {/each}
            </select>
          </div>
        </div>
      {/if}

      {#if canShare}
        <label class="share-option">
          <input type="checkbox" bind:checked={nv.shared} />
          <span>
            <strong>Share this view</strong>
            <small>Show it in every user's Views list and dashboard.</small>
          </span>
        </label>
      {/if}

      {#if saveError}<div class="err save-error" role="alert">{saveError}</div>{/if}

      <div class="form-actions">
        <button type="button" class="btn" onclick={closeCreate} disabled={saving}>Cancel</button>
        <button class="btn primary" disabled={saving || !nv.name.trim()}>{saving ? 'Saving…' : 'Save view'}</button>
      </div>
      </form>
    </div>
  </div>
{/if}

<style>
  .views-page { max-width: 960px; margin: 0 auto; }
  .views-intro { display: flex; align-items: flex-end; justify-content: space-between; gap: 28px; margin: 10px 0 26px; }
  .views-intro > div { max-width: 610px; }
  .eyebrow { display: block; margin-bottom: 7px; color: var(--accent); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .66rem; font-weight: 700; letter-spacing: 0; }
  .views-intro h2 { font-size: 1.55rem; line-height: 1.18; }
  .views-intro p { margin: 8px 0 0; color: var(--muted); font-size: .9rem; }
  .new-view { flex: none; padding: 9px 16px; }

  .views-panel { overflow: hidden; background: var(--surface); border: 1px solid var(--line); border-radius: var(--r); }
  .panel-head { display: flex; align-items: center; justify-content: space-between; gap: 18px; padding: 14px 17px; border-bottom: 1px solid var(--line); background: var(--surface-2); }
  .panel-head > div { display: flex; align-items: baseline; gap: 9px; }
  .panel-head h3 { font-size: .9rem; }
  .panel-head span { color: var(--muted); font-size: .72rem; }
  .panel-hint { text-align: right; }

  .view-list { display: flex; flex-direction: column; }
  .view-row { display: grid; grid-template-columns: minmax(0, 1fr) 48px; min-height: 78px; border-bottom: 1px solid var(--line); transition: background .14s ease; }
  .view-row.shared-view { grid-template-columns: minmax(0, 1fr); }
  .view-row:last-child { border-bottom: 0; }
  .view-row:hover { background: var(--tint); }
  .view-link { display: flex; align-items: center; gap: 13px; min-width: 0; padding: 13px 8px 13px 17px; color: inherit; text-decoration: none; }
  .view-mark { display: grid; place-items: center; width: 34px; height: 34px; flex: none; border: 1px solid color-mix(in srgb, var(--accent) 24%, var(--line)); border-radius: 9px; background: var(--tint); color: var(--accent); }
  .view-copy { display: flex; flex: 1; flex-direction: column; gap: 7px; min-width: 0; }
  .view-name { display: flex; align-items: center; gap: 8px; min-width: 0; }
  .view-name strong { overflow: hidden; font-size: .9rem; font-weight: 650; text-overflow: ellipsis; white-space: nowrap; }
  .filter-summary { display: flex; gap: 5px; min-width: 0; overflow: hidden; }
  .filter-summary > span { overflow: hidden; max-width: 220px; padding: 2px 7px; border-radius: 5px; background: var(--surface-2); color: var(--muted); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .65rem; text-overflow: ellipsis; white-space: nowrap; }
  .open-view { display: grid; place-items: center; width: 30px; height: 30px; flex: none; color: var(--faint); transition: transform .14s ease, color .14s ease; }
  .view-row:hover .open-view { transform: translateX(2px); color: var(--accent); }
  .delete-view { align-self: stretch; width: 48px; border: 0; border-left: 1px solid transparent; background: transparent; color: var(--faint); opacity: 0; transition: opacity .14s ease, color .14s ease, background .14s ease; }
  .view-row:hover .delete-view, .delete-view:focus-visible { opacity: 1; }
  .delete-view:hover { border-left-color: var(--line); background: var(--danger-soft); color: var(--danger); }

  .loading-row { display: flex; align-items: center; gap: 13px; padding: 13px 17px; }
  .loading-row:hover { background: transparent; }
  .skeleton-mark { border-color: var(--line); background: var(--surface-2); }
  .loading-copy { display: grid; flex: 1; gap: 10px; }
  .views-empty { padding: 64px 20px; }
  .empty-mark { display: grid; place-items: center; width: 52px; height: 52px; border: 1px solid var(--line); border-radius: 14px; background: var(--surface-2); color: var(--accent); }
  .views-empty > span:not(.empty-mark) { max-width: 390px; }

  .create-modal { width: min(720px, 94vw); }
  .create-head { align-items: flex-start; margin-bottom: 20px; }
  .create-head h3 { font-size: 1.08rem; }
  .create-head p { margin: 3px 0 0; color: var(--muted); font-size: .82rem; }
  .create-modal .field { margin-bottom: 14px; }
  .field label span { margin-left: 5px; color: var(--faint); font-size: .68rem; font-weight: 500; }
  .name-field .input { font-weight: 600; }
  .search-field { padding-bottom: 3px; }
  .snapshot-note { display: flex; gap: 11px; margin-bottom: 15px; padding: 13px; border: 1px solid color-mix(in srgb, var(--accent) 28%, var(--line)); border-radius: 10px; background: var(--tint); }
  .snapshot-mark { display: grid; place-items: center; width: 34px; height: 34px; flex: none; border-radius: 9px; background: var(--surface); color: var(--accent); }
  .snapshot-note > span:last-child { display: flex; flex-direction: column; gap: 3px; }
  .snapshot-note strong { font-size: .82rem; }
  .snapshot-note small { color: var(--muted); font-size: .72rem; line-height: 1.45; }
  .save-error { margin: 0 0 12px; }
  .filter-heading { display: flex; align-items: baseline; justify-content: space-between; gap: 12px; margin: 2px 0 10px; padding-top: 14px; border-top: 1px solid var(--line); }
  .filter-heading span { font-size: .78rem; font-weight: 700; }
  .filter-heading small { color: var(--muted); font-size: .7rem; }
  .filter-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 0 12px; }
  .share-option { display: flex; align-items: flex-start; gap: 10px; margin-top: 3px; padding: 12px; border: 1px solid var(--line); border-radius: 9px; background: var(--surface); cursor: pointer; }
  .share-option input { margin-top: 3px; accent-color: var(--accent); }
  .share-option span { display: flex; flex-direction: column; gap: 1px; }
  .share-option strong { font-size: .8rem; }
  .share-option small { color: var(--muted); font-size: .72rem; }
  .form-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 18px; padding-top: 14px; border-top: 1px solid var(--line); }

  @media (hover: none) {
    .delete-view { opacity: 1; }
  }

  @media (max-width: 620px) {
    .views-intro { align-items: flex-start; flex-direction: column; gap: 16px; margin-top: 2px; }
    .new-view { width: 100%; justify-content: center; }
    .panel-head { align-items: flex-start; }
    .panel-hint { display: none; }
    .filter-summary { max-width: 100%; }
    .filter-summary > span { max-width: 170px; }
    .filter-grid { grid-template-columns: 1fr; }
    .modal-veil { padding-top: 3vh; }
    .create-modal { max-height: 94vh; }
  }
</style>
