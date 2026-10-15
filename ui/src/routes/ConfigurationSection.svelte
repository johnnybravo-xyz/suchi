<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "../lib/i18n.svelte.js";
  import { tick, onDestroy } from 'svelte'
  import {
    getLLMSettings, saveLLMSettings, testLLMSettings, subscriptionLoginAction,
    getSubscriptionModels, saveSubscriptionModel, saveResearchContextMode,
  } from '../lib/api.js'
  import { isLocalEndpoint } from '../lib/net.js'

  let { notify } = $props()
  const RESEARCH_CONTEXT_MODES = [
    { id: 'focused', label: translate("configurationSection.focused"), bound: translate("configurationSection.1MatchingSectionUpTo1600Characters"), description: translate("configurationSection.lessTextForSmallerLocalModelsAndFaster") },
    { id: 'balanced', label: translate("configurationSection.balanced"), bound: translate("configurationSection.2MatchingSectionsUpTo3200Characters"), description: translate("configurationSection.recommendedForMostArchivesAndModels") },
    { id: 'detailed', label: translate("configurationSection.detailed"), bound: translate("configurationSection.3MatchingSectionsUpTo4800Characters"), description: translate("configurationSection.checksMorePlacesInLongDocumentsMayBe") },
  ]
  let loaded = $state(new Set())
  let loading = $state(new Set())
  let loadErrors = $state({})
  const LOAD_ERROR_MESSAGES = {
    llm: translate("configurationSection.couldNotLoadClassificationSettings"),
  }

  function loadOnce(key, fn) {
    if (loaded.has(key) || loading.has(key)) return
    loading = new Set([...loading, key])
    loadErrors = { ...loadErrors, [key]: '' }
    Promise.resolve()
      .then(fn)
      .then(() => { loaded = new Set([...loaded, key]) })
      .catch((ex) => {
        loadErrors = { ...loadErrors, [key]: ex.message || LOAD_ERROR_MESSAGES[key] || translate("configurationSection.couldNotLoadThisConfiguration") }
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
  let modelDialogOpen = $state(false)
  let researchOpen = $state(false)
  let researchOptions = $state()
  let llm = $state({
    enabled: false, endpoint_url: '', model: '', api_key: '', clear_api_key: false,
    egress_ack: false, confidence_threshold: 0.7,
  })
  let researchContextMode = $state('balanced')
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

  async function toggleResearchOptions() {
    if (researchOpen) {
      researchOpen = false
      return
    }
    researchOpen = true
    await tick()
    researchOptions?.scrollIntoView({
      behavior: window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth',
      block: 'start',
    })
    researchOptions?.focus({ preventScroll: true })
  }

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
      if (!disposed && generation === modelsGeneration) { modelsError = ex.message || translate("configurationSection.couldNotLoadChatgptModels"); subscriptionModels = [] }
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
      notify?.(translate("configurationSection.modelSelectionSavedTestAndEnableItTo"))
    } catch (ex) {
      if (!disposed) modelSaveError = ex.message || translate("configurationSection.couldNotSaveModelSelection")
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
        notify?.(translate("configurationSection.chatgptConnectedTestTheModelBeforeEnablingIt"))
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


  async function loadLLM() {
    const st = await getLLMSettings()
    llmStatus = st
    modeDrafts = {}
    llm.enabled = !!st?.enabled
    llm.endpoint_url = st?.endpoint_url || 'http://host.suchi.local:11434/v1'
    llm.model = st?.model || 'qwen2.5:7b'
    llm.egress_ack = !!st?.egress_ack
    llm.confidence_threshold = st?.confidence_threshold ?? 0.7
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
  const loadFunctions = { llm: loadLLM }
  const requiredLoadKeys = ['llm']
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

  for (const key of requiredLoadKeys) loadOnce(key, loadFunctions[key])

  async function saveAnd(fn, label) {
    err = ''; busy = true
    try {
      const result = await fn()
      notify?.(typeof label === 'function' ? label(result) : label)
    }
    catch (ex) { err = ex.message || translate("configurationSection.theServerRejectedThat") }
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


  async function saveResearchContext() {
    const result = await saveResearchContextMode(researchContextMode)
    llmStatus = { ...llmStatus, research_context_mode: result.research_context_mode }
    return result
  }



  async function testClassifier() {
    err = ''; llmTesting = true; llmTestResult = null; llmTestError = ''
    const generation = ++llmTestGeneration
    try {
      const result = await testLLMSettings(llmPayload(true))
      if (disposed || generation !== llmTestGeneration) return
      llmTestResult = result?.result || null
      notify?.(result?.message || translate("configurationSection.modelConnectionAndResponseFormatChecked"))
    } catch (ex) {
      if (!disposed && generation === llmTestGeneration) llmTestError = ex.message || translate("configurationSection.theDocumentModelDidNotReturnAValid")
    } finally { llmTesting = false }
  }

  function subscriptionTestHint() {
    if (!llmStatus?.subscription_connected) {
      return translate("configurationSection.connectAndAcknowledgeFirstSendsOneSampleSentence")
    }
    if (modelsLoading) return translate("configurationSection.loadingModelsFromTheConnectedAccount")
    if (modelsError) return translate("configurationSection.refreshTheModelListBeforeTesting")
    if (!subscriptionModels.some((model) => model.id === llm.model)) {
      return translate("configurationSection.chooseAnAvailableModelBeforeTesting")
    }
    if (!llm.egress_ack) {
      return translate("configurationSection.acknowledgeDocumentEgressFirstTheTestSendsOne")
    }
    return translate("configurationSection.sendsOneSampleSentenceNeverYourDocuments")
  }

  function elapsedLabel(milliseconds) {
    const value = Number(milliseconds) || 0
    if (value < 1000) return translate("configurationSection.valueMs", { "value": Math.round(value) })
    return translate("configurationSection.valueS", { "$": (value / 1000).toFixed(1).replace(/\.0$/, '') })
  }

  const llmIsRemote = $derived(llmMode === 'subscription' || (!!llm.endpoint_url && !isLocalEndpoint(llm.endpoint_url)))
</script>

<div class="configuration-section">
  {#if activeLoadFailure}
    <div class="configuration-load-error">
      <div class="err">{activeLoadFailure.message}</div>
      <div class="toolbar">
        <button class="btn sm" onclick={() => retryLoad(activeLoadFailure.key)}>{translate("common.retry")}</button>
      </div>
    </div>
  {:else if activeLoading}
    <div class="configuration-loading" aria-label={translate("configurationSection.loadingConfiguration")}>
      <div class="skel" style="width:32%;height:16px"></div>
      <div class="skel" style="width:78%"></div>
      <div class="skel" style="width:62%"></div>
    </div>
  {:else}
    {#if err}<div class="err">{err}</div>{/if}

    <div class="cls-card">
      <div class="cls-head"><h3>{translate("configurationSection.suggestions")}</h3><p>{translate("configurationSection.howSuggestionsAreMadeAndApplied")}</p></div>
      <div class="cls-row">
        <div class="cls-main">
          <b>{translate("configurationSection.documentModel")}{#if llmStatus?.active}<span class="mdot"></span><em>{translate("configurationSection.active")}</em>{/if}</b>
          {#if llmStatus?.active}<small><span class="mono">{llm.model}</span> · {llmMode === 'subscription' ? translate("configurationSection.accountSubscription2") : llmMode === 'hosted' ? translate("configurationSection.hostedModel2") : translate("configurationSection.localModel2")}</small>
          {:else}<small>{translate("configurationSection.documentSuggestionsAndQuestionsOff")}</small>{/if}
        </div>
        <div class="cls-right">{#if llmStatus?.active}<button class="act-link" onclick={() => (modelDialogOpen = true)}>{translate("configurationSection.manage")}</button>
          {:else}<button class="btn sm" onclick={() => (modelDialogOpen = true)}>{translate("configurationSection.setUpModel")}</button>{/if}</div>
      </div>
    </div>
      <div class="cls-card" role="region" aria-label={translate("configurationSection.askYourDocumentsSettings")}>
        <div class="cls-head"><h3 id="research-context-title">{translate("configurationSection.askYourDocuments")}</h3><p>{translate("configurationSection.answersQuestionsFromYourDocuments")}</p><span class="cls-when">{translate("configurationSection.appliesToTheNextQuestion")}</span></div>
        <div class="cls-row"><div class="cls-main"><b>{translate("configurationSection.readingPerDocument")}</b>
            {#if llmStatus?.active}<small style="text-transform:capitalize">{researchContextMode}</small>
            {:else}<small>{translate("configurationSection.needsTheDocumentModel")}</small>{/if}</div>
          <div class="cls-right"><button class="act-link" onclick={toggleResearchOptions}>{researchOpen ? translate("configurationSection.hide") : translate("configurationSection.change")}</button></div></div>
        {#if researchOpen}<div class="cls-disc" bind:this={researchOptions} tabindex="-1" aria-labelledby="research-context-title"><div class="context-presets" role="radiogroup" aria-label={translate("configurationSection.howMuchToRead")}>
          {#each RESEARCH_CONTEXT_MODES as mode, index (mode.id)}
            <label class="context-preset" class:on={researchContextMode === mode.id}>
              <input type="radio" name="research-context-mode" bind:group={researchContextMode} value={mode.id} />
              <span class="context-copy">
                <span class="context-label">
                  <b>{mode.label}</b>
                  {#if mode.id === 'balanced'}<span class="recommended">{translate("configurationSection.recommended")}</span>{/if}
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
                  onclick={() => saveAnd(saveResearchContext, translate("configurationSection.readingAmountSaved"))}>{translate("configurationSection.saveReadingAmount")}</button>
        </div>
      </div>{/if}
      </div>
      {#if modelDialogOpen}
        <div class="mdl-dim" aria-hidden="true"></div>
        <div class="mdl" role="dialog" aria-label={translate("configurationSection.setUpDocumentModel")}>
          <div class="mdl-head"><b>{translate("configurationSection.setUpDocumentModel")}</b><button class="act-link" style="color:var(--muted)" onclick={() => (modelDialogOpen = false)}>{translate("common.cancel")}</button></div>
          <div class="mdl-body">
      <span class="seg" style="margin-bottom:14px">
        <button disabled={modelSaving || loginBusy} class:on={llmMode === 'local'} onclick={() => setLLMMode('local')}>{translate("configurationSection.localModel")}</button>
        <button disabled={modelSaving || loginBusy} class:on={llmMode === 'hosted'} onclick={() => setLLMMode('hosted')}>{translate("configurationSection.hostedModel")}</button>
        <button disabled={modelSaving || loginBusy} class:on={llmMode === 'subscription'} onclick={() => setLLMMode('subscription')}>{translate("configurationSection.accountSubscription")}</button>
      </span>
      {#if llmMode === 'subscription'}
        <div class="subscription-maturity"><span class="experimental-tag">{translate("configurationSection.experimental")}</span></div>
        <div class="field subscription-provider"><label for="l-provider">{translate("configurationSection.provider")}</label>
          <select id="l-provider" class="input" bind:value={subscriptionProvider} disabled={loginBusy || modelSaving}><option value="openai_chatgpt">{translate("configurationSection.openaiChatgptCodex")}</option></select>
        </div>
        <div class="subscription-account" class:connected={llmStatus?.subscription_connected}>
          <div class="subscription-account-copy">
            <span class="subscription-state-title">
              {#if llmStatus?.subscription_connected}<span class="subscription-state-dot" aria-hidden="true"></span>{/if}
              <strong role="status">{llmStatus?.subscription_connected ? translate("configurationSection.chatgptConnected") : translate("configurationSection.chatgpt")}</strong>
            </span>
            <span>{llmStatus?.subscription_connected
              ? translate("configurationSection.subscriptionLimitsApplyToThisArchive")
              : translate("configurationSection.powersDocumentSuggestionsAndQuestionsForThisArchive")}</span>
          </div>
          <div class="subscription-account-actions">
            {#if llmStatus?.subscription_connected}
              <button class="act-link subscription-disconnect" disabled={loginBusy} onclick={() => endSubscriptionLogin('disconnect')}>{translate("configurationSection.disconnectChatgpt")}</button>
            {:else}
              <button class="btn sm" disabled={loginBusy || !!loginCode} onclick={startSubscriptionLogin}>
                {loginBusy ? translate("configurationSection.connecting") : translate("configurationSection.connectChatgpt")}
              </button>
            {/if}
          </div>
          {#if loginCode}
            <div class="subscription-login">
              <p>{translate("configurationSection.open")} <a href={loginCode.verification_url} target="_blank" rel="noopener noreferrer">{translate("configurationSection.chatgptDeviceLogin")}</a> {translate("configurationSection.andEnter")} <strong class="mono">{loginCode.user_code}</strong>{translate("configurationSection.waitingForSignIn")}</p>
              <button class="btn sm" onclick={() => endSubscriptionLogin('cancel')}>{translate("configurationSection.cancelLogin")}</button>
            </div>
          {/if}
          {#if loginError}<p class="err subscription-account-error" role="alert">{loginError}</p>{/if}
        </div>
      {:else if llmMode === 'local'}
        <p class="wiz-p sub" style="font-size:.8rem">{translate("configurationSection.theDockerComposeDefaultReachesOllamaOn")}</p>
      {:else}
        <p class="wiz-p sub" style="font-size:.8rem">{translate("configurationSection.useTheOpenaiCompatibleBaseUrlFrom")}</p>
      {/if}
      {#if llmMode !== 'subscription'}
      <div class="field"><label for="l-url">{translate("configurationSection.modelServiceUrl")}</label>
        <input id="l-url" class="input mono" placeholder={translate("configurationSection.httpHostSuchiLocal11434V1")} bind:value={llm.endpoint_url} oninput={invalidateLLMTest} /></div>
      {/if}
      <div class="field model-field">
        <div class="field-label-row">
          <label for="l-model">{translate("configurationSection.model")}</label>
          {#if llmMode === 'subscription' && llmStatus?.subscription_connected}
            <button class="act-link refresh-models" disabled={modelsLoading || modelSaving} onclick={loadSubscriptionModels}>{translate("configurationSection.refreshModels")}</button>
          {/if}
        </div>
        {#if llmMode === 'subscription'}
          <select id="l-model" class="input" bind:value={llm.model} onchange={persistSubscriptionModel} disabled={!llmStatus?.subscription_connected || modelsLoading || modelSaving || !subscriptionModels.length}>
            {#if !subscriptionModels.length}<option value="">{modelsLoading ? translate("configurationSection.loadingModels") : llmStatus?.subscription_connected ? translate("configurationSection.noModelsAvailable") : translate("configurationSection.connectToChooseAModel")}</option>{/if}
            {#each subscriptionModels as model (model.id)}<option value={model.id}>{model.name} ({model.id})</option>{/each}
          </select>
        {:else}
          <input id="l-model" class="input mono" placeholder={translate("configurationSection.qwen257b")} bind:value={llm.model} oninput={invalidateLLMTest} />
        {/if}
      </div>
      {#if llmMode === 'subscription'}
        {#if modelsError}<p class="err subscription-model-status" role="alert">{modelsError}</p>{/if}
        {#if modelSaving}<p class="sub subscription-model-status" role="status">{translate("configurationSection.savingModelSelection")}</p>{/if}
        {#if modelSaveError}<p class="err subscription-model-status" role="alert">{modelSaveError}</p><button class="btn sm" onclick={persistSubscriptionModel}>{translate("configurationSection.retrySavingModel")}</button>{/if}
      {:else}
      <div class="field"><label for="l-key">{translate("configurationSection.apiKeyBlankForLocal")}</label>
        <input id="l-key" class="input mono" type="password" bind:value={llm.api_key} autocomplete="off"
               oninput={invalidateLLMTest}
               disabled={llm.clear_api_key}
               placeholder={llmStatus?.has_api_key ? translate("configurationSection.storedKeyLeaveBlankToKeep") : ''} /></div>
      {#if llmStatus?.has_api_key}
        <label class="wiz-check"><input type="checkbox" checked={llm.clear_api_key} onchange={setClearAPIKey} />
          {translate("configurationSection.clearTheSavedApiKeyWhenSaving")}</label>
      {/if}
      {/if}
      {#if llmIsRemote}
        <label class="wiz-check attn" class:subscription-ack={llmMode === 'subscription'}><input type="checkbox" bind:checked={llm.egress_ack} onchange={invalidateLLMTest} />
          {translate("configurationSection.thisModelRunsOutsideThisMachineI")}</label>
      {/if}

      <div class="field model-confidence"><label for="l-confidence">{translate("configurationSection.minimumModelConfidenceToApplyAutomaticallyValue", { "confidence_threshold": Math.round(Number(llm.confidence_threshold) * 100) })}</label>
        <input id="l-confidence" class="range" type="range" min="0.5" max="0.95" step="0.05" bind:value={llm.confidence_threshold} oninput={invalidateLLMTest} /></div>
      {#if llmMode === 'subscription'}
        <div class="subscription-test" class:tested={!!llmTestResult}>
          <div class="subscription-test-main">
            <button class="btn sm" disabled={busy || llmTesting || modelSaving || !!modelSaveError || !llm.model || !llmStatus?.subscription_connected || modelsLoading || !subscriptionModels.some(model => model.id === llm.model) || !llm.egress_ack}
                    onclick={testClassifier}>{llmTesting ? translate("configurationSection.testing") : translate("configurationSection.testConnection")}</button>
            {#if llmTestError}
              <span class="subscription-test-verdict failed" role="alert">
                <span class="verdict-mark" aria-hidden="true">!</span>
                {translate("configurationSection.connectionFailedValue", { "llmTestError": llmTestError })}
              </span>
            {:else if llmTestResult}
              <span class="subscription-test-verdict" role="status">
                <span class="verdict-mark" aria-hidden="true">✓</span>
                {translate("configurationSection.respondedInValueModelConfidenceValueSelf", { "elapsed_ms": elapsedLabel(llmTestResult.elapsed_ms), "confidence": Math.round(Number(llmTestResult.confidence) * 100) })}
              </span>
            {/if}
          </div>
          <p>{subscriptionTestHint()}</p>
        </div>
      {:else}
        <div class="toolbar connection-actions">
          <button class="btn primary sm" disabled={busy || llmTesting || modelSaving || !!modelSaveError || !llm.endpoint_url || !llm.model || (llmIsRemote && !llm.egress_ack)}
                  onclick={testClassifier}>{llmTesting ? translate("configurationSection.testing") : translate("configurationSection.testConnection")}</button>
        </div>
        {#if llmTestError}
          <div class="test-result failed">
            <b>{translate("configurationSection.connectionFailed")}</b>
            <span>{llmTestError}</span>
          </div>
        {:else if llmTestResult}
          <div class="test-result">
            <b>{translate("configurationSection.connectionRespondedInValueMs", { "elapsed_ms": llmTestResult.elapsed_ms })}</b>
            <span>{translate("configurationSection.valueModelConfidenceValueSelfReported", { "title": llmTestResult.title || 'No title', "confidence": Math.round(Number(llmTestResult.confidence) * 100) })}</span>
            {#if llmTestResult.tags?.length}<span class="sub">{translate("configurationSection.tagsValue", { "join": llmTestResult.tags.join(', ') })}</span>{/if}
          </div>
        {/if}
        <p class="wiz-p sub" style="font-size:.8rem;margin-top:12px">{translate("configurationSection.sendsOneSampleSentenceNeverYourDocuments")}</p>
      {/if}

          </div>
          <div class="mdl-foot"><span class="cls-note">{translate("configurationSection.enablingStartsSuggestionsForNewDocuments")}</span>
            {#if llmStatus?.enabled}<button class="act-link" style="color:var(--muted)" disabled={busy || llmTesting}
                onclick={() => saveAnd(() => saveClassifier(false), 'Model disabled; local matching settings unchanged').then(() => (modelDialogOpen = false))}>{translate("configurationSection.disableModel")}</button>{/if}
            <button class="btn primary sm" disabled={busy || llmTesting || modelSaving || !!modelSaveError || !llmTestResult || (llmMode !== 'subscription' && !llm.endpoint_url) || !llm.model || (llmMode === 'subscription' && (!llmStatus?.subscription_connected || modelsLoading || !subscriptionModels.some(model => model.id === llm.model))) || (llmIsRemote && !llm.egress_ack)}
                onclick={() => saveAnd(() => saveClassifier(true), translate("configurationSection.modelEnabled")).then(() => (modelDialogOpen = false))}>{translate("configurationSection.enableModel")}</button></div>
        </div>
      {/if}

  {/if}
  </div>

<style>
  .wiz-p { color: var(--muted); font-size: .92rem; margin: 6px 0 16px; max-width: 46em; }
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
  .cls-disc { margin: 0 20px 14px 20px; border: 1px solid var(--line); border-radius: var(--r-sm); background: var(--surface-2); padding: 14px 16px }
  .cls-note { color: var(--muted); font-size: .76rem; margin: 6px 0 0 }
  .act-link { background: none; border: 0; padding: 0; color: var(--accent); font: inherit; font-size: .8rem; font-weight: 600; cursor: pointer }
  .act-link:hover { text-decoration: underline }
  .mdl-dim { position: fixed; inset: 0; background: rgba(23,24,26,.4); z-index: 60 }
  .mdl { position: fixed; left: 50%; top: 7vh; transform: translateX(-50%); width: min(740px, 94vw); background: var(--surface); border-radius: var(--r); box-shadow: 0 30px 70px rgba(20,22,28,.3); z-index: 61; max-height: 86vh; overflow: auto }
  .mdl-head { display: flex; justify-content: space-between; align-items: center; padding: 18px 26px; border-bottom: 1px solid var(--line) } .mdl-head b { font-size: 1rem }
  .mdl-body { padding: 22px 26px } .mdl-body .seg { display: flex; width: 100%; margin-bottom: 18px }
  .mdl-body .seg button { display: inline-flex; align-items: center; justify-content: center; gap: 6px; flex: 1; }
  .mdl-foot { display: flex; align-items: center; gap: 14px; padding: 16px 26px; border-top: 1px solid var(--line); background: color-mix(in srgb, var(--surface-2) 55%, var(--surface)) }
  .mdl-foot .btn.primary { margin-left: auto }
  .subscription-provider { margin-bottom: 14px; }
  .subscription-maturity { display: flex; margin: 0 0 8px; }
  .experimental-tag {
    padding: 1px 6px;
    border: 1px solid color-mix(in srgb, var(--warn) 55%, transparent);
    border-radius: 999px;
    background: color-mix(in srgb, var(--warn) 30%, var(--surface));
    color: color-mix(in srgb, var(--warn) 82%, var(--ink));
    font-size: .52rem;
    font-weight: 800;
    letter-spacing: .04em;
    line-height: 1.2;
    text-transform: uppercase;
  }
  .subscription-account {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    align-items: center;
    gap: 14px;
    margin-bottom: 18px;
    padding: 14px 16px;
    border: 1px solid var(--line);
    border-radius: var(--r-sm);
    background: var(--surface-2);
  }
  .subscription-account-copy { display: flex; min-width: 0; flex-direction: column; gap: 3px; }
  .subscription-account-copy > span:last-child { color: var(--muted); font-size: .8rem; line-height: 1.4; }
  .subscription-state-title { display: flex; align-items: center; gap: 10px; color: var(--ink); font-size: .9rem; }
  .subscription-state-dot { width: 8px; height: 8px; flex: none; border-radius: 50%; background: var(--ok); }
  .subscription-account-actions { display: flex; align-items: center; justify-content: flex-end; }
  .subscription-disconnect { color: var(--muted); }
  .subscription-login {
    display: flex;
    grid-column: 1 / -1;
    align-items: center;
    justify-content: space-between;
    gap: 14px;
    padding-top: 12px;
    border-top: 1px solid var(--line);
  }
  .subscription-login p { margin: 0; color: var(--muted); font-size: .8rem; line-height: 1.45; }
  .subscription-account-error { grid-column: 1 / -1; margin: 0; }
  .field-label-row { display: flex; align-items: baseline; justify-content: space-between; gap: 12px; }
  .refresh-models { color: var(--muted); }
  .subscription-model-status { margin: -4px 0 12px; font-size: .78rem; }
  .subscription-ack { align-items: center; margin: 4px 0 18px; color: var(--warn); font-weight: 600; }
  .subscription-ack input { margin: 0; accent-color: var(--warn); }
  .model-confidence { margin-top: 16px; }
  .subscription-test {
    margin-top: 16px;
    padding: 14px 16px;
    border: 1px solid var(--line);
    border-radius: var(--r-sm);
    background: var(--surface-2);
  }
  .subscription-test-main { display: flex; align-items: center; gap: 14px; min-height: 34px; }
  .subscription-test-verdict { display: flex; align-items: center; gap: 8px; color: var(--ok); font-size: .8rem; font-weight: 650; }
  .subscription-test-verdict.failed { color: var(--danger); }
  .verdict-mark {
    display: inline-grid;
    width: 18px;
    height: 18px;
    flex: none;
    place-items: center;
    border-radius: 50%;
    background: var(--ok);
    color: white;
    font-size: .7rem;
    line-height: 1;
  }
  .subscription-test-verdict.failed .verdict-mark { background: var(--danger); }
  .subscription-test p { margin: 12px 0 0; color: var(--muted); font-size: .78rem; line-height: 1.45; }
  .subscription-test.tested p { text-align: right; }
  @media (max-width: 620px) {
    .mdl { top: 3vh; max-height: 94vh; }
    .mdl-head,
    .mdl-body,
    .mdl-foot { padding-right: 18px; padding-left: 18px; }
    .subscription-account { grid-template-columns: 1fr; }
    .subscription-account-actions { justify-content: flex-start; }
    .subscription-login,
    .subscription-test-main { align-items: flex-start; flex-direction: column; }
    .subscription-test.tested p { text-align: left; }
    .mdl-foot { flex-wrap: wrap; }
  }
</style>
