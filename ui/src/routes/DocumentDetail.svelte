<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { scopedHash as filingHref } from '../lib/systems.svelte.js'
  import { captureScope, scopeCurrent } from '../lib/systems.svelte.js'
  import { onDestroy, untrack } from 'svelte'
  import { getDocument, getDocumentIntrinsic, patchDocument, deleteDocument, restoreDocument, permanentlyDeleteDocument, documentVersions, documentBacklinks, uploadDocumentVersion, setDocumentCustomField, clearDocumentCustomField, createShareLink, listShareLinks, deleteShareLink, previewPath, downloadPath, similarDocs, listGrants, putGrant, deleteGrant, listAllTags, listCustomFields, listDocuments, bulkEdit } from '../lib/api.js'
  import { go } from '../lib/router.svelte.js'
  import { SENSITIVITY_OPTIONS, fmtDate, fmtBytes, isHighSensitivity, sensDot, sensitivityLabel } from '../lib/format.js'
  import { session } from '../lib/session.svelte.js'
  import { hasCapability } from '../lib/capabilities.js'
  import { CUSTOM_FIELDS_SETTINGS_HASH, TAGS_SETTINGS_HASH } from '../lib/configuration.js'
  import Icon from '../lib/Icon.svelte'
  import TagPicker from '../lib/TagPicker.svelte'
  import DocumentUnlockStatus from '../lib/DocumentUnlockStatus.svelte'
  import ConfirmDialog from '../lib/ConfirmDialog.svelte'
  import { copyText } from '../lib/clipboard.js'
  import LinkQR from '../lib/LinkQR.svelte'
  import DocumentLinkDialog from '../lib/DocumentLinkDialog.svelte'
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
  let editingLanguages = $state(false)
  let languagesDraft = $state('')
  let editingTags = $state(false)
  let tagOptions = $state([])
  let tagsBusy = $state(false)
  let tagsError = $state('')
  let tagLoadError = $state('')
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
  let loadVersion = 0
  let tagRefreshVersion = 0
  let loadedID
  let disposed = false
  onDestroy(() => { disposed = true; loadVersion++ })

  const ACCESS_LEVELS = [
    ['1', 'View'],
    ['3', 'Edit'],
    ['7', 'Full control'],
  ]

  const RELATED_TABS = [
    { id: 'similar', label: 'Similar documents' },
    { id: 'backlinks', label: 'Linked documents' },
    { id: 'versions', label: 'Versions' },
  ]

  function relatedTabCount(tab) {
    return tab === 'backlinks' ? linkedDocumentCount : Math.max(0, versionHistory.count - 1)
  }

  function selectRelatedTab(tab) {
    relatedTab = tab
  }

  function handleRelatedTabKey(event) {
    const keys = ['ArrowLeft', 'ArrowRight', 'Home', 'End']
    if (!keys.includes(event.key)) return
    event.preventDefault()
    const tabs = [...event.currentTarget.parentElement.querySelectorAll('[role="tab"]')]
    const current = RELATED_TABS.findIndex(tab => tab.id === relatedTab)
    const next = event.key === 'Home' ? 0 : event.key === 'End' ? RELATED_TABS.length - 1
      : (current + (event.key === 'ArrowRight' ? 1 : -1) + RELATED_TABS.length) % RELATED_TABS.length
    relatedTab = RELATED_TABS[next].id
    tabs[next]?.focus()
  }

  const highSensitivity = $derived(isHighSensitivity(doc?.sensitivity))
  const blurred = $derived(highSensitivity && !revealed)
  const trashed = $derived(doc?.trashed_at != null)
  const canManageTrash = $derived(trashed && (session.user?.role === 'admin' || doc?.owner_id === session.user?.user_id))
  const canRestore = $derived(canManageTrash && doc?.deletes_at > Math.floor(Date.now() / 1000))
  const canReadFile = $derived(!trashed || canManageTrash)
  const canShareLinks = $derived(!trashed && hasCapability(session.user, 'share_links'))
  const ordinaryCustomFields = $derived((doc?.custom_fields || []).filter(field => field.data_type !== 'documentlink'))
  const documentLinkFields = $derived.by(() => {
    const fields = new Map()
    for (const field of customFieldDefinitions) {
      if (field.data_type === 'documentlink') fields.set(Number(field.id), { ...field })
    }
    for (const value of doc?.custom_fields || []) {
      if (value.data_type !== 'documentlink') continue
      fields.set(Number(value.field_id), { ...(fields.get(Number(value.field_id)) || {}), id: value.field_id, name: value.name })
    }
    return [...fields.values()].sort((a, b) => String(a.name || '').localeCompare(String(b.name || '')))
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
    similar = null
    addingReference = false
    newReferenceFieldID = ''
    similarLoading = false
    similarError = ''
    relatedTab = selectedRelatedTab
    access = null
    canManageAccess = false
    editingTitle = false
    editingLanguages = false
    editingTags = false
    tagOptions = []
    tagsBusy = false
    tagsError = ''
    tagLoadError = ''
    shareOpen = false
    shareLinks = []
    documentLinkOpen = false
    shareURL = ''
    trashOpen = false
    trashBusy = false
    deleteOpen = false
    recoveryBusy = false
    try {
      const loaded = await getDocument(documentID)
      if (version !== loadVersion || !scopeCurrent(scope)) return
      doc = loaded
      titleDraft = loaded.title
      if (loaded.trashed_at != null) return
      void loadVersions(documentID, 1, version, scope)
      void loadBacklinks(documentID, 1, version, scope)
      void loadCustomFieldDefinitions(version, scope)
      void loadSimilarDocuments(documentID, version, scope)
      void loadAccess(documentID, version, scope)
    } catch (ex) {
      if (version === loadVersion && scopeCurrent(scope)) err = ex.message || 'Could not load this document.'
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
        similarError = ex.message || 'Could not load similar documents.'
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
        versionsError = ex.message || 'Could not load version history.'
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
        backlinksError = ex.message || 'Could not load backlinks.'
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
        customFieldsError = ex.message || 'Could not load reference fields.'
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
      if (ex.status !== 403) notify?.(ex.message || 'Could not load document access')
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
    return ACCESS_LEVELS.find(([value]) => Number(value) === Number(bits))?.[1] || 'Custom'
  }

  function sourceLabel(source) {
    if (source.kind === 'upload') return source.label ? `Uploaded by ${source.label}` : 'Manual upload'
    if (source.kind === 'api') return source.label ? `API upload by ${source.label}` : 'API upload'
    if (source.kind === 'mailbox') return source.label || 'Mailbox'
    if (source.kind === 'watched_folder') return source.label || 'Watched folder'
    if (source.kind === 'import') return source.label || 'Import'
    return source.label || 'Unknown source'
  }

  function sourceDetail(source) {
    const detail = source.detail?.trim() || ''
    const label = source.label?.trim() || ''
    if (source.kind !== 'mailbox' || !label) return detail
    if (detail === label) return ''
    const repeatedPrefix = `${label} / `
    return detail.startsWith(repeatedPrefix) ? detail.slice(repeatedPrefix.length) : detail
  }

  function documentLinkValue(fieldID) {
    return (doc?.custom_fields || []).find(value =>
      Number(value.field_id) === Number(fieldID) && value.data_type === 'documentlink')
  }

  function customFieldDisplay(field) {
    if (field.data_type === 'bool') return field.value ? 'Yes' : 'No'
    if (field.data_type === 'date') return fmtDate(field.value)
    if (field.data_type === 'multi') return Array.isArray(field.value) ? field.value.join(', ') : ''
    return String(field.value ?? '')
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
        ? `These bytes already belong to live document #${existing}. Choose a different file.`
        : 'These bytes already belong to a live document. Choose a different file.'
    }
    if (error?.status === 413) return 'This file is larger than the configured upload limit.'
    if (error?.status === 403) return 'You no longer have permission to upload a replacement.'
    if (error?.status === 404) return 'This revision is no longer available for replacement.'
    if (error?.status === 409) return error.message || 'The replacement conflicts with a newer server state.'
    return error?.message || 'The replacement could not be uploaded. Retry keeps the same request key.'
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
      notify?.('Replacement uploaded. File processing continues in the background.')
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
        if (target.trashed_at != null) throw new Error('That document is in Trash.')
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
        error: results.length ? '' : 'No matching documents found.',
      })
    } catch (ex) {
      if (!disposed && version === loadVersion && id === documentID && scopeCurrent(scope)) {
        updateReferenceEditor(field.id, {
          busy: false,
          results: [],
          selected: null,
          error: ex.message || 'Could not find that document.',
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
      notify?.(`${field.name} updated`)
      await load(true)
    } catch (ex) {
      if (!disposed && version === loadVersion && id === documentID && scopeCurrent(scope)) {
        updateReferenceEditor(field.id, { busy: false, error: ex.message || 'Could not update this reference.' })
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
      notify?.(`${field.name} cleared`)
      await load(true)
    } catch (ex) {
      if (!disposed && version === loadVersion && id === documentID && scopeCurrent(scope)) {
        updateReferenceEditor(field.id, { busy: false, error: ex.message || 'Could not clear this reference.' })
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
      notify?.('Access granted')
    } catch (ex) { notify?.(ex.message || 'Could not grant access') }
  }

  async function changeAccess(grant, bits) {
    try {
      await putGrant('document', id, {
        principal_kind: grant.principal_kind,
        principal_id: grant.principal_id,
        perm_bits: Number(bits),
      })
      grant.perm_bits = Number(bits)
      notify?.('Access updated')
    } catch (ex) { notify?.(ex.message || 'Could not update access') }
  }

  async function revokeAccess(grant) {
    try {
      await deleteGrant('document', id, grant.principal_kind, grant.principal_id)
      access = { ...access, results: access.results.filter(g => g.id !== grant.id) }
      notify?.('Access revoked')
    } catch (ex) { notify?.(ex.message || 'Could not revoke access') }
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
        notify?.('Saved, but tags could not be refreshed. Reload to see their current state.')
      }
    }
  }

  async function save(patch, label) {
    const version = loadVersion
    try {
      await patchDocument(id, patch)
      if (disposed || version !== loadVersion) return
      doc = { ...doc, ...patch }
      if ('languages' in patch) doc.languages_locked = patch.languages !== ''
      await refreshReviewTags(version, id)
      if (disposed || version !== loadVersion) return
      notify?.(label || 'Saved')
    } catch (ex) {
      if (!disposed && version === loadVersion) notify?.(ex.message || 'Could not save')
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
      if (!disposed && version === loadVersion && scopeCurrent(scope)) tagLoadError = ex.message || 'Could not load tags'
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
        throw new Error(outcome?.code === 'forbidden' ? 'You do not have permission to edit this document.' : 'Could not update tags')
      }
      doc = { ...doc, tags: method === 'add_tag'
        ? [...new Set([...(doc.tags || []), tag.slug])].sort()
        : (doc.tags || []).filter(slug => slug !== tag.slug) }
      await refreshReviewTags(version, documentID)
      if (disposed || version !== loadVersion || !scopeCurrent(scope)) return false
      notify?.(method === 'add_tag' ? 'Tag added' : 'Tag removed')
      return true
    } catch (ex) {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) tagsError = ex.message || 'Could not update tags'
      return false
    } finally {
      if (!disposed && version === loadVersion && scopeCurrent(scope)) tagsBusy = false
    }
  }

  function startEditLanguages() {
    languagesDraft = doc?.languages || ''
    editingLanguages = true
  }

  function saveLanguages() {
    const trimmed = languagesDraft.trim()
    editingLanguages = false
    return save({ languages: trimmed }, trimmed ? 'Languages updated' : 'Languages cleared')
  }

  // ---- share dialog: expiry + optional password + existing links ----
  let shareOpen = $state(false)
  let shareLinks = $state([])
  let sh = $state({ expiry: '0', password: '' })
  const EXPIRIES = [['0', 'Never expires'], ['86400', '1 day'], ['604800', '7 days'], ['2592000', '30 days']]
  const shareIdentity = $derived.by(() => {
    const name = session.user?.display_name?.trim() || ''
    const host = session.user?.instance_host?.trim() || location.host
    if (name && host) return `Shared by ${name} · ${host}`
    if (name) return `Shared by ${name}`
    return `Shared from ${host}`
  })

  async function openShare() {
    const user = session.user
    const documentID = id
    shareLinks = []
    shareOpen = true
    try {
      const r = await listShareLinks()
      if (!disposed && session.user === user && id === documentID) {
        shareLinks = (r?.results || r || []).filter(l => (l.doc_ids || []).includes(Number(documentID)))
      }
    } catch {
      if (!disposed && session.user === user && id === documentID) shareLinks = []
    }
  }
  async function makeLink() {
    if (shareBusy) return
    const user = session.user
    const documentID = id
    shareBusy = true
    try {
      const res = await createShareLink({
        doc_ids: [Number(documentID)], label: doc?.title || '',
        expires_in_sec: Number(sh.expiry), password: sh.password,
      })
      if (disposed || session.user !== user || id !== documentID) return
      shareURL = res?.public_url || `${location.origin}/s/${res?.token}`
      sh = { expiry: '0', password: '' }
      openShare()
      await copyShareLink()
    } catch (ex) {
      if (!disposed && session.user === user && id === documentID) notify?.(ex.message || 'Could not create the link')
    }
    finally { shareBusy = false }
  }
  async function copyShareLink() {
    const user = session.user
    const documentID = id
    const url = shareURL
    const copied = await copyText(url)
    if (disposed || session.user !== user || id !== documentID || shareURL !== url) return
    notify?.(copied ? 'Share link copied' : 'Share link ready. Select the link and copy it manually.')
  }
  async function copyExtractedText() {
    if (blurred || !doc?.content) return
    const user = session.user
    const documentID = id
    const copied = await copyText(doc.content)
    if (disposed || session.user !== user || id !== documentID || blurred) return
    textCopyFallback = !copied
    textCopyStatus = copied ? 'Text copied' : 'Select the text below and copy it manually.'
  }
  async function revoke(l) {
    const user = session.user
    const documentID = id
    try {
      await deleteShareLink(l.id)
      if (disposed || session.user !== user || id !== documentID) return
      shareLinks = shareLinks.filter(x => x.id !== l.id)
      if (shareURL === (l.public_url || `${location.origin}/s/${l.token}`)) shareURL = ''
      notify?.('Link revoked')
    }
    catch (ex) {
      if (!disposed && session.user === user && id === documentID) notify?.(ex.message || 'Could not revoke')
    }
  }

  async function trash() {
    const version = loadVersion
    trashBusy = true
    try {
      await deleteDocument(id)
      if (disposed || version !== loadVersion) return
      trashOpen = false
      notify?.('Moved to trash')
      go('#/documents')
    } catch (ex) {
      if (!disposed && version === loadVersion) notify?.(ex.message || 'Could not delete')
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
      notify?.('Restored')
      await load()
    } catch (ex) {
      if (!disposed && version === loadVersion) notify?.(ex.message || 'Could not restore')
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
      notify?.('Permanently deleted')
      go('#/trash')
    } catch (ex) {
      if (!disposed && version === loadVersion) notify?.(ex.message || 'Could not permanently delete')
    } finally {
      if (!disposed && version === loadVersion) recoveryBusy = false
    }
  }

  async function copyAddress() {
    const scope = captureScope()
    const address = doc?.jd_address
    if (!address) return
    const copied = await copyText(address)
    if (!disposed && scopeCurrent(scope)) notify?.(copied ? 'Filing address copied' : 'Select the filing address and copy it manually.')
  }
  $effect(() => {
    if (id === loadedID) return
    loadedID = id
    untrack(() => { revealed = false; accessOpen = false; void load() })
  })
