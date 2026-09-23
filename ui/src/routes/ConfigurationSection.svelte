<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { scopedHash as filingHref, systems } from '../lib/systems.svelte.js'
  import { untrack } from 'svelte'
  import { setupState, adminListUsers, applyPreset,
           getLLMSettings, saveLLMSettings, testLLMSettings,
           saveResearchContextMode, saveClassificationAutoApply,
           getPreferences, savePreferences, getIngestSettings, saveIngestSettings,
           listPresets } from '../lib/api.js'
  import { isLocalEndpoint } from '../lib/net.js'
  import EmailAccounts from '../lib/EmailAccounts.svelte'
  import TaxonomyImport from '../lib/TaxonomyImport.svelte'

  let { section = 'archive', notify, onTaxonomyChanged } = $props()
  // Server is the source of truth (GET /api/presets/); this list is
  // only the offline fallback so the section never renders empty.
  const FALLBACK_PRESETS = [
    { id: 'solo', name: 'Solo', description: 'One person: life admin, money, health, home.', areas: [] },
    { id: 'household', name: 'Household', description: 'A family: shared areas plus per-person categories.', areas: [] },
    { id: 'freelance', name: 'Freelance', description: 'Clients, invoicing, taxes, contracts.', areas: [] },
    { id: 'smb_billing', name: 'Small business', description: 'AP/AR heavy: vendors, invoices, compliance.', areas: [] },
    { id: 'blank', name: 'Blank', description: 'No tree. Build your own from scratch.', blank: true, areas: [] },
  ]
  const RESEARCH_CONTEXT_MODES = [
    { id: 'focused', label: 'Focused', bound: '1 matching passage · up to 1,600 characters', description: 'Less text for smaller local models and faster answers.' },
    { id: 'balanced', label: 'Balanced', bound: '2 matching passages · up to 3,200 characters', description: 'Recommended for most archives and models.' },
    { id: 'detailed', label: 'Detailed', bound: '3 matching passages · up to 4,800 characters', description: 'Checks more places in long documents; may be slower and send more text.' },
  ]
  let presets = $state(FALLBACK_PRESETS)
  let loaded = $state(new Set())
  let loading = $state(new Set())
  let loadErrors = $state({})
  const LOAD_ERROR_MESSAGES = {
    setup: 'Could not load the filing-tree settings.',
    users: 'Could not load archive users.',
    ingest: 'Could not load watched-folder settings.',
    llm: 'Could not load classification settings.',
    preferences: 'Could not load OCR and backup settings.',
  }

  function loadOnce(key, fn) {
    if (loaded.has(key) || loading.has(key)) return
    loading = new Set([...loading, key])
    loadErrors = { ...loadErrors, [key]: '' }
    Promise.resolve()
      .then(fn)
      .then(() => { loaded = new Set([...loaded, key]) })
      .catch((ex) => {
        loadErrors = { ...loadErrors, [key]: ex.message || LOAD_ERROR_MESSAGES[key] || 'Could not load this configuration.' }
      })
      .finally(() => {
        const next = new Set(loading)
        next.delete(key)
        loading = next
      })
  }

  let busy = $state(false)
  let err = $state('')

  // Section form state.
  let mailUsers = $state([])
  let preset = $state({ preset_id: 'solo', confirm_blank: false, refile: false, include_seeds: true })
  let filingTreeChosen = $state(false)
  let importOpen = $state(false)
  let importBusy = $state(false)
  let llm = $state({
    enabled: false, endpoint_url: '', model: '', api_key: '', clear_api_key: false,
    egress_ack: false, confidence_threshold: 0.7,
    archive_enabled: true, archive_review_threshold: 0.5, archive_auto_threshold: 0.9,
  })
  let researchContextMode = $state('balanced')
  let autoApply = $state(false)
  let llmStatus = $state(null)
  let llmTesting = $state(false)
  let llmMode = $state('local')
  let llmTestResult = $state(null)
  let llmTestError = $state('')
  let prefs = $state({ backup_interval_hours: 24, ocr_languages: 'eng' })
  let ingest = $state({ fs_watch_dir: '', fs_watch_owner_email: '', fs_watch_system: '' })

  async function loadPresets() {
    try {
      const result = await listPresets()
      const rows = result?.results || result || []
      if (rows.length) presets = rows
    } catch {}
  }

  async function loadSetup() {
    const state = await setupState()
    filingTreeChosen = !!state?.filing_tree_chosen
    const selected = state?.current_preset
    if (selected) preset.preset_id = selected
  }

  async function loadUsers() {
    const result = await adminListUsers()
    mailUsers = result?.results || result || []
    seedSourceOwner()
  }

  function seedSourceOwner() {
    if (!ingest.fs_watch_owner_email) {
      ingest.fs_watch_owner_email = mailUsers.find(u => !u.disabled)?.email || ''
    }
  }

  async function saveArchive() {
    const result = await applyPreset(preset)
    filingTreeChosen = true
    await onTaxonomyChanged?.()
    return result
  }

  async function importedTree() {
    await Promise.all([loadSetup(), onTaxonomyChanged?.()])
  }

  async function loadLLM() {
    const st = await getLLMSettings()
    llmStatus = st
    llm.enabled = !!st?.enabled
    llm.endpoint_url = st?.endpoint_url || 'http://host.suchi.local:11434/v1'
    llm.model = st?.model || 'qwen2.5:7b'
    llm.egress_ack = !!st?.egress_ack
    llm.confidence_threshold = st?.confidence_threshold ?? 0.7
    llm.archive_enabled = st?.archive_enabled ?? true
    llm.archive_review_threshold = st?.archive_review_threshold ?? 0.5
    llm.archive_auto_threshold = st?.archive_auto_threshold ?? 0.9
    autoApply = st?.auto_apply ?? true
    researchContextMode = RESEARCH_CONTEXT_MODES.some(mode => mode.id === st?.research_context_mode)
      ? st.research_context_mode : 'balanced'
    llm.api_key = ''
    llm.clear_api_key = false
    llmMode = st?.endpoint_url && !isLocalEndpoint(st.endpoint_url) ? 'hosted' : 'local'
  }
  async function loadPreferences() {
    const current = await getPreferences()
    prefs.backup_interval_hours = current?.backup_interval_hours ?? 24
    prefs.ocr_languages = (current?.ocr_languages || ['eng']).join(',')
  }
  async function loadIngest() {
    const current = await getIngestSettings()
    ingest.fs_watch_dir = current?.fs_watch_dir || ''
    ingest.fs_watch_owner_email = current?.fs_watch_owner_email || ''
    ingest.fs_watch_system = current?.fs_watch_system || ''
    seedSourceOwner()
  }
  const loadFunctions = {
    setup: loadSetup,
    users: loadUsers,
    ingest: loadIngest,
    llm: loadLLM,
    preferences: loadPreferences,
  }
  const requiredLoadKeys = $derived(({
    archive: ['setup'],
    sources: ['users', 'ingest'],
    mail: ['users'],
    llm: ['llm'],
    preferences: ['preferences'],
  })[section] || [])
  const activeLoadFailure = $derived.by(() => {
    const key = requiredLoadKeys.find((candidate) => loadErrors[candidate])
    return key ? { key, message: loadErrors[key] } : null
  })
  const activeLoading = $derived(requiredLoadKeys.some((key) => loading.has(key) || !loaded.has(key)))

  function retryLoad(key) {
    const next = new Set(loaded)
    next.delete(key)
    loaded = next
    loadOnce(key, loadFunctions[key])
  }

  $effect(() => {
    const selected = section
    const keys = requiredLoadKeys
    untrack(() => {
      for (const key of keys) loadOnce(key, loadFunctions[key])
      if (selected === 'archive') loadOnce('presets', loadPresets)
    })
  })

  async function saveAnd(fn, label) {
    err = ''; busy = true
    try {
      const result = await fn()
      notify?.(typeof label === 'function' ? label(result) : label)
    }
    catch (ex) { err = ex.message || 'The server rejected that.' }
    finally { busy = false }
  }


  // POST and test reject unknown fields; keep PATCH-only settings out here.
  function llmPayload(enabled) {
    return {
      enabled,
      endpoint_url: llm.endpoint_url,
      model: llm.model,
      api_key: llm.api_key || '',
      clear_api_key: !!llm.clear_api_key,
      egress_ack: !!llm.egress_ack,
      confidence_threshold: Number(llm.confidence_threshold),
      archive_enabled: llmStatus?.archive_enabled ?? true,
      archive_review_threshold: llmStatus?.archive_review_threshold ?? 0.5,
      archive_auto_threshold: llmStatus?.archive_auto_threshold ?? 0.9,
    }
  }

  function matchingPayload() {
    return {
      enabled: !!llmStatus?.enabled,
      endpoint_url: llmStatus?.endpoint_url || '',
      model: llmStatus?.model || '',
      api_key: '',
      clear_api_key: false,
      egress_ack: !!llmStatus?.egress_ack,
      confidence_threshold: llmStatus?.confidence_threshold ?? 0.7,
      archive_enabled: llm.archive_enabled,
      archive_review_threshold: Number(llm.archive_review_threshold),
      archive_auto_threshold: Number(llm.archive_auto_threshold),
    }
  }

  function invalidateLLMTest() {
    llmTestResult = null
    llmTestError = ''
  }

  function setLLMMode(mode) {
    llmMode = mode
    invalidateLLMTest()
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
    invalidateLLMTest()
  }

  async function saveClassifier(enabled) {
    const result = await saveLLMSettings(llmPayload(enabled))
    // Refresh saved model state without discarding independent, unsaved options.
    const st = await getLLMSettings()
    llmStatus = st
    llm.enabled = !!st.enabled
    llm.endpoint_url = st.endpoint_url
    llm.model = st.model
    llm.egress_ack = !!st.egress_ack
    llm.confidence_threshold = st.confidence_threshold ?? 0.7
    llm.api_key = ''
    llm.clear_api_key = false
    invalidateLLMTest()
    return result
  }

  async function saveMatching() {
    const payload = matchingPayload()
    const result = await saveLLMSettings(payload)
    llmStatus = {
      ...llmStatus,
      archive_enabled: payload.archive_enabled,
      archive_review_threshold: payload.archive_review_threshold,
      archive_auto_threshold: payload.archive_auto_threshold,
    }
    return result
  }

  async function saveResearchContext() {
    const result = await saveResearchContextMode(researchContextMode)
    llmStatus = { ...llmStatus, research_context_mode: result.research_context_mode }
    return result
  }

  async function saveApplicationMode() {
    const result = await saveClassificationAutoApply(autoApply)
    llmStatus = { ...llmStatus, auto_apply: result.auto_apply }
    return result
  }

  function applicationModeLabel(enabled) {
    return enabled ? 'High-confidence suggestions apply automatically' : 'New inferred changes require review'
  }

  async function testClassifier() {
    err = ''; llmTesting = true; llmTestResult = null; llmTestError = ''
    try {
      const result = await testLLMSettings(llmPayload(true))
      llmTestResult = result?.result || null
      notify?.(result?.message || 'Model connection and response format checked')
    } catch (ex) {
      llmTestError = ex.message || 'The classifier did not return a valid response.'
    } finally { llmTesting = false }
  }

  const llmIsRemote = $derived(!!llm.endpoint_url && !isLocalEndpoint(llm.endpoint_url))
