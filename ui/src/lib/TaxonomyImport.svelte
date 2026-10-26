<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "./i18n.svelte.js";
  import { plural } from './format.js'
  import { onDestroy } from 'svelte'
  import { importTaxonomy } from './api.js'
  import { systems, captureScope, scopeCurrent, selectSystem } from './systems.svelte.js'
  import { refreshSystems } from './system-routing.js'
  import ConfirmDialog from './ConfirmDialog.svelte'
  import Icon from './Icon.svelte'

  let { notify, onApplied, busy = $bindable(false) } = $props()
  let content = $state('')
  let format = $state('toml')
  let fileName = $state('')
  let diff = $state(null)
  let collisions = $state([])
  let err = $state('')
  let reading = $state(false)
  let remaps = $state({})
  let confirmBlank = $state(false)
  let firstDestination = $state(false)
  let separate = $state(false)
  let existingCode = $state('A00')
  const scope = captureScope()
  let generation = 0
  let reader
  let fileInput
  let controller
  const canApply = $derived(!!diff?.state_hash && !diff.applied && !diff.collisions?.some(c => !c.resolved))

  function invalidate(newContent = false) {
    generation++
    reader?.abort()
    reader = null
    reading = false
    diff = null
    err = ''
    confirmBlank = false
    if (newContent) {
      collisions = []
      remaps = {}
      firstDestination = false
      separate = false
      existingCode = 'A00'
    }
  }

  function detectContentFormat(value) {
    const separator = value.match(/^\s*format\s*([=:])/m)?.[1]
    if (separator === '=') return 'toml'
    if (separator === ':') return 'huml'
    return ''
  }

  function editContent(value) {
    fileName = ''
    const detected = detectContentFormat(value)
    if (detected) format = detected
    invalidate(true)
  }

  function pick(event) {
    const file = event.currentTarget.files?.[0]
    event.currentTarget.value = ''
    if (!file) return
    invalidate(true)
    fileName = file.name
    content = ''
    const extension = file.name.split('.').pop()?.toLowerCase()
    if (!['huml', 'toml'].includes(extension)) {
      err = translate("taxonomyImport.chooseAHumlOrTomlFileOrPaste")
      return
    }
    format = extension
    const version = generation
    const nextReader = new FileReader()
    reader = nextReader
    reading = true
    nextReader.onload = () => {
      if (version !== generation) return
      content = String(nextReader.result || '')
      reading = false
      reader = null
    }
    nextReader.onerror = () => {
      if (version !== generation) return
      err = translate("taxonomyImport.couldNotReadValueChooseTheFileAgain", { "name": file.name })
      reading = false
      reader = null
    }
    try { nextReader.readAsText(file) }
    catch { nextReader.onerror() }
  }

  async function run(apply) {
    if (busy || reading || !content.trim() || (apply && !canApply)) return
    const version = ++generation
    const body = { content, format, apply, remaps: { ...remaps } }
    if (separate && firstDestination) body.existing_system_code = existingCode
    if (apply) body.expected_state_hash = diff.state_hash
    busy = true
    err = ''
    controller = new AbortController()
    try {
      const result = await importTaxonomy(body, controller.signal)
      if (version !== generation) return
      diff = result
      if (!systems.introduced && result.systems_introduced) firstDestination = true
      collisions = result.collisions || []
      if (apply && result.applied) {
        confirmBlank = false
        notify?.(translate("taxonomyImport.importedValueVersionValue", { "name": result.name, "preset_version": result.preset_version }))
        try {
          if (!result.system_code) await onApplied?.(result)
          if (!scopeCurrent(scope)) return
          await refreshSystems()
          if (scopeCurrent(scope) && result.system_code) selectSystem(result.system_code)
        } catch { err = translate("taxonomyImport.importSucceededButRefreshingTheArchiveFailedReload") }
      }
    } catch (ex) {
      if (version !== generation) return
      diff = null
      confirmBlank = false
      err = ex.status === 409
        ? translate("taxonomyImport.thePreviewIsNoLongerCurrentYourInput")
        : (ex.message || translate("taxonomyImport.couldNotValidateTheFileTryPreviewAgain"))
    } finally {
      if (version === generation) busy = false
    }
  }

  function applyImport() {
    if (!canApply || busy) return
    if (diff.categories_incoming === 0) confirmBlank = true
    else run(true)
  }

  onDestroy(() => {
    generation++
    reader?.abort()
    controller?.abort()
    busy = false
  })
</script>

