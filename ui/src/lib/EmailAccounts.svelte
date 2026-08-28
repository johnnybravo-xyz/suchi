<script>
  // List of mailboxes. Each row shows liveness at a glance: last sync
  // recency + a red pill when last_error is set. Add/edit/delete flows
  // route through MailAccountForm, which owns the wire shape.
  import { listEmailAccounts, deleteEmailAccount, testEmailAccount } from './api.js'
  import MailAccountForm from './MailAccountForm.svelte'
  import Icon from './Icon.svelte'

  let { notify, viewerRole = 'admin', users = [] } = $props()

  let accounts = $state([])
  let capabilities = $state({ microsoft_oauth: { ready: false, reason: '' } })
  let loaded = $state(false)
  let loadError = $state('')
  let openForm = $state(null)      // null | {mode, account?}
  let busy = $state(false)
  let loadVersion = 0

  const PROVIDER_LABELS = {
    microsoft: 'Microsoft',
    gmail: 'Gmail',
    fastmail: 'Fastmail',
    icloud: 'iCloud',
    proton: 'Proton',
    zoho: 'Zoho',
    custom: 'Custom',
  }

  // usersByID drives ownerLabel — only populated for admins (members
  // don't fetch the roster and don't render the owner column).
  const usersByID = $derived(Object.fromEntries((users || []).map(u => [u.id, u])))
  function ownerLabel(id) {
    const u = usersByID[id]
    if (!u) return `Owner #${id}`
    if (u.display_name) return `${u.display_name} <${u.email}>`
    return u.email || `Owner #${id}`
  }

  async function load() {
    const version = ++loadVersion
    loaded = false
    loadError = ''
    try {
      const r = await listEmailAccounts()
      if (version !== loadVersion) return
      accounts = r?.accounts || []
      capabilities = r?.capabilities || capabilities
    } catch (ex) {
      if (version === loadVersion) loadError = ex.data?.message || ex.message || 'Could not load mailboxes.'
    } finally {
      if (version === loadVersion) loaded = true
    }
  }

  function relTime(value) {
    if (!value) return 'never synced'
    const seconds = Number(value)
    const t = Number.isFinite(seconds) ? seconds * 1000 : Date.parse(value)
    if (!Number.isFinite(t)) return 'never synced'
    const secs = Math.max(0, Math.floor((Date.now() - t) / 1000))
    if (secs < 60) return `${secs}s ago`
    const mins = Math.floor(secs / 60)
    if (mins < 60) return `last sync ${mins} min ago`
    const hrs = Math.floor(mins / 60)
    if (hrs < 24) return `last sync ${hrs}h ago`
    return `last sync ${Math.floor(hrs / 24)}d ago`
  }

  async function testOne(a) {
    busy = true
    try {
      const r = await testEmailAccount(a.id)
      notify?.(r?.message || (r?.ok ? 'Mailbox reachable.' : 'Test failed.'))
      load()
    } catch (ex) {
      notify?.(ex.data?.message || ex.message || 'Test failed.')
    } finally { busy = false }
  }

  async function del(a) {
    if (!confirm(`Delete “${a.name}”?`)) return
    busy = true
    try {
      await deleteEmailAccount(a.id)
      notify?.('Mailbox deleted')
      load()
    } catch (ex) {
      notify?.(ex.data?.message || ex.message || 'Could not delete')
    } finally { busy = false }
  }

  function onFormClose(saved) {
    openForm = null
    if (saved) load()
  }

  load()
</script>

<div class="mailbox-toolbar">
  <button class="btn primary sm" onclick={() => (openForm = { mode: 'create' })}>
    <Icon name="plus" size={13} /> Add mailbox
  </button>
</div>