</script>

<div class="configuration-section">
  {#if activeLoadFailure}
    <div class="configuration-load-error">
      <div class="err">{activeLoadFailure.message}</div>
      <div class="toolbar">
        <button class="btn sm" onclick={() => retryLoad(activeLoadFailure.key)}>Retry</button>
      </div>
    </div>
  {:else if activeLoading}
    <div class="configuration-loading" aria-label="Loading configuration">
      <div class="skel" style="width:32%;height:16px"></div>
      <div class="skel" style="width:78%"></div>
      <div class="skel" style="width:62%"></div>
    </div>
  {:else}
    {#if err}<div class="err">{err}</div>{/if}

    {#if section === 'archive'}
      <h3>Filing tree</h3>
      <p class="wiz-p">Add a filing tree to this archive. Later imports merge without overwriting existing categories or local rule choices.</p>
      {#if !filingTreeChosen}
        <div class="setup-requirement" role="note">
          <b>Required for archive setup.</b>
          <span>Apply a preset, import a filing tree, or explicitly confirm Blank to complete setup. Users, groups, and other settings can be managed later.</span>
        </div>
      {/if}
        <div class="toolbar">
          <button class="btn sm" disabled={busy || importBusy} onclick={() => { importOpen = !importOpen }}>{importOpen ? 'Back to presets' : 'Import a file'}</button>
        </div>
        {#if importOpen}
          <TaxonomyImport {notify} bind:busy={importBusy} onApplied={importedTree} />
        {:else}
        <fieldset disabled={busy} style="border:0;padding:0;margin:0;min-width:0">
        <div class="preset-grid">
          {#each presets as p (p.id)}
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
          Install the preset's starter automations. Turn this off if you want to start from scratch.</label>
        <label class="wiz-check"><input type="checkbox" bind:checked={preset.refile} />
          Refile existing documents into the new tree now.</label>
        <div class="toolbar">
          <button class="btn primary sm" disabled={busy || (preset.preset_id === 'blank' && !preset.confirm_blank)}
                  onclick={() => saveAnd(saveArchive, 'Filing tree updated')}>Apply filing tree</button>
        </div>
        </fieldset>
        {/if}
      <p class="migration-note">
        Moving an existing archive? Large export bundles are safer through the CLI.
        <a href="https://docs.suchi.page/guides/importer" target="_blank" rel="noopener">Read the migration guide</a>.
      </p>

    {:else if section === 'sources'}
      <h3>Watched folder</h3>
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
      {#if systems.introduced}
        <div class="field"><label for="i-system">Watched folder filing system</label>
          <select id="i-system" class="input" bind:value={ingest.fs_watch_system}>
            <option value="">Original archive (default)</option>
            {#each systems.results as system (system.code)}<option value={system.code}>{system.code} · {system.name}</option>{/each}
          </select>
          <p class="sub">This is the server-wide watched-folder destination, not the system currently displayed. Its owner must be able to enter it.</p>
        </div>
      {/if}
      <div class="toolbar">
        <button class="btn primary sm" disabled={busy || !ingest.fs_watch_dir || !ingest.fs_watch_owner_email}
                onclick={() => saveAnd(() => saveIngestSettings(ingest), 'Watched folder saved')}>Save folder</button>
      </div>

    {:else if section === 'mail'}
      <h3>Email intake</h3>
      <p class="wiz-p">Point suchi at one or more mailboxes and forwarded documents file themselves. Credentials stay server-side; the password field never reads back.</p>
      <EmailAccounts {notify} users={mailUsers} />

    {:else if section === 'llm'}
      <h3>Filing suggestions, dates, and research</h3>
      <p class="wiz-p">Local matching can suggest filing details from documents already in your archive, without a model or sending text elsewhere. An optional model proposes metadata and dates, and powers <b>Archive research</b> for authorized users.</p>
      <div class="side-head" style="padding-left:0;margin-top:20px">Optional model</div>
      <div class="toolbar" style="margin:0 0 12px">
        {#if llmStatus?.active}
          <span class="pill ok">Model active</span>
        {:else if llmStatus?.enabled}
          <span class="pill warn">Model inactive</span>
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
        <input id="l-url" class="input mono" placeholder="http://host.suchi.local:11434/v1" bind:value={llm.endpoint_url} oninput={invalidateLLMTest} /></div>
      <div class="field"><label for="l-model">Model</label>
        <input id="l-model" class="input mono" placeholder="qwen2.5:7b" bind:value={llm.model} oninput={invalidateLLMTest} /></div>
      <div class="field"><label for="l-key">API key (blank for local)</label>
        <input id="l-key" class="input mono" type="password" bind:value={llm.api_key} autocomplete="off"
               oninput={invalidateLLMTest}
               disabled={llm.clear_api_key}
               placeholder={llmStatus?.has_api_key ? 'stored key — leave blank to keep' : ''} /></div>
      {#if llmStatus?.has_api_key}
        <label class="wiz-check"><input type="checkbox" checked={llm.clear_api_key} onchange={setClearAPIKey} />
          Clear the saved API key when saving. Config-file and environment keys are unchanged.</label>
      {/if}
      {#if llmIsRemote}
        <label class="wiz-check attn"><input type="checkbox" bind:checked={llm.egress_ack} onchange={invalidateLLMTest} />
          This endpoint is not local. I acknowledge document text will leave this machine.</label>
      {/if}

      <div class="toolbar connection-actions">
        <button class="btn primary sm" disabled={busy || llmTesting || !llm.endpoint_url || !llm.model || (llmIsRemote && !llm.egress_ack)}
                onclick={testClassifier}>Test connection</button>
        <button class="btn sm" disabled={busy || llmTesting}
                onclick={() => saveAnd(() => saveClassifier(false), 'Model disabled; local matching settings unchanged')}>Disable model</button>
      </div>
      {#if llmTestError}
        <div class="test-result failed">
          <b>Connection failed</b>
          <span>{llmTestError}</span>
        </div>
      {:else if llmTestResult}
        <div class="test-result">
          <b>Connection responded in {llmTestResult.elapsed_ms} ms</b>
          <span>{llmTestResult.title || 'No title'} · self-reported score {Number(llmTestResult.confidence).toFixed(2)}</span>
          {#if llmTestResult.tags?.length}<span class="sub">Tags: {llmTestResult.tags.join(', ')}</span>{/if}
        </div>
      {/if}
      <p class="wiz-p sub" style="font-size:.8rem;margin-top:12px">Test connection sends a sample, not your documents. A valid response checks connectivity and response format, not accuracy or permission to apply suggestions. Testing does not enable the model.</p>

      <section class="research-context" aria-labelledby="research-context-title">
        <div class="research-context-heading">
          <div>
            <span class="option-kind">Archive answer evidence</span>
            <h4 id="research-context-title">Archive research configuration</h4>
          </div>
          <span class="context-live">Applies to the next question</span>
        </div>
        <p>Choose how many relevant sections Archive research can include from each document. When room remains, Suchi may also include a separate document ending. More text can improve answers from long documents, but may take longer and send more to your model. It still considers only documents the person asking can access, with at most six documents per answer.</p>
        <div class="context-presets" role="radiogroup" aria-label="Research context">
          {#each RESEARCH_CONTEXT_MODES as mode, index (mode.id)}
            <label class="context-preset" class:on={researchContextMode === mode.id}>
              <input type="radio" name="research-context-mode" bind:group={researchContextMode} value={mode.id} />
              <span class="context-copy">
                <span class="context-label">
                  <b>{mode.label}</b>
                  {#if mode.id === 'balanced'}<span class="recommended">Recommended</span>{/if}
                </span>
                <span>{mode.description}</span>
                <small>{mode.bound}</small>
              </span>
              <span class="context-depth" aria-hidden="true">
                {#each Array(index + 1) as _}<i></i>{/each}
              </span>
            </label>
          {/each}
        </div>
        <div class="toolbar option-save">
          <button class="btn primary sm" disabled={busy || llmTesting}
                  onclick={() => saveAnd(saveResearchContext, 'Research context saved')}>Save research context</button>
        </div>
      </section>

      <section class="model-options" aria-labelledby="model-options-title">
        <h4 id="model-options-title">Classification options</h4>
        <label class="wiz-check application-mode"><input type="checkbox" bind:checked={autoApply} disabled={busy} aria-describedby="application-mode-help" /> Automatically apply high-confidence suggestions</label>
        <p id="application-mode-help" class="options-intro"><b>On:</b> Add dates to Calendar and apply suggested metadata when they meet your thresholds.<br /><b>Off:</b> Review inferred changes in Approvals before they apply.</p>
        <div class="toolbar option-save">
          <button class="btn primary sm" disabled={busy || llmTesting}
                  onclick={() => saveAnd(saveApplicationMode, result => applicationModeLabel(result.auto_apply))}>Save application mode</button>
        </div>
        <p class="options-intro">Local matching and model activation are separate. This choice does not enable a model or authorize external text sharing. Explicit filing rules still run independently.</p>

        <div class="option-group independent">
          <span class="option-kind">Works without a model</span>
          <h5>Similar-document matching</h5>
          <p>Uses locally indexed documents already filed in this archive to suggest metadata. Saving these options does not enable a model or authorize external text sharing.</p>
          <label class="wiz-check"><input type="checkbox" bind:checked={llm.archive_enabled} /> Offer filing suggestions from similar documents</label>
          {#if llm.archive_enabled}
            <div class="field">
              <label for="archive-review">Minimum similarity score for review suggestions · {Number(llm.archive_review_threshold).toFixed(2)}</label>
              <input id="archive-review" class="range" type="range" min="0.5" max="0.9" step="0.05"
                     bind:value={llm.archive_review_threshold} />
            </div>
              <div class="field">
                <label for="archive-auto">Minimum similarity score to apply automatically · {Number(llm.archive_auto_threshold).toFixed(2)}</label>
                <input id="archive-auto" class="range" type="range" min="0.55" max="0.95" step="0.05"
                       bind:value={llm.archive_auto_threshold} disabled={!autoApply} aria-describedby="archive-score-help" />
              </div>
              <p id="archive-score-help">Must be above the review floor. Lower-scoring eligible matches remain in Approvals.</p>
          {/if}
          {#if Number(llm.archive_review_threshold) >= Number(llm.archive_auto_threshold)}
            <p role="alert">The review floor must be below the automatic threshold. Lower the review floor or turn automatic mode on to adjust its threshold.</p>
          {/if}
          <div class="toolbar option-save">
            <button class="btn primary sm" disabled={busy || llmTesting || Number(llm.archive_review_threshold) >= Number(llm.archive_auto_threshold)}
                    onclick={() => saveAnd(saveMatching, 'Similar-document matching saved')}>Save matching options</button>
          </div>
        </div>

        <div class="option-group model-driven">
          <span class="option-kind">Uses the configured model</span>
          <h5>Model suggestions</h5>
          <p>Suggests titles, correspondents, filing details, tags, languages, and dates. Test the connection before enabling the model.</p>
            <div class="field">
              <label for="l-confidence">Minimum model score to apply automatically · {Number(llm.confidence_threshold).toFixed(2)}</label>
              <input id="l-confidence" class="range" type="range" min="0.5" max="0.95" step="0.05"
                     bind:value={llm.confidence_threshold} disabled={!autoApply} aria-describedby="model-score-help" />
            </div>
          <p id="model-score-help">Confidence is the model's self-reported score, not measured accuracy. Source and evidence checks still apply. Eligible suggestions that cannot apply automatically stay in Approvals.</p>
          <h5>Calendar dates</h5>
          <p>Automatic mode adds eligible high-confidence dates to Calendar. In review-first mode, accept each new date in Approvals first. Existing decisions are preserved; saving settings or restarting Suchi does not accept pending dates.</p>
        </div>

        <div class="toolbar option-save">
          <button class="btn primary sm" disabled={busy || llmTesting || !llmTestResult || !llm.endpoint_url || !llm.model || (llmIsRemote && !llm.egress_ack)}
                  onclick={() => saveAnd(
                    () => saveClassifier(true),
                    () => `Model enabled; ${applicationModeLabel(llmStatus?.auto_apply ?? true)}`
                  )}>Enable model and save options</button>
        </div>
      </section>
      <p class="wiz-p sub" style="font-size:.8rem;margin-top:14px">The enabled model proposes metadata and dates for new documents and answers authorized research questions on demand. To request suggestions for older documents, select them in <a href={filingHref("#/documents")}>Documents</a> and use Rescan or Extract dates. Existing review decisions are preserved.</p>

    {:else if section === 'automations'}
      <h3>Automations</h3>
      <p class="wiz-p">Your preset can install starter filing automations. Preset-owned automations show their filing-tree owner; editing one forks a user-owned copy, so re-picking the preset never overwrites your edits.</p>
      <p class="wiz-p">Automations file documents by title, content, sender, tags, and other metadata.</p>
      <div class="toolbar">
        <a role="button" class="btn primary sm" href={filingHref("#/automations")}>Open automations</a>
      </div>

    {:else if section === 'preferences'}
      <h3>OCR and backups</h3>
      <div class="field"><label for="p-ocr">OCR languages (comma-separated tesseract codes)</label>
        <input id="p-ocr" class="input mono" placeholder="eng,hin,nep" bind:value={prefs.ocr_languages} /></div>
      <div class="field"><label for="p-bk">Database snapshot interval (hours, 0 disables)</label>
        <input id="p-bk" class="input" type="number" min="0" bind:value={prefs.backup_interval_hours} /></div>
      <p class="wiz-p sub" style="font-size:.8rem">Snapshots include only the database. A complete backup also needs document files, the credential key, and configuration.</p>
      <div class="toolbar">
        <button class="btn primary sm" disabled={busy}
                onclick={() => saveAnd(() => savePreferences({
                  backup_interval_hours: Number(prefs.backup_interval_hours) || 0,
                  ocr_languages: prefs.ocr_languages.split(',').map(x => x.trim()).filter(Boolean),
                }), 'Preferences saved')}>Save preferences</button>
      </div>
    {/if}
  {/if}
  </div>

<style>
  .wiz-p { color: var(--muted); font-size: .92rem; margin: 6px 0 16px; max-width: 46em; }
  .wiz-check { display: flex; gap: 9px; align-items: baseline; font-size: .86rem; color: var(--muted); margin: 0 0 14px; }
  .wiz-check.attn { color: var(--warn); }
  .setup-requirement {
    display: grid; gap: 2px; margin: 0 0 14px; padding: 10px 12px;
    border-left: 3px solid var(--warn); border-radius: var(--r-sm);
    background: var(--warn-soft); font-size: .82rem; line-height: 1.45;
  }
  .setup-requirement b { color: var(--warn); }
  .setup-requirement span { color: var(--muted); }
  .preset-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; margin-bottom: 14px; }
  .migration-note { margin: 18px 0 0; color: var(--faint); font-size: .76rem; }
  .range { width: 100%; accent-color: var(--accent); }
  .test-result {
    display: flex; flex-direction: column; gap: 3px; border-left: 3px solid var(--ok);
    padding: 7px 10px; margin-top: 12px; font-size: .82rem;
  }
  .test-result.failed { border-left-color: var(--danger); }
  .connection-actions { margin-top: 16px; }
  .research-context { margin-top: 24px; padding: 18px; border: 1px solid var(--line-strong); border-radius: var(--r); background: var(--surface-2); }
  .research-context-heading { display: flex; align-items: end; justify-content: space-between; gap: 16px; }
  .research-context h4 { margin: 0; font-size: 1rem; }
  .research-context > p { max-width: 56em; margin: 7px 0 14px; color: var(--muted); font-size: .8rem; line-height: 1.5; }
  .context-live { color: var(--muted); font-size: .76rem; white-space: nowrap; }
  .context-presets { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 8px; }
  .context-preset {
    position: relative; display: grid; grid-template-columns: auto 1fr; gap: 8px; min-width: 0;
    padding: 11px; border: 1px solid var(--line); border-radius: var(--r-sm); background: var(--surface); cursor: pointer;
  }
  .context-preset:hover { border-color: var(--line-strong); }
  .context-preset.on { border-color: var(--accent); box-shadow: inset 0 0 0 1px var(--accent); }
  .context-preset input { margin: 3px 0 0; accent-color: var(--accent); }
  .context-copy { display: flex; flex-direction: column; gap: 3px; color: var(--muted); font-size: .76rem; line-height: 1.4; }
  .context-label { display: flex; flex-wrap: wrap; align-items: center; gap: 5px; color: var(--ink); font-size: .82rem; }
  .context-copy small { margin-top: 3px; color: var(--muted); font-size: .75rem; }
  .recommended { padding: 2px 5px; border-radius: 999px; background: var(--tint); color: var(--accent); font-size: .65rem; font-weight: 700; text-transform: uppercase; letter-spacing: .04em; }
  .context-depth { position: absolute; right: 9px; top: 10px; display: flex; align-items: end; gap: 2px; height: 12px; opacity: .38; }
  .context-depth i { display: block; width: 3px; height: 5px; border-radius: 2px; background: var(--accent); }
  .context-depth i:nth-child(2) { height: 8px; }
  .context-depth i:nth-child(3) { height: 11px; }
  .model-options { margin-top: 24px; padding: 18px; border: 1px solid var(--line-strong); border-radius: var(--r); background: var(--surface-2); }
  .model-options h4 { margin: 0; font-size: 1rem; }
  .options-intro { max-width: 54em; margin: 6px 0 0; color: var(--muted); font-size: .8rem; line-height: 1.5; }
  .application-mode { margin-top: 14px; align-items: start; }
  .application-mode input { flex-shrink: 0; }
  .model-options .option-save .btn { white-space: normal; text-align: left; }
  .option-group { margin-top: 14px; padding: 14px; border: 1px solid var(--line); border-left-width: 3px; border-radius: var(--r-sm); background: var(--surface); }
  .option-group.independent { border-left-color: var(--ok); }
  .option-group.model-driven { border-left-color: var(--accent); }
  .option-kind { display: inline-flex; margin-bottom: 5px; padding: 3px 7px; border-radius: 999px; background: var(--surface-2); color: var(--muted); font-size: .67rem; font-weight: 650; }
  .option-group h5 { margin: 0; font-size: .88rem; }
  .option-group > p { margin: 4px 0 12px; color: var(--muted); font-size: .76rem; line-height: 1.45; }
  .option-group .field:last-child, .option-group .wiz-check:last-child { margin-bottom: 0; }
  .option-save { margin-top: 14px; }
  .configuration-loading { display:grid;gap:12px;padding:8px 0; }
  .configuration-load-error .toolbar { margin-top:10px; }
  @media (max-width: 640px) { .preset-grid { grid-template-columns: 1fr; } }
  @media (max-width: 640px) { .model-options { padding: 13px; } }
  @media (max-width: 760px) {
    .research-context { padding: 13px; }
    .research-context-heading { align-items: start; flex-direction: column; gap: 4px; }
    .context-presets { grid-template-columns: 1fr; }
  }
  .preset {
    display: flex; flex-direction: column; gap: 3px; cursor: pointer;
    border: 1px solid var(--line-strong); border-radius: var(--r-sm); padding: 12px 14px;
  }
  .preset .sub { font-size: .78rem; color: var(--muted); }
  .preset-tree { display: flex; flex-wrap: wrap; gap: 5px; margin-top: 7px; }
  .preset.on { border-color: var(--accent); background: var(--tint); }
</style>
