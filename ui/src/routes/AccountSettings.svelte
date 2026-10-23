<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from '../lib/i18n.svelte.js'
  import { availableLocales, displayLanguage } from '../lib/locale-options.js'
  import { scopedHash as filingHref } from '../lib/systems.svelte.js'
  import { route, go } from '../lib/router.svelte.js'
  import { onDestroy, tick, untrack } from 'svelte'
  import { listTokens, createToken, deleteToken, patchMe, uploadAvatar,
           listDecryptionPasswords, renameDecryptionPassword, deleteDecryptionPassword } from '../lib/api.js'
  import { session, refreshSession } from '../lib/session.svelte.js'
  import { fmtDate } from '../lib/format.js'
  import { hasCapability } from '../lib/capabilities.js'
  import Icon from '../lib/Icon.svelte'
  import EmailChangeDialog from '../lib/EmailChangeDialog.svelte'
  import Lazy from '../lib/Lazy.svelte'
  import ConfirmDialog from '../lib/ConfirmDialog.svelte'
  import TwoFactorSettings from '../lib/TwoFactorSettings.svelte'

  const loadMailboxes = () => import('../lib/EmailAccounts.svelte')
  const loadMobilePairing = () => import('../lib/MobilePairing.svelte')

  let { notify, accountNotify, profileOnly = false } = $props()
  let tokens = $state([])
  let err = $state('')
  let newName = $state('')
  let tokenAccess = $state('read')
  let minted = $state('')   // freshly created secret, shown once
  let pairingOpen = $state(false)
  let tokensLoading = $state(true)
  let revoking = $state([])
  let loadController
  let loadGeneration = 0
  let loadPending = false
  let disposed = false
  let destructiveRequest = $state(null)
  let destructiveBusy = $state(false)
  const demoVisitor = $derived(session.user?.kind === 'demo-anon' || session.user?.kind === 'demo-scratch')
  const accountToolsAvailable = $derived(!profileOnly && !demoVisitor)
  const emailChangeMode = $derived(session.user?.email_change_mode || 'disabled')
  const signInMethod = $derived(emailChangeMode === 'oidc'
    ? translate('accountSettings.identityProvider')
    : emailChangeMode === 'password' ? translate('accountSettings.password') : translate('accountSettings.managedAccount'))

  function isOwnMobileToken(token) {
    return token.source === 'mobile_pairing' && token.user_id === session.user?.user_id
  }
  const mobileTokens = $derived(tokens.filter(isOwnMobileToken))
  const apiTokens = $derived(tokens.filter(token => !isOwnMobileToken(token)))

  let profile = $state({ display_name: session.user?.display_name || '', locale: session.user?.locale || '' })
  let profileBusy = $state(false)
  let avatarInput = $state()
  let emailDialogOpen = $state(false)
  let emailChangeButton = $state()
  async function closeEmailDialog() {
    emailDialogOpen = false
    await tick()
    emailChangeButton?.focus()
  }
  async function emailChanged() {
    const notifyResult = accountNotify
    emailDialogOpen = false
    await refreshSession()
    await tick()
    notifyResult?.(translate('accountSettings.signInEmailChanged'))
  }
  async function saveProfile() {
    if (demoVisitor) return
    const notifyResult = notify
    profileBusy = true
    try {
      await patchMe({ display_name: profile.display_name.trim(), locale: profile.locale })
      await refreshSession()
      notifyResult?.(translate('accountSettings.profileSaved'))
    } catch (ex) {
      notifyResult?.(ex.message || translate('accountSettings.couldNotSaveTheProfile'))
    } finally { if (!disposed) profileBusy = false }
  }
  async function sendAvatar(file) {
    if (!file || demoVisitor) return
    const notifyResult = notify
    try { await uploadAvatar(file); await refreshSession(); notifyResult?.(translate("accountSettings.avatarUpdated")) }
    catch (ex) {
      notifyResult?.(ex.message || translate("accountSettings.couldNotUploadTheAvatar"))
    }
  }

  async function load({ background = false } = {}) {
    if (disposed || !accountToolsAvailable || !session.user || (background && loadPending)) return
    const user = session.user
    const generation = ++loadGeneration
    loadController?.abort()
    const controller = new AbortController()
    loadController = controller
    loadPending = true
    if (!background) tokensLoading = true
    try {
      const signal = AbortSignal.any([controller.signal, AbortSignal.timeout(10000)])
      const res = await listTokens(signal)
      if (disposed || generation !== loadGeneration || session.user !== user) return
      tokens = res?.results || res || []
      err = ''
    } catch (ex) {
      if (!disposed && generation === loadGeneration && session.user === user && !controller.signal.aborted) {
        err = ex.message || translate("accountSettings.couldNotLoadConnectedAppsAndApiTokens")
      }
    } finally {
      if (generation === loadGeneration) {
        tokensLoading = false
        loadPending = false
        loadController = undefined
      }
    }
  }

  $effect(() => {
    const user = session.user
    const available = accountToolsAvailable
    tokens = []
    err = ''
    destructiveRequest = null
    tokensLoading = !!user && available
    if (user && available) untrack(() => void load())
    return () => {
      loadGeneration++
      loadController?.abort()
      loadPending = false
    }
  })

  $effect(() => {
    if (!pairingOpen) return
    const timer = setInterval(() => void load({ background: true }), 2000)
    return () => clearInterval(timer)
  })

  onDestroy(() => {
    disposed = true
    loadGeneration++
    loadController?.abort()
  })

  // ---- decryption-password vault ----
  let vault = $state([])
  let vaultErr = $state('')
  async function loadVault() {
    if (!accountToolsAvailable) return
    try {
      const res = await listDecryptionPasswords()
      vault = res?.results || res || []
    } catch (ex) { vaultErr = ex.message || translate("accountSettings.couldNotLoadTheVault") }
  }
  let editingVaultID = $state(0)
  let vaultDraft = $state('')
  function startVaultRename(v) {
    editingVaultID = v.id
    vaultDraft = v.label || ''
  }
  function focusVaultEditor(node) {
    queueMicrotask(() => {
      node.focus()
      node.select()
    })
  }
  async function focusVaultRenameButton(id) {
    await tick()
    document.getElementById(`vault-rename-${id}`)?.focus()
  }
  function cancelVaultRename(v) {
    if (editingVaultID !== v.id) return
    editingVaultID = 0
    vaultDraft = ''
    void focusVaultRenameButton(v.id)
  }
  function commitVaultRename(v, restoreFocus = false) {
    if (editingVaultID !== v.id) return
    const next = vaultDraft
    editingVaultID = 0
    vaultDraft = ''
    void renameVault(v, next)
    if (restoreFocus) void focusVaultRenameButton(v.id)
  }
  async function renameVault(v, next) {
    const trimmed = (next || '').trim()
    if (trimmed === (v.label || '')) return
    try {
      await renameDecryptionPassword(v.id, trimmed)
      v.label = trimmed
      notify?.(translate("accountSettings.labelUpdated"))
    } catch (ex) { notify?.(ex.message || translate("accountSettings.couldNotRename")) }
  }
  function removeVault(entry) {
    if (destructiveBusy || destructiveRequest) return
    destructiveRequest = { kind: 'vault', item: entry, user: session.user }
  }

  async function mint(e) {
    e.preventDefault()
    const name = newName.trim()
    if (!name) {
      notify?.(translate("accountSettings.giveTheTokenAName"))
      return
    }
    try {
      const scopes = tokenAccess === 'write'
        ? 'documents:read,documents:write'
        : 'documents:read'
      const res = await createToken({ name, scopes })
      minted = res?.token || ''
      newName = ''
      notify?.(translate("accountSettings.tokenCreatedCopyItNowItIsShown"))
      load()
    } catch (ex) { notify?.(ex.message || translate("accountSettings.couldNotCreateAToken")) }
  }

  function revoke(token) {
    if (destructiveBusy || destructiveRequest) return
    destructiveRequest = { kind: 'token', item: token, user: session.user }
  }

  const destructiveDialog = $derived.by(() => {
    const request = destructiveRequest
    if (!request) return null
    if (request.kind === 'vault') return {
      title: request.item.label ? translate("accountSettings.deleteNamedPassword", { name: request.item.label }) : translate("accountSettings.deleteSavedPassword"),
      message: translate("accountSettings.documentsAlreadyUnlockedStayUnlockedFutureUploadsWith"),
      confirmLabel: translate("common.delete"),
      busyLabel: translate("accountSettings.deleting"),
    }
    const connected = isOwnMobileToken(request.item)
    return {
      title: translate(connected ? "accountSettings.disconnectNamed" : "accountSettings.revokeNamed", { name: request.item.name }),
      message: connected
        ? translate("accountSettings.thisMobileAppWillLoseAccessImmediately")
        : translate("accountSettings.anythingUsingThisTokenWillLoseAccessImmediately"),
      confirmLabel: connected ? translate("accountSettings.disconnectApp") : translate("accountSettings.revokeToken"),
      busyLabel: connected ? translate("accountSettings.disconnecting") : translate("accountSettings.revoking"),
    }
  })

  async function confirmDestructiveAction() {
    const request = destructiveRequest
    if (!request || destructiveBusy || request.user !== session.user) {
      if (request === destructiveRequest) destructiveRequest = null
      return
    }
    destructiveBusy = true
    if (request.kind === 'token') revoking = [...revoking, request.item.id]
    try {
      if (request.kind === 'vault') {
        await deleteDecryptionPassword(request.item.id)
        vault = vault.filter(entry => entry.id !== request.item.id)
        notify?.(translate("accountSettings.vaultEntryRemoved"))
      } else {
        await deleteToken(request.item.id)
        if (disposed || session.user !== request.user) return
        tokens = tokens.filter(token => token.id !== request.item.id)
        notify?.(isOwnMobileToken(request.item) ? translate("accountSettings.mobileAppDisconnected") : translate("accountSettings.tokenRevoked"))
        void load()
      }
    } catch (ex) {
      if (!disposed && session.user === request.user) {
        notify?.(ex.message || (request.kind === 'vault' ? translate("accountSettings.couldNotRemove") : translate("accountSettings.couldNotRevoke")))
      }
    } finally {
      if (!disposed && request.kind === 'token') {
        revoking = revoking.filter(id => id !== request.item.id)
      }
      if (destructiveRequest === request) destructiveRequest = null
      if (!disposed) destructiveBusy = false
    }
  }

  function tokenAccessLabel(scopes) {
    const values = (scopes || '').split(',').map(value => value.trim()).filter(Boolean).sort()
    if (values.length === 1 && values[0] === 'documents:read') return translate("accountSettings.readOnly")
    if (values.length === 2 && values[0] === 'documents:read' && values[1] === 'documents:write') return translate("accountSettings.readWrite")
    return translate("accountSettings.custom")
  }

  // Administrators manage every mailbox in Archive configuration. Members
  // with the capability manage only their own rows here.
  const mailboxesVisible = $derived(
    session.user?.role !== 'admin' && hasCapability(session.user, 'mailboxes')
  )

  loadVault()
  // The Omnibox's Pair a phone command links here with ?pair=1.
  untrack(() => {
    if (route.query.get('pair') !== '1') return
    if (accountToolsAvailable) pairingOpen = true
    go(filingHref('#/settings'), { replace: true })
  })
