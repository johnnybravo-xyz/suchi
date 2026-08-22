<script>
  import { getDocument, patchDocument, deleteDocument, documentVersions, createShareLink, listShareLinks, deleteShareLink, listJDCategories, previewPath, downloadPath, similarDocs, listGrants, putGrant, deleteGrant } from '../lib/api.js'
  import { go } from '../lib/router.svelte.js'
  import { fmtDate, fmtBytes, sensDot } from '../lib/format.js'
  import { session } from '../lib/session.svelte.js'
  import Icon from '../lib/Icon.svelte'
  import ConfirmDialog from '../lib/ConfirmDialog.svelte'

  let { id, notify } = $props()

  let doc = $state(null)
  let versions = $state([])
  let err = $state('')
  let revealed = $state(false)
  let editingTitle = $state(false)
  let titleDraft = $state('')
  let editingLanguages = $state(false)
  let languagesDraft = $state('')
  let shareURL = $state('')
  let jdCats = $state([])
  let similar = $state(null)   // {results, method} | null
  let access = $state(null)    // owner/admin-only {results, principals}
  let canManageAccess = $state(false)
  let accessDraft = $state({ principal: '', perm_bits: '1' })
  let trashOpen = $state(false)
  let trashBusy = $state(false)

  const ACCESS_LEVELS = [
    ['1', 'View'],
    ['3', 'Edit'],
    ['7', 'Full control'],
  ]

  const blurred = $derived(doc?.sensitivity === 'confidential' && !revealed)
  // Inline-previewable formats: archive_blob is always PDF, and browsers
  // render PDF + common images + plain text natively. Everything else
  // (docx/xlsx/pptx, epub, eml, unknown) can't be iframed without
  // triggering a save/download dialog, so we swap the iframe for a
  // small "no inline preview" panel and lean on the extracted-text card
  // below. Keeps parity with previewPath's server-side content-type.
  const previewable = $derived.by(() => {
    if (!doc) return false
    if (doc.archive_blob) return true
    const m = (doc.mime_type || '').toLowerCase().split(';')[0].trim()
    if (m === 'application/pdf') return true
    if (m.startsWith('image/')) {
      return ['image/png', 'image/jpeg', 'image/gif', 'image/webp', 'image/svg+xml', 'image/avif'].includes(m)
    }
    if (m === 'text/plain' || m === 'text/html' || m === 'text/csv' || m === 'text/markdown') return true
    return false
  })
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
    access = null
    canManageAccess = false
    try {
      doc = await getDocument(id)
      titleDraft = doc.title
      documentVersions(id).then(v => (versions = v?.results || v || [])).catch(() => {})
      similarDocs(id).then(r => (similar = r)).catch(() => (similar = null))
      listJDCategories().then(r => (jdCats = r?.results || [])).catch(() => {})
      loadAccess()
    } catch (ex) { err = ex.message || 'Could not load this document.' }
  }

  async function loadAccess() {
    try {
      access = await listGrants('document', id)
      canManageAccess = true
    } catch (ex) {
      access = null
      canManageAccess = false
      if (ex.status !== 403) notify?.(ex.message || 'Could not load document access')
    }
  }

  function principalKey(principal) {
    return `${principal.kind}:${principal.id}`
  }

  function principalFor(grant) {
    return access?.principals?.find(p => p.kind === grant.principal_kind && Number(p.id) === Number(grant.principal_id))
  }

  function availablePrincipals(kind) {
    const granted = new Set((access?.results || []).map(g => `${g.principal_kind}:${g.principal_id}`))
    return (access?.principals || []).filter(p =>
      p.kind === kind && !p.disabled && !granted.has(principalKey(p)) &&
      !(p.kind === 'user' && Number(p.id) === Number(session.user?.user_id))
    )
  }

  function accessLabel(bits) {
    return ACCESS_LEVELS.find(([value]) => Number(value) === Number(bits))?.[1] || 'Custom'
  }

  function sourceLabel(source) {
    if (source.kind === 'upload') return source.label ? `Uploaded by ${source.label}` : 'Manual upload'
    if (source.kind === 'api') return source.label ? `API upload by ${source.label}` : 'API upload'
    if (source.kind === 'mailbox') return source.label || 'Mailbox'
    if (source.kind === 'watched_folder') return source.label || 'Watched folder'
    if (source.kind === 'import') return source.label || 'Import'
    return source.label || 'Unknown source'
  }

  async function grantAccess(e) {
    e.preventDefault()
    const [principalKind, rawID] = accessDraft.principal.split(':')
    const principalID = Number(rawID)
    if (!principalKind || !principalID) return
    try {
      await putGrant('document', id, {
        principal_kind: principalKind,
        principal_id: principalID,
        perm_bits: Number(accessDraft.perm_bits),
      })
      accessDraft = { principal: '', perm_bits: '1' }
      await loadAccess()
      notify?.('Access granted')
    } catch (ex) { notify?.(ex.message || 'Could not grant access') }
  }

  async function changeAccess(grant, bits) {
    try {
      await putGrant('document', id, {
        principal_kind: grant.principal_kind,
        principal_id: grant.principal_id,
        perm_bits: Number(bits),
      })
      grant.perm_bits = Number(bits)
      notify?.('Access updated')
    } catch (ex) { notify?.(ex.message || 'Could not update access') }
  }

  async function revokeAccess(grant) {
    try {
      await deleteGrant('document', id, grant.principal_kind, grant.principal_id)
      access = { ...access, results: access.results.filter(g => g.id !== grant.id) }
      notify?.('Access revoked')
    } catch (ex) { notify?.(ex.message || 'Could not revoke access') }
  }

  async function save(patch, label) {
    try {
      await patchDocument(id, patch)
      doc = { ...doc, ...patch }
      notify?.(label || 'Saved')
    } catch (ex) { notify?.(ex.message || 'Could not save') }
  }

  function startEditLanguages() {
    languagesDraft = doc?.languages || ''
    editingLanguages = true
  }

  async function saveLanguages() {
    const trimmed = languagesDraft.trim()
    editingLanguages = false
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

  // ---- share dialog: expiry + optional password + existing links ----
  let shareOpen = $state(false)
  let shareLinks = $state([])
  let sh = $state({ expiry: '0', password: '' })
  const EXPIRIES = [['0', 'Never expires'], ['86400', '1 day'], ['604800', '7 days'], ['2592000', '30 days']]

  async function openShare() {
    shareOpen = true
    try {
      const r = await listShareLinks()
      shareLinks = (r?.results || r || []).filter(l => (l.doc_ids || []).includes(Number(id)))
    } catch { shareLinks = [] }
  }
  async function makeLink() {
    try {
      const res = await createShareLink({
        doc_ids: [Number(id)], label: doc?.title || '',
        expires_in_sec: Number(sh.expiry), password: sh.password,
      })
      const url = location.origin + (res?.public_url || `/s/${res?.token}`)
      await navigator.clipboard?.writeText(url)
      shareURL = url
      notify?.(sh.password ? 'Password-protected link copied' : 'Share link copied')
      sh = { expiry: '0', password: '' }
      openShare()
    } catch (ex) { notify?.(ex.message || 'Could not create the link') }
  }
  async function revoke(l) {
    try { await deleteShareLink(l.id); shareLinks = shareLinks.filter(x => x.id !== l.id); notify?.('Link revoked') }
    catch (ex) { notify?.(ex.message || 'Could not revoke') }
  }

  async function trash() {
    trashBusy = true
    try {
      await deleteDocument(id)
      trashOpen = false
      notify?.('Moved to trash')
      go('#/documents')
    } catch (ex) { notify?.(ex.message || 'Could not delete') }
    finally { trashBusy = false }
  }

  $effect(() => { id; revealed = false; load() })
</script>

<div class="toolbar">
  <a class="btn sm" href="#/documents"><Icon name="left" size={13} /> All documents</a>
  <span class="spacer"></span>
  {#if doc?.sensitivity === 'confidential'}
    <!-- Persistent Reveal/Hide toggle. The in-panel Reveal button
         (inside the preview) still works — this one gives a symmetric
         way to re-hide without navigating away and back. -->
    <button class="btn sm" onclick={() => (revealed = !revealed)}
            title={revealed ? 'Hide preview + extracted text' : 'Reveal preview + extracted text'}>
      <Icon name="eye" size={13} /> {revealed ? 'Hide' : 'Reveal'}
    </button>
  {/if}
  <a class="btn sm" href={downloadPath(id)} download><Icon name="download" size={13} /> Download</a>
  {#if canManageAccess}<button class="btn sm" onclick={openShare}><Icon name="link" size={13} /> Share</button>{/if}
  <button class="btn sm danger" onclick={() => (trashOpen = true)}><Icon name="trash" size={13} /> Trash</button>
</div>

{#if err}<div class="err">{err}</div>{/if}

{#if doc}
  <div class="detail">
    <div class="preview" class:blurred>
      {#if blurred}
        <div class="reveal">
          <span class="pill danger">Confidential</span>
          <button class="btn" onclick={() => (revealed = true)}><Icon name="eye" size={14} /> Reveal preview</button>
        </div>
      {:else if previewable}
        <!-- direct URL: session cookie authenticates; server CSP sandboxes;
             streaming + immutable-ETag caching come back for free -->
        <iframe src={previewPath(id, doc.sensitivity === 'confidential')} title="Document preview"></iframe>
      {:else}
        <div class="reveal">
          <span class="pill">{doc.mime_type || 'unknown format'}</span>
          <b style="font-size:.95rem">No inline preview for this format</b>
          <span class="sub" style="max-width:32ch;text-align:center">Browsers can't render this file inline. Download the original, or read the extracted text below.</span>
          <a class="btn" href={downloadPath(id)} download><Icon name="download" size={13} /> Download original</a>
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
          <dt>Added</dt><dd>{fmtDate(doc.added_at || doc.created_at)}</dd>
          {#if doc.sources?.length}
            <dt>{doc.sources.length === 1 ? 'Source' : 'Sources'}</dt>
            <dd style="display:flex;flex-direction:column;gap:4px;min-width:0">
              {#each doc.sources as source, i}
                <div style="min-width:0;overflow-wrap:anywhere">
                  <span>{sourceLabel(source)}</span>
                  {#if source.detail}<span class="sub"> · {source.detail}</span>{/if}
                  {#if i === 0 && doc.sources.length > 1}<span class="pill" style="margin-left:6px;font-size:.68rem">first seen</span>{/if}
                </div>
              {/each}
            </dd>
          {/if}
          {#if doc.source_mtime || (doc.created_at && doc.created_at !== doc.added_at)}
            <dt title="Date carried from the source at ingest">Source date</dt>
            <dd>{fmtDate(doc.source_mtime || doc.created_at)}</dd>
          {/if}
          <dt>Original</dt><dd>{doc.mime_type} · {fmtBytes(doc.original_size)}</dd>
          {#if doc.archive_blob}
            <dt>Archive</dt>
            <dd title={doc.content ? 'Full-text searchable — OCR / extraction populated documents.content' : 'PDF wrapper only — no OCR layer. Install tesseract or ocrmypdf and rescan to make this searchable.'}>
              {doc.content ? 'searchable PDF' : 'PDF preview (no OCR)'} · {fmtBytes(doc.archive_size)}
            </dd>
          {/if}
          {#if doc.tags?.length}
            <dt>Tags</dt><dd>{#each doc.tags as t}<span class="pill" style="margin-right:5px">{t}</span>{/each}</dd>
          {/if}
          {#if doc.correspondents?.length}
            <dt>Correspondents</dt>
            <dd>{#each doc.correspondents as c}<span class="pill" style="margin-right:5px">{c.name} · {c.role}</span>{/each}</dd>
          {/if}
          <dt>Languages</dt>
          <dd>
            {#if editingLanguages}
              <form onsubmit={(e) => { e.preventDefault(); saveLanguages() }} style="display:flex;gap:6px;align-items:center;flex-wrap:wrap">
                <input class="input mono" style="max-width:200px" bind:value={languagesDraft}
                       placeholder="e.g. de,en — empty clears" />
                <button class="btn sm" type="submit">Save</button>
                <button class="btn sm" type="button" onclick={() => (editingLanguages = false)}>Cancel</button>
                <span class="sub" style="width:100%">Comma-separated ISO codes. Empty clears &amp; unlocks.</span>
              </form>
            {:else}
              {#if doc.languages}
                {#each doc.languages.split(',') as code}
                  <span class="pill" style="margin-right:5px">{code.trim()}</span>
                {/each}
                {#if doc.languages_locked}<span class="sub" title="Set by user; automatic detection won't overwrite">· locked</span>{/if}
              {:else}
                <span class="sub">not detected</span>
              {/if}
              <button class="btn sm" style="margin-left:8px" onclick={startEditLanguages} type="button">Edit</button>
            {/if}
          </dd>
        </dl>
        {#if shareURL}
          <div class="field" style="margin-top:12px;margin-bottom:0">
            <label for="share">Share link (copied)</label>
            <input id="share" class="input mono" style="font-size:.76rem" readonly value={shareURL} />
          </div>
        {/if}
      </div>

      {#if access}
        <div class="card">
          <h3 style="display:flex;align-items:center;gap:8px">
            <Icon name="shield" size={14} /> Access
            <span class="pill">{access.results?.length || 0}</span>
          </h3>
          <form class="toolbar" style="margin-bottom:{access.results?.length ? '10px' : '0'}" onsubmit={grantAccess}>
            <select class="input" style="flex:1;max-width:none;min-width:180px" bind:value={accessDraft.principal} aria-label="Person or group">
              <option value="">Select a person or group</option>
              {#if availablePrincipals('user').length}
                <optgroup label="People">
                  {#each availablePrincipals('user') as person (person.id)}
                    <option value={principalKey(person)}>{person.name}{person.email && person.email !== person.name ? ` · ${person.email}` : ''}</option>
                  {/each}
                </optgroup>
              {/if}
              {#if availablePrincipals('group').length}
                <optgroup label="Groups">
                  {#each availablePrincipals('group') as group (group.id)}
                    <option value={principalKey(group)}>{group.name}</option>
                  {/each}
                </optgroup>
              {/if}
            </select>
            <select class="input" style="max-width:130px" bind:value={accessDraft.perm_bits} aria-label="Access level">
              {#each ACCESS_LEVELS as [value, label]}<option {value}>{label}</option>{/each}
            </select>
            <button class="btn primary sm" disabled={!accessDraft.principal}><Icon name="plus" size={12} /> Add</button>
          </form>
          {#if access.results?.length}
            <div class="index" style="border:0">
              {#each access.results as grant (grant.id)}
                {@const principal = principalFor(grant)}
                <div class="irow" style="padding:7px 2px;cursor:default">
                  <span class="dot" class:accent={grant.principal_kind === 'group'}></span>
                  <span class="grow" style="min-width:0">
                    <span class="title" style="display:block;font-size:.82rem">{principal?.name || `${grant.principal_kind} #${grant.principal_id}`}</span>
                    <span class="sub" style="display:block;overflow:hidden;text-overflow:ellipsis">{principal?.email || (grant.principal_kind === 'group' ? 'Group' : '')}</span>
                  </span>
                  <select class="input" style="width:118px;padding:4px 7px;font-size:.76rem" value={String(grant.perm_bits)}
                          aria-label={`Access for ${principal?.name || grant.principal_kind}`}
                          title={accessLabel(grant.perm_bits)} onchange={(e) => changeAccess(grant, e.target.value)}>
                    {#each ACCESS_LEVELS as [value, label]}<option {value}>{label}</option>{/each}
                  </select>
                  <button class="btn sm danger" title="Revoke access" aria-label="Revoke access" onclick={() => revokeAccess(grant)}><Icon name="trash" size={12} /></button>
                </div>
              {/each}
            </div>
          {/if}
        </div>
      {/if}

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

      {#if similar}
        <div class="card">
          <h3 style="display:flex;align-items:center;gap:8px">Similar documents
            <span class="pill" title={similar.method === 'fts' ? 'lexical (FTS5 more-like-this)' : 'semantic'}>{similar.method}</span>
            {#if similar.matched_on_title_only && similar.results?.length}
              <span class="pill warn"
                    title="This document has no extracted text; matches are based on title alone and may be noisy.">
                title-only match
              </span>
            {/if}
          </h3>
          {#if similar.results?.length}
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
          {:else}
            <p class="sub" style="margin:8px 4px 0;font-style:italic;opacity:.75;font-size:.8rem">
              {#if similar.matched_on_title_only}
                Nothing overlaps the title strongly enough. Reingesting this document so its text is extracted will usually surface more.
              {:else}
                Nothing in the archive overlaps this document's vocabulary yet.
              {/if}
            </p>
          {/if}
        </div>
      {/if}

      {#if doc.content}
        <div class="card">
          <h3 style="display:flex;align-items:center;gap:8px">
            Extracted text
            {#if blurred}
              <span class="pill danger" style="font-size:.7rem">Confidential</span>
              <button class="btn sm" style="margin-left:auto" onclick={() => (revealed = true)}><Icon name="eye" size={12} /> Reveal</button>
            {/if}
          </h3>
          <p class="sub extracted"
             class:blurred
             style="white-space:pre-wrap;max-height:220px;overflow:auto;font-size:.8rem;color:var(--muted);margin:0"
             aria-hidden={blurred}>{doc.content.slice(0, 2000)}{doc.content.length > 2000 ? '…' : ''}</p>
        </div>
      {/if}
    </div>
  </div>
{/if}

{#if trashOpen}
  <ConfirmDialog
    title="Move document to trash?"
    message={`“${doc?.title || `Document #${id}`}” will move to Trash, where it can be restored.`}
    confirmLabel="Move to trash"
    busy={trashBusy}
    onConfirm={trash}
    onCancel={() => (trashOpen = false)} />
{/if}

{#if shareOpen}
  <div class="modal-veil" onclick={() => (shareOpen = false)} role="presentation">
    <div class="modal" style="width:min(520px,94vw)" onclick={(e) => e.stopPropagation()} onkeydown={(e) => { if (e.key === 'Escape') shareOpen = false }} role="dialog" aria-label="Share document" tabindex="-1">
      <div class="modal-head">
        <h3>Share “{doc?.title || `Document #${id}`}”</h3>
        <button class="btn sm" onclick={() => (shareOpen = false)}><Icon name="x" size={13} /></button>
      </div>
      <p class="sub" style="color:var(--muted);font-size:.82rem;margin:0 0 12px">
        Anyone with the link can view and download. No account needed on their side.
      </p>
      <div class="toolbar" style="margin:0 0 10px">
        <select class="input" bind:value={sh.expiry} aria-label="Link expiry">
          {#each EXPIRIES as [v, label]}<option value={v}>{label}</option>{/each}
        </select>
        <input class="input" type="password" style="flex:1" placeholder="Password (optional)"
               bind:value={sh.password} autocomplete="new-password" />
        <button class="btn primary sm" onclick={makeLink}><Icon name="link" size={13} /> Create &amp; copy</button>
      </div>
      {#if shareURL}
        <input class="input mono" style="font-size:.74rem;margin-bottom:12px" readonly value={shareURL}
               onclick={(e) => e.target.select()} />
      {/if}
      {#if shareLinks.length}
        <h3 style="font-size:.85rem;margin:6px 0 4px">Active links</h3>
        <div class="index" style="border:0">
          {#each shareLinks as l (l.id)}
            <div class="irow" style="padding:7px 2px">
              <span class="dot" class:warn={l.has_passwd || l.HasPasswd}></span>
              <span class="title grow mono" style="font-size:.74rem">{l.public_url || `/s/${l.token}`}</span>
              {#if l.has_passwd || l.HasPasswd}<span class="pill warn">password</span>{/if}
              <button class="btn sm" onclick={() => revoke(l)}>Revoke</button>
            </div>
          {/each}
        </div>
      {/if}
    </div>
  </div>
{/if}
