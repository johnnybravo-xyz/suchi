<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { tick } from 'svelte'
  import { t as translate } from "../lib/i18n.svelte.js";
  import { customFieldLabel, plural } from '../lib/format.js'
  import { captureScope, scopeCurrent, systems } from '../lib/systems.svelte.js'
  import { go } from '../lib/router.svelte.js'
  import { adminListUsers, adminPatchUser,
           listGroups, createGroup, deleteGroup, groupMembers, addGroupMember, removeGroupMember,
           listCustomFields, createCustomField, patchCustomField, deleteCustomField,
           listAllTags, listCorrespondents,
           createTaxon, patchTaxon, deleteTaxon, deleteTags } from '../lib/api.js'
  import ConfirmDialog from '../lib/ConfirmDialog.svelte'
  import UserCreateForm from '../lib/UserCreateForm.svelte'
  import AdminTwoFactorResetDialog from '../lib/AdminTwoFactorResetDialog.svelte'
  import Icon from '../lib/Icon.svelte'
  import { USER_CAPABILITIES } from '../lib/capabilities.js'
  import { session } from '../lib/session.svelte.js'

  let { notify, view = 'people', initialPeople = '', initialMetadata = '' } = $props()
  let deleteRequest = $state(null)
  let deleteBusy = $state(false)
  let activeDelete = null
  const PEOPLE_TABS = ['users', 'groups']
  const tab = $derived(view === 'metadata'
    ? 'metadata'
    : PEOPLE_TABS.includes(initialPeople) ? initialPeople : 'users')

  function navigatePeople(nextTab, nextMetadata = 'tags') {
    if (tagDeleteBusy || taxMutationBusy || tagDeleteRequest || deleteBusy || deleteRequest) return
    const section = nextTab === 'metadata' ? 'metadata' : 'users'
    const params = new URLSearchParams({ tab: 'archive', section })
    if (nextTab !== 'metadata') params.set('people', nextTab)
    if (nextTab === 'metadata') params.set('metadata', nextMetadata)
    go(`#/settings?${params}`)
  }

  // ---------- users ----------
  let users = $state([])
  let usersLoaded = $state(false)
  let usersLoading = $state(false)
  let usersError = $state('')
  let userBusy = $state(false)
  let usersLoadVersion = 0
  let twoFactorResetUser = $state(null)
  let twoFactorResetTrigger

  function openTwoFactorReset(user, event) {
    twoFactorResetUser = user
    twoFactorResetTrigger = event.currentTarget
  }

  async function closeTwoFactorReset() {
    const returnFocus = twoFactorResetTrigger
    twoFactorResetUser = null
    twoFactorResetTrigger = undefined
    await tick()
    returnFocus?.focus()
  }

  async function twoFactorReset(user) {
    user.two_factor_enabled = false
    notify?.(translate('peopleSettings.twoFactorResetValue', { email: user.email }))
    await closeTwoFactorReset()
  }

  async function loadUsers() {
    const version = ++usersLoadVersion
    const scope = captureScope()
    usersLoading = true
    usersError = ''
    try {
      const r = await adminListUsers()
      if (version !== usersLoadVersion || !scopeCurrent(scope) || (tab !== 'users' && tab !== 'groups')) return
      users = r?.results || []
    } catch (ex) {
      if (version === usersLoadVersion && scopeCurrent(scope) && (tab === 'users' || tab === 'groups')) usersError = ex.message || translate("peopleSettings.couldNotLoadUsers")
    } finally {
      if (version === usersLoadVersion && scopeCurrent(scope) && (tab === 'users' || tab === 'groups')) {
        usersLoaded = true
        usersLoading = false
      }
    }
  }


  async function toggleUserCap(u, cap) {
    if (userBusy) return
    const has = (u.capabilities || []).includes(cap.key)
    const newCaps = has ? u.capabilities.filter(c => c !== cap.key) : [...(u.capabilities || []), cap.key]
    userBusy = true
    try {
      await adminPatchUser(u.id, { capabilities: newCaps })
      u.capabilities = newCaps
      notify?.(has ? translate("peopleSettings.revokedValueFromValue", { "label": cap.label, "email": u.email }) : translate("peopleSettings.grantedValueToValue", { "label": cap.label, "email": u.email }))
    } catch (ex) { notify?.(ex.message || translate("peopleSettings.couldNotUpdateCapabilities")) }
    finally { userBusy = false }
  }

  async function toggleDisabled(u) {
    if (userBusy || u.id === session.user?.user_id) return
    const disabled = !u.disabled
    userBusy = true
    try {
      await adminPatchUser(u.id, { disabled })
      u.disabled = disabled
      notify?.(disabled ? translate("peopleSettings.disabledValue", { "email": u.email }) : translate("peopleSettings.enabledValue", { "email": u.email }))
    } catch (ex) { notify?.(ex.message || translate("peopleSettings.couldNotUpdateUser")) }
    finally { userBusy = false }
  }

  // ---------- groups ----------
  let groups = $state([])
  let groupsLoaded = $state(false)
  let groupsLoading = $state(false)
  let groupsLoadVersion = 0
  let groupsError = $state('')
  let ngName = $state('')
  let openGroup = $state(null)      // {id, members: []}
  let addUID = $state('')

  function availableGroupUsers() {
    const memberIDs = new Set((openGroup?.members || []).map(m => Number(m.user_id ?? m.id)))
    return users.filter(u => !memberIDs.has(Number(u.id)))
  }
  async function loadGroups() {
    const version = ++groupsLoadVersion
    const scope = captureScope()
    groupsLoading = true
    groupsError = ''
    try {
      const r = await listGroups()
      if (version !== groupsLoadVersion || !scopeCurrent(scope) || tab !== 'groups') return
      groups = r?.results || r || []
      groupsLoaded = true
    } catch (ex) {
      if (version === groupsLoadVersion && scopeCurrent(scope) && tab === 'groups') groupsError = ex.message || translate("peopleSettings.couldNotLoadGroups")
    } finally {
      if (version === groupsLoadVersion && scopeCurrent(scope) && tab === 'groups') groupsLoading = false
    }
  }
  async function addGroup(e) {
    e.preventDefault()
    if (!ngName.trim()) return
    try { await createGroup({ name: ngName.trim() }); ngName = ''; notify?.(translate("peopleSettings.groupCreated")); loadGroups() }
    catch (ex) { notify?.(ex.message || translate("peopleSettings.couldNotCreateTheGroup")) }
  }
  function rmGroup(group) {
    requestDelete('group', group)
  }
  async function toggleMembers(g) {
    if (openGroup?.id === g.id) { openGroup = null; return }
    addUID = ''
    try { await reloadMembers(g) }
    catch (ex) { notify?.(ex.message || translate("peopleSettings.couldNotLoadMembers")) }
  }

  async function reloadMembers(g) {
    const r = await groupMembers(g.id)
    openGroup = { id: g.id, members: r?.results || r || [] }
  }

  async function addMember(g) {
    const uid = Number(addUID)
    if (!uid) return
    try { await addGroupMember(g.id, uid); addUID = ''; await reloadMembers(g); notify?.(translate("peopleSettings.memberAdded")) }
    catch (ex) { notify?.(ex.message || translate("peopleSettings.couldNotAddTheMember")) }
  }
  async function rmMember(g, uid) {
    try { await removeGroupMember(g.id, uid); openGroup = { ...openGroup, members: openGroup.members.filter(m => (m.user_id ?? m.id) !== uid) } }
    catch (ex) { notify?.(ex.message || translate("peopleSettings.couldNotRemove")) }
  }

  // ---------- custom fields ----------
  let fields = $state([])
  let fieldsLoaded = $state(false)
  let fieldsLoading = $state(false)
  let fieldsError = $state('')
  let nf = $state({ name: '', data_type: 'text', choices: '' })
  let fieldChoiceDrafts = $state({})
  let fieldChoiceBusy = $state(null)
  const FIELD_TYPES = {
    text: translate("peopleSettings.text"), number: translate("peopleSettings.number"), date: translate("peopleSettings.date"), bool: translate("peopleSettings.yesNo"),
    select: translate("peopleSettings.singleChoice"), multi: translate("peopleSettings.multipleChoices"), url: translate("peopleSettings.webLink"),
    monetary: translate("peopleSettings.money"), documentlink: translate("peopleSettings.documentLink"),
  }
  async function loadFields() {
    const version = ++fieldsLoadVersion
    const scope = captureScope()
    fieldsLoading = true
    fieldsLoaded = false
    fieldsError = ''
    try {
      const r = await listCustomFields()
      if (version !== fieldsLoadVersion || !scopeCurrent(scope) || tab !== 'metadata' || taxon !== 'custom_fields') return
      fields = r?.results || r || []
      fieldChoiceDrafts = Object.fromEntries(fields
        .filter(field => field.data_type === 'select' || field.data_type === 'multi')
        .map(field => [field.id, customFieldChoices(field).join('\n')]))
      fieldsLoaded = true
    } catch (ex) {
      if (version === fieldsLoadVersion && scopeCurrent(scope) && tab === 'metadata' && taxon === 'custom_fields') fieldsError = ex.message || translate("peopleSettings.couldNotLoadCustomFields")
    } finally {
      if (version === fieldsLoadVersion && scopeCurrent(scope) && tab === 'metadata' && taxon === 'custom_fields') fieldsLoading = false
    }
  }
  function customFieldChoices(field) {
    try {
      return JSON.parse(field.extra_data || '{}')?.choices || []
    } catch {
      return []
    }
  }
  function normalizedChoices(value) {
    return value.split('\n').map(choice => choice.trim()).filter(Boolean)
  }
  async function addField(e) {
    e.preventDefault()
    if (!nf.name.trim()) return
    const choiceType = nf.data_type === 'select' || nf.data_type === 'multi'
    const choices = normalizedChoices(nf.choices)
    if (choiceType && !choices.length) return
    const payload = { name: nf.name.trim(), data_type: nf.data_type }
    if (choiceType) payload.extra_data = JSON.stringify({ choices })
    try {
      await createCustomField(payload)
      nf = { name: '', data_type: 'text', choices: '' }
      notify?.(translate("peopleSettings.fieldCreated"))
      loadFields()
    } catch (ex) { notify?.(ex.message || translate("peopleSettings.couldNotCreateTheField")) }
  }
  async function renameField(field, name) {
    if (field.managed_by || !name.trim() || name === field.name) return
    try { await patchCustomField(field.id, { name: name.trim() }); field.name = name.trim(); notify?.(translate("peopleSettings.renamed")) }
    catch (ex) { notify?.(ex.message || translate("peopleSettings.couldNotRename")) }
  }
  function rmField(field) {
    if (field.managed_by) return
    requestDelete('custom_field', field)
  }
  async function saveFieldChoices(field) {
    if (field.managed_by) return
    const choices = normalizedChoices(fieldChoiceDrafts[field.id] || '')
    if (!choices.length || fieldChoiceBusy != null) return
    fieldChoiceBusy = field.id
    try {
      const extra_data = JSON.stringify({ choices })
      await patchCustomField(field.id, { extra_data })
      field.extra_data = extra_data
      fieldChoiceDrafts = { ...fieldChoiceDrafts, [field.id]: choices.join('\n') }
      notify?.(translate("peopleSettings.choicesUpdated"))
    } catch (ex) {
      notify?.(ex.code === 'choice_in_use'
        ? translate("peopleSettings.aChoiceStillUsedByADocumentCannot")
        : (ex.message || translate("peopleSettings.couldNotUpdateChoices")))
    } finally {
      fieldChoiceBusy = null
    }
  }

  // ---------- document metadata ----------
  const TAXA = [
    { kind: 'tags', label: translate("peopleSettings.tags"), description: translate("peopleSettings.labelsForFindingAndGroupingDocumentsAcrossYour"), load: listAllTags,
      placeholderKey: 'peopleSettings.newTagName', createKey: 'peopleSettings.createTag', existingKey: 'peopleSettings.existingTags', loadingKey: 'peopleSettings.loadingTags', emptyKey: 'peopleSettings.noTagsYet' },
    { kind: 'correspondents', label: translate("peopleSettings.correspondents"), description: translate("peopleSettings.peopleAndOrganizationsYouSendDocumentsToOr"), load: listCorrespondents,
      placeholderKey: 'peopleSettings.newCorrespondentName', createKey: 'peopleSettings.createCorrespondent', existingKey: 'peopleSettings.existingCorrespondents', loadingKey: 'peopleSettings.loadingCorrespondents', emptyKey: 'peopleSettings.noCorrespondentsYet' },
    { kind: 'custom_fields', label: translate("peopleSettings.customFields"), description: translate("peopleSettings.extraDocumentDetailsSuchAsAnInvoiceNumber") },
  ]
  const taxon = $derived(TAXA.some(t => t.kind === initialMetadata) ? initialMetadata : 'tags')
  const selectedTaxon = $derived(TAXA.find(t => t.kind === taxon))
  let taxRows = $state([])
  let taxRowsLoaded = $state(false)
  let taxRowsKind = $state('')
  let taxRowsLoading = $state(false)
  let taxRowsError = $state('')
  let taxLoadVersion = 0
  let fieldsLoadVersion = 0
  let tagSearch = $state('')
  let selectedTagIDs = $state(new Set())
  let tagDeleteRequest = $state(null)
  let tagDeleteBusy = $state(false)
  let activeTagDelete = null
  let tagDeleteError = $state('')
  let tagDeleteSuccess = $state('')
  let taxMutationBusy = $state(false)
  const matchingTags = $derived.by(() => {
    if (taxon !== 'tags' || taxRowsKind !== 'tags') return []
    const query = tagSearch.trim().toLowerCase()
    return query
      ? taxRows.filter(row => row.name?.toLowerCase().includes(query) || row.slug?.toLowerCase().includes(query))
      : taxRows
  })

  function toggleTag(id, checked) {
    if (tagDeleteBusy || tagDeleteRequest || taxMutationBusy) return
    const next = new Set(selectedTagIDs)
    if (checked) next.add(id)
    else next.delete(id)
    selectedTagIDs = next
  }
  function selectMatchingTags() {
    if (tagDeleteBusy || tagDeleteRequest || taxMutationBusy || !matchingTags.length || matchingTags.length > 500) return
    selectedTagIDs = new Set([...selectedTagIDs, ...matchingTags.map(row => row.id)])
  }
  function requestTagDelete(ids, name = '') {
    if (tagDeleteBusy || tagDeleteRequest || taxMutationBusy || !ids.length || ids.length > 500) return
    tagDeleteError = ''
    tagDeleteSuccess = ''
    tagDeleteRequest = { ids: [...ids], name, scope: captureScope() }
  }
  async function confirmTagDelete() {
    const request = tagDeleteRequest
    if (!request || tagDeleteBusy || taxMutationBusy || !scopeCurrent(request.scope) ||
        request.scope.code !== systems.code || tab !== 'metadata' || taxon !== 'tags') return
    const version = taxLoadVersion
    tagDeleteBusy = true
    activeTagDelete = request
    tagDeleteError = ''
    try {
      await deleteTags(request.ids)
      if (tagDeleteRequest !== request || version !== taxLoadVersion ||
          !scopeCurrent(request.scope) || request.scope.code !== systems.code ||
          tab !== 'metadata' || taxon !== 'tags') return
      const deleted = new Set(request.ids)
      taxRows = taxRows.filter(row => !deleted.has(row.id))
      selectedTagIDs = new Set([...selectedTagIDs].filter(id => !deleted.has(id)))
      tagDeleteSuccess = plural("peopleSettings.deletedTagCount", request.ids.length)
      tagDeleteRequest = null
      await loadTaxa()
    } catch (ex) {
      if (tagDeleteRequest === request && version === taxLoadVersion &&
          scopeCurrent(request.scope) && request.scope.code === systems.code &&
          tab === 'metadata' && taxon === 'tags') {
        tagDeleteError = ex.message || translate("peopleSettings.couldNotDeleteTagsPleaseTryAgain")
        tagDeleteRequest = null
      }
    } finally {
      if (activeTagDelete === request) {
        activeTagDelete = null
        tagDeleteBusy = false
      }
    }
  }
  let ntName = $state('')
  async function loadTaxa() {
    if (taxon === 'custom_fields') { await loadFields(); return }
    const version = ++taxLoadVersion
    const kind = taxon
    const scope = captureScope()
    const spec = TAXA.find(t => t.kind === kind)
    taxRowsLoading = true
    taxRowsLoaded = false
    taxRowsError = ''
    try {
      const r = await spec.load()
      if (version !== taxLoadVersion || !scopeCurrent(scope) || tab !== 'metadata' || kind !== taxon) return
      taxRows = r?.results || r || []
      taxRowsKind = kind
      taxRowsLoaded = true
      if (kind === 'tags') {
        const available = new Set(taxRows.map(row => row.id))
        selectedTagIDs = new Set([...selectedTagIDs].filter(id => available.has(id)))
      }
    } catch (ex) {
      if (version === taxLoadVersion && scopeCurrent(scope) && tab === 'metadata' && kind === taxon) taxRowsError = ex.message || translate("peopleSettings.couldNotLoadMetadata", { label: spec.label })
    } finally {
      if (version === taxLoadVersion && scopeCurrent(scope) && tab === 'metadata' && kind === taxon) taxRowsLoading = false
    }
  }
  async function addTaxon(e) {
    e.preventDefault()
    if (!ntName.trim() || taxMutationBusy || tagDeleteBusy || tagDeleteRequest) return
    const kind = taxon
    const scope = captureScope()
    taxMutationBusy = true
    try {
      await createTaxon(kind, { name: ntName.trim() })
      if (scopeCurrent(scope) && scope.code === systems.code && kind === taxon) {
        ntName = ''
        notify?.(translate("peopleSettings.created"))
        await loadTaxa()
      }
    } catch (ex) { if (scopeCurrent(scope) && kind === taxon) notify?.(ex.message || translate("peopleSettings.couldNotCreate")) }
    finally { taxMutationBusy = false }
  }
  async function renameTaxon(row, name) {
    if (!name.trim() || name === row.name || taxMutationBusy || tagDeleteBusy || tagDeleteRequest) return
    const kind = taxon
    const scope = captureScope()
    taxMutationBusy = true
    try {
      await patchTaxon(kind, row.id, { name: name.trim() })
      if (scopeCurrent(scope) && scope.code === systems.code && kind === taxon) {
        row.name = name.trim()
        notify?.(translate("peopleSettings.renamed"))
      }
    } catch (ex) { if (scopeCurrent(scope) && kind === taxon) notify?.(ex.message || translate("peopleSettings.couldNotRename")) }
    finally { taxMutationBusy = false }
  }
  function rmTaxon(row) {
    if (taxon === 'tags') { requestTagDelete([row.id], row.name); return }
    requestDelete('taxon', row, taxon)
  }

  function requestDelete(kind, item, metadataKind = '') {
    if (deleteBusy || deleteRequest) return
    deleteRequest = { kind, item, metadataKind, scope: captureScope() }
  }

  function deleteRequestIsCurrent(request) {
    if (!request || !scopeCurrent(request.scope) || request.scope.code !== systems.code) return false
    if (request.kind === 'group') return tab === 'groups'
    if (request.kind === 'custom_field') return tab === 'metadata' && taxon === 'custom_fields'
    return request.kind === 'taxon' && tab === 'metadata' && taxon === request.metadataKind
  }

  const deleteDialog = $derived.by(() => {
    const request = deleteRequest
    if (!request) return null
    if (request.kind === 'group') return {
      title: translate("peopleSettings.deleteGroupValue", { "name": request.item.name }),
      message: translate("peopleSettings.itsMembersWillBeRemovedSharedAccessMust"),
      confirmLabel: translate("peopleSettings.deleteGroup"),
    }
    if (request.kind === 'custom_field') return {
      title: translate("peopleSettings.deleteFieldValue", { "name": request.item.name }),
      message: translate("peopleSettings.itsValuesWillBeRemovedFromEveryDocument"),
      confirmLabel: translate("peopleSettings.deleteField"),
    }
    return {
      title: translate("peopleSettings.deleteValue", { "name": request.item.name }),
      message: translate("peopleSettings.documentsKeepWorkingButThisLabelWillBe"),
      confirmLabel: translate("peopleSettings.deleteCorrespondent"),
    }
  })

  async function confirmDelete() {
    const request = deleteRequest
    if (!deleteRequestIsCurrent(request) || deleteBusy) {
      if (request === deleteRequest) deleteRequest = null
      return
    }
    deleteBusy = true
    activeDelete = request
    try {
      if (request.kind === 'group') {
        await deleteGroup(request.item.id)
        if (deleteRequestIsCurrent(request)) {
          groups = groups.filter(group => group.id !== request.item.id)
          notify?.(translate("peopleSettings.groupDeleted"))
        }
      } else if (request.kind === 'custom_field') {
        await deleteCustomField(request.item.id)
        if (deleteRequestIsCurrent(request)) {
          fields = fields.filter(field => field.id !== request.item.id)
          notify?.(translate("peopleSettings.fieldDeleted"))
        }
      } else {
        await deleteTaxon(request.metadataKind, request.item.id)
        if (deleteRequestIsCurrent(request)) {
          taxRows = taxRows.filter(row => row.id !== request.item.id)
          notify?.(translate("peopleSettings.deleted"))
        }
      }
    } catch (ex) {
      if (deleteRequestIsCurrent(request)) {
        if (request.kind === 'group' && ex.code === 'delete_conflict') {
          notify?.(translate("peopleSettings.thisGroupIsStillUsedForSharedAccess"))
        } else {
          notify?.(ex.message || (request.kind === 'taxon' ? translate("peopleSettings.couldNotDeleteInUse") : translate("peopleSettings.couldNotDelete")))
        }
      }
    } finally {
      if (deleteRequest === request) deleteRequest = null
      if (activeDelete === request) {
        activeDelete = null
        deleteBusy = false
      }
    }
  }

  $effect(() => {
    const context = `${tab}:${tab === 'metadata' ? taxon : ''}:${systems.generation}:${systems.code}`
    // A pending request keeps its captured scope; a new panel must not receive its result.
    if (context) {
      selectedTagIDs = new Set()
      tagSearch = ''
      tagDeleteError = ''
      tagDeleteSuccess = ''
      tagDeleteRequest = null
      deleteRequest = null
      activeTagDelete = null
      tagDeleteBusy = false
    }
  })
  $effect(() => {
    const selected = tab === 'metadata' ? taxon : ''
    if (selected) {
      ntName = ''
      loadTaxa()
    }
    return () => { taxLoadVersion++; fieldsLoadVersion++ }
  })
  $effect(() => {
    if (tab === 'users') loadUsers()
    else if (tab === 'groups') { loadGroups(); loadUsers() }
    return () => { usersLoadVersion++; groupsLoadVersion++ }
  })
