<script>
  // Create/edit modal for one mailbox. Sealed secrets never come back from
  // the server, so the password field is always write-only and a blank
  // value on edit means "keep the stored secret".
  import { untrack } from 'svelte'
  import { createEmailAccount, patchEmailAccount, deleteEmailAccount,
           testEmailAccount, revokeEmailOAuth } from './api.js'
  import Icon from './Icon.svelte'
  import OAuthDeviceCodeModal from './OAuthDeviceCodeModal.svelte'

  let { mode, account, onClose, notify } = $props()

  // Preset table mirrors core/ingest/emailwatch/providers.go.
  const presets = {
    microsoft: { host: 'outlook.office365.com', port: 993, use_tls: 1, auth_method: 'xoauth2',
      help: 'Outlook / M365 — click Sign in with Microsoft to complete the device-code flow.' },
    gmail: { host: 'imap.gmail.com', port: 993, use_tls: 1, auth_method: 'password',
      help: 'Gmail — requires a Google App Password (2FA on).' },
    fastmail: { host: 'imap.fastmail.com', port: 993, use_tls: 1, auth_method: 'password',
      help: 'Fastmail — generate an app password under Settings → Password & Security.' },
    icloud: { host: 'imap.mail.me.com', port: 993, use_tls: 1, auth_method: 'password',
      help: 'iCloud — requires an app-specific password from appleid.apple.com.' },
    proton: { host: 'protonmail-bridge', port: 143, use_tls: 0, auth_method: 'password',
      help: 'Proton Bridge via the socat relay (host = protonmail-bridge, port 143).' },
    zoho: { host: 'imap.zoho.com', port: 993, use_tls: 1, auth_method: 'password',
      help: 'Zoho — generate an app password under Security → App Passwords.' },
    custom: { auth_method: 'password',
      help: 'Configure host/port/TLS by hand.' },
  }

  // Snapshot props once; the modal remounts per-open so a reactive
  // read would just chase the same initial value. untrack() silences
  // the state_referenced_locally warning without adding a dep.
  const isEdit = untrack(() => mode === 'edit')
  const seed = untrack(() => account || {})

  // datetime-local expects "YYYY-MM-DDTHH:mm" in local time. Round-trip
  // helpers: unix seconds ↔ input string. Empty string means "sync all
  // history" (the wire uses `sync_since: 0` for that).
  function unixToLocalInput(sec) {
    if (!sec) return ''
    const d = new Date(sec * 1000)
    const pad = (n) => String(n).padStart(2, '0')
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
  }
  function localInputToUnix(v) {
    if (!v) return 0
    const t = new Date(v).getTime()
    return Number.isFinite(t) ? Math.floor(t / 1000) : 0
  }
  // Create → default to now. Edit → whatever the row already has.
  const seedSyncSince = isEdit
    ? unixToLocalInput(seed.sync_since)
    : unixToLocalInput(Math.floor(Date.now() / 1000))

  let form = $state({
    name: seed.name || '',
    owner_id: seed.owner_id || 0,
    provider: seed.provider || 'custom',
    host: seed.host || '',
    port: seed.port || 993,
    use_tls: seed.use_tls ?? 1,
    tls_ca_file: seed.tls_ca_file || '',
    folder: seed.folder || 'INBOX',
    processed_folder: seed.processed_folder || '',
    poll_interval_min: seed.poll_interval_min || 10,
    auth_method: seed.auth_method || 'password',
    username: seed.username || '',
    password: '',
    attachments_only: seed.attachments_only ?? 0,
    mark_seen: seed.mark_seen ?? false,
    from_allowlist: seed.from_allowlist || '',
    sync_since: seedSyncSince,
    oauth_account_id: seed.oauth_account_id || '',
    sealed_secret_b64: '',
    enabled: seed.enabled ?? 1,
  })

  let err = $state('')
  let busy = $state(false)
  let oauthOpen = $state(false)
  let signedInAs = $state('')

  function applyPreset() {
    const p = presets[form.provider]
    if (!p) return
    // In create mode we happily overwrite; in edit mode only fill blanks
    // so the operator's tuned host/port survive a provider switch.
    if (!isEdit || !form.host) form.host = p.host || form.host
    if (!isEdit || !form.port || form.port === 993) form.port = p.port ?? form.port
    if (!isEdit || form.use_tls === 1) form.use_tls = p.use_tls ?? form.use_tls
    if (!isEdit) form.auth_method = p.auth_method || form.auth_method
  }

  function onOAuthSuccess({ username, oauth_account_id, sealed_secret_b64 }) {
    form.username = username || form.username
    form.oauth_account_id = oauth_account_id || ''
    form.sealed_secret_b64 = sealed_secret_b64 || ''
    form.auth_method = 'xoauth2'
    signedInAs = username || oauth_account_id || ''
  }

  async function save() {
    err = ''; busy = true
    try {
      const body = {
        name: form.name,
        owner_id: Number(form.owner_id) || 0,
        provider: form.provider,
        host: form.host,
        port: Number(form.port) || 993,
        use_tls: !!form.use_tls,
        tls_ca_file: form.tls_ca_file,
        folder: form.folder,
        processed_folder: form.processed_folder,
        poll_interval_min: Number(form.poll_interval_min) || 10,
        auth_method: form.auth_method,
        username: form.username,
        password: form.password,
        oauth_account_id: form.oauth_account_id,
        attachments_only: !!form.attachments_only,
        mark_seen: !!form.mark_seen,
        from_allowlist: form.from_allowlist,
        enabled: !!form.enabled,
      }
      if (form.sealed_secret_b64) body.sealed_secret_b64 = form.sealed_secret_b64
      // Strip empty strings so PATCH stays sparse and POST doesn't send
      // an empty password for OAuth accounts.
      for (const k of Object.keys(body)) {
        if (body[k] === '' || body[k] === null || body[k] === undefined) delete body[k]
      }
      if (!isEdit && !body.tls_ca_file) delete body.tls_ca_file
      // sync_since is always sent (0 = "sync all"), otherwise a cleared
      // input on PATCH would be indistinguishable from "leave alone".
      body.sync_since = localInputToUnix(form.sync_since)

      if (isEdit) {
        await patchEmailAccount(account.id, body)
      } else {
        await createEmailAccount(body)
      }
      notify?.('Mailbox saved')
      onClose?.(true)
    } catch (ex) {
      err = ex.data?.message || ex.message || 'Could not save.'
    } finally { busy = false }
  }

  async function testConn() {
    if (!isEdit) return
    busy = true
    try {
      const r = await testEmailAccount(account.id)
      notify?.(r?.message || (r?.ok ? 'Mailbox reachable.' : 'Test failed.'))
    } catch (ex) {
      notify?.(ex.data?.message || ex.message || 'Test failed.')
    } finally { busy = false }
  }

  async function del() {
    if (!isEdit) return
    if (!confirm('Delete this mailbox?')) return
    busy = true
    try {
      await deleteEmailAccount(account.id)
      notify?.('Mailbox deleted')
      onClose?.(true)
    } catch (ex) {
      err = ex.data?.message || ex.message || 'Could not delete.'
    } finally { busy = false }
  }

  async function revoke() {
    if (!isEdit) return
    if (!confirm('Revoke Microsoft sign-in? The mailbox will be disabled until you re-authenticate or set a password.')) return
    busy = true
    try {
      await revokeEmailOAuth(account.id)
      notify?.('Sign-in revoked')
      onClose?.(true)
    } catch (ex) {
      err = ex.data?.message || ex.message || 'Could not revoke.'
    } finally { busy = false }
  }

  function onKey(e) { if (e.key === 'Escape' && !oauthOpen) onClose?.(false) }

  const providerHelp = $derived(presets[form.provider]?.help || '')
  const shortOAuthID = $derived(
    form.oauth_account_id ? form.oauth_account_id.slice(0, 8) + '…' : ''
  )
