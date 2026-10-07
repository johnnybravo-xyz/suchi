<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { onDestroy, onMount } from 'svelte'
  import {
    listRenderedLayouts, createRenderedLayout, patchRenderedLayout,
    deleteRenderedLayout, previewRenderedLayout,
  } from '../lib/api.js'
  import ConfirmDialog from '../lib/ConfirmDialog.svelte'
  import Icon from '../lib/Icon.svelte'

  let { notify } = $props()

  const STARTER_TEMPLATE = '{{ jd.area.code_start }}-{{ jd.area.code_end }} {{ jd.area.name }}/{{ jd.category.code }} {{ jd.category.name }}'
  const VARIABLES = [
    ['Document', ['title', 'doc_pk', 'created', 'created_year', 'created_month', 'created_day', 'added', 'added_year', 'added_month', 'added_day']],
    ['Filing tree', ['jd.area.code_start', 'jd.area.code_end', 'jd.area.name', 'jd.category.code', 'jd.category.name', 'jd.system.code', 'jd.system.name', 'jd.address']],
    ['Metadata', ['correspondent', 'tag_list', 'owner']],
    ['Compatibility', ['storage_path', 'asn']],
  ]

  let rows = $state([])
  let loading = $state(true)
  let error = $state('')
  let busy = $state(false)
  let editingID = $state(null)
  let draft = $state({ name: '', template: STARTER_TEMPLATE })
  let sample = $state(null)
  let previewError = $state('')
  let previewBusy = $state(false)
  let deleteRequest = $state(null)
  let previewVersion = 0
  let previewTimer

  onMount(() => { void load(); schedulePreview() })
  onDestroy(() => { clearTimeout(previewTimer); previewVersion++ })

  async function load() {
    loading = true
    error = ''
    try {
      const result = await listRenderedLayouts()
      rows = result?.results || result || []
    } catch (ex) {
      error = ex.message || 'Could not load folder layouts.'
    } finally {
      loading = false
    }
  }

  function schedulePreview() {
    clearTimeout(previewTimer)
    sample = null
    previewError = ''
    const template = draft.template.trim()
    if (!template) return
    previewTimer = setTimeout(() => void refreshPreview(template), 250)
  }

  async function refreshPreview(template = draft.template.trim()) {
    const version = ++previewVersion
    if (!template) return
    previewBusy = true
    previewError = ''
    try {
      const result = await previewRenderedLayout(template)
      if (version !== previewVersion) return
      sample = result
    } catch (ex) {
      if (version === previewVersion) {
        sample = null
        previewError = ex.message || 'This template could not be rendered.'
      }
    } finally {
      if (version === previewVersion) previewBusy = false
    }
  }

  function edit(row) {
    editingID = row.id
    draft = { name: row.name, template: row.path }
    schedulePreview()
  }

  function cancelEdit() {
    editingID = null
    draft = { name: '', template: STARTER_TEMPLATE }
    sample = null
    previewError = ''
    schedulePreview()
  }

  function insertVariable(name) {
    const suffix = draft.template && !draft.template.endsWith('/') ? '/' : ''
    draft.template = `${draft.template}${suffix}{{ ${name} }}`
    schedulePreview()
  }

  async function save(event) {
    event.preventDefault()
    if (busy || !draft.name.trim() || !draft.template.trim() || previewError) return
    busy = true
    try {
      if (editingID != null) {
        await patchRenderedLayout(editingID, { name: draft.name.trim(), path: draft.template.trim() })
        notify?.('Folder layout updated')
      } else {
        await createRenderedLayout({ name: draft.name.trim(), path: draft.template.trim() })
        notify?.('Folder layout created')
      }
      await load()
      cancelEdit()
    } catch (ex) {
      previewError = ex.message || 'Could not save this folder layout.'
    } finally {
      busy = false
    }
  }

  async function remove() {
    const row = deleteRequest
    if (!row || busy) return
    busy = true
    try {
      await deleteRenderedLayout(row.id)
      rows = rows.filter(candidate => candidate.id !== row.id)
      deleteRequest = null
      if (editingID === row.id) cancelEdit()
      notify?.('Folder layout deleted')
    } catch (ex) {
      deleteRequest = null
      error = ex.message || 'Could not delete this folder layout.'
    } finally {
      busy = false
    }
  }
