<script>
  import { adminCreateUser, listGroups, createGroup, deleteGroup, groupMembers, addGroupMember, removeGroupMember,
           listCustomFields, createCustomField, patchCustomField, deleteCustomField,
           listTags, listCorrespondents, listDocumentTypes, listStoragePaths,
           createTaxon, patchTaxon, deleteTaxon } from '../lib/api.js'
  import EmailAccounts from '../lib/EmailAccounts.svelte'
  import TaxonomyImport from '../lib/TaxonomyImport.svelte'
  import { exportTaxonomy } from '../lib/api.js'
  import Icon from '../lib/Icon.svelte'

  let { notify } = $props()
  let tab = $state('users')

  // ---------- users ----------
  let nu = $state({ email: '', display_name: '', password: '', role: 'member' })
  let createdUsers = $state([])
  async function createUser(e) {
    e.preventDefault()
    try {
      const res = await adminCreateUser({ ...nu })
      createdUsers = [{ ...nu, id: res?.id }, ...createdUsers]
      notify?.(`Created ${nu.email}`)
      nu = { email: '', display_name: '', password: '', role: 'member' }
    } catch (ex) { notify?.(ex.message || 'Could not create the user') }
  }

  // ---------- groups ----------
  let groups = $state([])
  let ngName = $state('')
  let openGroup = $state(null)      // {id, members: []}
  let addUID = $state('')
  async function loadGroups() {
    try { const r = await listGroups(); groups = r?.results || r || [] } catch {}
  }
  async function addGroup(e) {
    e.preventDefault()
    if (!ngName.trim()) return
    try { await createGroup({ name: ngName.trim() }); ngName = ''; notify?.('Group created'); loadGroups() }
    catch (ex) { notify?.(ex.message || 'Could not create the group') }
  }
  async function rmGroup(g) {
    if (!confirm(`Delete group “${g.name}”? Documents shared to it lose that grant.`)) return
    try { await deleteGroup(g.id); groups = groups.filter(x => x.id !== g.id); notify?.('Group deleted') }
    catch (ex) { notify?.(ex.message || 'Could not delete') }
  }
  async function toggleMembers(g) {
    if (openGroup?.id === g.id) { openGroup = null; return }
    try { const r = await groupMembers(g.id); openGroup = { id: g.id, members: r?.results || r || [] } }
    catch (ex) { notify?.(ex.message || 'Could not load members') }
  }
  async function addMember(g) {
    const uid = Number(addUID)
    if (!uid) return
    try { await addGroupMember(g.id, uid); addUID = ''; toggleMembers({ id: -1 }); toggleMembers(g); notify?.('Member added') }
    catch (ex) { notify?.(ex.message || 'Could not add (check the user id)') }
  }
  async function rmMember(g, uid) {
    try { await removeGroupMember(g.id, uid); openGroup = { ...openGroup, members: openGroup.members.filter(m => (m.user_id ?? m.id) !== uid) } }
    catch (ex) { notify?.(ex.message || 'Could not remove') }
  }

  // ---------- custom fields ----------
  let fields = $state([])
  let nf = $state({ name: '', data_type: 'text' })
  const FIELD_TYPES = ['text', 'number', 'date', 'bool', 'select', 'multi', 'url', 'monetary', 'documentlink']
  async function loadFields() {
    try { const r = await listCustomFields(); fields = r?.results || r || [] } catch {}
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
    { kind: 'tags', label: 'Tags', load: listTags },
    { kind: 'correspondents', label: 'Correspondents', load: listCorrespondents },
    { kind: 'document_types', label: 'Document types', load: listDocumentTypes },
    { kind: 'storage_paths', label: 'Storage paths', load: listStoragePaths },
  ]
  let taxon = $state('tags')
  let taxImpOpen = $state(false)
  async function doExport(format) {
    try {
      const text = await exportTaxonomy(format)
      const a = document.createElement('a')
      a.href = URL.createObjectURL(new Blob([text], { type: 'text/plain' }))
      a.download = `taxonomy.${format}`
      a.click()
      setTimeout(() => URL.revokeObjectURL(a.href), 10_000)
      notify?.(`Exported taxonomy.${format}`)
    } catch (ex) { notify?.(ex.message || 'Export failed') }
  }
  let taxRows = $state([])
  let ntName = $state('')
  async function loadTaxa() {
    const spec = TAXA.find(t => t.kind === taxon)
    try { const r = await spec.load(); taxRows = r?.results || r || [] } catch { taxRows = [] }
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

  loadGroups(); loadFields(); loadTaxa()
  $effect(() => { taxon; loadTaxa() })
</script>

<span class="seg admin-tabs">
  <button class:on={tab === 'users'} onclick={() => (tab = 'users')}>Users</button>
  <button class:on={tab === 'groups'} onclick={() => (tab = 'groups')}>Groups</button>
  <button class:on={tab === 'fields'} onclick={() => (tab = 'fields')}>Custom fields</button>
  <button class:on={tab === 'taxonomy'} onclick={() => (tab = 'taxonomy')}>Taxonomy</button>
  <button class:on={tab === 'mail'} onclick={() => (tab = 'mail')}>Mail intake</button>
</span>

{#if tab === 'users'}
  <div class="card content-narrow" style="margin:0">
    <h3>Create a user</h3>
    <form onsubmit={createUser}>
      <div class="toolbar" style="margin-bottom:0">
        <div class="field" style="flex:1;min-width:170px"><label for="au-email">Email</label>
          <input id="au-email" class="input" type="email" bind:value={nu.email} required /></div>
        <div class="field" style="flex:1;min-width:150px"><label for="au-name">Display name</label>
          <input id="au-name" class="input" bind:value={nu.display_name} /></div>
      </div>
      <div class="toolbar">
        <div class="field" style="flex:1;min-width:150px;margin-bottom:0"><label for="au-pw">Password</label>
          <input id="au-pw" class="input" type="password" bind:value={nu.password} autocomplete="new-password" required /></div>
        <div class="field" style="max-width:140px;margin-bottom:0"><label for="au-role">Role</label>
          <select id="au-role" class="input" bind:value={nu.role}><option value="member">Member</option><option value="admin">Admin</option></select></div>
        <button class="btn primary sm" style="align-self:flex-end"><Icon name="plus" size={13} /> Create</button>
      </div>
    </form>
    {#if createdUsers.length}
      <div class="index" style="margin-top:8px">
        {#each createdUsers as u}
          <div class="irow"><span class="dot ok"></span><span class="title grow">{u.email}</span><span class="pill">{u.role}</span></div>
        {/each}
      </div>
    {/if}
    <p class="sub" style="color:var(--faint);font-size:.76rem;margin:12px 0 0">
      Listing and disabling existing users needs <code>GET /api/admin/users</code> — backend task; this panel grows those controls when it lands.
    </p>
  </div>

{:else if tab === 'groups'}
  <div class="content-narrow" style="margin:0">
    <form class="toolbar" onsubmit={addGroup}>
      <input class="input" style="flex:1;max-width:320px" placeholder="New group name, e.g. family" bind:value={ngName} />
      <button class="btn primary sm"><Icon name="plus" size={13} /> Create group</button>
    </form>
    <div class="index">
      {#each groups as g (g.id)}
        <div class="irow" style="flex-wrap:wrap">
          <span class="dot"></span>
          <span class="title grow">{g.name}</span>
          <button class="btn sm" onclick={() => toggleMembers(g)}>{openGroup?.id === g.id ? 'Hide members' : 'Members'}</button>
          <button class="btn sm danger" onclick={() => rmGroup(g)}><Icon name="trash" size={13} /></button>
          {#if openGroup?.id === g.id}
            <div style="flex-basis:100%;padding:8px 0 2px 20px">
              {#each openGroup.members as mrow ((mrow.user_id ?? mrow.id))}
                <div class="irow" style="padding:5px 0;border:0">
                  <span class="dot accent" style="width:6px;height:6px"></span>
                  <span class="title grow" style="font-size:.84rem">{mrow.email || mrow.display_name || `user #${mrow.user_id ?? mrow.id}`}</span>
                  <button class="btn sm" onclick={() => rmMember(g, mrow.user_id ?? mrow.id)}>Remove</button>
                </div>
              {:else}
                <span class="sub">No members yet.</span>
              {/each}
              <div class="toolbar" style="margin:8px 0 0">
                <input class="input" style="max-width:130px;padding:4px 10px" type="number" placeholder="user id" bind:value={addUID} />
                <button class="btn sm" onclick={() => addMember(g)}>Add member</button>
              </div>
            </div>
          {/if}
        </div>
      {:else}
        <div class="irow"><span class="sub">No groups yet — create one to share documents with several people at once.</span></div>
      {/each}
    </div>
  </div>

{:else if tab === 'fields'}
  <div class="content-narrow" style="margin:0">
    <form class="toolbar" onsubmit={addField}>
      <input class="input" style="flex:1;max-width:260px" placeholder="Field name, e.g. Invoice number" bind:value={nf.name} />
      <select class="input" style="max-width:150px" bind:value={nf.data_type}>
        {#each FIELD_TYPES as t}<option value={t}>{t}</option>{/each}
      </select>
      <button class="btn primary sm"><Icon name="plus" size={13} /> Create field</button>
    </form>
    <div class="index">
      {#each fields as f (f.id)}
        <div class="irow">
          <span class="dot"></span>
          <span class="grow"><input class="inline-edit" value={f.name} onchange={(e) => renameField(f, e.target.value)} /></span>
          <span class="chip">{f.data_type}</span>
          <button class="btn sm danger" onclick={() => rmField(f)}><Icon name="trash" size={13} /></button>
        </div>
      {:else}
        <div class="irow"><span class="sub">No custom fields yet.</span></div>
      {/each}
    </div>
  </div>

{:else if tab === 'taxonomy'}
  <div class="content-narrow" style="margin:0">
    <div class="toolbar">
      <span class="seg">
        {#each TAXA as t}<button class:on={taxon === t.kind} onclick={() => (taxon = t.kind)}>{t.label}</button>{/each}
      </span>
      <span class="spacer"></span>
      <button class="btn sm" onclick={() => (taxImpOpen = true)}><Icon name="upload" size={13} /> Import</button>
      <button class="btn sm" onclick={() => doExport('huml')} title="suchi-taxonomy/v1, HuML">Export</button>
      <select class="input" style="max-width:86px;padding:5px 8px;font-size:.76rem"
              onchange={(e) => { if (e.target.value) { doExport(e.target.value); e.target.value = '' } }}
              aria-label="Export as">
        <option value="">as…</option><option value="huml">huml</option><option value="toml">toml</option><option value="yaml">yaml</option>
      </select>
    </div>
    <form class="toolbar" onsubmit={addTaxon}>
      <input class="input" style="flex:1;max-width:300px" placeholder={`New ${TAXA.find(t => t.kind === taxon).label.toLowerCase().replace(/s$/, '')} name`} bind:value={ntName} />
      <button class="btn primary sm"><Icon name="plus" size={13} /> Create</button>
    </form>
    <div class="index">
      {#each taxRows as row (row.id)}
        <div class="irow">
          <span class="dot" style={row.color ? `background:${row.color}` : ''}></span>
          <span class="grow"><input class="inline-edit" value={row.name} onchange={(e) => renameTaxon(row, e.target.value)} /></span>
          {#if row.child_count}<span class="sub">{row.child_count} children</span>{/if}
          {#if row.document_count != null}<span class="sub">{row.document_count} docs</span>{/if}
          <button class="btn sm danger" onclick={() => rmTaxon(row)}><Icon name="trash" size={13} /></button>
        </div>
      {:else}
        <div class="irow"><span class="sub">Nothing here yet.</span></div>
      {/each}
    </div>
  </div>

{:else if tab === 'mail'}
  <div class="content-narrow" style="margin:0">
    <p class="sub" style="color:var(--muted);font-size:.84rem;margin:0 0 14px">
      suchi polls each mailbox on its own schedule and files what it finds. Credentials are stored server-side and never shown back.
    </p>
    <EmailAccounts {notify} />
  </div>
{/if}

<svelte:window onkeydown={(e) => { if (taxImpOpen && e.key === 'Escape') taxImpOpen = false }} />

{#if taxImpOpen}
  <div class="modal-veil"
       onclick={() => (taxImpOpen = false)}
       onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); taxImpOpen = false } }}
       role="button" tabindex="-1" aria-label="Close import dialog">
    <div class="modal" style="width:min(640px,94vw)"
         onclick={(e) => e.stopPropagation()}
         onkeydown={(e) => e.stopPropagation()}
         role="dialog" aria-modal="true" aria-label="Import taxonomy" tabindex="-1">
      <div class="modal-head">
        <h3>Import a taxonomy</h3>
        <button class="btn sm" onclick={() => (taxImpOpen = false)}><Icon name="x" size={13} /></button>
      </div>
      <TaxonomyImport {notify} onApplied={() => { taxImpOpen = false; loadTaxa() }} />
    </div>
  </div>
{/if}
