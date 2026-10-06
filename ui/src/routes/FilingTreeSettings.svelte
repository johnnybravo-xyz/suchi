<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { onMount, tick } from 'svelte'
  import { captureScope } from '../lib/systems.svelte.js'
  import {
    setupState, listPresets, listFilingSets, listJDCategories,
    previewPresetChange, applyPresetChange, exportTaxonomy,
  } from '../lib/api.js'
  import Icon from '../lib/Icon.svelte'
  import TaxonomyImport from '../lib/TaxonomyImport.svelte'

  let { notify, onTaxonomyChanged } = $props()

  let mode = $state('presets')
  let presets = $state([])
  let sets = $state([])
  let maxSets = $state(5)
  let currentPreset = $state('')
  let currentSetIDs = $state([])
  let currentAreas = $state([])
  let selectedPresetID = $state('solo')
  let selectedSetIDs = $state([])
  let includeSeeds = $state(true)
  let refile = $state(false)
  let confirmBlank = $state(false)
  let preview = $state(null)
  let collisionChoices = $state({})
  let reviewDirty = $state(false)
  let loading = $state(true)
  let busy = $state(false)
  let error = $state('')
  let applied = $state(null)
  let importOpen = $state(false)
  let importBusy = $state(false)
  let exportBusy = $state(false)
  let exportFormat = $state('huml')
  let reviewSection = $state()
  let reviewRevealed = $state(false)

  const lanes = $derived([...new Set(sets.map((set) => set.lane))].sort((a, b) => a - b))
  const selectedPreset = $derived(presets.find((preset) => preset.id === selectedPresetID))
  const blankTarget = $derived(mode === 'presets' ? selectedPreset?.blank : selectedSetIDs.length === 0)

  onMount(load)

  function groupCategories(rows) {
    const grouped = new Map()
    for (const category of rows || []) {
      if (category.system) continue
      if (!grouped.has(category.area_code)) grouped.set(category.area_code, {
        code: category.area_code, name: category.area_name, categories: [],
      })
      grouped.get(category.area_code).categories.push(category)
    }
    return [...grouped.values()].sort((a, b) => a.code - b.code)
  }

  async function load() {
    loading = true
    error = ''
    try {
      const [state, presetRows, catalog, categories] = await Promise.all([
        setupState(), listPresets(), listFilingSets(), listJDCategories(),
      ])
      presets = presetRows?.results || presetRows || []
      sets = catalog?.sets || []
      maxSets = catalog?.max_sets || 5
      currentPreset = state?.current_preset || ''
      currentSetIDs = state?.current_set_ids?.length
        ? state.current_set_ids
        : (catalog?.recipes?.[currentPreset] || [])
      if (presets.some((preset) => preset.id === currentPreset)) {
        selectedPresetID = currentPreset
      } else if (currentSetIDs.length) {
        mode = 'build'
      }
      selectedSetIDs = [...currentSetIDs]
      currentAreas = groupCategories(categories?.results || categories || [])
    } catch (ex) {
      error = ex.message || 'Could not load the filing tree.'
    } finally {
      loading = false
    }
  }

  function resetReview() {
    preview = null
    collisionChoices = {}
    reviewDirty = false
    applied = null
    error = ''
    reviewRevealed = false
  }

  function chooseMode(next) {
    mode = next
    resetReview()
  }

  function chooseSet(set) {
    const selected = selectedSetIDs.includes(set.id)
    selectedSetIDs = selected
      ? selectedSetIDs.filter((id) => id !== set.id)
      : [...selectedSetIDs.filter((id) => sets.find((candidate) => candidate.id === id)?.lane !== set.lane), set.id]
    resetReview()
  }

  function baseBody() {
    const body = {
      include_seeds: includeSeeds,
      confirm_blank: confirmBlank,
      refile,
      replacements: {},
      remaps: {},
    }
    if (mode === 'presets') body.preset_id = selectedPresetID
    else body.set_ids = selectedSetIDs
    for (const collision of preview?.collisions || []) {
      if (collision.replace) continue
      const choice = collisionChoices[collision.code]
      if (choice === 'replace') body.replacements[collision.code] = true
      else if (choice === 'remap') body.remaps[collision.code] = collision.proposed_code
      else if (choice === 'skip') body.remaps[collision.code] = 0
    }
    return body
  }

  function seedCollisionChoices(change) {
    const choices = { ...collisionChoices }
    for (const collision of change.collisions || []) {
      if (collision.resolved || choices[collision.code]) continue
      choices[collision.code] = collision.suggested_replace
        ? 'replace'
        : collision.proposed_code ? 'remap' : 'skip'
    }
    collisionChoices = choices
  }

  async function review() {
    busy = true
    error = ''
    applied = null
    reviewRevealed = false
    try {
      preview = await previewPresetChange(baseBody())
      seedCollisionChoices(preview)
      reviewDirty = preview.collisions?.some((collision) => !collision.resolved) || false
      await tick()
      reviewSection?.scrollIntoView({
        behavior: window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth',
        block: 'start',
      })
      reviewSection?.focus({ preventScroll: true })
      reviewRevealed = true
    } catch (ex) {
      error = ex.message || 'Could not preview this filing-tree change.'
    } finally {
      busy = false
    }
  }

  function chooseCollision(code, choice) {
    collisionChoices = { ...collisionChoices, [code]: choice }
    reviewDirty = true
  }

  async function applyChange() {
    if (blankTarget && !confirmBlank) return
    busy = true
    error = ''
    try {
      let reviewed = preview
      if (!reviewed || reviewDirty) reviewed = await previewPresetChange(baseBody())
      const unresolved = reviewed.collisions?.filter((collision) => !collision.resolved) || []
      if (unresolved.length) {
        preview = reviewed
        seedCollisionChoices(reviewed)
        reviewDirty = true
        error = 'Choose how to handle every category conflict.'
        return
      }
      preview = reviewed
      const result = await applyPresetChange({ ...baseBody(), expected_state_hash: reviewed.state_hash })
      applied = result
      preview = result
      reviewDirty = false
      await Promise.all([loadCurrentTree(), onTaxonomyChanged?.()])
      currentPreset = result.preset_id
      currentSetIDs = result.set_ids || []
      notify?.('Filing tree updated')
    } catch (ex) {
      error = ex.code === 'stale_preview'
        ? 'The archive changed after the preview. Review the change again.'
        : (ex.message || 'Could not update the filing tree.')
    } finally {
      busy = false
    }
  }

  async function loadCurrentTree() {
    const categories = await listJDCategories()
    currentAreas = groupCategories(categories?.results || categories || [])
  }

  async function importedTree() {
    await Promise.all([load(), onTaxonomyChanged?.()])
  }

  async function doExport(skipSeeds) {
    if (exportBusy || importBusy) return
    exportBusy = true
    try {
      const text = await exportTaxonomy(exportFormat, skipSeeds)
      const a = document.createElement('a')
      a.href = URL.createObjectURL(new Blob([text], { type: 'text/plain' }))
      const code = captureScope().code
      a.download = `${code ? `${code}.taxonomy` : `archive${skipSeeds ? '-tree' : ''}`}.${exportFormat}`
      a.click()
      setTimeout(() => URL.revokeObjectURL(a.href), 10_000)
      notify?.(`Exported ${a.download}`)
    } catch (ex) {
      notify?.(ex.message || 'Export failed')
    } finally {
      exportBusy = false
    }
  }
