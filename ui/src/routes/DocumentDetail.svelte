<script>
  import { getDocument, patchDocument, deleteDocument, documentVersions, createShareLink, listJDCategories, previewPath, downloadPath, similarDocs } from '../lib/api.js'
  import { go } from '../lib/router.svelte.js'
  import { fmtDate, fmtBytes, sensDot } from '../lib/format.js'
  import Icon from '../lib/Icon.svelte'

  let { id, notify } = $props()

  let doc = $state(null)
  let versions = $state([])
  let err = $state('')
  let revealed = $state(false)
  let editingTitle = $state(false)
  let titleDraft = $state('')
  let shareURL = $state('')
  let jdCats = $state([])
  let similar = $state(null)   // {results, method} | null

  const blurred = $derived(doc?.sensitivity === 'confidential' && !revealed)
  const areaGroups = $derived.by(() => {
    const m = new Map()
    for (const c of jdCats) {
      const lo = Number(c.area_code)
      if (!m.has(lo)) m.set(lo, { lo, name: c.area_name, categories: [] })
      m.get(lo).categories.push(c)
    }
    return [...m.values()].sort((a, b) => a.lo - b.lo)
  })

  async function load() {
    err = ''
    try {
      doc = await getDocument(id)
      titleDraft = doc.title
      documentVersions(id).then(v => (versions = v?.results || v || [])).catch(() => {})
      similarDocs(id).then(r => (similar = r)).catch(() => (similar = null))
      listJDCategories().then(r => (jdCats = r?.results || [])).catch(() => {})
    } catch (ex) { err = ex.message || 'Could not load this document.' }
  }

  async function save(patch, label) {
    try {
      await patchDocument(id, patch)
      doc = { ...doc, ...patch }
      notify?.(label || 'Saved')
    } catch (ex) { notify?.(ex.message || 'Could not save') }
  }

  async function editLanguages() {
    // Simple prompt-based edit — matches how title / sensitivity
    // land on this page today. Empty string clears both value +
    // lock so future automatic detection can populate it again.
    const current = doc?.languages || ''
    const next = window.prompt(
      'Languages — comma-separated ISO codes (e.g. "de,en"). Empty clears + unlocks.',
      current
    )
    if (next === null) return
    const trimmed = next.trim()
    try {
      await patchDocument(id, { languages: trimmed })
      doc = {
        ...doc,
        languages: trimmed,
        languages_locked: trimmed !== '',
      }
      notify?.(trimmed ? 'Languages updated' : 'Languages cleared')
    } catch (ex) { notify?.(ex.message || 'Could not update languages') }
  }

  async function share() {
    try {
      const res = await createShareLink({ doc_ids: [Number(id)], label: doc?.title || '' })
      shareURL = res?.public_url ? location.origin + res.public_url
               : res?.token ? `${location.origin}/s/${res.token}` : ''
      if (shareURL && navigator.clipboard) {
        await navigator.clipboard.writeText(shareURL)
        notify?.('Share link copied')
      }
    } catch (ex) { notify?.(ex.message || 'Could not create a share link') }
  }

  async function trash() {
    if (!confirm('Move this document to trash?')) return
    try { await deleteDocument(id); notify?.('Moved to trash'); go('#/documents') }
    catch (ex) { notify?.(ex.message || 'Could not delete') }
  }

  $effect(() => { id; revealed = false; load() })
</script>

<div class="toolbar">
  <a class="btn sm" href="#/documents"><Icon name="left" size={13} /> All documents</a>
  <span class="spacer"></span>
  <a class="btn sm" href={downloadPath(id)} download><Icon name="download" size={13} /> Download</a>
  <button class="btn sm" onclick={share}><Icon name="link" size={13} /> Share</button>
  <button class="btn sm danger" onclick={trash}><Icon name="trash" size={13} /> Trash</button>
</div>

