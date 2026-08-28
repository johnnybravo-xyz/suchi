<script>
  // Create/edit modal for one mailbox. Sealed secrets never come back from
  // the server, so the password field is always write-only and a blank
  // value on edit means "keep the stored secret".
  import { untrack } from 'svelte'
  import { createEmailAccount, patchEmailAccount, deleteEmailAccount,
           testEmailAccount, previewEmailAccount, revokeEmailOAuth } from './api.js'
  import { session } from './session.svelte.js'
  import Icon from './Icon.svelte'
  import OAuthDeviceCodeModal from './OAuthDeviceCodeModal.svelte'

  let { mode, account, onClose, notify, viewerRole = 'admin', users = [],
        microsoftOAuth = { ready: false, reason: '' } } = $props()

  // Preset table mirrors core/ingest/emailwatch/providers.go.
  const presets = {
    microsoft: { host: 'outlook.office365.com', port: 993, use_tls: 1,
      help: 'Outlook / M365 — click Sign in with Microsoft to complete the device-code flow.' },
    gmail: { host: 'imap.gmail.com', port: 993, use_tls: 1,
      usernamePlaceholder: 'you@gmail.com',
      usernameHelp: 'Use your full Gmail address.',
      credential: {
        note: 'Use a Google app password, not your regular Google password.',
        steps: ['Turn on 2-Step Verification.', 'Create an app password for Suchi.', 'Paste the generated 16-character password above.'],
        links: [
          { label: 'Google App Passwords', href: 'https://myaccount.google.com/apppasswords' },
          { label: 'Google instructions', href: 'https://support.google.com/mail/answer/185833' },
        ],
      } },
    fastmail: { host: 'imap.fastmail.com', port: 993, use_tls: 1,
      help: 'Fastmail — generate an app password under Settings → Password & Security.' },
    icloud: { host: 'imap.mail.me.com', port: 993, use_tls: 1,
      usernamePlaceholder: 'you',
      usernameHelp: 'Usually the part before @icloud.com; try the full address if needed.',
      credential: {
        note: 'Use an Apple app-specific password, not your Apple Account password.',
        steps: ['Turn on two-factor authentication.', 'Create an app-specific password for Suchi.', 'Paste the generated password above.'],
        links: [
          { label: 'Apple Account', href: 'https://account.apple.com/' },
          { label: 'Apple instructions', href: 'https://support.apple.com/102654' },
        ],
      } },
    proton: { host: 'protonmail-bridge', port: 143, use_tls: 0,
      help: 'Proton Bridge via the socat relay (host = protonmail-bridge, port 143).' },
    zoho: { host: 'imap.zoho.com', port: 993, use_tls: 1,
      help: 'Zoho — generate an app password under Security → App Passwords.' },
    custom: { help: 'Configure host/port/TLS by hand.' },
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
  let nextRuleID = 1
  function makeIntakeRule(source = {}) {
    return {
      id: nextRuleID++,
      selection: source.selection || 'all',
      content: source.content || 'email_and_files',
      from: source.from || '',
      recipients: source.recipients || '',
      subject_terms: source.subject_terms || '',
      attachment_names: source.attachment_names || '',
    }
  }
  const seedRules = seed.intake_policy?.rules?.length
    ? seed.intake_policy.rules.map((rule) => makeIntakeRule(rule))
    : [makeIntakeRule()]

  // Members can't pick an owner — server forces owner_id to their own
  // user id. Seed the form so the save-button guard passes without
  // rendering the field.
  const selfOwnerID = untrack(() => (viewerRole !== 'admin' ? (session.user?.user_id || session.user?.id || 0) : 0))

  let form = $state({
    name: seed.name || '',
    owner_id: seed.owner_id || selfOwnerID || 0,
    provider: seed.provider || 'custom',
    host: seed.host || '',
    port: seed.port || 993,
    use_tls: seed.use_tls ?? 1,
    tls_ca_file: seed.tls_ca_file || '',
    folder: seed.folder || 'INBOX',
    processed_folder: seed.processed_folder || '',
    poll_interval_min: seed.poll_interval_min || 10,
    username: seed.username || '',
    password: '',
    after_ingest: seed.processed_folder ? 'move' : (seed.mark_seen ? 'read' : 'leave'),
    sync_since: seedSyncSince,
    oauth_account_id: seed.oauth_account_id || '',
    sealed_secret_b64: '',
    enabled: seed.enabled ?? 1,
  })

  let err = $state('')
  let busy = $state(false)
  let oauthOpen = $state(false)
  let signedInAs = $state('')
  let preview = $state(null)
  let previewError = $state('')
  let intakeRules = $state(seedRules)

  function intakePolicy() {
    return { rules: intakeRules.map((rule) => {
      const wireRule = { selection: rule.selection, content: rule.content }
      if (rule.selection === 'matching') {
        for (const field of ['from', 'recipients', 'subject_terms', 'attachment_names']) {
          const value = rule[field].trim()
          if (value) wireRule[field] = value
        }
      }
      return wireRule
    }) }
  }

  function validateIntakePolicy(policy) {
    if (policy.rules.length < 1) throw new Error('Add at least one intake rule.')
    if (policy.rules.length > 20) throw new Error('A mailbox can have at most 20 intake rules.')
    const invalidRule = policy.rules.findIndex((rule) =>
      rule.selection === 'matching' &&
      !rule.from && !rule.recipients && !rule.subject_terms && !rule.attachment_names)
    if (invalidRule >= 0) throw new Error(`Rule ${invalidRule + 1}: add at least one matching condition.`)
  }

  function addIntakeRule() {
    if (intakeRules.length >= 20) return
    intakeRules.push(makeIntakeRule({ selection: 'matching' }))
    preview = null
    previewError = ''
  }

  function removeIntakeRule(id) {
    if (intakeRules.length === 1) return
    intakeRules = intakeRules.filter((rule) => rule.id !== id)
    preview = null
    previewError = ''
  }

  function applyPreset() {
    const p = presets[form.provider]
    if (!p) return
    // In create mode we happily overwrite; in edit mode only fill blanks
    // so the operator's tuned host/port survive a provider switch.
    if (!isEdit || !form.host) form.host = p.host || form.host
    if (!isEdit || !form.port || form.port === 993) form.port = p.port ?? form.port
    if (!isEdit || form.use_tls === 1) form.use_tls = p.use_tls ?? form.use_tls
    if (!isEdit) {
      form.password = ''
      form.oauth_account_id = ''
      form.sealed_secret_b64 = ''
      signedInAs = ''
    }
  }

  function onOAuthSuccess({ username, oauth_account_id, sealed_secret_b64 }) {
    form.username = username || form.username
    form.oauth_account_id = oauth_account_id || ''
    form.sealed_secret_b64 = sealed_secret_b64 || ''
    if (isEdit) form.enabled = true
    signedInAs = username || oauth_account_id || ''
  }

  async function save() {
    err = ''; busy = true
    try {
      const pollInterval = Number(form.poll_interval_min)
      if (!Number.isInteger(pollInterval) || pollInterval < 1 || pollInterval > 1440) {
        throw new Error('Poll interval must be between 1 and 1440 minutes.')
      }
      const policy = intakePolicy()
      validateIntakePolicy(policy)
      if (form.after_ingest === 'move' && !form.processed_folder.trim()) {
        throw new Error('Choose a folder for processed messages.')
      }
      const body = {
        name: form.name,
        owner_id: Number(form.owner_id) || 0,
        provider: form.provider,
        host: form.host,
        port: Number(form.port) || 993,
        use_tls: !!form.use_tls,
      ...(viewerRole === 'admin' ? { tls_ca_file: form.tls_ca_file } : {}),
        folder: form.folder,
        processed_folder: form.after_ingest === 'move' ? form.processed_folder : '',
        poll_interval_min: pollInterval,
        username: form.username,
        password: form.password,
        intake_policy: policy,
        mark_seen: form.after_ingest === 'read',
        enabled: !!form.enabled,
      }
      if (!isEdit && form.oauth_account_id) body.oauth_account_id = form.oauth_account_id
      if (!isEdit && form.sealed_secret_b64) body.sealed_secret_b64 = form.sealed_secret_b64
      // Strip empty strings so PATCH stays sparse and POST doesn't send
      // an empty password for OAuth accounts.
      for (const k of Object.keys(body)) {
        if ((body[k] === '' && k !== 'processed_folder') || body[k] === null || body[k] === undefined) delete body[k]
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

  async function previewPolicy() {
    if (!isEdit) return
    previewError = ''; preview = null; busy = true
    try {
      const policy = intakePolicy()
      validateIntakePolicy(policy)
      preview = await previewEmailAccount(account.id, policy)
    } catch (ex) {
      previewError = ex.data?.message || ex.message || 'Could not preview this mailbox.'
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
    if (!confirm('Revoke Microsoft sign-in? The mailbox will be disabled until you sign in again.')) return
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
  const microsoftOAuthReady = $derived(microsoftOAuth?.ready === true)
  const displayedProviderHelp = $derived(
    form.provider === 'microsoft' && !microsoftOAuthReady
      ? (microsoftOAuth?.reason || 'Microsoft OAuth is not configured on this server.')
      : providerHelp
  )
  const isMicrosoft = $derived(form.provider === 'microsoft')
  const oauthCredentialReady = $derived(
    !isMicrosoft ||
      ((isEdit && !!form.oauth_account_id) || !!form.sealed_secret_b64)
  )
  const passwordLabel = $derived(
    form.provider === 'proton' ? 'Bridge password' :
      form.provider === 'custom' ? 'Password / app password' : 'App password'
  )
  const usernamePlaceholder = $derived(presets[form.provider]?.usernamePlaceholder || '')
  const usernameHelp = $derived(presets[form.provider]?.usernameHelp || '')
  const credentialGuide = $derived(presets[form.provider]?.credential || null)
  const shortOAuthID = $derived(
    form.oauth_account_id ? form.oauth_account_id.slice(0, 8) + '…' : ''
  )
  const intakeSummary = $derived(
    `${intakeRules.length} ${intakeRules.length === 1 ? 'rule' : 'rules'} · archive when any rule matches`
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
      {#if viewerRole === 'admin'}
        <div class="field" style="max-width:260px">
          <label for="ma-owner">Owner</label>
          <select id="ma-owner" class="input" bind:value={form.owner_id}>
            <option value={0} disabled>Select a user…</option>
            {#each users as u (u.id)}
              <option value={u.id}>{u.display_name ? `${u.display_name} <${u.email}>` : u.email}</option>
            {/each}
          </select>
        </div>
      {/if}
    </div>

    <div class="toolbar" style="margin-bottom:0">
      <div class="field" style="flex:1;min-width:200px">
        <label for="ma-provider">Provider</label>
        <select id="ma-provider" class="input" bind:value={form.provider} onchange={applyPreset} disabled={isEdit}>
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
    {#if displayedProviderHelp}
      <p class="sub" style="margin:-6px 0 10px;color:var(--muted);font-size:.82rem">{displayedProviderHelp}</p>
    {/if}

    <div class="toolbar" style="margin-bottom:0">
      <div class="field" style="flex:1;min-width:200px">
        <label for="ma-user">Username</label>
        <input id="ma-user" class="input mono" bind:value={form.username}
               placeholder={usernamePlaceholder} autocomplete="off" />
        {#if usernameHelp}<span class="sub" style="font-size:.76rem;color:var(--faint)">{usernameHelp}</span>{/if}
      </div>
    </div>

    {#if !isMicrosoft}
      <div class="field">
        <label for="ma-pw">{passwordLabel}</label>
        <input id="ma-pw" class="input mono" type="password" bind:value={form.password}
               placeholder={isEdit ? 'unchanged if left blank' : (credentialGuide ? 'Paste generated app password' : '')}
               autocomplete="new-password" />
        {#if credentialGuide}
          <div style="margin-top:6px;display:flex;flex-direction:column;gap:5px">
            <span class="sub" style="font-size:.8rem;color:var(--muted)"><b>{credentialGuide.note}</b></span>
            <ol class="sub" style="margin:0 0 0 18px;padding:0;font-size:.78rem;line-height:1.55;color:var(--muted)">
              {#each credentialGuide.steps as step}<li>{step}</li>{/each}
            </ol>
            <div class="toolbar" style="margin:1px 0 0;gap:12px">
              {#each credentialGuide.links as link}
                <a href={link.href} target="_blank" rel="noopener noreferrer"
                   style="font-size:.78rem;display:inline-flex;gap:5px;align-items:center">
                  <Icon name="link" size={12} /> {link.label}
                </a>
              {/each}
            </div>
          </div>
        {/if}
      </div>
    {:else}
      <div class="field">
        <label for="ma-oauth-btn">Microsoft sign-in</label>
        {#if isEdit && form.oauth_account_id}
          <div class="toolbar" style="margin:0;gap:8px">
            <span class="pill ok">Signed in · {shortOAuthID}</span>
            <button id="ma-oauth-btn" class="btn sm" onclick={() => (oauthOpen = true)} disabled={busy}>Re-sign in</button>
            <button class="btn sm" onclick={revoke} disabled={busy}>Revoke sign-in</button>
          </div>
        {:else if !microsoftOAuthReady}
          <p class="sub" style="margin:0;color:var(--warn)">
            {microsoftOAuth?.reason || 'Microsoft OAuth is not configured on this server.'}
          </p>
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

    <section class="intake-panel" aria-labelledby="intake-heading">
      <div class="intake-heading">
        <div>
          <h4 id="intake-heading">Mail to archive</h4>
          <span>{intakeSummary}</span>
        </div>
      </div>

      <div class="rule-list">
        {#each intakeRules as rule, index (rule.id)}
          <fieldset class="rule-block">
            <legend id={`intake-rule-${rule.id}`}>Rule {index + 1}</legend>
            <button type="button" class="btn sm rule-remove"
                    aria-label={`Remove rule ${index + 1}`} title="Remove rule"
                    disabled={intakeRules.length === 1}
                    onclick={() => removeIntakeRule(rule.id)}>
              <Icon name="x" size={12} />
            </button>

            <div class="field">
              <span class="field-label">Accept</span>
              <span class="seg choice-row accept-choice" aria-label={`Rule ${index + 1} messages to accept`}>
                <button type="button" class:on={rule.selection === 'all'}
                        aria-pressed={rule.selection === 'all'}
                        onclick={() => (rule.selection = 'all')}>Every message</button>
                <button type="button" class:on={rule.selection === 'files'}
                        aria-pressed={rule.selection === 'files'}
                        onclick={() => (rule.selection = 'files')}>With files</button>
                <button type="button" class:on={rule.selection === 'matching'}
                        aria-pressed={rule.selection === 'matching'}
                        onclick={() => (rule.selection = 'matching')}>Matching</button>
              </span>
            </div>

            {#if rule.selection === 'matching'}
              <div class="condition-grid">
                <div class="field">
                  <label for={`ma-from-${rule.id}`}>From</label>
                  <input id={`ma-from-${rule.id}`} class="input" bind:value={rule.from}
                         placeholder="billing@example.com, @trusted.org" />
                </div>
                <div class="field">
                  <label for={`ma-recipients-${rule.id}`}>To or Cc</label>
                  <input id={`ma-recipients-${rule.id}`} class="input" bind:value={rule.recipients}
                         placeholder="receipts@example.com" />
                </div>
                <div class="field">
                  <label for={`ma-subject-${rule.id}`}>Subject contains</label>
                  <input id={`ma-subject-${rule.id}`} class="input" bind:value={rule.subject_terms}
                         placeholder="invoice, statement" />
                </div>
                <div class="field">
                  <label for={`ma-filename-${rule.id}`}>File name</label>
                  <input id={`ma-filename-${rule.id}`} class="input mono" bind:value={rule.attachment_names}
                         placeholder="*.pdf, invoice-*" />
                </div>
              </div>
            {/if}

            <div class="field">
              <span class="field-label">Keep</span>
              <span class="seg choice-row keep-choice" aria-label={`Rule ${index + 1} content to archive`}>
                <button type="button" class:on={rule.content === 'email_and_files'}
                        aria-pressed={rule.content === 'email_and_files'}
                        onclick={() => (rule.content = 'email_and_files')}>Email and files</button>
                <button type="button" class:on={rule.content === 'files_only'}
                        aria-pressed={rule.content === 'files_only'}
                        onclick={() => (rule.content = 'files_only')}>Files only</button>
              </span>
            </div>
          </fieldset>
          {#if index < intakeRules.length - 1}
            <div class="rule-or" role="separator" aria-label="or"><span>OR</span></div>
          {/if}
        {/each}
      </div>

      <div class="rule-footer">
        <button type="button" class="btn sm" onclick={addIntakeRule} disabled={intakeRules.length >= 20}>
          <Icon name="plus" size={12} /> Add rule
        </button>
        <span>{intakeRules.length} / 20</span>
      </div>
      <p class="overlap-note">If rules overlap, Email and files wins.</p>

      {#if isEdit}
        <div class="preview-row">
          <button class="btn sm" disabled={busy} onclick={previewPolicy}>Preview matches</button>
          {#if preview}
            <b>{preview.matched} of {preview.inspected} new messages match</b>
          {/if}
        </div>
        {#if previewError}<div class="err compact-error">{previewError}</div>{/if}
        {#if preview?.samples?.length}
          <div class="preview-list">
            {#each preview.samples as sample}
              <div>
                <b>{sample.subject || '(no subject)'}</b>
                <span>{sample.from || 'Unknown sender'}</span>
                {#if sample.attachments?.length}<small>{sample.attachments.join(', ')}</small>{/if}
              </div>
            {/each}
          </div>
        {/if}
      {/if}
    </section>

    <div class="field">
      <span class="field-label">After archiving</span>
      <span class="seg choice-row" aria-label="After archiving">
        <button type="button" class:on={form.after_ingest === 'leave'}
                onclick={() => (form.after_ingest = 'leave')}>Leave unchanged</button>
        <button type="button" class:on={form.after_ingest === 'read'}
                onclick={() => (form.after_ingest = 'read')}>Mark read</button>
        <button type="button" class:on={form.after_ingest === 'move'}
                onclick={() => (form.after_ingest = 'move')}>Move</button>
      </span>
    </div>
    {#if form.after_ingest === 'move'}
      <div class="field">
        <label for="ma-pfolder">Move to folder</label>
        <input id="ma-pfolder" class="input mono" bind:value={form.processed_folder}
               placeholder="Processed" />
      </div>
    {/if}

    <details class="advanced-settings">
      <summary>Advanced</summary>
      <div class="advanced-body">
        <div class="toolbar" style="margin-bottom:0">
          <div class="field" style="flex:2;min-width:200px">
            <label for="ma-host">Host</label>
            <input id="ma-host" class="input mono" bind:value={form.host} placeholder="imap.example.com" />
          </div>
          <div class="field" style="max-width:110px">
            <label for="ma-port">Port</label>
            <input id="ma-port" class="input mono" type="number" bind:value={form.port} />
          </div>
          <div class="field tls-field">
            <span class="field-label">TLS</span>
            <span class="switch-control">
              <button id="ma-tls" type="button" class="switch" role="switch"
                      aria-checked={form.use_tls} aria-label="Use TLS"
                      onclick={() => (form.use_tls = !form.use_tls)}></button>
              <span>{form.use_tls ? 'On' : 'Off'}</span>
            </span>
          </div>
        </div>

        {#if viewerRole === 'admin'}
          <div class="field">
            <label for="ma-ca">TLS CA file</label>
            <input id="ma-ca" class="input mono" bind:value={form.tls_ca_file}
                   placeholder="/etc/ssl/certs/custom.pem" />
          </div>
        {/if}

        <div class="toolbar" style="margin-bottom:0">
          <div class="field" style="flex:1;min-width:150px">
            <label for="ma-folder">Source folder</label>
            <input id="ma-folder" class="input mono" bind:value={form.folder} />
          </div>
          <div class="field" style="max-width:140px">
            <label for="ma-poll">Poll every (min)</label>
            <input id="ma-poll" class="input" type="number" min="1" max="1440" step="1"
                   bind:value={form.poll_interval_min} />
          </div>
        </div>

        <div class="field">
          <label for="ma-since">Sync mail from</label>
          <input id="ma-since" class="input" type="datetime-local" bind:value={form.sync_since} />
        </div>
      </div>
    </details>

    {#if isEdit}
      <div class="field">
        <label for="ma-en">Mailbox polling</label>
        <span class="switch-control">
          <button id="ma-en" type="button" class="switch" role="switch" aria-checked={form.enabled}
                  aria-label="Poll this mailbox"
                  onclick={() => (form.enabled = !form.enabled)}></button>
          <span>{form.enabled ? 'Enabled' : 'Disabled'}</span>
        </span>
      </div>
    {/if}

    <div class="toolbar" style="margin:14px 0 0">
      <button class="btn primary sm" disabled={busy || !form.name || (viewerRole === 'admin' && !form.owner_id) || !form.username || !oauthCredentialReady} onclick={save}>
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

{#if oauthOpen && microsoftOAuthReady}
  <OAuthDeviceCodeModal
    provider="microsoft"
    accountID={isEdit ? account.id : null}
    {notify}
    onSuccess={onOAuthSuccess}
    onClose={() => (oauthOpen = false)} />
{/if}

<style>
  .intake-panel {
    display: grid;
    gap: 14px;
    margin: 16px 0;
    padding: 16px 0;
    border-block: 1px solid var(--line);
  }
  .intake-heading { display: flex; align-items: start; justify-content: space-between; gap: 16px; }
  .intake-heading h4 { margin: 0 0 3px; font-size: .95rem; }
  .intake-heading span { color: var(--muted); font-size: .78rem; }
  .choice-row { width: max-content; max-width: 100%; }
  .rule-list { display: grid; }
  .rule-block {
    position: relative;
    display: grid;
    gap: 11px;
    min-width: 0;
    margin: 0;
    padding: 12px;
    border: 1px solid var(--line);
    border-radius: 7px;
  }
  .rule-block legend { padding: 0 6px; font-size: .78rem; font-weight: 700; color: var(--muted); }
  .rule-remove { position: absolute; top: 8px; right: 8px; }
  .rule-or { display: flex; align-items: center; gap: 10px; color: var(--muted); font-size: .68rem; font-weight: 750; }
  .rule-or::before, .rule-or::after { content: ''; height: 1px; background: var(--line); flex: 1; }
  .rule-or span { padding: 5px 0; }
  .rule-footer { display: flex; align-items: center; gap: 9px; }
  .rule-footer > span { color: var(--faint); font-size: .72rem; }
  .overlap-note { margin: -5px 0 0; color: var(--muted); font-size: .76rem; }
  .condition-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 10px 12px; }
  .preview-row { display: flex; align-items: center; gap: 10px; font-size: .78rem; }
  .compact-error { margin: 0; }
  .preview-list { border-left: 2px solid var(--accent); padding-left: 12px; }
  .preview-list > div { display: grid; gap: 2px; padding: 5px 0; }
  .preview-list b { font-size: .8rem; }
  .preview-list span, .preview-list small { color: var(--muted); font-size: .74rem; }
  .advanced-settings { margin-top: 14px; border-top: 1px solid var(--line); }
  .advanced-settings summary {
    width: max-content;
    padding: 12px 0 4px;
    color: var(--muted);
    cursor: pointer;
    font-size: .8rem;
    font-weight: 650;
  }
  .advanced-body { display: grid; gap: 10px; padding-top: 8px; }
  .tls-field { min-width: 92px; justify-content: end; }

  @media (max-width: 620px) {
    .condition-grid { grid-template-columns: 1fr; }
    .choice-row { display: grid; width: 100%; }
    .choice-row button { min-height: 38px; }
    .rule-block { padding: 11px 10px; }
    .rule-block .field { min-width: 0; }
    .rule-block .choice-row { grid-template-columns: repeat(2, minmax(0, 1fr)); }
    .rule-block .accept-choice { grid-template-columns: repeat(3, minmax(0, 1fr)); }
    .rule-block .choice-row button { padding-inline: 5px; white-space: normal; }
    .preview-row { align-items: start; flex-direction: column; }
    .tls-field { justify-content: start; }
  }
</style>
