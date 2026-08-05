<script>
  import { search } from '../lib/api.js'
  import { route, go } from '../lib/router.svelte.js'
  import { fmtDate } from '../lib/format.js'
  import Icon from '../lib/Icon.svelte'

  let q = $state(route.query.get('q') || '')
  let hits = $state([])
  let count = $state(0)
  let page = $state(1)
  let loading = $state(false)
  let err = $state('')
  let searched = $state(false)

  async function run() {
    const query = q.trim()
    if (!query) { hits = []; count = 0; searched = false; return }
    loading = true; err = ''; searched = true
    try {
      const res = await search(query, { page, page_size: 25 })
      hits = res?.results || []
      count = res?.count ?? hits.length
    } catch (ex) { err = ex.status === 400 ? 'That query has unbalanced quotes or operators.' : (ex.message || 'Search failed.') }
    finally { loading = false }
  }

  function safeSnippet(t) {
    const esc = t.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    return esc.replaceAll('&lt;mark&gt;', '<mark>').replaceAll('&lt;/mark&gt;', '</mark>')
  }

  function submit(e) { e.preventDefault(); page = 1; go(`#/search?q=${encodeURIComponent(q.trim())}`); run() }
  $effect(() => { const rq = route.query.get('q'); if (rq && rq !== q) { q = rq; page = 1; run() } })
  queueMicrotask(() => { if (q) run() })  // initial query from the URL
  const pages = $derived(Math.max(1, Math.ceil(count / 25)))
</script>

<div class="content-narrow">
  <form class="toolbar" onsubmit={submit}>
    <input class="input" style="flex:1" placeholder={'Search full text — supports "quoted phrases", AND, OR, jd:2*'}
           bind:value={q} />
    <button class="btn primary">Search</button>
  </form>

  {#if err}<div class="err">{err}</div>{/if}

  {#if loading}
    <div class="index">{#each Array(4) as _}<div class="irow"><div class="skel" style="width:70%"></div></div>{/each}</div>
  {:else if searched && hits.length === 0}
    <div class="empty"><Icon name="search" size={56} /><b>Nothing matched.</b><span>Try fewer words, or a <code>jd:</code> prefix.</span></div>
  {:else if hits.length}
    <p class="sub" style="color:var(--muted);margin:0 0 10px">{count} result{count === 1 ? '' : 's'}</p>
    <div class="index">
      {#each hits as h (h.id)}
        <a class="irow" href={`#/doc/${h.id}`} style="align-items:flex-start">
          <span class="dot accent" style="margin-top:7px"></span>
          <span class="grow">
            <span class="title" style="display:block">{h.title || `Document #${h.id}`}</span>
            {#if h.snippet}<span class="sub" style="white-space:normal">{@html safeSnippet(h.snippet)}</span>{/if}
          </span>
          <span class="sub">{fmtDate(h.created_at)}</span>
        </a>
      {/each}
    </div>
    {#if pages > 1}
      <div class="pager">
        <button class="btn sm" disabled={page <= 1} onclick={() => { page--; run() }}>‹ Prev</button>
        <span>page {page} of {pages}</span>
        <button class="btn sm" disabled={page >= pages} onclick={() => { page++; run() }}>Next ›</button>
      </div>
    {/if}
  {/if}
</div>
