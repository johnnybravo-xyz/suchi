<script>
  import { autocomplete } from './api.js'
  import { go } from './router.svelte.js'
  import Icon from './Icon.svelte'

  let { close } = $props()
  let q = $state('')
  let hits = $state([])
  let sel = $state(0)
  let inputEl
  let timer

  const pages = [
    { label: 'Documents', hash: '#/documents' },
    { label: 'Inbox', hash: '#/inbox' },
    { label: 'Approvals', hash: '#/tasks' },
    { label: 'Automations', hash: '#/automations' },
    { label: 'Upload', hash: '#/upload' },
    { label: 'Settings', hash: '#/settings' },
  ]

  const items = $derived([
    ...pages.filter(p => !q || p.label.toLowerCase().includes(q.toLowerCase()))
            .map(p => ({ kind: 'page', ...p })),
    ...hits.map(h => ({ kind: 'doc', label: h.title || h.value || h.name, hash: `#/doc/${h.id}` })),
    ...(q ? [{ kind: 'search', label: `Search all documents for “${q}”`, hash: `#/search?q=${encodeURIComponent(q)}` }] : []),
  ])

  function onInput() {
    sel = 0
    clearTimeout(timer)
    if (!q.trim()) { hits = []; return }
    timer = setTimeout(async () => {
      try {
        const res = await autocomplete(q.trim())
        hits = (res?.results || res || []).filter(h => h.id)
      } catch { hits = [] }
    }, 140)
  }

  function onKey(e) {
    if (e.key === 'ArrowDown') { e.preventDefault(); sel = Math.min(sel + 1, items.length - 1) }
    if (e.key === 'ArrowUp') { e.preventDefault(); sel = Math.max(sel - 1, 0) }
    if (e.key === 'Enter' && items[sel]) { go(items[sel].hash); close() }
  }

  $effect(() => { inputEl?.focus() })
</script>

<div class="palette-veil" onclick={close} role="presentation">
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <div class="palette" onclick={(e) => e.stopPropagation()} role="dialog" aria-label="Command palette" tabindex="-1">
    <input bind:this={inputEl} bind:value={q} oninput={onInput} onkeydown={onKey}
           placeholder="Search documents or jump to a page…" />
    <div class="hits index" style="border:0;border-radius:0">
      {#each items as it, i}
        <button class="irow" class:on={i === sel} style={i === sel ? 'background:var(--tint)' : ''}
                onclick={() => { go(it.hash); close() }}>
          <span class="dot" class:accent={it.kind === 'doc'}></span>
          <span class="title grow">{it.label}</span>
          <span class="sub">{it.kind === 'page' ? 'page' : it.kind === 'doc' ? 'document' : ''}</span>
        </button>
      {/each}
    </div>
    <div class="hint">↑↓ navigate · Enter open · Esc close</div>
  </div>
</div>