</script>

{#if view === 'people'}
  <nav class="people-nav" aria-label={translate("peopleSettings.people")}>
    <button class:on={tab === 'users'} aria-pressed={tab === 'users'} onclick={() => navigatePeople('users')}>{translate("peopleSettings.users")}</button>
    <button class:on={tab === 'groups'} aria-pressed={tab === 'groups'} onclick={() => navigatePeople('groups')}>{translate("peopleSettings.groups")}</button>
  </nav>
{/if}

<div class="people-content">
{#if tab === 'users'}
  <header class="panel-heading">
    <h3>{translate("peopleSettings.users")}</h3>
    <p>{translate("peopleSettings.manageWhoCanSignInAndWhat")}
      {#if systems.introduced}{translate("peopleSettings.accountsAreServerWideSystemMembershipAnd")}{/if}
    </p>
  </header>
  <section class="card editor" aria-labelledby="create-user-heading">
    <h4 id="create-user-heading">{translate("peopleSettings.createAUser")}</h4>
    <UserCreateForm {notify} onCreated={loadUsers} />
  </section>
  <section aria-labelledby="users-heading">
    <h4 class="list-heading" id="users-heading">{translate("peopleSettings.existingUsers")}</h4>
    {#if usersError}
      <div class="settings-load-state err"><span>{usersError}</span><button class="btn sm" onclick={loadUsers}>{translate("common.retry")}</button></div>
    {:else if !usersLoaded || usersLoading}
      <p class="quiet" role="status">{translate("peopleSettings.loadingUsers")}</p>
    {:else if users.length === 0}
      <p class="empty">{translate("peopleSettings.noUsersYet")}</p>
    {:else}
      <div class="index">
        {#each users as u (u.id)}
          {@const isCurrentUser = u.id === session.user?.user_id}
          <div class="user-entry">
            <div class="irow user-row">
              <span class="grow identity">
                <span class="title">{u.display_name || u.email}</span>
                {#if u.display_name}<span class="quiet">{u.email}</span>{/if}
              </span>
              <span class="pill">{u.role}</span>
              {#if u.two_factor_enabled}<span class="pill ok">{translate('peopleSettings.twoFactor')}</span>{/if}
              {#if u.two_factor_enabled && !isCurrentUser}
                <button class="btn sm" type="button" disabled={userBusy}
                        onclick={(event) => openTwoFactorReset(u, event)}>{translate('peopleSettings.resetTwoFactor')}</button>
              {/if}
              <span class="switch-control" title={isCurrentUser ? translate("peopleSettings.youCannotDisableYourOwnAccount") : undefined}>
                <span>{u.disabled ? translate("peopleSettings.disabled") : translate("peopleSettings.active")}{isCurrentUser ? ' (you)' : ''}</span>
                <button type="button" class="switch" role="switch" aria-checked={!u.disabled}
                        aria-label={translate("peopleSettings.userValueActive", { "email": u.email })} disabled={userBusy || isCurrentUser} onclick={() => toggleDisabled(u)}></button>
              </span>
            </div>
            {#if u.role !== 'admin'}
              <details class="user-access">
                <summary>{translate("peopleSettings.additionalAccess")} <span class="quiet">{translate("peopleSettings.valueGranted", { "length": (u.capabilities || []).length })}</span></summary>
                <div class="access-options">
                  {#each USER_CAPABILITIES as cap (cap.key)}
                    <label class="access-option">
                      <span><b>{cap.label}</b><small>{cap.description}</small></span>
                      <button type="button" class="switch" role="switch"
                              aria-checked={(u.capabilities || []).includes(cap.key)}
                              aria-label={translate("peopleSettings.valueCapabilityForValue", { "label": cap.label, "email": u.email })} disabled={userBusy}
                              onclick={() => toggleUserCap(u, cap)}></button>
                    </label>
                  {/each}
                </div>
              </details>
            {/if}
          </div>
        {/each}
      </div>
    {/if}
  </section>

{:else if tab === 'groups'}
  <header class="panel-heading">
    <h3>{translate("peopleSettings.groups")}</h3>
    <p>{translate("peopleSettings.shareDocumentsWithSeveralPeopleAtOnce")}
      {#if systems.introduced}{translate("peopleSettings.groupsAreServerWideJoiningOneDoes")}{/if}
    </p>
  </header>
  <form class="card editor" onsubmit={addGroup}>
    <h4>{translate("peopleSettings.createAGroup")}</h4>
    <label class="field">{translate("peopleSettings.groupName")}
      <input class="input" placeholder={translate("peopleSettings.eGFamilyOrAccountsTeam")} bind:value={ngName} required />
    </label>
    <div class="form-actions"><button class="btn primary" disabled={!ngName.trim()}><Icon name="plus" size={14} />{translate("peopleSettings.createGroup")}</button></div>
  </form>
  <section aria-labelledby="groups-heading">
    <h4 class="list-heading" id="groups-heading">{translate("peopleSettings.existingGroups")}</h4>
    {#if groupsError}
      <div class="settings-load-state err"><span>{groupsError}</span><button class="btn sm" onclick={loadGroups}>{translate("common.retry")}</button></div>
    {:else if !groupsLoaded || groupsLoading}
      <p class="quiet" role="status">{translate("peopleSettings.loadingGroups")}</p>
    {:else}
      <div class="index">
        {#each groups as g (g.id)}
          <div class="group-entry">
            <div class="irow">
              <span class="title grow">{g.name}</span>
              <button class="btn sm" disabled={deleteBusy || !!deleteRequest} onclick={() => toggleMembers(g)} aria-expanded={openGroup?.id === g.id} aria-controls={`group-members-${g.id}`}>{openGroup?.id === g.id ? translate("peopleSettings.hideMembers") : translate("peopleSettings.members")}</button>
              <button class="btn sm danger" disabled={deleteBusy || !!deleteRequest} onclick={() => rmGroup(g)} title={translate("peopleSettings.deleteGroup")} aria-label={translate("peopleSettings.deleteValue2", { "name": g.name })}><Icon name="trash" size={13} /></button>
            </div>
            {#if openGroup?.id === g.id}
              <div class="group-members" id={`group-members-${g.id}`}>
                {#each openGroup.members as mrow ((mrow.user_id ?? mrow.id))}
                  <div class="irow member-row">
                    <span class="grow identity">
                      <span class="title">{mrow.display_name || mrow.email}</span>
                      {#if mrow.display_name}<span class="quiet">{mrow.email}</span>{/if}
                    </span>
                    <button class="btn sm" aria-label={translate("peopleSettings.removeValue", { "email": mrow.email })} onclick={() => rmMember(g, mrow.user_id ?? mrow.id)}>{translate("peopleSettings.remove")}</button>
                  </div>
                {:else}
                  <p class="quiet">{translate("peopleSettings.noMembersYetAddAPersonBelow")}</p>
                {/each}
                <div class="member-add">
                  <label class="field">{translate("peopleSettings.userToAdd")}
                    <select class="input" bind:value={addUID} disabled={usersLoading || !!usersError}>
                      <option value="">{translate("peopleSettings.selectAUserByNameOrEmail")}</option>
                      {#each availableGroupUsers() as u (u.id)}
                        <option value={String(u.id)} disabled={u.disabled}>
                          {u.display_name ? translate("peopleSettings.valueValue", { "display_name": u.display_name, "email": u.email }) : u.email}{u.disabled ? translate("peopleSettings.disabled2") : ''}
                        </option>
                      {/each}
                    </select>
                  </label>
                  <button class="btn" disabled={!addUID || usersLoading || !!usersError} onclick={() => addMember(g)}><Icon name="plus" size={14} />{translate("peopleSettings.addMember")}</button>
                </div>
                {#if usersError}<div class="settings-load-state err"><span>{usersError}</span><button class="btn sm" onclick={loadUsers}>{translate("common.retry")}</button></div>{/if}
              </div>
            {/if}
          </div>
        {:else}
          <p class="empty">{translate("peopleSettings.noGroupsYetCreateOneToShare")}</p>
        {/each}
      </div>
    {/if}
  </section>

{:else if tab === 'metadata'}
  <header class="panel-heading metadata-heading">
    <div><h3>{translate("peopleSettings.metadata")}</h3><p>{translate("peopleSettings.manageTheLabelsAndExtraDetailsUsed")}</p></div>
    <label class="field">{translate("peopleSettings.metadataType")}
      <select class="input" value={taxon} disabled={tagDeleteBusy || taxMutationBusy || !!tagDeleteRequest || deleteBusy || !!deleteRequest} onchange={(event) => navigatePeople('metadata', event.currentTarget.value)}>
        {#each TAXA as t}<option value={t.kind}>{t.label}</option>{/each}
      </select>
    </label>
  </header>
  <section class="card editor" aria-labelledby="metadata-heading">
    <h4 id="metadata-heading">{selectedTaxon.label}</h4>
    <p class="editor-description">{selectedTaxon.description}</p>
    {#if taxon === 'custom_fields'}
      <form onsubmit={addField}>
        <div class="field-grid">
          <label class="field">{translate("peopleSettings.fieldName")}
            <input class="input" placeholder={translate("peopleSettings.eGInvoiceNumber")} bind:value={nf.name} required />
          </label>
          <label class="field">{translate("peopleSettings.fieldType")}
            <select class="input" bind:value={nf.data_type}>
              {#each Object.entries(FIELD_TYPES) as [type, label]}<option value={type}>{label}</option>{/each}
            </select>
          </label>
        </div>
        {#if nf.data_type === 'select' || nf.data_type === 'multi'}
          <label class="field choice-field">{translate("peopleSettings.choices")}
            <textarea class="input" rows="4" bind:value={nf.choices}
                      placeholder={translate("peopleSettings.oneChoicePerLineEGPendingApproved")} required></textarea>
            <small>{translate("peopleSettings.oneChoicePerLineChoicesCanBe")}</small>
          </label>
        {/if}
        <div class="form-actions"><button class="btn primary" disabled={!nf.name.trim() || ((nf.data_type === 'select' || nf.data_type === 'multi') && !normalizedChoices(nf.choices).length)}><Icon name="plus" size={14} />{translate("peopleSettings.createField")}</button></div>
      </form>
    {:else}
      <form onsubmit={addTaxon}>
        <label class="field">{translate("peopleSettings.name")}
          <input class="input" placeholder={translate(selectedTaxon.placeholderKey)} bind:value={ntName} disabled={taxMutationBusy || tagDeleteBusy || !!tagDeleteRequest} required />
        </label>
        <div class="form-actions"><button class="btn primary" disabled={!ntName.trim() || taxMutationBusy || tagDeleteBusy || !!tagDeleteRequest}><Icon name="plus" size={14} />{translate(selectedTaxon.createKey)}</button></div>
      </form>
    {/if}
  </section>
  <section aria-labelledby="metadata-list-heading">
    <h4 class="list-heading" id="metadata-list-heading">{translate(selectedTaxon.existingKey)}</h4>
    {#if taxon === 'tags'}
      <div class="tag-management">
        <label class="field tag-search">{translate("peopleSettings.searchTags")}
          <input class="input" type="search" bind:value={tagSearch} placeholder={translate("peopleSettings.filterByNameOrSlug")} />
        </label>
        <div class="tag-selection">
          <span role="status">{translate("peopleSettings.valueSelected", { "size": selectedTagIDs.size })}</span>
          <button class="btn sm" type="button" disabled={!taxRowsLoaded || taxRowsKind !== 'tags' || taxRowsLoading || matchingTags.length === 0 || matchingTags.length > 500 || tagDeleteBusy || taxMutationBusy || !!tagDeleteRequest} onclick={selectMatchingTags}>{translate("peopleSettings.selectMatching")}</button>
          <button class="btn sm danger" type="button" disabled={!selectedTagIDs.size || selectedTagIDs.size > 500 || tagDeleteBusy || taxMutationBusy || !!tagDeleteRequest} onclick={() => requestTagDelete([...selectedTagIDs])}>{translate("peopleSettings.deleteSelected")}</button>
          <button class="btn sm" type="button" disabled={!selectedTagIDs.size || tagDeleteBusy || taxMutationBusy || !!tagDeleteRequest} onclick={() => (selectedTagIDs = new Set())}>{translate("peopleSettings.clearSelection")}</button>
        </div>
        {#if taxRowsLoaded && taxRowsKind === 'tags' && matchingTags.length > 500}
          <p class="quiet">{translate("peopleSettings.moreThan500TagsMatchNarrowThe")}</p>
        {/if}
        {#if selectedTagIDs.size > 500}
          <p class="quiet">{translate("peopleSettings.narrowYourSelectionTo500TagsOr")}</p>
        {/if}
        {#if tagDeleteError}<p class="err" role="alert">{tagDeleteError}</p>{/if}
        {#if tagDeleteSuccess}<p role="status">{tagDeleteSuccess}</p>{/if}
      </div>
    {/if}
    {#if taxon === 'custom_fields'}
      {#if fieldsError}
        <div class="settings-load-state err"><span>{fieldsError}</span><button class="btn sm" onclick={loadFields}>{translate("common.retry")}</button></div>
      {:else if !fieldsLoaded || fieldsLoading}
        <p class="quiet" role="status">{translate("peopleSettings.loadingCustomFields")}</p>
      {:else}
        {#if fields.some(field => field.managed_by)}
          <p class="managed-field-help">{translate("peopleSettings.managedFieldsAreRequiredByDocumentExtraction")}</p>
        {/if}
        <div class="index">
          {#each fields as f (f.id)}
            {@const label = customFieldLabel(f)}
            <div class="field-entry">
              <div class="irow">
                <span class="grow">
                  <input class="inline-edit"
                         aria-label={f.managed_by ? translate("peopleSettings.valueManaged", { "name": label }) : translate("peopleSettings.renameValue", { "name": label })}
                         title={f.managed_by ? translate("peopleSettings.requiredByEInvoiceExtraction") : ''}
                         value={label}
                         readonly={!!f.managed_by}
                         onchange={(e) => renameField(f, e.target.value)} />
                </span>
                <span class="pill">{FIELD_TYPES[f.data_type] || f.data_type}</span>
                {#if f.managed_by}
                  <span class="pill managed-field-pill">{translate("peopleSettings.managed")}</span>
                {:else}
                  <button class="btn sm danger" disabled={deleteBusy || !!deleteRequest}
                          onclick={() => rmField(f)} title={translate("peopleSettings.deleteField")} aria-label={translate("peopleSettings.deleteValue2", { "name": f.name })}><Icon name="trash" size={13} /></button>
                {/if}
              </div>
              {#if f.data_type === 'select' || f.data_type === 'multi'}
                <div class="choice-editor">
                  <label class="field">{translate("peopleSettings.choices")}
                    <textarea class="input" rows="3" aria-label={translate("peopleSettings.choicesForValue", { "name": f.name })}
                              value={fieldChoiceDrafts[f.id] || ''}
                              oninput={(event) => (fieldChoiceDrafts = { ...fieldChoiceDrafts, [f.id]: event.currentTarget.value })}
                              disabled={!!f.managed_by || fieldChoiceBusy != null}></textarea>
                  </label>
                  <button class="btn sm" type="button"
                          disabled={!!f.managed_by || fieldChoiceBusy != null || !normalizedChoices(fieldChoiceDrafts[f.id] || '').length}
                          onclick={() => saveFieldChoices(f)}>
                    {fieldChoiceBusy === f.id ? translate("common.saving") : translate("peopleSettings.saveChoices")}
                  </button>
                </div>
              {/if}
            </div>
          {:else}
            <p class="empty">{translate("peopleSettings.noCustomFieldsYetAddAField")}</p>
          {/each}
        </div>
      {/if}
    {:else if taxRowsError}
      <div class="settings-load-state err"><span>{taxRowsError}</span><button class="btn sm" onclick={loadTaxa}>{translate("common.retry")}</button></div>
    {:else if !taxRowsLoaded || taxRowsKind !== taxon || taxRowsLoading}
      <p class="quiet" role="status">{translate(selectedTaxon.loadingKey)}</p>
    {:else}
      <div class="index">
        {#each taxon === 'tags' ? matchingTags : taxRows as row (row.id)}
          <div class="irow tag-row">
            {#if taxon === 'tags'}
              <input type="checkbox" class="tag-checkbox" aria-label={translate("peopleSettings.selectTagValue", { "name": row.name })}
                     checked={selectedTagIDs.has(row.id)} disabled={tagDeleteBusy || taxMutationBusy || !!tagDeleteRequest}
                     onchange={(e) => toggleTag(row.id, e.currentTarget.checked)} />
            {/if}
            <span class="dot" style={row.color ? `background:${row.color}` : ''}></span>
            <span class="grow"><input class="inline-edit" aria-label={translate("peopleSettings.renameValue", { "name": row.name })} value={row.name}
                                    disabled={tagDeleteBusy || taxMutationBusy || !!tagDeleteRequest || deleteBusy || !!deleteRequest}
                                    onchange={(e) => renameTaxon(row, e.target.value)} /></span>
            {#if row.child_count}<span class="quiet">{plural("peopleSettings.childCount", row.child_count)}</span>{/if}
            {#if row.document_count != null}<span class="quiet">{plural("peopleSettings.documentCount", row.document_count)}</span>{/if}
            <button class="btn sm danger" disabled={tagDeleteBusy || taxMutationBusy || !!tagDeleteRequest || deleteBusy || !!deleteRequest}
                    onclick={() => rmTaxon(row)} title={translate("peopleSettings.deleteEntry")} aria-label={translate("peopleSettings.deleteValue2", { "name": row.name })}><Icon name="trash" size={13} /></button>
          </div>
        {:else}
          <p class="empty">{taxon === 'tags' && tagSearch.trim() ? translate("peopleSettings.noMatchingTags") : translate(selectedTaxon.emptyKey)}</p>
        {/each}
      </div>
    {/if}
  </section>
{/if}
</div>

{#if twoFactorResetUser}
  <AdminTwoFactorResetDialog user={twoFactorResetUser}
    onClose={closeTwoFactorReset} onReset={twoFactorReset} />
{/if}

{#if deleteRequest && deleteDialog}
  <ConfirmDialog
    title={deleteDialog.title}
    message={deleteDialog.message}
    confirmLabel={deleteDialog.confirmLabel}
    busyLabel={translate("peopleSettings.deleting")}
    busy={deleteBusy}
    onConfirm={confirmDelete}
    onCancel={() => (deleteRequest = null)} />
{/if}

{#if tagDeleteRequest}
  <ConfirmDialog
    title={plural("peopleSettings.deleteTagCount", tagDeleteRequest.ids.length)}
    message={tagDeleteRequest.name ? translate("peopleSettings.deleteNamedTagLinks", { name: tagDeleteRequest.name }) : translate("peopleSettings.deleteSelectedTagLinks")}
    confirmLabel={translate("peopleSettings.deleteTags")}
    busyLabel={translate("peopleSettings.deleting")}
    busy={tagDeleteBusy}
    onConfirm={confirmTagDelete}
    onCancel={() => (tagDeleteRequest = null)} />
{/if}

<style>
  .people-nav { display: flex; flex-wrap: wrap; gap: 4px; margin-bottom: 22px; padding-bottom: 10px; border-bottom: 1px solid var(--line); }
  .people-nav button { border: 0; border-radius: 7px; background: none; padding: 8px 13px; color: var(--muted); font-size: .82rem; font-weight: 550; }
  .people-nav button:hover { background: var(--surface-2); color: var(--ink); }
  .people-nav button.on { color: var(--accent); background: var(--tint); }
  .people-nav button:disabled { opacity: .5; cursor: default; }
  .people-content { display: grid; gap: 20px; max-width: 980px; }
  .panel-heading h3 { font-size: 1.1rem; }
  .panel-heading p, .editor-description { color: var(--muted); font-size: .82rem; margin: 5px 0 0; max-width: 62em; }
  .editor h4, .list-heading { font-size: .88rem; font-weight: 600; margin: 0 0 14px; }
  .editor-description { margin: -7px 0 16px; }
  .field { min-width: 0; margin: 0; color: var(--muted); font-size: .78rem; font-weight: 600; }
  .input { min-width: 0; min-height: 42px; font-weight: 400; }
  .field-grid { display: grid; grid-template-columns: 2fr 1fr; gap: 14px; }
  .form-actions { display: flex; justify-content: flex-end; gap: 10px; margin-top: 18px; padding-top: 16px; border-top: 1px solid var(--line); }
  .choice-field { display:grid; gap:6px; margin-top:14px; }
  .choice-field textarea, .choice-editor textarea { width:100%; max-width:none; resize:vertical; line-height:1.4; }
  .choice-field small { color:var(--muted); font-size:.7rem; font-weight:400; }
  .managed-field-help { margin:0 0 10px; color:var(--muted); font-size:.78rem; }
  .managed-field-pill { color:var(--accent); background:var(--tint); }
  .field-entry .inline-edit[readonly] { color:var(--muted); cursor:default; }
  .field-entry { border-bottom:1px solid var(--line); }
  .field-entry:last-child { border-bottom:0; }
  .field-entry > .irow { border-bottom:0; }
  .choice-editor { display:grid; grid-template-columns:minmax(0,1fr) auto; align-items:end; gap:10px; padding:0 16px 14px; background:var(--bg); }
  .choice-editor .field { display:grid; gap:5px; }
  .irow { cursor: default; }
  .quiet, .empty { color: var(--muted); font-size: .78rem; }
  .empty { padding: 18px; margin: 0; }
  .identity { display: flex; flex-direction: column; }
  .identity .quiet { overflow-wrap: anywhere; }
  .user-entry, .group-entry { border-bottom: 1px solid var(--line); }
  .user-entry:last-child, .group-entry:last-child { border-bottom: 0; }
  .user-row { border-bottom: 0; }
  .user-access { padding: 0 16px 14px; }
  summary { cursor: pointer; font-size: .8rem; }
  .user-access summary > span { margin-left: 8px; }
  .access-options { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 250px), 1fr)); gap: 16px 24px; margin-top: 16px; }
  .access-option { display: flex; justify-content: space-between; align-items: flex-start; gap: 12px; }
  .access-option span { display: flex; flex-direction: column; font-size: .8rem; }
  .access-option b { font-weight: 550; }
  .access-option small { margin-top: 3px; color: var(--muted); }
  .group-members { padding: 4px 16px 16px; background: var(--bg); }
  .member-row { padding-inline: 0; }
  .member-add { display: flex; align-items: flex-end; flex-wrap: wrap; gap: 10px; margin-top: 16px; }
  .member-add .field { flex: 1 1 240px; }
  .member-add .btn { min-height: 42px; }
  .metadata-heading { display: flex; justify-content: space-between; align-items: flex-start; gap: 20px; }
  .metadata-heading .field { flex: 0 0 210px; }
  .settings-load-state { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
  .tag-management { display: grid; gap: 9px; margin: 0 0 14px; }
  .tag-search { display: grid; gap: 6px; max-width: 390px; }
  .tag-selection { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; }
  .tag-selection > span { color: var(--muted); font-size: .8rem; margin-right: 4px; }
  .tag-management p { margin: 0; font-size: .8rem; }
  .tag-checkbox { flex: none; accent-color: var(--accent); }
  .tag-row { flex-wrap: wrap; gap: 8px; }
  .tag-row .grow { min-width: min(100%, 160px); }
  .tag-row .inline-edit { max-width: 100%; }
  @media (max-width: 600px) {
    .people-nav { gap: 2px; }
    .people-nav button { flex: 1 1 auto; padding: 9px 10px; }
    .field-grid { grid-template-columns: minmax(0, 1fr); }
    .metadata-heading { flex-direction: column; align-items: stretch; gap: 14px; }
    .metadata-heading .field { flex: auto; }
    .form-actions .btn { flex: 1 1 auto; justify-content: center; min-height: 42px; }
    .user-row { flex-wrap: wrap; gap: 10px; }
    .user-row > .grow { flex-basis: 100%; }
    .tag-selection .btn { flex: 1 1 auto; }
  }
</style>
