<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "../lib/i18n.svelte.js";
  import { scopedHash as filingHref } from '../lib/systems.svelte.js'
  import { onDestroy, untrack } from 'svelte'
  import { search, listLanguages, resolveAddress } from '../lib/api.js'
  import { isCanonicalFilingAddress, resolvedFilingAddressHash } from '../lib/filing-address.js'
  import { route, go } from '../lib/router.svelte.js'
  import { fmtDate, plural } from '../lib/format.js'
  import { addDays, localISODate } from '../lib/intelligence.js'
  import Icon from '../lib/Icon.svelte'
  import { createQueryAssistant } from '../lib/queryAssist.js'

  let { onScopeChange } = $props()

  const urlQuery = $derived(route.query.get('q') || '')
  const urlLanguage = $derived(route.query.get('lang') || '')
  let q = $state('')
  let lang = $state('')
  let hits = $state([])
  let count = $state(0)
  let page = $state(1)
  let loading = $state(false)
  let err = $state('')
  let searched = $state(false)
  let languages = $state([])
  let runVersion = 0
  let activeController // cancel superseded searches, not only their UI updates
  let suggestions = $state([])
  const queryAssistant = createQueryAssistant((next) => (suggestions = next))
  onDestroy(() => {
    runVersion++
    queryAssistant.dispose()
    activeController?.abort()
  })

  function publishEmptyScope() {
    onScopeChange?.({
      label: lang ? translate("search.currentSearchResults") : translate("search.allDocuments"), query: '', document_ids: [], jd_category_id: 0,
      sensitivity: '', tag_ids: [], correspondent_ids: [],
      created_at_gte: null, created_at_lte: null, language: lang,
    })
  }

  // Load the language facet once on mount so the filter chips have
  // observed codes + counts to render.
  ;(async () => {
    try {
      const res = await listLanguages()
      languages = res?.languages || []
    } catch { /* facet is optional */ }
  })()

  async function run() {
    const version = ++runVersion
    activeController?.abort()
    activeController = undefined
    loading = false
    err = ''
    const query = q.trim()
    if (!query) {
      hits = []; count = 0; searched = false
      publishEmptyScope()
      return
    }
    const requestPage = page
    const requestLang = lang
    const controller = new AbortController()
    activeController = controller
    loading = true; searched = true
    try {
      if (isCanonicalFilingAddress(query)) {
        // A missing category is ordinary text, such as the price 10.15.
        const result = await resolveAddress(query).catch(ex => { if (ex?.status === 404) return null; throw ex })
        if (version !== runVersion) return
        if (result) {
          go(resolvedFilingAddressHash(result))
          return
        }
      }
      const params = { page: requestPage, page_size: 25 }
      if (requestLang) params.lang = requestLang
      onScopeChange?.({
        label: translate("search.currentSearchResults"),
        query,
        document_ids: [], jd_category_id: 0, sensitivity: '',
        tag_ids: [], correspondent_ids: [], created_at_gte: null, created_at_lte: null,
        language: requestLang,
      })
      const res = await search(query, params, controller.signal)
      if (version !== runVersion) return
      hits = res?.results || []
      count = res?.count ?? hits.length
    } catch (ex) {
      if (version === runVersion) {
        err = ex.message || translate("search.searchFailed")
        hits = []
        count = 0
        searched = false
      }
    } finally {
      if (activeController === controller) activeController = undefined
      if (version === runVersion) loading = false
    }
  }

  // Two rich queries shown before the first search; each runs when chosen.
  const today = localISODate()
  const examples = [
    { query: `is:inbox added:>=${addDays(today, -30)}`, label: translate("search.exampleRecentInbox") },
    { query: `date-role:expiry date:>=${today}`, label: translate("search.exampleUpcomingExpiry") },
  ]

  function runExample(query) {
    q = query
    page = 1
    navigateToQuery()
  }

  function navigateToQuery() {
    const params = new URLSearchParams()
    if (q.trim()) params.set('q', q.trim())
    if (lang) params.set('lang', lang)
    const query = params.toString()
    const hash = `#/search${query ? `?${query}` : ''}`
    if (location.hash === filingHref(hash)) run()
    else go(hash)
  }

  function setLang(code) {
    lang = code === lang ? '' : code
    page = 1
    navigateToQuery()
  }

  function safeSnippet(t) {
    const esc = t.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    return esc.replaceAll('&lt;mark&gt;', '<mark>').replaceAll('&lt;/mark&gt;', '</mark>')
  }

  function submit(e) {
    e.preventDefault(); page = 1
    navigateToQuery()
  }
  $effect(() => {
    const query = urlQuery
    const language = urlLanguage
    // Only applied URL values trigger a search; typing and pagination do not.
    untrack(() => {
      q = query
      lang = language
      queryAssistant.clear()
      page = 1
      run()
    })
  })
  const pages = $derived(Math.max(1, Math.ceil(count / 25)))
