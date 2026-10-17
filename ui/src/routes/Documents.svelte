<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "../lib/i18n.svelte.js";
  import { scopedHash as filingHref, systems, captureScope, scopeCurrent } from '../lib/systems.svelte.js'
  import { onDestroy, untrack } from 'svelte'
  import { listDocuments, listTags, listAllTags, listCorrespondents, patchDocument, deleteDocument, bulkEdit, createShareLink, thumbPath, decryptDocument, decryptBatch, extractIntelligence } from '../lib/api.js'
  import { route, go } from '../lib/router.svelte.js'
  import { uploadBus } from '../lib/upload_bus.svelte.js'
  import { SENSITIVITY_OPTIONS, fmtDate, formatNumber, isHighSensitivity, plural, sensDot } from '../lib/format.js'
  import { session } from '../lib/session.svelte.js'
  import { hasCapability } from '../lib/capabilities.js'
  import Icon from '../lib/Icon.svelte'
  import DocumentUnlockStatus from '../lib/DocumentUnlockStatus.svelte'
  import ConfirmDialog from '../lib/ConfirmDialog.svelte'
  import TypeaheadPicker from '../lib/TypeaheadPicker.svelte'
  import { createQueryAssistant } from '../lib/queryAssist.js'
  import { copyText } from '../lib/clipboard.js'
  import LinkQR from '../lib/LinkQR.svelte'
  import ShareLinkForm from '../lib/ShareLinkForm.svelte'
  import ISODateInput from '../lib/ISODateInput.svelte'

  let { notify, inbox = null, inboxMode = false, taxonomyLoaded = true, jdCategories = [],
        canAskArchive = false, canReviewIntelligence = false, onAskDocuments, onScopeChange } = $props()

  let docs = $state([])
  let count = $state(0)
  const requestedPage = $derived(Number(route.query.get('page')))
  const page = $derived(Number.isSafeInteger(requestedPage) && requestedPage > 0 ? requestedPage : 1)
  let loading = $state(true)
  let err = $state('')
  let tags = $state([]), correspondents = $state([])
  let facetError = $state('')
  const fQuery = $derived(route.query.get('q') || '')
  const fTag = $derived(route.query.get('tags__id__in') || '')
  const fCorr = $derived(route.query.get('correspondents__id__in') || '')
  const fCorrMissing = $derived(route.query.get('correspondents__isnull') === 'true')
  const fSens = $derived(route.query.get('sensitivity') || '')
  const fDocumentIDs = $derived(route.query.get('document_ids') || '')
  const fShareLink = $derived(route.query.get('share_link') || '')
  const ordering = $derived(route.query.get('ordering') || '-created_at')
  const dateFrom = $derived(dateFilterValue('created_at__gte'))
  const dateTo = $derived(dateFilterValue('created_at__lte'))
  let dateFilterOpen = $state(false)
  let mobileFiltersOpen = $state(false)
  let dateFilterBox = $state()
  let dateFilterTrigger = $state()
  const hasDateFilter = $derived(!!(dateFrom || dateTo))
  const dateRangeLabel = $derived(
    dateFrom && dateTo ? translate("documents.valueValue", { "dateFrom": dateFrom, "dateTo": dateTo }) :
    dateFrom ? translate("documents.fromValue", { "dateFrom": dateFrom }) :
    dateTo ? translate("documents.throughValue", { "dateTo": dateTo }) : translate("documents.anyDate")
  )
  const activeFilters = $derived.by(() => {
    const filters = []
    if (fTag) {
      filters.push({ key: 'tags__id__in', label: translate("documents.tagValue", { "fTag": tags.find(tag => String(tag.id) === fTag)?.name || fTag }) })
    }
    if (fCorrMissing) {
      filters.push({ key: 'correspondents__isnull', label: translate("documents.correspondentNone") })
    } else if (fCorr) {
      filters.push({ key: 'correspondents__id__in', label: translate("documents.correspondentValue", { "fCorr": correspondents.find(correspondent => String(correspondent.id) === fCorr)?.name || fCorr }) })
    }
    if (fShareLink === 'active') filters.push({ key: 'share_link', label: translate("documents.sharedByMe") })
    if (fSens) {
      filters.push({ key: 'sensitivity', label: SENSITIVITY_OPTIONS.find(option => option.value === fSens)?.label || fSens })
    }
    if (hasDateFilter) filters.push({ key: 'date', label: translate("documents.addedValue", { "dateRangeLabel": dateRangeLabel }) })
    return filters
  })
  let view = $state((() => { try { return localStorage.getItem('suchi.docs.view') || 'list' } catch { return 'list' } })())
  function setView(v) { view = v; try { localStorage.setItem('suchi.docs.view', v) } catch {} }
  const pageSize = 50
  const isInbox = $derived(inboxMode)
  const jdFilter = $derived(route.query.get('jd') || '')
  const canShareLinks = $derived(hasCapability(session.user, 'share_links'))
  const publicShareAvailable = $derived(!!session.user?.public_share_available)
  let loadVersion = 0
  let disposed = false
  let activeController // cancel superseded filters, not only their UI updates
  let suggestions = $state([])
  const queryAssistant = createQueryAssistant((next) => (suggestions = next))
  onDestroy(() => {
    disposed = true
    loadVersion++
    bulkTagVersion++
    bulkShareVersion++
    queryAssistant.dispose()
    activeController?.abort()
  })

  function setRouteFilter(key, value) {
    queryAssistant.clear()
    const params = new URLSearchParams(route.query)
    const normalized = String(value ?? '').trim()
    if (normalized) params.set(key, normalized)
    else params.delete(key)
    params.delete('page')
    const query = params.toString()
    go(`#${route.path}${query ? `?${query}` : ''}`)
  }

  function toggleSharedByMe() {
    setRouteFilter('share_link', fShareLink === 'active' ? '' : 'active')
  }

  function setCorrespondentFilter(value) {
    queryAssistant.clear()
    const params = new URLSearchParams(route.query)
    params.delete('correspondents__id__in')
    params.delete('correspondents__isnull')
    if (value === '__missing__') params.set('correspondents__isnull', 'true')
    else if (value) params.set('correspondents__id__in', value)
    params.delete('page')
    const query = params.toString()
    go(`#${route.path}${query ? `?${query}` : ''}`)
  }

  function pageHash(target) {
    const params = new URLSearchParams(route.query)
    if (target > 1) params.set('page', String(target))
    else params.delete('page')
    const query = params.toString()
    return `#${route.path}${query ? `?${query}` : ''}`
  }

  function dateFilterValue(key) {
    const value = route.query.get(key)
    if (!value) return ''
    const date = new Date(Number(value) * 1000)
    return Number.isFinite(date.getTime()) ? date.toISOString().slice(0, 10) : ''
  }

  function setDateFilter(key, value) {
    const timestamp = value ? Math.floor(new Date(value).getTime() / 1000) : ''
    setRouteFilter(key, timestamp === '' ? '' : timestamp + (key === 'created_at__lte' ? 86399 : 0))
  }

  function clearDateFilters() {
    queryAssistant.clear()
    const params = new URLSearchParams(route.query)
    params.delete('created_at__gte')
    params.delete('created_at__lte')
    params.delete('page')
    dateFilterOpen = false
    const query = params.toString()
    go(`#${route.path}${query ? `?${query}` : ''}`)
  }

  function clearDocumentFilter(key) {
    if (key === 'date') {
      clearDateFilters()
      return
    }
    setRouteFilter(key, '')
  }

  function clearDocumentFilters() {
    queryAssistant.clear()
    const params = new URLSearchParams(route.query)
    for (const key of ['tags__id__in', 'correspondents__id__in', 'correspondents__isnull', 'share_link', 'sensitivity', 'created_at__gte', 'created_at__lte']) {
      params.delete(key)
    }
    params.delete('page')
    dateFilterOpen = false
    mobileFiltersOpen = false
    const query = params.toString()
    go(`#${route.path}${query ? `?${query}` : ''}`)
  }

  function toggleMobileFilters() {
    mobileFiltersOpen = !mobileFiltersOpen
    if (!mobileFiltersOpen) dateFilterOpen = false
  }

  function closeDateFilter() {
    dateFilterOpen = false
    dateFilterTrigger?.focus()
  }

  function closeDateFilterOutside(event) {
    if (dateFilterOpen && dateFilterBox && !dateFilterBox.contains(event.target)) {
      dateFilterOpen = false
    }
  }

  async function loadFacets() {
    facetError = ''
    try {
      const [t, c] = await Promise.all([listTags(), listCorrespondents()])
      tags = t?.results || []; correspondents = c?.results || []
    } catch (ex) { facetError = ex.message || translate("documents.someDocumentFiltersCouldNotBeLoaded") }
  }

  async function load({ background = false } = {}) {
    const version = ++loadVersion
    activeController?.abort()
    activeController = undefined
    if (!background) loading = true
    err = ''
    if (isInbox && !inbox?.id) {
      docs = []
      count = 0
      loading = !taxonomyLoaded
      if (taxonomyLoaded) err = translate("documents.theInboxIsUnavailable")
      return
    }
    const controller = new AbortController()
    activeController = controller
    try {
      const params = {
        page, page_size: pageSize, ordering,
        q: fQuery, document_ids: fDocumentIDs,
        tags__id__in: fTag, correspondents__id__in: fCorr,
        correspondents__isnull: fCorrMissing ? 'true' : '',
        sensitivity: fSens, jd_category_id: isInbox ? inbox?.id : jdFilter,
        created_at__gte: dateFrom ? Math.floor(new Date(dateFrom) / 1000) : '',
        created_at__lte: dateTo ? Math.floor(new Date(dateTo) / 1000) + 86399 : '',
        share_link: fShareLink,
      }
      const csvIDs = (value) => [...new Set(String(value || '').split(',')
        .map(item => Number(item.trim())).filter(item => Number.isInteger(item) && item > 0))]
      onScopeChange?.({
        label: isInbox ? translate("documents.inboxDocuments") : translate("documents.documentsShownHere"),
        query: params.q || '',
        document_ids: csvIDs(params.document_ids),
        jd_category_id: Number(params.jd_category_id) || 0,
        sensitivity: params.sensitivity || '',
        tag_ids: csvIDs(params.tags__id__in),
        correspondent_ids: csvIDs(params.correspondents__id__in),
        correspondent_missing: params.correspondents__isnull === 'true',
        created_at_gte: params.created_at__gte === '' ? null : params.created_at__gte,
        created_at_lte: params.created_at__lte === '' ? null : params.created_at__lte,
        language: '',
        share_link: params.share_link || '',
      })
      const res = await listDocuments(params, controller.signal)
      if (version !== loadVersion) return
      docs = res?.results || []
      count = res?.count ?? docs.length
      const lastPage = Math.max(1, Math.ceil(count / pageSize))
      if (page > lastPage || (route.query.has('page') && requestedPage !== page)) {
        go(pageHash(Math.min(page, lastPage)), { replace: true })
        return
      }
      const visibleIDs = new Set(docs.map((document) => document.id))
      const visibleSelection = new Set([...sel].filter((id) => visibleIDs.has(id)))
      if (sel.size && !visibleSelection.size) clearSel()
      else sel = visibleSelection
    } catch (ex) {
      if (version === loadVersion) err = ex.message || translate("documents.couldNotLoadDocuments")
    } finally {
      if (activeController === controller) activeController = undefined
      if (version === loadVersion) loading = false
    }
  }

  async function fileTo(doc, jdId) {
    try {
      await patchDocument(doc.id, { jd_category_id: Number(jdId) })
      await load()
      notify?.(translate("documents.filed"))
    } catch (ex) { notify?.(ex.message || translate("documents.couldNotFileIt")) }
  }
  let trashRequest = $state(null)
  let trashBusy = $state(false)

  async function confirmTrash() {
    const request = trashRequest
    if (!request) return
    trashBusy = true
    if (request.kind === 'bulk') {
      await bulk(translate("documents.trashed"), 'delete', {})
      trashRequest = null
    } else {
      try {
        await deleteDocument(request.doc.id)
        await load()
        trashRequest = null
        notify?.(translate("documents.trashed"))
      } catch (ex) { notify?.(ex.message || translate("documents.couldNotTrashIt")) }
    }
    trashBusy = false
  }

  // ---- selection + bulk actions ----
  let sel = $state(new Set())
  const selectedHighSensitivity = $derived(docs.some(doc => sel.has(doc.id) && isHighSensitivity(doc.sensitivity)))
  let lastIdx = $state(-1)      // anchor for shift-range and j/k cursor
  let bulkBusy = $state(false)
  let shareURL = $state('')
  let bulkShareOpen = $state(false)
  let bulkShareInput = $state({ visibility: 'network', expiry: '0', password: '' })
  let bulkShareVersion = 0
  let bulkTagOpen = $state(false)
  let bulkTags = $state([])
  let bulkTagLoading = $state(false)
  let bulkTagLoadError = $state('')
  let bulkTagError = $state('')
  let bulkTagVersion = 0
  let bulkTagSaving = false

  function closeBulkTags() {
    bulkTagVersion++
    if (bulkTagSaving) { bulkTagSaving = false; bulkBusy = false }
    bulkTagOpen = false
    bulkTagLoading = false
    bulkTagLoadError = ''
    bulkTagError = ''
    bulkTags = []
  }

  function bulkTagCurrent(version, scope, path, query, user) {
    return !disposed && version === bulkTagVersion && bulkTagOpen &&
      scopeCurrent(scope) && route.path === path && route.query.toString() === query &&
      session.user === user
  }

  async function openBulkTags() {
    if (bulkBusy || !sel.size || isInbox) return
    if (bulkTagOpen && !bulkTagLoadError) { closeBulkTags(); return }
    bulkTagOpen = true
    bulkTagLoading = true
    bulkTagLoadError = ''
    bulkTagError = ''
    const version = ++bulkTagVersion
    const scope = captureScope(), path = route.path, query = route.query.toString(), user = session.user
    try {
      const available = await listAllTags()
      if (bulkTagCurrent(version, scope, path, query, user)) bulkTags = available
    } catch (ex) {
      if (bulkTagCurrent(version, scope, path, query, user))
        bulkTagLoadError = ex.message || translate("documents.couldNotLoadTags")
    } finally {
      if (bulkTagCurrent(version, scope, path, query, user)) bulkTagLoading = false
    }
  }

  async function chooseBulkTag(tag) {
    if (bulkBusy || !bulkTagOpen || !sel.size) return false
    const ids = [...sel]
    const version = bulkTagVersion
    const scope = captureScope(), path = route.path, query = route.query.toString(), user = session.user
    bulkTagError = ''
    bulkTagSaving = true
    bulkBusy = true
    try {
      const res = await bulkEdit(ids, 'add_tag', { tag_id: tag.id })
      if (!bulkTagCurrent(version, scope, path, query, user)) return false
      const successful = new Set((res?.results || []).filter(row => row.ok).map(row => row.id))
      const applied = ids.filter(id => successful.has(id)).length
      const failed = ids.length - applied
      if (failed) {
        bulkTagError = plural("documents.addedTagCountFailed", applied, { name: tag.name, failed })
        await load({ background: true })
        return false
      }
      clearSel()
      notify?.(plural("documents.addedTagCount", applied, { name: tag.name }))
      await load({ background: true })
      return true
    } catch (ex) {
      if (bulkTagCurrent(version, scope, path, query, user))
        bulkTagError = ex.message || translate("documents.couldNotAddTheTagRetryTheSelection")
      return false
    } finally {
      if (version === bulkTagVersion && bulkTagSaving) {
        bulkTagSaving = false
        bulkBusy = false
      }
    }
  }

  function toggleSel(i, ev) {
    if (bulkBusy) return
    const d = docs[i]
    if (!d) return
    const next = new Set(sel)
    if (ev?.shiftKey && lastIdx >= 0) {
      const [a, b] = [Math.min(lastIdx, i), Math.max(lastIdx, i)]
      for (let k = a; k <= b; k++) next.add(docs[k].id)
    } else {
      next.has(d.id) ? next.delete(d.id) : next.add(d.id)
    }
    if (!next.size) clearSel()
    else { sel = next; lastIdx = i }
  }
  function clearSel() {
    sel = new Set()
    lastIdx = -1
    closeBulkTags()
    if (!bulkBusy) bulkShareOpen = false
  }

  async function bulk(label, method, parameters) {
    bulkBusy = true
    try {
      const res = await bulkEdit([...sel], method, parameters)
      const failed = (res?.results || []).filter(r => !r.ok).length
      notify?.(failed ? translate("documents.valueValueDoneValueFailed", { "label": label, "applied": res.applied, "failed": failed }) : plural("documents.bulkResultCount", res?.applied ?? sel.size, { label }))
    } catch (ex) { notify?.(ex.message || translate("documents.valueFailed", { "label": label })) }
    bulkBusy = false
    clearSel()
    load()
  }
  const bulkRefile = (jdId) => bulk(translate("documents.refiled"), 'set_jd_category', { jd_category_id: Number(jdId) })
  const bulkSens = (s) => bulk(translate("documents.sensitivitySet"), 'set_sensitivity', { sensitivity: s })
  const bulkCorrespondent = (id) => bulk(translate("documents.correspondentSet"), 'set_correspondent', { correspondent_id: Number(id) })
  const bulkRescan = () => bulk(translate("documents.rescanStarted"), 'rescan_enqueue', {})
  const bulkTrash = () => (trashRequest = { kind: 'bulk', count: sel.size })
  function openBulkShare() {
    if (bulkBusy || !sel.size) return
    bulkShareVersion++
    bulkShareInput = { visibility: 'network', expiry: '0', password: '' }
    bulkShareOpen = true
  }

  function closeBulkShare() {
    bulkShareVersion++
    bulkShareOpen = false
    bulkShareInput = { visibility: 'network', expiry: '0', password: '' }
    bulkBusy = false
  }

  async function bulkShare() {
    const user = session.user
    const ids = [...sel]
    const version = bulkShareVersion
    bulkBusy = true
    try {
      const res = await createShareLink({
        doc_ids: ids,
        label: translate("documents.selectionOfValue", { "length": ids.length }),
        visibility: bulkShareInput.visibility,
        expires_in_sec: Number(bulkShareInput.expiry),
        password: bulkShareInput.password,
      })
      if (disposed || !bulkShareOpen || session.user !== user || version !== bulkShareVersion) return
      shareURL = res?.public_url || `${location.origin}/s/${res?.token}`
      bulkShareOpen = false
      bulkShareInput = { visibility: 'network', expiry: '0', password: '' }
      await copyShareLink()
    } catch (ex) {
      if (!disposed && bulkShareOpen && session.user === user && version === bulkShareVersion) {
        notify?.(ex.message || translate("documents.couldNotCreateTheBundle"))
      }
    } finally {
      if (!disposed && session.user === user && version === bulkShareVersion) bulkBusy = false
    }
  }

  async function copyShareLink() {
    const copied = await copyText(shareURL)
    notify?.(copied ? translate("documents.shareLinkCopied") : translate("documents.shareLinkReadySelectTheLinkAndCopy"))
  }

  async function bulkExtractDates() {
    const ids = [...sel]
    bulkBusy = true
    try {
      const result = await extractIntelligence({ document_ids: ids, types: ['date'] })
      const failed = (result?.results || []).filter(item => !item.ok).length
      notify?.(failed
        ? translate("documents.dateReadingPartial", { applied: result?.applied || 0, failed })
        : plural("documents.dateReadingStartedCount", result?.applied || ids.length))
      clearSel()
    } catch (ex) {
      notify?.(ex.message || translate("documents.couldNotStartDateReading"))
    } finally {
      bulkBusy = false
    }
  }

  // The postingest retry fills metadata after this optimistic unlock state.
  let unlockPw = $state({})           // { [id]: string }
  let unlockErr = $state({})          // { [id]: string }
  let unlockBusy = $state(new Set())  // ids in flight
  async function unlockOne(d) {
    const password = (unlockPw[d.id] || '').trim()
    if (!password) return
    unlockBusy = new Set([...unlockBusy, d.id])
    unlockErr = { ...unlockErr, [d.id]: '' }
    try {
      await decryptDocument(d.id, { password, remember: true })
      d.encryption_state = 'decrypted'
      unlockPw = { ...unlockPw, [d.id]: '' }
      notify?.(translate("documents.unlockedValue", { "id": d.title || 'document #' + d.id }))
    } catch (ex) {
      const code = ex?.code || ''
      unlockErr = { ...unlockErr, [d.id]: code === 'bad_password' ? translate("documents.wrongPassword") : (ex.message || translate("documents.unlockFailed")) }
    } finally {
      const next = new Set(unlockBusy); next.delete(d.id); unlockBusy = next
    }
  }

  let bulkDecOpen = $state(false)
  let bulkDecPw = $state('')
  let bulkDecBusy = $state(false)
  let bulkDecInput = $state()
  $effect(() => { if (bulkDecOpen && bulkDecInput) bulkDecInput.focus() })
  const anyLockedSelected = $derived(
    [...sel].some(id => docs.find(d => d.id === id)?.encryption_state === 'encrypted')
  )
  async function runBulkDecrypt() {
    const password = bulkDecPw.trim()
    if (!password) return
    bulkDecBusy = true
    try {
      const res = await decryptBatch({ password, remember: true, doc_ids: [...sel] })
      const results = res?.results || []
      const opened = results.filter(r => r.ok).length
      const tried = results.length || sel.size
      notify?.(opened ? translate("documents.unlockedValueOfValue", { "opened": opened, "tried": tried }) : translate("documents.thatPasswordOpenedNothing"))
      bulkDecPw = ''
      bulkDecOpen = false
      clearSel()
      load()
    } catch (ex) {
      notify?.(ex.message || translate("documents.batchUnlockFailed"))
    } finally { bulkDecBusy = false }
  }

  // ---- keyboard: j/k move, x select, Enter open ----
  function onKey(e) {
    // Handle modal and popover Escape before ignoring focused inputs.
    if (bulkDecOpen && e.key === 'Escape') { bulkDecOpen = false; return }
    if (dateFilterOpen && e.key === 'Escape') { e.preventDefault(); closeDateFilter(); return }
    if (e.target.closest('input,select,textarea,button,a') || e.metaKey || e.ctrlKey) return
    if (e.key === 'j' || e.key === 'k') {
      e.preventDefault()
      lastIdx = Math.min(Math.max(lastIdx + (e.key === 'j' ? 1 : -1), 0), docs.length - 1)
      document.querySelector(`[data-row="${lastIdx}"]`)?.scrollIntoView({ block: 'nearest' })
    } else if (e.key === 'x' && lastIdx >= 0) {
      e.preventDefault(); toggleSel(lastIdx)
    } else if (e.key === 'Enter' && lastIdx >= 0 && docs[lastIdx]) {
      go(`#/doc/${docs[lastIdx].id}`)
    }
  }

  loadFacets()
  $effect(() => {
    uploadBus.revision
    systems.generation
    route.path
    route.query.toString()
    // clearSel reads bulk state; tracking it would re-clear the selection
    // whenever a bulk edit finishes.
    untrack(clearSel)
    load()
  })
  $effect(() => {
    const fn = () => { if (document.visibilityState === 'visible') load({ background: true }) }
    document.addEventListener('visibilitychange', fn)
    return () => document.removeEventListener('visibilitychange', fn)
  })

  const pages = $derived(Math.max(1, Math.ceil(count / pageSize)))
  const pageNumbers = $derived.by(() => {
    if (pages <= 5) return Array.from({ length: pages }, (_, index) => index + 1)
    const start = Math.max(1, Math.min(page - 1, pages - 2))
    return [...new Set([1, start, Math.min(start + 1, pages), Math.min(start + 2, pages), pages])]
      .sort((a, b) => a - b)
  })
