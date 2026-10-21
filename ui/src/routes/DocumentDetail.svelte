<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from '../lib/i18n.svelte.js'
  import { compareLocalized, customFieldLabel } from '../lib/format.js'
  import { scopedHash as filingHref } from '../lib/systems.svelte.js'
  import { captureScope, scopeCurrent } from '../lib/systems.svelte.js'
  import { onDestroy, tick, untrack } from 'svelte'
  import { getDocument, getDocumentIntrinsic, patchDocument, deleteDocument, restoreDocument, permanentlyDeleteDocument, documentVersions, documentBacklinks, uploadDocumentVersion, setDocumentCustomField, clearDocumentCustomField, createDocumentNote, updateDocumentNote, deleteDocumentNote, createShareLink, listShareLinks, deleteShareLink, previewPath, downloadPath, similarDocs, listGrants, putGrant, deleteGrant, listAllTags, listCorrespondents, listCustomFields, listDocuments, bulkEdit } from '../lib/api.js'
  import { go } from '../lib/router.svelte.js'
  import { SENSITIVITY_OPTIONS, fmtDate, fmtBytes, formatLabel, isHighSensitivity, sensDot, sensitivityLabel } from '../lib/format.js'
  import { originalNames, sourceGroups } from '../lib/documentSources.js'
  import { session } from '../lib/session.svelte.js'
  import { hasCapability } from '../lib/capabilities.js'
  import { CUSTOM_FIELDS_SETTINGS_HASH, TAGS_SETTINGS_HASH } from '../lib/configuration.js'
  import Icon from '../lib/Icon.svelte'
  import TypeaheadPicker from '../lib/TypeaheadPicker.svelte'
  import DocumentUnlockStatus from '../lib/DocumentUnlockStatus.svelte'
  import ConfirmDialog from '../lib/ConfirmDialog.svelte'
  import { copyText } from '../lib/clipboard.js'
  import LinkQR from '../lib/LinkQR.svelte'
  import DocumentLinkDialog from '../lib/DocumentLinkDialog.svelte'
  import ShareLinkForm from '../lib/ShareLinkForm.svelte'
  import { markUploaded } from '../lib/upload_bus.svelte.js'
  import { parseDocumentReference } from '../lib/documentReferences.js'

  let { id, notify, jdCategories = [] } = $props()

  let doc = $state(null)
  let versionHistory = $state({ count: 0, results: [], head_id: null, can_upload: false, next: null, previous: null })
  let versionsLoading = $state(false)
  let versionsError = $state('')
  let versionsPage = $state(1)
  let replacementFile = $state(null)
  let replacementKey = $state('')
  let replacementBusy = $state(false)
  let replacementError = $state('')
  let replacementInput = $state()
  let customFieldDefinitions = $state([])
  let customFieldsLoading = $state(false)
  let customFieldsError = $state('')
  let backlinks = $state({ count: 0, results: [], next: null, previous: null })
  let backlinksLoading = $state(false)
  let backlinksError = $state('')
  let backlinksPage = $state(1)
  let referenceEditors = $state({})
  let customFieldEditors = $state({})
  let editingCustomFields = $state(false)
  let showMoreFields = $state(false)
  let moreFieldsElement = $state(null)
  let moreToggleElement = $state(null)
  let addingReference = $state(false)
  let newReferenceFieldID = $state('')
  let loading = $state(true)
  let err = $state('')
  let revealed = $state(false)
  let fullText = $state(false)
  let textCopyFallback = $state(false)
  let textCopyStatus = $state('')
  let editingTitle = $state(false)
  let titleDraft = $state('')
  let titleOwnerBusy = $state(false)
  let editingLanguages = $state(false)
  let languagesDraft = $state('')
  let editingArchiveSerialNumber = $state(false)
  let archiveSerialNumberDraft = $state('')
  let archiveSerialNumberBusy = $state(false)
  let archiveSerialNumberError = $state('')
  let editingTags = $state(false)
  let tagOptions = $state([])
  let tagsBusy = $state(false)
  let tagsError = $state('')
  let tagLoadError = $state('')
  let editingCorrespondent = $state(false)
  let correspondentOptions = $state([])
  let correspondentDraft = $state('')
  let correspondentBusy = $state(false)
  let correspondentError = $state('')
  const correspondentChoices = $derived([
    { id: 0, name: translate("documentDetail.noSender") },
    ...correspondentOptions,
  ])
  let shareURL = $state('')
  let shareBusy = $state(false)
  let documentLinkOpen = $state(false)
  let similar = $state(null)   // {results, method} | null
  let similarLoading = $state(false)
  let similarError = $state('')
  let relatedTab = $state('similar')
  let access = $state(null)    // owner/admin-only {results, principals}
  let canManageAccess = $state(false)
  let accessDraft = $state({ principal: '', perm_bits: '1' })
  let accessOpen = $state(false)
  let trashOpen = $state(false)
  let trashBusy = $state(false)
  let deleteOpen = $state(false)
  let recoveryBusy = $state(false)
  let noteDraft = $state('')
  let noteEditID = $state(null)
  let noteEditDraft = $state('')
  let noteBusy = $state(false)
  let noteError = $state('')
  let notePendingDelete = $state(null)
  let loadVersion = 0
  let tagRefreshVersion = 0
  let loadedID
  let disposed = false
  onDestroy(() => { disposed = true; loadVersion++ })

  const ACCESS_LEVELS = [
    ['1', translate("documentDetail.view")],
    ['3', translate("documentDetail.edit")],
    ['7', translate("documentDetail.fullControl")],
  ]

  const RELATED_TABS = [
    { id: 'similar', label: translate("documentDetail.similarDocuments"), shortLabel: translate("documentDetail.similar") },
    { id: 'backlinks', label: translate("documentDetail.linkedDocuments"), shortLabel: translate("documentDetail.linked") },
    { id: 'versions', label: translate("documentDetail.versions"), shortLabel: translate("documentDetail.versions") },
    { id: 'notes', label: translate("documentDetail.notes"), shortLabel: translate("documentDetail.notes") },
  ]

  function relatedTabCount(tab) {
    if (tab === 'backlinks') return linkedDocumentCount
    if (tab === 'notes') return doc?.notes?.length || 0
    return Math.max(0, versionHistory.count - 1)
  }

  function selectRelatedTab(tab) {
    relatedTab = tab
  }

  function handleRelatedTabKey(event) {
    const keys = ['ArrowLeft', 'ArrowRight', 'Home', 'End']
    if (!keys.includes(event.key)) return
    event.preventDefault()
    const tabs = [...event.currentTarget.parentElement.querySelectorAll('[role="tab"]')]
    const current = visibleRelatedTabs.findIndex(tab => tab.id === relatedTab)
    const next = event.key === 'Home' ? 0 : event.key === 'End' ? visibleRelatedTabs.length - 1
      : (current + (event.key === 'ArrowRight' ? 1 : -1) + visibleRelatedTabs.length) % visibleRelatedTabs.length
    relatedTab = visibleRelatedTabs[next].id
    tabs[next]?.focus()
  }

  const highSensitivity = $derived(isHighSensitivity(doc?.sensitivity))
  const blurred = $derived(highSensitivity && !revealed)
  const trashed = $derived(doc?.trashed_at != null)
  const canManageTrash = $derived(trashed && (session.user?.role === 'admin' || doc?.owner_id === session.user?.user_id))
  const canRestore = $derived(canManageTrash && doc?.deletes_at > Math.floor(Date.now() / 1000))
  const canReadFile = $derived(!trashed || canManageTrash)
  const canShareLinks = $derived(!trashed && hasCapability(session.user, 'share_links'))
  const visibleRelatedTabs = $derived(trashed ? RELATED_TABS.filter(tab => tab.id === 'notes') : RELATED_TABS)
  const ordinaryCustomFields = $derived.by(() => {
    const fields = new Map()
    for (const definition of customFieldDefinitions) {
      if (definition.data_type === 'documentlink') continue
      fields.set(Number(definition.id), {
        field_id: definition.id,
        name: definition.name,
        data_type: definition.data_type,
        managed_by: definition.managed_by,
        extra_data: parseCustomFieldExtra(definition.extra_data),
        value: null,
        display_value: '',
        is_set: false,
      })
    }
    for (const value of doc?.custom_fields || []) {
      if (value.data_type === 'documentlink') continue
      fields.set(Number(value.field_id), {
        ...(fields.get(Number(value.field_id)) || {}),
        ...value,
        extra_data: parseCustomFieldExtra(value.extra_data),
        is_set: true,
      })
    }
    return [...fields.values()].sort((a, b) => compareLocalized(customFieldLabel(a), customFieldLabel(b)))
  })
  const displayedOrdinaryCustomFields = $derived(ordinaryCustomFields.filter(field =>
    field.is_set || customFieldEditors[field.field_id]?.open
  ))
  const availableOrdinaryCustomFields = $derived(ordinaryCustomFields.filter(field =>
    !field.managed_by && !field.is_set && !customFieldEditors[field.field_id]?.open
  ))
  // The card keeps core metadata visible. An unset archive serial number and
  // all ordinary custom fields sit behind the More toggle.
  const archiveSerialNumberInMore = $derived(doc?.archive_serial_number == null)
  const sourceGroupList = $derived(sourceGroups(doc?.sources))
  const originalNameList = $derived(originalNames(doc?.sources))
  // created_at differs from added_at only when ingest carried the file's own date.
  const originalFileDate = $derived(doc?.source_mtime ||
    (doc?.created_at && doc.created_at !== doc.added_at ? doc.created_at : null))
  const setCustomFieldCount = $derived(displayedOrdinaryCustomFields.filter(field => field.is_set).length)
  const hasMoreFields = $derived(archiveSerialNumberInMore || displayedOrdinaryCustomFields.length > 0 ||
    (!trashed && availableOrdinaryCustomFields.length > 0))
  const documentLinkFields = $derived.by(() => {
    const fields = new Map()
    for (const field of customFieldDefinitions) {
      if (field.data_type === 'documentlink') fields.set(Number(field.id), { ...field })
    }
    for (const value of doc?.custom_fields || []) {
      if (value.data_type !== 'documentlink') continue
      fields.set(Number(value.field_id), { ...(fields.get(Number(value.field_id)) || {}), id: value.field_id, name: value.name })
    }
    return [...fields.values()].sort((a, b) => compareLocalized(String(a.name || ''), String(b.name || '')))
  })
  const outgoingDocumentLinkFields = $derived(documentLinkFields.filter(field => documentLinkValue(field.id)?.value))
  const availableDocumentLinkFields = $derived(documentLinkFields.filter(field => !documentLinkValue(field.id)?.value))
  const selectedNewReferenceField = $derived(documentLinkFields.find(field => Number(field.id) === Number(newReferenceFieldID)))
  const linkedDocumentCount = $derived(backlinks.count + outgoingDocumentLinkFields.length)
  // Inline-previewable formats: archive_blob is always PDF, browsers render
  // common media natively, and the server turns stored email bodies into a
  // sandboxed HTML preview. Other formats swap the iframe for a download panel.
  const previewable = $derived.by(() => {
    if (!doc) return false
    if (doc.archive_blob) return true
    const m = (doc.mime_type || '').toLowerCase().split(';')[0].trim()
    if (m === 'application/pdf') return true
    if (m === 'message/rfc822') return true
    if (m.startsWith('image/')) {
      return ['image/png', 'image/jpeg', 'image/gif', 'image/webp', 'image/svg+xml', 'image/avif'].includes(m)
    }
    if (m === 'text/plain' || m === 'text/html' || m === 'text/csv' || m === 'text/markdown') return true
    return false
  })
  const areaGroups = $derived.by(() => {
    const m = new Map()
    for (const c of jdCategories) {
      const lo = Number(c.area_code)
      if (!m.has(lo)) m.set(lo, { lo, name: c.area_name, categories: [] })
      m.get(lo).categories.push(c)
    }
    return [...m.values()].sort((a, b) => a.lo - b.lo)
  })

  async function load(preserveRelatedTab = false) {
    const selectedRelatedTab = preserveRelatedTab ? relatedTab : 'similar'
    const version = ++loadVersion
    const scope = captureScope()
    tagRefreshVersion++
    const documentID = id
    loading = true
    err = ''
    doc = null
    fullText = false
    textCopyFallback = false
    textCopyStatus = ''
    versionHistory = { count: 0, results: [], head_id: null, can_upload: false, next: null, previous: null }
    versionsLoading = false
    versionsError = ''
    versionsPage = 1
    replacementFile = null
    replacementKey = ''
    replacementBusy = false
    replacementError = ''
    customFieldDefinitions = []
    customFieldsLoading = false
    customFieldsError = ''
    backlinks = { count: 0, results: [], next: null, previous: null }
    backlinksLoading = false
    backlinksError = ''
    backlinksPage = 1
    referenceEditors = {}
    customFieldEditors = {}
    editingCustomFields = false
    showMoreFields = false
    similar = null
    addingReference = false
    newReferenceFieldID = ''
    similarLoading = false
    similarError = ''
    relatedTab = selectedRelatedTab
    access = null
    canManageAccess = false
    editingTitle = false
    titleOwnerBusy = false
    editingLanguages = false
    editingArchiveSerialNumber = false
    archiveSerialNumberDraft = ''
    archiveSerialNumberBusy = false
    archiveSerialNumberError = ''
    editingTags = false
    tagOptions = []
    tagsBusy = false
    tagsError = ''
    tagLoadError = ''
    editingCorrespondent = false
    correspondentOptions = []
    correspondentDraft = ''
    correspondentBusy = false
    correspondentError = ''
    shareOpen = false
    shareLoadVersion++
    shareLinks = []
    shareLoading = false
    shareError = ''
    shareHistory = { count: 0, results: [] }
    shareHistoryOpen = false
    shareHistoryLoaded = false
    shareHistoryLoading = false
    sharePendingRevoke = null
    shareBusy = false
    shareRevokeBusy = false
    documentLinkOpen = false
    shareURL = ''
    trashOpen = false
    trashBusy = false
    deleteOpen = false
    recoveryBusy = false
    noteDraft = ''
    noteEditID = null
    noteEditDraft = ''
    noteBusy = false
    noteError = ''
    notePendingDelete = null
    try {
      const loaded = await getDocument(documentID)
      if (version !== loadVersion || !scopeCurrent(scope)) return
      doc = loaded
      titleDraft = loaded.title
      archiveSerialNumberDraft = loaded.archive_serial_number == null ? '' : String(loaded.archive_serial_number)
      if (loaded.trashed_at != null) {
        relatedTab = 'notes'
        return
      }
      void loadVersions(documentID, 1, version, scope)
      void loadBacklinks(documentID, 1, version, scope)
      void loadCustomFieldDefinitions(version, scope)
      void loadSimilarDocuments(documentID, version, scope)
      void loadAccess(documentID, version, scope)
    } catch (ex) {
      if (version === loadVersion && scopeCurrent(scope)) err = ex.message || translate("documentDetail.couldNotLoadThisDocument")
    } finally {
      if (version === loadVersion && scopeCurrent(scope)) loading = false
    }
  }

  async function loadSimilarDocuments(documentID = id, version = loadVersion, scope = captureScope()) {
    similarLoading = true
    similarError = ''
    try {
      const result = await similarDocs(documentID)
      if (disposed || version !== loadVersion || !scopeCurrent(scope)) return
      similar = result
    } catch (ex) {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) {
        similar = null
        similarError = ex.message || translate("documentDetail.couldNotLoadSimilarDocuments")
      }
    } finally {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) similarLoading = false
    }
  }

  async function loadVersions(documentID = id, page = 1, version = loadVersion, scope = captureScope()) {
    versionsLoading = true
    versionsError = ''
    try {
      const result = await documentVersions(documentID, { page, page_size: 50 })
      if (disposed || version !== loadVersion || !scopeCurrent(scope)) return
      versionHistory = {
        count: result?.count || 0,
        results: result?.results || [],
        head_id: result?.head_id ?? null,
        can_upload: Boolean(result?.can_upload),
        next: result?.next || null,
        previous: result?.previous || null,
      }
      versionsPage = page
    } catch (ex) {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) {
        versionsError = ex.message || translate("documentDetail.couldNotLoadVersionHistory")
      }
    } finally {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) versionsLoading = false
    }
  }

  async function loadBacklinks(documentID = id, page = 1, version = loadVersion, scope = captureScope()) {
    backlinksLoading = true
    backlinksError = ''
    try {
      const result = await documentBacklinks(documentID, { page, page_size: 50 })
      if (disposed || version !== loadVersion || !scopeCurrent(scope)) return
      backlinks = {
        count: result?.count || 0,
        results: result?.results || [],
        next: result?.next || null,
        previous: result?.previous || null,
      }
      backlinksPage = page
    } catch (ex) {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) {
        backlinksError = ex.message || translate("documentDetail.couldNotLoadBacklinks")
      }
    } finally {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) backlinksLoading = false
    }
  }

  async function loadCustomFieldDefinitions(version = loadVersion, scope = captureScope()) {
    customFieldsLoading = true
    customFieldsError = ''
    try {
      const result = await listCustomFields()
      if (disposed || version !== loadVersion || !scopeCurrent(scope)) return
      customFieldDefinitions = result?.results || result || []
    } catch (ex) {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) {
        customFieldsError = ex.message || translate("documentDetail.couldNotLoadReferenceFields")
      }
    } finally {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) customFieldsLoading = false
    }
  }


  async function loadAccess(documentID = id, version = loadVersion, scope = captureScope()) {
    try {
      const result = await listGrants('document', documentID)
      if (version !== loadVersion || !scopeCurrent(scope)) return
      access = result
      canManageAccess = true
    } catch (ex) {
      if (version !== loadVersion || !scopeCurrent(scope)) return
      access = null
      canManageAccess = false
      if (ex.status !== 403) notify?.(ex.message || translate("documentDetail.couldNotLoadDocumentAccess"))
    }
  }

  function principalKey(principal) {
    return `${principal.kind}:${principal.id}`
  }

  function principalFor(grant) {
    return access?.principals?.find(p => p.kind === grant.principal_kind && Number(p.id) === Number(grant.principal_id))
  }

  function availablePrincipals(kind) {
    const granted = new Set((access?.results || []).map(g => `${g.principal_kind}:${g.principal_id}`))
    return (access?.principals || []).filter(p =>
      p.kind === kind && !p.disabled && !granted.has(principalKey(p)) &&
      !(p.kind === 'user' && Number(p.id) === Number(session.user?.user_id))
    )
  }

  function accessLabel(bits) {
    return ACCESS_LEVELS.find(([value]) => Number(value) === Number(bits))?.[1] || translate("documentDetail.custom")
  }

  function sourceLabel(source) {
    if (source.kind === 'upload') return source.label ? translate("documentDetail.uploadedByValue", { "label": source.label }) : translate("documentDetail.manualUpload")
    if (source.kind === 'api') return source.label ? translate("documentDetail.apiUploadByValue", { "label": source.label }) : translate("documentDetail.apiUpload")
    if (source.kind === 'mailbox') return source.label || translate("documentDetail.mailbox")
    if (source.kind === 'watched_folder') return source.label || translate("documentDetail.watchedFolder")
    if (source.kind === 'import') return source.label || translate("documentDetail.import")
    return source.label || translate("documentDetail.unknownOrigin")
  }

  function documentLinkValue(fieldID) {
    return (doc?.custom_fields || []).find(value =>
      Number(value.field_id) === Number(fieldID) && value.data_type === 'documentlink')
  }

  function parseCustomFieldExtra(extra) {
    if (extra && typeof extra === 'object') return extra
    try {
      return JSON.parse(extra || '{}')
    } catch {
      return {}
    }
  }

  function customFieldDisplay(field) {
    if (field.display_value) return field.display_value
    if (field.data_type === 'bool' && field.is_set) return field.value ? translate("documentDetail.yes") : translate("documentDetail.no")
    if (field.data_type === 'date' && field.value) return fmtDate(field.value)
    if (field.data_type === 'multi') return Array.isArray(field.value) ? field.value.join(', ') : ''
    return String(field.value ?? '')
  }

  function customFieldInitialValue(field) {
    if (field.data_type === 'multi') return Array.isArray(field.value) ? [...field.value] : []
    if (field.data_type === 'bool') return field.value ? 'true' : 'false'
    if (field.data_type === 'date' && field.value) return new Date(Number(field.value) * 1000).toISOString().slice(0, 10)
    return String(field.value ?? '')
  }

  function updateCustomFieldEditor(fieldID, patch) {
    customFieldEditors = {
      ...customFieldEditors,
      [fieldID]: { ...(customFieldEditors[fieldID] || {}), ...patch },
    }
  }

  function startCustomFieldEdit(field) {
    updateCustomFieldEditor(field.field_id, {
      open: true,
      value: customFieldInitialValue(field),
      busy: false,
      error: '',
    })
  }

  function addCustomField(event) {
    const fieldID = Number(event.currentTarget.value)
    event.currentTarget.value = ''
    if (!fieldID) return
    const field = ordinaryCustomFields.find(candidate => Number(candidate.field_id) === fieldID)
    if (field) {
      editingCustomFields = false
      startCustomFieldEdit(field)
    }
  }

  function customFieldValueReady(field, editor) {
    if (!editor) return false
    if (field.data_type === 'bool') return true
    if (field.data_type === 'multi') return Array.isArray(editor.value) && editor.value.length > 0
    return String(editor.value ?? '').trim() !== ''
  }

  async function saveCustomField(event, field) {
    event.preventDefault()
    const editor = customFieldEditors[field.field_id]
    if (!customFieldValueReady(field, editor) || editor.busy || trashed) return
    const version = loadVersion
    const documentID = id
    const scope = captureScope()
    updateCustomFieldEditor(field.field_id, { busy: true, error: '' })
    let value = editor.value
    if (field.data_type === 'number' || field.data_type === 'monetary') value = Number(value)
    else if (field.data_type === 'bool') value = value === 'true'
    else if (field.data_type !== 'multi') value = String(value).trim()
    try {
      await setDocumentCustomField(documentID, field.field_id, value)
      const loaded = await getDocument(documentID)
      if (disposed || version !== loadVersion || id !== documentID || !scopeCurrent(scope)) return
      doc = loaded
      const next = { ...customFieldEditors }
      delete next[field.field_id]
      customFieldEditors = next
      notify?.(translate("documentDetail.valueSaved", { "name": customFieldLabel(field) }))
    } catch (ex) {
      if (!disposed && version === loadVersion && id === documentID && scopeCurrent(scope)) {
        updateCustomFieldEditor(field.field_id, { busy: false, error: ex.message || translate("documentDetail.couldNotSaveValue", { "name": customFieldLabel(field) }) })
      }
    }
  }

  async function clearOrdinaryCustomField(field) {
    const editor = customFieldEditors[field.field_id]
    if (editor?.busy || trashed) return
    const version = loadVersion
    const documentID = id
    const scope = captureScope()
    updateCustomFieldEditor(field.field_id, { busy: true, error: '' })
    try {
      await clearDocumentCustomField(documentID, field.field_id)
      if (disposed || version !== loadVersion || id !== documentID || !scopeCurrent(scope)) return
      doc = { ...doc, custom_fields: (doc.custom_fields || []).filter(value => Number(value.field_id) !== Number(field.field_id)) }
      const next = { ...customFieldEditors }
      delete next[field.field_id]
      customFieldEditors = next
      notify?.(translate("documentDetail.valueCleared", { "name": customFieldLabel(field) }))
    } catch (ex) {
      if (!disposed && version === loadVersion && id === documentID && scopeCurrent(scope)) {
        updateCustomFieldEditor(field.field_id, { busy: false, error: ex.message || translate("documentDetail.couldNotClearValue", { "name": customFieldLabel(field) }) })
      }
    }
  }

  function chooseReplacement(event) {
    const file = event.currentTarget.files?.[0]
    if (!file) return
    replacementFile = file
    replacementKey = crypto.randomUUID()
    replacementError = ''
  }

  function cancelReplacement() {
    if (replacementBusy) return
    replacementFile = null
    replacementKey = ''
    replacementError = ''
    if (replacementInput) replacementInput.value = ''
  }

  function replacementFailure(error) {
    if (error?.code === 'duplicate_version_blob') {
      const existing = error.data?.existing_id
      return existing
        ? translate("documentDetail.theseBytesAlreadyBelongToLiveDocumentValue", { "existing": existing })
        : translate("documentDetail.theseBytesAlreadyBelongToALiveDocument")
    }
    if (error?.status === 413) return translate("documentDetail.thisFileIsLargerThanTheConfiguredUpload")
    if (error?.status === 403) return translate("documentDetail.youNoLongerHavePermissionToUploadA")
    if (error?.status === 404) return translate("documentDetail.thisVersionIsNoLongerAvailableForReplacement")
    if (error?.status === 409) return error.message || translate("documentDetail.theDocumentChangedBeforeTheReplacementWasSaved")
    return error?.message || translate("documentDetail.theReplacementCouldNotBeUploadedTryingAgain")
  }

  async function uploadReplacement() {
    if (!replacementFile || !replacementKey || replacementBusy) return
    const version = loadVersion
    const documentID = id
    const scope = captureScope()
    const file = replacementFile
    const key = replacementKey
    replacementBusy = true
    replacementError = ''
    try {
      const result = await uploadDocumentVersion(documentID, file, key)
      if (disposed || version !== loadVersion || id !== documentID || !scopeCurrent(scope)) return
      markUploaded(scope)
      notify?.(translate("documentDetail.replacementUploadedFileProcessingContinuesInTheBackground"))
      go(`#/doc/${result.id}`)
    } catch (ex) {
      if (!disposed && version === loadVersion && id === documentID && scopeCurrent(scope)) {
        replacementError = replacementFailure(ex)
      }
    } finally {
      if (!disposed && version === loadVersion && id === documentID && scopeCurrent(scope) &&
          replacementFile === file && replacementKey === key) {
        replacementBusy = false
      }
    }
  }

  function referenceEditor(fieldID) {
    return referenceEditors[fieldID] || { open: false, query: '', results: [], selected: null, busy: false, error: '' }
  }

  function updateReferenceEditor(fieldID, patch) {
    referenceEditors = {
      ...referenceEditors,
      [fieldID]: { ...referenceEditor(fieldID), ...patch },
    }
  }

  function toggleReferenceEditor(field) {
    const open = !referenceEditor(field.id).open
    addingReference = false
    newReferenceFieldID = ''
    referenceEditors = open
      ? { [field.id]: { ...referenceEditor(field.id), open: true, error: '' } }
      : {}
  }

  function startNewReference() {
    addingReference = true
    const field = availableDocumentLinkFields.length === 1 ? availableDocumentLinkFields[0] : null
    newReferenceFieldID = field?.id || ''
    referenceEditors = field
      ? { [field.id]: { ...referenceEditor(field.id), open: true, error: '' } }
      : {}
  }

  function selectNewReferenceField(event) {
    const fieldID = Number(event.currentTarget.value) || ''
    newReferenceFieldID = fieldID
    referenceEditors = fieldID
      ? { [fieldID]: { ...referenceEditor(fieldID), open: true, error: '' } }
      : {}
  }

  function cancelNewReference() {
    addingReference = false
    newReferenceFieldID = ''
    referenceEditors = {}
  }

  async function findReferenceTargets(field) {
    const editor = referenceEditor(field.id)
    const query = editor.query.trim()
    if (!query || editor.busy) return
    const exact = parseDocumentReference(query, location.origin, location.pathname)
    if (exact?.error) {
      updateReferenceEditor(field.id, { results: [], selected: null, error: exact.error })
      return
    }
    const version = loadVersion
    const documentID = id
    const scope = captureScope()
    updateReferenceEditor(field.id, { busy: true, error: '', results: [], selected: null })
    try {
      let results
      if (exact?.id) {
        const target = await getDocumentIntrinsic(exact.id)
        if (target.trashed_at != null) throw new Error(translate("documentDetail.thatDocumentIsInTrash"))
        results = [target]
      } else {
        const result = await listDocuments({ q: query, page_size: 8 })
        results = result?.results || []
      }
      if (disposed || version !== loadVersion || id !== documentID || !scopeCurrent(scope)) return
      updateReferenceEditor(field.id, {
        busy: false,
        results,
        selected: results.length === 1 ? results[0] : null,
        error: results.length ? '' : translate("documentDetail.noMatchingDocumentsFound"),
      })
    } catch (ex) {
      if (!disposed && version === loadVersion && id === documentID && scopeCurrent(scope)) {
        updateReferenceEditor(field.id, {
          busy: false,
          results: [],
          selected: null,
          error: ex.message || translate("documentDetail.couldNotFindThatDocument"),
        })
      }
    }
  }

  async function saveReference(field) {
    const editor = referenceEditor(field.id)
    if (!editor.selected || editor.busy) return
    const version = loadVersion
    const documentID = id
    const scope = captureScope()
    updateReferenceEditor(field.id, { busy: true, error: '' })
    try {
      await setDocumentCustomField(documentID, field.id, editor.selected.id)
      if (disposed || version !== loadVersion || id !== documentID || !scopeCurrent(scope)) return
      notify?.(translate("documentDetail.valueUpdated", { "name": field.name }))
      await load(true)
    } catch (ex) {
      if (!disposed && version === loadVersion && id === documentID && scopeCurrent(scope)) {
        updateReferenceEditor(field.id, { busy: false, error: ex.message || translate("documentDetail.couldNotUpdateThisReference") })
      }
    }
  }

  async function clearReference(field) {
    const version = loadVersion
    const documentID = id
    const scope = captureScope()
    updateReferenceEditor(field.id, { busy: true, error: '' })
    try {
      await clearDocumentCustomField(documentID, field.id)
      if (disposed || version !== loadVersion || id !== documentID || !scopeCurrent(scope)) return
      notify?.(translate("documentDetail.valueCleared", { "name": field.name }))
      await load(true)
    } catch (ex) {
      if (!disposed && version === loadVersion && id === documentID && scopeCurrent(scope)) {
        updateReferenceEditor(field.id, { busy: false, error: ex.message || translate("documentDetail.couldNotClearThisReference") })
      }
    }
  }

  async function grantAccess(e) {
    e.preventDefault()
    const [principalKind, rawID] = accessDraft.principal.split(':')
    const principalID = Number(rawID)
    if (!principalKind || !principalID) return
    try {
      await putGrant('document', id, {
        principal_kind: principalKind,
        principal_id: principalID,
        perm_bits: Number(accessDraft.perm_bits),
      })
      accessDraft = { principal: '', perm_bits: '1' }
      await loadAccess()
      notify?.(translate("documentDetail.accessGranted"))
    } catch (ex) { notify?.(ex.message || translate("documentDetail.couldNotGrantAccess")) }
  }

  async function changeAccess(grant, bits) {
    try {
      await putGrant('document', id, {
        principal_kind: grant.principal_kind,
        principal_id: grant.principal_id,
        perm_bits: Number(bits),
      })
      grant.perm_bits = Number(bits)
      notify?.(translate("documentDetail.accessUpdated"))
    } catch (ex) { notify?.(ex.message || translate("documentDetail.couldNotUpdateAccess")) }
  }

  async function revokeAccess(grant) {
    try {
      await deleteGrant('document', id, grant.principal_kind, grant.principal_id)
      access = { ...access, results: access.results.filter(g => g.id !== grant.id) }
      notify?.(translate("documentDetail.accessRevoked"))
    } catch (ex) { notify?.(ex.message || translate("documentDetail.couldNotRevokeAccess")) }
  }

  async function refreshReviewTags(version, documentID) {
    const refresh = ++tagRefreshVersion
    if (!doc?.tags?.includes('needs-review')) return
    try {
      const latest = await getDocument(documentID)
      if (!disposed && version === loadVersion && refresh === tagRefreshVersion) {
        doc = { ...doc, tags: latest.tags || [] }
      }
    } catch (ex) {
      if (!disposed && version === loadVersion && refresh === tagRefreshVersion) {
        notify?.(translate("documentDetail.savedButTagsCouldNotBeRefreshedReload"))
      }
    }
  }

  async function save(patch, label) {
    const version = loadVersion
    try {
      await patchDocument(id, patch)
      if (disposed || version !== loadVersion) return
      doc = { ...doc, ...patch }
      if ('title' in patch && !('title_owner' in patch)) doc.title_owner = 'human'
      if ('languages' in patch) doc.languages_locked = patch.languages !== ''
      await refreshReviewTags(version, id)
      if (disposed || version !== loadVersion) return
      notify?.(label || translate("documentDetail.saved"))
    } catch (ex) {
      if (!disposed && version === loadVersion) notify?.(ex.message || translate("documentDetail.couldNotSave"))
    }
  }

  async function toggleMoreFields() {
    showMoreFields = !showMoreFields
    if (!showMoreFields) return
    await tick()
    const first = moreFieldsElement?.querySelector('dt')
    if (!first || !moreToggleElement) return
    const behavior = window.matchMedia?.('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth'
    const revealedHeight = moreToggleElement.getBoundingClientRect().bottom - first.getBoundingClientRect().top
    if (revealedHeight > window.innerHeight) first.scrollIntoView({ block: 'start', behavior })
    else moreToggleElement.scrollIntoView({ block: 'nearest', behavior })
    moreFieldsElement.querySelector('button, input, select, textarea, a[href]')?.focus({ preventScroll: true })
  }

  function startEditArchiveSerialNumber() {
    archiveSerialNumberDraft = doc?.archive_serial_number == null ? '' : String(doc.archive_serial_number)
    archiveSerialNumberError = ''
    editingArchiveSerialNumber = true
  }

  function cancelEditArchiveSerialNumber() {
    editingArchiveSerialNumber = false
    archiveSerialNumberError = ''
  }

  async function saveArchiveSerialNumber(event) {
    event.preventDefault()
    if (archiveSerialNumberBusy) return
    const raw = archiveSerialNumberDraft.trim()
    const value = raw === '' ? null : Number(raw)
    if (value !== null && (!/^\d+$/.test(raw) || !Number.isSafeInteger(value) || value <= 0)) {
      archiveSerialNumberError = translate("documentDetail.invalidArchiveSerialNumber")
      return
    }
    const version = loadVersion
    archiveSerialNumberBusy = true
    archiveSerialNumberError = ''
    try {
      await patchDocument(id, { archive_serial_number: value })
      if (disposed || version !== loadVersion) return
      doc = { ...doc, archive_serial_number: value }
      editingArchiveSerialNumber = false
      notify?.(translate("documentDetail.archiveSerialNumberSaved"))
    } catch (ex) {
      if (!disposed && version === loadVersion) {
        archiveSerialNumberError = ex.message || translate("documentDetail.couldNotSave")
      }
    } finally {
      if (!disposed && version === loadVersion) archiveSerialNumberBusy = false
    }
  }

  async function releaseTitle() {
    if (titleOwnerBusy) return
    const version = loadVersion
    titleOwnerBusy = true
    try {
      await patchDocument(id, { title_owner: 'auto' })
      if (disposed || version !== loadVersion) return
      doc = { ...doc, title_owner: 'auto' }
      notify?.(translate("documentDetail.suchiCanRenameThisTitle"))
    } catch (ex) {
      if (!disposed && version === loadVersion) notify?.(ex.message || translate("documentDetail.couldNotUnpinTitle"))
    } finally {
      if (!disposed && version === loadVersion) titleOwnerBusy = false
    }
  }

  async function startEditTags() {
    if (tagsBusy || trashed) return
    const version = loadVersion
    const scope = captureScope()
    editingTags = true
    tagsBusy = true
    tagOptions = []
    tagLoadError = ''
    tagsError = ''
    try {
      const options = await listAllTags()
      if (disposed || version !== loadVersion || !scopeCurrent(scope)) return
      tagOptions = options
    } catch (ex) {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) tagLoadError = ex.message || translate("documentDetail.couldNotLoadTags")
    } finally {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) tagsBusy = false
    }
  }

  async function changeTag(tag, method) {
    if (!tag || tagsBusy || trashed) return false
    const version = loadVersion
    const scope = captureScope()
    const documentID = Number(id)
    tagsBusy = true
    tagsError = ''
    try {
      const result = await bulkEdit([documentID], method, { tag_id: tag.id })
      if (disposed || version !== loadVersion || !scopeCurrent(scope)) return false
      const outcome = result?.results?.find(item => item.id === documentID)
      if (!outcome?.ok) {
        throw new Error(outcome?.code === 'forbidden' ? translate("documentDetail.youDoNotHavePermissionToEditThis") : translate("documentDetail.couldNotUpdateTags"))
      }
      doc = { ...doc, tags: method === 'add_tag'
        ? [...new Set([...(doc.tags || []), tag.slug])].sort()
        : (doc.tags || []).filter(slug => slug !== tag.slug) }
      await refreshReviewTags(version, documentID)
      if (disposed || version !== loadVersion || !scopeCurrent(scope)) return false
      notify?.(method === 'add_tag' ? translate("documentDetail.tagAdded") : translate("documentDetail.tagRemoved"))
      return true
    } catch (ex) {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) tagsError = ex.message || translate("documentDetail.couldNotUpdateTags")
      return false
    } finally {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) tagsBusy = false
    }
  }

  async function startEditCorrespondent() {
    if (correspondentBusy || trashed) return
    const version = loadVersion
    const scope = captureScope()
    const currentOptions = (doc?.correspondents || []).map(item => ({ id: item.id, name: item.name }))
    correspondentOptions = currentOptions
    correspondentDraft = String(doc?.correspondents?.find(item => item.role === 'sender')?.id || 0)
    editingCorrespondent = true
    correspondentBusy = true
    correspondentError = ''
    try {
      const response = await listCorrespondents()
      if (disposed || version !== loadVersion || !scopeCurrent(scope)) return
      correspondentOptions = [...(response.results || [])]
      for (const current of currentOptions) {
        if (!correspondentOptions.some(option => option.id === current.id)) {
          correspondentOptions.unshift(current)
        }
      }
    } catch (ex) {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) {
        correspondentError = ex.message || translate("documentDetail.couldNotLoadCorrespondents")
      }
    } finally {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) correspondentBusy = false
    }
  }

  function chooseCorrespondent(correspondent) {
    correspondentDraft = String(correspondent.id)
    return true
  }

  async function saveCorrespondent(event) {
    event.preventDefault()
    if (correspondentBusy || trashed || correspondentDraft === '') return
    const version = loadVersion
    const scope = captureScope()
    const documentID = Number(id)
    const correspondentID = Number(correspondentDraft)
    correspondentBusy = true
    correspondentError = ''
    try {
      const result = await bulkEdit([documentID], 'set_correspondent', { correspondent_id: correspondentID })
      if (disposed || version !== loadVersion || !scopeCurrent(scope)) return
      const outcome = result?.results?.find(item => item.id === documentID)
      if (!outcome?.ok) {
        throw new Error(outcome?.code === 'forbidden' ? translate("documentDetail.youDoNotHavePermissionToEditThis") : translate("documentDetail.couldNotUpdateCorrespondent"))
      }
      const current = doc.correspondents || []
      const selected = correspondentOptions.find(item => item.id === correspondentID)
        || current.find(item => item.id === correspondentID)
      const retained = current.filter(item => selected
        ? item.role !== 'sender' || item.id !== selected.id
        : item.role !== 'sender')
      doc = {
        ...doc,
        correspondents: selected
          ? [{ id: selected.id, name: selected.name, role: 'sender' }, ...retained]
          : retained,
      }
      editingCorrespondent = false
      notify?.(selected ? translate("documentDetail.correspondentUpdated") : translate("documentDetail.senderCleared"))
    } catch (ex) {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) {
        correspondentError = ex.message || translate("documentDetail.couldNotUpdateCorrespondent")
      }
    } finally {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) correspondentBusy = false
    }
  }

  function startEditLanguages() {
    languagesDraft = doc?.languages || ''
    editingLanguages = true
  }

  function saveLanguages() {
    const trimmed = languagesDraft.trim()
    editingLanguages = false
    return save({ languages: trimmed }, trimmed ? translate("documentDetail.languagesUpdated") : translate("documentDetail.languagesCleared"))
  }

  async function addNote(event) {
    event.preventDefault()
    const note = noteDraft.trim()
    if (!note || noteBusy || trashed) return
    const version = loadVersion
    const documentID = id
    const scope = captureScope()
    noteBusy = true
    noteError = ''
    try {
      const created = await createDocumentNote(documentID, note)
      if (disposed || version !== loadVersion || id !== documentID || !scopeCurrent(scope)) return
      doc = { ...doc, notes: [...(doc.notes || []), created] }
      noteDraft = ''
      notify?.(translate("documentDetail.noteAdded"))
    } catch (ex) {
      if (!disposed && version === loadVersion && id === documentID && scopeCurrent(scope)) {
        noteError = ex.message || translate("documentDetail.couldNotAddThisNote")
      }
    } finally {
      if (!disposed && version === loadVersion && id === documentID && scopeCurrent(scope)) noteBusy = false
    }
  }

  function startNoteEdit(note) {
    noteEditID = note.id
    noteEditDraft = note.note
    noteError = ''
  }

  async function saveNoteEdit(event, note) {
    event.preventDefault()
    const next = noteEditDraft.trim()
    if (!next || noteBusy || trashed) return
    const version = loadVersion
    const documentID = id
    const scope = captureScope()
    noteBusy = true
    noteError = ''
    try {
      const result = await updateDocumentNote(documentID, note.id, next)
      if (disposed || version !== loadVersion || id !== documentID || !scopeCurrent(scope)) return
      doc = {
        ...doc,
        notes: (doc.notes || []).map(item => item.id === note.id
          ? { ...item, note: next, updated_at: result.updated_at }
          : item),
      }
      noteEditID = null
      noteEditDraft = ''
      notify?.(translate("documentDetail.noteUpdated"))
    } catch (ex) {
      if (!disposed && version === loadVersion && id === documentID && scopeCurrent(scope)) {
        noteError = ex.message || translate("documentDetail.couldNotUpdateThisNote")
      }
    } finally {
      if (!disposed && version === loadVersion && id === documentID && scopeCurrent(scope)) noteBusy = false
    }
  }

  async function removeNote() {
    const note = notePendingDelete
    if (!note || noteBusy) return
    const version = loadVersion
    const documentID = id
    const scope = captureScope()
    noteBusy = true
    noteError = ''
    try {
      await deleteDocumentNote(documentID, note.id)
      if (disposed || version !== loadVersion || id !== documentID || !scopeCurrent(scope)) return
      doc = { ...doc, notes: (doc.notes || []).filter(item => item.id !== note.id) }
      notePendingDelete = null
      if (noteEditID === note.id) noteEditID = null
      notify?.(translate("documentDetail.noteDeleted"))
    } catch (ex) {
      if (!disposed && version === loadVersion && id === documentID && scopeCurrent(scope)) {
        noteError = ex.message || translate("documentDetail.couldNotDeleteThisNote")
        notePendingDelete = null
      }
    } finally {
      if (!disposed && version === loadVersion && id === documentID && scopeCurrent(scope)) noteBusy = false
    }
  }

  // ---- share dialog: recipient surface + existing links ----
  let shareOpen = $state(false)
  let shareLinks = $state([])
  let shareLoading = $state(false)
  let shareError = $state('')
  let shareHistory = $state({ count: 0, results: [] })
  let shareHistoryOpen = $state(false)
  let shareHistoryLoaded = $state(false)
  let shareHistoryLoading = $state(false)
  let shareHistoryPage = $state(1)
  let sharePendingRevoke = $state(null)
  let shareRevokeBusy = $state(false)
  let shareLoadVersion = 0
  let shareHistoryRequest = 0
  let sh = $state({ visibility: 'network', expiry: '0', password: '' })
  const SHARE_HISTORY_PAGE_SIZE = 5
  const publicShareAvailable = $derived(!!session.user?.public_share_available)
  const shareIdentity = $derived.by(() => {
    const name = session.user?.display_name?.trim() || ''
    if (sh.visibility === 'public') return name ? translate("documentDetail.sharedByValue", { "name": name }) : translate("documentDetail.sharedDocument")
    const host = session.user?.instance_host?.trim() || location.host
    if (name && host) return translate("documentDetail.sharedByValueValue", { "name": name, "host": host })
    if (name) return translate("documentDetail.sharedByValue", { "name": name })
    return translate("documentDetail.sharedFromValue", { "host": host })
  })

  function shareLinkURL(link) {
    return link.public_url || `${location.origin}/s/${link.token}`
  }

  function shareExpiry(link) {
    return link.expires_at ? translate("documentDetail.expiresValue", { "expires_at": fmtDate(link.expires_at) }) : translate("documentDetail.noExpiry")
  }

  function closeShare() {
    shareOpen = false
    sharePendingRevoke = null
    sh = { visibility: 'network', expiry: '0', password: '' }
    shareURL = ''
    shareLoadVersion++
    shareHistoryRequest++
  }

  async function loadActiveShares(user, documentID, version) {
    shareLoading = true
    shareError = ''
    try {
      const response = await listShareLinks({
        doc_id: Number(documentID), status: 'active', page_size: 50,
      })
      if (!disposed && shareOpen && session.user === user && id === documentID && version === shareLoadVersion) {
        shareLinks = response?.results || response || []
      }
    } catch (ex) {
      if (!disposed && shareOpen && session.user === user && id === documentID && version === shareLoadVersion) {
        shareLinks = []
        shareError = ex.message || translate("documentDetail.couldNotLoadActiveLinks")
      }
    } finally {
      if (!disposed && session.user === user && id === documentID && version === shareLoadVersion) shareLoading = false
    }
  }

  async function openShare() {
    const user = session.user
    const documentID = id
    const version = ++shareLoadVersion
    shareHistoryRequest++
    shareLinks = []
    shareLoading = false
    shareHistory = { count: 0, results: [] }
    shareHistoryOpen = false
    shareHistoryLoaded = false
    shareHistoryLoading = false
    shareHistoryPage = 1
    sharePendingRevoke = null
    shareBusy = false
    shareRevokeBusy = false
    shareError = ''
    shareURL = ''
    sh = { visibility: 'network', expiry: '0', password: '' }
    shareOpen = true
    await loadActiveShares(user, documentID, version)
  }

  async function loadShareHistory(page = 1) {
    const user = session.user
    const documentID = id
    const version = shareLoadVersion
    const request = ++shareHistoryRequest
    shareHistoryLoading = true
    shareError = ''
    try {
      const response = await listShareLinks({
        doc_id: Number(documentID), status: 'inactive', page, page_size: SHARE_HISTORY_PAGE_SIZE,
      })
      if (!disposed && shareOpen && session.user === user && id === documentID &&
          version === shareLoadVersion && request === shareHistoryRequest) {
        shareHistory = { count: response?.count || 0, results: response?.results || response || [] }
        shareHistoryPage = page
        shareHistoryLoaded = true
      }
    } catch (ex) {
      if (!disposed && shareOpen && session.user === user && id === documentID &&
          version === shareLoadVersion && request === shareHistoryRequest) {
        shareError = ex.message || translate("documentDetail.couldNotLoadLinkHistory")
      }
    } finally {
      if (!disposed && version === shareLoadVersion && request === shareHistoryRequest) shareHistoryLoading = false
    }
  }

  async function toggleShareHistory() {
    shareHistoryOpen = !shareHistoryOpen
    if (shareHistoryOpen && !shareHistoryLoaded) await loadShareHistory(1)
  }

  async function makeLink() {
    if (shareBusy) return
    const user = session.user
    const documentID = id
    const version = shareLoadVersion
    shareBusy = true
    try {
      const res = await createShareLink({
        doc_ids: [Number(documentID)], label: doc?.title || '',
        visibility: sh.visibility, expires_in_sec: Number(sh.expiry), password: sh.password,
      })
      if (disposed || !shareOpen || session.user !== user || id !== documentID || version !== shareLoadVersion) return
      shareURL = res?.public_url || `${location.origin}/s/${res?.token}`
      sh = { visibility: 'network', expiry: '0', password: '' }
      await loadActiveShares(user, documentID, version)
      await copyShareLink()
    } catch (ex) {
      if (!disposed && shareOpen && session.user === user && id === documentID && version === shareLoadVersion) {
        notify?.(ex.message || translate("documentDetail.couldNotCreateTheLink"))
      }
    } finally {
      if (!disposed && session.user === user && id === documentID && version === shareLoadVersion) shareBusy = false
    }
  }

  async function copyShareLink() {
    const user = session.user
    const documentID = id
    const url = shareURL
    const copied = await copyText(url)
    if (disposed || session.user !== user || id !== documentID || shareURL !== url) return
    notify?.(copied ? translate("documentDetail.shareLinkCopied") : translate("documentDetail.shareLinkReadySelectTheLinkAndCopy"))
  }

  async function copyExistingShare(link) {
    const user = session.user
    const documentID = id
    const url = shareLinkURL(link)
    const copied = await copyText(url)
    if (disposed || session.user !== user || id !== documentID) return
    notify?.(copied ? translate("documentDetail.shareLinkCopied") : translate("documentDetail.shareLinkReadySelectTheLinkAndCopy"))
  }

  async function copyExtractedText() {
    if (blurred || !doc?.content) return
    const scope = captureScope()
    const documentID = id
    const copied = await copyText(doc.content)
    if (disposed || !scopeCurrent(scope) || id !== documentID || blurred) return
    textCopyFallback = !copied
    textCopyStatus = copied ? translate("documentDetail.textCopied") : translate("documentDetail.selectTheTextBelowAndCopyItManually")
  }

  async function revokeShare() {
    const link = sharePendingRevoke
    if (!link || shareRevokeBusy) return
    const user = session.user
    const documentID = id
    const version = shareLoadVersion
    shareRevokeBusy = true
    try {
      await deleteShareLink(link.id)
      if (disposed || !shareOpen || session.user !== user || id !== documentID || version !== shareLoadVersion) return
      if (shareURL === shareLinkURL(link)) shareURL = ''
      sharePendingRevoke = null
      shareHistory = { count: 0, results: [] }
      shareHistoryOpen = false
      shareHistoryLoaded = false
      await loadActiveShares(user, documentID, version)
      notify?.(translate("documentDetail.linkRevoked"))
    } catch (ex) {
      if (!disposed && shareOpen && session.user === user && id === documentID && version === shareLoadVersion) {
        notify?.(ex.message || translate("documentDetail.couldNotRevoke"))
      }
    } finally {
      if (!disposed && version === shareLoadVersion) shareRevokeBusy = false
    }
  }

  async function trash() {
    const version = loadVersion
    trashBusy = true
    try {
      await deleteDocument(id)
      if (disposed || version !== loadVersion) return
      trashOpen = false
      notify?.(translate("documentDetail.movedToTrash"))
      go('#/documents')
    } catch (ex) {
      if (!disposed && version === loadVersion) notify?.(ex.message || translate("documentDetail.couldNotDelete"))
    } finally {
      if (!disposed && version === loadVersion) trashBusy = false
    }
  }

  async function restore() {
    const version = loadVersion
    recoveryBusy = true
    try {
      await restoreDocument(id)
      if (disposed || version !== loadVersion) return
      notify?.(translate("documentDetail.restored"))
      await load()
    } catch (ex) {
      if (!disposed && version === loadVersion) notify?.(ex.message || translate("documentDetail.couldNotRestore"))
    } finally {
      if (!disposed && version === loadVersion) recoveryBusy = false
    }
  }

  async function permanentlyDelete() {
    const version = loadVersion
    recoveryBusy = true
    try {
      await permanentlyDeleteDocument(id)
      if (disposed || version !== loadVersion) return
      deleteOpen = false
      notify?.(translate("documentDetail.permanentlyDeleted"))
      go('#/trash')
    } catch (ex) {
      if (!disposed && version === loadVersion) notify?.(ex.message || translate("documentDetail.couldNotPermanentlyDelete"))
    } finally {
      if (!disposed && version === loadVersion) recoveryBusy = false
    }
  }

  async function copyAddress() {
    const scope = captureScope()
    const address = doc?.jd_address
    if (!address) return
    const copied = await copyText(address)
    if (!disposed && scopeCurrent(scope)) notify?.(copied ? translate("documentDetail.filingAddressCopied") : translate("documentDetail.selectTheFilingAddressAndCopyItManually"))
  }
  $effect(() => {
    if (id === loadedID) return
    loadedID = id
    untrack(() => { revealed = false; accessOpen = false; void load() })
  })
