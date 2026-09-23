<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { captureScope, systems } from '../lib/systems.svelte.js'
  import { adminListUsers, adminPatchUser,
           listGroups, createGroup, deleteGroup, groupMembers, addGroupMember, removeGroupMember,
           listCustomFields, createCustomField, patchCustomField, deleteCustomField,
           listTags, listCorrespondents, listDocumentTypes, listStoragePaths,
           createTaxon, patchTaxon, deleteTaxon, exportTaxonomy } from '../lib/api.js'
  import TaxonomyImport from '../lib/TaxonomyImport.svelte'
  import UserCreateForm from '../lib/UserCreateForm.svelte'
  import Icon from '../lib/Icon.svelte'
  import { USER_CAPABILITIES } from '../lib/capabilities.js'
  import { session } from '../lib/session.svelte.js'

  let { notify, onTaxonomyChanged } = $props()
  let tab = $state('users')

  // ---------- users ----------
  let users = $state([])
  let usersLoaded = $state(false)
  let usersLoading = $state(false)
  let usersError = $state('')
  let userBusy = $state(false)
  let usersLoadVersion = 0

  async function loadUsers() {
    const version = ++usersLoadVersion
    usersLoading = true
    usersError = ''
    try {
      const r = await adminListUsers()
      if (version !== usersLoadVersion) return
      users = r?.results || []
    } catch (ex) {
      if (version === usersLoadVersion) usersError = ex.message || 'Could not load users.'
    } finally {
      if (version === usersLoadVersion) {
        usersLoaded = true
        usersLoading = false
      }
    }
  }


  async function toggleUserCap(u, cap) {
    if (userBusy) return
    const has = (u.capabilities || []).includes(cap.key)
    const newCaps = has ? u.capabilities.filter(c => c !== cap.key) : [...(u.capabilities || []), cap.key]
    userBusy = true
    try {
      await adminPatchUser(u.id, { capabilities: newCaps })
      u.capabilities = newCaps
      notify?.(has ? `Revoked ${cap.label} from ${u.email}` : `Granted ${cap.label} to ${u.email}`)
    } catch (ex) { notify?.(ex.message || 'Could not update capabilities') }
    finally { userBusy = false }
  }

  async function toggleDisabled(u) {
    if (userBusy || u.id === session.user?.user_id) return
    const disabled = !u.disabled
    userBusy = true
    try {
      await adminPatchUser(u.id, { disabled })
      u.disabled = disabled
      notify?.(disabled ? `Disabled ${u.email}` : `Enabled ${u.email}`)
    } catch (ex) { notify?.(ex.message || 'Could not update user') }
    finally { userBusy = false }
  }

  // ---------- groups ----------
  let groups = $state([])
  let groupsLoaded = $state(false)
  let groupsLoading = $state(false)
  let groupsError = $state('')
  let ngName = $state('')
  let openGroup = $state(null)      // {id, members: []}
  let addUID = $state('')

  function availableGroupUsers() {
    const memberIDs = new Set((openGroup?.members || []).map(m => Number(m.user_id ?? m.id)))
    return users.filter(u => !memberIDs.has(Number(u.id)))
  }
  async function loadGroups() {
    groupsLoading = true
    groupsError = ''
    try {
      const r = await listGroups()
      groups = r?.results || r || []
      groupsLoaded = true
    } catch (ex) {
      groupsError = ex.message || 'Could not load groups.'
    } finally { groupsLoading = false }
  }
  async function addGroup(e) {
    e.preventDefault()
    if (!ngName.trim()) return
    try { await createGroup({ name: ngName.trim() }); ngName = ''; notify?.('Group created'); loadGroups() }
    catch (ex) { notify?.(ex.message || 'Could not create the group') }
  }
  async function rmGroup(g) {
    if (!confirm(`Delete group “${g.name}”? Its members will be removed. Shared access must be revoked first.`)) return
    try { await deleteGroup(g.id); groups = groups.filter(x => x.id !== g.id); notify?.('Group deleted') }
    catch (ex) { notify?.(ex.code === 'delete_conflict' ? 'This group is still used for shared access. Revoke those grants before deleting it.' : (ex.message || 'Could not delete')) }
  }
  async function toggleMembers(g) {
    if (openGroup?.id === g.id) { openGroup = null; return }
    addUID = ''
    try { await reloadMembers(g) }
    catch (ex) { notify?.(ex.message || 'Could not load members') }
  }

  async function reloadMembers(g) {
    const r = await groupMembers(g.id)
    openGroup = { id: g.id, members: r?.results || r || [] }
  }

  async function addMember(g) {
    const uid = Number(addUID)
    if (!uid) return
    try { await addGroupMember(g.id, uid); addUID = ''; await reloadMembers(g); notify?.('Member added') }
    catch (ex) { notify?.(ex.message || 'Could not add the member') }
  }
  async function rmMember(g, uid) {
    try { await removeGroupMember(g.id, uid); openGroup = { ...openGroup, members: openGroup.members.filter(m => (m.user_id ?? m.id) !== uid) } }
    catch (ex) { notify?.(ex.message || 'Could not remove') }
  }

  // ---------- custom fields ----------
  let fields = $state([])
  let fieldsLoaded = $state(false)
  let fieldsLoading = $state(false)
  let fieldsError = $state('')
  let nf = $state({ name: '', data_type: 'text' })
  const FIELD_TYPES = {
    text: 'Text', number: 'Number', date: 'Date', bool: 'Yes / no',
    select: 'Single choice', multi: 'Multiple choices', url: 'Web link',
    monetary: 'Money', documentlink: 'Document link',
  }
  async function loadFields() {
    fieldsLoading = true
    fieldsError = ''
    try {
      const r = await listCustomFields()
      fields = r?.results || r || []
      fieldsLoaded = true
    } catch (ex) {
      fieldsError = ex.message || 'Could not load custom fields.'
    } finally { fieldsLoading = false }
  }
  async function addField(e) {
    e.preventDefault()
    if (!nf.name.trim()) return
    try { await createCustomField({ name: nf.name.trim(), data_type: nf.data_type }); nf = { name: '', data_type: 'text' }; notify?.('Field created'); loadFields() }
    catch (ex) { notify?.(ex.message || 'Could not create the field') }
  }
  async function renameField(f, name) {
    if (!name.trim() || name === f.name) return
    try { await patchCustomField(f.id, { name: name.trim() }); f.name = name.trim(); notify?.('Renamed') }
    catch (ex) { notify?.(ex.message || 'Could not rename') }
  }
  async function rmField(f) {
    if (!confirm(`Delete field “${f.name}”? Values on documents are removed.`)) return
    try { await deleteCustomField(f.id); fields = fields.filter(x => x.id !== f.id); notify?.('Field deleted') }
    catch (ex) { notify?.(ex.message || 'Could not delete') }
  }

  // ---------- taxonomy ----------
  const TAXA = [
    { kind: 'tags', label: 'Tags', singular: 'tag', description: 'Labels for finding and grouping documents across your filing tree.', load: listTags },
    { kind: 'correspondents', label: 'Correspondents', singular: 'correspondent', description: 'People and organizations you send documents to or receive them from.', load: listCorrespondents },
    { kind: 'document_types', label: 'Document types', singular: 'document type', description: 'Describe what a document is, such as an invoice, contract, or statement.', load: listDocumentTypes },
    { kind: 'storage_paths', label: 'Storage paths', singular: 'storage path', description: 'Named storage paths used when filing documents.', load: listStoragePaths },
    { kind: 'custom_fields', label: 'Custom fields', description: 'Extra document details, such as an invoice number, renewal date, or web link.' },
  ]
  let taxon = $state('tags')
  const selectedTaxon = $derived(TAXA.find(t => t.kind === taxon))
  let taxImpOpen = $state(false)
  let taxImportBusy = $state(false)
  let exportBusy = $state(false)
  let exportFormat = $state('huml')
  async function importedTaxonomy() {
    await Promise.all([loadTaxa(), onTaxonomyChanged?.()])
  }
  async function doExport(skipSeeds) {
    if (exportBusy || taxImportBusy) return
    exportBusy = true
    try {
      const text = await exportTaxonomy(exportFormat, skipSeeds)
      const a = document.createElement('a')
      a.href = URL.createObjectURL(new Blob([text], { type: 'text/plain' }))
      a.download = `${captureScope().code ? `${captureScope().code}.taxonomy` : `archive${skipSeeds ? '-tree' : ''}`}.${exportFormat}`
      a.click()
      setTimeout(() => URL.revokeObjectURL(a.href), 10_000)
      notify?.(`Exported ${a.download}`)
    } catch (ex) { notify?.(ex.message || 'Export failed') }
    finally { exportBusy = false }
  }
  let taxRows = $state([])
  let taxRowsLoaded = $state(false)
  let taxRowsKind = $state('')
  let taxRowsLoading = $state(false)
  let taxRowsError = $state('')
  let taxLoadVersion = 0
  let ntName = $state('')
  async function loadTaxa() {
    if (taxon === 'custom_fields') { await loadFields(); return }
    const version = ++taxLoadVersion
    const kind = taxon
    const spec = TAXA.find(t => t.kind === taxon)
    taxRowsLoading = true
    taxRowsLoaded = false
    taxRowsError = ''
    try {
      const r = await spec.load()
      if (version !== taxLoadVersion || kind !== taxon) return
      taxRows = r?.results || r || []
      taxRowsKind = kind
      taxRowsLoaded = true
    } catch (ex) {
      if (version === taxLoadVersion) taxRowsError = ex.message || `Could not load ${spec.label.toLowerCase()}.`
    } finally {
      if (version === taxLoadVersion) taxRowsLoading = false
    }
  }
  async function addTaxon(e) {
    e.preventDefault()
    if (!ntName.trim()) return
    try { await createTaxon(taxon, { name: ntName.trim() }); ntName = ''; notify?.('Created'); loadTaxa() }
    catch (ex) { notify?.(ex.message || 'Could not create') }
  }
  async function renameTaxon(row, name) {
    if (!name.trim() || name === row.name) return
    try { await patchTaxon(taxon, row.id, { name: name.trim() }); row.name = name.trim(); notify?.('Renamed') }
    catch (ex) { notify?.(ex.message || 'Could not rename') }
  }
  async function rmTaxon(row) {
    if (!confirm(`Delete “${row.name}”? Documents keep working; the label goes away.`)) return
    try { await deleteTaxon(taxon, row.id); taxRows = taxRows.filter(x => x.id !== row.id); notify?.('Deleted') }
    catch (ex) { notify?.(ex.message || 'Could not delete (in use?)') }
  }

  let loadedTab = ''
  let loadedTaxon = ''
  $effect(() => {
    const selected = tab
    if (selected === loadedTab) return
    loadedTab = selected
    if (selected === 'users') loadUsers()
    else if (selected === 'groups') { loadGroups(); loadUsers() }
  })
  $effect(() => {
    const selected = tab === 'metadata' ? taxon : ''
    if (!selected) { loadedTaxon = ''; return }
    if (selected === loadedTaxon) return
    loadedTaxon = selected
    loadTaxa()
  })
