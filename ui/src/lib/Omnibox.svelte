<script>
  // Combined command palette and query entry point.
  import { onDestroy } from 'svelte'
  import { createQueryAssistant } from './queryAssist.js'
  import { go } from './router.svelte.js'
  import Icon from './Icon.svelte'

  let { pages = [], commands = [] } = $props()
  let q = $state('')
  let open = $state(false)
  let idx = $state(-1)
  let suggestions = $state([])
  let box, input
  const queryAssistant = createQueryAssistant(
    (next) => (suggestions = next),
    { delay: 160, limit: 6 },
  )
  onDestroy(queryAssistant.dispose)

  function matches(label) {
    const s = q.trim().toLowerCase()
    return !s || label.toLowerCase().includes(s)
  }

  const commandHits = $derived(commands.filter(c => matches(c.label)).slice(0, 6))
  const pageHits = $derived(pages.filter(p => matches(p.label)).slice(0, q.trim() ? 3 : 5))

  // Keyboard order must match the rendered groups.
  const items = $derived([
    ...commandHits.map((c, i) => ({ kind: 'cmd', i, run: c.run })),
    ...suggestions.map((suggestion) => ({ kind: 'query', query: suggestion.query })),
    ...pageHits.map((p) => ({ kind: 'page', href: p.href })),
  ])
  const total = $derived(items.length)

  function activate(i) {
    if (i >= 0 && items[i]) {
      const it = items[i]
      if (it.kind === 'query') {
        q = it.query
        idx = -1
        queryAssistant.update(q)
        queueMicrotask(() => input?.focus())
        return
      }
      if (it.kind === 'cmd') it.run?.()
      else if (it.href) go(it.href)
    } else if (q.trim()) {
      go(`#/search?q=${encodeURIComponent(q.trim())}`)
    }
    close()
  }
  function close() {
    queryAssistant.clear()
    open = false; idx = -1; q = ''; input?.blur()
  }

  function onKey(e) {
    if (e.key === 'Escape') { close(); return }
    if (!open) return
    if (e.key === 'ArrowDown') { e.preventDefault(); idx = Math.min(idx + 1, total - 1) }
    else if (e.key === 'ArrowUp') { e.preventDefault(); idx = Math.max(idx - 1, -1) }
    else if (e.key === 'Enter') { e.preventDefault(); activate(idx) }
  }

  function globalKey(e) {
    if ((e.metaKey || e.ctrlKey) && e.key === 'k') { e.preventDefault(); input?.focus(); open = true }
  }
  function outside(e) { if (box && !box.contains(e.target)) close() }

  const suggestionsBase = $derived(commandHits.length)
  const pagesBase = $derived(commandHits.length + suggestions.length)
</script>

<svelte:window onkeydown={globalKey} onmousedown={outside} />

<div class="omni" bind:this={box}>
  <Icon name="search" size={15} />
  <input bind:this={input} bind:value={q} type="search" placeholder="Search or run a command"
         autocomplete="off" spellcheck="false" aria-label="Search or run a command"
         onfocus={() => (open = true)} oninput={(e) => { open = true; idx = -1; queryAssistant.update(e.target.value) }}
         onkeydown={onKey} />

  {#if open}
    <div class="omni-drop" role="listbox">
      {#if commandHits.length}
        <div class="omni-lbl">Commands</div>
        {#each commandHits as c, i}
          <button class="omni-row" class:hot={idx === i} role="option" aria-selected={idx === i}
                  onmousedown={(e) => { e.preventDefault(); activate(i) }}>
            <Icon name={c.ico} size={13} /><span class="grow">{c.label}</span>
          </button>
        {/each}
      {/if}
      {#if suggestions.length}
        <div class="omni-lbl">Query suggestions</div>
        {#each suggestions as suggestion, i (suggestion.query)}
          <button class="omni-row" class:hot={idx === suggestionsBase + i} role="option" aria-selected={idx === suggestionsBase + i}
                  onmousedown={(e) => { e.preventDefault(); activate(suggestionsBase + i) }}>
            <Icon name="search" size={13} />
            <span class="chip">{suggestion.kind}</span>
            <span class="grow">{suggestion.value}</span>
          </button>
        {/each}
      {/if}
      {#if pageHits.length}
        <div class="omni-lbl">Pages</div>
        {#each pageHits as p, j}
          <button class="omni-row" class:hot={idx === pagesBase + j} role="option" aria-selected={idx === pagesBase + j}
                  onmousedown={(e) => { e.preventDefault(); activate(pagesBase + j) }}>
            <Icon name={p.ico} size={13} /><span class="grow">{p.label}</span>
          </button>
        {/each}
      {/if}
      {#if q.trim()}
        <button class="omni-row all" class:hot={idx === -1}
                onmousedown={(e) => { e.preventDefault(); activate(-1) }}>
          <Icon name="search" size={13} /><span class="grow">Search everything for “{q.trim()}”</span>
        </button>
      {/if}
    </div>
  {/if}
</div>