</script>

<section class="filing-workspace" aria-labelledby="filing-tree-heading">
  <header class="workspace-heading">
    <div>
      <span class="eyebrow">Archive structure</span>
      <h2 id="filing-tree-heading">Filing tree</h2>
      <p>Choose a ready-made tree, combine focused sets, or use a taxonomy file. Every change is reviewed before it touches the archive.</p>
    </div>
    {#if currentPreset}<span class="current-chip">Current · {currentPreset.replaceAll('_', ' ')}</span>{/if}
  </header>

  {#if loading}
    <div class="card loading" aria-label="Loading filing tree"><div class="skel"></div><div class="skel"></div><div class="skel"></div></div>
  {:else if error && !presets.length}
    <div class="card"><div class="err">{error}</div><button class="btn sm" onclick={load}>Retry</button></div>
  {:else}
    <nav class="mode-tabs" aria-label="Filing tree methods">
      <button class:on={mode === 'presets'} aria-pressed={mode === 'presets'} onclick={() => chooseMode('presets')}>Ready-made</button>
      <button class:on={mode === 'build'} aria-pressed={mode === 'build'} onclick={() => chooseMode('build')}>Build your own</button>
      <button class:on={mode === 'files'} aria-pressed={mode === 'files'} onclick={() => chooseMode('files')}>Import or export</button>
    </nav>

    {#if mode === 'files'}
      <div class="file-panels">
        <section class="card file-card" aria-labelledby="import-heading">
          <div class="file-heading">
            <div><h3 id="import-heading">Import a taxonomy file</h3><p>Preview an advanced HuML or TOML tree before applying it.</p></div>
            <button class="btn" disabled={exportBusy || importBusy} onclick={() => (importOpen = !importOpen)}><Icon name="upload" size={14} />{importOpen ? 'Close' : 'Import file'}</button>
          </div>
          {#if importOpen}<fieldset class="import-body" disabled={exportBusy}><TaxonomyImport {notify} bind:busy={importBusy} onApplied={importedTree} /></fieldset>{/if}
        </section>
        <section class="card file-card" aria-labelledby="export-heading">
          <h3 id="export-heading">Export this filing tree</h3>
          <p>Save the tree with supported starter rules, or the structure alone.</p>
          <label class="field">File format
            <select class="input" bind:value={exportFormat} disabled={exportBusy || importBusy}><option value="huml">HuML</option><option value="toml">TOML</option></select>
          </label>
          <div class="toolbar">
            <button class="btn" disabled={exportBusy || importBusy} onclick={() => doExport(false)}><Icon name="download" size={14} />Export with starter rules</button>
            <button class="btn" disabled={exportBusy || importBusy} onclick={() => doExport(true)}>Export tree only</button>
          </div>
          <p class="note">A filing-tree export is not an archive backup. Documents, permissions, user rules, and history are not included.</p>
        </section>
      </div>
    {:else}
      <div class="workspace-grid">
        <div class="choice-column">
          {#if mode === 'presets'}
            <div class="preset-grid">
              {#each presets as preset (preset.id)}
                <label class="preset-card" class:on={selectedPresetID === preset.id}>
                  <input type="radio" bind:group={selectedPresetID} value={preset.id} onchange={resetReview} />
                  <span><b>{preset.name}</b><small>{preset.description}</small></span>
                  {#if preset.areas?.length}
                    <span class="lane-list">
                      {#each preset.areas as area}<span>{area.code}–{area.code + 9} {area.name}</span>{/each}
                    </span>
                  {/if}
                </label>
              {/each}
            </div>
          {:else}
            <p class="composer-intro">Choose at most one set in each numbered lane. Up to {maxSets} sets keep the tree compact.</p>
            <div class="lane-groups">
              {#each lanes as lane}
                <section class="lane-group">
                  <header><b>{lane}–{lane + 9}</b><span>Choose one</span></header>
                  <div class="lane-options">
                    {#each sets.filter((set) => set.lane === lane) as set (set.id)}
                      <button class:on={selectedSetIDs.includes(set.id)} aria-pressed={selectedSetIDs.includes(set.id)} onclick={() => chooseSet(set)}>
                        <b>{set.name}</b><small>{set.description}</small>
                      </button>
                    {/each}
                  </div>
                </section>
              {/each}
            </div>
          {/if}

          <div class="change-options">
            {#if blankTarget}
              <label class="warning-check"><input type="checkbox" bind:checked={confirmBlank} /> I understand that documents will remain in Inbox until I add categories.</label>
            {/if}
            <label><input type="checkbox" bind:checked={includeSeeds} onchange={resetReview} /> Include starter filing rules</label>
            <label><input type="checkbox" bind:checked={refile} /> Refile existing documents after the change</label>
            <p>Refile runs the selected tree’s active rules and refreshes rendered paths. It does not migrate the taxonomy itself.</p>
          </div>
          {#if error}<div class="err" role="alert">{error}</div>{/if}
          <button class="btn primary review-button" disabled={busy || (blankTarget && !confirmBlank)} onclick={review}>{busy ? 'Working…' : 'Review change'}</button>
        </div>

        <aside class="current-tree" aria-label="Current filing tree">
          <header><div><span class="current-label">In use now</span><h3>Current tree</h3></div><span>{currentAreas.length} lanes</span></header>
          {#each currentAreas as area (area.code)}
            <div class="tree-area"><b>{area.code}–{area.code + 9} {area.name}</b><span>{area.categories.map((category) => `${category.code} ${category.name}`).join(' · ')}</span></div>
          {:else}<p class="empty">No user categories yet.</p>{/each}
        </aside>
      </div>

      {#if preview}
        <section class="review card" class:revealed={reviewRevealed} bind:this={reviewSection} tabindex="-1" aria-labelledby="review-heading">
          <header class="review-heading">
            <div><span class="eyebrow">Review</span><h3 id="review-heading">{preview.name}</h3><p>{preview.story}</p></div>
          </header>
          <div class="target-tree">
            {#each preview.user_areas || [] as area (area.code)}
              <section><h4>{area.code}–{area.code + 9} {area.name}</h4><ul>{#each area.categories || [] as category (category.code)}<li><code>{category.code}</code><span>{category.name}</span></li>{/each}</ul></section>
            {:else}<p class="empty">Blank keeps only Suchi’s internal Inbox.</p>{/each}
          </div>

          {#if preview.collisions?.length}
            <div class="collisions">
              <h4>Category mappings</h4>
              {#each preview.collisions as collision (collision.code)}
                <fieldset class="collision" disabled={busy || collision.replace}>
                  <legend><code>{collision.code}</code> {collision.existing} → {collision.incoming}</legend>
                  <p>{collision.live_documents || 0} live · {collision.trashed_documents || 0} in Trash</p>
                  {#if collision.replace}
                    <span class="mapping automatic">Keep category and documents; update its label</span>
                  {:else}
                    <label><input type="radio" name={`collision-${collision.code}`} checked={collisionChoices[collision.code] === 'replace'} onchange={() => chooseCollision(collision.code, 'replace')} /> Replace meaning in place {collision.suggested_replace ? '· recommended' : ''}</label>
                    {#if collision.proposed_code}<label><input type="radio" name={`collision-${collision.code}`} checked={collisionChoices[collision.code] === 'remap'} onchange={() => chooseCollision(collision.code, 'remap')} /> Add as {collision.proposed_code}; keep existing {collision.code}</label>{/if}
                    <label><input type="radio" name={`collision-${collision.code}`} checked={collisionChoices[collision.code] === 'skip'} onchange={() => chooseCollision(collision.code, 'skip')} /> Skip incoming category</label>
                  {/if}
                </fieldset>
              {/each}
            </div>
          {/if}

          <div class="review-summary">
            <span>{preview.categories_to_add?.length || 0} categories added</span>
            <span>{preview.rules_to_add?.length || 0} starter rules added</span>
            <span>{preview.rules_preserved?.length || 0} rules preserved</span>
          </div>
          {#if applied}
            <div class="success" role="status">Filing tree updated.{#if applied.refile} Refile scanned {applied.refile.documents_scanned} documents and queued {applied.refile.renders_queued} renders.{/if}</div>
          {:else}
            <button class="btn primary apply-button" disabled={busy || (blankTarget && !confirmBlank)} onclick={applyChange}>{busy ? 'Applying…' : 'Apply reviewed change'}</button>
          {/if}
        </section>
      {/if}
    {/if}
  {/if}
</section>

<style>
  .filing-workspace { max-width: 1120px; }
  .workspace-heading { display:flex;align-items:flex-end;justify-content:space-between;gap:20px;margin-bottom:18px; }
  .workspace-heading h2 { margin:0;font-size:1.35rem; }
  .workspace-heading p { max-width:700px;margin:6px 0 0;color:var(--muted);font-size:.82rem;line-height:1.5; }
  .eyebrow { display:block;margin-bottom:5px;color:var(--accent);font-family:ui-monospace,monospace;font-size:.64rem;font-weight:700;letter-spacing:.07em; }
  .current-chip { flex:none;padding:5px 9px;border-radius:99px;background:var(--surface-2);color:var(--muted);font-size:.68rem;text-transform:capitalize; }
  .mode-tabs { display:flex;gap:4px;margin-bottom:18px;padding-bottom:10px;border-bottom:1px solid var(--line); }
  .mode-tabs button { border:0;border-radius:7px;background:none;padding:8px 13px;color:var(--muted);font-size:.82rem;font-weight:550; }
  .mode-tabs button.on { color:var(--accent);background:var(--tint); }
  .workspace-grid { display:grid;grid-template-columns:minmax(0,1.6fr) minmax(260px,.8fr);gap:16px;align-items:start; }
  .choice-column { min-width:0; }
  .preset-grid { display:grid;grid-template-columns:1fr 1fr;gap:10px; }
  .preset-card { display:grid;grid-template-columns:auto minmax(0,1fr);gap:10px;padding:13px;border:1px solid var(--line-strong);border-radius:var(--r-sm);cursor:pointer; }
  .preset-card.on { border-color:var(--accent);background:var(--tint);box-shadow:inset 0 0 0 1px var(--accent); }
  .preset-card input { margin-top:3px;accent-color:var(--accent); }
  .preset-card span,.lane-options button { min-width:0; }
  .preset-card b,.preset-card small,.lane-options b,.lane-options small { display:block; }
  .preset-card small,.lane-options small { margin-top:3px;color:var(--muted);font-size:.73rem;line-height:1.4; }
  .lane-list { grid-column:2;display:flex;flex-wrap:wrap;gap:5px; }
  .lane-list span { padding:4px 7px;border-radius:5px;background:var(--tint);color:var(--accent);font-family:ui-monospace,monospace;font-size:.65rem;font-weight:650;line-height:1.2; }
  .composer-intro { margin:0 0 12px;color:var(--muted);font-size:.82rem; }
  .lane-groups { display:grid;gap:10px; }
  .lane-group { border:1px solid var(--line);border-radius:var(--r-sm);overflow:hidden; }
  .lane-group > header { display:flex;justify-content:space-between;padding:9px 11px;background:var(--bg);font-size:.72rem; }
  .lane-group > header span { color:var(--muted); }
  .lane-options { display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:1px;background:var(--line); }
  .lane-options button { border:0;background:var(--surface);padding:11px;text-align:left;color:var(--ink); }
  .lane-options button.on { background:var(--tint);color:var(--accent); }
  .change-options { display:grid;gap:10px;margin-top:16px;padding:14px;border:1px solid var(--line);border-radius:var(--r-sm);background:var(--bg);color:var(--muted);font-size:.8rem; }
  .change-options label { display:flex;align-items:baseline;gap:8px; }
  .change-options input { accent-color:var(--accent); }
  .change-options p { margin:0;color:var(--faint);font-size:.72rem;line-height:1.45; }
  .warning-check { color:var(--warn); }
  .review-button { width:100%;justify-content:center;margin-top:12px; }
  .current-tree { overflow:hidden;border-left:3px solid var(--line-strong);background:var(--bg); }
  .current-tree > header { display:flex;align-items:flex-end;justify-content:space-between;padding:5px 12px 11px;border-bottom:1px solid var(--line); }
  .current-tree h3 { margin:1px 0 0;font-size:.85rem; }
  .current-tree header > span { color:var(--muted);font-size:.7rem; }
  .current-label { color:var(--faint);font-size:.6rem;font-weight:650;letter-spacing:.08em;text-transform:uppercase; }
  .tree-area { display:grid;gap:4px;padding:10px 12px;border-bottom:1px solid var(--line); }
  .tree-area:last-child { border-bottom:0; }
  .tree-area b { font-size:.74rem; }
  .tree-area span { color:var(--muted);font-size:.68rem;line-height:1.45; }
  .review { margin-top:18px;padding:18px;scroll-margin-top:18px; }
  .review:focus { outline:1px solid color-mix(in srgb,var(--accent) 32%,var(--line));outline-offset:2px; }
  .review-heading { display:flex;justify-content:space-between;gap:18px; }
  .review-heading h3 { margin:0;font-size:1.05rem; }
  .review-heading p { margin:4px 0 0;color:var(--muted);font-size:.78rem; }
  .review.revealed { animation:review-reveal .45s ease-out; }
  .target-tree { display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:10px;margin-top:16px; }
  .target-tree section { padding:11px;border:1px solid var(--line);border-radius:var(--r-sm); }
  .target-tree h4 { margin:0 0 7px;font-size:.76rem; }
  .target-tree ul { display:grid;gap:4px;margin:0;padding:0;list-style:none; }
  .target-tree li { display:grid;grid-template-columns:26px 1fr;gap:6px;color:var(--muted);font-size:.7rem; }
  .target-tree code,.collision code { color:var(--accent); }
  .collisions { margin-top:18px; }
  .collisions > h4 { margin:0 0 9px;font-size:.86rem; }
  .collision { display:grid;gap:7px;margin:0 0 8px;padding:11px;border:1px solid var(--line);border-radius:var(--r-sm); }
  .collision legend { padding:0 5px;font-size:.77rem;font-weight:600; }
  .collision p { margin:0;color:var(--muted);font-size:.7rem; }
  .collision label,.mapping { color:var(--muted);font-size:.75rem; }
  .collision input { accent-color:var(--accent); }
  .automatic { color:var(--ok); }
  .review-summary { display:flex;flex-wrap:wrap;gap:7px;margin-top:14px; }
  .review-summary span { padding:4px 7px;border-radius:99px;background:var(--surface-2);color:var(--muted);font-size:.68rem; }
  .apply-button { width:100%;justify-content:center;margin-top:14px; }
  .success { margin-top:14px;padding:10px 12px;border-left:3px solid var(--ok);background:var(--ok-soft);color:var(--ok);font-size:.78rem; }
  .file-panels { display:grid;gap:14px; }
  .file-card { padding:18px; }
  .file-card h3 { margin:0;font-size:.92rem; }
  .file-card p { margin:5px 0 14px;color:var(--muted);font-size:.8rem; }
  .file-heading { display:flex;align-items:center;justify-content:space-between;gap:18px; }
  .import-body { border:0;border-top:1px solid var(--line);padding:18px 0 0;margin:18px 0 0;min-width:0; }
  .file-card .field { display:block;max-width:220px;color:var(--muted);font-size:.76rem;font-weight:600; }
  .file-card .input { margin-top:5px; }
  .file-card .toolbar { margin-top:12px; }
  .file-card .note { margin:16px 0 0;padding-top:12px;border-top:1px solid var(--line);font-size:.72rem; }
  .empty { margin:0;padding:14px;color:var(--muted);font-size:.76rem; }
  .loading { display:grid;gap:12px;padding:18px; }
  @keyframes review-reveal { from { transform:translateY(7px);border-color:var(--accent);box-shadow:0 0 0 4px color-mix(in srgb,var(--accent) 12%,transparent); } to { transform:none;border-color:var(--line);box-shadow:none; } }
  @media (max-width: 820px) { .workspace-grid { grid-template-columns:1fr; }.current-tree { order:-1; }.preset-grid { grid-template-columns:1fr; } }
  @media (max-width: 600px) { .workspace-heading,.review-heading,.file-heading { align-items:flex-start;flex-direction:column; }.mode-tabs { overflow-x:auto; }.mode-tabs button { flex:none; }.lane-options { grid-template-columns:1fr; } }
</style>
