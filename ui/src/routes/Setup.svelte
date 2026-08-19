<script>
  import { setupState, setupStep, setupComplete, saveSetupIntent, adminCreateUser, adminListUsers, applyPreset,
           getLLMSettings, saveLLMSettings, testLLMSettings,
           getPreferences, savePreferences, getIngestSettings, saveIngestSettings,
           listPresets } from '../lib/api.js'
  import { isLocalEndpoint } from '../lib/net.js'
  import { go } from '../lib/router.svelte.js'
  import Icon from '../lib/Icon.svelte'
  import EmailAccounts from '../lib/EmailAccounts.svelte'
  import TaxonomyImport from '../lib/TaxonomyImport.svelte'

  let { notify, onDone } = $props()

  const STEPS = [
    { name: 'welcome',     label: 'Your archive' },
    { name: 'jd',          label: 'Filing tree' },
    { name: 'users',       label: 'People' },
    { name: 'sources',     label: 'Ingest sources' },
    { name: 'mail',        label: 'Email intake' },
    { name: 'llm',         label: 'Classification (LLM)' },
    { name: 'rules',       label: 'Rules' },
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

  let steps = $state({})          // name -> 'done' | 'skipped'
  let cur = $state('welcome')
  let busy = $state(false)
  let err = $state('')

  // step-local form state
  // KNOWN_CAPS mirrors core/authz/capabilities.go. Admin-grantable
  // feature switches on top of the "member" role; admins are implicitly
  // capable of everything so the checkboxes only render for members.
  const KNOWN_CAPS = [
    { slug: 'mailboxes',   label: 'Manage own mailboxes' },
    { slug: 'share_links', label: 'Create share links' },
  ]
  let user = $state({ email: '', password: '', display_name: '', role: 'member', capabilities: [] })
  let mailUsers = $state([])
  function toggleCap(slug) {
    user.capabilities = user.capabilities.includes(slug)
      ? user.capabilities.filter(s => s !== slug)
      : [...user.capabilities, slug]
  }
  let preset = $state({ preset_id: 'solo', confirm_blank: false, refile: false, include_seeds: true })
  let intent = $state('')
  let showAllPresets = $state(false)
  let jdTab = $state('presets')
  let llm = $state({ enabled: false, endpoint_url: '', model: '', api_key: '', clear_api_key: false, egress_ack: false, confidence_threshold: 0.7 })
  let llmStatus = $state(null)
  let llmTesting = $state(false)
  let llmMode = $state('local')
  let llmTestResult = $state(null)
  let prefs = $state({ backup_interval_hours: 24, ocr_languages: 'eng' })
  let ingest = $state({ fs_watch_dir: '', fs_watch_owner_email: '' })

  setupState().then(st => {
    steps = st?.steps || {}
    intent = st?.intent || ''
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
  loadUsers()

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

  async function loadLLM() {
    try {
      const st = await getLLMSettings()
      llmStatus = st
      llm.enabled = !!st?.enabled
      llm.endpoint_url = st?.endpoint_url || 'http://host.suchi.local:11434/v1'
      llm.model = st?.model || 'qwen2.5:7b'
      llm.egress_ack = !!st?.egress_ack
      llm.confidence_threshold = st?.confidence_threshold ?? 0.7
      llm.api_key = ''
      llm.clear_api_key = false
      llmMode = st?.endpoint_url && !isLocalEndpoint(st.endpoint_url) ? 'hosted' : 'local'
    } catch {}
  }
  loadLLM()

  async function loadPreferences() {
    try {
      const current = await getPreferences()
      prefs.backup_interval_hours = current?.backup_interval_hours ?? 24
      prefs.ocr_languages = (current?.ocr_languages || ['eng']).join(',')
    } catch {}
  }
  loadPreferences()

  async function loadIngest() {
    try {
      const current = await getIngestSettings()
      ingest.fs_watch_dir = current?.fs_watch_dir || ''
      ingest.fs_watch_owner_email = current?.fs_watch_owner_email || ''
      seedSourceOwner()
    } catch {}
  }
  loadIngest()

  const idx = $derived(STEPS.findIndex(s => s.name === cur))
  const doneCount = $derived(Object.values(steps).filter(v => v === 'done' || v === 'skipped').length)

  async function mark(status) {
    err = ''
    try {
      await setupStep(cur, status)
      steps = { ...steps, [cur]: status }
      if (idx < STEPS.length - 1) cur = STEPS[idx + 1].name
    } catch (ex) { err = ex.message || 'Could not record the step.' }
  }

  async function saveAnd(fn, label) {
    err = ''; busy = true
    try {
      const result = await fn()
      notify?.(typeof label === 'function' ? label(result) : label)
      await mark('done')
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
    err = ''; llmTesting = true; llmTestResult = null
    try {
      const result = await testLLMSettings(llmPayload(true))
      llmTestResult = result?.result || null
      notify?.(result?.message || 'Classifier connection passed')
    } catch (ex) {
      err = ex.message || 'The classifier did not return a valid response.'
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
    <div class="side-head" style="padding-left:0">Setup · {doneCount}/{STEPS.length}</div>
    {#each STEPS as s, i}
      <button class="wiz-step" class:on={cur === s.name} onclick={() => (cur = s.name)}>
        <span class="dot" class:ok={steps[s.name] === 'done'}
              class:warn={steps[s.name] === 'skipped'}
              class:accent={cur === s.name && !steps[s.name]}></span>
        <span class="grow">{s.label}</span>
        {#if steps[s.name] === 'skipped'}<span class="sub">skipped</span>{/if}
      </button>
    {/each}
    <button class="btn primary" style="margin-top:14px;justify-content:center" onclick={finish} disabled={busy}>
      Finish setup
    </button>
    <p class="sub" style="font-size:.72rem;color:var(--faint);margin-top:8px">
      Every step is optional. Nothing is blocked while this is open, and every step can be revisited later.
    </p>
  </aside>

  <div class="card wiz-body">
    {#if err}<div class="err">{err}</div>{/if}

    {#if cur === 'welcome'}
      <h3>What are you organizing?</h3>
      <p class="wiz-p">Pick the closest fit. Suchi will recommend a ready-made filing tree, and every option remains editable.</p>
      <div class="intent-grid">
        {#each INTENTS as option (option.id)}
          <button class="intent-choice" class:on={intent === option.id} onclick={() => chooseIntent(option)}>
            <b>{option.label}</b><span class="sub">{option.description}</span>
          </button>
        {/each}
      </div>
      {#if recommendedPreset}
        <div class="recommendation">
          <span class="pill ok">Recommended</span>
          <b>{recommendedPreset.name}</b>
          <span class="sub">{recommendedPreset.description}</span>
        </div>
      {:else if intent === 'custom'}
        <div class="recommendation"><b>Compare every filing tree</b><span class="sub">The next step will show the complete catalog.</span></div>
      {/if}
      <div class="toolbar">
        <button class="btn primary sm" disabled={busy || !intent}
                onclick={() => saveAnd(saveIntent, 'Setup direction saved')}>Choose filing tree</button>
      </div>

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
          {#each KNOWN_CAPS as c (c.slug)}
            <label style="display:flex;gap:8px;align-items:center;font-weight:normal;margin-top:4px">
              <input type="checkbox" checked={user.capabilities.includes(c.slug)}
                     onchange={() => toggleCap(c.slug)} />
              {c.label}
            </label>
          {/each}
        </div>
      {/if}
      <div class="toolbar">
        <button class="btn primary sm" disabled={busy || !user.email || !user.password}
                onclick={() => saveAnd(createSetupUser, 'User created')}>Create user</button>
        <button class="btn sm" onclick={() => mark('skipped')}>Just me for now</button>
      </div>

    {:else if cur === 'jd'}
      <h3>Pick a filing tree</h3>
      <p class="wiz-p">Johnny.Decimal areas and categories, tailored to how you'll use the archive. You can always switch later — refile moves every document to the closest match in the new tree.</p>
      {#if ENABLE_SETUP_TAXONOMY_IMPORT}
        <span class="seg" style="margin-bottom:14px">
          <button class:on={jdTab === 'presets'} onclick={() => (jdTab = 'presets')}>Presets</button>
          <button class:on={jdTab === 'import'} onclick={() => (jdTab = 'import')}>Import a file</button>
        </span>
      {/if}
      {#if ENABLE_SETUP_TAXONOMY_IMPORT && jdTab === 'import'}
        <TaxonomyImport {notify} onApplied={() => mark('done')} />
        <div class="toolbar" style="margin-top:10px">
          <button class="btn sm" onclick={() => mark('skipped')}>Keep the current tree</button>
        </div>
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
        Install the preset's starter filing rules and automations (recommended). Turn off if you want to start from scratch — you can still add them by re-picking the preset later.</label>
      <label class="wiz-check"><input type="checkbox" bind:checked={preset.refile} />
        Refile existing documents into the new tree now.</label>
      <div class="toolbar">
        <button class="btn primary sm" disabled={busy || (preset.preset_id === 'blank' && !preset.confirm_blank)}
                onclick={() => saveAnd(() => applyPreset(preset), 'Filing tree applied')}>Apply preset</button>
        <button class="btn sm" onclick={() => mark('skipped')}>Keep the current tree</button>
      </div>
      {/if}

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
        <button class="btn sm" onclick={() => mark('skipped')}>Uploads only</button>
      </div>

    {:else if cur === 'mail'}
      <h3>Email intake</h3>
      <p class="wiz-p">Point suchi at one or more mailboxes and forwarded documents file themselves. Credentials stay server-side; the password field never reads back.</p>
      <EmailAccounts {notify} users={mailUsers} />
      <div class="toolbar" style="margin-top:12px">
        <button class="btn primary sm" onclick={() => mark('done')}>Continue</button>
        <button class="btn sm" onclick={() => mark('skipped')}>Skip for now</button>
      </div>

    {:else if cur === 'llm'}
      <h3>Classification model</h3>
      <p class="wiz-p">The rules engine works with no model at all. Add any OpenAI-compatible endpoint — a local Ollama keeps everything on your hardware — and low-confidence documents get a second opinion.</p>
      <div class="toolbar" style="margin:0 0 12px">
        {#if llmStatus?.active}
          <span class="pill ok">Classifier active</span>
        {:else if llmStatus?.enabled}
          <span class="pill warn">Classifier inactive</span>
        {:else}
          <span class="pill">Rules only</span>
        {/if}
        {#if llmStatus?.has_api_key}<span class="chip">API key stored</span>{/if}
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
          Clear the stored API key when saving.</label>
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
                onclick={() => saveAnd(() => saveClassifier(false), 'Classifier disabled; rules remain active')}>Use rules only</button>
      </div>
      {#if llmTestResult}
        <div class="test-result">
          <b>Validated in {llmTestResult.elapsed_ms} ms</b>
          <span>{llmTestResult.title || 'No title'} · confidence {Number(llmTestResult.confidence).toFixed(2)}</span>
          {#if llmTestResult.tags?.length}<span class="sub">Tags: {llmTestResult.tags.join(', ')}</span>{/if}
        </div>
      {/if}
      <p class="wiz-p sub" style="font-size:.8rem;margin-top:14px">The classifier runs automatically on new documents. To classify older documents, select them in <a href="#/documents">Documents</a> and use Rescan.</p>

    {:else if cur === 'rules'}
      <h3>Rules</h3>
      <p class="wiz-p">Your preset already ships a starter set of filing rules and automations — the ones under the "Owned by <em>&lt;preset&gt;</em> filing tree" pill. Editing or disabling any of them forks a user-owned copy; re-picking the same preset never touches your edits.</p>
      <p class="wiz-p">Full editor for both engines lives in <a href="#/automations">Automations</a>; nothing more to decide here during setup.</p>
      <div class="toolbar">
        <a role="button" class="btn primary sm" href="#/automations">Open automations</a>
        <button class="btn sm" onclick={() => mark('done')}>Done</button>
        <button class="btn sm" onclick={() => mark('skipped')}>Skip</button>
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
        <button class="btn sm" onclick={() => mark('skipped')}>Defaults are fine</button>
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
  .wiz-step .dot.ok { background: var(--ok); }
  .wiz-step .dot.warn { background: var(--warn); }
  .wiz-step .dot.accent { background: var(--accent); }
  .wiz-step .grow { flex: 1; }
  .wiz-step .sub { font-size: .68rem; color: var(--faint); }
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
  .recommendation {
    display: grid; grid-template-columns: auto 1fr; align-items: center; gap: 5px 9px;
    border-left: 3px solid var(--ok); padding: 8px 10px; margin-bottom: 14px;
  }
  .recommendation .sub { grid-column: 2; color: var(--muted); font-size: .8rem; }
  .range { width: 100%; accent-color: var(--accent); }
  .test-result {
    display: flex; flex-direction: column; gap: 3px; border-left: 3px solid var(--ok);
    padding: 7px 10px; margin-top: 12px; font-size: .82rem;
  }
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