</script>

<div class="content-narrow">
  <form class="toolbar" onsubmit={submit}>
    <input class="input" style="flex:1" placeholder={translate("search.searchTextOrUseJdTagFrom")}
           bind:value={q} list="search-query-suggestions" oninput={(event) => queryAssistant.update(event.currentTarget.value)} />
    <button class="btn primary">{translate("search.search")}</button>
  </form>
  <datalist id="search-query-suggestions">
    {#each suggestions as suggestion (suggestion.query)}
      <option value={suggestion.query}>{suggestion.value}</option>
    {/each}
  </datalist>

  {#if languages.length}
    <div style="display:flex;flex-wrap:wrap;gap:6px;margin:6px 0 12px;align-items:center">
      <span class="sub">{translate("search.language")}</span>
      <button class="pill" class:accent={!lang} type="button" onclick={() => setLang('')}>{translate("search.all")}</button>
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
    <div class="empty"><Icon name="search" size={56} /><b>{translate("search.nothingMatched")}</b><span>{translate("search.tryFewerWords")}</span></div>
  {:else if !searched && !q.trim()}
    <div class="search-examples">
      <span class="search-examples-label">{translate("search.tryAQuery")}</span>
      {#each examples as example (example.query)}
        <button class="search-example" type="button" onclick={() => runExample(example.query)}>
          <code>{example.query}</code><span>{example.label}</span>
        </button>
      {/each}
    </div>
  {:else if hits.length}
    <p class="sub" style="color:var(--muted);margin:0 0 10px">{plural("search.resultCount", count)}</p>
    <div class="index">
      {#each hits as h (h.id)}
        <a class="irow filing-row" href={filingHref(`#/doc/${h.id}`)} style="align-items:flex-start">
          <span class="dot accent" style="margin-top:7px"></span>
          <span class="filing-code filing-code-column" style="margin-top:2px">{h.jd_address || h.jd_category_code || ''}</span>
          <span class="grow">
            <span class="title" style="display:block">{h.title || translate("search.documentValue", { "id": h.id })}</span>
            {#if h.archive_serial_number != null}<span class="chip">{translate("common.asnValue", { value: String(h.archive_serial_number) })}</span>{/if}
            {#if h.snippet}<span class="sub" style="white-space:normal">{@html safeSnippet(h.snippet)}</span>{/if}
          </span>
          <span class="sub">{fmtDate(h.created_at)}</span>
        </a>
      {/each}
    </div>
    {#if pages > 1}
      <div class="pager">
        <button class="btn sm" disabled={page <= 1} onclick={() => { page--; run() }}>{translate("search.prev")}</button>
        <span>{translate("search.pageValueOfValue", { "page": page, "pages": pages })}</span>
        <button class="btn sm" disabled={page >= pages} onclick={() => { page++; run() }}>{translate("search.next")}</button>
      </div>
    {/if}
  {/if}
</div>
