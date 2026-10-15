<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from '../lib/i18n.svelte.js'
  import { getArchiveMatchingSettings, saveArchiveMatchingSettings, saveClassificationAutoApply } from '../lib/api.js'

  let { notify } = $props()
  let loading = $state(true)
  let loadError = $state('')
  let error = $state('')
  let busy = $state(false)
  let thresholdsOpen = $state(false)
  let autoApply = $state(false)
  let matching = $state({ archive_enabled: true, archive_review_threshold: 0.5, archive_auto_threshold: 0.9 })
  let loadVersion = 0

  async function load() {
    const version = ++loadVersion
    loading = true
    loadError = ''
    try {
      const settings = await getArchiveMatchingSettings()
      if (version !== loadVersion) return
      autoApply = settings?.auto_apply ?? true
      matching.archive_enabled = settings?.archive_enabled ?? true
      matching.archive_review_threshold = settings?.archive_review_threshold ?? 0.5
      matching.archive_auto_threshold = settings?.archive_auto_threshold ?? 0.9
    } catch (exception) {
      if (version === loadVersion) loadError = exception.message || translate('configurationSection.couldNotLoadClassificationSettings')
    } finally {
      if (version === loadVersion) loading = false
    }
  }

  async function save(action, message) {
    error = ''
    busy = true
    try {
      await action()
      notify?.(message)
    } catch (exception) {
      error = exception.message || translate('configurationSection.theServerRejectedThat')
    } finally {
      busy = false
    }
  }

  function saveMatching() {
    return saveArchiveMatchingSettings({
      archive_enabled: matching.archive_enabled,
      archive_review_threshold: Number(matching.archive_review_threshold),
      archive_auto_threshold: Number(matching.archive_auto_threshold),
    })
  }

  load()
</script>

