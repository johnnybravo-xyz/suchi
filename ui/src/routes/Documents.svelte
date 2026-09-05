<script>
  import { onDestroy } from 'svelte'
  import { listDocuments, listTags, listCorrespondents, listDocumentTypes, patchDocument, deleteDocument, bulkEdit, createShareLink, thumbPath, decryptDocument, decryptBatch, extractIntelligence } from '../lib/api.js'
  import { route, go } from '../lib/router.svelte.js'
  import { uploadBus } from '../lib/upload_bus.svelte.js'
  import { SENSITIVITY_OPTIONS, fmtDate, isHighSensitivity, sensDot } from '../lib/format.js'
  import { session } from '../lib/session.svelte.js'
  import { hasCapability } from '../lib/capabilities.js'
  import Icon from '../lib/Icon.svelte'
  import ConfirmDialog from '../lib/ConfirmDialog.svelte'
  import { createQueryAssistant } from '../lib/queryAssist.js'

  let { notify, inbox = null, inboxMode = false, taxonomyLoaded = true, jdCategories = [],
        canAskArchive = false, canReviewIntelligence = false, onAskDocuments, onScopeChange } = $props()

  let docs = $state([])
  let count = $state(0)
  let page = $state(1)
  let loading = $state(true)
  let err = $state('')
  let tags = $state([]), correspondents = $state([]), types = $state([])
  let facetError = $state('')
  const fQuery = $derived(route.query.get('q') || '')
  const fTag = $derived(route.query.get('tags__id__in') || '')
  const fCorr = $derived(route.query.get('correspondents__id__in') || '')
  const fType = $derived(route.query.get('document_type__id') || '')
  const fSens = $derived(route.query.get('sensitivity') || '')
  const fDocumentIDs = $derived(route.query.get('document_ids') || '')
  const ordering = $derived(route.query.get('ordering') || '-created_at')
  let dateFrom = $state('')   // yyyy-mm-dd → created_at__gte (unix)
  let dateTo = $state('')
  let view = $state((() => { try { return localStorage.getItem('suchi.docs.view') || 'list' } catch { return 'list' } })())
  function setView(v) { view = v; try { localStorage.setItem('suchi.docs.view', v) } catch {} }
  const pageSize = 50
  const isInbox = $derived(inboxMode)
  const jdFilter = $derived(route.query.get('jd') || '')
  const canShareLinks = $derived(hasCapability(session.user, 'share_links'))
  const activeFilterKey = $derived(JSON.stringify([
    ordering, fQuery, fTag, fCorr, fType, fSens, fDocumentIDs, jdFilter,
    inbox?.id || '', taxonomyLoaded, dateFrom, dateTo,
  ]))
  let loadedFilterKey = ''
  let loadVersion = 0
  let activeController // cancel superseded filters, not only their UI updates
  let suggestions = $state([])
  const queryAssistant = createQueryAssistant((next) => (suggestions = next))
  onDestroy(() => {
    loadVersion++
    queryAssistant.dispose()
    activeController?.abort()
  })

  function setRouteFilter(key, value) {
    queryAssistant.clear()
    const params = new URLSearchParams(route.query)
    const normalized = String(value ?? '').trim()
    if (normalized) params.set(key, normalized)
    else params.delete(key)
    page = 1
    const query = params.toString()
    go(`#/documents${query ? `?${query}` : ''}`)
  }

  async function loadFacets() {
    facetError = ''
    try {
      const [t, c, d] = await Promise.all([listTags(), listCorrespondents(), listDocumentTypes()])
      tags = t?.results || []; correspondents = c?.results || []; types = d?.results || []
    } catch (ex) { facetError = ex.message || 'Some document filters could not be loaded.' }
  }

  async function load({ background = false } = {}) {
    const version = ++loadVersion
    activeController?.abort()
    activeController = undefined
    if (!background) loading = true
    err = ''
    if (isInbox && !inbox?.id) {
      docs = []
      count = 0
      loading = !taxonomyLoaded
      if (taxonomyLoaded) err = 'The inbox is unavailable.'
      return
    }
    const controller = new AbortController()
    activeController = controller
    try {
      const params = {
        page, page_size: pageSize, ordering,
        q: fQuery, document_ids: fDocumentIDs,
        tags__id__in: fTag, correspondents__id__in: fCorr,
        document_type__id: fType, sensitivity: fSens,
        jd_category_id: isInbox ? inbox?.id : jdFilter,
        created_at__gte: dateFrom ? Math.floor(new Date(dateFrom) / 1000) : '',
        created_at__lte: dateTo ? Math.floor(new Date(dateTo) / 1000) + 86399 : '',
      }
      const csvIDs = (value) => [...new Set(String(value || '').split(',')
        .map(item => Number(item.trim())).filter(item => Number.isInteger(item) && item > 0))]
      onScopeChange?.({
        label: isInbox ? 'Current inbox view' : 'Current document view',
        query: params.q || '',
        document_ids: csvIDs(params.document_ids),
        jd_category_id: Number(params.jd_category_id) || 0,
        sensitivity: params.sensitivity || '',
        document_type_id: Number(params.document_type__id) || 0,
        tag_ids: csvIDs(params.tags__id__in),
        correspondent_ids: csvIDs(params.correspondents__id__in),
        created_at_gte: params.created_at__gte === '' ? null : params.created_at__gte,
        created_at_lte: params.created_at__lte === '' ? null : params.created_at__lte,
        language: '',
      })
      const res = await listDocuments(params, controller.signal)
      if (version !== loadVersion) return
      docs = res?.results || []
      count = res?.count ?? docs.length
      const visibleIDs = new Set(docs.map((document) => document.id))
      sel = new Set([...sel].filter((id) => visibleIDs.has(id)))
    } catch (ex) {
      if (version === loadVersion) err = ex.message || 'Could not load documents.'
    } finally {
      if (activeController === controller) activeController = undefined
      if (version === loadVersion) loading = false
    }
  }

  async function fileTo(doc, jdId) {
    try {
      await patchDocument(doc.id, { jd_category_id: Number(jdId) })
      if (isInbox) { docs = docs.filter(d => d.id !== doc.id); count = Math.max(0, count - 1) }
      else load()
      notify?.('Filed')
    } catch (ex) { notify?.(ex.message || 'Could not file it') }
  }
  let trashRequest = $state(null)
  let trashBusy = $state(false)

  async function confirmTrash() {
    const request = trashRequest
    if (!request) return
    trashBusy = true
    if (request.kind === 'bulk') {
      await bulk('Trashed', 'delete', {})
      trashRequest = null
    } else {
      try {
        await deleteDocument(request.doc.id)
        docs = docs.filter(d => d.id !== request.doc.id)
        count = Math.max(0, count - 1)
        trashRequest = null
        notify?.('Trashed')
      } catch (ex) { notify?.(ex.message || 'Could not trash it') }
    }
    trashBusy = false
  }

  // ---- selection + bulk actions ----
  let sel = $state(new Set())
  let lastIdx = $state(-1)      // anchor for shift-range and j/k cursor
  let bulkBusy = $state(false)

  function toggleSel(i, ev) {
    const d = docs[i]
    if (!d) return
    const next = new Set(sel)
    if (ev?.shiftKey && lastIdx >= 0) {
      const [a, b] = [Math.min(lastIdx, i), Math.max(lastIdx, i)]
      for (let k = a; k <= b; k++) next.add(docs[k].id)
    } else {
      next.has(d.id) ? next.delete(d.id) : next.add(d.id)
    }
    sel = next
    lastIdx = i
  }
  function clearSel() { sel = new Set(); lastIdx = -1 }

  async function bulk(label, method, parameters) {
    bulkBusy = true
    try {
      const res = await bulkEdit([...sel], method, parameters)
      const failed = (res?.results || []).filter(r => !r.ok).length
      notify?.(failed ? `${label}: ${res.applied} done, ${failed} failed` : `${label}: ${res?.applied ?? sel.size} document${sel.size === 1 ? '' : 's'}`)
    } catch (ex) { notify?.(ex.message || `${label} failed`) }
    bulkBusy = false
    clearSel()
    load()
  }
  const bulkRefile = (jdId) => bulk('Refiled', 'set_jd_category', { jd_category_id: Number(jdId) })
  const bulkSens = (s) => bulk('Sensitivity set', 'set_sensitivity', { sensitivity: s })
  const bulkRescan = () => bulk('Rescan enqueued', 'rescan_enqueue', {})
  const bulkTrash = () => (trashRequest = { kind: 'bulk', count: sel.size })
  async function bulkShare() {
    bulkBusy = true
    try {
      const res = await createShareLink({ doc_ids: [...sel], label: `Selection of ${sel.size}` })
      const url = res?.public_url || `${location.origin}/s/${res?.token}`
      await navigator.clipboard?.writeText(url)
      notify?.('Share link for the selection copied')
    } catch (ex) { notify?.(ex.message || 'Could not create the bundle') }
    bulkBusy = false
  }

  async function bulkExtractDates() {
    const ids = [...sel]
    bulkBusy = true
    try {
      const result = await extractIntelligence({ document_ids: ids, types: ['date'] })
      const failed = (result?.results || []).filter(item => !item.ok).length
      notify?.(failed
        ? `Date extraction queued for ${result?.applied || 0}; ${failed} could not be accessed`
        : `Date extraction queued for ${result?.applied || ids.length} document${ids.length === 1 ? '' : 's'}`)
      clearSel()
    } catch (ex) {
      notify?.(ex.message || 'Could not queue date extraction')
    } finally {
      bulkBusy = false
    }
  }

  // The postingest retry fills metadata after this optimistic unlock state.
  let unlockPw = $state({})           // { [id]: string }
  let unlockErr = $state({})          // { [id]: string }
  let unlockBusy = $state(new Set())  // ids in flight
  async function unlockOne(d) {
    const password = (unlockPw[d.id] || '').trim()
    if (!password) return
    unlockBusy = new Set([...unlockBusy, d.id])
    unlockErr = { ...unlockErr, [d.id]: '' }
    try {
      await decryptDocument(d.id, { password, remember: true })
      d.encryption_state = 'decrypted'
      unlockPw = { ...unlockPw, [d.id]: '' }
      notify?.(`Unlocked "${d.title || 'document #' + d.id}"`)
    } catch (ex) {
      const code = ex?.code || ''
      unlockErr = { ...unlockErr, [d.id]: code === 'bad_password' ? 'wrong password' : (ex.message || 'unlock failed') }
    } finally {
      const next = new Set(unlockBusy); next.delete(d.id); unlockBusy = next
    }
  }

  let bulkDecOpen = $state(false)
  let bulkDecPw = $state('')
  let bulkDecBusy = $state(false)
  let bulkDecInput = $state()
  $effect(() => { if (bulkDecOpen && bulkDecInput) bulkDecInput.focus() })
  const anyLockedSelected = $derived(
    [...sel].some(id => docs.find(d => d.id === id)?.encryption_state === 'encrypted')
  )
  async function runBulkDecrypt() {
    const password = bulkDecPw.trim()
    if (!password) return
    bulkDecBusy = true
    try {
      const res = await decryptBatch({ password, remember: true, doc_ids: [...sel] })
      const results = res?.results || []
      const opened = results.filter(r => r.ok).length
      const tried = results.length || sel.size
      notify?.(opened ? `Unlocked ${opened} of ${tried}` : 'That password opened nothing')
      bulkDecPw = ''
      bulkDecOpen = false
      clearSel()
      load()
    } catch (ex) {
      notify?.(ex.message || 'Batch unlock failed')
    } finally { bulkDecBusy = false }
  }

  // ---- keyboard: j/k move, x select, Enter open ----
  function onKey(e) {
    // Handle modal Escape before ignoring focused inputs.
    if (bulkDecOpen && e.key === 'Escape') { bulkDecOpen = false; return }
    if (e.target.closest('input,select,textarea') || e.metaKey || e.ctrlKey) return
    if (e.key === 'j' || e.key === 'k') {
      e.preventDefault()
      lastIdx = Math.min(Math.max(lastIdx + (e.key === 'j' ? 1 : -1), 0), docs.length - 1)
      document.querySelector(`[data-row="${lastIdx}"]`)?.scrollIntoView({ block: 'nearest' })
    } else if (e.key === 'x' && lastIdx >= 0) {
      e.preventDefault(); toggleSel(lastIdx)
    } else if (e.key === 'Enter' && lastIdx >= 0 && docs[lastIdx]) {
      go(`#/doc/${docs[lastIdx].id}`)
    }
  }

  loadFacets()
  $effect(() => {
    const key = activeFilterKey
    uploadBus.revision
    const filterChanged = loadedFilterKey && loadedFilterKey !== key
    if (filterChanged) clearSel()
    if (filterChanged && page !== 1) {
      loadedFilterKey = key
      page = 1
      return
    }
    loadedFilterKey = key
    page
    load()
  })
  $effect(() => {
    const fn = () => { if (document.visibilityState === 'visible') load({ background: true }) }
    document.addEventListener('visibilitychange', fn)
    return () => document.removeEventListener('visibilitychange', fn)
  })

  const pages = $derived(Math.max(1, Math.ceil(count / pageSize)))
