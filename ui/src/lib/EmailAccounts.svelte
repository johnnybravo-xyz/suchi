<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "./i18n.svelte.js";
  // List of mailboxes. Each row shows liveness at a glance: last sync
  // recency or the full actionable error. Add/edit/delete flows route
  // through MailAccountForm, which owns the wire shape.
  import { listEmailAccounts, deleteEmailAccount, testEmailAccount } from './api.js'
  import MailAccountForm from './MailAccountForm.svelte'
  import Icon from './Icon.svelte'
  import { requestStatusRefresh } from './processing_bus.svelte.js'
  import ConfirmDialog from './ConfirmDialog.svelte'

  let { notify, viewerRole = 'admin', users = [] } = $props()

  let accounts = $state([])
  let capabilities = $state({ microsoft_oauth: { ready: false, reason: '' } })
  let loaded = $state(false)
  let loadError = $state('')
  let openForm = $state(null)      // null | {mode, account?}
  let busy = $state(false)
  let loadVersion = 0
  let deleteRequest = $state(null)

  const PROVIDER_LABELS = {
    microsoft: translate("emailAccounts.microsoft"),
    gmail: translate("emailAccounts.gmail"),
    fastmail: translate("emailAccounts.fastmail"),
    icloud: 'iCloud',
    proton: translate("emailAccounts.proton"),
    zoho: translate("emailAccounts.zoho"),
    custom: translate("emailAccounts.custom"),
  }

  // usersByID drives ownerLabel — only populated for admins (members
  // don't fetch the roster and don't render the owner column).
  const usersByID = $derived(Object.fromEntries((users || []).map(u => [u.id, u])))
  function ownerLabel(id) {
    const u = usersByID[id]
    if (!u) return translate("emailAccounts.ownerValue", { "id": id })
    if (u.display_name) return translate("emailAccounts.valueValue", { "display_name": u.display_name, "email": u.email })
    return u.email || translate("emailAccounts.ownerValue", { "id": id })
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
      if (version === loadVersion) loadError = ex.data?.message || ex.message || translate("emailAccounts.couldNotLoadMailboxes")
    } finally {
      if (version === loadVersion) loaded = true
    }
  }

  async function refreshStatuses() {
    await load()
    requestStatusRefresh()
  }

  function relTime(value) {
    if (!value) return translate("emailAccounts.neverSynced")
    const seconds = Number(value)
    const t = Number.isFinite(seconds) ? seconds * 1000 : Date.parse(value)
    if (!Number.isFinite(t)) return translate("emailAccounts.neverSynced")
    const secs = Math.max(0, Math.floor((Date.now() - t) / 1000))
    if (secs < 60) return translate("emailAccounts.valueSAgo", { "secs": secs })
    const mins = Math.floor(secs / 60)
    if (mins < 60) return translate("emailAccounts.lastSyncValueMinAgo", { "mins": mins })
    const hrs = Math.floor(mins / 60)
    if (hrs < 24) return translate("emailAccounts.lastSyncValueHAgo", { "hrs": hrs })
    return translate("emailAccounts.lastSyncValueDAgo", { "hrs": Math.floor(hrs / 24) })
  }

  async function testOne(a) {
    busy = true
    try {
      const r = await testEmailAccount(a.id)
      notify?.(r?.message || (r?.ok ? translate("emailAccounts.mailboxReachable") : translate("emailAccounts.testFailed")))
      await refreshStatuses()
    } catch (ex) {
      notify?.(ex.data?.message || ex.message || translate("emailAccounts.testFailed"))
    } finally { busy = false }
  }

  function del(account) {
    if (busy || deleteRequest) return
    deleteRequest = account
  }

  async function confirmDelete() {
    const account = deleteRequest
    if (!account || busy) return
    busy = true
    try {
      await deleteEmailAccount(account.id)
      notify?.(translate("emailAccounts.mailboxDeleted"))
      deleteRequest = null
      refreshStatuses()
    } catch (ex) {
      notify?.(ex.data?.message || ex.message || translate("emailAccounts.couldNotDelete"))
      deleteRequest = null
    } finally { busy = false }
  }

  function onFormClose(saved) {
    openForm = null
    if (saved) refreshStatuses()
  }

  load()
</script>

<div class="mailbox-toolbar">
  <button class="btn primary sm" onclick={() => (openForm = { mode: 'create' })}>
    <Icon name="plus" size={13} /> {translate("emailAccounts.addMailbox")}
  </button>
</div>

{#if !loaded}
  <p class="mailbox-empty">{translate("common.loading")}</p>
{:else if loadError}
  <div class="mailbox-error">
    <span>{loadError}</span>
    <button class="btn sm" onclick={load}><Icon name="refresh" size={13} /> {translate("common.retry")}</button>
  </div>
{:else if accounts.length === 0}
  <p class="mailbox-empty">{translate("emailAccounts.noMailboxesConnected")}</p>
{:else}
  <div class="mailbox-list">
    {#each accounts as a (a.id)}
      <div class="mbx-row">
        <span class="mailbox-dot" class:ok={!a.last_error && a.last_sync_at}
              class:warn={!a.last_sync_at && !a.last_error}
              class:danger={!!a.last_error}></span>
        <div class="mailbox-identity">
          <span class="mailbox-name">{a.name || translate("emailAccounts.mailboxValue", { "id": a.id })}</span>
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
            <span class="mailbox-status-error" role="alert">{translate("emailAccounts.mailboxError", { "message": a.last_error })}</span>
          {:else}
            <span>{relTime(a.last_sync_at)}</span>
          {/if}
          {#if !a.enabled}<span class="pill warn">{translate("emailAccounts.disabled")}</span>{/if}
        </div>
        <div class="mailbox-actions">
          <button class="btn sm" disabled={busy} onclick={() => testOne(a)}>{translate("emailAccounts.test")}</button>
          <button class="btn sm" disabled={busy} onclick={() => (openForm = { mode: 'edit', account: a })}>{translate("emailAccounts.edit")}</button>
          <button class="btn sm danger" disabled={busy} onclick={() => del(a)} title={translate("emailAccounts.deleteMailbox")} aria-label={translate("emailAccounts.deleteMailbox")}><Icon name="trash" size={13} /></button>
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
    onStatusChange={refreshStatuses}
    onClose={onFormClose} />
{/if}

{#if deleteRequest}
  <ConfirmDialog
    title={translate("emailAccounts.deleteValue", { "name": deleteRequest.name })}
    message={translate("emailAccounts.thisMailboxAndItsSavedConnectionDetails")}
    confirmLabel={translate("emailAccounts.deleteMailbox")}
    busyLabel={translate("emailAccounts.deleting")}
    {busy}
    onConfirm={confirmDelete}
    onCancel={() => (deleteRequest = null)} />
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
  .mailbox-status { display:flex;align-items:center;gap:8px;max-width:36ch;color:var(--muted);font-size:.78rem;white-space:normal;overflow-wrap:anywhere }
  .mailbox-status-error { color:var(--danger) }
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
