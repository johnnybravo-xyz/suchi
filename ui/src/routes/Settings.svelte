<script>
  import { listTokens, createToken, deleteToken, setupState, patchMe, uploadAvatar,
           listDecryptionPasswords, renameDecryptionPassword, deleteDecryptionPassword,
           adminListUsers } from '../lib/api.js'
  import { session, refreshSession } from '../lib/session.svelte.js'
  import { fmtDate } from '../lib/format.js'
  import { hasCapability } from '../lib/capabilities.js'
  import Icon from '../lib/Icon.svelte'
  import EmailAccounts from '../lib/EmailAccounts.svelte'

  let { notify } = $props()
  let tokens = $state([])
  let err = $state('')
  let newName = $state('')
  let tokenAccess = $state('read')
  let minted = $state('')   // freshly created secret, shown once

  // --- setup wizard state (admin only; quiet on 403) ---
  let setup = $state(null)
  setupState().then(st => (setup = st)).catch(() => {})
  const setupDone = $derived(!!setup?.completed_at)


  let profile = $state({ display_name: session.user?.display_name || '', email: session.user?.email || '' })
  let profileBusy = $state(false)
  let avatarInput
  async function saveProfile() {
    profileBusy = true
    try {
      await patchMe({ display_name: profile.display_name.trim(), email: profile.email.trim() })
      await refreshSession()
      notify?.('Profile saved')
    } catch (ex) {
      notify?.(ex.message || 'Could not save the profile')
    } finally { profileBusy = false }
  }
  async function sendAvatar(file) {
    if (!file) return
    try { await uploadAvatar(file); await refreshSession(); notify?.('Avatar updated') }
    catch (ex) {
      notify?.(ex.message || 'Could not upload the avatar')
    }
  }

  async function load() {
    try {
      const res = await listTokens()
      tokens = res?.results || res || []
    } catch (ex) { err = ex.message || 'Could not load tokens.' }
  }

  // ---- decryption-password vault ----
  let vault = $state([])
  let vaultErr = $state('')
  async function loadVault() {
    try {
      const res = await listDecryptionPasswords()
      vault = res?.results || res || []
    } catch (ex) { vaultErr = ex.message || 'Could not load the vault.' }
  }
  async function renameVault(v, next) {
    const trimmed = (next || '').trim()
    if (trimmed === (v.label || '')) return
    try {
      await renameDecryptionPassword(v.id, trimmed)
      v.label = trimmed
      vault = vault
      notify?.('Label updated')
    } catch (ex) { notify?.(ex.message || 'Could not rename') }
  }
  async function removeVault(v) {
    if (!confirm(`Delete the vault entry${v.label ? ' “' + v.label + '”' : ''}? Docs it already unlocked stay unlocked; future uploads with the same password won't auto-decrypt.`)) return
    try {
      await deleteDecryptionPassword(v.id)
      vault = vault.filter(x => x.id !== v.id)
      notify?.('Vault entry removed')
    } catch (ex) { notify?.(ex.message || 'Could not remove') }
  }

  async function mint(e) {
    e.preventDefault()
    const name = newName.trim()
    if (!name) {
      notify?.('Give the token a name')
      return
    }
    try {
      const scopes = tokenAccess === 'write'
        ? 'documents:read,documents:write'
        : 'documents:read'
      const res = await createToken({ name, scopes })
      minted = res?.token || ''
      newName = ''
      notify?.('Token created — copy it now, it is shown once')
      load()
    } catch (ex) { notify?.(ex.message || 'Could not create a token') }
  }

  async function revoke(t) {
    if (!confirm(`Revoke “${t.name}”? Anything using it stops working immediately.`)) return
    try { await deleteToken(t.id); tokens = tokens.filter(x => x.id !== t.id); notify?.('Token revoked') }
    catch (ex) { notify?.(ex.message || 'Could not revoke') }
  }

  function tokenAccessLabel(scopes) {
    const values = (scopes || '').split(',').map(value => value.trim()).filter(Boolean).sort()
    if (values.length === 1 && values[0] === 'documents:read') return 'Read only'
    if (values.length === 2 && values[0] === 'documents:read' && values[1] === 'documents:write') return 'Read & write'
    return 'Custom'
  }

  // ---- mailboxes ----
  // Admin sees everyone's mailboxes and needs the user roster for the
  // owner picker; a member only sees their own rows (server filters)
  // and has no picker, so we skip the admin-only /users fetch for them.
  const mailboxesVisible = $derived(hasCapability(session.user, 'mailboxes'))
  let mbxUsers = $state([])
  async function loadMbxUsers() {
    if (session.user?.role !== 'admin') { mbxUsers = []; return }
    try { const r = await adminListUsers(); mbxUsers = r?.results || [] }
    catch { mbxUsers = [] }
  }

  load()
  loadVault()
  loadMbxUsers()