</script>

<svelte:window onkeydown={onKey} />

{#if sel.size > 0}
  <div class="bulkbar">
    <b>{sel.size} selected</b>
    <select class="input" style="max-width:190px" disabled={bulkBusy}
            onchange={(e) => e.target.value && bulkRefile(e.target.value)}>
      <option value="">File under…</option>
      {#each jdCategories as c}<option value={c.id}>{c.code} {c.name}</option>{/each}
    </select>
    <select class="input" style="max-width:160px" disabled={bulkBusy}
            onchange={(e) => bulkSens(e.target.value)}>
      <option value="" selected disabled>Sensitivity…</option>
      {#each SENSITIVITY_OPTIONS as option (option.value)}
        <option value={option.value}>{option.label}</option>
      {/each}
    </select>
    {#if canAskArchive}
      <button class="btn sm" disabled={bulkBusy} onclick={() => onAskDocuments?.([...sel])}>
        <Icon name="ask" size={12} /> Ask selection
      </button>
    {/if}
    {#if canReviewIntelligence}
      <button class="btn sm" disabled={bulkBusy} onclick={bulkExtractDates} title="Extract dates for review using the configured model">
        <Icon name="calendar" size={12} /> Extract dates
      </button>
    {/if}
    {#if canShareLinks}
      <button class="btn sm" disabled={bulkBusy} onclick={bulkShare}><Icon name="link" size={12} /> Share</button>
    {/if}
    {#if anyLockedSelected}
      <button class="btn sm" disabled={bulkBusy} onclick={() => (bulkDecOpen = true)}
              title="Try one password against every encrypted doc in the selection">
        <Icon name="lock" size={12} /> Decrypt…
      </button>
    {/if}
    <button class="btn sm danger" disabled={bulkBusy} onclick={bulkTrash}>Trash</button>
    <span class="spacer"></span>
    {#if bulkBusy}<span class="sub">working…</span>{/if}
    <button class="btn sm" disabled={bulkBusy} onclick={bulkRescan} title="Re-run the extraction pipeline on the selected documents">
      <Icon name="zap" size={12} /> Rescan
    </button>
    <button class="btn sm" onclick={clearSel}>Clear</button>
  </div>
{/if}

{#if err}<div class="err">{err}</div>{/if}

{#if !isInbox}
  <div class="toolbar" onchangecapture={(e) => { if (e.target.matches('select, input[type="date"]')) e.target.blur() }}>
    <input class="input" type="search" value={fQuery} placeholder="Search or use jd:, tag:, from:…"
           list="documents-query-suggestions"
           oninput={(event) => queryAssistant.update(event.currentTarget.value)}
           onchange={(e) => setRouteFilter('q', e.target.value)}
           onkeydown={(e) => { if (e.key === 'Enter') { e.preventDefault(); setRouteFilter('q', e.currentTarget.value) } }}
           style="min-width:180px" />
    <datalist id="documents-query-suggestions">
      {#each suggestions as suggestion (suggestion.query)}
        <option value={suggestion.query}>{suggestion.value}</option>
      {/each}
    </datalist>
    <select class="input" value={fTag} onchange={(e) => setRouteFilter('tags__id__in', e.target.value)}>
      <option value="">All tags</option>
      {#each tags as t}<option value={t.id}>{t.name}</option>{/each}
    </select>
    <select class="input" value={fCorr} onchange={(e) => setRouteFilter('correspondents__id__in', e.target.value)}>
      <option value="">All correspondents</option>
      {#each correspondents as c}<option value={c.id}>{c.name}</option>{/each}
    </select>
    <select class="input" value={fType} onchange={(e) => setRouteFilter('document_type__id', e.target.value)}>
      <option value="">All types</option>
      {#each types as t}<option value={t.id}>{t.name}</option>{/each}
    </select>
    <select class="input" value={fSens} onchange={(e) => setRouteFilter('sensitivity', e.target.value)}>
      <option value="">Any sensitivity</option>
      {#each SENSITIVITY_OPTIONS as option (option.value)}
        <option value={option.value}>{option.label}</option>
      {/each}
    </select>
    <span class="document-date-filter">
      <input class="input" type="date" bind:value={dateFrom} title="Added on or after"
             aria-label="Added on or after" placeholder="dd/mm/yyyy" />
      {#if !dateFrom}<span class="date-format-hint" aria-hidden="true">dd/mm/yyyy</span>{/if}
    </span>
    <span class="document-date-filter">
      <input class="input" type="date" bind:value={dateTo} title="Added on or before"
             aria-label="Added on or before" placeholder="dd/mm/yyyy" />
      {#if !dateTo}<span class="date-format-hint" aria-hidden="true">dd/mm/yyyy</span>{/if}
    </span>
    <span class="spacer"></span>
    <span class="seg">
      <button class:on={view === 'list'} onclick={() => setView('list')}>List</button>
      <button class:on={view === 'grid'} onclick={() => setView('grid')}>Grid</button>
    </span>
    <button class="btn sm" onclick={load} title="Refresh" aria-label="Refresh documents"><Icon name="refresh" size={13} /></button>
    <a class="btn sm" href="#/trash" title="Trash" aria-label="Open trash"><Icon name="trash" size={13} /></a>
    <select class="input" value={ordering} onchange={(e) => setRouteFilter('ordering', e.target.value === '-created_at' ? '' : e.target.value)}>
      <option value="-created_at">Newest first</option>
      <option value="created_at">Oldest first</option>
      <option value="title">Title A–Z</option>
      <option value="-title">Title Z–A</option>
    </select>
  </div>
  {#if facetError}
    <div class="err" style="display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:12px">
      <span>{facetError}</span><button class="btn sm" onclick={loadFacets}>Retry filters</button>
    </div>
  {/if}
{/if}

{#if loading}
  <div class="index">{#each Array(6) as _}<div class="irow"><div class="skel" style="width:60%"></div></div>{/each}</div>
{:else if docs.length === 0}
  <div class="empty">
    <Icon name={isInbox ? 'inbox' : 'docs'} size={56} />
    {#if isInbox}
      <b>Inbox zero.</b><span>Everything is filed. New low-confidence documents will wait here.</span>
    {:else}
      <b>No documents match.</b><span>Clear a filter, or <a href="#/upload">upload the first one</a>.</span>
    {/if}
  </div>
{:else}
  {#if view === 'grid' && !isInbox}
    <div class="dgrid">
      {#each docs as d, i (d.id)}
        <a class="card gcard" href={`#/doc/${d.id}`} class:selected={sel.has(d.id)}>
          <span class="gthumb" class:blurred={isHighSensitivity(d.sensitivity)}><img src={thumbPath(d.id, isHighSensitivity(d.sensitivity))} alt="" loading="lazy" onerror={(e) => e.target.closest('.gthumb').classList.add('none')} /></span>
          <span class="gmeta">
            <input type="checkbox" class="rowcheck" checked={sel.has(d.id)}
                   onclick={(e) => e.stopPropagation()}
                   onchange={(e) => toggleSel(i, e)} aria-label="Select" />
            {#if d.jd_category_code}<span class="chip">{d.jd_category_code}</span>{/if}
            <span class="title">{d.title || `Document #${d.id}`}</span>
          </span>
          <span class="sub" style="padding:0 12px 10px">{fmtDate(d.created_at)}</span>
        </a>
      {/each}
    </div>
  {:else}
  <div class="index">
    {#each docs as d, i (d.id)}
      <a class="irow hoverable" href={`#/doc/${d.id}`} data-row={i} class:cursor={i === lastIdx} class:selected={sel.has(d.id)}>
        <input type="checkbox" class="rowcheck" checked={sel.has(d.id)}
               onclick={(e) => e.stopPropagation()}
               onchange={(e) => toggleSel(i, e)}
               aria-label={`Select ${d.title || 'document ' + d.id}`} />
        <span class="rthumb" class:blurred={isHighSensitivity(d.sensitivity)}><img src={thumbPath(d.id, isHighSensitivity(d.sensitivity))} alt="" loading="lazy" onerror={(e) => e.target.closest('.rthumb').classList.add('none')} /></span>
        <span class="dot {sensDot(d.sensitivity)}" class:accent={!d.sensitivity}></span>
        {#if d.jd_category_code}<span class="chip" title={`${d.jd_category_name} · ${d.jd_area_name}`}>{d.jd_category_code}</span>{/if}
        <span class="title grow">{d.title || `Document #${d.id}`}</span>
        {#if d.tags?.length}
          {#each d.tags.slice(0, 3) as t}<span class="pill">{t}</span>{/each}
        {/if}
        <span class="end">
          {#if d.encryption_state === 'encrypted'}
            <span class="unlock-inline" role="group" aria-label="Unlock this document">
              <span class="pill warn" title="This doc is encrypted; unlock to make it searchable and previewable">
                <Icon name="lock" size={11} /> Locked
              </span>
              <input type="password" class="input" style="padding:3px 8px;font-size:.76rem;max-width:160px"
                     placeholder="password"
                     bind:value={unlockPw[d.id]}
                     disabled={unlockBusy.has(d.id)}
                     onclick={(e) => e.preventDefault()}
                     onkeydown={(e) => { if (e.key === 'Enter') { e.preventDefault(); unlockOne(d) } }}
                     aria-label={`Password for ${d.title || 'document ' + d.id}`} />
              <button class="btn sm primary" disabled={unlockBusy.has(d.id) || !unlockPw[d.id]}
                      onclick={(e) => { e.preventDefault(); e.stopPropagation(); unlockOne(d) }}>
                {unlockBusy.has(d.id) ? '…' : 'Unlock'}
              </button>
              {#if unlockErr[d.id]}<span class="sub" style="color:var(--danger)">{unlockErr[d.id]}</span>{/if}
            </span>
          {:else if isInbox && jdCategories.length}
            <select class="input" style="padding:3px 8px;font-size:.76rem;max-width:150px"
                    onclick={(e) => e.preventDefault()}
                    onchange={(e) => { e.preventDefault(); if (e.target.value) fileTo(d, e.target.value) }}>
              <option value="">File under…</option>
              {#each jdCategories as c}<option value={c.id}>{c.code} {c.name}</option>{/each}
            </select>
          {:else}
            <span class="hoveracts" role="group" aria-label="Quick actions">
              <select class="input" style="padding:2px 6px;font-size:.72rem;max-width:130px"
                      onclick={(e) => e.preventDefault()}
                      onchange={(e) => { e.preventDefault(); if (e.target.value) fileTo(d, e.target.value) }}>
                <option value="">Refile…</option>
                {#each jdCategories as c}<option value={c.id}>{c.code} {c.name}</option>{/each}
              </select>
              <button class="btn sm danger" title="Trash" aria-label={`Trash ${d.title || `document ${d.id}`}`}
                      onclick={(e) => { e.preventDefault(); e.stopPropagation(); trashRequest = { kind: 'one', doc: d } }}>
                <Icon name="trash" size={12} /></button>
            </span>
          {/if}
          <span class="sub">{fmtDate(d.created_at)}</span>
        </span>
      </a>
    {/each}
  </div>
  {/if}
  {#if pages > 1}
    <div class="pager">
      <button class="btn sm" disabled={page <= 1} onclick={() => page--}>‹ Prev</button>
      <span>page {page} of {pages} · {count} documents</span>
      <button class="btn sm" disabled={page >= pages} onclick={() => page++}>Next ›</button>
    </div>
  {/if}
{/if}

{#if trashRequest}
  <ConfirmDialog
    title={trashRequest.kind === 'bulk'
      ? `Move ${trashRequest.count} document${trashRequest.count === 1 ? '' : 's'} to trash?`
      : 'Move document to trash?'}
    message={trashRequest.kind === 'bulk'
      ? `The selected document${trashRequest.count === 1 ? '' : 's'} will move to Trash, where ${trashRequest.count === 1 ? 'it can' : 'they can'} be restored.`
      : `“${trashRequest.doc.title || `Document #${trashRequest.doc.id}`}” will move to Trash, where it can be restored.`}
    confirmLabel="Move to trash"
    busy={trashBusy}
    onConfirm={confirmTrash}
    onCancel={() => (trashRequest = null)} />
{/if}

{#if bulkDecOpen}
  <div class="modal-veil"
       onclick={() => (bulkDecOpen = false)}
       onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); bulkDecOpen = false } }}
       role="button" tabindex="-1" aria-label="Close bulk-decrypt dialog">
    <div class="modal" style="width:min(420px,94vw)"
         onclick={(e) => e.stopPropagation()}
         onkeydown={(e) => e.stopPropagation()}
         role="dialog" aria-modal="true" aria-label="Bulk unlock" tabindex="-1">
      <div class="modal-head">
        <h3>Try one password against {sel.size} document{sel.size === 1 ? '' : 's'}</h3>
        <button class="btn sm" onclick={() => (bulkDecOpen = false)} title="Close" aria-label="Close bulk unlock"><Icon name="x" size={13} /></button>
      </div>
      <p class="sub" style="margin:0 0 10px">
        The server tries this password against every selected document that's still encrypted.
        Matches unlock in place and land in the vault so the next upload with the same password
        auto-decrypts.
      </p>
      <div class="field">
        <label for="bulkdec-pw">Password</label>
        <input id="bulkdec-pw" type="password" class="input" bind:value={bulkDecPw}
               bind:this={bulkDecInput} disabled={bulkDecBusy}
               onkeydown={(e) => { if (e.key === 'Enter') runBulkDecrypt() }} />
      </div>
      <div class="toolbar" style="margin-top:8px">
        <button class="btn primary sm" disabled={bulkDecBusy || !bulkDecPw.trim()} onclick={runBulkDecrypt}>
          {bulkDecBusy ? 'Trying…' : 'Try password'}
        </button>
        <button class="btn sm" disabled={bulkDecBusy} onclick={() => (bulkDecOpen = false)}>Cancel</button>
      </div>
    </div>
  </div>
{/if}