{#if !loaded}
  <p class="mailbox-empty">Loading…</p>
{:else if loadError}
  <div class="mailbox-error">
    <span>{loadError}</span>
    <button class="btn sm" onclick={load}><Icon name="refresh" size={13} /> Retry</button>
  </div>
{:else if accounts.length === 0}
  <p class="mailbox-empty">No mailboxes connected.</p>
{:else}
  <div class="mailbox-list">
    {#each accounts as a (a.id)}
      <div class="mbx-row">
        <span class="mailbox-dot" class:ok={!a.last_error && a.last_sync_at}
              class:warn={!a.last_sync_at && !a.last_error}
              class:danger={!!a.last_error}></span>
        <div class="mailbox-identity">
          <span class="mailbox-name">{a.name || `Mailbox #${a.id}`}</span>
          <span class="mailbox-owner">
            {#if viewerRole === 'admin'}
              {a.username || ''} · {ownerLabel(a.owner_id)}
            {:else}
              {a.username || ''}
            {/if}
          </span>
        </div>
        <span class="chip mailbox-provider">{PROVIDER_LABELS[a.provider] || a.provider}</span>
        <div class="mailbox-status">
          {#if a.last_error}
            <span class="pill danger" title={a.last_error}>error</span>
          {:else}
            <span>{relTime(a.last_sync_at)}</span>
          {/if}
          {#if !a.enabled}<span class="pill warn">disabled</span>{/if}
        </div>
        <div class="mailbox-actions">
          <button class="btn sm" disabled={busy} onclick={() => testOne(a)}>Test</button>
          <button class="btn sm" disabled={busy} onclick={() => (openForm = { mode: 'edit', account: a })}>Edit</button>
          <button class="btn sm danger" disabled={busy} onclick={() => del(a)} title="Delete mailbox" aria-label="Delete mailbox"><Icon name="trash" size={13} /></button>
        </div>
      </div>
    {/each}
  </div>
{/if}

{#if openForm}
  <MailAccountForm
    mode={openForm.mode}
    account={openForm.account}
    {notify}
    {viewerRole}
    {users}
    microsoftOAuth={capabilities.microsoft_oauth}
    onClose={onFormClose} />
{/if}

<style>
  .mailbox-toolbar { display:flex;justify-content:flex-start;margin-bottom:14px }
  .mailbox-empty { color:var(--muted);font-size:.84rem;margin:0;padding:14px 0;border-top:1px solid var(--line) }
  .mailbox-error { display:flex;align-items:center;justify-content:space-between;gap:12px;padding:12px 0;border-top:1px solid var(--line);color:var(--muted);font-size:.84rem }
  .mailbox-list { border-top:1px solid var(--line) }
  .mbx-row {
    display:grid;grid-template-columns:auto minmax(220px,1fr) auto minmax(120px,auto) auto;
    gap:14px;align-items:center;padding:13px 0;border-bottom:1px solid var(--line)
  }
  .mbx-row:last-child { border-bottom:0 }
  .mailbox-dot { width:8px;height:8px;border-radius:50%;background:var(--faint) }
  .mailbox-dot.ok { background:var(--ok) }
  .mailbox-dot.warn { background:var(--warn) }
  .mailbox-dot.danger { background:var(--danger) }
  .mailbox-identity { min-width:0 }
  .mailbox-name { display:block;font-weight:600;overflow-wrap:anywhere }
  .mailbox-owner { display:block;color:var(--muted);font-size:.78rem;margin-top:3px;overflow-wrap:anywhere }
  .mailbox-provider { justify-self:start }
  .mailbox-status { display:flex;align-items:center;gap:8px;color:var(--muted);font-size:.78rem;white-space:nowrap }
  .mailbox-actions { display:flex;gap:8px;justify-content:flex-end }
  @media (max-width: 760px) {
    .mbx-row { grid-template-columns:auto minmax(0,1fr) auto;gap:9px 12px }
    .mailbox-status { grid-column:2 }
    .mailbox-actions { grid-column:3;grid-row:2 }
  }
  @media (max-width: 520px) {
    .mailbox-provider { grid-column:3 }
    .mailbox-status { grid-column:2 / -1 }
    .mailbox-actions { grid-column:2 / -1;grid-row:3;justify-content:flex-start;flex-wrap:wrap }
  }
</style>