{#if err}<div class="err">{err}</div>{/if}

{#if doc}
  <div class="detail">
    <div class="preview" class:blurred>
      {#if !blurred}
        <!-- direct URL: session cookie authenticates; server CSP sandboxes;
             streaming + immutable-ETag caching come back for free -->
        <iframe src={previewPath(id, doc.sensitivity === 'confidential')} title="Document preview"></iframe>
      {/if}
      {#if blurred}
        <div class="reveal">
          <span class="pill danger">Confidential</span>
          <button class="btn" onclick={() => (revealed = true)}><Icon name="eye" size={14} /> Reveal preview</button>
        </div>
      {/if}
    </div>

    <div style="display:flex;flex-direction:column;gap:14px">
      <div class="card">
        {#if editingTitle}
          <form onsubmit={(e) => { e.preventDefault(); editingTitle = false; save({ title: titleDraft }, 'Title saved') }}>
            <input class="input" bind:value={titleDraft} />
          </form>
        {:else}
          <h2 style="font-size:1.15rem">
            <button style="all:unset;cursor:text" title="Rename" onclick={() => (editingTitle = true)}>
              <span class="dot {sensDot(doc.sensitivity)}" style="display:inline-block;margin-right:8px"></span>{doc.title || `Document #${doc.id}`}
            </button>
          </h2>
        {/if}

        <dl class="kv" style="margin-top:12px">
          <dt>Filed under</dt>
          <dd>
            {#if jdCats.length}
              <select class="input" style="padding:4px 8px;font-size:.8rem"
                      value={doc.jd_category_id}
                      onchange={(e) => save({ jd_category_id: Number(e.target.value) }, 'Refiled')}>
                {#each areaGroups as g}
                  <optgroup label={`${g.lo}–${g.lo + 9} ${g.name}`}>
                    {#each g.categories as c}<option value={c.id}>{c.code} {c.name}</option>{/each}
                  </optgroup>
                {/each}
              </select>
            {:else if doc.jd_category_code}
              <span class="chip">{doc.jd_category_code} {doc.jd_category_name}</span>
              <span class="sub" style="margin-left:6px">{doc.jd_area_name}</span>
            {:else}<span class="chip">jd {doc.jd_category_id}</span>{/if}
          </dd>
          <dt>Sensitivity</dt>
          <dd>
            <select class="input" style="padding:4px 8px;font-size:.8rem"
                    value={doc.sensitivity || ''}
                    onchange={(e) => save({ sensitivity: e.target.value || null }, 'Sensitivity saved')}>
              <option value="">Unset</option>
              <option value="public">Public</option>
              <option value="internal">Internal</option>
              <option value="confidential">Confidential</option>
            </select>
          </dd>
          <dt>Added</dt><dd>{fmtDate(doc.created_at)}</dd>
          <dt>Original</dt><dd>{doc.mime_type} · {fmtBytes(doc.original_size)}</dd>
          {#if doc.archive_blob}<dt>Archive</dt><dd>searchable PDF · {fmtBytes(doc.archive_size)}</dd>{/if}
          {#if doc.tags?.length}
            <dt>Tags</dt><dd>{#each doc.tags as t}<span class="pill" style="margin-right:5px">{t}</span>{/each}</dd>
          {/if}
          {#if doc.correspondents?.length}
            <dt>Correspondents</dt>
            <dd>{#each doc.correspondents as c}<span class="pill" style="margin-right:5px">{c.name} · {c.role}</span>{/each}</dd>
          {/if}
          <dt>Languages</dt>
          <dd>
            {#if doc.languages}
              {#each doc.languages.split(',') as code}
                <span class="pill" style="margin-right:5px">{code.trim()}</span>
              {/each}
              {#if doc.languages_locked}<span class="sub" title="Set by user; automatic detection won't overwrite">· locked</span>{/if}
            {:else}
              <span class="sub">not detected</span>
            {/if}
            <button class="btn sm" style="margin-left:8px" onclick={editLanguages} type="button">Edit</button>
          </dd>
        </dl>
        {#if shareURL}
          <div class="field" style="margin-top:12px;margin-bottom:0">
            <label for="share">Share link (copied)</label>
            <input id="share" class="input mono" style="font-size:.76rem" readonly value={shareURL} />
          </div>
        {/if}
      </div>

      {#if versions.length > 1}
        <div class="card">
          <h3>Versions</h3>
          <div class="index" style="border:0">
            {#each versions as v}
              <a class="irow" href={`#/doc/${v.id}`} style="padding:8px 4px">
                <span class="dot" class:accent={String(v.id) === String(id)}></span>
                <span class="title grow">#{v.id} {v.title || ''}</span>
                <span class="sub">{fmtDate(v.created_at)}</span>
              </a>
            {/each}
          </div>
        </div>
      {/if}

      {#if similar?.results?.length}
        <div class="card">
          <h3 style="display:flex;align-items:center;gap:8px">Similar documents
            <span class="pill" title={similar.method === 'fts' ? 'lexical (FTS5 more-like-this)' : 'semantic'}>{similar.method}</span>
          </h3>
          <div class="index" style="border:0">
            {#each similar.results.slice(0, 6) as sd (sd.id)}
              <a class="irow" href={`#/doc/${sd.id}`} style="padding:8px 4px">
                <span class="dot"></span>
                {#if sd.jd_category_id}<span class="chip">jd</span>{/if}
                <span class="title grow">{sd.title || `Document #${sd.id}`}</span>
                <span class="sub">{fmtDate(sd.created_at)}</span>
              </a>
            {/each}
          </div>
        </div>
      {/if}

      {#if doc.content}
        <div class="card">
          <h3>Extracted text</h3>
          <p class="sub" style="white-space:pre-wrap;max-height:220px;overflow:auto;font-size:.8rem;color:var(--muted);margin:0">{doc.content.slice(0, 2000)}{doc.content.length > 2000 ? '…' : ''}</p>
        </div>
      {/if}
    </div>
  </div>
{/if}
