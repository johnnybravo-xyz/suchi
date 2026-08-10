<script>
  // The topbar centerpiece: search + command palette folded together.
  // On focus (even empty) it shows Commands. Typing filters commands +
  // pages by label and fetches document hits via /api/autocomplete/.
  // Enter opens the highlighted row, or falls back to full search.
  // Cmd/Ctrl+K focuses it from anywhere.
  import { autocomplete } from './api.js'
  import Icon from './Icon.svelte'

  let { pages = [], commands = [] } = $props()
  let q = $state('')
  let open = $state(false)
  let idx = $state(-1)
  let docs = $state([])
  let box, input, timer

  function matches(label) {
    const s = q.trim().toLowerCase()
    return !s || label.toLowerCase().includes(s)
  }

  // On empty focus we show ALL commands + all pages (Commands are the
  // headline — that's the palette). Typing filters both. Documents
  // section only appears when we have hits from autocomplete.
  const commandHits = $derived(commands.filter(c => matches(c.label)).slice(0, 6))
  const pageHits = $derived(pages.filter(p => matches(p.label)).slice(0, q.trim() ? 3 : 5))

  // Flat, keyboard-navigable order matches the render order below:
  //   commands → documents → pages → "search everything" tail
  const items = $derived([
    ...commandHits.map((c, i) => ({ kind: 'cmd', i, run: c.run })),
    ...docs.map((d) => ({ kind: 'doc', href: `#/doc/${d.id}` })),
    ...pageHits.map((p) => ({ kind: 'page', href: p.href })),
  ])
  const total = $derived(items.length)

  function search(v) {
    clearTimeout(timer)
    if (!v.trim()) { docs = []; return }
    timer = setTimeout(async () => {
      try {
        const r = await autocomplete(v.trim(), 6)
        docs = (r?.results || r || []).slice(0, 6)
      } catch { docs = [] }
    }, 160)
  }

  function activate(i) {
    if (i >= 0 && items[i]) {
      const it = items[i]
      if (it.kind === 'cmd') it.run?.()
      else if (it.href) location.hash = it.href.replace(/^#/, '')
    } else if (q.trim()) {
      location.hash = `/search?q=${encodeURIComponent(q.trim())}`
    }
    close()
  }
  function close() { open = false; idx = -1; q = ''; docs = []; input?.blur() }

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

  // Absolute index into `items` for the palette-order (commands first,
  // then documents, then pages). Keeps the render loops readable.
  const docsBase = $derived(commandHits.length)
  const pagesBase = $derived(commandHits.length + docs.length)
</script>

<svelte:window onkeydown={globalKey} onmousedown={outside} />

<div class="omni" bind:this={box}>
  <Icon name="search" size={15} />
  <input bind:this={input} bind:value={q} type="search" placeholder="Search or run a command"
         autocomplete="off" spellcheck="false" aria-label="Search or run a command"
         onfocus={() => (open = true)} oninput={(e) => { open = true; idx = -1; search(e.target.value) }}
         onkeydown={onKey} />
  <kbd>⌘K</kbd>

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
      {#if docs.length}
        <div class="omni-lbl">Documents</div>
        {#each docs as d, i (d.id)}
          <button class="omni-row" class:hot={idx === docsBase + i} role="option" aria-selected={idx === docsBase + i}
                  onmousedown={(e) => { e.preventDefault(); activate(docsBase + i) }}>
            <span class="dot"></span>
            {#if d.jd_category_code}<span class="chip">{d.jd_category_code}</span>{/if}
            <span class="grow">{d.title || `Document #${d.id}`}</span>
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
          <Icon name="search" size={13} /><span class="grow">Search everything for “{q.trim()}”</span><kbd>⏎</kbd>
        </button>
      {/if}
    </div>
  {/if}
</div>
