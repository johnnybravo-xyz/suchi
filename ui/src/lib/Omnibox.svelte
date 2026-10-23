<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "./i18n.svelte.js";
  // Combined command palette and query entry point.
  import { go } from './router.svelte.js'
  import { resolveAddress } from './api-core.js'
  import { isCanonicalFilingAddress, resolvedFilingAddressHash } from './filing-address.js'
  import { captureScope, scopeCurrent, scopedHash, systems } from './systems.svelte.js'
  import { session } from './session.svelte.js'
  import { recentDocuments } from './recentDocuments.js'
  import Icon from './Icon.svelte'

  let { pages = [], commands = [], canAsk = false, onAsk } = $props()
  let q = $state('')
  let open = $state(false)
  let idx = $state(-1)
  let error = $state('')
  let queryVersion = 0
  let box, input
  const appleShortcut = typeof navigator !== 'undefined' && /Mac|iPhone|iPad|iPod/.test(navigator.platform)
  const shortcutLabel = appleShortcut ? translate("omnibox.k") : translate("omnibox.ctrlK")
  const shortcutKeys = appleShortcut ? translate("omnibox.metaK") : translate("omnibox.controlK")

  // Documents and saved views load on demand; their API modules stay out of
  // the initial page.
  const DOCUMENT_LIMIT = 5
  const SEARCH_DELAY = 200
  let documentHits = $state([])
  let recent = $state([])
  let views = $state(null)              // [{ id, name, href }] once loaded
  let searchTimer

  function matches(label) {
    const s = q.trim().toLowerCase()
    return !s || label.toLowerCase().includes(s)
  }

  const query = $derived(q.trim())
  const documentRows = $derived((query ? documentHits : recent).map(doc => ({
    key: `doc-${doc.id}`, ico: 'docs', href: scopedHash(`#/doc/${doc.id}`),
    label: doc.title || translate("omnibox.documentValue", { id: doc.id }),
  })))
  const viewRows = $derived(query ? (views || []).filter(view => matches(view.name)).slice(0, 3)
    .map(view => ({ key: `view-${view.id}`, ico: 'eye', href: view.href, label: view.name })) : [])
  const commandRows = $derived(commands.filter(c => matches(c.label)).slice(0, 6)
    .map((c, i) => ({ key: `cmd-${i}`, ico: c.ico, run: c.run, href: c.href, label: c.label })))
  const pageRows = $derived(pages.filter(p => matches(p.label)).slice(0, query ? 3 : 5)
    .map(p => ({ key: `page-${p.href}`, ico: p.ico, href: p.href, label: p.label })))

  // Keyboard order is the rendered order: one flat list across the groups.
  const groups = $derived([
    { label: query ? translate("omnibox.documents") : translate("omnibox.recent"), rows: documentRows },
    { label: translate("omnibox.views"), rows: viewRows },
    { label: translate("omnibox.commands"), rows: commandRows },
    { label: translate("omnibox.pages"), rows: pageRows },
  ].filter(group => group.rows.length))
  const items = $derived(groups.flatMap(group => group.rows))
  const offsets = $derived(groups.map((_, index) => groups.slice(0, index).reduce((sum, group) => sum + group.rows.length, 0)))

  function opened() {
    open = true
    recent = session.user ? recentDocuments(session.user.user_id, systems.code).slice(0, DOCUMENT_LIMIT) : []
    if (!views) void loadViews()
  }

  async function loadViews() {
    const scope = captureScope()
    try {
      const [{ listSavedViews }, { documentListHash, parseSavedViewFilters }] = await Promise.all([
        import('./api.js'), import('./documentFilters.js'),
      ])
      const res = await listSavedViews({ include: 'shared' })
      if (!scopeCurrent(scope)) return
      views = (res?.results || res || []).map(view => ({
        id: view.id, name: view.name, href: scopedHash(documentListHash(parseSavedViewFilters(view.filter_json))),
      }))
    } catch {
      views = []
    }
  }

  function scheduleDocumentSearch() {
    clearTimeout(searchTimer)
    documentHits = []
    const value = q.trim()
    if (value.length < 2) return
    const version = queryVersion
    const scope = captureScope()
    searchTimer = setTimeout(async () => {
      try {
        const { search } = await import('./api.js')
        const res = await search(value, { page_size: DOCUMENT_LIMIT })
        if (version === queryVersion && scopeCurrent(scope)) documentHits = res?.results || []
      } catch {
        // A partial rich query such as `field:` is still being typed.
      }
    }, SEARCH_DELAY)
  }

  async function activate(i) {
    if (i >= 0 && items[i]) {
      const it = items[i]
      if (it.run) it.run()
      else if (it.href) go(it.href)
    } else if (q.trim()) {
      const value = q.trim()
      if (isCanonicalFilingAddress(value)) {
        const version = ++queryVersion
        const scope = captureScope()
        let result = null
        try {
          result = await resolveAddress(value)
        } catch (ex) {
          // A missing category is ordinary text, such as the price 10.15.
          if (ex?.status !== 404) {
            if (scopeCurrent(scope) && version === queryVersion) error = ex.message || translate("omnibox.addressUnavailable")
            return
          }
        }
        if (!scopeCurrent(scope) || version !== queryVersion) return
        go(result ? resolvedFilingAddressHash(result) : `#/search?q=${encodeURIComponent(value)}`)
      } else go(`#/search?q=${encodeURIComponent(value)}`)
    }
    close()
  }
  function close() {
    queryVersion++
    clearTimeout(searchTimer)
    open = false; idx = -1; q = ''; error = ''; documentHits = []; views = null; input?.blur()
  }
  function restoreFocusWithoutMenu() {
    input?.focus()
    open = false
    idx = -1
  }

  function ask() {
    const question = q.trim()
    close()
    onAsk?.(question, restoreFocusWithoutMenu)
  }

  function onKey(e) {
    if (e.key === 'Escape') { close(); return }
    if (!open) return
    if (e.key === 'ArrowDown') { e.preventDefault(); idx = Math.min(idx + 1, items.length - 1) }
    else if (e.key === 'ArrowUp') { e.preventDefault(); idx = Math.max(idx - 1, -1) }
    else if (e.key === 'Enter') { e.preventDefault(); activate(idx) }
  }

  function globalKey(e) {
    if ((e.metaKey || e.ctrlKey) && e.key === 'k') {
      if (document.querySelector('[role="dialog"], [role="alertdialog"]')) return
      e.preventDefault(); input?.focus(); opened()
    }
  }
  function outside(e) { if (box && !box.contains(e.target)) close() }