<section class="taximp" aria-label={translate("taxonomyImport.importTaxonomy")}>
  <h3>{translate("taxonomyImport.importTaxonomy")}</h3>
  <div class="source-toolbar">
    <div class="file-source">
      <button type="button" class="btn file-picker" disabled={busy} onclick={() => fileInput?.click()}>
        <Icon name="upload" size={14} />
        {translate("taxonomyImport.chooseTaxonomyFile")}
      </button>
      <input bind:this={fileInput} type="file" accept=".huml,.toml" disabled={busy} onchange={pick} />
      {#if fileName}<span class="file-name sub">{fileName}</span>{/if}
    </div>
    <label class="serialization">
      <span>{translate("taxonomyImport.fileFormat")}</span>
      <select class="input" bind:value={format} disabled={busy} onchange={() => invalidate()}>
        <option value="toml">TOML</option><option value="huml">{translate("taxonomyImport.huml")}</option>
      </select>
    </label>
  </div>
  <label class="content-label" for="taxonomy-content">{translate("taxonomyImport.pasteContent")}</label>
  <textarea id="taxonomy-content" class="input mono" rows="7" bind:value={content} disabled={busy}
            oninput={(event) => editContent(event.currentTarget.value)} placeholder={translate("taxonomyImport.pasteASuchiTaxonomyV1File")}></textarea>
  {#if reading}<p class="sub" role="status">{translate("taxonomyImport.readingFile")}</p>{/if}
  <p class="sub">{translate("taxonomyImport.validationChecksSupportedStructureNotDomainCorrectness")}</p>
  {#if firstDestination && !diff?.applied}
    <fieldset disabled={busy} style="margin:12px 0;padding:12px;border:1px solid var(--line)">
      <legend>{translate("taxonomyImport.firstSystemImportDestination")}</legend>
      <label><input type="radio" name="first-destination" checked={!separate}
        onchange={() => { separate = false; invalidate() }} /> {translate("taxonomyImport.useThisArchiveNameItsExistingDocuments")}</label>
      <label style="display:block;margin-top:8px"><input type="radio" name="first-destination" checked={separate}
        onchange={() => { separate = true; invalidate() }} /> {translate("taxonomyImport.createSeparatelyPreserveThisArchiveUnderAnother")}</label>
      {#if separate}
        <label>{translate("taxonomyImport.existingArchiveCode")} <input class="input mono" bind:value={existingCode} maxlength="3"
          pattern="[A-Z][0-9]{2}" required oninput={() => invalidate()} /></label>
      {/if}
      <p class="sub">{translate("taxonomyImport.previewAgainAfterChangingTheDestinationSystem")}</p>
    </fieldset>
  {/if}

  {#if err}<div class="err" role="alert">{err}</div>{/if}

  {#if diff}
    <div class="card preview" aria-label={translate("taxonomyImport.taxonomyPreview")}>
      <h4>{translate("taxonomyImport.valueValueVersionValue", { "name": diff.name, "preset_id": diff.preset_id, "preset_version": diff.preset_version })}</h4>
      <p>{diff.story}</p>
      <p class="sub">{diff.format} · {diff.mode === 'replace' ? translate("taxonomyImport.replaceTheInitialFilingTree") : translate("taxonomyImport.addToTheCurrentFilingTree")}</p>
      {#if diff.system_code}
        <p><b>{diff.system_code} · {diff.system_name}</b> — {diff.system_created ? translate("taxonomyImport.createANewFilingSystem") : translate("taxonomyImport.importIntoTheExistingArchive")}.</p>
        {#if diff.systems_introduced}
          <p>{translate("taxonomyImport.valueFullFilingAddressesAndSystemRooted", { "system_code": diff.existing_system_code ? `Existing documents, memberships and permissions stay in ${diff.existing_system_code}.` : `Existing documents keep their IDs, memberships and permissions in ${diff.system_code}.` })}</p>
        {/if}
        {#if diff.system_created}<p>{translate("taxonomyImport.initiallyOnlyServerAdministratorsCanOpenThe")}</p>{/if}
        <p class="sub">{translate("taxonomyImport.suchiWillUpdateTheFilingIndexAnd")}</p>
      {/if}
      <div class="trees">
        <div>
          <h4>{translate("taxonomyImport.userAreasAndCategories")}</h4>
          {#each diff.user_areas || [] as area (area.code)}
            <p><b>{area.code}–{area.code + 9} {area.name}</b></p>
            <ul>{#each area.categories || [] as category (category.code)}
              <li>{category.code} {category.name}{#if category.description}<span class="sub description">{category.description}</span>{/if}</li>
            {/each}</ul>
          {:else}<p>{translate("taxonomyImport.noUserCategoriesBlankTree")}</p>{/each}
        </div>
        <div>
          <h4>{translate("taxonomyImport.generatedBySuchi")}</h4>
          <p><b>{translate("taxonomyImport.0009SystemIndex")}</b><span class="sub description">{translate("taxonomyImport.0000ArchiveHumlIsAnOn")}</span></p>
          {#each diff.generated_areas || [] as area (area.code)}
            <p><b>{area.code}–{area.code + 9} {area.name}</b></p>
            <ul>{#each area.categories || [] as category (category.code)}<li>{category.code} {category.name}</li>{/each}</ul>
          {/each}
        </div>
      </div>
      <h4>{diff.applied ? translate("taxonomyImport.applied") : translate("taxonomyImport.plannedChanges")}</h4>
      <p>{translate(diff.applied ? "taxonomyImport.addedCounts" : "taxonomyImport.addsCounts", { categories: plural("taxonomyImport.categoryCount", diff.categories_to_add?.length || 0) })}</p>
      {#if diff.categories_to_add?.length}<p class="sub">{translate("taxonomyImport.categoryCodesValue", { "join": diff.categories_to_add.join(', ') })}</p>{/if}
      <p class="sub hash">{translate("taxonomyImport.fileFingerprintSha256")} <code>{diff.content_sha256}</code></p>
      {#if diff.applied && diff.index_refresh_pending}<p role="status">{translate("taxonomyImport.theFilingMapWillUpdateInThe")}</p>{/if}
    </div>
  {/if}

  {#if collisions.length && !diff?.applied}
    <div class="collisions" aria-label={translate("taxonomyImport.categoryCollisions")}>
      <h4>{translate("taxonomyImport.categoryCollisions")}</h4>
      <p class="sub">{translate("taxonomyImport.keepEachExistingCategoryExplicitlySkipThe")}</p>
      {#each collisions as collision (collision.code)}
        <div class="collision">
          <p><b>{collision.code}</b> {translate("taxonomyImport.existingValueIncomingValue", { "existing": collision.existing, "incoming": collision.incoming })}</p>
          <label>{translate("taxonomyImport.choiceForValue", { "code": collision.code })}
            <select class="input" disabled={busy} value={remaps[collision.code] ?? ''}
              onchange={(event) => {
                const next = { ...remaps }
                if (event.currentTarget.value === '') delete next[collision.code]
                else next[collision.code] = Number(event.currentTarget.value)
                remaps = next
                invalidate()
              }}>
              <option value="">{translate("taxonomyImport.chooseAResolution")}</option>
              {#if collision.proposed_code}<option value={collision.proposed_code}>{translate("taxonomyImport.moveToValue", { "proposed_code": collision.proposed_code })}</option>{/if}
              <option value={0}>{translate("taxonomyImport.skipIncoming")}</option>
            </select>
          </label>
        </div>
      {/each}
    </div>
  {/if}
  <div class="toolbar">
    <button class="btn primary sm" disabled={busy || reading || !content.trim()} onclick={() => run(false)}>{busy && !confirmBlank ? translate("taxonomyImport.working") : translate("taxonomyImport.preview")}</button>
    <button class="btn sm" disabled={busy || !canApply} onclick={applyImport}>{translate("taxonomyImport.applyImport")}</button>
    {#if !diff && collisions.length}<span class="sub">{translate("taxonomyImport.choicesChangedPreviewAgainToReviewTheir")}</span>{/if}
  </div>
</section>

{#if confirmBlank}
  <ConfirmDialog title={translate("taxonomyImport.importABlankFilingTree")}
    message={translate("taxonomyImport.thisFileHasNoUserCategoriesDocuments")}
    confirmLabel={translate("taxonomyImport.applyBlankImport")} busyLabel={translate("taxonomyImport.applying")} {busy} onConfirm={() => run(true)} onCancel={() => { confirmBlank = false }} />
{/if}

<style>
  .taximp { min-width: 0; }
  .taximp h4 { margin: 12px 0 8px; }
  .taximp p, .taximp li { overflow-wrap: anywhere; }
  .taximp .sub { color: color-mix(in srgb, var(--ink) 75%, var(--muted)); font-size: .78rem; line-height: 1.5; }
  .taximp input[type=file] { display: none; }
  .taximp label:focus-within { outline: 2px solid var(--accent); outline-offset: 3px; }
  .source-toolbar { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; margin-bottom: 14px; }
  .file-source { display: flex; align-items: center; gap: 10px; min-width: 0; }
  .file-picker {
    cursor: pointer; color: var(--accent); background: var(--tint);
    border-color: color-mix(in srgb, var(--accent) 38%, var(--line));
  }
  .file-picker:hover:not(:disabled) { border-color: var(--accent); color: var(--accent); }
  .serialization { display: flex; align-items: center; gap: 8px; margin-left: auto; color: var(--muted); font-size: .78rem; font-weight: 600; }
  .serialization > span { white-space: nowrap; }
  .serialization .input { min-width: 130px; margin-top: 0; }
  .file-name { overflow-wrap: anywhere; min-width: 0; }
  .content-label { display: block; margin: 8px 0; }
  textarea { width: 100%; resize: vertical; font-size: .78rem; }
  .preview { margin: 12px 0; padding: 14px; }
  .trees { display: grid; grid-template-columns: 1fr 1fr; gap: 20px; }
  .trees > div { min-width: 0; }
  ul { padding-left: 20px; }
  .description { display: block; }
  .hash { overflow-wrap: anywhere; }
  .collision { padding: 8px 0; border-bottom: 1px solid var(--line); }
  .collision select { max-width: 100%; margin-top: 6px; }
  @media (max-width: 600px) {
    .source-toolbar { align-items: flex-start; }
    .file-source { flex: 1 1 100%; flex-wrap: wrap; }
    .serialization { margin-left: 0; }
    .trees { grid-template-columns: 1fr; gap: 0; }
  }
</style>