</script>

<nav class="people-nav" aria-label="People and metadata">
  <button class:on={tab === 'users'} aria-pressed={tab === 'users'} disabled={taxImportBusy || exportBusy} onclick={() => (tab = 'users')}>Users</button>
  <button class:on={tab === 'groups'} aria-pressed={tab === 'groups'} disabled={taxImportBusy || exportBusy} onclick={() => (tab = 'groups')}>Groups</button>
  <button class:on={tab === 'metadata'} aria-pressed={tab === 'metadata'} disabled={taxImportBusy || exportBusy} onclick={() => (tab = 'metadata')}>Metadata</button>
  <button class:on={tab === 'files'} aria-pressed={tab === 'files'} disabled={taxImportBusy || exportBusy} onclick={() => (tab = 'files')}>Taxonomy</button>
</nav>

<div class="people-content">
{#if tab === 'users'}
  <header class="panel-heading">
    <h3>Users</h3>
    <p>Manage who can sign in and what they can do.
      {#if systems.introduced}Accounts are server-wide; system membership and document sharing control access.{/if}
    </p>
  </header>
  <section class="card editor" aria-labelledby="create-user-heading">
    <h4 id="create-user-heading">Create a user</h4>
    <UserCreateForm {notify} onCreated={loadUsers} />
  </section>
  <section aria-labelledby="users-heading">
    <h4 class="list-heading" id="users-heading">Existing users</h4>
    {#if usersError}
      <div class="settings-load-state err"><span>{usersError}</span><button class="btn sm" onclick={loadUsers}>Retry</button></div>
    {:else if !usersLoaded || usersLoading}
      <p class="quiet" role="status">Loading users…</p>
    {:else if users.length === 0}
      <p class="empty">No users yet.</p>
    {:else}
      <div class="index">
        {#each users as u (u.id)}
          {@const isCurrentUser = u.id === session.user?.user_id}
          <div class="user-entry">
            <div class="irow user-row">
              <span class="grow identity">
                <span class="title">{u.display_name || u.email}</span>
                {#if u.display_name}<span class="quiet">{u.email}</span>{/if}
              </span>
              <span class="pill">{u.role}</span>
              <span class="switch-control" title={isCurrentUser ? 'You cannot disable your own account.' : undefined}>
                <span>{u.disabled ? 'Disabled' : 'Active'}{isCurrentUser ? ' (you)' : ''}</span>
                <button type="button" class="switch" role="switch" aria-checked={!u.disabled}
                        aria-label={`User ${u.email} active`} disabled={userBusy || isCurrentUser} onclick={() => toggleDisabled(u)}></button>
              </span>
            </div>
            {#if u.role !== 'admin'}
              <details class="user-access">
                <summary>Additional access <span class="quiet">{(u.capabilities || []).length} granted</span></summary>
                <div class="access-options">
                  {#each USER_CAPABILITIES as cap (cap.key)}
                    <label class="access-option">
                      <span><b>{cap.label}</b><small>{cap.description}</small></span>
                      <button type="button" class="switch" role="switch"
                              aria-checked={(u.capabilities || []).includes(cap.key)}
                              aria-label={`${cap.label} capability for ${u.email}`} disabled={userBusy}
                              onclick={() => toggleUserCap(u, cap)}></button>
                    </label>
                  {/each}
                </div>
              </details>
            {/if}
          </div>
        {/each}
      </div>
    {/if}
  </section>

{:else if tab === 'groups'}
  <header class="panel-heading">
    <h3>Groups</h3>
    <p>Share documents with several people at once.
      {#if systems.introduced}Groups are server-wide; joining one does not grant entry to a filing system.{/if}
    </p>
  </header>
  <form class="card editor" onsubmit={addGroup}>
    <h4>Create a group</h4>
    <label class="field">Group name
      <input class="input" placeholder="e.g. Family or Accounts team" bind:value={ngName} required />
    </label>
    <div class="form-actions"><button class="btn primary" disabled={!ngName.trim()}><Icon name="plus" size={14} />Create group</button></div>
  </form>
  <section aria-labelledby="groups-heading">
    <h4 class="list-heading" id="groups-heading">Existing groups</h4>
    {#if groupsError}
      <div class="settings-load-state err"><span>{groupsError}</span><button class="btn sm" onclick={loadGroups}>Retry</button></div>
    {:else if !groupsLoaded || groupsLoading}
      <p class="quiet" role="status">Loading groups…</p>
    {:else}
      <div class="index">
        {#each groups as g (g.id)}
          <div class="group-entry">
            <div class="irow">
              <span class="title grow">{g.name}</span>
              <button class="btn sm" onclick={() => toggleMembers(g)} aria-expanded={openGroup?.id === g.id} aria-controls={`group-members-${g.id}`}>{openGroup?.id === g.id ? 'Hide members' : 'Members'}</button>
              <button class="btn sm danger" onclick={() => rmGroup(g)} title="Delete group" aria-label={`Delete ${g.name}`}><Icon name="trash" size={13} /></button>
            </div>
            {#if openGroup?.id === g.id}
              <div class="group-members" id={`group-members-${g.id}`}>
                {#each openGroup.members as mrow ((mrow.user_id ?? mrow.id))}
                  <div class="irow member-row">
                    <span class="grow identity">
                      <span class="title">{mrow.display_name || mrow.email}</span>
                      {#if mrow.display_name}<span class="quiet">{mrow.email}</span>{/if}
                    </span>
                    <button class="btn sm" aria-label={`Remove ${mrow.email}`} onclick={() => rmMember(g, mrow.user_id ?? mrow.id)}>Remove</button>
                  </div>
                {:else}
                  <p class="quiet">No members yet. Add a person below.</p>
                {/each}
                <div class="member-add">
                  <label class="field">User to add
                    <select class="input" bind:value={addUID} disabled={usersLoading || !!usersError}>
                      <option value="">Select a user by name or email</option>
                      {#each availableGroupUsers() as u (u.id)}
                        <option value={String(u.id)} disabled={u.disabled}>
                          {u.display_name ? `${u.display_name} · ${u.email}` : u.email}{u.disabled ? ' · disabled' : ''}
                        </option>
                      {/each}
                    </select>
                  </label>
                  <button class="btn" disabled={!addUID || usersLoading || !!usersError} onclick={() => addMember(g)}><Icon name="plus" size={14} />Add member</button>
                </div>
                {#if usersError}<div class="settings-load-state err"><span>{usersError}</span><button class="btn sm" onclick={loadUsers}>Retry</button></div>{/if}
              </div>
            {/if}
          </div>
        {:else}
          <p class="empty">No groups yet. Create one to share documents with your family or team.</p>
        {/each}
      </div>
    {/if}
  </section>

{:else if tab === 'metadata'}
  <header class="panel-heading metadata-heading">
    <div><h3>Metadata</h3><p>Manage the labels and extra details used on documents in this filing system.</p></div>
    <label class="field">Metadata type
      <select class="input" bind:value={taxon} onchange={() => { ntName = '' }}>
        {#each TAXA as t}<option value={t.kind}>{t.label}</option>{/each}
      </select>
    </label>
  </header>
  <section class="card editor" aria-labelledby="metadata-heading">
    <h4 id="metadata-heading">{selectedTaxon.label}</h4>
    <p class="editor-description">{selectedTaxon.description}</p>
    {#if taxon === 'custom_fields'}
      <form onsubmit={addField}>
        <div class="field-grid">
          <label class="field">Field name
            <input class="input" placeholder="e.g. Invoice number" bind:value={nf.name} required />
          </label>
          <label class="field">Field type
            <select class="input" bind:value={nf.data_type}>
              {#each Object.entries(FIELD_TYPES) as [type, label]}<option value={type}>{label}</option>{/each}
            </select>
          </label>
        </div>
        <div class="form-actions"><button class="btn primary" disabled={!nf.name.trim()}><Icon name="plus" size={14} />Create field</button></div>
      </form>
    {:else}
      <form onsubmit={addTaxon}>
        <label class="field">Name
          <input class="input" placeholder={`New ${selectedTaxon.singular} name`} bind:value={ntName} required />
        </label>
        <div class="form-actions"><button class="btn primary" disabled={!ntName.trim()}><Icon name="plus" size={14} />Create {selectedTaxon.singular}</button></div>
      </form>
    {/if}
  </section>
  <section aria-labelledby="metadata-list-heading">
    <h4 class="list-heading" id="metadata-list-heading">Existing {selectedTaxon.label.toLowerCase()}</h4>
    {#if taxon === 'custom_fields'}
      {#if fieldsError}
        <div class="settings-load-state err"><span>{fieldsError}</span><button class="btn sm" onclick={loadFields}>Retry</button></div>
      {:else if !fieldsLoaded || fieldsLoading}
        <p class="quiet" role="status">Loading custom fields…</p>
      {:else}
        <div class="index">
          {#each fields as f (f.id)}
            <div class="irow">
              <span class="grow"><input class="inline-edit" aria-label={`Rename ${f.name}`} value={f.name} onchange={(e) => renameField(f, e.target.value)} /></span>
              <span class="pill">{FIELD_TYPES[f.data_type] || f.data_type}</span>
              <button class="btn sm danger" onclick={() => rmField(f)} title="Delete field" aria-label={`Delete ${f.name}`}><Icon name="trash" size={13} /></button>
            </div>
          {:else}
            <p class="empty">No custom fields yet. Add a field when a document needs a detail that the standard fields do not cover.</p>
          {/each}
        </div>
      {/if}
    {:else if taxRowsError}
      <div class="settings-load-state err"><span>{taxRowsError}</span><button class="btn sm" onclick={loadTaxa}>Retry</button></div>
    {:else if !taxRowsLoaded || taxRowsKind !== taxon || taxRowsLoading}
      <p class="quiet" role="status">Loading {selectedTaxon.label.toLowerCase()}…</p>
    {:else}
      <div class="index">
        {#each taxRows as row (row.id)}
          <div class="irow">
            <span class="dot" style={row.color ? `background:${row.color}` : ''}></span>
            <span class="grow"><input class="inline-edit" aria-label={`Rename ${row.name}`} value={row.name} onchange={(e) => renameTaxon(row, e.target.value)} /></span>
            {#if row.child_count}<span class="quiet">{row.child_count} children</span>{/if}
            {#if row.document_count != null}<span class="quiet">{row.document_count} docs</span>{/if}
            <button class="btn sm danger" onclick={() => rmTaxon(row)} title="Delete entry" aria-label={`Delete ${row.name}`}><Icon name="trash" size={13} /></button>
          </div>
        {:else}
          <p class="empty">No {selectedTaxon.label.toLowerCase()} yet. Create your first {selectedTaxon.singular} above.</p>
        {/each}
      </div>
    {/if}
  </section>

{:else if tab === 'files'}
  <header class="panel-heading">
    <h3>Filing-tree files</h3>
    <p>Import or export a taxonomy file in HuML or TOML. This is separate from editing document metadata.</p>
  </header>
  <section class="card editor" aria-labelledby="import-heading">
    <div class="import-heading">
      <div><h4 id="import-heading">Import a filing tree</h4><p class="editor-description">Preview the changes before applying them to your archive.</p></div>
      <button class="btn" disabled={exportBusy || taxImportBusy} onclick={() => { taxImpOpen = !taxImpOpen }} aria-expanded={taxImpOpen} aria-controls="people-taxonomy-import"><Icon name="upload" size={14} />{taxImpOpen ? 'Close import' : 'Import a file'}</button>
    </div>
    {#if taxImpOpen}
      <fieldset id="people-taxonomy-import" class="import-body" disabled={exportBusy}>
        <TaxonomyImport {notify} bind:busy={taxImportBusy} onApplied={importedTaxonomy} />
      </fieldset>
    {/if}
  </section>
  <section class="card editor" aria-labelledby="export-heading">
    <h4 id="export-heading">Export the current filing tree</h4>
    <p class="editor-description">Save a reusable snapshot, with supported starter rules or just the tree.</p>
    <label class="field export-format">File format
      <select class="input" bind:value={exportFormat} disabled={exportBusy || taxImportBusy}>
        <option value="huml">HuML</option><option value="toml">TOML</option>
      </select>
    </label>
    <div class="export-actions">
      <button class="btn" disabled={exportBusy || taxImportBusy} onclick={() => doExport(false)}><Icon name="download" size={14} />Export with starter rules</button>
      <button class="btn" disabled={exportBusy || taxImportBusy} onclick={() => doExport(true)}>Export tree only</button>
    </div>
    <div class="export-note">
      <p>Filing-tree exports are not archive backups.</p>
      <details>
        <summary>What is included?</summary>
        <p>Starter-rule exports include supported preset-owned rules only. Tree-only exports omit keywords and starter rules; they are a separate choice, not an automatic fallback if an export fails.</p>
        <p>Documents, user-owned automations and forks, permissions, and review history require a <a href="https://docs.suchi.page/backup-restore" target="_blank" rel="noopener">full backup</a>.</p>
      </details>
    </div>
  </section>
{/if}
</div>

<style>
  .people-nav { display: flex; flex-wrap: wrap; gap: 4px; margin-bottom: 22px; padding-bottom: 10px; border-bottom: 1px solid var(--line); }
  .people-nav button { border: 0; border-radius: 7px; background: none; padding: 8px 13px; color: var(--muted); font-size: .82rem; font-weight: 550; }
  .people-nav button:hover { background: var(--surface-2); color: var(--ink); }
  .people-nav button.on { color: var(--accent); background: var(--tint); }
  .people-nav button:disabled { opacity: .5; cursor: default; }
  .people-content { display: grid; gap: 20px; max-width: 980px; }
  .panel-heading h3 { font-size: 1.1rem; }
  .panel-heading p, .editor-description { color: var(--muted); font-size: .82rem; margin: 5px 0 0; max-width: 62em; }
  .editor h4, .list-heading { font-size: .88rem; font-weight: 600; margin: 0 0 14px; }
  .editor-description { margin: -7px 0 16px; }
  .field { min-width: 0; margin: 0; color: var(--muted); font-size: .78rem; font-weight: 600; }
  .input { min-width: 0; min-height: 42px; font-weight: 400; }
  .field-grid { display: grid; grid-template-columns: 2fr 1fr; gap: 14px; }
  .form-actions { display: flex; justify-content: flex-end; gap: 10px; margin-top: 18px; padding-top: 16px; border-top: 1px solid var(--line); }
  .irow { cursor: default; }
  .quiet, .empty { color: var(--muted); font-size: .78rem; }
  .empty { padding: 18px; margin: 0; }
  .identity { display: flex; flex-direction: column; }
  .identity .quiet { overflow-wrap: anywhere; }
  .user-entry, .group-entry { border-bottom: 1px solid var(--line); }
  .user-entry:last-child, .group-entry:last-child { border-bottom: 0; }
  .user-row { border-bottom: 0; }
  .user-access { padding: 0 16px 14px; }
  summary { cursor: pointer; font-size: .8rem; }
  .user-access summary > span { margin-left: 8px; }
  .access-options { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 250px), 1fr)); gap: 16px 24px; margin-top: 16px; }
  .access-option { display: flex; justify-content: space-between; align-items: flex-start; gap: 12px; }
  .access-option span { display: flex; flex-direction: column; font-size: .8rem; }
  .access-option b { font-weight: 550; }
  .access-option small { margin-top: 3px; color: var(--muted); }
  .group-members { padding: 4px 16px 16px; background: var(--bg); }
  .member-row { padding-inline: 0; }
  .member-add { display: flex; align-items: flex-end; flex-wrap: wrap; gap: 10px; margin-top: 16px; }
  .member-add .field { flex: 1 1 240px; }
  .member-add .btn { min-height: 42px; }
  .metadata-heading { display: flex; justify-content: space-between; align-items: flex-start; gap: 20px; }
  .metadata-heading .field { flex: 0 0 210px; }
  .import-heading { display: flex; align-items: center; justify-content: space-between; gap: 18px; }
  .import-heading .editor-description { margin-bottom: 0; }
  .import-heading .btn { flex: none; }
  .import-body { border: 0; border-top: 1px solid var(--line); padding: 18px 0 0; margin: 18px 0 0; min-width: 0; }
  .export-format { max-width: 220px; }
  .export-actions { display: flex; flex-wrap: wrap; gap: 10px; margin-top: 14px; }
  .export-note { border-top: 1px solid var(--line); margin-top: 20px; padding-top: 14px; color: color-mix(in srgb, var(--ink) 75%, var(--muted)); font-size: .78rem; }
  .export-note p { margin: 0 0 8px; max-width: 65em; }
  .export-note details p { margin: 10px 0 0; }
  .settings-load-state { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
  @media (max-width: 600px) {
    .people-nav { gap: 2px; }
    .people-nav button { flex: 1 1 auto; padding: 9px 10px; }
    .field-grid { grid-template-columns: minmax(0, 1fr); }
    .metadata-heading, .import-heading { flex-direction: column; align-items: stretch; gap: 14px; }
    .metadata-heading .field { flex: auto; }
    .form-actions .btn, .export-actions .btn, .import-heading .btn { flex: 1 1 auto; justify-content: center; min-height: 42px; }
    .user-row { flex-wrap: wrap; gap: 10px; }
    .user-row > .grow { flex-basis: 100%; }
    .export-format { max-width: none; }
  }
</style>
