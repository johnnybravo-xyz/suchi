<script>
  import { listDocuments, listTags, listCorrespondents, listDocumentTypes, patchDocument, listJDCategories } from '../lib/api.js'
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

  loadFacets()
  if (isInbox) loadJDCats()
  $effect(() => { page; ordering; fTag; fCorr; fType; fSens; jdFilter; inbox; load() })

  const pages = $derived(Math.max(1, Math.ceil(count / pageSize)))
</script>

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
    {#each docs as d (d.id)}
      <a class="irow" href={`#/doc/${d.id}`}>
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
