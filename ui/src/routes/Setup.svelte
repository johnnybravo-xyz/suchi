<script>
  import { setupState, setupComplete, saveSetupIntent, adminCreateUser, adminListUsers, applyPreset,
           getLLMSettings, saveLLMSettings, testLLMSettings,
           getPreferences, savePreferences, getIngestSettings, saveIngestSettings,
           listPresets } from '../lib/api.js'
  import { isLocalEndpoint } from '../lib/net.js'
  import EmailAccounts from '../lib/EmailAccounts.svelte'
  import TaxonomyImport from '../lib/TaxonomyImport.svelte'
  import { USER_CAPABILITIES } from '../lib/capabilities.js'

  let { notify, onDone, onTaxonomyChanged } = $props()

  const STEPS = [
    { name: 'archive',     label: 'Your archive' },
    { name: 'users',       label: 'People' },
    { name: 'sources',     label: 'Ingest sources' },
    { name: 'mail',        label: 'Email intake' },
    { name: 'llm',         label: 'Classification (LLM)' },
    { name: 'automations', label: 'Automations' },
    { name: 'preferences', label: 'OCR & backups' },
  ]
  const ENABLE_SETUP_TAXONOMY_IMPORT = false
  const INTENTS = [
    { id: 'personal', label: 'Personal', description: 'IDs, taxes, health, receipts, and everyday administration.', preset: 'solo' },
    { id: 'household', label: 'Household', description: 'Shared finances, home, school, activities, and family records.', preset: 'household' },
    { id: 'freelance', label: 'Freelance', description: 'Clients, contracts, projects, invoices, and self-employment tax.', preset: 'freelance' },
    { id: 'small_business', label: 'Small business', description: 'Customer billing, vendor bills, payroll, and compliance.', preset: 'smb_billing' },
    { id: 'custom', label: 'Choose myself', description: 'Compare every filing tree before deciding.', preset: '' },
  ]
  // Server is the source of truth (GET /api/presets/); this list is
  // only the offline fallback so the step never renders empty.
  const FALLBACK_PRESETS = [
    { id: 'solo', name: 'Solo', description: 'One person: life admin, money, health, home.', areas: [] },
    { id: 'household', name: 'Household', description: 'A family: shared areas plus per-person categories.', areas: [] },
    { id: 'freelance', name: 'Freelance', description: 'Clients, invoicing, taxes, contracts.', areas: [] },
    { id: 'smb_billing', name: 'Small business', description: 'AP/AR heavy: vendors, invoices, compliance.', areas: [] },
    { id: 'blank', name: 'Blank', description: 'No tree. Build your own from scratch.', blank: true, areas: [] },
  ]
  let presets = $state(FALLBACK_PRESETS)
  listPresets().then(r => { const rows = r?.results || r || []; if (rows.length) presets = rows }).catch(() => {})
  const loaded = new Set()
  function loadOnce(key, fn) {
    if (loaded.has(key)) return
    loaded.add(key)
    fn()
  }

  let cur = $state('archive')
  let busy = $state(false)
  let err = $state('')

  // step-local form state
  let user = $state({ email: '', password: '', display_name: '', role: 'member', capabilities: [] })
  let mailUsers = $state([])
  function toggleCap(slug) {
    user.capabilities = user.capabilities.includes(slug)
      ? user.capabilities.filter(s => s !== slug)
      : [...user.capabilities, slug]
  }
  let preset = $state({ preset_id: 'solo', confirm_blank: false, refile: false, include_seeds: true })
  let intent = $state('')
  let filingTreeChosen = $state(false)
  let showAllPresets = $state(false)
  let jdTab = $state('presets')
  let llm = $state({
    enabled: false, endpoint_url: '', model: '', api_key: '', clear_api_key: false,
    egress_ack: false, confidence_threshold: 0.7,
    archive_enabled: true, archive_auto_threshold: 0.9, archive_review_threshold: 0.5,
  })
  let llmStatus = $state(null)
  let llmTesting = $state(false)
  let llmMode = $state('local')
  let llmTestResult = $state(null)
  let llmTestError = $state('')
  let prefs = $state({ backup_interval_hours: 24, ocr_languages: 'eng' })
  let ingest = $state({ fs_watch_dir: '', fs_watch_owner_email: '' })

  setupState().then(st => {
    intent = st?.intent || ''
    filingTreeChosen = !!st?.filing_tree_chosen
    const selected = st?.current_preset || st?.recommended_preset
    if (selected) preset.preset_id = selected
    showAllPresets = intent === 'custom'
  }).catch(() => {})

  async function loadUsers() {
    try {
      const result = await adminListUsers()
      mailUsers = result?.results || result || []
      seedSourceOwner()
    } catch {}
  }

  function seedSourceOwner() {
    if (!ingest.fs_watch_owner_email) {
      ingest.fs_watch_owner_email = mailUsers.find(u => !u.disabled)?.email || ''
    }
  }
  async function createSetupUser() {
    const result = await adminCreateUser(user)
    await loadUsers()
    return result
  }

  function chooseIntent(option) {
    intent = option.id
    showAllPresets = option.id === 'custom'
    if (option.preset) preset.preset_id = option.preset
  }

  async function saveIntent() {
    return saveSetupIntent(intent)
  }

  async function saveArchive() {
    await saveIntent()
    const result = await applyPreset(preset)
    filingTreeChosen = true
    await onTaxonomyChanged?.()
    return result
  }

  async function loadLLM() {
    try {
      const st = await getLLMSettings()
      llmStatus = st
      llm.enabled = !!st?.enabled
      llm.endpoint_url = st?.endpoint_url || 'http://host.suchi.local:11434/v1'
      llm.model = st?.model || 'qwen2.5:7b'
      llm.egress_ack = !!st?.egress_ack
      llm.confidence_threshold = st?.confidence_threshold ?? 0.7
      llm.archive_enabled = st?.archive_enabled ?? true
      llm.archive_auto_threshold = st?.archive_auto_threshold ?? 0.9
      llm.archive_review_threshold = st?.archive_review_threshold ?? 0.5
      llm.api_key = ''
      llm.clear_api_key = false
      llmMode = st?.endpoint_url && !isLocalEndpoint(st.endpoint_url) ? 'hosted' : 'local'
    } catch {}
  }
  async function loadPreferences() {
    try {
      const current = await getPreferences()
      prefs.backup_interval_hours = current?.backup_interval_hours ?? 24
      prefs.ocr_languages = (current?.ocr_languages || ['eng']).join(',')
    } catch {}
  }
  async function loadIngest() {
    try {
      const current = await getIngestSettings()
      ingest.fs_watch_dir = current?.fs_watch_dir || ''
      ingest.fs_watch_owner_email = current?.fs_watch_owner_email || ''
      seedSourceOwner()
    } catch {}
  }
  $effect(() => {
    if (cur === 'users' || cur === 'sources' || cur === 'mail') loadOnce('users', loadUsers)
    if (cur === 'sources') loadOnce('ingest', loadIngest)
    if (cur === 'llm') loadOnce('llm', loadLLM)
    if (cur === 'preferences') loadOnce('preferences', loadPreferences)
  })

  const idx = $derived(STEPS.findIndex(s => s.name === cur))

  function advance() {
    if (idx < STEPS.length - 1) cur = STEPS[idx + 1].name
  }

  async function saveAnd(fn, label) {
    err = ''; busy = true
    try {
      const result = await fn()
      notify?.(typeof label === 'function' ? label(result) : label)
      advance()
    }
    catch (ex) { err = ex.message || 'The server rejected that.' }
    finally { busy = false }
  }

  async function finish() {
    err = ''; busy = true
    try {
      await setupComplete()
      notify?.('Setup complete — you can revisit any step from Settings')
      onDone?.()
    } catch (ex) { err = ex.message || 'Could not finish setup.' }
    finally { busy = false }
  }

  function llmPayload(enabled) {
    return { ...llm, enabled, api_key: llm.api_key || '' }
  }

  function setLLMMode(mode) {
    llmMode = mode
    llmTestResult = null
    llmTestError = ''
    if (mode === 'local' && (!llm.endpoint_url || !isLocalEndpoint(llm.endpoint_url))) {
      llm.endpoint_url = 'http://host.suchi.local:11434/v1'
      if (!llm.model) llm.model = 'qwen2.5:7b'
      llm.egress_ack = false
    } else if (mode === 'hosted' && isLocalEndpoint(llm.endpoint_url)) {
      llm.endpoint_url = ''
      llm.egress_ack = false
    }
  }

  function setClearAPIKey(event) {
    llm.clear_api_key = event.currentTarget.checked
    if (llm.clear_api_key) llm.api_key = ''
  }

  async function saveClassifier(enabled) {
    const result = await saveLLMSettings(llmPayload(enabled))
    await loadLLM()
    return result
  }

  async function testClassifier() {
    err = ''; llmTesting = true; llmTestResult = null; llmTestError = ''
    try {
      const result = await testLLMSettings(llmPayload(true))
      llmTestResult = result?.result || null
      notify?.(result?.message || 'Classifier connection passed')
    } catch (ex) {
      llmTestError = ex.message || 'The classifier did not return a valid response.'
    } finally { llmTesting = false }
  }

  const llmIsRemote = $derived(!!llm.endpoint_url && !isLocalEndpoint(llm.endpoint_url))
  const recommendedPresetID = $derived(INTENTS.find(x => x.id === intent)?.preset || '')
  const recommendedPreset = $derived(presets.find(x => x.id === recommendedPresetID))
  const visiblePresets = $derived(
    showAllPresets || !recommendedPreset ? presets : [recommendedPreset]
  )
