<script>
  import { search, listLanguages } from '../lib/api.js'
  import { route, go } from '../lib/router.svelte.js'
  import { fmtDate } from '../lib/format.js'
  import Icon from '../lib/Icon.svelte'

  let q = $state(route.query.get('q') || '')
  let lang = $state(route.query.get('lang') || '')
  let hits = $state([])
  let count = $state(0)
  let page = $state(1)
  let loading = $state(false)
  let err = $state('')
  let searched = $state(false)
  let languages = $state([])

  // Load the language facet once on mount so the filter chips have
  // observed codes + counts to render. Cheap — one query per session.
  ;(async () => {
    try {
      const res = await listLanguages()
      languages = res?.languages || []
    } catch { /* facet is optional */ }
  })()

  async function run() {
    const query = q.trim()
    if (!query) { hits = []; count = 0; searched = false; return }
    loading = true; err = ''; searched = true
    try {
      const params = { page, page_size: 25 }
      if (lang) params.lang = lang
      const res = await search(query, params)
      hits = res?.results || []
      count = res?.count ?? hits.length
    } catch (ex) { err = ex.status === 400 ? 'That query has unbalanced quotes or operators.' : (ex.message || 'Search failed.') }
    finally { loading = false }
  }

  function setLang(code) {
    lang = code === lang ? '' : code
    page = 1
    const params = new URLSearchParams()
    if (q.trim()) params.set('q', q.trim())
    if (lang) params.set('lang', lang)
    go(`#/search?${params.toString()}`)
    if (q.trim()) run()
  }

  function safeSnippet(t) {
    const esc = t.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    return esc.replaceAll('&lt;mark&gt;', '<mark>').replaceAll('&lt;/mark&gt;', '</mark>')
  }

  function submit(e) {
    e.preventDefault(); page = 1
    const params = new URLSearchParams()
    if (q.trim()) params.set('q', q.trim())
    if (lang) params.set('lang', lang)
    go(`#/search?${params.toString()}`)
    run()
  }
  // Sync q FROM the URL when the URL changes — but never read q inside
  // this effect, or every keystroke would re-fire it and clobber the
  // user's typing. Locally-cached lastURLQ guards against re-running
  // the search on unrelated route changes.
  let lastURLQ = route.query.get('q') || ''
  let lastURLLang = route.query.get('lang') || ''
  $effect(() => {
    const rq = route.query.get('q') || ''
    const rl = route.query.get('lang') || ''
    if (rq !== lastURLQ || rl !== lastURLLang) {
      lastURLQ = rq
      lastURLLang = rl
      q = rq
      lang = rl
      page = 1
      if (rq) run()
      else { hits = []; count = 0; searched = false }
    }
  })
  queueMicrotask(() => { if (q) run() })  // initial query from the URL
  const pages = $derived(Math.max(1, Math.ceil(count / 25)))
</script>

<div class="content-narrow">
  <form class="toolbar" onsubmit={submit}>
    <input class="input" style="flex:1" placeholder={'Search full text — supports "quoted phrases", AND, OR, jd:2*'}
           bind:value={q} />
    <button class="btn primary">Search</button>
  </form>

  {#if languages.length}
    <div style="display:flex;flex-wrap:wrap;gap:6px;margin:6px 0 12px;align-items:center">
      <span class="sub">Language:</span>
      <button class="pill" class:accent={!lang} type="button" onclick={() => setLang('')}>all</button>
      {#each languages as l}
        <button class="pill" class:accent={lang === l.code} type="button" onclick={() => setLang(l.code)}>
          {l.code} <span class="sub">({l.count})</span>
        </button>
      {/each}
    </div>
  {/if}

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
