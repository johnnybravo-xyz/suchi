<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "../lib/i18n.svelte.js";
  import { onDestroy } from 'svelte'
  import { scopedHash as filingHref } from '../lib/systems.svelte.js'
  import { listSavedViews, createSavedView, patchSavedView, deleteSavedView,
           listDocuments, listTags, listCorrespondents, listCustomFields } from '../lib/api.js'
  import {
    canonicalSavedViewQuery,
    documentListHash,
    extractFieldPresence,
    fieldPresenceSelection,
    parseFieldPresenceSelection,
    parseSavedViewFilters,
  } from '../lib/documentFilters.js'
  import { SENSITIVITY_OPTIONS, customFieldLabel, plural, sensitivityLabel } from '../lib/format.js'
  import { DATE_ROLES, intelligenceRoleLabel } from '../lib/intelligence.js'
  import Icon from '../lib/Icon.svelte'
  import ISODateInput from '../lib/ISODateInput.svelte'
  import RuleWorkspace from '../lib/RuleWorkspace.svelte'
  import RuleRow from '../lib/RuleRow.svelte'
  import RuleEmpty from '../lib/RuleEmpty.svelte'
  import RuleSection from '../lib/RuleSection.svelte'
  import ConfirmDialog from '../lib/ConfirmDialog.svelte'
  import { go } from '../lib/router.svelte.js'

  let { notify, canShare = false, startCreate = false, createQuery = '', createDocumentIDs = '', jdCategories = [] } = $props()
  let views = $state([])
  let loading = $state(true)
  let loadError = $state('')
  let saving = $state(false)
  let saveError = $state('')
  let editorOpen = $state(false)
  let editing = $state(null)
  let startCreateHandled = $state(false)
  let nameInput = $state()
  let tags = $state([]), corrs = $state([]), customFields = $state([])
  let visibleFilters = $state([])
  let matchCount = $state(null)
  let matchLoading = $state(false)
  let matchError = $state(false)
  const filingCategories = $derived(jdCategories.filter((category) => !category.is_area))
  let facetsPromise
  let facetsError = $state('')
  let loadVersion = 0
  let matchController
  let matchTimer
  let matchVersion = 0
  let nv = $state(emptyView())
  let deleteRequest = $state(null)
  let deleteBusy = $state(false)

  const filterFields = {
    tag: 'tags__id__in',
    corr: 'correspondents__id__in',
    jd: 'jd_category_id',
    sens: 'sensitivity',
  }
  const VIEW_FILTERS = [
    { kind: 'jd', label: 'category', keys: ['jd'] },
    { kind: 'tag', label: 'tag', keys: ['tag'] },
    { kind: 'corr', label: 'correspondent', keys: ['corr'] },
    { kind: 'fieldPresence', label: translate("views.customField2"), keys: ['fieldPresence'] },
    { kind: 'sens', label: 'sensitivity', keys: ['sens'] },
    { kind: 'dates', label: 'dates', keys: ['dateFrom', 'dateTo', 'dateRole'] },
    { kind: 'q', label: translate("views.searchText2"), keys: ['q'] },
  ]
  const DEFAULT_VIEW_PATTERNS = [
    { value: 'waiting', label: translate("views.waitingToFile") },
    { value: 'taxes', label: translate("views.thisYearSTaxes") },
    { value: 'correspondent', label: translate("views.fromOneCorrespondent") },
  ]
  const viewPatterns = $derived.by(() => {
    const categories = filingCategories.filter((category) => !category.system)
    const selected = []
    const selectedIDs = new Set()
    const selectedAreas = new Set()
    const add = (category) => {
      selected.push({
        value: `category:${category.id}`,
        label: translate("views.valueValue", { "code": category.code, "name": category.name }),
      })
      selectedIDs.add(category.id)
    }
    for (const category of categories) {
      const area = String(category.area_code ?? '')
      if (selectedAreas.has(area)) continue
      selectedAreas.add(area)
      add(category)
      if (selected.length === 3) return selected
    }
    for (const category of categories) {
      if (selectedIDs.has(category.id)) continue
      add(category)
      if (selected.length === 3) return selected
    }
    for (const pattern of DEFAULT_VIEW_PATTERNS) {
      selected.push(pattern)
      if (selected.length === 3) return selected
    }
    return selected
  })

  function resetEditor() {
    editorOpen = false
    editing = null
    visibleFilters = []
    nv = emptyView()
  }

  function emptyView() {
    return { name: '', q: '', tag: '', corr: '', jd: '', sens: '', dateFrom: '', dateTo: '', dateRole: '', fieldPresence: '', ids: [], shared: false }
  }

  function parseDocumentIDs(value) {
    return [...new Set(String(value || '').split(',')
      .map(id => Number(id.trim()))
      .filter(id => Number.isInteger(id) && id > 0))].slice(0, 100)
  }

  async function load() {
    const version = ++loadVersion
    loading = true
    loadError = ''
    try {
      const r = await listSavedViews({ include: 'shared' })
      if (version !== loadVersion) return
      const raw = r?.results || r || []
      views = raw.map(view => ({ ...view, filters: parseSavedViewFilters(view.filter_json) }))
    } catch (ex) {
      if (version === loadVersion) loadError = ex.message || translate("views.couldNotLoadSavedViews2")
    } finally {
      if (version === loadVersion) loading = false
    }
  }


  function href(view) {
    return documentListHash(view.filters)
  }

  function nameFor(items, id, fallback, format = (item) => item.name) {
    const item = items.find((candidate) => String(candidate.id) === String(id))
    return item ? format(item) : fallback
  }

  function filterSummary(filters) {
    const summary = []
    if (Array.isArray(filters.document_ids) && filters.document_ids.length) {
      summary.push(plural("views.savedDocumentCount", filters.document_ids.length))
    }
    if (filters.q) summary.push(filters.q === 'is:inbox' ? translate("views.inInbox") : filters.q)
    if (filters.jd_category_id) {
      summary.push(translate("views.filedUnderValue", { name: nameFor(filingCategories, filters.jd_category_id, translate("views.aCategory"), (category) => `${category.code} ${category.name}`) }))
    }
    if (filters.tags__id__in) summary.push(translate("views.tagValue", { tag: nameFor(tags, filters.tags__id__in, translate("views.selectedTag")) }))
    if (filters.correspondents__id__in) summary.push(translate("views.correspondentValue", { correspondent: nameFor(corrs, filters.correspondents__id__in, translate("views.selectedCorrespondent")) }))
    if (filters.sensitivity) summary.push(sensitivityLabel(filters.sensitivity).toLocaleLowerCase())
    if (filters.ordering) summary.push(filters.ordering === 'title' ? translate("views.titleOrder") : filters.ordering === '-created_at' ? translate("views.newestFirst") : translate("views.customOrder"))
    return summary.join(' · ') || translate("views.allDocuments")
  }


  function knownFieldPresence(value) {
    const selection = parseFieldPresenceSelection(value)
    return selection && customFields.some((field) => field.name === selection.name)
  }

  function fieldPresenceLabel(value) {
    const selection = parseFieldPresenceSelection(value)
    if (!selection) return translate("views.savedCustomField")
    const field = customFields.find(candidate => candidate.name === selection.name)
    const name = field ? customFieldLabel(field) : selection.name
    return translate("views.valueValue2", { value: translate(selection.missing ? "views.missingValue" : "views.hasValue"), name })
  }

  function configuredFilters(draft) {
    return VIEW_FILTERS.filter((filter) => filter.keys.some((key) => {
      const value = draft[key]
      return Array.isArray(value) ? value.length > 0 : String(value || '').trim() !== ''
    })).map((filter) => filter.kind)
  }

  function availableFilters() {
    const shown = new Set(visibleFilters)
    return VIEW_FILTERS.filter((filter) => !shown.has(filter.kind))
  }

  function addFilter(event) {
    const kind = event.currentTarget.value
    event.currentTarget.value = ''
    if (!kind || visibleFilters.includes(kind)) return
    visibleFilters = [...visibleFilters, kind]
  }

  function removeFilter(kind) {
    const filter = VIEW_FILTERS.find((candidate) => candidate.kind === kind)
    if (!filter) return
    for (const key of filter.keys) nv[key] = ''
    visibleFilters = visibleFilters.filter((candidate) => candidate !== kind)
  }

  function openCreate(q = '', ids = []) {
    editing = null
    const presence = extractFieldPresence(q)
    nv = { ...emptyView(), q: presence.query, fieldPresence: presence.selection, ids }
    openEditor()
  }

  function openPattern(kind) {
    editing = null
    nv = emptyView()
    let filters = []
    const category = String(kind).startsWith('category:')
      ? filingCategories.find((item) => `category:${item.id}` === kind)
      : null
    if (category) {
      nv.name = translate("views.valueValue", { "code": category.code, "name": category.name })
      nv.jd = category.id
      filters = ['jd']
    } else if (kind === 'waiting') {
      nv.name = translate("views.waitingToFile")
      nv.q = 'is:inbox'
      filters = ['q']
    } else if (kind === 'taxes') {
      const year = new Date().getFullYear()
      nv.name = translate("views.thisYearSTaxes")
      nv.q = 'tax'
      nv.dateFrom = `${year}-01-01`
      nv.dateTo = `${year}-12-31`
      filters = ['q', 'dates']
    } else if (kind === 'correspondent') {
      nv.name = translate("views.fromOneCorrespondent")
      filters = ['corr']
    }
    openEditor(filters)
  }

  function openEdit(view) {
    if (view.owner_id) return
    editing = view
    const presence = extractFieldPresence(view.filters.q || '')
    nv = {
      ...emptyView(),
      name: view.name,
      q: presence.query,
      fieldPresence: presence.selection,
      ids: view.filters.document_ids || [],
      shared: !!view.shared,
    }
    for (const [field, key] of Object.entries(filterFields)) {
      nv[field] = String(view.filters[key] ?? '')
    }
    openEditor()
  }

  function openEditor(filters = null) {
    if (!nv.ids.length) loadFacets()
    saveError = ''
    visibleFilters = filters ?? configuredFilters(nv)
    editorOpen = true
    queueMicrotask(() => nameInput?.focus())
  }

  function loadFacets() {
    if (facetsPromise) return facetsPromise
    facetsError = ''
    facetsPromise = Promise.allSettled([
      listTags().then((r) => (tags = r?.results || r || [])),
      listCorrespondents().then((r) => (corrs = r?.results || r || [])),
      listCustomFields().then((r) => (customFields = r?.results || r || [])),
    ]).then((results) => {
      if (results.some((result) => result.status === 'rejected')) {
        facetsError = translate("views.someFiltersCouldNotBeLoaded")
        facetsPromise = null
      }
    })
    return facetsPromise
  }

  function closeEditor() {
    if (saving) return
    matchController?.abort()
    clearTimeout(matchTimer)
    resetEditor()
    saveError = ''
    if (startCreate) go('#/views')
  }

  function draftFilters() {
    const filters = editing ? { ...editing.filters } : {}
    if (nv.ids.length) {
      filters.document_ids = nv.ids
      return filters
    }

    const draft = { ...nv }
    // Keep untouched legacy filters, including multi-value OR scopes.
    // Only changed selections join the rich query; ordering stays intact.
    for (const [field, key] of Object.entries(filterFields)) {
      if (String(filters[key] ?? '') === String(nv[field])) {
        draft[field] = ''
      } else {
        delete filters[key]
      }
    }
    const query = canonicalSavedViewQuery(draft, {
      tags,
      correspondents: corrs,
      categories: filingCategories,
    })
    if (query) filters.q = query
    else delete filters.q
    return filters
  }

  async function save(event) {
    event.preventDefault()
    if (!nv.name.trim() || saving) return

    saveError = ''
    saving = true
    try {
      const body = {
        name: nv.name.trim(),
        filter_json: JSON.stringify(draftFilters()),
        shared: canShare && nv.shared,
      }
      if (editing) {
        await patchSavedView(editing.id, body)
      } else {
        await createSavedView({
          ...body,
          display: 'list',
          position: views.filter((view) => !view.owner_id).length,
        })
      }
      notify?.(editing ? translate("views.viewUpdated") : translate("views.viewSaved"))
      resetEditor()
      if (startCreate) go('#/views')
      await load()
    } catch (ex) { saveError = ex.message || translate("views.couldNotSaveTheView") }
    finally { saving = false }
  }

  function remove(view) {
    if (deleteBusy || deleteRequest) return
    deleteRequest = view
  }

  async function confirmRemove() {
    const view = deleteRequest
    if (!view || deleteBusy) return
    deleteBusy = true
    try {
      await deleteSavedView(view.id)
      views = views.filter(candidate => candidate.id !== view.id)
      if (editing?.id === view.id) resetEditor()
      notify?.(translate("views.viewDeleted"))
      deleteRequest = null
    } catch (ex) {
      notify?.(ex.message || translate("views.couldNotDelete"))
      deleteRequest = null
    } finally {
      deleteBusy = false
    }
  }

  $effect(() => {
    const previewKey = editorOpen
      ? JSON.stringify([
          editing?.id || 0, nv.q, nv.tag, nv.corr, nv.jd, nv.sens,
          nv.dateFrom, nv.dateTo, nv.dateRole, nv.fieldPresence, nv.ids, visibleFilters,
        ])
      : ''
    clearTimeout(matchTimer)
    matchController?.abort()
    const version = ++matchVersion
    if (!previewKey) {
      matchCount = null
      matchLoading = false
      matchError = false
      return
    }
    matchLoading = true
    matchError = false
    matchTimer = setTimeout(async () => {
      const controller = new AbortController()
      matchController = controller
      try {
        const result = await listDocuments({ ...draftFilters(), page_size: 1 }, controller.signal)
        if (version !== matchVersion) return
        matchCount = result?.count ?? 0
      } catch (ex) {
        if (version !== matchVersion || ex?.name === 'AbortError') return
        matchCount = null
        matchError = true
      } finally {
        if (version === matchVersion) matchLoading = false
      }
    }, 180)
  })

  $effect(() => {
    if (!startCreate) {
      startCreateHandled = false
    } else if (!startCreateHandled) {
      startCreateHandled = true
      openCreate(createQuery, parseDocumentIDs(createDocumentIDs))
    }
  })

  onDestroy(() => {
    loadVersion++
    matchController?.abort()
    clearTimeout(matchTimer)
  })

  load()
