<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  let { tags = [], excludedSlugs = [], disabled = false, label = 'Tag to add', menuId = 'tag-options', onChoose } = $props()
  let query = $state('')
  let open = $state(false)
  let highlight = $state(-1)
  let working = $state(false)
  let box, input

  const matches = $derived.by(() => {
    const needle = query.trim().toLocaleLowerCase()
    const excluded = new Set(excludedSlugs)
    return tags.filter(tag => !excluded.has(tag.slug) &&
      (!needle || tag.name.toLocaleLowerCase().includes(needle) || tag.slug.toLocaleLowerCase().includes(needle)))
      .sort((a, b) => a.name.localeCompare(b.name) || a.id - b.id).slice(0, 8)
  })

  function close() { open = false; highlight = -1 }
  function outside(event) { if (box && !box.contains(event.target)) close() }

  async function choose(tag) {
    if (working || disabled || !tag) return
    working = true
    try {
      if (await onChoose(tag)) {
        query = ''
        close()
      }
    } finally {
      working = false
    }
  }

  function keydown(event) {
    if (event.key === 'Escape') { if (open) event.preventDefault(); close(); return }
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault()
      if (!open) { open = true; highlight = -1 }
      highlight = event.key === 'ArrowDown'
        ? Math.min(highlight + 1, matches.length - 1)
        : Math.max(highlight - 1, 0)
    } else if (event.key === 'Enter' && open) {
      event.preventDefault()
      if (highlight >= 0) choose(matches[highlight])
    }
  }
</script>

<svelte:window onmousedown={outside} />
<div class="tag-picker" bind:this={box}>
  <input bind:this={input} class="input" type="search" bind:value={query} {disabled}
         autocomplete="off" role="combobox" aria-label={label} aria-autocomplete="list"
         aria-expanded={open} aria-controls={menuId}
         aria-activedescendant={open && highlight >= 0 && matches[highlight] ? `${menuId}-${highlight}` : undefined}
         placeholder="Type to select a tag" onfocus={() => (open = true)}
         oninput={() => { open = true; highlight = -1 }} onkeydown={keydown} />
  {#if open && !disabled}
    <div class="omni-drop" id={menuId} role="listbox" aria-label={label}>
      {#each matches as tag, i (tag.id)}
        <button type="button" class="omni-row" class:hot={highlight === i}
                id={`${menuId}-${i}`} role="option" aria-selected={highlight === i}
                disabled={working} onmousedown={(event) => event.preventDefault()}
                onclick={() => choose(tag)}>{tag.name}<span class="slug">{tag.slug}</span></button>
      {:else}
        <div class="empty">No matching tags</div>
      {/each}
    </div>
  {/if}
</div>

<style>
  .tag-picker { position:relative; min-width:0; width:min(100%, 260px) }
  .tag-picker input { width:100%; min-width:0 }
  .omni-drop { left:0; right:auto; width:min(320px, 90vw); max-width:100% }
  .omni-row { min-width:0; justify-content:space-between; gap:12px }
  .slug { color:var(--muted); overflow:hidden; text-overflow:ellipsis; white-space:nowrap; font-size:.75rem }
  .empty { padding:10px; color:var(--muted); font-size:.85rem }
</style>