</script>

<div class="wizard">
  <aside class="wiz-steps">
    <div class="side-head" style="padding-left:0">Setup</div>
    {#each STEPS as s}
      <button class="wiz-step" class:on={cur === s.name} onclick={() => (cur = s.name)}>
        <span class="dot" class:accent={cur === s.name}></span>
        <span class="grow">{s.label}</span>
      </button>
    {/each}
    <button class="btn primary" style="margin-top:14px;justify-content:center" onclick={finish}
            disabled={busy || !filingTreeChosen} title={filingTreeChosen ? '' : 'Choose a filing tree first'}>
      Finish setup
    </button>
    <p class="sub" style="font-size:.72rem;color:var(--faint);margin-top:8px">
      Choose a filing tree to finish setup. Every other step is optional and can be revisited later.
    </p>
  </aside>

  <div class="card wiz-body">
    {#if err}<div class="err">{err}</div>{/if}

    {#if cur === 'archive'}
      <h3>What are you organizing?</h3>
      <p class="wiz-p">Pick the closest fit. Suchi will recommend a ready-made filing tree, and every option remains editable.</p>
      <div class="intent-grid">
        {#each INTENTS as option (option.id)}
          <button class="intent-choice" class:on={intent === option.id} onclick={() => chooseIntent(option)}>
            <b>{option.label}</b><span class="sub">{option.description}</span>
          </button>
        {/each}
      </div>

      {#if intent}
        <h3 class="section-heading">Choose a filing tree
          {#if !filingTreeChosen}<span class="pill warn" style="margin-left:8px">Required to finish setup</span>{/if}
        </h3>
        <p class="wiz-p">Start with the recommendation or compare every ready-made tree. You can switch later.</p>
        {#if ENABLE_SETUP_TAXONOMY_IMPORT}
          <span class="seg" style="margin-bottom:14px">
            <button class:on={jdTab === 'presets'} onclick={() => (jdTab = 'presets')}>Presets</button>
            <button class:on={jdTab === 'import'} onclick={() => (jdTab = 'import')}>Import a file</button>
          </span>
        {/if}
        {#if ENABLE_SETUP_TAXONOMY_IMPORT && jdTab === 'import'}
          <TaxonomyImport {notify} onApplied={() => saveAnd(saveIntent, 'Archive setup saved')} />
        {:else}
          {#if recommendedPreset && !showAllPresets}
            <div class="toolbar" style="margin:0 0 12px">
              <span class="pill ok">Recommended for {INTENTS.find(x => x.id === intent)?.label}</span>
              <button class="btn sm" onclick={() => (showAllPresets = true)}>Compare all filing trees</button>
            </div>
          {/if}
          <div class="preset-grid">
            {#each visiblePresets as p (p.id)}
              <label class="preset" class:on={preset.preset_id === p.id}>
                <input type="radio" bind:group={preset.preset_id} value={p.id} hidden />
                <b>{p.name}</b><span class="sub">{p.description}</span>
                {#if p.areas?.length}
                  <span class="preset-tree">
                    {#each p.areas as a}<span class="chip" title={`${a.category_count} categories`}>{a.code}–{a.code + 9} {a.name}</span>{/each}
                  </span>
                {/if}
              </label>
            {/each}
          </div>
          {#if presets.find(p => p.id === preset.preset_id)?.blank || preset.preset_id === 'blank'}
            <label class="wiz-check"><input type="checkbox" bind:checked={preset.confirm_blank} />
              I understand documents will pile up in the inbox until I build categories.</label>
          {/if}
          <label class="wiz-check"><input type="checkbox" bind:checked={preset.include_seeds} />
            Install the preset's starter automations (recommended). Turn off if you want to start from scratch; you can add them later by re-picking the preset.</label>
          <label class="wiz-check"><input type="checkbox" bind:checked={preset.refile} />
            Refile existing documents into the new tree now.</label>
          <div class="toolbar">
            <button class="btn primary sm" disabled={busy || (preset.preset_id === 'blank' && !preset.confirm_blank)}
                    onclick={() => saveAnd(saveArchive, 'Archive setup saved')}>Apply filing tree</button>
            {#if filingTreeChosen}
              <button class="btn sm" disabled={busy} onclick={() => saveAnd(saveIntent, 'Archive direction saved')}>Keep the current tree</button>
            {/if}
          </div>
        {/if}
      {:else}
        <div class="toolbar">
          <button class="btn sm" onclick={advance}>Skip for now</button>
        </div>
      {/if}
      <p class="migration-note">
        Moving an existing archive? Large export bundles are safer through the CLI.
        <a href="https://docs.suchi.page/importer" target="_blank" rel="noopener">Read the migration guide</a>.
      </p>

    {:else if cur === 'users'}
      <h3>Add another person</h3>
      <p class="wiz-p">You already have the admin account you signed in with. Add family members or teammates here; each gets their own documents and their own inbox.</p>
      <div class="field"><label for="u-email">Email</label><input id="u-email" class="input" type="email" bind:value={user.email} /></div>
      <div class="field"><label for="u-name">Display name</label><input id="u-name" class="input" bind:value={user.display_name} /></div>
      <div class="field"><label for="u-pw">Password</label><input id="u-pw" class="input" type="password" bind:value={user.password} autocomplete="new-password" /></div>
      <div class="field"><label for="u-role">Role</label>
        <select id="u-role" class="input" bind:value={user.role}>
          <option value="member">Member</option><option value="admin">Admin</option>
        </select>
      </div>
      {#if user.role === 'member'}
        <div class="field">
          <span class="input-label">Capabilities</span>
          {#each USER_CAPABILITIES as c (c.key)}
            <label style="display:flex;gap:8px;align-items:center;font-weight:normal;margin-top:4px">
              <input type="checkbox" checked={user.capabilities.includes(c.key)}
                     onchange={() => toggleCap(c.key)} />
              {c.setupLabel}
            </label>
          {/each}
        </div>
      {/if}
      <div class="toolbar">
        <button class="btn primary sm" disabled={busy || !user.email || !user.password}
                onclick={() => saveAnd(createSetupUser, 'User created')}>Create user</button>
        <button class="btn sm" onclick={advance}>Just me for now</button>
      </div>

    {:else if cur === 'sources'}
      <h3>Ingest sources</h3>
      <p class="wiz-p">Point suchi at a folder (a scanner target, a synced directory) and everything dropped there becomes a document. Uploads and the API work regardless.</p>
      <div class="field"><label for="i-dir">Watched directory (on the server)</label>
        <input id="i-dir" class="input mono" placeholder="/data/staging" bind:value={ingest.fs_watch_dir} /></div>
      <div class="field"><label for="i-owner">Documents from it belong to</label>
        <select id="i-owner" class="input" bind:value={ingest.fs_watch_owner_email}>
          <option value="">Select owner</option>
          {#each mailUsers.filter(u => !u.disabled) as u}
            <option value={u.email}>{u.display_name || u.email} · {u.email}</option>
          {/each}
        </select></div>
      <div class="toolbar">
        <button class="btn primary sm" disabled={busy || !ingest.fs_watch_dir || !ingest.fs_watch_owner_email}
                onclick={() => saveAnd(() => saveIngestSettings(ingest), 'Ingest source saved')}>Save source</button>
        <button class="btn sm" onclick={advance}>Uploads only</button>
      </div>

    {:else if cur === 'mail'}
      <h3>Email intake</h3>
      <p class="wiz-p">Point suchi at one or more mailboxes and forwarded documents file themselves. Credentials stay server-side; the password field never reads back.</p>
      <EmailAccounts {notify} users={mailUsers} />
      <div class="toolbar" style="margin-top:12px">
        <button class="btn primary sm" onclick={advance}>Continue</button>
        <button class="btn sm" onclick={advance}>Skip for now</button>
      </div>

    {:else if cur === 'llm'}
	  <h3>Classification</h3>
	  <p class="wiz-p">Suchi first learns from similar documents already in your archive, then runs your automations. An optional model fills details that remain unresolved.</p>
	  <label class="wiz-check"><input type="checkbox" bind:checked={llm.archive_enabled} /> Learn from similar documents in this archive</label>
	  {#if llm.archive_enabled}
		<div class="field">
		  <label for="archive-auto">Apply archive matches at · {Number(llm.archive_auto_threshold).toFixed(2)}</label>
		  <input id="archive-auto" class="range" type="range" min="0.55" max="0.95" step="0.05"
			 bind:value={llm.archive_auto_threshold}
			 onchange={() => { if (Number(llm.archive_review_threshold) >= Number(llm.archive_auto_threshold)) llm.archive_review_threshold = Number(llm.archive_auto_threshold) - 0.05 }} />
		</div>
		<div class="field">
		  <label for="archive-review">Offer uncertain matches for review at · {Number(llm.archive_review_threshold).toFixed(2)}</label>
		  <input id="archive-review" class="range" type="range" min="0.5" max={Number(llm.archive_auto_threshold) - 0.05} step="0.05"
			 bind:value={llm.archive_review_threshold} />
		</div>
	  {/if}
	  <div class="side-head" style="padding-left:0;margin-top:20px">Optional model</div>
      <div class="toolbar" style="margin:0 0 12px">
        {#if llmStatus?.active}
          <span class="pill ok">Classifier active</span>
        {:else if llmStatus?.enabled}
          <span class="pill warn">Classifier inactive</span>
        {:else}
          <span class="pill">No model</span>
        {/if}
        {#if llmStatus?.has_api_key}<span class="chip">API key configured</span>{/if}
      </div>
      <span class="seg" style="margin-bottom:14px">
        <button class:on={llmMode === 'local'} onclick={() => setLLMMode('local')}>Local model</button>
        <button class:on={llmMode === 'hosted'} onclick={() => setLLMMode('hosted')}>Hosted endpoint</button>
      </span>
      {#if llmMode === 'local'}
        <p class="wiz-p sub" style="font-size:.8rem">The Docker Compose default reaches Ollama on the host. Edit the URL for a native install or another machine on your network.</p>
      {:else}
        <p class="wiz-p sub" style="font-size:.8rem">Use the OpenAI-compatible base URL from your provider. Suchi sends extracted text, never the original file.</p>
      {/if}
      <div class="field"><label for="l-url">Endpoint URL</label>
        <input id="l-url" class="input mono" placeholder="http://host.suchi.local:11434/v1" bind:value={llm.endpoint_url} /></div>
      <div class="field"><label for="l-model">Model</label>
        <input id="l-model" class="input mono" placeholder="qwen2.5:7b" bind:value={llm.model} /></div>
      <div class="field"><label for="l-key">API key (blank for local)</label>
        <input id="l-key" class="input mono" type="password" bind:value={llm.api_key} autocomplete="off"
               disabled={llm.clear_api_key}
               placeholder={llmStatus?.has_api_key ? 'stored key — leave blank to keep' : ''} /></div>
      {#if llmStatus?.has_api_key}
        <label class="wiz-check"><input type="checkbox" checked={llm.clear_api_key} onchange={setClearAPIKey} />
          Clear the saved API key when saving. Config-file and environment keys are unchanged.</label>
      {/if}
      {#if llmIsRemote}
        <label class="wiz-check attn"><input type="checkbox" bind:checked={llm.egress_ack} />
          This endpoint is not local. I acknowledge document text will leave this machine.</label>
      {/if}
      <div class="field">
        <label for="l-confidence">Auto-apply confidence · {Number(llm.confidence_threshold).toFixed(2)}</label>
        <input id="l-confidence" class="range" type="range" min="0.5" max="0.95" step="0.05"
               bind:value={llm.confidence_threshold} />
        <span class="sub" style="font-size:.76rem">Lower applies more model suggestions; higher sends more uncertain documents to review.</span>
      </div>
      <div class="toolbar">
        <button class="btn primary sm" disabled={busy || llmTesting || !llm.endpoint_url || !llm.model || (llmIsRemote && !llm.egress_ack)}
                onclick={() => saveAnd(
                  () => saveClassifier(true),
                  'Classifier configured'
                )}>Save classifier</button>
        <button class="btn sm" disabled={busy || llmTesting || !llm.endpoint_url || !llm.model || (llmIsRemote && !llm.egress_ack)}
                onclick={testClassifier}>Test connection</button>
        <button class="btn sm" disabled={busy || llmTesting}
				onclick={() => saveAnd(() => saveClassifier(false), 'Model disabled; local classification remains active')}>Use local classification only</button>
      </div>
      {#if llmTestError}
        <div class="test-result failed">
          <b>Connection failed</b>
          <span>{llmTestError}</span>
        </div>
      {:else if llmTestResult}
        <div class="test-result">
          <b>Validated in {llmTestResult.elapsed_ms} ms</b>
          <span>{llmTestResult.title || 'No title'} · confidence {Number(llmTestResult.confidence).toFixed(2)}</span>
          {#if llmTestResult.tags?.length}<span class="sub">Tags: {llmTestResult.tags.join(', ')}</span>{/if}
        </div>
      {/if}
      <p class="wiz-p sub" style="font-size:.8rem;margin-top:14px">The classifier runs automatically on new documents. To classify older documents, select them in <a href="#/documents">Documents</a> and use Rescan.</p>

    {:else if cur === 'automations'}
      <h3>Automations</h3>
      <p class="wiz-p">Your preset can install starter filing automations. Preset-owned automations show their filing-tree owner; editing one forks a user-owned copy, so re-picking the preset never overwrites your edits.</p>
		<p class="wiz-p">Automations file documents by title, content, sender, tags, and other metadata.</p>
      <div class="toolbar">
        <a role="button" class="btn primary sm" href="#/automations">Open automations</a>
        <button class="btn sm" onclick={advance}>Done</button>
        <button class="btn sm" onclick={advance}>Skip</button>
      </div>

    {:else if cur === 'preferences'}
      <h3>OCR & backups</h3>
      <div class="field"><label for="p-ocr">OCR languages (comma-separated tesseract codes)</label>
        <input id="p-ocr" class="input mono" placeholder="eng,hin,nep" bind:value={prefs.ocr_languages} /></div>
      <div class="field"><label for="p-bk">Database snapshot interval (hours, 0 disables)</label>
        <input id="p-bk" class="input" type="number" min="0" bind:value={prefs.backup_interval_hours} /></div>
      <p class="wiz-p sub" style="font-size:.8rem">Snapshots cover the database; blobs are plain files — point restic or borg at the data directory for the full story.</p>
      <div class="toolbar">
        <button class="btn primary sm" disabled={busy}
                onclick={() => saveAnd(() => savePreferences({
                  backup_interval_hours: Number(prefs.backup_interval_hours) || 0,
                  ocr_languages: prefs.ocr_languages.split(',').map(x => x.trim()).filter(Boolean),
                }), 'Preferences saved')}>Save preferences</button>
        <button class="btn sm" onclick={advance}>Defaults are fine</button>
      </div>
    {/if}
  </div>
</div>

<style>
  .wizard { display: grid; grid-template-columns: 240px 1fr; gap: 20px; align-items: start; max-width: 920px; }
  .wiz-steps { display: flex; flex-direction: column; gap: 2px; }
  .wiz-step {
    display: flex; align-items: center; gap: 10px; width: 100%;
    padding: 8px 10px; border: 0; background: none; border-radius: var(--r-sm);
    font-size: .88rem; text-align: left; color: var(--muted);
  }
  .wiz-step:hover { background: var(--surface-2); color: var(--ink); }
  .wiz-step.on { background: var(--tint); color: var(--ink); font-weight: 600; }
  .wiz-step .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--line-strong); flex: none; }
  .wiz-step .dot.accent { background: var(--accent); }
  .wiz-step .grow { flex: 1; }
  .wiz-body { min-height: 340px; }
  .wiz-p { color: var(--muted); font-size: .92rem; margin: 6px 0 16px; max-width: 46em; }
  .wiz-check { display: flex; gap: 9px; align-items: baseline; font-size: .86rem; color: var(--muted); margin: 0 0 14px; }
  .wiz-check.attn { color: var(--warn); }
  .preset-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; margin-bottom: 14px; }
  .intent-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; margin-bottom: 14px; }
  .intent-choice {
    display: flex; flex-direction: column; gap: 4px; min-width: 0; padding: 12px 14px;
    border: 1px solid var(--line-strong); border-radius: var(--r-sm); background: var(--surface);
    color: var(--ink); text-align: left;
  }
  .intent-choice:hover { border-color: var(--accent); }
  .intent-choice.on { border-color: var(--accent); background: var(--tint); }
  .intent-choice .sub { color: var(--muted); font-size: .78rem; line-height: 1.35; }
  .section-heading { margin-top: 22px; }
  .migration-note { margin: 18px 0 0; color: var(--faint); font-size: .76rem; }
  .range { width: 100%; accent-color: var(--accent); }
  .test-result {
    display: flex; flex-direction: column; gap: 3px; border-left: 3px solid var(--ok);
    padding: 7px 10px; margin-top: 12px; font-size: .82rem;
  }
  .test-result.failed { border-left-color: var(--danger); }
  @media (max-width: 640px) { .preset-grid { grid-template-columns: 1fr; } }
  @media (max-width: 640px) { .intent-grid { grid-template-columns: 1fr; } }
  .preset {
    display: flex; flex-direction: column; gap: 3px; cursor: pointer;
    border: 1px solid var(--line-strong); border-radius: var(--r-sm); padding: 12px 14px;
  }
  .preset .sub { font-size: .78rem; color: var(--muted); }
  .preset-tree { display: flex; flex-wrap: wrap; gap: 5px; margin-top: 7px; }
  .preset.on { border-color: var(--accent); background: var(--tint); }
  @media (max-width: 780px) {
    .wizard { grid-template-columns: 1fr; gap: 12px; }
    .wiz-steps { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 2px 6px; }
    .wiz-steps > .side-head,
    .wiz-steps > .btn { grid-column: 1 / -1; }
    .wiz-steps > .sub { display: none; }
    .wiz-step { min-width: 0; padding: 7px 8px; font-size: .8rem; }
  }
</style>
