<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { scopedHash as filingHref, systems } from '../lib/systems.svelte.js'
  import { untrack, onDestroy } from 'svelte'
  import { adminListUsers,
           getLLMSettings, saveLLMSettings, testLLMSettings, subscriptionLoginAction, getSubscriptionModels, saveSubscriptionModel,
           saveResearchContextMode, saveClassificationAutoApply, saveArchiveMatchingSettings,
           getPreferences, savePreferences, getIngestSettings, saveIngestSettings } from '../lib/api.js'
  import { isLocalEndpoint } from '../lib/net.js'
  import EmailAccounts from '../lib/EmailAccounts.svelte'

  let { section, notify } = $props()
  const RESEARCH_CONTEXT_MODES = [
    { id: 'focused', label: 'Focused', bound: '1 matching passage · up to 1,600 characters', description: 'Less text for smaller local models and faster answers.' },
    { id: 'balanced', label: 'Balanced', bound: '2 matching passages · up to 3,200 characters', description: 'Recommended for most archives and models.' },
    { id: 'detailed', label: 'Detailed', bound: '3 matching passages · up to 4,800 characters', description: 'Checks more places in long documents; may be slower and send more text.' },
  ]
  let loaded = $state(new Set())
  let loading = $state(new Set())
  let loadErrors = $state({})
  const LOAD_ERROR_MESSAGES = {
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
  let modelDialogOpen = $state(false)
  let thresholdsOpen = $state(false)
  let researchOpen = $state(false)
  let llm = $state({
    enabled: false, endpoint_url: '', model: '', api_key: '', clear_api_key: false,
    egress_ack: false, confidence_threshold: 0.7,
    archive_enabled: true, archive_review_threshold: 0.5, archive_auto_threshold: 0.9,
  })
  let researchContextMode = $state('balanced')
  let autoApply = $state(false)
  let llmStatus = $state(null)
  let llmTesting = $state(false)
  let subscriptionProvider = $state('openai_chatgpt')
  let llmMode = $state('local')
  let modeDrafts = {}
  let loginCode = $state(null)
  let loginBusy = $state(false)
  let loginError = $state('')
  let subscriptionModels = $state([])
  let modelsLoading = $state(false)
  let modelsError = $state('')
  let modelSaving = $state(false)
  let modelSaveError = $state('')
  let modelsGeneration = 0
  let loginTimer
  let disposed = false
  let loginGeneration = 0
  onDestroy(() => { disposed = true; loginGeneration++; modelsGeneration++; clearTimeout(loginTimer) })

  async function loadSubscriptionModels() {
    const generation = ++modelsGeneration
    modelsLoading = true; modelsError = ''
    invalidateLLMTest()
    try {
      const result = await getSubscriptionModels(subscriptionProvider)
      if (disposed || generation !== modelsGeneration || llmMode !== 'subscription' || !llmStatus?.subscription_connected) return
      subscriptionModels = result.models || []
      if (!subscriptionModels.some(model => model.id === llm.model)) {
        const saved = llmStatus?.subscription_model
        llm.model = subscriptionModels.some(model => model.id === saved) ? saved : subscriptionModels[0]?.id || ''
      }
    } catch (ex) {
      if (!disposed && generation === modelsGeneration) { modelsError = ex.message || 'Could not load ChatGPT models.'; subscriptionModels = [] }
    } finally { if (!disposed && generation === modelsGeneration) modelsLoading = false }
  }

  async function persistSubscriptionModel() {
    const model = llm.model
    if (!model || modelSaving) return
    invalidateLLMTest()
    modelSaving = true; modelSaveError = ''
    try {
      const result = await saveSubscriptionModel(subscriptionProvider, model)
      if (disposed) return
      llmStatus = { ...llmStatus, subscription_model: result.subscription_model }
      notify?.('Model selection saved. Test and enable it to use this model.')
    } catch (ex) {
      if (!disposed) modelSaveError = ex.message || 'Could not save model selection.'
    } finally { if (!disposed) modelSaving = false }
  }

  async function startSubscriptionLogin() {
    clearTimeout(loginTimer)
    const generation = ++loginGeneration
    loginBusy = true; loginError = ''; loginCode = null
    try {
      const code = await subscriptionLoginAction(subscriptionProvider, 'start')
      if (disposed || generation !== loginGeneration) return
      loginCode = code
      loginTimer = setTimeout(() => pollSubscriptionLogin(generation), code.interval * 1000)
    } catch (ex) { if (!disposed && generation === loginGeneration) loginError = ex.message }
    finally { if (!disposed && generation === loginGeneration) loginBusy = false }
  }
  async function pollSubscriptionLogin(generation) {
    if (disposed || generation !== loginGeneration) return
    try {
      const result = await subscriptionLoginAction(subscriptionProvider, 'poll')
      if (disposed || generation !== loginGeneration) return
      if (result.pending) {
        loginTimer = setTimeout(() => pollSubscriptionLogin(generation), (loginCode?.interval || 5) * 1000)
      } else {
        loginCode = null
        llmStatus = { ...llmStatus, subscription_connected: true }
        invalidateLLMTest()
        if (llmMode === 'subscription') void loadSubscriptionModels()
        notify?.('ChatGPT connected. Test the model before enabling it.')
      }
    } catch (ex) { if (!disposed && generation === loginGeneration) { loginError = ex.message; loginCode = null } }
  }
  async function endSubscriptionLogin(action) {
    loginGeneration++; clearTimeout(loginTimer); loginCode = null
    loginBusy = true; loginError = ''
    try {
      await subscriptionLoginAction(subscriptionProvider, action)
      if (disposed) return
      if (action === 'disconnect') {
        modelsGeneration++; modelsLoading = false; subscriptionModels = []; modelsError = ''; llm.model = ''
        llmStatus = { ...llmStatus, subscription_connected: false, active: llmStatus?.mode === 'subscription' ? false : llmStatus?.active }
        invalidateLLMTest()
      }
    } catch (ex) { if (!disposed) loginError = ex.message }
    finally { if (!disposed) loginBusy = false }
  }
  let llmTestResult = $state(null)
  let llmTestError = $state('')
  let llmTestGeneration = 0
  let prefs = $state({ backup_interval_hours: 24, ocr_languages: 'eng' })
  let ingest = $state({ fs_watch_dir: '', fs_watch_owner_email: '', fs_watch_system: '' })


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


  async function loadLLM() {
    const st = await getLLMSettings()
    llmStatus = st
    modeDrafts = {}
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
    subscriptionProvider = st?.subscription_provider || 'openai_chatgpt'
    llmMode = st?.mode === 'subscription' || (!st?.endpoint_url && st?.subscription_connected) ? 'subscription' : st?.endpoint_url && !isLocalEndpoint(st.endpoint_url) ? 'hosted' : 'local'
    if (llmMode === 'subscription') {
      llm.endpoint_url = ''
      llm.model = st?.subscription_model || st?.model || ''
      if (st?.subscription_connected) void loadSubscriptionModels()
    }
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
    users: loadUsers,
    ingest: loadIngest,
    llm: loadLLM,
    preferences: loadPreferences,
  }
  const requiredLoadKeys = $derived(({
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
    const keys = requiredLoadKeys
    untrack(() => {
      for (const key of keys) loadOnce(key, loadFunctions[key])
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
      subscription_provider: llmMode === 'subscription' ? subscriptionProvider : '',
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
      archive_enabled: llm.archive_enabled,
      archive_review_threshold: Number(llm.archive_review_threshold),
      archive_auto_threshold: Number(llm.archive_auto_threshold),
    }
  }

  function invalidateLLMTest() {
    llmTestGeneration++
    llmTestResult = null
    llmTestError = ''
  }

  function setLLMMode(mode) {
    if (llmMode === mode) return
    if (loginCode) void endSubscriptionLogin('cancel')
    modelsGeneration++; modelsLoading = false; subscriptionModels = []; modelsError = ''
    modeDrafts[llmMode] = { endpoint_url: llm.endpoint_url, model: llm.model, egress_ack: llm.egress_ack }
    llmMode = mode
    modelSaveError = ''
    llm.api_key = ''
    llm.clear_api_key = false
    invalidateLLMTest()
    const draft = modeDrafts[mode] || (mode === 'local'
      ? { endpoint_url: 'http://host.suchi.local:11434/v1', model: 'qwen2.5:7b', egress_ack: false }
      : { endpoint_url: '', model: mode === 'subscription' ? llmStatus?.subscription_model || (llmStatus?.mode === 'subscription' ? llmStatus.model : '') : '', egress_ack: false })
    llm.endpoint_url = draft.endpoint_url
    llm.model = draft.model
    llm.egress_ack = draft.egress_ack
    if (mode === 'subscription' && llmStatus?.subscription_connected) void loadSubscriptionModels()
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
    const result = await saveArchiveMatchingSettings(payload)
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


  async function testClassifier() {
    err = ''; llmTesting = true; llmTestResult = null; llmTestError = ''
    const generation = ++llmTestGeneration
    try {
      const result = await testLLMSettings(llmPayload(true))
      if (disposed || generation !== llmTestGeneration) return
      llmTestResult = result?.result || null
      notify?.(result?.message || 'Model connection and response format checked')
    } catch (ex) {
      if (!disposed && generation === llmTestGeneration) llmTestError = ex.message || 'The classifier did not return a valid response.'
    } finally { llmTesting = false }
  }

  const llmIsRemote = $derived(llmMode === 'subscription' || (!!llm.endpoint_url && !isLocalEndpoint(llm.endpoint_url)))
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

    {#if section === 'sources'}
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
      <div class="cls-card">
        <div class="cls-head"><h3>Suggestions</h3><p>How suggestions are made and applied.</p></div>
        <div class="cls-row">
          <div class="apply-mode" role="radiogroup" aria-label="Suggestion application" aria-describedby="application-mode-help">
            <label class="apply-opt"><input type="radio" name="application-mode" value="review" checked={!autoApply} disabled={busy} onchange={() => (autoApply = false)} />
              <span><b>Review first</b><small>Every suggestion waits in Approvals.</small></span></label>
            <label class="apply-opt"><input type="radio" name="application-mode" value="auto" checked={autoApply} disabled={busy} onchange={() => (autoApply = true)} />
              <span><b>Apply when confident</b><small>High-confidence suggestions file themselves; the rest wait in Approvals.</small></span></label>
          </div>
          <p class="cls-note" id="application-mode-help">Dates follow the same choice, into Calendar or Approvals.</p>
          <button class="btn sm" disabled={busy} onclick={() => saveAnd(saveApplicationMode, 'Application mode saved')}>Save application mode</button>
        </div>
        <div class="cls-row">
          <button type="button" class="switch" role="switch" aria-checked={llm.archive_enabled}
                  aria-label="Offer filing suggestions from similar documents"
                  onclick={() => (llm.archive_enabled = !llm.archive_enabled)}></button>
          <div class="cls-main"><b>Similar documents</b><small>Learns from what is already filed. Nothing leaves this machine.</small></div>
          <div class="cls-right"><span class="mono-sum">review ≥ {Math.round(Number(llm.archive_review_threshold) * 100)}% · auto ≥ {Math.round(Number(llm.archive_auto_threshold) * 100)}%</span>
            <button class="act-link" onclick={() => (thresholdsOpen = !thresholdsOpen)}>{thresholdsOpen ? 'Hide' : 'Adjust'}</button></div>
        </div>
        {#if thresholdsOpen}
          <div class="cls-disc">
            <div class="field"><label for="archive-review">Minimum confidence to suggest for review · {Math.round(Number(llm.archive_review_threshold) * 100)}%</label>
              <input id="archive-review" class="range" type="range" min="0.5" max="0.9" step="0.05" bind:value={llm.archive_review_threshold} /></div>
            <div class="field"><label for="archive-auto">Minimum confidence to apply automatically · {Math.round(Number(llm.archive_auto_threshold) * 100)}%</label>
              <input id="archive-auto" class="range" type="range" min="0.55" max="0.95" step="0.05" bind:value={llm.archive_auto_threshold} disabled={!autoApply} /></div>
            <p class="cls-note">Automatic must stay above the review floor.</p>
            {#if Number(llm.archive_review_threshold) >= Number(llm.archive_auto_threshold)}<p role="alert">The review floor must be below the automatic threshold.</p>{/if}
          </div>
        {/if}
        <div class="cls-row">
          <div class="cls-main">
            <b>Document model{#if llmStatus?.active}<span class="mdot"></span><em>Active</em>{/if}</b>
            {#if llmStatus?.active}<small><span class="mono">{llm.model}</span> · {llmMode === 'subscription' ? 'account subscription' : llmMode === 'hosted' ? 'hosted endpoint' : 'local model'}</small>
            {:else}<small>Titles, dates, tags and Archive research. Off.</small>{/if}
          </div>
          <div class="cls-right">{#if llmStatus?.active}<button class="act-link" onclick={() => (modelDialogOpen = true)}>Manage</button>
            {:else}<button class="btn sm" onclick={() => (modelDialogOpen = true)}>Set up model</button>{/if}</div>
        </div>
        <div class="cls-foot"><span class="cls-note">New documents only; use Rescan in Documents for older ones.</span>
          <button class="btn primary sm" disabled={busy || Number(llm.archive_review_threshold) >= Number(llm.archive_auto_threshold)}
                  onclick={() => saveAnd(saveMatching, 'Similar-document matching saved')}>Save matching options</button></div>
      </div>
      <div class="cls-card" role="region" aria-label="Archive research configuration">
        <div class="cls-head"><h3 id="research-context-title">Archive research</h3><p>Answers questions from your documents.</p><span class="cls-when">Applies to the next question</span></div>
        <div class="cls-row"><div class="cls-main"><b>Reading per document</b>
            {#if llmStatus?.active}<small style="text-transform:capitalize">{researchContextMode}</small>
            {:else}<small>Needs the document model.</small>{/if}</div>
          <div class="cls-right">{#if llmStatus?.active}<button class="act-link" onclick={() => (researchOpen = !researchOpen)}>{researchOpen ? 'Hide' : 'Change'}</button>
            {:else}<button class="act-link" onclick={() => (researchOpen = !researchOpen)}>{researchOpen ? 'Hide' : 'Change'}</button>{/if}</div></div>
        {#if researchOpen}<div class="cls-disc"><div class="context-presets" role="radiogroup" aria-label="Research context">
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
      </div>{/if}
      </div>
      {#if modelDialogOpen}
        <div class="mdl-dim" aria-hidden="true"></div>
        <div class="mdl" role="dialog" aria-label="Set up document model">
          <div class="mdl-head"><b>Set up document model</b><button class="act-link" style="color:var(--muted)" onclick={() => (modelDialogOpen = false)}>Cancel</button></div>
          <div class="mdl-body">
      <span class="seg" style="margin-bottom:14px">
        <button disabled={modelSaving || loginBusy} class:on={llmMode === 'local'} onclick={() => setLLMMode('local')}>Local model</button>
        <button disabled={modelSaving || loginBusy} class:on={llmMode === 'hosted'} onclick={() => setLLMMode('hosted')}>Hosted endpoint</button>
        <button disabled={modelSaving || loginBusy} class:on={llmMode === 'subscription'} onclick={() => setLLMMode('subscription')}>Account subscription</button>
      </span>
      {#if llmMode === 'subscription'}
        <div class="field"><label for="l-provider">Provider</label>
          <select id="l-provider" class="input" bind:value={subscriptionProvider} disabled={loginBusy || modelSaving}><option value="openai_chatgpt">OpenAI ChatGPT (Codex)</option></select>
        </div>
        <p class="wiz-p sub">Connect your ChatGPT Codex subscription for classification and archive research. This account powers AI for the whole archive. Suchi sends extracted text to OpenAI; subscription limits apply.</p>
        <p role="status">{llmStatus?.subscription_connected ? 'ChatGPT connected' : 'ChatGPT not connected'}</p>
        <div class="toolbar">
          <button class="btn sm" disabled={loginBusy || !!loginCode} onclick={startSubscriptionLogin}>{llmStatus?.subscription_connected ? 'Reconnect ChatGPT' : 'Connect ChatGPT'}</button>
          {#if llmStatus?.subscription_connected}<button class="btn sm" disabled={loginBusy} onclick={() => endSubscriptionLogin('disconnect')}>Disconnect ChatGPT</button>{/if}
        </div>
        {#if loginCode}
          <p>Open <a href={loginCode.verification_url} target="_blank" rel="noopener noreferrer">ChatGPT device login</a> and enter <strong class="mono">{loginCode.user_code}</strong>. Waiting for sign-in…</p>
          <button class="btn sm" onclick={() => endSubscriptionLogin('cancel')}>Cancel login</button>
        {/if}
        {#if loginError}<p class="err" role="alert">{loginError}</p>{/if}
      {:else if llmMode === 'local'}
        <p class="wiz-p sub" style="font-size:.8rem">The Docker Compose default reaches Ollama on the host. Edit the URL for a native install or another machine on your network.</p>
      {:else}
        <p class="wiz-p sub" style="font-size:.8rem">Use the OpenAI-compatible base URL from your provider. Suchi sends extracted text, never the original file.</p>
      {/if}
      {#if llmMode !== 'subscription'}
      <div class="field"><label for="l-url">Endpoint URL</label>
        <input id="l-url" class="input mono" placeholder="http://host.suchi.local:11434/v1" bind:value={llm.endpoint_url} oninput={invalidateLLMTest} /></div>
      {/if}
      <div class="field"><label for="l-model">Model</label>
        {#if llmMode === 'subscription'}
          <select id="l-model" class="input" bind:value={llm.model} onchange={persistSubscriptionModel} disabled={!llmStatus?.subscription_connected || modelsLoading || modelSaving || !subscriptionModels.length}>
            {#if !subscriptionModels.length}<option value="">{modelsLoading ? 'Loading models…' : llmStatus?.subscription_connected ? 'No models available' : 'Connect ChatGPT to load models'}</option>{/if}
            {#each subscriptionModels as model (model.id)}<option value={model.id}>{model.name} ({model.id})</option>{/each}
          </select>
        {:else}
          <input id="l-model" class="input mono" placeholder="qwen2.5:7b" bind:value={llm.model} oninput={invalidateLLMTest} />
        {/if}
      </div>
      {#if llmMode === 'subscription'}
        {#if modelsError}<p class="err" role="alert">{modelsError}</p>{/if}
        {#if llmStatus?.subscription_connected}<button class="btn sm" disabled={modelsLoading || modelSaving} onclick={loadSubscriptionModels}>Refresh models</button>{/if}
        <p class="sub">Selection saves automatically. Test and enable the model to use it for classification and research.</p>
        {#if modelSaving}<p class="sub" role="status">Saving model selection…</p>{/if}
        {#if modelSaveError}<p class="err" role="alert">{modelSaveError}</p><button class="btn sm" onclick={persistSubscriptionModel}>Retry saving model</button>{/if}
        <p class="sub">Models come from your connected ChatGPT account. Test connection verifies access to the selected model.</p>
      {:else}
      <div class="field"><label for="l-key">API key (blank for local)</label>
        <input id="l-key" class="input mono" type="password" bind:value={llm.api_key} autocomplete="off"
               oninput={invalidateLLMTest}
               disabled={llm.clear_api_key}
               placeholder={llmStatus?.has_api_key ? 'stored key — leave blank to keep' : ''} /></div>
      {#if llmStatus?.has_api_key}
        <label class="wiz-check"><input type="checkbox" checked={llm.clear_api_key} onchange={setClearAPIKey} />
          Clear the saved API key when saving. Config-file and environment keys are unchanged.</label>
      {/if}
      {/if}
      {#if llmIsRemote}
        <label class="wiz-check attn"><input type="checkbox" bind:checked={llm.egress_ack} onchange={invalidateLLMTest} />
          This endpoint is not local. I acknowledge document text will leave this machine.</label>
      {/if}

      {#if llmMode === 'subscription' && !llm.egress_ack}
        <p class="sub">Check the acknowledgement above to test the model. The test sends only a synthetic sample; enabling the model allows document text to leave this machine.</p>
      {/if}
      <div class="field"><label for="l-confidence">Minimum model confidence to apply automatically · {Math.round(Number(llm.confidence_threshold) * 100)}%</label>
        <input id="l-confidence" class="range" type="range" min="0.5" max="0.95" step="0.05" bind:value={llm.confidence_threshold} disabled={!autoApply} oninput={invalidateLLMTest} /></div>
      <div class="toolbar connection-actions">
        <button class="btn primary sm" disabled={busy || llmTesting || modelSaving || !!modelSaveError || (llmMode !== 'subscription' && !llm.endpoint_url) || !llm.model || (llmMode === 'subscription' && (!llmStatus?.subscription_connected || modelsLoading || !subscriptionModels.some(model => model.id === llm.model))) || (llmIsRemote && !llm.egress_ack)}
                onclick={testClassifier}>Test connection</button>
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

          </div>
          <div class="mdl-foot"><span class="cls-note">Enabling starts suggestions for new documents.</span>
            {#if llmStatus?.enabled}<button class="act-link" style="color:var(--muted)" disabled={busy || llmTesting}
                onclick={() => saveAnd(() => saveClassifier(false), 'Model disabled; local matching settings unchanged').then(() => (modelDialogOpen = false))}>Disable model</button>{/if}
            <button class="btn primary sm" disabled={busy || llmTesting || modelSaving || !!modelSaveError || !llmTestResult || (llmMode !== 'subscription' && !llm.endpoint_url) || !llm.model || (llmMode === 'subscription' && (!llmStatus?.subscription_connected || modelsLoading || !subscriptionModels.some(model => model.id === llm.model))) || (llmIsRemote && !llm.egress_ack)}
                onclick={() => saveAnd(() => saveClassifier(true), 'Model enabled').then(() => (modelDialogOpen = false))}>Enable model</button></div>
        </div>
      {/if}

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
  .apply-mode { margin: 0 0 14px; display: flex; flex-direction: column; gap: 10px }
  .apply-opt { display: flex; gap: 10px; align-items: flex-start; cursor: pointer }
  .apply-opt input { margin-top: 3px }
  .apply-opt b { display: block; font-size: .88rem; font-weight: 600; color: var(--ink) }
  .apply-opt small { display: block; font-size: .78rem; color: var(--muted); margin-top: 2px }
  .wiz-check { display: flex; gap: 9px; align-items: baseline; font-size: .86rem; color: var(--muted); margin: 0 0 14px; }
  .wiz-check.attn { color: var(--warn); }
  .range { width: 100%; accent-color: var(--accent); }
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
  .option-save { margin-top: 14px; }
  .configuration-loading { display:grid;gap:12px;padding:8px 0; }
  .configuration-load-error .toolbar { margin-top:10px; }
  @media (max-width: 760px) {
    .context-presets { grid-template-columns: 1fr; }
  }
  .cls-card { background: var(--surface); border: 1px solid var(--line); border-radius: var(--r); margin-top: 16px; overflow: visible }
  .cls-head { display: flex; align-items: baseline; gap: 10px; padding: 14px 20px; border-bottom: 1px solid var(--line) }
  .cls-head h3 { margin: 0; font-size: .95rem } .cls-head p { margin: 0; color: var(--muted); font-size: .8rem }
  .cls-when { margin-left: auto; color: var(--faint); font-size: .76rem }
  .cls-row { display: flex; align-items: center; gap: 14px; padding: 13px 20px; border-top: 1px solid var(--line) }
  .cls-row:nth-child(2) { border-top: 0 }
  .cls-main { min-width: 0 } .cls-main b { display: flex; align-items: center; gap: 8px; font-size: .9rem }
  .cls-main small { display: block; color: var(--muted); font-size: .78rem; margin-top: 2px }
  .cls-main em { font-style: normal; font-size: .76rem; font-weight: 600; color: var(--ok) }
  .mdot { width: 8px; height: 8px; border-radius: 4px; background: var(--ok) }
  .cls-right { margin-left: auto; display: flex; align-items: center; gap: 12px; flex: none }
  .mono-sum { font-family: 'Spline Sans Mono', ui-monospace, Menlo, monospace; font-size: .76rem; color: var(--faint) }
  .cls-disc { margin: 0 20px 14px 20px; border: 1px solid var(--line); border-radius: var(--r-sm); background: var(--surface-2); padding: 14px 16px }
  .cls-note { color: var(--muted); font-size: .76rem; margin: 6px 0 0 }
  .cls-foot { display: flex; align-items: center; gap: 14px; padding: 12px 20px; border-top: 1px solid var(--line) }
  .cls-foot .btn { margin-left: auto }
  .act-link { background: none; border: 0; padding: 0; color: var(--accent); font: inherit; font-size: .8rem; font-weight: 600; cursor: pointer }
  .act-link:hover { text-decoration: underline }
  .mdl-dim { position: fixed; inset: 0; background: rgba(23,24,26,.4); z-index: 60 }
  .mdl { position: fixed; left: 50%; top: 7vh; transform: translateX(-50%); width: min(620px, 94vw); background: var(--surface); border-radius: var(--r); box-shadow: 0 30px 70px rgba(20,22,28,.3); z-index: 61; max-height: 86vh; overflow: auto }
  .mdl-head { display: flex; justify-content: space-between; align-items: center; padding: 16px 22px; border-bottom: 1px solid var(--line) } .mdl-head b { font-size: 1rem }
  .mdl-body { padding: 16px 22px } .mdl-body .seg { margin-bottom: 14px }
  .mdl-foot { display: flex; align-items: center; gap: 14px; padding: 14px 22px; border-top: 1px solid var(--line) }
  .mdl-foot .btn.primary { margin-left: auto }
</style>
