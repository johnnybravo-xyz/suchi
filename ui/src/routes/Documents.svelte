<script>
  import { listDocuments, listTags, listCorrespondents, listDocumentTypes, patchDocument, deleteDocument, listJDCategories } from '../lib/api.js'
  import { route } from '../lib/router.svelte.js'
  import { fmtDate, sensDot } from '../lib/format.js'
  import Icon from '../lib/Icon.svelte'

  let { notify, inbox = null } = $props()

  let docs = $state([])
  let count = $state(0)
  let page = $state(1)
  let loading = $state(true)
  let err = $state('')
  let tags = $state([]), correspondents = $state([]), types = $state([])
  let fTag = $state(''), fCorr = $state(''), fType = $state(''), fSens = $state('')
  let ordering = $state('-created_at')
  const pageSize = 50
  const isInbox = $derived(inbox != null)
  const jdFilter = $derived(route.query.get('jd') || '')

  async function loadFacets() {
    try {
      const [t, c, d] = await Promise.all([listTags(), listCorrespondents(), listDocumentTypes()])
      tags = t?.results || []; correspondents = c?.results || []; types = d?.results || []
    } catch {}
  }

  async function load() {
    loading = true; err = ''
    try {
      const params = {
        page, page_size: pageSize, ordering,
        tags__id__in: fTag, correspondents__id__in: fCorr,
        document_type__id: fType, sensitivity: fSens,
        jd_category_id: isInbox ? inbox?.id : jdFilter,
      }
      const res = await listDocuments(params)
      docs = res?.results || []
      count = res?.count ?? docs.length
    } catch (ex) { err = ex.message || 'Could not load documents.' }
    finally { loading = false }
  }

  async function fileTo(doc, jdId) {
    try {
      await patchDocument(doc.id, { jd_category_id: Number(jdId) })
      docs = docs.filter(d => d.id !== doc.id)
      count = Math.max(0, count - 1)
      notify?.('Filed')
    } catch (ex) { notify?.(ex.message || 'Could not file it') }
  }

  let jdCats = $state([])
  async function loadJDCats() {
    try { jdCats = (await listJDCategories())?.results || [] } catch {}
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

  async function bulk(label, fn) {
    bulkBusy = true
    let ok = 0, fail = 0
    for (const id of sel) {
      try { await fn(id); ok++ } catch { fail++ }
    }
    bulkBusy = false
    notify?.(fail ? `${label}: ${ok} done, ${fail} failed` : `${label}: ${ok} document${ok === 1 ? '' : 's'}`)
    clearSel()
    load()
  }
  const bulkRefile = (jdId) => bulk('Refiled', (id) => patchDocument(id, { jd_category_id: Number(jdId) }))
  const bulkSens = (s) => bulk('Sensitivity set', (id) => patchDocument(id, { sensitivity: s || null }))
  const bulkTrash = () => confirm(`Move ${sel.size} document${sel.size === 1 ? '' : 's'} to trash?`) &&
    bulk('Trashed', (id) => deleteDocument(id))

  // ---- keyboard: j/k move, x select, Enter open ----
  function onKey(e) {
    if (e.target.closest('input,select,textarea') || e.metaKey || e.ctrlKey) return
    if (e.key === 'j' || e.key === 'k') {
      e.preventDefault()
      lastIdx = Math.min(Math.max(lastIdx + (e.key === 'j' ? 1 : -1), 0), docs.length - 1)
      document.querySelector(`[data-row="${lastIdx}"]`)?.scrollIntoView({ block: 'nearest' })
    } else if (e.key === 'x' && lastIdx >= 0) {
      e.preventDefault(); toggleSel(lastIdx)
    } else if (e.key === 'Enter' && lastIdx >= 0 && docs[lastIdx]) {
      location.hash = `#/doc/${docs[lastIdx].id}`
    }
  }

  loadFacets()
  loadJDCats()
  $effect(() => { page; ordering; fTag; fCorr; fType; fSens; jdFilter; inbox; load() })

  const pages = $derived(Math.max(1, Math.ceil(count / pageSize)))
</script>

<svelte:window onkeydown={onKey} />

{#if sel.size > 0}
  <div class="bulkbar">
    <b>{sel.size} selected</b>
    <select class="input" style="max-width:190px" disabled={bulkBusy}
            onchange={(e) => e.target.value && bulkRefile(e.target.value)}>
      <option value="">File under…</option>
      {#each jdCats as c}<option value={c.id}>{c.code} {c.name}</option>{/each}
    </select>
    <select class="input" style="max-width:160px" disabled={bulkBusy}
            onchange={(e) => bulkSens(e.target.value)}>
      <option value="" selected disabled>Sensitivity…</option>
      <option value="public">Public</option>
      <option value="internal">Internal</option>
      <option value="confidential">Confidential</option>
    </select>
    <button class="btn sm danger" disabled={bulkBusy} onclick={bulkTrash}>Trash</button>
    <span class="spacer"></span>
    {#if bulkBusy}<span class="sub">working…</span>{/if}
    <button class="btn sm" onclick={clearSel}>Clear</button>
  </div>
{/if}

{#if err}<div class="err">{err}</div>{/if}

{#if !isInbox}
  <div class="toolbar">
    <select class="input" bind:value={fTag}>
      <option value="">All tags</option>
      {#each tags as t}<option value={t.id}>{t.name}</option>{/each}
    </select>
    <select class="input" bind:value={fCorr}>
      <option value="">All correspondents</option>
      {#each correspondents as c}<option value={c.id}>{c.name}</option>{/each}
    </select>
    <select class="input" bind:value={fType}>
      <option value="">All types</option>
      {#each types as t}<option value={t.id}>{t.name}</option>{/each}
    </select>
    <select class="input" bind:value={fSens}>
      <option value="">Any sensitivity</option>
      <option value="public">Public</option>
      <option value="internal">Internal</option>
      <option value="confidential">Confidential</option>
    </select>
    <span class="spacer"></span>
    <a class="btn sm" href="#/trash" title="Trash"><Icon name="trash" size={13} /></a>
    <select class="input" bind:value={ordering}>
      <option value="-created_at">Newest first</option>
      <option value="created_at">Oldest first</option>
      <option value="title">Title A–Z</option>
      <option value="-title">Title Z–A</option>
    </select>
  </div>
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
  <div class="index">
    {#each docs as d, i (d.id)}
      <a class="irow" href={`#/doc/${d.id}`} data-row={i} class:cursor={i === lastIdx} class:selected={sel.has(d.id)}>
        <input type="checkbox" class="rowcheck" checked={sel.has(d.id)}
               onclick={(e) => { e.preventDefault(); e.stopPropagation(); toggleSel(i, e) }}
               aria-label={`Select ${d.title || 'document ' + d.id}`} />
        <span class="dot {sensDot(d.sensitivity)}" class:accent={!d.sensitivity}></span>
        {#if d.jd_category_code}<span class="chip" title={`${d.jd_category_name} · ${d.jd_area_name}`}>{d.jd_category_code}</span>{/if}
        <span class="title grow">{d.title || `Document #${d.id}`}</span>
        {#if d.tags?.length}
          {#each d.tags.slice(0, 3) as t}<span class="pill">{t}</span>{/each}
        {/if}
        <span class="end">
          {#if isInbox && jdCats.length}
            <select class="input" style="padding:3px 8px;font-size:.76rem;max-width:150px"
                    onclick={(e) => e.preventDefault()}
                    onchange={(e) => { e.preventDefault(); if (e.target.value) fileTo(d, e.target.value) }}>
              <option value="">File under…</option>
              {#each jdCats as c}<option value={c.id}>{c.code} {c.name}</option>{/each}
            </select>
          {/if}
          <span class="sub">{fmtDate(d.created_at)}</span>
        </span>
      </a>
    {/each}
  </div>
  {#if pages > 1}
    <div class="pager">
      <button class="btn sm" disabled={page <= 1} onclick={() => page--}>‹ Prev</button>
      <span>page {page} of {pages} · {count} documents</span>
      <button class="btn sm" disabled={page >= pages} onclick={() => page++}>Next ›</button>
    </div>
  {/if}
{/if}