</script>

<div class="content-narrow settings-page">
  <section class="settings-section" aria-labelledby="profile-heading">
    <div class="section-heading">
      <h2 id="profile-heading">Profile</h2>
      <div class="profile-meta">
        <span class="pill">{session.user?.role}</span>
        <span>Signed in with {session.user?.authn_by}</span>
      </div>
    </div>
    <div class="profile-layout">
      <div class="avatar-control">
        <span class="avatar profile-avatar">
          {#if session.user?.avatar_url}<img src={session.user.avatar_url} alt="" />{:else}{(profile.display_name || session.user?.email || '?').split(/[\s@._-]+/).filter(Boolean).slice(0,2).map(w=>w[0].toUpperCase()).join('')}{/if}
        </span>
        <button class="btn sm" onclick={() => avatarInput.click()}>Change photo</button>
        <input bind:this={avatarInput} type="file" accept="image/png,image/jpeg" hidden onchange={(e) => sendAvatar(e.target.files[0])} />
      </div>
      <div class="profile-fields">
        <div class="field">
          <label for="p-name">Display name</label>
          <input id="p-name" class="input" bind:value={profile.display_name} />
        </div>
        <div class="field">
          <label for="p-email">Email</label>
          <input id="p-email" class="input" type="email" bind:value={profile.email} />
        </div>
        <button class="btn primary sm profile-save" disabled={profileBusy} onclick={saveProfile}>Save profile</button>
      </div>
    </div>
  </section>

  <section class="settings-section" aria-labelledby="tokens-heading">
    <div class="section-heading">
      <div>
        <h2 id="tokens-heading">API tokens</h2>
        <p>Named access for mobile apps, scripts, and <code>suchi mcp</code>.</p>
      </div>
      <div class="auth-syntax">
        <span>Request header</span>
        <code>Authorization: Token &lt;token&gt;</code>
      </div>
    </div>
    {#if err}<div class="err">{err}</div>{/if}
    {#if minted}
      <div class="minted-token">
        <label for="minted">New token — copy it now, it is not shown again</label>
        <div>
          <input id="minted" class="input mono" readonly value={minted} />
          <button class="btn sm" type="button" onclick={() => { navigator.clipboard?.writeText(minted); notify?.('Token copied') }}>Copy</button>
        </div>
      </div>
    {/if}
    <form class="token-form" onsubmit={mint}>
      <div class="field">
        <label for="token-name">Token name</label>
        <input id="token-name" class="input" placeholder="e.g. phone or archive-script"
               maxlength="64" required bind:value={newName} />
      </div>
      <div class="field">
        <span class="field-label">Access</span>
        <span class="seg token-access" aria-label="Token access">
          <button type="button" class:on={tokenAccess === 'read'} aria-pressed={tokenAccess === 'read'}
                  onclick={() => tokenAccess = 'read'}>Read only</button>
          <button type="button" class:on={tokenAccess === 'write'} aria-pressed={tokenAccess === 'write'}
                  onclick={() => tokenAccess = 'write'}>Read &amp; write</button>
        </span>
      </div>
      <button class="btn primary token-create"><Icon name="plus" size={13} /> Create token</button>
    </form>
    {#if tokens.length}
      <div class="settings-list token-list">
        {#each tokens as t (t.id)}
          <div class="token-row">
            <span class="token-name">{t.name}</span>
            <span class="pill" title={t.scopes}>{tokenAccessLabel(t.scopes)}</span>
            <span class="row-date">Created {t.created_at ? fmtDate(t.created_at) : '—'}</span>
            <button class="btn sm danger" onclick={() => revoke(t)}>Revoke</button>
          </div>
        {/each}
      </div>
    {:else}
      <p class="empty-setting">No API tokens.</p>
    {/if}
  </section>

  {#if mailboxesVisible}
    <section class="settings-section" aria-labelledby="mailboxes-heading">
      <div class="section-heading">
        <div>
          <h2 id="mailboxes-heading">Mailboxes</h2>
          <p>Connected inboxes and their latest sync status.</p>
        </div>
      </div>
        <EmailAccounts {notify} viewerRole={session.user?.role} users={mbxUsers} />
    </section>
  {/if}

  <section class="settings-section" aria-labelledby="passwords-heading">
      <div class="section-heading">
        <div>
          <h2 id="passwords-heading">Saved decryption passwords</h2>
          <p>Encrypted passwords reused when matching protected documents arrive.</p>
        </div>
      </div>
      {#if vaultErr}<div class="err">{vaultErr}</div>{/if}
      {#if vault.length}
        <div class="settings-list vault-list">
          {#each vault as v (v.id)}
            <div class="vault-row">
              <input class="inline-edit vault-label" value={v.label || ''}
                     placeholder="unlabeled"
                     title={v.label || 'Unlabeled saved password'}
                     onblur={(e) => renameVault(v, e.target.value)}
                     onkeydown={(e) => { if (e.key === 'Enter') e.target.blur() }}
                     aria-label="Saved password label" />
              <div class="vault-use">
                <span>{v.last_used_at ? `Last used ${fmtDate(v.last_used_at)}` : 'Not used yet'}</span>
                {#if v.last_used_doc_id}
                  {#if v.last_used_doc_title}
                    <a href={`#/doc/${v.last_used_doc_id}`} title={v.last_used_doc_title}>{v.last_used_doc_title}</a>
                  {:else}
                    <button class="linkish" onclick={() => notify?.('That document is no longer available')}>(no longer available)</button>
                  {/if}
                {/if}
              </div>
              <button class="btn sm danger" onclick={() => removeVault(v)} title="Delete saved password" aria-label="Delete saved password">
                <Icon name="trash" size={13} />
              </button>
            </div>
          {/each}
        </div>
      {:else}
        <p class="empty-setting">No saved passwords. Unlock a protected document from Inbox to save one.</p>
      {/if}
  </section>

  {#if setup !== null}
    <section class="setup-row" aria-label="Setup wizard">
      <div>
        <b>{setupDone ? 'Setup wizard' : 'Setup is incomplete'}</b>
        <span>{setupDone ? 'Revisit filing, intake, OCR, classification, or backups.' : 'Finish the guided archive configuration when you are ready.'}</span>
      </div>
      <a role="button" class="btn sm" class:primary={!setupDone} href="#/setup">{setupDone ? 'Open setup' : 'Continue setup'}</a>
    </section>
  {/if}
</div>

<style>
  .settings-page { width:100%;max-width:1120px;gap:0 }
  .settings-section { padding:6px 0 28px;margin-bottom:26px;border-bottom:1px solid var(--line) }
  .section-heading { display:flex;align-items:flex-start;justify-content:space-between;gap:24px;margin-bottom:18px }
  .section-heading h2 { font-size:1.05rem;margin:0 }
  .section-heading p { color:var(--muted);font-size:.84rem;line-height:1.45;margin:5px 0 0 }
  .profile-meta { display:flex;align-items:center;gap:9px;color:var(--muted);font-size:.78rem }
  .profile-layout { display:grid;grid-template-columns:auto minmax(0,1fr);gap:18px;align-items:start }
  .avatar-control { display:flex;flex-direction:column;align-items:center;gap:8px }
  .profile-avatar { width:64px;height:64px;font-size:1.25rem }
  .profile-fields { display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1fr);gap:12px;align-items:end }
  .profile-fields .field { margin:0 }
  .profile-save { justify-self:start }
  .auth-syntax { display:flex;flex-direction:column;align-items:flex-end;gap:4px;min-width:0 }
  .auth-syntax span { color:var(--muted);font-size:.72rem;font-weight:600 }
  .auth-syntax code { max-width:100%;padding:6px 8px;border:1px solid var(--line);border-radius:4px;background:var(--surface-2);font-size:.75rem;overflow-wrap:anywhere }
  .minted-token { margin-bottom:14px }
  .minted-token > label { display:block;color:var(--muted);font-size:.78rem;font-weight:600;margin-bottom:5px }
  .minted-token > div { display:flex;gap:8px }
  .minted-token input { min-width:0;font-size:.76rem }
  .token-form {
    display: grid;
    grid-template-columns: minmax(220px, 1fr) minmax(230px, auto) auto;
    gap: 10px 12px;
    align-items: end;
    margin-bottom: 16px;
  }
  .token-form .field { margin: 0; }
  .field-label { font-size: .78rem; font-weight: 600; color: var(--muted); }
  .token-access { min-height: 37px; }
  .token-access button { flex: 1; white-space: nowrap; }
  .token-create { white-space:nowrap }
  .settings-list { border-top:1px solid var(--line) }
  .token-row { display:flex;align-items:center;gap:12px;min-width:0;padding:12px 0;border-bottom:1px solid var(--line) }
  .token-row:last-child, .vault-row:last-child { border-bottom:0 }
  .token-name { flex:1;min-width:0;font-weight:600;overflow-wrap:anywhere }
  .row-date { color:var(--muted);font-size:.78rem;white-space:nowrap }
  .empty-setting { color:var(--muted);font-size:.84rem;margin:0;padding:14px 0;border-top:1px solid var(--line) }
  .vault-row { display:grid;grid-template-columns:minmax(220px,.9fr) minmax(0,1.4fr) auto;gap:18px;align-items:center;padding:12px 0;border-bottom:1px solid var(--line) }
  .vault-label { width:100%;min-width:0;font-weight:600 }
  .vault-use { display:flex;align-items:center;gap:6px 14px;min-width:0;color:var(--muted);font-size:.78rem }
  .vault-use a { min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap }
  .linkish { background:none;border:0;padding:0;color:var(--faint);font:inherit;font-style:italic;cursor:pointer }
  .setup-row { display:flex;align-items:center;justify-content:space-between;gap:20px;padding:2px 0 8px }
  .setup-row > div { display:flex;flex-direction:column;gap:3px }
  .setup-row b { font-size:.86rem }
  .setup-row span { color:var(--muted);font-size:.8rem }
  @media (max-width: 760px) {
    .section-heading { flex-direction:column;gap:10px }
    .auth-syntax { align-items:flex-start;width:100% }
    .profile-fields { grid-template-columns:minmax(0,1fr) }
    .token-form { grid-template-columns: minmax(0, 1fr); }
    .token-create { width:100%;justify-content:center }
    .vault-row { grid-template-columns:minmax(0,1fr) auto;gap:8px 12px }
    .vault-use { grid-column:1 / -1;grid-row:2 }
    .setup-row { align-items:flex-start }
  }
  @media (max-width: 520px) {
    .profile-layout { grid-template-columns:minmax(0,1fr) }
    .avatar-control { flex-direction:row;justify-content:flex-start }
    .profile-save { width:100%;justify-content:center }
    .token-row { flex-wrap:wrap }
    .token-name { flex-basis:calc(100% - 100px) }
    .row-date { order:4;flex-basis:100% }
    .setup-row { flex-direction:column }
  }
</style>