</script>

<svelte:window onkeydown={globalKey} onmousedown={outside} />

<div class="omni" bind:this={box}>
  <Icon name="search" size={15} />
  <input bind:this={input} bind:value={q} type="search" placeholder={translate("omnibox.searchOrRunACommand")}
         maxlength="2000"
         autocomplete="off" spellcheck="false" aria-label={translate("omnibox.searchOrRunACommand")}
         aria-keyshortcuts={shortcutKeys}
         onfocus={opened} oninput={() => { open = true; idx = -1; error = ''; queryVersion++; scheduleDocumentSearch() }}
         onkeydown={onKey} />

  {#if !open && !q}<kbd class="omni-shortcut" aria-hidden="true">{shortcutLabel}</kbd>{/if}

  {#if canAsk}
    <button type="button" class="omni-ask" aria-label={translate("omnibox.askYourDocuments")} title={translate("omnibox.askYourDocuments")}
            onmousedown={(e) => e.preventDefault()} onclick={ask}>
      <Icon name="ask" size={14} /><span>{translate("omnibox.ask")}</span>
    </button>
  {/if}

  {#if open}
    <div class="omni-drop" role="listbox">
      {#if error}<p class="err" role="alert">{error}</p>{/if}
      {#each groups as group, g (group.label)}
        <div class="omni-lbl">{group.label}</div>
        {#each group.rows as row, r (row.key)}
          {@const index = offsets[g] + r}
          <button class="omni-row" class:hot={idx === index} role="option" aria-selected={idx === index}
                  onmousedown={(e) => { e.preventDefault(); activate(index) }}>
            <Icon name={row.ico} size={13} /><span class="grow">{row.label}</span>
          </button>
        {/each}
      {/each}
      {#if q.trim()}
        <button class="omni-row all" class:hot={idx === -1}
                onmousedown={(e) => { e.preventDefault(); activate(-1) }}>
          <Icon name="search" size={13} /><span class="grow">{translate("omnibox.searchEverythingForValue", { "trim": q.trim() })}</span>
        </button>
      {/if}
    </div>
  {/if}
</div>