</script>

<svelte:window onkeydown={onKey} />

<div class="modal-veil" onclick={() => onClose?.(false)} role="presentation">
  <div class="modal" style="width:min(680px,94vw)"
       onclick={(e) => e.stopPropagation()}
       onkeydown={(e) => e.stopPropagation()}
       role="dialog" aria-modal="true" aria-label={isEdit ? 'Edit mailbox' : 'Add mailbox'} tabindex="-1">
    <div class="modal-head">
      <h3>{isEdit ? 'Edit mailbox' : 'Add mailbox'}</h3>
      <button class="btn sm" onclick={() => onClose?.(false)}><Icon name="x" size={13} /></button>
    </div>

    {#if err}<div class="err" style="margin-bottom:10px">{err}</div>{/if}

    <div class="toolbar" style="margin-bottom:0">
      <div class="field" style="flex:2;min-width:200px">
        <label for="ma-name">Name</label>
        <input id="ma-name" class="input" bind:value={form.name} placeholder="Household mailbox" />
      </div>
      <div class="field" style="max-width:140px">
        <label for="ma-owner">Owner user id</label>
        <input id="ma-owner" class="input" type="number" min="1" bind:value={form.owner_id} />
      </div>
    </div>

    <div class="toolbar" style="margin-bottom:0">
      <div class="field" style="flex:1;min-width:200px">
        <label for="ma-provider">Provider</label>
        <select id="ma-provider" class="input" bind:value={form.provider} onchange={applyPreset}>
          <option value="microsoft">Microsoft / Outlook</option>
          <option value="gmail">Gmail</option>
          <option value="fastmail">Fastmail</option>
          <option value="icloud">iCloud</option>
          <option value="proton">Proton Bridge</option>
          <option value="zoho">Zoho</option>
          <option value="custom">Custom</option>
        </select>
      </div>
    </div>
    {#if providerHelp}
      <p class="sub" style="margin:-6px 0 10px;color:var(--muted);font-size:.82rem">{providerHelp}</p>
    {/if}

    <div class="toolbar" style="margin-bottom:0">
      <div class="field" style="flex:2;min-width:200px">
        <label for="ma-host">Host</label>
        <input id="ma-host" class="input mono" bind:value={form.host} placeholder="imap.example.com" />
      </div>
      <div class="field" style="max-width:110px">
        <label for="ma-port">Port</label>
        <input id="ma-port" class="input mono" type="number" bind:value={form.port} />
      </div>
      <div class="field" style="max-width:110px;justify-content:flex-end">
        <label for="ma-tls">TLS</label>
        <label style="display:flex;gap:6px;align-items:center;padding:8px 0">
          <input id="ma-tls" type="checkbox" bind:checked={form.use_tls} /> Use TLS
        </label>
      </div>
    </div>

    <div class="field">
      <label for="ma-ca">TLS CA file (optional)</label>
      <input id="ma-ca" class="input mono" bind:value={form.tls_ca_file}
             placeholder="/etc/ssl/certs/custom.pem" />
    </div>

    <div class="toolbar" style="margin-bottom:0">
      <div class="field" style="flex:1;min-width:200px">
        <label for="ma-user">Username</label>
        <input id="ma-user" class="input mono" bind:value={form.username} autocomplete="off" />
      </div>
    </div>

    <div class="field">
      <label for="ma-auth-pw">Auth method</label>
      <div class="toolbar" style="margin:0;gap:16px">
        <label style="display:flex;gap:6px;align-items:center">
          <input id="ma-auth-pw" type="radio" name="auth" value="password" bind:group={form.auth_method} />
          Password
        </label>
        <label style="display:flex;gap:6px;align-items:center">
          <input id="ma-auth-oauth" type="radio" name="auth" value="xoauth2" bind:group={form.auth_method} />
          Microsoft OAuth
        </label>
      </div>
    </div>

    {#if form.auth_method === 'password'}
      <div class="field">
        <label for="ma-pw">Password / app token</label>
        <input id="ma-pw" class="input mono" type="password" bind:value={form.password}
               placeholder={isEdit ? 'unchanged if left blank' : ''} autocomplete="new-password" />
      </div>
    {:else}
      <div class="field">
        <label for="ma-oauth-btn">Microsoft sign-in</label>
        {#if isEdit && form.oauth_account_id}
          <div class="toolbar" style="margin:0;gap:8px">
            <span class="pill ok">Signed in · {shortOAuthID}</span>
            <button id="ma-oauth-btn" class="btn sm" onclick={revoke} disabled={busy}>Revoke sign-in</button>
          </div>
        {:else if signedInAs}
          <div class="toolbar" style="margin:0;gap:8px">
            <span class="pill ok">Signed in as {signedInAs}</span>
            <button class="btn sm" onclick={() => (oauthOpen = true)}>Re-sign in</button>
          </div>
        {:else}
          <button id="ma-oauth-btn" class="btn sm" onclick={() => (oauthOpen = true)}>
            <Icon name="mail" size={13} /> Sign in with Microsoft
          </button>
        {/if}
      </div>
    {/if}

    <div class="toolbar" style="margin-bottom:0">
      <div class="field" style="flex:1;min-width:150px">
        <label for="ma-folder">Folder</label>
        <input id="ma-folder" class="input mono" bind:value={form.folder} />
      </div>
      <div class="field" style="flex:1;min-width:150px">
        <label for="ma-pfolder">Processed folder (optional)</label>
        <input id="ma-pfolder" class="input mono" bind:value={form.processed_folder}
               placeholder="Processed" />
      </div>
      <div class="field" style="max-width:140px">
        <label for="ma-poll">Poll every (min)</label>
        <input id="ma-poll" class="input" type="number" min="1" bind:value={form.poll_interval_min} />
      </div>
    </div>

    <div class="field">
      <label for="ma-since">Sync mail from</label>
      <input id="ma-since" class="input" type="datetime-local"
             bind:value={form.sync_since} />
      <span class="sub" style="font-size:.76rem;color:var(--faint)">
        Older messages are ignored. Clear this field to sync the whole archive.
      </span>
    </div>

    <div class="field">
      <label for="ma-att">Attachments</label>
      <label style="display:flex;gap:6px;align-items:center">
        <input id="ma-att" type="checkbox" bind:checked={form.attachments_only} />
        Ingest attachments only (skip the message body)
      </label>
    </div>

    <div class="field">
      <label for="ma-seen">After ingest</label>
      <label style="display:flex;gap:6px;align-items:center">
        <input id="ma-seen" type="checkbox" bind:checked={form.mark_seen} />
        Mark messages as read on the server
      </label>
      <span class="sub" style="font-size:.76rem;color:var(--faint)">
        Off (default) leaves your unread state untouched — suchi tracks a UID cursor so nothing is re-imported.
      </span>
    </div>

    <div class="field">
      <label for="ma-allow">From allowlist</label>
      <input id="ma-allow" class="input mono" bind:value={form.from_allowlist}
             placeholder="alice@example.com, @trusted.org" />
      <span class="sub" style="font-size:.76rem;color:var(--faint)">
        Optional — comma-separated addresses or @domain suffixes.
      </span>
    </div>

    {#if isEdit}
      <div class="field">
        <label for="ma-en">Enabled</label>
        <label style="display:flex;gap:6px;align-items:center">
          <input id="ma-en" type="checkbox" bind:checked={form.enabled} /> Poll this mailbox
        </label>
      </div>
    {/if}

    <div class="toolbar" style="margin:14px 0 0">
      <button class="btn primary sm" disabled={busy || !form.name || !form.owner_id || !form.username} onclick={save}>
        {isEdit ? 'Save changes' : 'Create mailbox'}
      </button>
      {#if isEdit}
        <button class="btn sm" disabled={busy} onclick={testConn}>
          <Icon name="mail" size={13} /> Test connection
        </button>
        <span class="spacer" style="flex:1"></span>
        <button class="btn sm danger" disabled={busy} onclick={del}>
          <Icon name="trash" size={13} /> Delete
        </button>
      {/if}
    </div>
  </div>
</div>

{#if oauthOpen}
  <OAuthDeviceCodeModal
    provider="microsoft"
    {notify}
    onSuccess={onOAuthSuccess}
    onClose={() => (oauthOpen = false)} />
{/if}