</script>

<svelte:window onfocus={() => void load({ background: true })} />

{#snippet tokenRows(items, emptyMessage, connected)}
  {#if items.length}
    <div class="settings-list token-list">
      {#each items as t (t.id)}
        <div class="token-row">
          <span class="token-name" class:mono={!connected}>{t.name}</span>
          {#if t.system_code}<span class="pill">{t.system_code}</span>{/if}
          <span class="pill" title={t.scopes}>{tokenAccessLabel(t.scopes)}</span>
          <span class="row-date">
            <span>{connected ? translate("accountSettings.connected") : translate("accountSettings.created")} {t.created_at ? fmtDate(t.created_at) : '—'}</span>
            {#if connected}<span>{t.last_used_at ? translate("accountSettings.lastUsed") + fmtDate(t.last_used_at) : translate("accountSettings.notUsedYet")}</span>{/if}
          </span>
          <button class="act-quiet" disabled={revoking.includes(t.id) || destructiveBusy || !!destructiveRequest} onclick={() => revoke(t)}>{translate("accountSettings.revoke")}</button>
        </div>
      {/each}
    </div>
  {:else if !tokensLoading && !err}
    <p class="empty-setting">{emptyMessage}</p>
  {/if}
{/snippet}

  <section class="settings-section" aria-labelledby="profile-heading">
    <div class="section-heading">
      <h2 id="profile-heading">{translate("accountSettings.profile")}</h2>
      <div class="profile-meta">
        <span class="pill">{session.user?.role}</span>
        <span>{translate("accountSettings.signInValue", { "signInMethod": signInMethod })}</span>
      </div>
    </div>
    <div class="profile-layout">
      <div class="avatar-control">
        <span class="avatar profile-avatar">
          {#if session.user?.avatar_url}<img src={session.user.avatar_url} alt="" />{:else}{(profile.display_name || session.user?.email || '?').split(/[\s@._-]+/).filter(Boolean).slice(0,2).map(w=>w[0].toUpperCase()).join('')}{/if}
        </span>
        {#if !demoVisitor}
          <button class="act-link" onclick={() => avatarInput.click()}>{translate("accountSettings.changePhoto")}</button>
          <input bind:this={avatarInput} type="file" accept="image/png,image/jpeg" hidden onchange={(e) => sendAvatar(e.target.files[0])} />
        {/if}
      </div>
      <div class="profile-fields">
        <div class="field">
          <label for="p-name">{translate("accountSettings.displayName")}</label>
          <input id="p-name" class="input" bind:value={profile.display_name} readonly={demoVisitor} />
          {#if demoVisitor}<span class="sub">{translate("accountSettings.demoProfilesAreReadOnly")}</span>{/if}
        </div>
        <div class="field">
          <label for="p-email">{translate("accountSettings.email")}</label>
          <input id="p-email" class="input" type="email" value={session.user?.email || ''} readonly />
          {#if emailChangeMode === 'password'}
            <div class="help-row">
              <span class="sub">{translate("accountSettings.thisAddressIsYourSignInIdentifier")}</span>
              <button class="act-link email-change-action" type="button" bind:this={emailChangeButton}
                      onclick={() => (emailDialogOpen = true)}>{translate("accountSettings.changeEmail")}</button>
            </div>
          {:else if emailChangeMode === 'oidc'}
            <div class="help-row">
              <span class="sub">{translate("accountSettings.yourIdentityProviderManagesThisAddressReauthenticate")}</span>
              <form method="post" action="/oidc/email-change" class="oidc-sync-form">
                <button class="act-link" type="submit">{translate("accountSettings.syncFromIdentityProvider")}</button>
              </form>
            </div>
          {:else}
            <span class="sub">{translate("accountSettings.signInEmailChangesAreNotAvailable")}</span>
          {/if}
        </div>
        <div class="field">
          <label for="p-language">{translate('language.label')}</label>
          <select id="p-language" class="input" bind:value={profile.locale} disabled={demoVisitor}>
            <option value="">{translate('language.automatic')}</option>
            {#each availableLocales() as locale}
              <option value={locale}>{displayLanguage(locale)}</option>
            {/each}
          </select>
          <span class="sub">{translate('language.help')}</span>
        </div>
      </div>
    </div>
    {#if !demoVisitor}
      <div class="card-foot">
        <button class="btn primary sm" disabled={profileBusy} onclick={saveProfile}>{translate("accountSettings.saveProfile")}</button>
      </div>
    {/if}
  </section>

  {#if emailDialogOpen}
    <EmailChangeDialog currentEmail={session.user?.email || ''}
      onClose={closeEmailDialog} onChanged={emailChanged} />
  {/if}

  {#if !demoVisitor && session.user?.authn_by === 'local-auth'}
    <TwoFactorSettings {notify} />
  {/if}


  {#if accountToolsAvailable}
    <section class="settings-section" aria-labelledby="mobile-heading">
      <div class="section-heading">
        <div>
          <h2 id="mobile-heading">{translate("accountSettings.mobileApp")}</h2>
          <p>{translate("accountSettings.connectTheSuchiAppToThisArchive")}</p>
        </div>
        <button class="btn sm" onclick={() => pairingOpen = true}>{translate("accountSettings.pairMobileApp")}</button>
      </div>
      {#if err}
        <div class="err" role="alert">{err} <button class="btn sm" onclick={() => void load()}>{translate("common.retry")}</button></div>
      {:else if tokensLoading && !tokens.length}
        <p class="empty-setting" role="status">{translate("accountSettings.loadingConnectedApps")}</p>
      {/if}
      {@render tokenRows(mobileTokens, translate("accountSettings.noConnectedMobileAppsPairTheAppTo"), true)}
    </section>
  {#if pairingOpen}
    <Lazy load={loadMobilePairing} props={{ notify, onClose: () => { pairingOpen = false; load() } }} />
  {/if}

  <section class="settings-section" aria-labelledby="tokens-heading">
    <div class="section-heading">
      <div>
        <h2 id="tokens-heading">{translate("accountSettings.apiTokens")}</h2>
        <p>{translate("accountSettings.namedAccessForMobileAppsScriptsAnd")} <code>{translate("accountSettings.suchiMcp")}</code>.</p>
      </div>
      <div class="auth-syntax">
        <span>{translate("accountSettings.requestHeader")}</span>
        <code>{translate("accountSettings.authorizationTokenToken")}</code>
      </div>
    </div>
    {#if err}<div class="err">{err}</div>{/if}
    {#if minted}
      <div class="minted-token">
        <label for="minted">{translate("accountSettings.newTokenCopyItNowItIs")}</label>
        <div>
          <input id="minted" class="input mono" readonly value={minted} />
          <button class="btn sm" type="button" onclick={() => { navigator.clipboard?.writeText(minted); notify?.(translate("accountSettings.tokenCopied")) }}>{translate("accountSettings.copy")}</button>
        </div>
      </div>
    {/if}
    <form class="token-form" onsubmit={mint}>
      <div class="field">
        <label for="token-name">{translate("accountSettings.tokenName")}</label>
        <input id="token-name" class="input" placeholder={translate("accountSettings.eGPhoneOrArchiveScript")}
               maxlength="64" required bind:value={newName} />
      </div>
      <div class="field">
        <span class="field-label">{translate("accountSettings.access")}</span>
        <span class="seg token-access" aria-label={translate("accountSettings.tokenAccess")}>
          <button type="button" class:on={tokenAccess === 'read'} aria-pressed={tokenAccess === 'read'}
                  onclick={() => tokenAccess = 'read'}>{translate("accountSettings.readOnly")}</button>
          <button type="button" class:on={tokenAccess === 'write'} aria-pressed={tokenAccess === 'write'}
                  onclick={() => tokenAccess = 'write'}>{translate("accountSettings.readWrite")}</button>
        </span>
      </div>
      <button class="btn primary token-create"><Icon name="plus" size={13} /> {translate("accountSettings.createToken")}</button>
    </form>
    {@render tokenRows(apiTokens, translate("accountSettings.noApiTokens"), false)}
  </section>

  {#if mailboxesVisible}
    <section class="settings-section" aria-labelledby="mailboxes-heading">
      <div class="section-heading">
        <div>
          <h2 id="mailboxes-heading">{translate("accountSettings.mailboxes")}</h2>
          <p>{translate("accountSettings.connectedInboxesAndTheirLatestSyncStatus")}</p>
        </div>
      </div>
        <div class="card-body"><Lazy load={loadMailboxes} props={{ notify, viewerRole: session.user?.role }} /></div>
    </section>
  {/if}

  <section class="settings-section" aria-labelledby="passwords-heading">
      <div class="section-heading">
        <div>
          <h2 id="passwords-heading">{translate("accountSettings.savedDecryptionPasswords")}</h2>
          <p>{translate("accountSettings.encryptedPasswordsReusedWhenMatchingProtectedDocuments")}</p>
        </div>
      </div>
      {#if vaultErr}<div class="err">{vaultErr}</div>{/if}
      {#if vault.length}
        <div class="settings-list vault-list">
          <div class="vault-columns">
            <span class="vault-column-label">{translate("accountSettings.savedPasswordLabel")}</span>
            <span>{translate("accountSettings.lastUsed")}</span>
            <span>{translate("accountSettings.lastUnlockedDocument")}</span>
            <span class="vault-column-actions">{translate("accountSettings.actions")}</span>
          </div>
          {#each vault as v (v.id)}
            <div class="vault-row">
              <span class="row-ico"><Icon name="lock" size={15} /></span>
              <div class="vault-label-cell">
                {#if editingVaultID === v.id}
                  <input class="inline-edit vault-label-editor"
                         bind:value={vaultDraft}
                         use:focusVaultEditor
                         placeholder={translate("accountSettings.unlabeled")}
                         onblur={() => commitVaultRename(v)}
                         onkeydown={(e) => {
                           if (e.key === 'Enter') {
                             e.preventDefault()
                             commitVaultRename(v, true)
                           } else if (e.key === 'Escape') {
                             e.preventDefault()
                             cancelVaultRename(v)
                           }
                         }}
                         aria-label={translate("accountSettings.savedPasswordLabel")} />
                {:else}
                  <span class="vault-label-text" class:unlabeled={!v.label}
                        title={v.label || translate("accountSettings.unlabeledSavedPassword")}>
                    {v.label || translate("accountSettings.unlabeled")}
                  </span>
                  <button id={`vault-rename-${v.id}`} class="vault-rename"
                          onclick={() => startVaultRename(v)}
                          title={translate("accountSettings.renameSavedPasswordLabelValue", { "label": v.label || translate("accountSettings.unlabeled") })}
                          aria-label={translate("accountSettings.renameSavedPasswordLabelValue", { "label": v.label || translate("accountSettings.unlabeled") })}>
                    <Icon name="edit" size={14} />
                  </button>
                {/if}
              </div>
              <span class="vault-date" data-label={translate("accountSettings.lastUsed")}>
                {v.last_used_at ? fmtDate(v.last_used_at) : translate("accountSettings.notUsedYet")}
              </span>
              <div class="vault-document" data-label={translate("accountSettings.lastUnlockedDocument")}>
                {#if v.last_used_doc_id}
                  {#if v.last_used_doc_title}
                    <a href={filingHref(`#/doc/${v.last_used_doc_id}`)} title={v.last_used_doc_title}>{v.last_used_doc_title}</a>
                  {:else}
                    <button class="linkish" onclick={() => notify?.(translate("accountSettings.thatDocumentIsNoLongerAvailable"))}>{translate("accountSettings.noLongerAvailable")}</button>
                  {/if}
                {/if}
              </div>
              <button class="act-quiet vault-remove" disabled={destructiveBusy || !!destructiveRequest} onclick={() => removeVault(v)} title={translate("accountSettings.deleteSavedPassword")} aria-label={translate("accountSettings.deleteSavedPassword")}>{translate("accountSettings.remove")}</button>
            </div>
          {/each}
        </div>
      {:else}
        <p class="empty-setting">{translate("accountSettings.noSavedPasswordsUnlockAProtectedDocument")}</p>
      {/if}
  </section>
  {/if}
{#if destructiveRequest && destructiveDialog}
  <ConfirmDialog
    title={destructiveDialog.title}
    message={destructiveDialog.message}
    confirmLabel={destructiveDialog.confirmLabel}
    busyLabel={destructiveDialog.busyLabel}
    busy={destructiveBusy}
    onConfirm={confirmDestructiveAction}
    onCancel={() => (destructiveRequest = null)} />
{/if}

<style>
  .settings-section { --pad:22px; flex:none; container-type:inline-size; background:var(--surface); border:1px solid var(--line); border-radius:var(--r); padding:0; margin:0; overflow:hidden }
  .section-heading { display:flex; align-items:center; justify-content:space-between; flex-wrap:wrap; gap:8px 24px; margin:0; padding:13px var(--pad); border-bottom:1px solid var(--line) }
  .section-heading > div { display:flex; align-items:baseline; flex-wrap:wrap; column-gap:10px; row-gap:2px; min-width:0; flex:1 1 240px }
  .section-heading h2 { font-size:.95rem; margin:0; white-space:nowrap }
  .section-heading .btn { white-space:nowrap }
  .section-heading p { color:var(--muted); font-size:.82rem; line-height:1.45; margin:0 }
  .profile-meta { display:flex; align-items:center; gap:9px; color:var(--muted); font-size:.78rem; flex:none }
  .profile-layout { display:grid; grid-template-columns:auto minmax(0,1fr); gap:18px 26px; align-items:start; padding:20px var(--pad) 4px }
  .avatar-control { display:flex; flex-direction:column; align-items:center; gap:10px }
  .profile-avatar { width:64px; height:64px; font-size:1.25rem }
  .profile-fields { display:grid; grid-template-columns:minmax(0,1fr) minmax(0,1fr); gap:12px 22px; align-items:start }
  .profile-fields .field { margin:0 }
  .profile-fields input[readonly] { background:var(--surface-2); color:var(--muted) }
  .field :global(.sub) { display:block; color:var(--muted); font-size:.8rem; line-height:1.45; margin-top:4px }
  .card-foot { display:flex; justify-content:flex-end; padding:8px var(--pad) 16px }
  .card-body { padding:8px var(--pad) 16px }
  .act-link { background:none; border:0; padding:0; color:var(--accent); font-weight:600; font-size:.8rem; cursor:pointer }
  .act-link:hover { text-decoration:underline }
  .act-quiet { background:none; border:0; padding:4px 2px; color:var(--muted); font-weight:600; font-size:.8rem; cursor:pointer; flex:none }
  .act-quiet:hover { color:var(--danger) }
  .act-quiet:disabled { opacity:.5; cursor:default }
  .help-row { display:flex; flex-wrap:wrap; align-items:baseline; column-gap:8px; row-gap:2px }
  .help-row .sub { display:inline; margin-top:0 }
  .oidc-sync-form { margin:0; display:flex }
  .section-heading > .auth-syntax { display:flex; align-items:center; justify-content:flex-end; flex-wrap:wrap; gap:4px 8px; min-width:0; flex:0 1 auto; margin-left:auto }
  .auth-syntax span { color:var(--muted); font-size:.72rem; font-weight:600 }
  .auth-syntax code { max-width:100%; padding:5px 8px; border:1px solid var(--line); border-radius:var(--r-sm); background:var(--surface-2); font-size:.74rem; overflow-wrap:anywhere }
  .minted-token { margin:14px var(--pad) 0 }
  .minted-token > label { display:block; color:var(--muted); font-size:.78rem; font-weight:600; margin-bottom:5px }
  .minted-token > div { display:flex; gap:8px }
  .minted-token input { min-width:0; font-size:.76rem }
  .token-form {
    display: grid;
    grid-template-columns: minmax(220px, 1fr) minmax(230px, auto) auto;
    gap: 10px 12px;
    align-items: end;
    margin: 0;
    padding: 16px var(--pad);
    border-bottom: 1px solid var(--line);
  }
  .token-form .field { margin: 0; }
  .field-label { font-size: .78rem; font-weight: 600; color: var(--muted); }
  .token-access { min-height: 37px; }
  .token-access button { flex: 1; white-space: nowrap; }
  .token-create { white-space:nowrap }
  .settings-list { border-top:0 }
  .token-row { display:flex; align-items:center; gap:12px; min-width:0; padding:12px var(--pad); border-bottom:1px solid var(--line) }
  .token-row:last-child, .vault-row:last-child { border-bottom:0 }
  .token-name { flex:1; min-width:0; font-weight:600; overflow-wrap:anywhere }
  .token-name.mono { font-size:.84rem }
  .row-date { display: flex; flex-direction: column; gap: 3px; color:var(--muted); font-size:.78rem; white-space:nowrap }
  .settings-section .err { margin:14px var(--pad) 0 }
  .empty-setting { color:var(--muted); font-size:.84rem; margin:0; padding:14px var(--pad) 16px; border-top:0 }
  .row-ico { width:34px; height:34px; border-radius:10px; background:var(--manila); color:var(--ink); display:flex; align-items:center; justify-content:center; flex:none }
  .vault-columns, .vault-row { display:grid; grid-template-columns:34px minmax(160px,.9fr) minmax(100px,.45fr) minmax(190px,1.4fr) auto; gap:14px; align-items:center }
  .vault-columns { min-height:35px; padding:7px var(--pad); border-bottom:1px solid var(--line); background:var(--surface-2); color:var(--muted); font-size:.64rem; font-weight:700; letter-spacing:.07em; text-transform:uppercase }
  .vault-column-label { grid-column:1 / 3 }
  .vault-column-actions { text-align:right }
  .vault-row { min-height:60px; padding:10px var(--pad); border-bottom:1px solid var(--line) }
  .vault-label-cell { display:flex; align-items:center; gap:5px; min-width:0 }
  .vault-label-text { min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; font-weight:600 }
  .vault-label-text.unlabeled { color:var(--muted); font-style:italic; font-weight:500 }
  .vault-label-editor { min-width:0; font-weight:600 }
  .vault-rename { width:28px; height:28px; flex:none; display:grid; place-items:center; border:0; border-radius:var(--r-sm); background:none; color:var(--muted); cursor:pointer }
  .vault-rename:hover, .vault-rename:focus-visible { background:var(--surface-2); color:var(--ink) }
  .vault-rename:focus-visible { outline:2px solid var(--accent); outline-offset:1px }
  .vault-date { color:var(--muted); font-size:.78rem; white-space:nowrap }
  .vault-document { min-width:0; color:var(--muted); font-size:.78rem }
  .vault-document a { display:block; min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap }
  .vault-remove { justify-self:end }
  .linkish { background:none; border:0; padding:0; color:var(--faint); font:inherit; font-style:italic; cursor:pointer }
  @container (max-width: 680px) {
    .settings-section { --pad:16px }
    .profile-fields { grid-template-columns:minmax(0,1fr) }
    .token-form { grid-template-columns: minmax(0, 1fr); }
    .token-create { width:100%; justify-content:center }
    .vault-columns { display:none }
    .vault-row { grid-template-columns:auto minmax(0,1fr) auto; grid-template-rows:auto auto auto; gap:5px 10px; align-items:center }
    .vault-row .row-ico { grid-column:1; grid-row:1 }
    .vault-label-cell { grid-column:2; grid-row:1 }
    .vault-remove { grid-column:3; grid-row:1 }
    .vault-date, .vault-document { grid-column:2 / -1 }
    .vault-date { grid-row:2 }
    .vault-document { grid-row:3 }
    .vault-date::before, .vault-document::before { content:attr(data-label) " "; color:var(--faint); font-weight:600 }
  }
  @container (max-width: 480px) {
    .profile-layout { grid-template-columns:minmax(0,1fr) }
    .avatar-control { flex-direction:row; justify-content:flex-start }
    .card-foot button { width:100%; justify-content:center }
    .token-row { flex-wrap:wrap }
    .token-name { flex-basis:calc(100% - 100px) }
    .row-date { order:4; flex-basis:100% }
  }
</style>