</script>

<svelte:window onkeydown={(event) => { if (editorOpen && event.key === 'Escape') closeEditor() }} />

<div class="views-page">
  {#if editorOpen}
    <RuleWorkspace class="view-builder"
                   title={editing ? translate("views.editView") : translate("views.newView")}
                   headingId="view-editor-heading">
      <form id="view-editor-form" onsubmit={save}>
        {#if saveError}<div class="err editor-error" role="alert">{saveError}</div>{/if}

        <div class="builder-name">
          <input class="input" bind:this={nameInput}
                 aria-label={translate("views.viewName")}
                 placeholder={translate("views.nameEGTaxDocumentsToReview")}
                 bind:value={nv.name}
                 required />
        </div>

        <RuleSection class="view-filter-section" title={translate("views.showDocumentsWhere")} headingId="view-filters-heading">
          {#if nv.ids.length}
            <div class="snapshot-note">
              <span class="snapshot-mark"><Icon name="docs" size={18} /></span>
              <span>
                <strong>{translate("views.exactDocumentSet")}</strong>
                <small>{plural("views.keepsDocumentCount", nv.ids.length)}</small>
              </span>
            </div>
          {:else}
            {#if facetsError}
              <div class="err facet-error">
                <span>{facetsError}</span>
                <button type="button" class="btn sm" onclick={loadFacets}>{translate("common.retry")}</button>
              </div>
            {/if}

            <div class="view-filters">
              {#each visibleFilters as kind, filterIndex (kind)}
                <div class="filter-row">
                  <span class="conjunction">{filterIndex ? 'and' : ''}</span>
                  {#if kind === 'jd'}
                    <span class="field-token">{translate("views.filedUnder")}</span>
                    <span class="operator-token">{translate("views.is")}</span>
                    <select class="input filter-value" aria-label={translate("views.filingCategory")} bind:value={nv.jd}>
                      <option value="">{translate("views.chooseCategory")}</option>
                      {#each filingCategories as category}<option value={category.id}>{category.code} {category.name}</option>{/each}
                      {#if nv.jd && !filingCategories.some(category => String(category.id) === String(nv.jd))}
                        <option value={nv.jd}>{translate("views.savedCategoryValue", { "jd": nv.jd })}</option>
                      {/if}
                    </select>
                  {:else if kind === 'tag'}
                    <span class="field-token">{translate("views.tag")}</span>
                    <span class="operator-token">{translate("views.is")}</span>
                    <select class="input filter-value" aria-label={translate("views.tag")} bind:value={nv.tag}>
                      <option value="">{translate("views.chooseTag")}</option>
                      {#each tags as tag}<option value={tag.id}>{tag.name}</option>{/each}
                      {#if nv.tag && !tags.some(tag => String(tag.id) === String(nv.tag))}
                        <option value={nv.tag}>{translate("views.savedTagsValue", { "tag": nv.tag })}</option>
                      {/if}
                    </select>
                  {:else if kind === 'corr'}
                    <span class="field-token">{translate("views.correspondent")}</span>
                    <span class="operator-token">{translate("views.is")}</span>
                    <select class="input filter-value" aria-label={translate("views.correspondent")} bind:value={nv.corr}>
                      <option value="">{translate("views.chooseCorrespondent")}</option>
                      {#each corrs as correspondent}<option value={correspondent.id}>{correspondent.name}</option>{/each}
                      {#if nv.corr && !corrs.some(correspondent => String(correspondent.id) === String(nv.corr))}
                        <option value={nv.corr}>{translate("views.savedCorrespondentsValue", { "corr": nv.corr })}</option>
                      {/if}
                    </select>
                  {:else if kind === 'fieldPresence'}
                    <span class="field-token">{translate("views.customField")}</span>
                    <span class="operator-token">{translate("views.is")}</span>
                    <select class="input filter-value" aria-label={translate("views.customFieldValue")} bind:value={nv.fieldPresence}>
                      <option value="">{translate("views.chooseFieldCondition")}</option>
                      {#each customFields as field}
                        <option value={fieldPresenceSelection(field.name)}>{translate("views.hasValueValue", { "name": customFieldLabel(field) })}</option>
                        <option value={fieldPresenceSelection(field.name, true)}>{translate("views.missingValueValue", { "name": customFieldLabel(field) })}</option>
                      {/each}
                      {#if nv.fieldPresence && !knownFieldPresence(nv.fieldPresence)}
                        <option value={nv.fieldPresence}>{fieldPresenceLabel(nv.fieldPresence)}</option>
                      {/if}
                    </select>
                  {:else if kind === 'sens'}
                    <span class="field-token">{translate("views.sensitivity")}</span>
                    <span class="operator-token">{translate("views.is")}</span>
                    <select class="input filter-value" aria-label={translate("views.sensitivity")} bind:value={nv.sens}>
                      <option value="">{translate("views.chooseSensitivity")}</option>
                      {#each SENSITIVITY_OPTIONS as option (option.value)}
                        <option value={option.value}>{option.label}</option>
                      {/each}
                      {#if nv.sens && !SENSITIVITY_OPTIONS.some(option => option.value === nv.sens)}
                        <option value={nv.sens}>{sensitivityLabel(nv.sens)}</option>
                      {/if}
                    </select>
                  {:else if kind === 'dates'}
                    <span class="field-token">{translate("views.documentDate")}</span>
                    <span class="operator-token">{translate("views.isWithin")}</span>
                    <div class="date-values">
                      <ISODateInput id="view-date-from" bind:value={nv.dateFrom} label={translate("views.documentDateFrom")} compact />
                      <span>{translate("views.to")}</span>
                      <ISODateInput id="view-date-to" bind:value={nv.dateTo} label={translate("views.documentDateTo")} compact />
                      <select class="input date-role" aria-label={translate("views.documentDateRole")} bind:value={nv.dateRole}>
                        <option value="">{translate("views.everyRole")}</option>
                        {#each DATE_ROLES as role}<option value={role}>{intelligenceRoleLabel(role)}</option>{/each}
                      </select>
                    </div>
                  {:else if kind === 'q'}
                    <span class="field-token">{translate("views.searchText")}</span>
                    <span class="operator-token">{translate("views.matches")}</span>
                    <input class="input filter-value search-value"
                           aria-label={translate("views.query")}
                           placeholder={translate("views.eGInvoiceIsTrash")}
                           bind:value={nv.q} />
                  {/if}
                  <button class="remove-filter" type="button"
                          aria-label={translate("views.removeValueFilter", { "kind": VIEW_FILTERS.find(filter => filter.kind === kind)?.label || kind })}
                          onclick={() => removeFilter(kind)}>
                    <Icon name="x" size={12} />
                  </button>
                </div>
              {/each}
            </div>

            {#if availableFilters().length}
              <div class="add-filter-row">
                <span class="plus-mark" aria-hidden="true">+</span>
                <select class="add-filter-select" value="" aria-label={translate("views.addViewFilter")} onchange={addFilter}>
                  <option value="">{translate("views.addAFilter")}</option>
                  {#each availableFilters() as filter}<option value={filter.kind}>{filter.label}</option>{/each}
                </select>
                <span class="available-filter-hint">{availableFilters().map(filter => filter.label).join(' · ')}</span>
              </div>
            {/if}
          {/if}
        </RuleSection>

        {#if canShare}
          <RuleSection class="share-section" tone="quiet">
            <div class="share-heading">
              <button type="button" class="switch" role="switch"
                      aria-label={translate("views.shareThisView")} aria-checked={nv.shared}
                      onclick={() => (nv.shared = !nv.shared)}></button>
              <div>
                <h3>{translate("views.shareThisView")}</h3>
                <p>{translate("views.showsInEveryUserSViewsAnd")}</p>
              </div>
            </div>
          </RuleSection>
        {/if}
      </form>

      {#snippet footer()}
        <div class="footer-note">
          <span class="match-count" aria-live="polite">
            {#if matchLoading}
              <b>{translate("views.countingMatchingDocuments")}</b>
            {:else if matchError}
              <b>{translate("views.matchCountUnavailable")}</b>
            {:else}
              <b>{plural("views.matchingDocumentCount", matchCount ?? 0)}</b> {translate("views.rightNow")}
            {/if}
          </span>
          {#if editing}
            <button class="delete-view" type="button" disabled={deleteBusy || !!deleteRequest} onclick={() => remove(editing)}>{translate("views.deleteView")}</button>
          {/if}
        </div>
        <div class="footer-actions">
          <button class="btn" type="button" disabled={saving} onclick={closeEditor}>{translate("common.cancel")}</button>
          <button class="btn primary save-view" type="submit" form="view-editor-form"
                  disabled={saving || !nv.name.trim()}>
            {saving ? translate("common.saving") : editing ? translate("views.saveChanges") : translate("views.saveView")}
          </button>
        </div>
      {/snippet}
    </RuleWorkspace>
  {:else}
    <RuleWorkspace class="views-panel"
                   title={translate("views.views")}
                   description={translate("views.savedFiltersThatReopenExactlyWhereYou")}
                   headingId="views-heading">
      {#snippet actions()}
        <button class="btn primary new-view" onclick={() => openCreate()}>
          <Icon name="plus" size={15} /> {translate("views.newView")}
        </button>
      {/snippet}

      {#if loading}
        <div class="views-list" role="list" aria-label={translate("views.loadingSavedViews")}>
          {#each Array(3) as _}
            <div class="loading-row"><div class="skel"></div></div>
          {/each}
        </div>
      {:else if loadError}
        <RuleEmpty title={translate("views.couldNotLoadSavedViews")} description={loadError}>
          {#snippet actions()}
            <button class="btn sm retry-views" onclick={load}><Icon name="refresh" size={13} /> {translate("common.retry")}</button>
          {/snippet}
        </RuleEmpty>
      {:else if views.length === 0}
        <RuleEmpty title={translate("views.noViewsYet")}
                   description={translate("views.aViewIsAFilterYouKeep")}
                   patterns={viewPatterns}
                   onselect={openPattern} />
      {:else}
        <div class="views-list" role="list">
          {#each views as view (view.id)}
            <RuleRow class="view-row"
                     title={view.name}
                     summary={filterSummary(view.filters)}
                     href={filingHref(href(view))}
                     actionText={view.owner_id ? '' : translate("views.edit")}
                     actionLabel={translate("views.editValue", { "name": view.name })}
                     onaction={() => openEdit(view)}>
              {#snippet trailing()}
                {#if view.shared}
                  <span class="shared-pill" title={view.owner_id ? translate("views.sharedByUserValue", { "owner_id": view.owner_id }) : translate("views.sharedWithEveryUser")}>{translate("views.shared")}</span>
                {/if}
              {/snippet}
            </RuleRow>
          {/each}
        </div>
      {/if}
    </RuleWorkspace>
  {/if}
</div>

{#if deleteRequest}
  <ConfirmDialog
    title={translate("views.deleteTheViewValue", { "name": deleteRequest.name })}
    message={translate("views.theSavedFilterWillBeRemovedDocuments")}
    confirmLabel={translate("views.deleteView")}
    busyLabel={translate("views.deleting")}
    busy={deleteBusy}
    onConfirm={confirmRemove}
    onCancel={() => (deleteRequest = null)} />
{/if}

<style>
  .views-page {
    width: min(100%, 1240px);
    margin: 0 auto;
  }
  .new-view { padding: 9px 15px; }
  .views-list { background: var(--surface); }
  .shared-pill {
    padding: 2px 7px;
    border-radius: 999px;
    background: var(--tint);
    color: var(--accent);
    font-family: "Spline Sans Mono", ui-monospace, monospace;
    font-size: .6rem;
    font-weight: 700;
    letter-spacing: .04em;
    text-transform: uppercase;
    white-space: nowrap;
  }
  .loading-row {
    min-height: 68px;
    padding: 28px 18px;
    border-bottom: 1px solid var(--line);
  }
  .loading-row:last-child { border-bottom: 0; }
  .loading-row .skel { width: 48%; }
  .retry-views { margin-top: 10px; }
  .editor-error { margin: 14px 20px 0; }
  .builder-name {
    padding: 16px 20px;
    border-bottom: 1px solid var(--line);
  }
  .builder-name .input {
    width: min(100%, 430px);
    max-width: none;
  }
  .facet-error {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    margin-bottom: 12px;
  }
  .snapshot-note {
    display: flex;
    gap: 11px;
    max-width: 680px;
    padding: 13px;
    border: 1px solid color-mix(in srgb, var(--accent) 28%, var(--line));
    border-radius: 10px;
    background: var(--tint);
  }
  .snapshot-mark {
    display: grid;
    place-items: center;
    width: 34px;
    height: 34px;
    flex: none;
    border-radius: 9px;
    background: var(--surface);
    color: var(--accent);
  }
  .snapshot-note > span:last-child { display: flex; flex-direction: column; gap: 3px; }
  .snapshot-note strong { font-size: .82rem; }
  .snapshot-note small { color: var(--muted); font-size: .72rem; line-height: 1.45; }
  .view-filters { display: grid; gap: 6px; }
  .filter-row {
    display: flex;
    align-items: center;
    gap: 8px;
    min-height: 36px;
    flex-wrap: wrap;
  }
  .conjunction {
    width: 30px;
    flex: 0 0 30px;
    color: var(--muted);
    font-size: .76rem;
    text-align: right;
  }
  .field-token,
  .operator-token {
    width: auto;
    min-width: 0;
    min-height: 34px;
    padding: 8px 11px;
    border: 1px solid var(--line);
    border-radius: 7px;
    background: var(--surface);
    font-size: .75rem;
    line-height: 1.25;
  }
  .field-token { min-width: 104px; font-weight: 600; }
  .operator-token { color: var(--muted); }
  .filter-value {
    width: min(100%, 300px);
    max-width: 300px;
  }
  .search-value {
    font-family: "Spline Sans Mono", ui-monospace, monospace;
    font-size: .74rem;
  }
  .date-values {
    display: flex;
    align-items: center;
    gap: 7px;
    min-width: 0;
    flex-wrap: wrap;
  }
  .date-values > span { color: var(--muted); font-size: .72rem; }
  .date-role { width: auto; min-width: 135px; }
  .remove-filter {
    display: inline-grid;
    place-items: center;
    width: 28px;
    height: 28px;
    padding: 0;
    border: 0;
    border-radius: 6px;
    background: transparent;
    color: var(--muted);
    cursor: pointer;
  }
  .remove-filter:hover { background: var(--surface-2); color: var(--ink); }
  .add-filter-row {
    display: flex;
    align-items: center;
    gap: 6px;
    margin: 10px 0 0 38px;
  }
  .plus-mark { color: var(--accent); font-weight: 700; }
  .add-filter-select {
    padding: 2px 20px 2px 0;
    border: 0;
    background-color: transparent;
    color: var(--accent);
    font-size: .75rem;
    font-weight: 650;
    cursor: pointer;
  }
  .available-filter-hint { color: var(--muted); font-size: .69rem; }
  .share-heading {
    display: flex;
    align-items: flex-start;
    gap: 10px;
  }
  .share-heading .switch { margin-top: 2px; }
  .share-heading h3 { margin: 0; font-size: .82rem; }
  .share-heading p { margin: 3px 0 0; color: var(--muted); font-size: .74rem; }
  .footer-note {
    display: flex;
    align-items: center;
    gap: 14px;
    min-width: 0;
    color: var(--muted);
    font-size: .72rem;
  }
  .match-count b { color: var(--ink); }
  .delete-view {
    padding: 4px 0;
    border: 0;
    background: transparent;
    color: var(--danger, #b42318);
    font-size: .72rem;
    cursor: pointer;
  }
  .footer-actions {
    display: flex;
    align-items: center;
    gap: 12px;
    flex: none;
  }
  .save-view { min-width: 112px; justify-content: center; }

  @media (max-width: 760px) {
    .filter-row { align-items: flex-start; }
    .date-values { flex: 1; }
  }
  @media (max-width: 520px) {
    .builder-name { padding-left: 14px; padding-right: 14px; }
    .filter-row {
      display: grid;
      grid-template-columns: 28px minmax(0, 1fr) auto;
    }
    .conjunction { grid-column: 1; grid-row: 1; padding-top: 9px; }
    .field-token { grid-column: 2; }
    .operator-token,
    .filter-value,
    .date-values { grid-column: 2 / -1; width: 100%; max-width: none; }
    .remove-filter { grid-column: 3; grid-row: 1; }
    .available-filter-hint { display: none; }
    .add-filter-row { margin-left: 0; }
    .date-values { display: grid; grid-template-columns: minmax(0, 1fr); }
    .date-values :global(.iso-date-input) { flex: none; width: 100%; max-width: none; }
    .date-role { width: 100%; }
    .footer-note { align-items: flex-start; flex-direction: column; gap: 5px; }
    .footer-actions { justify-content: flex-end; }
  }
</style>