<div class="configuration-section">
  {#if loadError}
    <div class="err">{loadError}</div>
    <div class="toolbar"><button class="btn sm" onclick={load}>{translate('common.retry')}</button></div>
  {:else if loading}
    <div class="configuration-loading" aria-label={translate('configurationSection.loadingConfiguration')}>
      <div class="skel" style="width:32%;height:16px"></div>
      <div class="skel" style="width:78%"></div>
      <div class="skel" style="width:62%"></div>
    </div>
  {:else}
    {#if error}<div class="err">{error}</div>{/if}
    <div class="cls-card">
      <div class="cls-head"><h3>{translate('configurationSection.suggestions')}</h3><p>{translate('configurationSection.howSuggestionsAreMadeAndApplied')}</p></div>
      <div class="cls-row application-row">
        <div class="apply-mode" role="radiogroup" aria-label={translate('configurationSection.suggestionApplication')} aria-describedby="application-mode-help">
          <label class="apply-opt"><input type="radio" name="application-mode" value="review" checked={!autoApply} disabled={busy} onchange={() => (autoApply = false)} />
            <span><b>{translate('configurationSection.reviewFirst')}</b><small>{translate('configurationSection.everySuggestionWaitsInApprovals')}</small></span></label>
          <label class="apply-opt"><input type="radio" name="application-mode" value="auto" checked={autoApply} disabled={busy} onchange={() => (autoApply = true)} />
            <span><b>{translate('configurationSection.applyWhenConfident')}</b><small>{translate('configurationSection.suggestionsAtOrAboveYourThresholdApply')}</small></span></label>
        </div>
        <p class="cls-note" id="application-mode-help">{translate('configurationSection.datesFollowTheSameChoiceIntoCalendar')}</p>
        <button class="btn sm" disabled={busy} onclick={() => save(() => saveClassificationAutoApply(autoApply), translate('configurationSection.applicationModeSaved'))}>{translate('configurationSection.saveApplicationMode')}</button>
      </div>
      <div class="cls-row matching-row">
        <button type="button" class="switch" role="switch" aria-checked={matching.archive_enabled}
                aria-label={translate('configurationSection.offerFilingSuggestionsFromSimilarDocuments')}
                onclick={() => (matching.archive_enabled = !matching.archive_enabled)}></button>
        <div class="cls-main"><b>{translate('configurationSection.similarDocuments')}</b><small>{translate('configurationSection.learnsFromWhatIsAlreadyFiledNothing')}</small></div>
        <div class="cls-right"><span class="mono-sum">{translate('configurationSection.reviewValueAutoValue', { archive_review_threshold: Math.round(Number(matching.archive_review_threshold) * 100), archive_auto_threshold: Math.round(Number(matching.archive_auto_threshold) * 100) })}</span>
          <button class="act-link" onclick={() => (thresholdsOpen = !thresholdsOpen)}>{thresholdsOpen ? translate('configurationSection.hide') : translate('configurationSection.adjust')}</button></div>
      </div>
      {#if thresholdsOpen}
        <div class="cls-disc">
          <div class="field"><label for="archive-review">{translate('configurationSection.minimumSimilarityToSuggestForReviewValue', { archive_review_threshold: Math.round(Number(matching.archive_review_threshold) * 100) })}</label>
            <input id="archive-review" class="range" type="range" min="0.5" max="0.9" step="0.05" bind:value={matching.archive_review_threshold} /></div>
          <div class="field"><label for="archive-auto">{translate('configurationSection.minimumSimilarityToApplyAutomaticallyValue', { archive_auto_threshold: Math.round(Number(matching.archive_auto_threshold) * 100) })}</label>
            <input id="archive-auto" class="range" type="range" min="0.55" max="0.95" step="0.05" bind:value={matching.archive_auto_threshold} disabled={!autoApply} /></div>
          <p class="cls-note">{translate('configurationSection.theAutomaticLevelMustStayAboveThe')}</p>
          {#if Number(matching.archive_review_threshold) >= Number(matching.archive_auto_threshold)}<p role="alert">{translate('configurationSection.theReviewLevelMustBeBelowThe')}</p>{/if}
        </div>
      {/if}
      <div class="cls-foot"><span class="cls-note">{translate('configurationSection.newDocumentsOnlyUseRescanInDocuments')}</span>
        <button class="btn primary sm" disabled={busy || Number(matching.archive_review_threshold) >= Number(matching.archive_auto_threshold)}
                onclick={() => save(saveMatching, translate('configurationSection.similarDocumentMatchingSaved'))}>{translate('configurationSection.saveMatchingOptions')}</button></div>
    </div>
  {/if}
</div>

<style>
  .configuration-loading { display: grid; gap: 10px; padding: 8px 0; }
  .cls-card { overflow: hidden; margin-bottom: 14px; border: 1px solid var(--line); border-radius: var(--r); }
  .cls-head { padding: 14px 16px; border-bottom: 1px solid var(--line); background: var(--bg); }
  .cls-head h3 { margin: 0; font-size: .9rem; }
  .cls-head p { margin: 3px 0 0; color: var(--muted); font-size: .74rem; }
  .cls-row { padding: 14px 16px; border-bottom: 1px solid var(--line); }
  .application-row { display: grid; grid-template-columns: minmax(0, 1fr) auto; align-items: end; gap: 8px 18px; }
  .application-row .cls-note { grid-column: 1; }
  .application-row > .btn { grid-column: 2; grid-row: 1 / span 2; }
  .matching-row { display: grid; grid-template-columns: auto minmax(0, 1fr) auto; align-items: center; gap: 12px; }
  .cls-main b, .cls-main small { display: block; }
  .cls-main small, .cls-note { color: var(--muted); font-size: .75rem; }
  .cls-right { display: flex; align-items: center; gap: 10px; }
  .mono-sum { color: var(--muted); font-family: ui-monospace, monospace; font-size: .7rem; }
  .apply-mode { display: flex; flex-direction: column; gap: 10px; }
  .apply-opt { display: flex; align-items: flex-start; gap: 10px; cursor: pointer; }
  .apply-opt input { margin-top: 3px; }
  .apply-opt b, .apply-opt small { display: block; }
  .apply-opt b { color: var(--ink); font-size: .88rem; font-weight: 600; }
  .apply-opt small { margin-top: 2px; color: var(--muted); font-size: .78rem; }
  .cls-disc { padding: 14px 16px; border-bottom: 1px solid var(--line); background: var(--bg); }
  .range { width: 100%; accent-color: var(--accent); }
  .cls-foot { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 12px 16px; }
  @media (max-width: 700px) {
    .application-row { display: block; }
    .application-row > .btn { margin-top: 12px; }
    .matching-row { grid-template-columns: auto minmax(0, 1fr); }
    .cls-right { grid-column: 2; justify-content: space-between; }
    .cls-foot { align-items: stretch; flex-direction: column; }
  }
</style>