</script>

<section class="layout-workspace" aria-labelledby="folder-layouts-heading">
  <header class="layout-heading">
    <div>
      <span class="eyebrow">Local filesystem</span>
      <h3 id="folder-layouts-heading">Folder layouts</h3>
      <p>Control how assigned documents appear in the local rendered filesystem. The filing tree and immutable originals stay unchanged.</p>
    </div>
    <span class="pill">{rows.length} saved</span>
  </header>

  <div class="layout-grid">
    <form class="card layout-editor" onsubmit={save}>
      <div class="editor-heading">
        <div><h4>{editingID == null ? 'Create a layout' : 'Edit layout'}</h4><p>Templates produce a relative path beneath this filing system.</p></div>
        {#if sample?.uses_asn}<span class="pill warn">Uses previous archive number</span>{/if}
      </div>
      <label class="field">Layout name
        <input class="input" bind:value={draft.name} placeholder="e.g. Household bills by year" required />
      </label>
      <label class="field">Path template
        <textarea class="input mono" rows="5" bind:value={draft.template} oninput={schedulePreview}
                  placeholder={'Bills/{{ created_year }}/{{ title }}'} required></textarea>
      </label>

      <div class="variable-reference">
        <strong>Supported variables</strong>
        {#each VARIABLES as [group, names]}
          <div class="variable-group">
            <span>{group}</span>
            <div>{#each names as name}<button class="variable" type="button" onclick={() => insertVariable(name)}>{`{{ ${name} }}`}</button>{/each}</div>
          </div>
        {/each}
        <p><code>{`{{ asn }}`}</code> is compatibility-only and works only for imported documents that retain a previous archive number.</p>
      </div>

      <div class="sample" class:error={!!previewError}>
        <span>Sample preview</span>
        {#if previewBusy}<code>Rendering…</code>
        {:else if previewError}<code role="alert">{previewError}</code>
        {:else if sample}<code>{sample.path}</code>
        {:else}<code>Enter a valid template to preview its path.</code>{/if}
      </div>
      <div class="editor-actions">
        {#if editingID != null}<button class="btn" type="button" disabled={busy} onclick={cancelEdit}>Cancel</button>{/if}
        <button class="btn primary" disabled={busy || previewBusy || !!previewError || !draft.name.trim() || !draft.template.trim()}>
          <Icon name={editingID == null ? 'plus' : 'check'} size={14} />{busy ? 'Saving…' : editingID == null ? 'Create layout' : 'Save layout'}
        </button>
      </div>
    </form>

    <section class="saved-layouts" aria-labelledby="saved-layouts-heading">
      <h4 id="saved-layouts-heading">Saved layouts</h4>
      {#if error}<div class="err" role="alert">{error} <button class="btn sm" onclick={load}>Retry</button></div>
      {:else if loading}<p class="empty">Loading folder layouts…</p>
      {:else}
        <div class="layout-list">
          {#each rows as row (row.id)}
            <article class="layout-row" class:active={editingID === row.id}>
              <div class="layout-row-heading">
                <strong>{row.name}</strong>
                {#if row.uses_asn}<span class="pill warn">Previous archive number</span>{/if}
              </div>
              <code>{row.path}</code>
              <div class="row-actions">
                <button class="btn sm" onclick={() => edit(row)}>Edit</button>
                <button class="btn sm danger" onclick={() => (deleteRequest = row)}><Icon name="trash" size={12} />Delete</button>
              </div>
            </article>
          {:else}
            <p class="empty">No folder layouts yet. Documents use the default filing-tree folders until a layout is assigned.</p>
          {/each}
        </div>
      {/if}
    </section>
  </div>
</section>

{#if deleteRequest}
  <ConfirmDialog
    title="Delete folder layout?"
    message={`“${deleteRequest.name}” will be removed. Documents assigned to it return to the default filing-tree folders.`}
    confirmLabel="Delete layout"
    busyLabel="Deleting…"
    busy={busy}
    onConfirm={remove}
    onCancel={() => (deleteRequest = null)} />
{/if}

<style>
  .layout-workspace { display:grid; gap:18px; }
  .layout-heading, .editor-heading, .layout-row-heading, .editor-actions, .row-actions { display:flex; align-items:flex-start; justify-content:space-between; gap:12px; }
  .layout-heading h3, .editor-heading h4, .saved-layouts h4 { margin:0; }
  .layout-heading h3 { margin-top:3px; font-size:1.05rem; }
  .layout-heading p, .editor-heading p { max-width:66ch; margin:5px 0 0; color:var(--muted); font-size:.8rem; line-height:1.5; }
  .layout-grid { display:grid; grid-template-columns:minmax(0,1.2fr) minmax(280px,.8fr); align-items:start; gap:18px; }
  .layout-editor { display:grid; gap:14px; }
  .layout-editor .field { display:grid; gap:6px; color:var(--muted); font-size:.76rem; font-weight:650; }
  .layout-editor textarea { width:100%; max-width:none; min-height:110px; resize:vertical; line-height:1.45; }
  .variable-reference { display:grid; gap:8px; padding:11px; border:1px solid var(--line); border-radius:9px; background:var(--bg); }
  .variable-reference > strong { font-size:.76rem; }
  .variable-reference > p { margin:2px 0 0; color:var(--muted); font-size:.68rem; line-height:1.45; }
  .variable-group { display:grid; grid-template-columns:86px minmax(0,1fr); gap:8px; }
  .variable-group > span { padding-top:4px; color:var(--muted); font-size:.67rem; font-weight:650; }
  .variable-group > div { display:flex; flex-wrap:wrap; gap:4px; }
  .variable { padding:3px 6px; border:1px solid var(--line); border-radius:5px; background:var(--surface); color:var(--ink); font:inherit; font-family:var(--mono); font-size:.63rem; cursor:pointer; }
  .variable:hover { border-color:var(--accent); color:var(--accent); }
  .sample { display:grid; gap:5px; padding:11px; border-left:3px solid var(--accent); border-radius:7px; background:var(--tint); }
  .sample.error { border-left-color:var(--danger); background:var(--danger-soft); }
  .sample > span { color:var(--muted); font-size:.67rem; font-weight:650; text-transform:uppercase; letter-spacing:.05em; }
  .sample code { overflow-wrap:anywhere; color:var(--ink); font-size:.75rem; }
  .editor-actions { justify-content:flex-end; }
  .saved-layouts { min-width:0; }
  .saved-layouts > h4 { margin-bottom:10px; font-size:.86rem; }
  .layout-list { display:grid; gap:9px; }
  .layout-row { display:grid; gap:8px; padding:12px; border:1px solid var(--line); border-radius:10px; background:var(--surface); }
  .layout-row.active { border-color:var(--accent); background:var(--tint); }
  .layout-row-heading strong { min-width:0; overflow-wrap:anywhere; font-size:.82rem; }
  .layout-row code { overflow-wrap:anywhere; color:var(--muted); font-size:.7rem; line-height:1.45; }
  .row-actions { justify-content:flex-end; }
  .empty { margin:0; padding:18px; color:var(--muted); font-size:.78rem; text-align:center; }
  @media (max-width:820px) {
    .layout-grid { grid-template-columns:1fr; }
  }
  @media (max-width:560px) {
    .layout-heading, .editor-heading { flex-direction:column; }
    .variable-group { grid-template-columns:1fr; }
  }
</style>