</script>

<svelte:window onkeydown={onKey} onmousedown={closeDateFilterOutside} />

{#if sel.size > 0}
  <div class="bulkbar">
    <b>{translate("documents.valueSelected", { "size": sel.size })}</b>
    <select class="input" style="max-width:190px" disabled={bulkBusy}
            onchange={(e) => e.target.value && bulkRefile(e.target.value)}>
      <option value="">{translate("documents.fileUnder")}</option>
      {#each jdCategories as c}<option value={c.id}>{c.code} {c.name}</option>{/each}
    </select>
    <select class="input" style="max-width:160px" disabled={bulkBusy}
            onchange={(e) => bulkSens(e.target.value)}>
      <option value="" selected disabled>{translate("documents.sensitivity")}</option>
      {#each SENSITIVITY_OPTIONS as option (option.value)}
        <option value={option.value}>{option.label}</option>
      {/each}
    </select>
    {#if !isInbox}
      <button class="btn sm" disabled={bulkBusy} aria-expanded={bulkTagOpen}
              onclick={openBulkTags}>{translate("documents.addTags")}</button>
      <select class="input" style="max-width:190px" aria-label={translate("documents.setCorrespondentForSelection")}
              disabled={bulkBusy} onchange={(event) => event.currentTarget.value !== '' && bulkCorrespondent(event.currentTarget.value)}>
        <option value="" selected disabled>{translate("documents.correspondent")}</option>
        <option value="0">{translate("documents.clearSender")}</option>
        {#each correspondents as correspondent}
          <option value={correspondent.id}>{correspondent.name}</option>
        {/each}
      </select>
    {/if}
    {#if canAskArchive}
      <button class="btn sm" disabled={bulkBusy} onclick={() => onAskDocuments?.([...sel])}>
        <Icon name="ask" size={12} /> {translate("documents.askSelection")}
      </button>
    {/if}
    {#if canReviewIntelligence}
      <button class="btn sm" disabled={bulkBusy} onclick={bulkExtractDates} title={translate("documents.extractDatesForReviewUsingTheConfigured")}>
        <Icon name="calendar" size={12} /> {translate("documents.extractDates")}
      </button>
    {/if}
    {#if canShareLinks}
      <button class="btn sm" disabled={bulkBusy} onclick={openBulkShare}><Icon name="link" size={12} /> {translate("documents.share")}</button>
    {/if}
    {#if anyLockedSelected}
      <button class="btn sm" disabled={bulkBusy} onclick={() => (bulkDecOpen = true)}
              title={translate("documents.tryOnePasswordAgainstEveryEncryptedDoc")}>
        <Icon name="lock" size={12} /> {translate("documents.decrypt")}
      </button>
    {/if}
    <button class="btn sm danger" disabled={bulkBusy} onclick={bulkTrash}>{translate("documents.trash")}</button>
    <span class="spacer"></span>
    {#if bulkBusy}<span class="sub">{translate("documents.working")}</span>{/if}
    <button class="btn sm" disabled={bulkBusy} onclick={bulkRescan} title={translate("documents.readAndProcessTheSelectedDocumentsAgain")}>
      <Icon name="zap" size={12} /> {translate("documents.rescan")}
    </button>
    <button class="btn sm" disabled={bulkBusy} onclick={clearSel}>{translate("documents.clear")}</button>
    {#if bulkTagOpen && !isInbox}
      <div class="bulk-tag-editor" role="group" aria-label={translate("documents.addTagsToSelectedDocuments")}>
        {#if bulkTagLoading}
          <span class="sub">{translate("documents.loadingTags")}</span>
        {:else if bulkTagLoadError}
          <span class="err" role="alert">{bulkTagLoadError}</span>
          <button class="btn sm" disabled={bulkBusy} onclick={openBulkTags}>{translate("documents.retryTags")}</button>
        {:else}
          <TypeaheadPicker items={bulkTags} disabled={bulkBusy} label={translate("documents.tagForSelection")}
                           menuId="bulk-tag-options" placeholder={translate("tagPicker.typeToSelectATag")}
                           emptyText={translate("tagPicker.noMatchingTags")} onChoose={chooseBulkTag} />
        {/if}
        {#if bulkTagError}<span class="err" role="alert">{bulkTagError}</span>{/if}
      </div>
    {/if}
  </div>
{/if}

{#if bulkShareOpen}
  <div class="modal-veil" onclick={closeBulkShare} role="presentation">
    <div class="modal" style="width:min(560px,94vw)" onclick={(event) => event.stopPropagation()}
         onkeydown={(event) => { if (event.key === 'Escape') closeBulkShare() }}
         role="dialog" aria-label={translate("documents.shareSelectedDocuments")} tabindex="-1">
      <div class="modal-head">
        <h3>{plural("documents.shareDocumentCount", sel.size)}</h3>
        <button class="btn sm" onclick={closeBulkShare} title={translate("common.close")} aria-label={translate("documents.closeSharing")}>
          <Icon name="x" size={13} />
        </button>
      </div>
      <p class="sub" style="margin:0 0 12px">{translate("documents.oneLinkWillContainEverySelectedDocument")}</p>
      <ShareLinkForm bind:value={bulkShareInput} publicAvailable={publicShareAvailable}
                     highSensitivity={selectedHighSensitivity} busy={bulkBusy} onSubmit={bulkShare} />
    </div>
  </div>
{/if}

{#if shareURL}
  <div class="card" role="group" aria-label={translate("documents.createdShareLink")}>
    <div class="toolbar">
    <input class="input mono" style="flex:1;min-width:0" aria-label={translate("documents.shareLink")} readonly value={shareURL}
           onclick={(event) => event.currentTarget.select()} />
    <button class="btn sm" onclick={copyShareLink}>{translate("documents.copyLink")}</button>
    <button class="btn sm" aria-label={translate("documents.dismissShareLink")} onclick={() => (shareURL = '')}><Icon name="x" size={13} /></button>
    </div>
    <LinkQR url={shareURL} />
  </div>
{/if}

{#if err}<div class="err">{err}</div>{/if}

{#if !isInbox}
  <div class="document-controls" onchangecapture={(e) => { if (e.target.matches('select')) e.target.blur() }}>
    <div class="toolbar document-search-toolbar" role="group" aria-label={translate("documents.documentSearch")}>
      <input class="input" type="search" value={fQuery} placeholder={translate("documents.searchOrUseJdTagFrom")}
             list="documents-query-suggestions"
             oninput={(event) => queryAssistant.update(event.currentTarget.value)}
             onchange={(e) => setRouteFilter('q', e.target.value)}
             onkeydown={(e) => { if (e.key === 'Enter') { e.preventDefault(); setRouteFilter('q', e.currentTarget.value) } }}
             style="min-width:180px" />
      <datalist id="documents-query-suggestions">
        {#each suggestions as suggestion (suggestion.query)}
          <option value={suggestion.query}>{suggestion.value}</option>
        {/each}
      </datalist>
    </div>
    <div class="mobile-filter-bar">
      <button type="button" class="btn sm" aria-expanded={mobileFiltersOpen}
              aria-controls="document-filter-panel" onclick={toggleMobileFilters}>
        <Icon name="settings" size={14} />
        {activeFilters.length ? translate("documents.filtersValue", { "length": activeFilters.length }) : translate("documents.filters")}
      </button>
    </div>
    <div class="toolbar document-toolbar document-options-toolbar" role="group" aria-label={translate("documents.documentOptions")}>
      <div id="document-filter-panel" class="document-filter-controls" class:mobile-open={mobileFiltersOpen}>
        <select class="input" aria-label={translate("documents.filterByTag")}
                onchange={(e) => setRouteFilter('tags__id__in', e.target.value)}>
          <option value="" selected={!fTag}>{translate("documents.allTags")}</option>
          {#each tags as t}<option value={t.id} selected={String(t.id) === fTag}>{t.name}</option>{/each}
        </select>
        <select class="input" aria-label={translate("documents.filterByCorrespondent")}
                onchange={(event) => setCorrespondentFilter(event.currentTarget.value)}>
          <option value="" selected={!fCorr && !fCorrMissing}>{translate("documents.allCorrespondents")}</option>
          <option value="__missing__" selected={fCorrMissing}>{translate("documents.noCorrespondent")}</option>
          {#each correspondents as correspondent}
            <option value={correspondent.id} selected={String(correspondent.id) === fCorr}>{correspondent.name}</option>
          {/each}
        </select>
        <button type="button" class="btn sm document-share-filter" class:primary={fShareLink === 'active'}
                aria-pressed={fShareLink === 'active'} onclick={toggleSharedByMe}>
          <Icon name="link" size={14} /> {translate("documents.sharedByMe")}
        </button>
        <select class="input" aria-label={translate("documents.filterBySensitivity")} value={fSens}
                onchange={(e) => setRouteFilter('sensitivity', e.target.value)}>
          <option value="">{translate("documents.anySensitivity")}</option>
          {#each SENSITIVITY_OPTIONS as option (option.value)}
            <option value={option.value}>{option.label}</option>
          {/each}
        </select>
        <div class="date-range-filter" bind:this={dateFilterBox}>
          <button bind:this={dateFilterTrigger} type="button" class="date-range-trigger" class:on={hasDateFilter}
                  aria-expanded={dateFilterOpen} aria-controls="document-date-range"
                  onclick={() => (dateFilterOpen = !dateFilterOpen)}>
            <span>{dateRangeLabel}</span><Icon name="calendar" size={15} />
          </button>
          {#if dateFilterOpen}
            <div id="document-date-range" class="date-range-menu" role="group" aria-label={translate("documents.addedDateRange")}>
              <div class="date-range-fields">
                <label class="date-range-field"><span>{translate("documents.from")}</span>
                  <ISODateInput value={dateFrom} label={translate("documents.addedOnOrAfter")}
                                onchange={(value) => setDateFilter('created_at__gte', value)} />
                </label>
                <label class="date-range-field"><span>{translate("documents.to")}</span>
                  <ISODateInput value={dateTo} label={translate("documents.addedOnOrBefore")}
                                onchange={(value) => setDateFilter('created_at__lte', value)} />
                </label>
              </div>
              <div class="date-range-actions">
                <button class="btn sm" type="button" disabled={!hasDateFilter} onclick={clearDateFilters}>{translate("documents.clearDates")}</button>
                <button class="btn primary sm" type="button" onclick={closeDateFilter}>{translate("documents.done")}</button>
              </div>
            </div>
          {/if}
        </div>
      </div>
      <span class="document-display-actions" role="group" aria-label={translate("documents.documentDisplay")}>
        <span class="seg">
          <button class:on={view === 'list'} onclick={() => setView('list')}>{translate("documents.list")}</button>
          <button class:on={view === 'grid'} onclick={() => setView('grid')}>{translate("documents.grid")}</button>
        </span>
        <button class="btn sm document-refresh" onclick={load} title={translate("documents.refresh")} aria-label={translate("documents.refreshDocuments")}><Icon name="refresh" size={13} /></button>
        <select class="input" aria-label={translate("documents.sortDocuments")} value={ordering} onchange={(e) => setRouteFilter('ordering', e.target.value === '-created_at' ? '' : e.target.value)}>
          <option value="-created_at">{translate("documents.newestFirst")}</option>
          <option value="created_at">{translate("documents.oldestFirst")}</option>
          <option value="title">{translate("documents.titleAZ")}</option>
          <option value="-title">{translate("documents.titleZA")}</option>
        </select>
      </span>
    </div>
  </div>
  {#if facetError}
    <div class="err" style="display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:12px">
      <span>{facetError}</span><button class="btn sm" onclick={loadFacets}>{translate("documents.retryFilters")}</button>
    </div>
  {/if}
  {#if activeFilters.length}
    <div class="mobile-filter-chips" role="group" aria-label={translate("documents.activeDocumentFilters")}>
      {#each activeFilters as filter (filter.key)}
        <button type="button" class="filter-chip" aria-label={translate("documents.removeValueFilter", { "label": filter.label })}
                onclick={() => clearDocumentFilter(filter.key)}>
          <span>{filter.label}</span><Icon name="x" size={11} />
        </button>
      {/each}
      <button type="button" class="btn sm clear-filter-chips" onclick={clearDocumentFilters}>{translate("documents.clearAll")}</button>
    </div>
  {/if}
{/if}

{#if loading}
  <div class="index">{#each Array(6) as _}<div class="irow"><div class="skel" style="width:60%"></div></div>{/each}</div>
{:else if docs.length === 0}
  <div class="empty">
    <Icon name={isInbox ? 'inbox' : 'docs'} size={56} />
    {#if isInbox}
      <b>{translate("documents.inboxZero")}</b><span>{translate("documents.everythingIsFiledNewDocumentsThatNeed")}</span>
    {:else if fShareLink === 'active'}
      <b>{translate("documents.noActivelySharedDocuments")}</b><span>{translate("documents.documentsAppearHereWhileOneOfYour")}</span>
    {:else}
      <b>{translate("documents.noDocumentsMatch")}</b><span>{translate("documents.clearAFilterOr")} <a href={filingHref("#/upload")}>{translate("documents.uploadTheFirstOne")}</a>.</span>
    {/if}
  </div>
{:else}
  {#if view === 'grid' && !isInbox}
    <div class="dgrid">
      {#each docs as d, i (d.id)}
        <article class="card gcard document-card" class:selected={sel.has(d.id)}>
          <span class="gthumb" class:blurred={isHighSensitivity(d.sensitivity)}><img src={thumbPath(d.id, isHighSensitivity(d.sensitivity))} alt="" loading="lazy" onerror={(e) => e.target.closest('.gthumb').classList.add('none')} /></span>
          <span class="gmeta">
            <input type="checkbox" class="rowcheck" checked={sel.has(d.id)} disabled={bulkBusy}
                   onchange={(e) => toggleSel(i, e)}
                   aria-label={translate("documents.selectValue", { "id": d.title || 'document ' + d.id })} />
            <a class="grid-document-link" href={filingHref(`#/doc/${d.id}`)}
               aria-label={d.title || translate("documents.documentValue", { "id": d.id })}>
              <span class="grid-document-title">
                {#if d.jd_address || d.jd_category_code}<span class="chip">{d.jd_address || d.jd_category_code}</span>{/if}
                {#if d.archive_serial_number != null}<span class="chip">{translate("common.asnValue", { value: String(d.archive_serial_number) })}</span>{/if}
                <span class="title">{d.title || translate("documents.documentValue2", { "id": d.id })}</span>
                <DocumentUnlockStatus state={d.encryption_state} />
              </span>
              <span class="sub">{fmtDate(d.created_at)}</span>
            </a>
          </span>
        </article>
      {/each}
    </div>
  {:else}
  <div class="index">
    {#each docs as d, i (d.id)}
      <div class="irow hoverable document-row" data-row={i} class:cursor={i === lastIdx} class:selected={sel.has(d.id)}>
        <input type="checkbox" class="rowcheck" checked={sel.has(d.id)} disabled={bulkBusy}
               onchange={(e) => toggleSel(i, e)}
               aria-label={translate("documents.selectValue", { "id": d.title || 'document ' + d.id })} />
        <a class="document-row-link" href={filingHref(`#/doc/${d.id}`)}
           aria-label={d.title || translate("documents.documentValue", { "id": d.id })}>
          <span class="rthumb" class:blurred={isHighSensitivity(d.sensitivity)}><img src={thumbPath(d.id, isHighSensitivity(d.sensitivity))} alt="" loading="lazy" onerror={(e) => e.target.closest('.rthumb').classList.add('none')} /></span>
          <span class="dot {sensDot(d.sensitivity)}" class:accent={!d.sensitivity}></span>
          {#if d.jd_address || d.jd_category_code}<span class="chip" title={translate("documents.valueValue2", { "jd_category_name": d.jd_category_name, "jd_area_name": d.jd_area_name })}>{d.jd_address || d.jd_category_code}</span>{/if}
          {#if d.archive_serial_number != null}<span class="chip">{translate("common.asnValue", { value: String(d.archive_serial_number) })}</span>{/if}
          <span class="title grow">{d.title || translate("documents.documentValue2", { "id": d.id })}</span>
          <DocumentUnlockStatus state={d.encryption_state} />
          {#if d.tags?.length}
            {#each d.tags.slice(0, 3) as t}<span class="pill">{t}</span>{/each}
          {/if}
        </a>
        <span class="end">
          {#if d.encryption_state === 'encrypted'}
            <span class="unlock-inline" role="group" aria-label={translate("documents.unlockValue", { "id": d.title || 'document ' + d.id })}>
              <span class="pill warn" title={translate("documents.thisDocIsEncryptedUnlockToMake")}>
                <Icon name="lock" size={11} /> {translate("documents.locked")}
              </span>
              <input type="password" class="input" style="padding:3px 8px;font-size:.76rem;max-width:160px"
                     placeholder={translate("documents.password")}
                     bind:value={unlockPw[d.id]}
                     disabled={unlockBusy.has(d.id)}
                     onkeydown={(e) => { if (e.key === 'Enter') { e.preventDefault(); unlockOne(d) } }}
                     aria-label={translate("documents.passwordForValue", { "id": d.title || 'document ' + d.id })} />
              <button class="btn sm primary" disabled={unlockBusy.has(d.id) || !unlockPw[d.id]}
                      onclick={() => unlockOne(d)}>
                {unlockBusy.has(d.id) ? '…' : translate("documents.unlock")}
              </button>
              {#if unlockErr[d.id]}<span class="sub" style="color:var(--danger)">{unlockErr[d.id]}</span>{/if}
            </span>
          {:else if isInbox && jdCategories.length}
            <select class="input" style="padding:3px 8px;font-size:.76rem;max-width:150px"
                    aria-label={translate("documents.fileValueUnder", { "id": d.title || 'document ' + d.id })}
                    onchange={(e) => { if (e.target.value) fileTo(d, e.target.value) }}>
              <option value="">{translate("documents.fileUnder")}</option>
              {#each jdCategories as c}<option value={c.id}>{c.code} {c.name}</option>{/each}
            </select>
          {:else}
            <span class="hoveracts" role="group" aria-label={translate("documents.quickActionsForValue", { "id": d.title || 'document ' + d.id })}>
              <select class="input" style="padding:2px 6px;font-size:.72rem;max-width:130px"
                      aria-label={translate("documents.refileValue", { "id": d.title || 'document ' + d.id })}
                      onchange={(e) => { if (e.target.value) fileTo(d, e.target.value) }}>
                <option value="">{translate("documents.refile")}</option>
                {#each jdCategories as c}<option value={c.id}>{c.code} {c.name}</option>{/each}
              </select>
              <button class="btn sm danger" title={translate("documents.trash")} aria-label={translate("documents.trashValue", { "id": d.title || `document ${d.id}` })}
                      onclick={() => (trashRequest = { kind: 'one', doc: d })}>
                <Icon name="trash" size={12} /></button>
            </span>
          {/if}
          <span class="sub">{fmtDate(d.created_at)}</span>
        </span>
      </div>
    {/each}
  </div>
  {/if}
  {#if pages > 1}
    <nav class="pager document-pagination" aria-label={translate("documents.documentPages")}>
      <span>{translate("documents.pageSummary", { page, pages, documents: plural("documents.documentCount", count) })}</span>
      <div class="page-links">
        <button class="btn sm page-direction" disabled={page <= 1} onclick={() => go(pageHash(page - 1))} aria-label={translate("documents.previousPage")}>
          <span aria-hidden="true">‹</span><span class="direction-label">{translate("documents.prev")}</span>
        </button>
        {#each pageNumbers as number, index (number)}
          {#if index > 0 && number > pageNumbers[index - 1] + 1}<span class="page-gap" aria-hidden="true">…</span>{/if}
          <a class="btn sm page-number" class:primary={number === page} href={filingHref(pageHash(number))}
             aria-label={translate("documents.pageValue", { "number": number })} aria-current={number === page ? 'page' : undefined}>{formatNumber(number)}</a>
        {/each}
        <button class="btn sm page-direction" disabled={page >= pages} onclick={() => go(pageHash(page + 1))} aria-label={translate("documents.nextPage")}>
          <span class="direction-label">{translate("documents.next")}</span><span aria-hidden="true">›</span>
        </button>
      </div>
    </nav>
  {/if}
{/if}

{#if trashRequest}
  <ConfirmDialog
    title={trashRequest.kind === 'bulk'
      ? plural("documents.moveCountToTrash", trashRequest.count)
      : translate("documents.moveDocumentToTrash")}
    message={trashRequest.kind === 'bulk'
      ? plural("documents.selectedCountWillMoveToTrash", trashRequest.count)
      : translate("documents.valueWillMoveToTrashWhereItCan", { "id": trashRequest.doc.title || translate("documents.documentNumber", { id: trashRequest.doc.id }) })}
    confirmLabel={translate("documents.moveToTrash")}
    busy={trashBusy}
    onConfirm={confirmTrash}
    onCancel={() => (trashRequest = null)} />
{/if}

{#if bulkDecOpen}
  <div class="modal-veil"
       onclick={() => (bulkDecOpen = false)}
       onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); bulkDecOpen = false } }}
       role="button" tabindex="-1" aria-label={translate("documents.closeBulkDecryptDialog")}>
    <div class="modal" style="width:min(420px,94vw)"
         onclick={(e) => e.stopPropagation()}
         onkeydown={(e) => e.stopPropagation()}
         role="dialog" aria-modal="true" aria-label={translate("documents.bulkUnlock")} tabindex="-1">
      <div class="modal-head">
        <h3>{plural("documents.tryPasswordDocumentCount", sel.size)}</h3>
        <button class="btn sm" onclick={() => (bulkDecOpen = false)} title={translate("common.close")} aria-label={translate("documents.closeBulkUnlock")}><Icon name="x" size={13} /></button>
      </div>
      <p class="sub" style="margin:0 0 10px">
        {translate("documents.theServerTriesThisPasswordAgainstEvery")}
      </p>
      <div class="field">
        <label for="bulkdec-pw">{translate("documents.password2")}</label>
        <input id="bulkdec-pw" type="password" class="input" bind:value={bulkDecPw}
               bind:this={bulkDecInput} disabled={bulkDecBusy}
               onkeydown={(e) => { if (e.key === 'Enter') runBulkDecrypt() }} />
      </div>
      <div class="toolbar" style="margin-top:8px">
        <button class="btn primary sm" disabled={bulkDecBusy || !bulkDecPw.trim()} onclick={runBulkDecrypt}>
          {bulkDecBusy ? translate("documents.trying") : translate("documents.tryPassword")}
        </button>
        <button class="btn sm" disabled={bulkDecBusy} onclick={() => (bulkDecOpen = false)}>{translate("common.cancel")}</button>
      </div>
    </div>
  </div>
{/if}