</script>

{#snippet referenceSearch(field, editor, adding = false)}
  <form class="reference-editor" onsubmit={(event) => { event.preventDefault(); findReferenceTargets(field) }}>
    <input class="input" aria-label={translate("documentDetail.valueDocument", { "name": field.name })}
           placeholder={translate("documentDetail.searchTitleExactIdOrThisArchive")}
           value={editor.query}
           oninput={(event) => updateReferenceEditor(field.id, { query: event.currentTarget.value, results: [], selected: null, error: '' })} />
    <button class="btn sm" disabled={editor.busy || !editor.query.trim()}>{editor.busy ? translate("documentDetail.searching") : translate("documentDetail.search")}</button>
  </form>
  {#if editor.error}<p class="err" role="alert">{editor.error}</p>{/if}
  {#if editor.results.length}
    <div class="reference-results">
      {#each editor.results as target (target.id)}
        <button class="reference-choice" class:selected={Number(editor.selected?.id) === Number(target.id)}
                type="button" onclick={() => updateReferenceEditor(field.id, { selected: target, error: '' })}>
          <span>{target.title || translate("documentDetail.documentValue", { "id": target.id })}</span>
          <small>{target.jd_address || `#${target.id}`}{target.system_code ? translate("documentDetail.value", { "system_code": target.system_code }) : ''}</small>
        </button>
      {/each}
    </div>
  {/if}
  {#if editor.selected}
    <div class="reference-actions">
      <button class="btn primary sm" disabled={editor.busy} onclick={() => saveReference(field)}>{translate("documentDetail.useSelectedDocument")}</button>
      <button class="btn sm" disabled={editor.busy}
              onclick={() => adding ? cancelNewReference() : updateReferenceEditor(field.id, { open: false, selected: null, results: [], error: '' })}>
        {translate("common.cancel")}
      </button>
    </div>
  {/if}
{/snippet}
<div class="document-view">
<div class="toolbar">
  <a class="btn sm" href={filingHref(trashed ? '#/trash' : '#/documents')}><Icon name="left" size={13} /> {trashed ? translate("documentDetail.backToTrash") : translate("documentDetail.allDocuments")}</a>
  <span class="spacer" style="flex:1"></span>
  {#if doc}
    {#if doc.jd_address}
      <input class="input mono" style="max-width:210px" aria-label={translate("documentDetail.filingAddress")} readonly value={doc.jd_address}
        onclick={event => event.currentTarget.select()} />
      <button class="btn sm" onclick={copyAddress}>{translate("documentDetail.copyAddress")}</button>
    {/if}
    {#if highSensitivity}
      <!-- Persistent Reveal/Hide toggle. The in-panel Reveal button
           (inside the preview) still works — this one gives a symmetric
           way to re-hide without navigating away and back. -->
      <button class="btn sm" onclick={() => (revealed = !revealed)}
              title={revealed ? translate("documentDetail.hidePreviewExtractedText") : translate("documentDetail.revealPreviewExtractedText")}>
        <Icon name="eye" size={13} /> {revealed ? translate("documentDetail.hide") : translate("documentDetail.reveal")}
      </button>
    {/if}
    {#if canReadFile}<a class="btn sm" href={downloadPath(id)} download><Icon name="download" size={13} /> {translate("documentDetail.download")}</a>{/if}
    {#if canManageAccess}
      <button class="btn sm" onclick={() => (accessOpen = true)}><Icon name="shield" size={13} /> {translate("documentDetail.access")}</button>
    {/if}
    {#if canShareLinks}
      <button class="btn sm" onclick={openShare}><Icon name="link" size={13} /> {translate("documentDetail.share")}</button>
    {/if}
    {#if !trashed}
      <button class="btn sm" onclick={() => (documentLinkOpen = true)}>{translate("documentDetail.openOnMyPhone")}</button>
      <button class="btn sm danger" onclick={() => (trashOpen = true)}><Icon name="trash" size={13} /> {translate("documentDetail.trash")}</button>
    {/if}
  {/if}
</div>

{#if loading}
  <div class="detail" aria-label={translate("documentDetail.loadingDocument")}>
    <div class="preview"><div class="skel" style="width:28%;height:14px"></div></div>
    <div class="card" style="align-self:start">
      <div class="skel" style="width:55%;height:18px;margin-bottom:18px"></div>
      <div class="skel" style="width:82%;margin-bottom:10px"></div>
      <div class="skel" style="width:68%"></div>
    </div>
  </div>
{:else if err}
  <div class="err">{err}</div>
{:else if doc}
  {#if trashed}
    <section class="trash-notice" aria-label={translate("documentDetail.trashedDocument")}>
      <div>
        <h2><Icon name="trash" size={17} /> {translate("documentDetail.documentInTrash")}</h2>
        <p class="sub">{translate("documentDetail.readOnlyDeletesPermanentlyValueValue", { "deletes_at": fmtDate(doc.deletes_at), "changes": canRestore ? ' Restore to make changes.' : '' })}</p>
      </div>
      {#if canManageTrash}
        <div class="trash-actions">
          {#if canRestore}
            <button class="btn sm primary" disabled={recoveryBusy} onclick={restore}><Icon name="refresh" size={13} /> {recoveryBusy && !deleteOpen ? translate("documentDetail.restoring") : translate("documentDetail.restore")}</button>
          {/if}
          <button class="btn sm danger" disabled={recoveryBusy} onclick={() => (deleteOpen = true)}><Icon name="trash" size={13} /> {translate("documentDetail.deletePermanently")}</button>
        </div>
      {/if}
    </section>
  {/if}
  <div class="detail document-detail-grid">
    <div class="preview" class:blurred>
      {#if !canReadFile}
        <div class="reveal"><span class="sub">{translate("documentDetail.onlyTheOwnerOrAnAdministratorCan")}</span></div>
      {:else if blurred}
        <div class="reveal">
          <span class="pill danger">{sensitivityLabel(doc.sensitivity)}</span>
          <button class="btn" onclick={() => (revealed = true)}><Icon name="eye" size={14} /> {translate("documentDetail.revealPreview")}</button>
        </div>
      {:else if previewable}
        <!-- direct URL: session cookie authenticates; server CSP sandboxes;
             streaming + immutable-ETag caching come back for free -->
        <iframe src={previewPath(id, highSensitivity)} title={translate("documentDetail.documentPreview")}></iframe>
      {:else}
        <div class="reveal">
          <span class="pill" title={doc.mime_type}>{formatLabel(doc.mime_type) || translate("documentDetail.unknownFormat")}</span>
          <b style="font-size:.95rem">{translate("documentDetail.noInlinePreviewForThisFormat")}</b>
          <span class="sub" style="max-width:32ch;text-align:center">{translate("documentDetail.browsersCanTRenderThisFileInline")}</span>
          <a class="btn" href={downloadPath(id)} download><Icon name="download" size={13} /> {translate("documentDetail.downloadOriginal")}</a>
        </div>
      {/if}
    </div>

    <div class="detail-sidebar">
      <div class="card">
        {#if trashed}
          <h2 style="font-size:1.15rem;overflow-wrap:anywhere">{doc.title || translate("documentDetail.documentValue", { "id": doc.id })}</h2>
        {:else if editingTitle}
          <form class="title-form" onsubmit={(e) => { e.preventDefault(); editingTitle = false; save({ title: titleDraft }, translate("documentDetail.titleSaved")) }}>
            <input class="input" aria-label={translate("documentDetail.documentTitle")} bind:value={titleDraft} />
          </form>
        {:else}
          <div class="title-heading">
            <h2>
              <button class="title-rename" title={translate("documentDetail.rename")} onclick={() => (editingTitle = true)}>
                <span class="dot {sensDot(doc.sensitivity)}"></span>{doc.title || translate("documentDetail.documentValue", { "id": doc.id })}
              </button>
            </h2>
            {#if doc.title_owner === 'human'}
              <div class="title-pin">
                <Icon name="pin" size={13} />
                <span title={translate("documentDetail.suggestionsAndRulesWonTChangeThis")}>{translate("documentDetail.titlePinned")}</span>
                <button class="btn sm" disabled={titleOwnerBusy} onclick={releaseTitle}>
                  {titleOwnerBusy ? translate("documentDetail.unpinning") : translate("documentDetail.unpin")}
                </button>
              </div>
            {/if}
          </div>
        {/if}

        <dl class="kv" style="margin-top:12px">
          {#if doc.encryption_state === 'decrypted'}
            <dt>{translate("documentDetail.password")}</dt>
            <dd><DocumentUnlockStatus state={doc.encryption_state} expanded /></dd>
          {/if}
          <dt>{translate("documentDetail.filedUnder")}</dt>
          <dd>
            {#if !trashed && jdCategories.length}
              <select class="input" style="padding:4px 8px;font-size:.8rem"
                      value={doc.jd_category_id}
                      onchange={(e) => save({ jd_category_id: Number(e.target.value) }, translate("documentDetail.refiled"))}>
                {#each areaGroups as g}
                  <optgroup label={translate("documentDetail.valueValueValue", { "lo": g.lo, "lo2": g.lo + 9, "name": g.name })}>
                    {#each g.categories as c}<option value={c.id}>{c.code} {c.name}</option>{/each}
                  </optgroup>
                {/each}
              </select>
            {:else if doc.jd_category_code}
              <span class="chip">{doc.jd_category_code} {doc.jd_category_name}</span>
              <span class="sub" style="margin-left:6px">{doc.jd_area_name}</span>
            {:else}<span class="chip">{translate("documentDetail.categoryValue", { "jd_category_id": doc.jd_category_id })}</span>{/if}
          </dd>
          <dt>{translate("documentDetail.sensitivity")}</dt>
          <dd>
            {#if trashed}
              {sensitivityLabel(doc.sensitivity)}
            {:else}
            <select class="input" style="padding:4px 8px;font-size:.8rem"
                    aria-label={translate("documentDetail.sensitivity")}
                    value={doc.sensitivity || ''}
                    onchange={(e) => save({ sensitivity: e.target.value || null }, translate("documentDetail.sensitivitySaved"))}>
              <option value="">{translate("common.unset")}</option>
              {#each SENSITIVITY_OPTIONS as option (option.value)}
                <option value={option.value}>{option.label}</option>
              {/each}
            </select>
            {/if}
          </dd>
          <dt>{translate("documentDetail.added")}</dt><dd>{fmtDate(doc.added_at || doc.created_at)}</dd>
          {#if sourceGroupList.length}
            <dt>{translate("documentDetail.addedFrom")}</dt>
            <dd style="display:flex;flex-direction:column;gap:4px;min-width:0">
              {#each sourceGroupList as group, i}
                <div style="min-width:0;overflow-wrap:anywhere">
                  <span>{sourceLabel(group)}</span>
                  {#if group.places.length}<span class="sub"> · {group.places[0]}</span>{/if}
                  {#if i === 0 && sourceGroupList.length > 1}<span class="pill" style="margin-left:6px;font-size:.68rem">{translate("documentDetail.firstAdded")}</span>{/if}
                  {#if group.places.length > 1}
                    <details class="sub">
                      <summary>{group.places.length === 2 ? translate("documentDetail.earlierPlacesOne") : translate("documentDetail.earlierPlacesMany", { count: String(group.places.length - 1) })}</summary>
                      {#each group.places.slice(1) as place (place)}<div>{place}</div>{/each}
                    </details>
                  {/if}
                </div>
              {/each}
            </dd>
          {/if}
          {#if !archiveSerialNumberInMore}{@render archiveSerialNumberRow()}{/if}
          <dt>{translate("documentDetail.original")}</dt>
          <dd style="min-width:0;overflow-wrap:anywhere">
            {#if originalNameList.length}<div>{originalNameList[0].name}</div>{/if}
            <div class={originalNameList.length ? 'sub' : ''}>
              <span title={doc.mime_type}>{formatLabel(doc.mime_type)}</span> · {fmtBytes(doc.original_size)}{originalFileDate ? ` · ${translate("documentDetail.fileDatedValue", { date: fmtDate(originalFileDate) })}` : ''}
            </div>
            {#if originalNameList.length > 1}
              <details class="sub">
                <summary>{originalNameList.length === 2 ? translate("documentDetail.otherNamesOne") : translate("documentDetail.otherNamesMany", { count: String(originalNameList.length - 1) })}</summary>
                {#each originalNameList.slice(1) as entry (entry.name)}
                  <div>{entry.name} · {sourceLabel(entry.source)} · {fmtDate(entry.source.observed_at)}</div>
                {/each}
              </details>
            {/if}
          </dd>
          {#if doc.archive_blob}
            <dt>{translate("documentDetail.archive")}</dt>
            <dd title={doc.content ? translate("documentDetail.fullTextSearchableOcrExtractionPopulatedDocumentsContent") : translate("documentDetail.pdfWrapperOnlyNoOcrLayerInstallTesseract")}>
              {doc.content ? translate("documentDetail.searchablePdf") : translate("documentDetail.pdfPreviewNoOcr")} · {fmtBytes(doc.archive_size)}
            </dd>
          {/if}
          <dt>{translate("documentDetail.tags")}</dt>
          <dd class="document-tags">
            <div class="metadata-display">
              <div class="metadata-value tag-pills">
                {#each doc.tags || [] as slug}
                  <span class="pill">{slug}
                    {#if editingTags}
                      <button class="tag-remove" type="button" aria-label={translate("documentDetail.removeTagValue", { "slug": slug })}
                              disabled={tagsBusy || !tagOptions.some(tag => tag.slug === slug)}
                              onclick={() => changeTag(tagOptions.find(tag => tag.slug === slug), 'remove_tag')}><Icon name="x" size={12} /></button>
                    {/if}
                  </span>
                {:else}<span class="sub">{translate("documentDetail.noTags")}</span>{/each}
              </div>
              {#if !trashed && !editingTags}
                <div class="metadata-actions"><button class="btn sm" aria-label={translate("documentDetail.editTags")} type="button" onclick={startEditTags}>{translate("documentDetail.edit")}</button></div>
              {:else if editingTags && session.user?.role === 'admin'}
                <div class="metadata-actions"><a class="btn sm" href={filingHref(TAGS_SETTINGS_HASH)}>{translate("documentDetail.manageTags")}</a></div>
              {/if}
            </div>
            {#if editingTags}
              <div class="tag-actions">
                {#if tagLoadError}
                  <p class="err" role="alert">{tagLoadError}</p>
                  <button class="btn sm" type="button" disabled={tagsBusy} onclick={startEditTags}>{translate("documentDetail.reloadTags")}</button>
                {:else if tagsBusy && !tagOptions.length}
                  <span class="sub">{translate("documentDetail.loadingTags")}</span>
                {:else}
                  <TypeaheadPicker items={tagOptions} excludedValues={doc.tags || []} valueKey="slug"
                                   disabled={tagsBusy} label={translate("documentDetail.tagToAdd")}
                                   menuId="document-tag-options" placeholder={translate("tagPicker.typeToSelectATag")}
                                   emptyText={translate("tagPicker.noMatchingTags")}
                                   onChoose={(tag) => changeTag(tag, 'add_tag')} />
                {/if}
                <button class="btn sm" type="button" disabled={tagsBusy} onclick={() => { editingTags = false; tagsError = ''; tagLoadError = '' }}>{translate("documentDetail.done")}</button>
              </div>
              <span class="sub">{translate("documentDetail.changesSaveImmediately")}</span>
              {#if tagsError}<p class="err" role="alert">{tagsError}</p>{/if}
            {/if}
          </dd>
          <dt>{translate("documentDetail.correspondents")}</dt>
          <dd class="document-correspondents">
            {#if editingCorrespondent}
              <form class="tag-actions" onsubmit={saveCorrespondent}>
                <TypeaheadPicker items={correspondentChoices} disabled={correspondentBusy}
                                 label={translate("documentDetail.primaryCorrespondent")}
                                 menuId="document-correspondent-options"
                                 placeholder={translate("documentDetail.typeToSelectACorrespondent")}
                                 emptyText={translate("documentDetail.noMatchingCorrespondents")}
                                 initialQuery={correspondentChoices.find(item => String(item.id) === correspondentDraft)?.name || ''}
                                 clearOnChoose={false}
                                 onQueryChange={() => (correspondentDraft = '')}
                                 onChoose={chooseCorrespondent} />
                <button class="btn sm primary" type="submit" disabled={correspondentBusy || correspondentDraft === ''}>
                  {correspondentBusy ? translate("common.working") : translate("common.save")}
                </button>
                <button class="btn sm" type="button" disabled={correspondentBusy}
                        onclick={() => { editingCorrespondent = false; correspondentError = '' }}>{translate("common.cancel")}</button>
              </form>
              {#if correspondentError}<p class="err" role="alert">{correspondentError}</p>{/if}
            {:else}
              <div class="metadata-display">
                <div class="metadata-value">
                  {#each doc.correspondents || [] as correspondent}
                    <span class="pill" style="margin-right:5px">{correspondent.name} · {correspondent.role}</span>
                  {:else}
                    <span class="sub">{translate("documentDetail.noCorrespondent")}</span>
                  {/each}
                </div>
                {#if !trashed}
                  <div class="metadata-actions">
                    <button class="btn sm" aria-label={translate("documentDetail.editCorrespondent")} type="button" onclick={startEditCorrespondent}>{translate("documentDetail.edit")}</button>
                  </div>
                {/if}
              </div>
            {/if}
          </dd>
          <dt>{translate("documentDetail.languages")}</dt>
          <dd>
            {#if editingLanguages}
              <form onsubmit={(e) => { e.preventDefault(); saveLanguages() }} style="display:flex;gap:6px;align-items:center;flex-wrap:wrap">
                <input class="input mono" style="max-width:200px" bind:value={languagesDraft}
                       placeholder={translate("documentDetail.eGDeEnEmptyClears")} />
                <button class="btn sm" type="submit">{translate("common.save")}</button>
                <button class="btn sm" type="button" onclick={() => (editingLanguages = false)}>{translate("common.cancel")}</button>
                <span class="sub" style="width:100%">{translate("documentDetail.commaSeparatedIsoCodesEmptyClearsUnlocks")}</span>
              </form>
            {:else}
              <div class="metadata-display">
                <div class="metadata-value">
                  {#if doc.languages}
                    {#each doc.languages.split(',') as code}
                      <span class="pill">{code.trim()}</span>
                    {/each}
                    {#if doc.languages_locked}<span class="sub" title={translate("documentDetail.setByUserAutomaticDetectionWonT")}>{translate("documentDetail.locked")}</span>{/if}
                  {:else}
                    <span class="sub">{translate("documentDetail.notDetected")}</span>
                  {/if}
                </div>
                {#if !trashed}<div class="metadata-actions"><button class="btn sm" onclick={startEditLanguages} type="button">{translate("documentDetail.edit")}</button></div>{/if}
              </div>
            {/if}
          </dd>
          {#if showMoreFields}
            <div class="more-fields" bind:this={moreFieldsElement}>
              {#if archiveSerialNumberInMore}{@render archiveSerialNumberRow()}{/if}
              {#each displayedOrdinaryCustomFields as field (field.field_id)}
                {@const editor = customFieldEditors[field.field_id]}
                {@const label = customFieldLabel(field)}
                <dt>{label}</dt>
                <dd class="custom-field-value">
                  {#if editor?.open}
                    <form class="custom-field-editor" onsubmit={(event) => saveCustomField(event, field)}>
                      {#if field.data_type === 'bool'}
                        <select class="input" aria-label={label} value={editor.value}
                                onchange={(event) => updateCustomFieldEditor(field.field_id, { value: event.currentTarget.value })}>
                          <option value="true">{translate("documentDetail.yes")}</option>
                          <option value="false">{translate("documentDetail.no")}</option>
                        </select>
                      {:else if field.data_type === 'select'}
                        <select class="input" aria-label={label} value={editor.value}
                                onchange={(event) => updateCustomFieldEditor(field.field_id, { value: event.currentTarget.value })}>
                          <option value="">{translate("documentDetail.selectAChoice")}</option>
                          {#each field.extra_data?.choices || [] as choice}<option value={choice}>{choice}</option>{/each}
                        </select>
                      {:else if field.data_type === 'multi'}
                        <div class="custom-field-choices" aria-label={label}>
                          {#each field.extra_data?.choices || [] as choice}
                            <label>
                              <input type="checkbox" value={choice} checked={editor.value?.includes(choice)}
                                     onchange={(event) => updateCustomFieldEditor(field.field_id, {
                                       value: event.currentTarget.checked
                                         ? [...new Set([...(editor.value || []), choice])]
                                         : (editor.value || []).filter(value => value !== choice),
                                     })} />
                              {choice}
                            </label>
                          {/each}
                        </div>
                      {:else}
                        <input class="input" aria-label={label}
                               type={field.data_type === 'date' ? 'date' : field.data_type === 'url' ? 'url' : field.data_type === 'number' || field.data_type === 'monetary' ? 'number' : 'text'}
                               step={field.data_type === 'monetary' ? '0.01' : field.data_type === 'number' ? 'any' : undefined}
                               value={editor.value}
                               oninput={(event) => updateCustomFieldEditor(field.field_id, { value: event.currentTarget.value })} />
                      {/if}
                      <div class="custom-field-actions">
                        <button class="btn primary sm" disabled={editor.busy || !customFieldValueReady(field, editor)}>{editor.busy ? translate("common.saving") : translate("common.save")}</button>
                        <button class="btn sm" type="button" disabled={editor.busy}
                                onclick={() => { const next = { ...customFieldEditors }; delete next[field.field_id]; customFieldEditors = next }}>{translate("common.cancel")}</button>
                        {#if field.is_set}
                          <button class="btn danger sm" type="button" disabled={editor.busy} onclick={() => clearOrdinaryCustomField(field)}>{translate("documentDetail.clear")}</button>
                        {/if}
                      </div>
                      {#if editor.error}<p class="err" role="alert">{editor.error}</p>{/if}
                    </form>
                  {:else}
                    <div class="metadata-display">
                      <span class="metadata-value" class:sub={!field.is_set}>
                        {#if field.data_type === 'url' && field.value}
                          <a href={field.value} target="_blank" rel="noreferrer">{customFieldDisplay(field)}</a>
                        {:else}
                          {field.is_set ? customFieldDisplay(field) : translate("documentDetail.notSet")}
                        {/if}
                      </span>
                      {#if !trashed}<div class="metadata-actions"><button class="btn sm" type="button" onclick={() => startCustomFieldEdit(field)}>{field.is_set ? translate("documentDetail.edit") : translate("documentDetail.add")}</button></div>{/if}
                    </div>
                  {/if}
                </dd>
              {/each}
              {#if !trashed && availableOrdinaryCustomFields.length}
                <dt>{translate("documentDetail.customFields")}</dt>
                <dd class="custom-field-add">
                  {#if editingCustomFields}
                    <div class="metadata-display">
                      <select class="input" aria-label={translate("documentDetail.addCustomField")} value="" onchange={addCustomField}>
                        <option value="">{translate("documentDetail.addCustomField2")}</option>
                        {#each availableOrdinaryCustomFields as field (field.field_id)}
                          <option value={field.field_id}>{customFieldLabel(field)}</option>
                        {/each}
                      </select>
                      <div class="metadata-actions">
                        <button class="btn sm" type="button" onclick={() => (editingCustomFields = false)}>{translate("documentDetail.done")}</button>
                      </div>
                    </div>
                  {:else}
                    <div class="metadata-display">
                      <span class="metadata-value"></span>
                      <div class="metadata-actions">
                        <button class="btn sm" type="button" aria-label={translate("documentDetail.editCustomFields")} onclick={() => (editingCustomFields = true)}>{translate("documentDetail.edit")}</button>
                      </div>
                    </div>
                  {/if}
                </dd>
              {/if}
            </div>
          {/if}
        </dl>
        {#if hasMoreFields}
          <button class="more-fields-toggle" type="button" bind:this={moreToggleElement}
                  aria-expanded={showMoreFields} onclick={toggleMoreFields}>
            {showMoreFields ? translate("documentDetail.lessFields") : translate("documentDetail.moreFields")}
            {#if !showMoreFields && setCustomFieldCount}<span class="sub">· {setCustomFieldCount === 1 ? translate("documentDetail.moreFieldsOne") : translate("documentDetail.moreFieldsMany", { count: String(setCustomFieldCount) })}</span>{/if}
            <span class="more-fields-chevron" class:open={showMoreFields} aria-hidden="true">▾</span>
          </button>
        {/if}
        {#if shareURL}
          <div class="field" style="margin-top:12px;margin-bottom:0">
            <label for="share">{translate("documentDetail.shareLink")}</label>
            <input id="share" class="input mono" style="font-size:.76rem" readonly value={shareURL}
                   onclick={(event) => event.currentTarget.select()} />
          </div>
        {/if}
      </div>



      <section class="card related-card" aria-label={translate("documentDetail.documentContext")}>
        <div class="related-tabs" role="tablist" aria-label={translate("documentDetail.documentContext")}>
            {#each visibleRelatedTabs as tab}
              {@const count = tab.id === 'similar' ? 0 : relatedTabCount(tab.id)}
              <button id={`related-tab-${tab.id}`} type="button" role="tab"
                      aria-selected={relatedTab === tab.id}
                      aria-label={count > 0 ? translate("documentDetail.valueValue", { "label": tab.label, "count": count }) : tab.label}
                      aria-controls={`related-panel-${tab.id}`}
                      tabindex={relatedTab === tab.id ? 0 : -1}
                      class:on={relatedTab === tab.id}
                      onclick={() => selectRelatedTab(tab.id)}
                      onkeydown={handleRelatedTabKey}>
                {tab.shortLabel}
                {#if count > 0}<span class="pill">{count}</span>{/if}
              </button>
            {/each}
          </div>

          {#if relatedTab === 'similar'}
            <div id="related-panel-similar" class="related-panel" role="tabpanel"
                 aria-labelledby="related-tab-similar" tabindex="0">
              {#if similarLoading}
                <p class="sub">{translate("documentDetail.findingSimilarDocuments")}</p>
              {:else if similarError}
                <div class="inline-state err" role="alert">
                  <span>{similarError}</span>
                  <button class="btn sm" onclick={() => loadSimilarDocuments()}>{translate("common.retry")}</button>
                </div>
              {:else if similar?.results?.length}
                {#if similar.matched_on_title_only}
                  <div class="related-meta">
                    <span class="pill warn"
                          title={translate("documentDetail.onlyTheTitleWasAvailableWhenFinding")}>
                      {translate("documentDetail.titleOnly")}
                    </span>
                  </div>
                {/if}
                <div class="index" style="border:0">
                  {#each similar.results.slice(0, 6) as sd (sd.id)}
                    <a class="irow" href={filingHref(`#/doc/${sd.id}`)} style="padding:8px 4px">
                      <span class="dot"></span>
                      <span class="title grow">{sd.title || translate("documentDetail.documentValue", { "id": sd.id })}</span>
                      <span class="sub">{fmtDate(sd.created_at)}</span>
                    </a>
                  {/each}
                </div>
              {:else}
                <p class="sub related-empty">
                  {#if similar?.matched_on_title_only}
                    {translate("documentDetail.noSimilarDocumentsFoundFromTheTitle")}
                  {:else}
                    {translate("documentDetail.noSimilarDocumentsFound")}
                  {/if}
                </p>
              {/if}
            </div>
          {:else if relatedTab === 'backlinks'}
            <div id="related-panel-backlinks" class="related-panel" role="tabpanel"
                 aria-labelledby="related-tab-backlinks" tabindex="0">
              <div class="linked-sections">
                <section class="linked-section" aria-label={translate("documentDetail.thisDocumentLinksTo")}>
                  <div class="linked-section-head">
                    <div>
                      <strong>{translate("documentDetail.thisDocumentLinksTo")}</strong>
                    </div>
                    {#if availableDocumentLinkFields.length && !addingReference}
                      <button class="btn sm" onclick={startNewReference}>{translate("documentDetail.addLink")}</button>
                    {/if}
                  </div>

                  {#if customFieldsLoading && !documentLinkFields.length}
                    <p class="sub">{translate("documentDetail.loadingLinkTypes")}</p>
                  {:else if customFieldsError && !documentLinkFields.length}
                    <div class="inline-state err" role="alert">
                      <span>{customFieldsError}</span>
                      <button class="btn sm" onclick={() => loadCustomFieldDefinitions()}>{translate("common.retry")}</button>
                    </div>
                  {:else}
                    {#if outgoingDocumentLinkFields.length}
                      <div class="reference-list">
                        {#each outgoingDocumentLinkFields as field (field.id)}
                          {@const current = documentLinkValue(field.id)}
                          {@const editor = referenceEditor(field.id)}
                          <div class="reference-row">
                            <div class="reference-head">
                              <div class="grow">
                                <strong>{field.name}</strong>
                                <a class="reference-target" href={filingHref(`#/doc/${current.value.id}`, current.value.system_code)}>
                                  {current.value.title || translate("documentDetail.documentValue", { "id": current.value.id })}
                                </a>
                                {#if current.value.jd_address}<span class="sub">{current.value.jd_address}</span>{/if}
                                {#if !current.value.is_latest}<span class="pill warn">{translate("documentDetail.earlierVersion")}</span>{/if}
                              </div>
                              <div class="reference-actions">
                                <button class="btn sm" onclick={() => toggleReferenceEditor(field)}>{translate("documentDetail.changeLink")}</button>
                                <button class="btn sm" disabled={editor.busy} onclick={() => clearReference(field)}>{translate("documentDetail.removeLink")}</button>
                              </div>
                            </div>
                            {#if editor.open}
                              {@render referenceSearch(field, editor)}
                            {/if}
                          </div>
                        {/each}
                      </div>
                    {:else if !documentLinkFields.length}
                      {#if session.user?.role === 'admin'}
                        <a role="button" class="btn sm" href={filingHref(CUSTOM_FIELDS_SETTINGS_HASH)}>{translate("documentDetail.createDocumentLinkField")}</a>
                      {:else}
                        <p class="sub related-empty">{translate("documentDetail.noLinkTypesAreConfiguredAskAn")}</p>
                      {/if}
                    {:else}
                      <p class="sub related-empty">{translate("documentDetail.thisDocumentDoesNotLinkToAnother")}</p>
                    {/if}

                    {#if addingReference}
                      <div class="reference-add">
                        <div class="reference-add-head">
                          <label for="new-document-link-field">{translate("documentDetail.linkType")}</label>
                          <select id="new-document-link-field" class="input" value={newReferenceFieldID}
                                  onchange={selectNewReferenceField}>
                            <option value="">{translate("documentDetail.chooseALinkType")}</option>
                            {#each availableDocumentLinkFields as field (field.id)}
                              <option value={field.id}>{field.name}</option>
                            {/each}
                          </select>
                          <button class="btn sm" onclick={cancelNewReference}>{translate("common.cancel")}</button>
                        </div>
                        {#if selectedNewReferenceField}
                          {@render referenceSearch(selectedNewReferenceField, referenceEditor(selectedNewReferenceField.id), true)}
                        {:else}
                          <p class="sub">{translate("documentDetail.chooseHowThisDocumentIsLinked")}</p>
                        {/if}
                      </div>
                    {/if}
                  {/if}
                </section>

                <section class="linked-section" aria-label={translate("documentDetail.documentsLinkingToThisVersion")}>
                  <div class="linked-section-head">
                    <div>
                      <strong>{translate("documentDetail.documentsLinkingToThisVersion")}</strong>
                    </div>
                    <span class="pill">{backlinks.count}</span>
                  </div>
                  {#if backlinksLoading}
                    <p class="sub">{translate("documentDetail.loadingDocumentsLinkingHere")}</p>
                  {:else if backlinksError}
                    <div class="inline-state err" role="alert">
                      <span>{backlinksError}</span>
                      <button class="btn sm" onclick={() => loadBacklinks(id, backlinksPage)}>{translate("common.retry")}</button>
                    </div>
                  {:else if backlinks.results.length}
                    <div class="index" style="border:0">
                      {#each backlinks.results as source (`${source.id}:${source.field_id}`)}
                        <a class="irow" href={filingHref(`#/doc/${source.id}`, source.system_code)} style="padding:8px 4px">
                          <span class="dot" class:warn={!source.is_latest}></span>
                          <span class="grow">
                            <span class="title">{source.title || translate("documentDetail.documentValue", { "id": source.id })}</span>
                            <span class="sub" style="display:block">{translate("documentDetail.linkedHereAsValue", { "field_name": source.field_name })}</span>
                          </span>
                          {#if !source.is_latest}<span class="pill warn">{translate("documentDetail.earlierVersion")}</span>{/if}
                          {#if source.jd_address}<span class="sub">{source.jd_address}</span>{/if}
                        </a>
                      {/each}
                    </div>
                    {#if backlinks.previous || backlinks.next}
                      <div class="pager">
                        <button class="btn sm" disabled={!backlinks.previous || backlinksLoading} onclick={() => loadBacklinks(id, backlinksPage - 1)}>{translate("documentDetail.previous")}</button>
                        <span class="sub">{translate("documentDetail.pageValue", { "backlinksPage": backlinksPage })}</span>
                        <button class="btn sm" disabled={!backlinks.next || backlinksLoading} onclick={() => loadBacklinks(id, backlinksPage + 1)}>{translate("documentDetail.next")}</button>
                      </div>
                    {/if}
                  {:else}
                    <p class="sub related-empty">{translate("documentDetail.noDocumentsLinkToThisVersion")}</p>
                  {/if}
                </section>
              </div>
            </div>
          {:else if relatedTab === 'versions'}
            <div id="related-panel-versions" class="related-panel" role="tabpanel"
                 aria-labelledby="related-tab-versions" tabindex="0">
              {#if versionHistory.head_id && String(versionHistory.head_id) !== String(id)}
                <div class="version-notice">
                  {translate("documentDetail.youAreViewingAnEarlierVersion")}
                  <a href={filingHref(`#/doc/${versionHistory.head_id}`)}>{translate("documentDetail.openTheLatestVisibleVersion")}</a>
                </div>
              {/if}
              {#if versionHistory.can_upload}
                <div class="replacement">
                  <input bind:this={replacementInput} type="file" hidden onchange={chooseReplacement} />
                  {#if replacementFile}
                    <div class="replacement-file">
                      <span class="grow"><strong>{replacementFile.name}</strong><small>{fmtBytes(replacementFile.size)}</small></span>
                      <button class="btn primary sm" disabled={replacementBusy} onclick={uploadReplacement}>{replacementBusy ? translate("documentDetail.uploading") : replacementError ? translate("documentDetail.retryUpload") : translate("documentDetail.uploadReplacement")}</button>
                      <button class="btn sm" disabled={replacementBusy} onclick={cancelReplacement}>{translate("common.cancel")}</button>
                    </div>
                  {:else}
                    <button class="btn sm" onclick={() => replacementInput?.click()}>{translate("documentDetail.uploadReplacement")}</button>
                  {/if}
                  {#if replacementError}<p class="err" role="alert">{replacementError}</p>{/if}
                </div>
              {/if}
              {#if versionsLoading}
                <p class="sub">{translate("documentDetail.loadingVersionHistory")}</p>
              {:else if versionsError}
                <div class="inline-state err" role="alert">
                  <span>{versionsError}</span>
                  <button class="btn sm" onclick={() => loadVersions(id, versionsPage)}>{translate("common.retry")}</button>
                </div>
              {:else}
                <div class="index" style="border:0">
                  {#each versionHistory.results as v (v.id)}
                    <a class="irow" href={filingHref(`#/doc/${v.id}`)} style="padding:8px 4px">
                      <span class="dot" class:accent={String(v.id) === String(id)}></span>
                      <span class="title grow">#{v.id} {v.title || ''}</span>
                      {#if v.archive_serial_number != null}<span class="chip">{translate("common.asnValue", { value: String(v.archive_serial_number) })}</span>{/if}
                      {#if v.is_head}<span class="pill accent">{translate("documentDetail.latest")}</span>{/if}
                      {#if String(v.id) === String(id)}<span class="pill">{translate("documentDetail.viewing")}</span>{/if}
                      {#if !v.is_head}<span class="pill warn">{translate("documentDetail.earlier")}</span>{/if}
                      <span class="sub">{fmtDate(v.created_at)}</span>
                    </a>
                  {/each}
                </div>
                {#if versionHistory.previous || versionHistory.next}
                  <div class="pager">
                    <button class="btn sm" disabled={!versionHistory.previous || versionsLoading} onclick={() => loadVersions(id, versionsPage - 1)}>{translate("documentDetail.previous")}</button>
                    <span class="sub">{translate("documentDetail.pageValue2", { "versionsPage": versionsPage })}</span>
                    <button class="btn sm" disabled={!versionHistory.next || versionsLoading} onclick={() => loadVersions(id, versionsPage + 1)}>{translate("documentDetail.next")}</button>
                  </div>
                {/if}
              {/if}
            </div>
          {:else}
            <div id="related-panel-notes" class="related-panel" role="tabpanel"
                 aria-labelledby="related-tab-notes" tabindex="0">
              <section class="notes-panel" aria-label={translate("documentDetail.documentNotes")}>
                {#if !trashed}
                  <form class="note-composer" onsubmit={addNote}>
                    <textarea class="input" rows="3" maxlength="16384" aria-label={translate("documentDetail.addANote")}
                              placeholder={translate("documentDetail.addANotePlaceholder")}
                              bind:value={noteDraft} disabled={noteBusy}></textarea>
                    <div class="note-composer-actions">
                      <span class="sub">{translate("documentDetail.visibleToPeopleWhoCanOpenThis")}</span>
                      <button class="btn primary sm" disabled={noteBusy || !noteDraft.trim()}>
                        {noteBusy && noteEditID == null && notePendingDelete == null ? translate("documentDetail.adding") : translate("documentDetail.addNote")}
                      </button>
                    </div>
                  </form>
                {/if}

                {#if noteError}<p class="err" role="alert">{noteError}</p>{/if}
                <div class="note-list">
                  {#each doc.notes || [] as note (note.id)}
                    <article class="note-item">
                      <div class="note-meta">
                        <strong>{note.author}</strong>
                        <span class="sub">
                          {fmtDate(note.updated_at || note.created_at)}
                          {#if note.updated_at > note.created_at} {translate("documentDetail.edited")}{/if}
                        </span>
                      </div>
                      {#if noteEditID === note.id}
                        <form class="note-edit" onsubmit={(event) => saveNoteEdit(event, note)}>
                          <textarea class="input" rows="4" maxlength="16384" aria-label={translate("documentDetail.editNoteByValue", { "author": note.author })}
                                    bind:value={noteEditDraft} disabled={noteBusy}></textarea>
                          <div class="note-actions">
                            <button class="btn primary sm" disabled={noteBusy || !noteEditDraft.trim()}>
                              {noteBusy ? translate("common.saving") : translate("common.save")}
                            </button>
                            <button class="btn sm" type="button" disabled={noteBusy}
                                    onclick={() => { noteEditID = null; noteEditDraft = ''; noteError = '' }}>{translate("common.cancel")}</button>
                          </div>
                        </form>
                      {:else}
                        <p class="note-body">{note.note}</p>
                        {#if note.can_edit && !trashed}
                          <div class="note-actions">
                            <button class="btn sm" type="button" disabled={noteBusy} onclick={() => startNoteEdit(note)}>{translate("documentDetail.edit")}</button>
                            <button class="btn danger sm" type="button" disabled={noteBusy} onclick={() => (notePendingDelete = note)}>{translate("common.delete")}</button>
                          </div>
                        {/if}
                      {/if}
                    </article>
                  {:else}
                    <p class="sub note-empty">{trashed ? translate("documentDetail.noNotesOnThisVersion") : translate("documentDetail.noNotesYet")}</p>
                  {/each}
                </div>
              </section>
            </div>
          {/if}
        </section>

      {#if doc.content}
        <section class="card" aria-label={translate("documentDetail.extractedText")}>
          <h3 style="display:flex;align-items:center;gap:8px;flex-wrap:wrap">
            {translate("documentDetail.extractedText")}
            {#if blurred}
              <span class="pill danger" style="font-size:.7rem">{sensitivityLabel(doc.sensitivity)}</span>
              <button class="btn sm" style="margin-left:auto" onclick={() => (revealed = true)}><Icon name="eye" size={12} /> {translate("documentDetail.reveal")}</button>
            {:else}
              <button class="btn sm" style="margin-left:auto" onclick={copyExtractedText}>{translate("documentDetail.copyText")}</button>
            {/if}
          </h3>
          {#if !blurred && textCopyStatus}<p class="sub" role="status">{textCopyStatus}</p>{/if}
          {#if !blurred && textCopyFallback}
            <textarea class="input extracted-copy" aria-label={translate("documentDetail.fullExtractedTextForCopying")} readonly value={doc.content}
                      onclick={(event) => event.currentTarget.select()} rows="12"></textarea>
          {:else}
            <p class="sub extracted" class:blurred class:expanded={fullText}
               aria-hidden={blurred}>{blurred ? translate("documentDetail.revealToReadExtractedText") : fullText ? doc.content : doc.content.slice(0, 2000)}{!blurred && !fullText && doc.content.length > 2000 ? '…' : ''}</p>
            {#if !blurred && doc.content.length > 2000}
              <button class="btn sm" style="margin-top:10px" aria-expanded={fullText}
                      onclick={() => (fullText = !fullText)}>{fullText ? translate("documentDetail.showLess") : translate("documentDetail.readAll")}</button>
            {/if}
          {/if}
        </section>
      {/if}
    </div>
  </div>
{/if}
</div>

{#if accessOpen && access}
  <div class="modal-veil" onclick={() => (accessOpen = false)} role="presentation">
    <div class="modal" style="width:min(620px,94vw)" onclick={(e) => e.stopPropagation()} onkeydown={(e) => { if (e.key === 'Escape') accessOpen = false }} role="dialog" aria-label={translate("documentDetail.documentAccess")} tabindex="-1">
      <div class="modal-head">
        <h3>{translate("documentDetail.accessToValue", { "id": doc?.title || `Document #${id}` })} <span class="pill">{access.results?.length || 0}</span></h3>
        <button class="btn sm" onclick={() => (accessOpen = false)} title={translate("common.close")} aria-label={translate("documentDetail.closeAccess")}><Icon name="x" size={13} /></button>
      </div>
      <form class="toolbar" style="margin-bottom:{access.results?.length ? '10px' : '0'}" onsubmit={grantAccess}>
        <select class="input" style="flex:1;max-width:none;min-width:180px" bind:value={accessDraft.principal} aria-label={translate("documentDetail.personOrGroup")}>
          <option value="">{translate("documentDetail.selectAPersonOrGroup")}</option>
          {#if availablePrincipals('user').length}
            <optgroup label={translate("documentDetail.people")}>
              {#each availablePrincipals('user') as person (person.id)}
                <option value={principalKey(person)}>{person.name}{person.email && person.email !== person.name ? translate("documentDetail.value2", { "email": person.email }) : ''}</option>
              {/each}
            </optgroup>
          {/if}
          {#if availablePrincipals('group').length}
            <optgroup label={translate("documentDetail.groups")}>
              {#each availablePrincipals('group') as group (group.id)}
                <option value={principalKey(group)}>{group.name}</option>
              {/each}
            </optgroup>
          {/if}
        </select>
        <select class="input" style="max-width:130px" bind:value={accessDraft.perm_bits} aria-label={translate("documentDetail.accessLevel")}>
          {#each ACCESS_LEVELS as [value, label]}<option {value}>{label}</option>{/each}
        </select>
        <button class="btn primary sm" disabled={!accessDraft.principal}><Icon name="plus" size={12} /> {translate("documentDetail.add")}</button>
      </form>
      {#if access.results?.length}
        <div class="index" style="border:0">
          {#each access.results as grant (grant.id)}
            {@const principal = principalFor(grant)}
            <div class="irow" style="padding:7px 2px;cursor:default">
              <span class="dot" class:accent={grant.principal_kind === 'group'}></span>
              <span class="grow" style="min-width:0">
                <span class="title" style="display:block;font-size:.82rem">{principal?.name || translate("documentDetail.valueValue2", { "principal_kind": grant.principal_kind, "principal_id": grant.principal_id })}</span>
                <span class="sub" style="display:block;overflow:hidden;text-overflow:ellipsis">{principal?.email || (grant.principal_kind === 'group' ? translate("documentDetail.group") : '')}</span>
              </span>
              <select class="input" style="width:118px;padding:4px 7px;font-size:.76rem" value={String(grant.perm_bits)}
                      aria-label={translate("documentDetail.accessForValue", { "principal_kind": principal?.name || grant.principal_kind })}
                      title={accessLabel(grant.perm_bits)} onchange={(e) => changeAccess(grant, e.target.value)}>
                {#each ACCESS_LEVELS as [value, label]}<option {value}>{label}</option>{/each}
              </select>
              <button class="btn sm danger" title={translate("documentDetail.revokeAccess")} aria-label={translate("documentDetail.revokeAccess")} onclick={() => revokeAccess(grant)}><Icon name="trash" size={12} /></button>
            </div>
          {/each}
        </div>
      {/if}
    </div>
  </div>
{/if}

{#if notePendingDelete}
  <ConfirmDialog
    title={translate("documentDetail.deleteNote")}
    message={translate("documentDetail.theNoteByValueWillBePermanentlyDeleted", { "author": notePendingDelete.author })}
    confirmLabel={translate("documentDetail.deleteNote2")}
    busyLabel={translate("documentDetail.deleting")}
    busy={noteBusy}
    onConfirm={removeNote}
    onCancel={() => (notePendingDelete = null)} />
{/if}

{#if trashOpen}
  <ConfirmDialog
    title={translate("documentDetail.moveDocumentToTrash")}
    message={translate("documentDetail.valueWillMoveToTrashWhereItCan", { "id": doc?.title || `Document #${id}` })}
    confirmLabel={translate("documentDetail.moveToTrash")}
    busy={trashBusy}
    onConfirm={trash}
    onCancel={() => (trashOpen = false)} />
{/if}

{#if deleteOpen}
  <ConfirmDialog
    title={translate("documentDetail.deletePermanently2")}
    message={translate("documentDetail.valueWillBePermanentlyDeletedAndAnyShare", { "id": doc?.title || `Document #${id}` })}
    confirmLabel={translate("documentDetail.deletePermanently")}
    busyLabel={translate("documentDetail.deleting")}
    busy={recoveryBusy}
    onConfirm={permanentlyDelete}
    onCancel={() => (deleteOpen = false)} />
{/if}

{#if documentLinkOpen && doc && !trashed}
  <DocumentLinkDialog {id} onClose={() => (documentLinkOpen = false)} />
{/if}

{#if shareOpen}
  <div class="modal-veil" onclick={closeShare} role="presentation">
    <div class="modal share-modal" onclick={(e) => e.stopPropagation()} onkeydown={(e) => { if (e.key === 'Escape') closeShare() }} role="dialog" aria-label={translate("documentDetail.shareDocument")} tabindex="-1">
      <div class="modal-head">
        <h3>{translate("documentDetail.shareValue", { "id": doc?.title || `Document #${id}` })}</h3>
        <button class="btn sm" onclick={closeShare} title={translate("common.close")} aria-label={translate("documentDetail.closeSharing")}><Icon name="x" size={13} /></button>
      </div>
      <p class="sub share-intro">{translate("documentDetail.chooseWhereTheLinkWorksRecipientsDo")}</p>
      <div class="share-preview">
        <span>{translate("documentDetail.recipientsWillSee")}</span>
        <strong>{doc?.title || translate("documentDetail.documentValue", { "id": id })}</strong>
        <small>{shareIdentity}</small>
        <em>{translate("documentDetail.poweredBySuchi")}</em>
      </div>
      <ShareLinkForm bind:value={sh} publicAvailable={publicShareAvailable}
                     highSensitivity={highSensitivity} busy={shareBusy} onSubmit={makeLink} />
      {#if shareURL}
        <div class="toolbar share-created">
          <input class="input mono" aria-label={translate("documentDetail.shareLink")} readonly value={shareURL}
                 onclick={(event) => event.currentTarget.select()} />
          <button class="btn sm" onclick={copyShareLink}>{translate("documentDetail.copyLink")}</button>
        </div>
        <LinkQR url={shareURL} />
      {/if}

      <section class="share-section" aria-labelledby="active-share-links">
        <h3 id="active-share-links">{translate("documentDetail.activeLinks")}</h3>
        {#if shareLoading}
          <p class="sub" role="status">{translate("documentDetail.loadingActiveLinks")}</p>
        {:else if shareLinks.length}
          <div class="share-list">
            {#each shareLinks as link (link.id)}
              <div class="share-row">
                <div class="grow">
                  <div class="share-badges">
                    <span class="pill">{link.visibility === 'public' ? translate("documentDetail.publicInternet") : translate("documentDetail.privateNetwork")}</span>
                    <span class="pill" class:warn={link.has_password}>{link.has_password ? translate("documentDetail.password") : translate("documentDetail.noPassword")}</span>
                  </div>
                  <span class="mono share-link-value">{shareLinkURL(link)}</span>
                  <small>{shareExpiry(link)}</small>
                </div>
                <div class="share-actions">
                  <button class="btn sm" onclick={() => copyExistingShare(link)}>{translate("documentDetail.copy")}</button>
                  <button class="btn sm" onclick={() => (shareURL = shareLinkURL(link))}>{translate("documentDetail.show")}</button>
                  <button class="btn sm danger" onclick={() => (sharePendingRevoke = link)}>{translate("documentDetail.revoke")}</button>
                </div>
              </div>
            {/each}
          </div>
        {:else}
          <p class="sub">{translate("documentDetail.noActiveLinks")}</p>
        {/if}
      </section>

      <section class="share-section">
        <button class="btn sm share-history-toggle" aria-expanded={shareHistoryOpen} onclick={toggleShareHistory}>
          {translate("documentDetail.historyValue", { "count": shareHistoryLoaded ? ` (${shareHistory.count})` : '' })}
        </button>
        {#if shareHistoryOpen}
          {#if shareHistoryLoading}
            <p class="sub" role="status">{translate("documentDetail.loadingHistory")}</p>
          {:else if shareHistory.results.length}
            <div class="share-list history">
              {#each shareHistory.results as link (link.id)}
                <div class="share-row">
                  <div class="grow">
                    <div class="share-badges">
                      <span class="pill">{link.visibility === 'public' ? translate("documentDetail.publicInternet") : translate("documentDetail.privateNetwork")}</span>
                      <span class="pill">{link.revoked_at ? translate("documentDetail.revoked") : translate("documentDetail.expired")}</span>
                      <span class="pill" class:warn={link.has_password}>{link.has_password ? translate("documentDetail.password") : translate("documentDetail.noPassword")}</span>
                    </div>
                    <small>{shareExpiry(link)}</small>
                  </div>
                </div>
              {/each}
            </div>
            {#if shareHistory.count > SHARE_HISTORY_PAGE_SIZE}
              <div class="toolbar share-history-pages">
                <button class="btn sm" disabled={shareHistoryPage <= 1 || shareHistoryLoading}
                        onclick={() => loadShareHistory(shareHistoryPage - 1)}>{translate("documentDetail.previous")}</button>
                <span class="sub">{translate("documentDetail.pageValue3", { "shareHistoryPage": shareHistoryPage })}</span>
                <button class="btn sm" disabled={shareHistoryPage * SHARE_HISTORY_PAGE_SIZE >= shareHistory.count || shareHistoryLoading}
                        onclick={() => loadShareHistory(shareHistoryPage + 1)}>{translate("documentDetail.next")}</button>
              </div>
            {/if}
          {:else}
            <p class="sub">{translate("documentDetail.noExpiredOrRevokedLinks")}</p>
          {/if}
        {/if}
      </section>
      {#if shareError}<p class="err" role="alert">{shareError}</p>{/if}
    </div>
  </div>
{/if}

{#if sharePendingRevoke}
  <ConfirmDialog
    title={translate("documentDetail.revokeShareLink")}
    message={translate("documentDetail.futureAccessWillStopImmediatelyButRevocation")}
    confirmLabel={translate("documentDetail.revokeLink")}
    busyLabel={translate("documentDetail.revoking")}
    busy={shareRevokeBusy}
    onConfirm={revokeShare}
    onCancel={() => (sharePendingRevoke = null)} />
{/if}

{#snippet archiveSerialNumberRow()}
<dt>{translate("documentDetail.archiveSerialNumber")}</dt>
<dd>
  {#if trashed}
    {#if doc.archive_serial_number != null}
      <span class="chip">{translate("common.asnValue", { value: String(doc.archive_serial_number) })}</span>
    {:else}
      <span class="sub">{translate("documentDetail.notSet")}</span>
    {/if}
  {:else if editingArchiveSerialNumber}
    <form class="archive-serial-form" onsubmit={saveArchiveSerialNumber}>
      <input class="input mono" inputmode="numeric" maxlength="16"
             aria-label={translate("documentDetail.archiveSerialNumber")}
             bind:value={archiveSerialNumberDraft} />
      <button class="btn primary sm" type="submit" disabled={archiveSerialNumberBusy}>
        {archiveSerialNumberBusy ? translate("common.saving") : translate("common.save")}
      </button>
      <button class="btn sm" type="button" disabled={archiveSerialNumberBusy}
              onclick={cancelEditArchiveSerialNumber}>{translate("common.cancel")}</button>
      <span class="sub archive-serial-help">{translate("documentDetail.archiveSerialNumberHelp")}</span>
      {#if archiveSerialNumberError}<span class="row-error" role="alert">{archiveSerialNumberError}</span>{/if}
    </form>
  {:else}
    <div class="metadata-display">
      <span class="metadata-value">
        {#if doc.archive_serial_number != null}
          <span class="chip">{translate("common.asnValue", { value: String(doc.archive_serial_number) })}</span>
        {:else}
          <span class="sub">{translate("documentDetail.notSet")}</span>
        {/if}
      </span>
      <div class="metadata-actions">
        <button class="btn sm" type="button" onclick={startEditArchiveSerialNumber}>
          {translate("documentDetail.edit")}
        </button>
      </div>
    </div>
  {/if}
</dd>
{/snippet}

<style>
  .document-view { min-height:100%; }
  .detail-sidebar { display:flex; flex-direction:column; gap:14px; }
  .detail-sidebar > .card { flex:none; }
  @media (min-width: 1001px) {
    .document-view { display:flex; flex-direction:column; height:100%; min-height:0; }
    .document-view > .toolbar, .document-view > .trash-notice { flex:none; }
    .document-detail-grid { flex:1 1 0; min-height:0; overflow:hidden; align-items:stretch; }
    .document-detail-grid > .preview { display:flex; align-self:auto; flex-direction:column; height:100%; min-height:0; }
    .document-detail-grid > .preview iframe { flex:1; height:100%; min-height:0; }
    .detail-sidebar { height:100%; min-height:0; overflow-y:auto; overscroll-behavior:contain; scrollbar-gutter:stable; }
  }
  .title-form .input { width:100%; max-width:none; }
  .title-heading { display:grid; gap:4px; min-width:0; }
  .title-heading h2 { display:flex; align-items:flex-start; gap:7px; min-width:0; font-size:1.15rem; }
  .title-rename { all:unset; display:inline; min-width:0; overflow-wrap:anywhere; cursor:text; }
  .title-rename .dot { display:inline-block; margin-right:8px; }
  .title-pin { display:flex; flex-wrap:wrap; align-items:center; gap:6px; color:var(--muted); font-size:.75rem; }
  .title-pin .btn.sm { padding:1px 8px; font-size:.7rem; }
  .document-tags { min-width:0 }
  .more-fields { display:contents; }
  .more-fields-toggle { display:inline-flex; align-items:center; gap:6px; margin-top:10px; padding:4px 0; border:0; background:none; color:var(--accent); font:inherit; font-size:.82rem; cursor:pointer; }
  .more-fields-toggle:hover { text-decoration:underline; }
  .more-fields-toggle .sub { color:var(--muted); }
  .more-fields-chevron { display:inline-block; transition:transform 150ms ease; }
  .more-fields-chevron.open { transform:rotate(180deg); }
  @media (prefers-reduced-motion: reduce) { .more-fields-chevron { transition:none; } }
  .metadata-display { display:flex; align-items:flex-start; flex-wrap:wrap; gap:7px; width:100%; min-width:0; }
  .metadata-value { display:flex; flex:1 1 90px; align-items:center; flex-wrap:wrap; gap:6px; min-width:0; }
  .metadata-actions { display:flex; flex:0 1 auto; align-items:center; justify-content:flex-end; flex-wrap:wrap; gap:6px; margin-left:auto; }
  .metadata-actions .btn { flex:none; }
  .archive-serial-form { display:flex; align-items:center; flex-wrap:wrap; gap:7px; width:100%; }
  .archive-serial-form .input { flex:1 1 120px; width:auto; max-width:180px; padding:4px 8px; font-size:.8rem; }
  .archive-serial-help, .archive-serial-form .row-error { flex-basis:100%; white-space:normal; }
  .archive-serial-form .row-error { color:var(--danger); font-size:.72rem; }
  .tag-pills, .tag-actions { display:flex; flex-wrap:wrap; align-items:center; gap:6px }
  .tag-pills .pill { max-width:100%; white-space:normal }
  .tag-remove { display:inline-flex; align-items:center; justify-content:center; width:24px; height:24px; padding:0; border:0; background:transparent; color:inherit; cursor:pointer }
  .tag-remove:disabled { cursor:default; opacity:.5 }
  .tag-actions { margin:8px 0 4px }
  .tag-actions :global(.typeahead-picker) { flex:1 1 140px }
  .inline-state { display:flex; align-items:center; justify-content:space-between; gap:10px; }
  .custom-field-value { min-width:0; }
  .custom-field-add .input { flex:1 1 180px; width:auto; max-width:none; padding:4px 8px; font-size:.8rem; }
  .custom-field-editor { display:grid; gap:7px; width:100%; }
  .custom-field-editor > .input { width:100%; max-width:none; }
  .custom-field-actions { display:flex; align-items:center; flex-wrap:wrap; gap:6px; }
  .custom-field-editor .err { margin:0; font-size:.72rem; }
  .custom-field-choices { display:flex; flex-wrap:wrap; gap:6px 12px; padding:7px 9px; border:1px solid var(--line); border-radius:7px; background:var(--bg); }
  .custom-field-choices label { display:flex; align-items:center; gap:5px; font-size:.75rem; }
  .note-meta, .note-composer-actions, .note-actions { display:flex; align-items:center; gap:8px; }
  .note-composer { display:grid; gap:7px; padding:11px; border:1px solid var(--line); border-radius:9px; background:var(--bg); }
  .note-composer textarea, .note-edit textarea { width:100%; max-width:none; min-height:72px; resize:vertical; line-height:1.45; }
  .note-composer-actions { flex-wrap:wrap; justify-content:space-between; }
  .note-composer-actions .sub { font-size:.68rem; }
  .note-list { display:grid; gap:0; margin-top:12px; }
  .note-item { display:grid; gap:7px; padding:12px 1px; border-top:1px solid var(--line); }
  .note-meta { justify-content:space-between; }
  .note-meta strong { min-width:0; overflow:hidden; font-size:.78rem; text-overflow:ellipsis; white-space:nowrap; }
  .note-meta .sub { flex:none; font-size:.68rem; }
  .note-body { margin:0; overflow-wrap:anywhere; color:var(--ink); font-size:.82rem; line-height:1.5; white-space:pre-wrap; }
  .note-edit { display:grid; gap:7px; }
  .note-actions { justify-content:flex-end; }
  .note-empty { margin:3px 0 0; padding:13px 0 2px; border-top:1px solid var(--line); font-size:.78rem; }
  .related-card { container-type:inline-size; overflow:hidden; padding-top:14px; }
  .related-tabs { display:flex; gap:2px; margin:0 0 14px; border-bottom:1px solid var(--line); overflow-x:auto; }
  .related-tabs button { display:flex; align-items:center; flex:none; margin-bottom:-1px; padding:8px 7px; border:0; border-bottom:2px solid transparent; background:none; color:var(--muted); font:inherit; font-size:.8rem; font-weight:600; white-space:nowrap; cursor:pointer; }
  .related-tabs button:hover { color:var(--ink); }
  .related-tabs button.on { border-color:var(--accent); color:var(--accent); }
  .related-tabs .pill { margin-left:6px; padding:1px 6px; font-size:.62rem; }
  .related-panel { min-height:54px; outline:none; }
  .related-panel:focus-visible { border-radius:6px; outline:2px solid var(--accent); outline-offset:3px; }
  .related-meta { display:flex; align-items:center; flex-wrap:wrap; gap:6px; margin-bottom:8px; }
  .related-empty { margin:8px 4px 0; font-size:.8rem; font-style:italic; opacity:.75; }
  .linked-sections { display:grid; gap:16px; }
  .linked-section + .linked-section { padding-top:16px; border-top:1px solid var(--line); }
  .linked-section-head { display:flex; align-items:flex-start; justify-content:space-between; gap:12px; margin-bottom:10px; }
  .linked-section-head > div { display:flex; min-width:0; flex-direction:column; gap:2px; }
  .linked-section-head strong { font-size:.82rem; }
  .reference-add { margin-top:10px; padding:11px; border:1px solid var(--line); border-radius:9px; background:var(--bg); }
  .reference-add-head { display:grid; grid-template-columns:auto minmax(0,1fr) auto; align-items:center; gap:8px; }
  .reference-add-head label { color:var(--muted); font-size:.72rem; font-weight:650; }
  .reference-add > .sub { margin:8px 0 0; }
  .reference-list { display:grid; gap:10px; }
  .reference-row { padding:11px; border:1px solid var(--line); border-radius:9px; background:var(--bg); }
  .reference-head, .reference-actions, .replacement-file, .pager { display:flex; align-items:center; flex-wrap:wrap; gap:8px; }
  .reference-head { align-items:flex-start; }
  .reference-head .grow { display:flex; min-width:0; flex-direction:column; gap:3px; }
  .reference-actions { margin-left:auto; }
  .reference-target { overflow-wrap:anywhere; font-size:.84rem; font-weight:650; }
  .reference-editor { display:grid; grid-template-columns:minmax(0,1fr) auto; gap:7px; margin-top:10px; }
  .reference-results { display:grid; gap:5px; margin:8px 0; }
  .reference-choice { display:flex; align-items:flex-start; flex-direction:column; gap:2px; padding:8px 10px; border:1px solid var(--line); border-radius:8px; background:var(--surface); color:inherit; font:inherit; text-align:left; cursor:pointer; }
  .reference-choice:hover, .reference-choice.selected { border-color:var(--accent); background:var(--tint); }
  .reference-choice span { font-size:.78rem; font-weight:650; }
  .reference-choice small, .replacement-file small { display:block; color:var(--muted); font-size:.68rem; }
  .version-notice { margin-bottom:10px; padding:9px 10px; border:1px solid color-mix(in srgb, var(--accent) 32%, var(--line)); border-radius:8px; background:var(--tint); font-size:.78rem; }
  .version-notice a { margin-left:4px; font-weight:650; }
  .replacement { display:grid; gap:7px; margin-bottom:10px; }
  .replacement-file { padding:9px 10px; border:1px solid var(--line); border-radius:8px; background:var(--bg); }
  .replacement-file .grow { min-width:120px; }
  .pager { justify-content:flex-end; margin-top:10px; }
  .extracted { white-space: pre-wrap; overflow-wrap: anywhere; max-height: 220px; overflow: auto; font-size: .8rem; color: var(--muted); margin: 0; }
  .extracted.expanded { max-height: 65vh; }
  .extracted-copy { width: 100%; max-width: none; font-size: .8rem; }
  @container (max-width: 520px) {
    .related-tabs { gap:0; }
    .related-tabs button { padding-inline:4px; font-size:.72rem; }
    .related-tabs .pill { margin-left:3px; padding-inline:4px; font-size:.58rem; }
  }
  .trash-notice { display: flex; align-items: center; justify-content: space-between; gap: 16px; padding: 14px 16px; margin-bottom: 18px; background: var(--warn-soft); border: 1px solid var(--line); border-left: 3px solid var(--warn); border-radius: var(--r); }
  .trash-notice h2 { display: flex; align-items: center; gap: 8px; color: var(--warn); font-size: 1rem; }
  .trash-notice .sub { margin: 3px 0 0; color: var(--muted); font-size: .82rem; }
  .trash-actions { display: flex; flex-wrap: wrap; gap: 8px; }
  .trash-actions .btn { min-height: 40px; }
  @media (max-width: 700px) {
    .trash-notice { align-items: stretch; flex-direction: column; gap: 12px; }
    .reference-editor { grid-template-columns:1fr; }
    .reference-actions { margin-left:0; }
    .reference-add-head { grid-template-columns:1fr; }
    .replacement-file { align-items:stretch; flex-direction:column; }
  }
  .share-modal { width:min(620px,94vw); max-height:min(88vh,760px); overflow:auto; }
  .share-intro { color:var(--muted); font-size:.82rem; margin:0 0 12px; }
  .share-created { margin:12px 0; }
  .share-created input { flex:1; min-width:0; font-size:.74rem; }
  .share-section { margin-top:14px; padding-top:12px; border-top:1px solid var(--line); }
  .share-section > h3 { margin:0 0 7px; font-size:.85rem; }
  .share-list { display:grid; gap:7px; }
  .share-row { display:flex; gap:10px; align-items:center; padding:9px; border:1px solid var(--line); border-radius:9px; }
  .share-row > .grow { display:grid; gap:5px; min-width:0; }
  .share-row small { color:var(--muted); font-size:.7rem; }
  .share-badges, .share-actions { display:flex; gap:5px; flex-wrap:wrap; align-items:center; }
  .share-link-value { overflow:hidden; color:var(--muted); font-size:.7rem; text-overflow:ellipsis; white-space:nowrap; }
  .share-actions { justify-content:flex-end; }
  .share-history-toggle { margin-bottom:7px; }
  .share-history-pages { justify-content:flex-end; margin-top:8px; }
  @media (max-width: 600px) {
    .share-row { align-items:stretch; flex-direction:column; }
    .share-actions { justify-content:flex-start; }
  }
</style>
