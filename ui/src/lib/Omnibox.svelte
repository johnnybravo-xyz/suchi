<script>
  // Combined command palette and query entry point.
  import { go } from './router.svelte.js'
  import { resolveAddress } from './api.js'
  import { captureScope, scopeCurrent } from './systems.svelte.js'
  import Icon from './Icon.svelte'

  let { pages = [], commands = [], canAsk = false, onAsk } = $props()
  let q = $state('')
  let open = $state(false)
  let idx = $state(-1)
  let error = $state('')
  let queryVersion = 0
  let box, input

  function matches(label) {
    const s = q.trim().toLowerCase()
    return !s || label.toLowerCase().includes(s)
  }

  const commandHits = $derived(commands.filter(c => matches(c.label)).slice(0, 6))
  const pageHits = $derived(pages.filter(p => matches(p.label)).slice(0, q.trim() ? 3 : 5))

  // Keyboard order must match the rendered groups.
  const items = $derived([
    ...commandHits.map((c, i) => ({ kind: 'cmd', i, run: c.run })),
    ...pageHits.map((p) => ({ kind: 'page', href: p.href })),
  ])
  const total = $derived(items.length)

  async function activate(i) {
    if (i >= 0 && items[i]) {
      const it = items[i]
      if (it.kind === 'cmd') it.run?.()
      else if (it.href) go(it.href)
    } else if (q.trim()) {
      const value = q.trim()
      if (/^[A-Z][0-9]{2}\.[0-9]{2}\.[1-9][0-9]*$/.test(value)) {
        const version = ++queryVersion
        const scope = captureScope()
        try {
          const document = await resolveAddress(value)
          if (!scopeCurrent(scope) || version !== queryVersion) return
          go(`#/doc/${value.split('.')[2]}?system=${document.system_code}`)
        } catch (ex) {
          if (scopeCurrent(scope) && version === queryVersion) error = ex.message || 'Address unavailable'
          return
        }
      } else go(`#/search?q=${encodeURIComponent(value)}`)
    }
    close()
  }
  function close() {
    queryVersion++
    open = false; idx = -1; q = ''; error = ''; input?.blur()
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
    if (e.key === 'ArrowDown') { e.preventDefault(); idx = Math.min(idx + 1, total - 1) }
    else if (e.key === 'ArrowUp') { e.preventDefault(); idx = Math.max(idx - 1, -1) }
    else if (e.key === 'Enter') { e.preventDefault(); activate(idx) }
  }

  function globalKey(e) {
    if ((e.metaKey || e.ctrlKey) && e.key === 'k') {
      if (document.querySelector('[role="dialog"], [role="alertdialog"]')) return
      e.preventDefault(); input?.focus(); open = true
    }
  }
  function outside(e) { if (box && !box.contains(e.target)) close() }

  const pagesBase = $derived(commandHits.length)
</script>

<svelte:window onkeydown={globalKey} onmousedown={outside} />

<div class="omni" bind:this={box}>
  <Icon name="search" size={15} />
  <input bind:this={input} bind:value={q} type="search" placeholder="Search or run a command"
         maxlength="2000"
         autocomplete="off" spellcheck="false" aria-label="Search or run a command"
         onfocus={() => (open = true)} oninput={() => { open = true; idx = -1; error = ''; queryVersion++ }}
         onkeydown={onKey} />

  {#if canAsk}
    <button type="button" class="omni-ask" aria-label="Ask the archive" title="Ask the archive"
            onmousedown={(e) => e.preventDefault()} onclick={ask}>
      <Icon name="ask" size={14} /><span>Ask</span>
    </button>
  {/if}

  {#if open}
    <div class="omni-drop" role="listbox">
      {#if error}<p class="err" role="alert">{error}</p>{/if}
      {#if commandHits.length}
        <div class="omni-lbl">Commands</div>
        {#each commandHits as c, i}
          <button class="omni-row" class:hot={idx === i} role="option" aria-selected={idx === i}
                  onmousedown={(e) => { e.preventDefault(); activate(i) }}>
            <Icon name={c.ico} size={13} /><span class="grow">{c.label}</span>
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