</script>

{#snippet referenceSearch(field, editor, adding = false)}
  <form class="reference-editor" onsubmit={(event) => { event.preventDefault(); findReferenceTargets(field) }}>
    <input class="input" aria-label={`${field.name} document`}
           placeholder="Search title, exact ID, or this archive's document URL"
           value={editor.query}
           oninput={(event) => updateReferenceEditor(field.id, { query: event.currentTarget.value, results: [], selected: null, error: '' })} />
    <button class="btn sm" disabled={editor.busy || !editor.query.trim()}>{editor.busy ? 'Searching…' : 'Search'}</button>
  </form>
  {#if editor.error}<p class="err" role="alert">{editor.error}</p>{/if}
  {#if editor.results.length}
    <div class="reference-results">
      {#each editor.results as target (target.id)}
        <button class="reference-choice" class:selected={Number(editor.selected?.id) === Number(target.id)}
                type="button" onclick={() => updateReferenceEditor(field.id, { selected: target, error: '' })}>
          <span>{target.title || `Document #${target.id}`}</span>
          <small>{target.jd_address || `#${target.id}`}{target.system_code ? ` · ${target.system_code}` : ''}</small>
        </button>
      {/each}
    </div>
  {/if}
  {#if editor.selected}
    <div class="reference-actions">
      <button class="btn primary sm" disabled={editor.busy} onclick={() => saveReference(field)}>Use selected document</button>
      <button class="btn sm" disabled={editor.busy}
              onclick={() => adding ? cancelNewReference() : updateReferenceEditor(field.id, { open: false, selected: null, results: [], error: '' })}>
        Cancel
      </button>
    </div>
  {/if}
{/snippet}
<div class="toolbar">
  <a class="btn sm" href={filingHref(trashed ? '#/trash' : '#/documents')}><Icon name="left" size={13} /> {trashed ? 'Back to Trash' : 'All documents'}</a>
  <span class="spacer" style="flex:1"></span>
  {#if doc}
    {#if doc.jd_address}
      <input class="input mono" style="max-width:210px" aria-label="Filing address" readonly value={doc.jd_address}
        onclick={event => event.currentTarget.select()} />
      <button class="btn sm" onclick={copyAddress}>Copy address</button>
    {/if}
    {#if highSensitivity}
      <!-- Persistent Reveal/Hide toggle. The in-panel Reveal button
           (inside the preview) still works — this one gives a symmetric
           way to re-hide without navigating away and back. -->
      <button class="btn sm" onclick={() => (revealed = !revealed)}
              title={revealed ? 'Hide preview + extracted text' : 'Reveal preview + extracted text'}>
        <Icon name="eye" size={13} /> {revealed ? 'Hide' : 'Reveal'}
      </button>
    {/if}
    {#if canReadFile}<a class="btn sm" href={downloadPath(id)} download><Icon name="download" size={13} /> Download</a>{/if}
    {#if canManageAccess}
      <button class="btn sm" onclick={() => (accessOpen = true)}><Icon name="shield" size={13} /> Access</button>
    {/if}
    {#if canShareLinks}
      <button class="btn sm" onclick={openShare}><Icon name="link" size={13} /> Share</button>
    {/if}
    {#if !trashed}
      <button class="btn sm" onclick={() => (documentLinkOpen = true)}>Open on my phone</button>
      <button class="btn sm danger" onclick={() => (trashOpen = true)}><Icon name="trash" size={13} /> Trash</button>
    {/if}
  {/if}
</div>

{#if loading}
  <div class="detail" aria-label="Loading document">
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
    <section class="trash-notice" aria-label="Trashed document">
      <div>
        <h2><Icon name="trash" size={17} /> Document in Trash</h2>
        <p class="sub">Read-only. Deletes permanently {fmtDate(doc.deletes_at)}.{canRestore ? ' Restore to make changes.' : ''}</p>
      </div>
      {#if canManageTrash}
        <div class="trash-actions">
          {#if canRestore}
            <button class="btn sm primary" disabled={recoveryBusy} onclick={restore}><Icon name="refresh" size={13} /> {recoveryBusy && !deleteOpen ? 'Restoring…' : 'Restore'}</button>
          {/if}
          <button class="btn sm danger" disabled={recoveryBusy} onclick={() => (deleteOpen = true)}><Icon name="trash" size={13} /> Delete permanently</button>
        </div>
      {/if}
    </section>
  {/if}
  <div class="detail">
    <div class="preview" class:blurred>
      {#if !canReadFile}
        <div class="reveal"><span class="sub">Only the owner or an administrator can preview files in Trash.</span></div>
      {:else if blurred}
        <div class="reveal">
          <span class="pill danger">{sensitivityLabel(doc.sensitivity)}</span>
          <button class="btn" onclick={() => (revealed = true)}><Icon name="eye" size={14} /> Reveal preview</button>
        </div>
      {:else if previewable}
        <!-- direct URL: session cookie authenticates; server CSP sandboxes;
             streaming + immutable-ETag caching come back for free -->
        <iframe src={previewPath(id, highSensitivity)} title="Document preview"></iframe>
      {:else}
        <div class="reveal">
          <span class="pill">{doc.mime_type || 'unknown format'}</span>
          <b style="font-size:.95rem">No inline preview for this format</b>
          <span class="sub" style="max-width:32ch;text-align:center">Browsers can't render this file inline. Download the original, or read the extracted text below.</span>
          <a class="btn" href={downloadPath(id)} download><Icon name="download" size={13} /> Download original</a>
        </div>
      {/if}
    </div>

    <div style="display:flex;flex-direction:column;gap:14px">
      <div class="card">
        {#if trashed}
          <h2 style="font-size:1.15rem;overflow-wrap:anywhere">{doc.title || `Document #${doc.id}`}</h2>
        {:else if editingTitle}
          <form onsubmit={(e) => { e.preventDefault(); editingTitle = false; save({ title: titleDraft }, 'Title saved') }}>
            <input class="input" bind:value={titleDraft} />
          </form>
        {:else}
          <h2 style="font-size:1.15rem">
            <button style="all:unset;cursor:text" title="Rename" onclick={() => (editingTitle = true)}>
              <span class="dot {sensDot(doc.sensitivity)}" style="display:inline-block;margin-right:8px"></span>{doc.title || `Document #${doc.id}`}
            </button>
          </h2>
        {/if}

        <dl class="kv" style="margin-top:12px">
          {#if doc.encryption_state === 'decrypted'}
            <dt>Password</dt>
            <dd><DocumentUnlockStatus state={doc.encryption_state} expanded /></dd>
          {/if}
          <dt>Filed under</dt>
          <dd>
            {#if !trashed && jdCategories.length}
              <select class="input" style="padding:4px 8px;font-size:.8rem"
                      value={doc.jd_category_id}
                      onchange={(e) => save({ jd_category_id: Number(e.target.value) }, 'Refiled')}>
                {#each areaGroups as g}
                  <optgroup label={`${g.lo}–${g.lo + 9} ${g.name}`}>
                    {#each g.categories as c}<option value={c.id}>{c.code} {c.name}</option>{/each}
                  </optgroup>
                {/each}
              </select>
            {:else if doc.jd_category_code}
              <span class="chip">{doc.jd_category_code} {doc.jd_category_name}</span>
              <span class="sub" style="margin-left:6px">{doc.jd_area_name}</span>
            {:else}<span class="chip">Category #{doc.jd_category_id}</span>{/if}
          </dd>
          <dt>Sensitivity</dt>
          <dd>
            {#if trashed}
              {sensitivityLabel(doc.sensitivity)}
            {:else}
            <select class="input" style="padding:4px 8px;font-size:.8rem"
                    aria-label="Sensitivity"
                    value={doc.sensitivity || ''}
                    onchange={(e) => save({ sensitivity: e.target.value || null }, 'Sensitivity saved')}>
              <option value="">Unset</option>
              {#each SENSITIVITY_OPTIONS as option (option.value)}
                <option value={option.value}>{option.label}</option>
              {/each}
            </select>
            {/if}
          </dd>
          <dt>Added</dt><dd>{fmtDate(doc.added_at || doc.created_at)}</dd>
          {#if doc.sources?.length}
            <dt>{doc.sources.length === 1 ? 'Source' : 'Sources'}</dt>
            <dd style="display:flex;flex-direction:column;gap:4px;min-width:0">
              {#each doc.sources as source, i}
                <div style="min-width:0;overflow-wrap:anywhere">
                  <span>{sourceLabel(source)}</span>
                  {#if sourceDetail(source)}<span class="sub"> · {sourceDetail(source)}</span>{/if}
                  {#if i === 0 && doc.sources.length > 1}<span class="pill" style="margin-left:6px;font-size:.68rem">first seen</span>{/if}
                </div>
              {/each}
            </dd>
          {/if}
          {#if doc.source_mtime || (doc.created_at && doc.created_at !== doc.added_at)}
            <dt title="Date carried from the source at ingest">Source date</dt>
            <dd>{fmtDate(doc.source_mtime || doc.created_at)}</dd>
          {/if}
          <dt>Original</dt><dd>{doc.mime_type} · {fmtBytes(doc.original_size)}</dd>
          {#if doc.archive_blob}
            <dt>Archive</dt>
            <dd title={doc.content ? 'Full-text searchable — OCR / extraction populated documents.content' : 'PDF wrapper only — no OCR layer. Install tesseract or ocrmypdf and rescan to make this searchable.'}>
              {doc.content ? 'searchable PDF' : 'PDF preview (no OCR)'} · {fmtBytes(doc.archive_size)}
            </dd>
          {/if}
          <dt>Tags</dt>
          <dd class="document-tags">
            <div class="tag-pills">
              {#each doc.tags || [] as slug}
                <span class="pill">{slug}
                  {#if editingTags}
                    <button class="tag-remove" type="button" aria-label={`Remove tag ${slug}`}
                            disabled={tagsBusy || !tagOptions.some(tag => tag.slug === slug)}
                            onclick={() => changeTag(tagOptions.find(tag => tag.slug === slug), 'remove_tag')}><Icon name="x" size={12} /></button>
                  {/if}
                </span>
              {:else}<span class="sub">No tags</span>{/each}
              {#if !trashed && !editingTags}<button class="btn sm" aria-label="Edit tags" type="button" onclick={startEditTags}>Edit</button>{/if}
              {#if session.user?.role === 'admin'}<a class="btn sm" href={filingHref(TAGS_SETTINGS_HASH)}>Manage tags</a>{/if}
            </div>
            {#if editingTags}
              <div class="tag-actions">
                {#if tagLoadError}
                  <p class="err" role="alert">{tagLoadError}</p>
                  <button class="btn sm" type="button" disabled={tagsBusy} onclick={startEditTags}>Reload tags</button>
                {:else if tagsBusy && !tagOptions.length}
                  <span class="sub">Loading tags…</span>
                {:else}
                  <TagPicker tags={tagOptions} excludedSlugs={doc.tags || []} disabled={tagsBusy}
                             label="Tag to add" menuId="document-tag-options"
                             onChoose={(tag) => changeTag(tag, 'add_tag')} />
                {/if}
                <button class="btn sm" type="button" disabled={tagsBusy} onclick={() => { editingTags = false; tagsError = ''; tagLoadError = '' }}>Done</button>
              </div>
              <span class="sub">Changes save immediately.</span>
              {#if tagsError}<p class="err" role="alert">{tagsError}</p>{/if}
            {/if}
          </dd>
          {#if doc.correspondents?.length}
            <dt>Correspondents</dt>
            <dd>{#each doc.correspondents as c}<span class="pill" style="margin-right:5px">{c.name} · {c.role}</span>{/each}</dd>
          {/if}
          {#each ordinaryCustomFields as field (field.field_id)}
            <dt>{field.name}</dt>
            <dd>
              {#if field.data_type === 'url' && field.value}
                <a href={field.value} target="_blank" rel="noreferrer">{field.value}</a>
              {:else}
                {customFieldDisplay(field)}
              {/if}
            </dd>
          {/each}
          <dt>Languages</dt>
          <dd>
            {#if editingLanguages}
              <form onsubmit={(e) => { e.preventDefault(); saveLanguages() }} style="display:flex;gap:6px;align-items:center;flex-wrap:wrap">
                <input class="input mono" style="max-width:200px" bind:value={languagesDraft}
                       placeholder="e.g. de,en — empty clears" />
                <button class="btn sm" type="submit">Save</button>
                <button class="btn sm" type="button" onclick={() => (editingLanguages = false)}>Cancel</button>
                <span class="sub" style="width:100%">Comma-separated ISO codes. Empty clears &amp; unlocks.</span>
              </form>
            {:else}
              {#if doc.languages}
                {#each doc.languages.split(',') as code}
                  <span class="pill" style="margin-right:5px">{code.trim()}</span>
                {/each}
                {#if doc.languages_locked}<span class="sub" title="Set by user; automatic detection won't overwrite">· locked</span>{/if}
              {:else}
                <span class="sub">not detected</span>
              {/if}
              {#if !trashed}<button class="btn sm" style="margin-left:8px" onclick={startEditLanguages} type="button">Edit</button>{/if}
            {/if}
          </dd>
        </dl>
        {#if shareURL}
          <div class="field" style="margin-top:12px;margin-bottom:0">
            <label for="share">Share link</label>
            <input id="share" class="input mono" style="font-size:.76rem" readonly value={shareURL}
                   onclick={(event) => event.currentTarget.select()} />
          </div>
        {/if}
      </div>


      {#if !trashed}
        <section class="card related-card" aria-label="Document insights">
          <div class="related-tabs" role="tablist" aria-label="Document insights">
            {#each RELATED_TABS as tab}
              <button id={`related-tab-${tab.id}`} type="button" role="tab"
                      aria-selected={relatedTab === tab.id}
                      aria-controls={`related-panel-${tab.id}`}
                      tabindex={relatedTab === tab.id ? 0 : -1}
                      class:on={relatedTab === tab.id}
                      onclick={() => selectRelatedTab(tab.id)}
                      onkeydown={handleRelatedTabKey}>
                {tab.label}
                {#if tab.id !== 'similar'}<span class="pill">{relatedTabCount(tab.id)}</span>{/if}
              </button>
            {/each}
          </div>

          {#if relatedTab === 'similar'}
            <div id="related-panel-similar" class="related-panel" role="tabpanel"
                 aria-labelledby="related-tab-similar" tabindex="0">
              {#if similarLoading}
                <p class="sub">Finding similar documents…</p>
              {:else if similarError}
                <div class="inline-state err" role="alert">
                  <span>{similarError}</span>
                  <button class="btn sm" onclick={() => loadSimilarDocuments()}>Retry</button>
                </div>
              {:else if similar?.results?.length}
                {#if similar.matched_on_title_only}
                  <div class="related-meta">
                    <span class="pill warn"
                          title="Only the title was available when finding similar documents.">
                      Title only
                    </span>
                  </div>
                {/if}
                <div class="index" style="border:0">
                  {#each similar.results.slice(0, 6) as sd (sd.id)}
                    <a class="irow" href={filingHref(`#/doc/${sd.id}`)} style="padding:8px 4px">
                      <span class="dot"></span>
                      <span class="title grow">{sd.title || `Document #${sd.id}`}</span>
                      <span class="sub">{fmtDate(sd.created_at)}</span>
                    </a>
                  {/each}
                </div>
              {:else}
                <p class="sub related-empty">
                  {#if similar?.matched_on_title_only}
                    No similar documents found from the title.
                  {:else}
                    No similar documents found.
                  {/if}
                </p>
              {/if}
            </div>
          {:else if relatedTab === 'backlinks'}
            <div id="related-panel-backlinks" class="related-panel" role="tabpanel"
                 aria-labelledby="related-tab-backlinks" tabindex="0">
              <div class="linked-sections">
                <section class="linked-section" aria-label="This document links to">
                  <div class="linked-section-head">
                    <div>
                      <strong>This document links to</strong>
                    </div>
                    {#if availableDocumentLinkFields.length && !addingReference}
                      <button class="btn sm" onclick={startNewReference}>Add link</button>
                    {/if}
                  </div>

                  {#if customFieldsLoading && !documentLinkFields.length}
                    <p class="sub">Loading link types…</p>
                  {:else if customFieldsError && !documentLinkFields.length}
                    <div class="inline-state err" role="alert">
                      <span>{customFieldsError}</span>
                      <button class="btn sm" onclick={() => loadCustomFieldDefinitions()}>Retry</button>
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
                                  {current.value.title || `Document #${current.value.id}`}
                                </a>
                                {#if current.value.jd_address}<span class="sub">{current.value.jd_address}</span>{/if}
                                {#if !current.value.is_latest}<span class="pill warn">Earlier version</span>{/if}
                              </div>
                              <div class="reference-actions">
                                <button class="btn sm" onclick={() => toggleReferenceEditor(field)}>Change link</button>
                                <button class="btn sm" disabled={editor.busy} onclick={() => clearReference(field)}>Remove link</button>
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
                        <a role="button" class="btn sm" href={filingHref(CUSTOM_FIELDS_SETTINGS_HASH)}>Create Document link field</a>
                      {:else}
                        <p class="sub related-empty">No link types are configured. Ask an administrator to create one.</p>
                      {/if}
                    {:else}
                      <p class="sub related-empty">This document does not link to another document yet.</p>
                    {/if}

                    {#if addingReference}
                      <div class="reference-add">
                        <div class="reference-add-head">
                          <label for="new-document-link-field">Link type</label>
                          <select id="new-document-link-field" class="input" value={newReferenceFieldID}
                                  onchange={selectNewReferenceField}>
                            <option value="">Choose a link type</option>
                            {#each availableDocumentLinkFields as field (field.id)}
                              <option value={field.id}>{field.name}</option>
                            {/each}
                          </select>
                          <button class="btn sm" onclick={cancelNewReference}>Cancel</button>
                        </div>
                        {#if selectedNewReferenceField}
                          {@render referenceSearch(selectedNewReferenceField, referenceEditor(selectedNewReferenceField.id), true)}
                        {:else}
                          <p class="sub">Choose how this document is linked.</p>
                        {/if}
                      </div>
                    {/if}
                  {/if}
                </section>

                <section class="linked-section" aria-label="Documents linking to this version">
                  <div class="linked-section-head">
                    <div>
                      <strong>Documents linking to this version</strong>
                    </div>
                    <span class="pill">{backlinks.count}</span>
                  </div>
                  {#if backlinksLoading}
                    <p class="sub">Loading documents linking here…</p>
                  {:else if backlinksError}
                    <div class="inline-state err" role="alert">
                      <span>{backlinksError}</span>
                      <button class="btn sm" onclick={() => loadBacklinks(id, backlinksPage)}>Retry</button>
                    </div>
                  {:else if backlinks.results.length}
                    <div class="index" style="border:0">
                      {#each backlinks.results as source (`${source.id}:${source.field_id}`)}
                        <a class="irow" href={filingHref(`#/doc/${source.id}`, source.system_code)} style="padding:8px 4px">
                          <span class="dot" class:warn={!source.is_latest}></span>
                          <span class="grow">
                            <span class="title">{source.title || `Document #${source.id}`}</span>
                            <span class="sub" style="display:block">Linked here as “{source.field_name}”</span>
                          </span>
                          {#if !source.is_latest}<span class="pill warn">Earlier version</span>{/if}
                          {#if source.jd_address}<span class="sub">{source.jd_address}</span>{/if}
                        </a>
                      {/each}
                    </div>
                    {#if backlinks.previous || backlinks.next}
                      <div class="pager">
                        <button class="btn sm" disabled={!backlinks.previous || backlinksLoading} onclick={() => loadBacklinks(id, backlinksPage - 1)}>Previous</button>
                        <span class="sub">Page {backlinksPage}</span>
                        <button class="btn sm" disabled={!backlinks.next || backlinksLoading} onclick={() => loadBacklinks(id, backlinksPage + 1)}>Next</button>
                      </div>
                    {/if}
                  {:else}
                    <p class="sub related-empty">No documents link to this version.</p>
                  {/if}
                </section>
              </div>
            </div>
          {:else}
            <div id="related-panel-versions" class="related-panel" role="tabpanel"
                 aria-labelledby="related-tab-versions" tabindex="0">
              {#if versionHistory.head_id && String(versionHistory.head_id) !== String(id)}
                <div class="version-notice">
                  You are viewing an earlier revision.
                  <a href={filingHref(`#/doc/${versionHistory.head_id}`)}>Open the latest visible revision</a>
                </div>
              {/if}
              {#if versionHistory.can_upload}
                <div class="replacement">
                  <input bind:this={replacementInput} type="file" hidden onchange={chooseReplacement} />
                  {#if replacementFile}
                    <div class="replacement-file">
                      <span class="grow"><strong>{replacementFile.name}</strong><small>{fmtBytes(replacementFile.size)}</small></span>
                      <button class="btn primary sm" disabled={replacementBusy} onclick={uploadReplacement}>{replacementBusy ? 'Uploading…' : replacementError ? 'Retry upload' : 'Upload replacement'}</button>
                      <button class="btn sm" disabled={replacementBusy} onclick={cancelReplacement}>Cancel</button>
                    </div>
                  {:else}
                    <button class="btn sm" onclick={() => replacementInput?.click()}>Upload replacement</button>
                  {/if}
                  {#if replacementError}<p class="err" role="alert">{replacementError}</p>{/if}
                </div>
              {/if}
              {#if versionsLoading}
                <p class="sub">Loading version history…</p>
              {:else if versionsError}
                <div class="inline-state err" role="alert">
                  <span>{versionsError}</span>
                  <button class="btn sm" onclick={() => loadVersions(id, versionsPage)}>Retry</button>
                </div>
              {:else}
                <div class="index" style="border:0">
                  {#each versionHistory.results as v (v.id)}
                    <a class="irow" href={filingHref(`#/doc/${v.id}`)} style="padding:8px 4px">
                      <span class="dot" class:accent={String(v.id) === String(id)}></span>
                      <span class="title grow">#{v.id} {v.title || ''}</span>
                      {#if v.is_head}<span class="pill accent">Latest</span>{/if}
                      {#if String(v.id) === String(id)}<span class="pill">Viewing</span>{/if}
                      {#if !v.is_head}<span class="pill warn">Earlier</span>{/if}
                      <span class="sub">{fmtDate(v.created_at)}</span>
                    </a>
                  {/each}
                </div>
                {#if versionHistory.previous || versionHistory.next}
                  <div class="pager">
                    <button class="btn sm" disabled={!versionHistory.previous || versionsLoading} onclick={() => loadVersions(id, versionsPage - 1)}>Previous</button>
                    <span class="sub">Page {versionsPage}</span>
                    <button class="btn sm" disabled={!versionHistory.next || versionsLoading} onclick={() => loadVersions(id, versionsPage + 1)}>Next</button>
                  </div>
                {/if}
              {/if}
            </div>
          {/if}
        </section>
      {/if}

      {#if doc.content}
        <section class="card" aria-label="Extracted text">
          <h3 style="display:flex;align-items:center;gap:8px;flex-wrap:wrap">
            Extracted text
            {#if blurred}
              <span class="pill danger" style="font-size:.7rem">{sensitivityLabel(doc.sensitivity)}</span>
              <button class="btn sm" style="margin-left:auto" onclick={() => (revealed = true)}><Icon name="eye" size={12} /> Reveal</button>
            {:else}
              <button class="btn sm" style="margin-left:auto" onclick={copyExtractedText}>Copy text</button>
            {/if}
          </h3>
          {#if !blurred && textCopyStatus}<p class="sub" role="status">{textCopyStatus}</p>{/if}
          {#if !blurred && textCopyFallback}
            <textarea class="input extracted-copy" aria-label="Full extracted text for copying" readonly value={doc.content}
                      onclick={(event) => event.currentTarget.select()} rows="12"></textarea>
          {:else}
            <p class="sub extracted" class:blurred class:expanded={fullText}
               aria-hidden={blurred}>{blurred ? 'Reveal to read extracted text.' : fullText ? doc.content : doc.content.slice(0, 2000)}{!blurred && !fullText && doc.content.length > 2000 ? '…' : ''}</p>
            {#if !blurred && doc.content.length > 2000}
              <button class="btn sm" style="margin-top:10px" aria-expanded={fullText}
                      onclick={() => (fullText = !fullText)}>{fullText ? 'Show less' : 'Read all'}</button>
            {/if}
          {/if}
        </section>
      {/if}
    </div>
  </div>
{/if}

{#if accessOpen && access}
  <div class="modal-veil" onclick={() => (accessOpen = false)} role="presentation">
    <div class="modal" style="width:min(620px,94vw)" onclick={(e) => e.stopPropagation()} onkeydown={(e) => { if (e.key === 'Escape') accessOpen = false }} role="dialog" aria-label="Document access" tabindex="-1">
      <div class="modal-head">
        <h3>Access to “{doc?.title || `Document #${id}`}” <span class="pill">{access.results?.length || 0}</span></h3>
        <button class="btn sm" onclick={() => (accessOpen = false)} title="Close" aria-label="Close access"><Icon name="x" size={13} /></button>
      </div>
      <form class="toolbar" style="margin-bottom:{access.results?.length ? '10px' : '0'}" onsubmit={grantAccess}>
        <select class="input" style="flex:1;max-width:none;min-width:180px" bind:value={accessDraft.principal} aria-label="Person or group">
          <option value="">Select a person or group</option>
          {#if availablePrincipals('user').length}
            <optgroup label="People">
              {#each availablePrincipals('user') as person (person.id)}
                <option value={principalKey(person)}>{person.name}{person.email && person.email !== person.name ? ` · ${person.email}` : ''}</option>
              {/each}
            </optgroup>
          {/if}
          {#if availablePrincipals('group').length}
            <optgroup label="Groups">
              {#each availablePrincipals('group') as group (group.id)}
                <option value={principalKey(group)}>{group.name}</option>
              {/each}
            </optgroup>
          {/if}
        </select>
        <select class="input" style="max-width:130px" bind:value={accessDraft.perm_bits} aria-label="Access level">
          {#each ACCESS_LEVELS as [value, label]}<option {value}>{label}</option>{/each}
        </select>
        <button class="btn primary sm" disabled={!accessDraft.principal}><Icon name="plus" size={12} /> Add</button>
      </form>
      {#if access.results?.length}
        <div class="index" style="border:0">
          {#each access.results as grant (grant.id)}
            {@const principal = principalFor(grant)}
            <div class="irow" style="padding:7px 2px;cursor:default">
              <span class="dot" class:accent={grant.principal_kind === 'group'}></span>
              <span class="grow" style="min-width:0">
                <span class="title" style="display:block;font-size:.82rem">{principal?.name || `${grant.principal_kind} #${grant.principal_id}`}</span>
                <span class="sub" style="display:block;overflow:hidden;text-overflow:ellipsis">{principal?.email || (grant.principal_kind === 'group' ? 'Group' : '')}</span>
              </span>
              <select class="input" style="width:118px;padding:4px 7px;font-size:.76rem" value={String(grant.perm_bits)}
                      aria-label={`Access for ${principal?.name || grant.principal_kind}`}
                      title={accessLabel(grant.perm_bits)} onchange={(e) => changeAccess(grant, e.target.value)}>
                {#each ACCESS_LEVELS as [value, label]}<option {value}>{label}</option>{/each}
              </select>
              <button class="btn sm danger" title="Revoke access" aria-label="Revoke access" onclick={() => revokeAccess(grant)}><Icon name="trash" size={12} /></button>
            </div>
          {/each}
        </div>
      {/if}
    </div>
  </div>
{/if}

{#if trashOpen}
  <ConfirmDialog
    title="Move document to trash?"
    message={`“${doc?.title || `Document #${id}`}” will move to Trash, where it can be restored.`}
    confirmLabel="Move to trash"
    busy={trashBusy}
    onConfirm={trash}
    onCancel={() => (trashOpen = false)} />
{/if}

{#if deleteOpen}
  <ConfirmDialog
    title="Delete permanently?"
    message={`“${doc?.title || `Document #${id}`}” will be permanently deleted, and any share link containing it will be revoked. This cannot be undone.`}
    confirmLabel="Delete permanently"
    busyLabel="Deleting…"
    busy={recoveryBusy}
    onConfirm={permanentlyDelete}
    onCancel={() => (deleteOpen = false)} />
{/if}

{#if documentLinkOpen && doc && !trashed}
  <DocumentLinkDialog {id} onClose={() => (documentLinkOpen = false)} />
{/if}

{#if shareOpen}
  <div class="modal-veil" onclick={() => (shareOpen = false)} role="presentation">
    <div class="modal" style="width:min(520px,94vw)" onclick={(e) => e.stopPropagation()} onkeydown={(e) => { if (e.key === 'Escape') shareOpen = false }} role="dialog" aria-label="Share document" tabindex="-1">
      <div class="modal-head">
        <h3>Share “{doc?.title || `Document #${id}`}”</h3>
        <button class="btn sm" onclick={() => (shareOpen = false)} title="Close" aria-label="Close sharing"><Icon name="x" size={13} /></button>
      </div>
      <p class="sub" style="color:var(--muted);font-size:.82rem;margin:0 0 12px">
        Anyone with the link can view and download. No account needed on their side.
      </p>
      <div class="share-preview">
        <span>Recipients will see</span>
        <strong>{doc?.title || `Document #${id}`}</strong>
        <small>{shareIdentity}</small>
        <em>Powered by suchi</em>
      </div>
      <div class="toolbar" style="margin:0 0 10px">
        <select class="input" bind:value={sh.expiry} aria-label="Link expiry">
          {#each EXPIRIES as [v, label]}<option value={v}>{label}</option>{/each}
        </select>
        <input class="input" type="password" style="flex:1" placeholder="Password (optional)"
               bind:value={sh.password} autocomplete="new-password" />
        <button class="btn primary sm" disabled={shareBusy} onclick={makeLink}><Icon name="link" size={13} /> {shareBusy ? 'Creating…' : 'Create & copy'}</button>
      </div>
      {#if shareURL}
        <div class="toolbar" style="margin-bottom:12px">
          <input class="input mono" style="flex:1;min-width:0;font-size:.74rem" aria-label="Share link" readonly value={shareURL}
                 onclick={(event) => event.currentTarget.select()} />
          <button class="btn sm" onclick={copyShareLink}>Copy link</button>
        </div>
        <LinkQR url={shareURL} />
      {/if}
      {#if shareLinks.length}
        <h3 style="font-size:.85rem;margin:6px 0 4px">Active links</h3>
        <div class="index" style="border:0">
          {#each shareLinks as l (l.id)}
            <div class="irow" style="padding:7px 2px">
              <span class="dot" class:warn={l.has_passwd || l.HasPasswd}></span>
              <span class="title grow mono" style="font-size:.74rem">{l.public_url || `/s/${l.token}`}</span>
              {#if l.has_passwd || l.HasPasswd}<span class="pill warn">password</span>{/if}
              <button class="btn sm" onclick={() => (shareURL = l.public_url || `${location.origin}/s/${l.token}`)}>Show link</button>
              <button class="btn sm" onclick={() => revoke(l)}>Revoke</button>
            </div>
          {/each}
        </div>
      {/if}
    </div>
  </div>
{/if}

<style>
  .document-tags { min-width:0 }
  .tag-pills, .tag-actions { display:flex; flex-wrap:wrap; align-items:center; gap:6px }
  .tag-pills .pill { max-width:100%; white-space:normal }
  .tag-remove { display:inline-flex; align-items:center; justify-content:center; width:24px; height:24px; padding:0; border:0; background:transparent; color:inherit; cursor:pointer }
  .tag-remove:disabled { cursor:default; opacity:.5 }
  .tag-actions { margin:8px 0 4px }
  .tag-actions :global(.tag-picker) { flex:1 1 140px }
  .inline-state { display:flex; align-items:center; justify-content:space-between; gap:10px; }
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
  @container (max-width: 380px) {
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
</style>
