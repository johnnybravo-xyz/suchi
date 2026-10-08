<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { listAutomations, createAutomation, patchAutomation, deleteAutomation,
           listTags, listCorrespondents, listCustomFields, listRenderedLayouts,
           adminListUsers, systemMembers, automationsSchema } from '../lib/api.js'
  import { systems } from '../lib/systems.svelte.js'
  import Icon from '../lib/Icon.svelte'
  import RuleWorkspace from '../lib/RuleWorkspace.svelte'
  import RuleRow from '../lib/RuleRow.svelte'
  import RuleEmpty from '../lib/RuleEmpty.svelte'
  import RuleSection from '../lib/RuleSection.svelte'

  let { notify, readOnly = false, jdCategories = [] } = $props()
  let items = $state([])
  let loading = $state(true)
  let err = $state('')
  let editing = $state(null)        // null | working copy (id present = edit)
  let mode = $state('builder')      // 'builder' | 'json'
  let jsonDraft = $state('')
  let draftErr = $state('')
  let facets = $state({ tags: [], correspondents: [], customFields: [], renderedLayouts: [], owners: [] })
  let facetsPromise
  let facetsError = $state('')
  let peekID = $state(null)
  let builtInsOpen = $state(false)
  let toggleID = $state(null)
  let reorderID = $state(null)
  let saving = $state(false)
  let visibleFilters = $state([])
  let nameInput = $state()
  let draggedID = null

  const userItems = $derived(items.filter(a => !a.preset_slug))
  const builtInItems = $derived(items.filter(a => a.preset_slug))

  let TRIGGER_TYPES = $state([])
  let ACTION_KINDS = $state([])
  let ASK_SCHEMA = $state({
    max_enabled: 5,
    answers: [{ type: 'yes_no', name: 'Yes / No' }, { type: 'choice', name: 'Multiple choice' }],
    action_kinds: ['assign_tags', 'assign_correspondent', 'assign_jd_category', 'assign_custom_field'],
  })

  const FILTERS = [
    { kind: 'filename', key: 'filter_filename', label: 'Filename', operator: 'matches', placeholder: '*.pdf' },
    { kind: 'title', key: 'filter_title_matching', label: 'Title', operator: 'matches', placeholder: 'invoice|statement' },
    { kind: 'content', key: 'filter_content_matching', label: 'Content', operator: 'matches', placeholder: 'account number' },
    { kind: 'tag', key: 'filter_has_tag', label: 'Tag', operator: 'is', type: 'tag' },
    { kind: 'correspondent', key: 'filter_has_correspondent', label: 'Correspondent', operator: 'is', type: 'correspondent' },
  ]
  const AUTOMATION_PATTERNS = [
    { value: 'correspondent', label: 'Tag by correspondent' },
    { value: 'title-route', label: 'Route by title' },
    { value: 'dated-title', label: 'Title scans by date' },
  ]
  automationsSchema().then(sc => {
    if (sc?.triggers?.length) TRIGGER_TYPES = sc.triggers.map(t => ({ code: t.code, label: t.name || t.type }))
    if (sc?.actions?.length) ACTION_KINDS = sc.actions.map(a => ({
      kind: a.kind, label: a.name || a.kind, description: a.description || '', params: a.params || [],
    }))
    if (sc?.ask) ASK_SCHEMA = sc.ask
  }).catch(ex => { err = ex.message || 'Could not load the automation schema.' })

  const newTrigger = () => ({
    type: 2, filter_filename: '', filter_path: '', filter_title_matching: '', filter_content_matching: '',
    filter_has_tag: 0, filter_has_correspondent: 0,
  })
  const blank = () => ({
    name: '', enabled: true, order: items.length,
    triggers: [newTrigger()],
    actions: [{ type: 'assign_tags', params: { tag_ids: [] } }],
  })

  async function load() {
    loading = true; err = ''
    try {
      const res = await listAutomations()
      items = res?.results || res || []
      if (items.length) loadFacets()
    } catch (ex) { err = ex.message || 'Could not load automations.' }
    finally { loading = false }
  }
  async function loadOwners() {
    if (readOnly) return []
    const membershipRequest = systems.introduced && systems.code
      ? systemMembers(systems.code)
      : Promise.resolve(null)
    const [directory, membership] = await Promise.all([adminListUsers(), membershipRequest])
    const memberIDs = membership ? new Set(membership.user_ids || []) : null
    return (directory?.results || []).filter(user =>
      !user.disabled && (!memberIDs || user.role === 'admin' || memberIDs.has(user.id)))
  }

  function loadFacets() {
    if (facetsPromise) return facetsPromise
    facetsError = ''
    facetsPromise = Promise.all([listTags(), listCorrespondents(), listCustomFields(), listRenderedLayouts(), loadOwners()])
      .then(([tags, correspondents, customFields, renderedLayouts, owners]) => {
        facets = {
          tags: tags?.results || [],
          correspondents: correspondents?.results || [],
          customFields: customFields?.results || customFields || [],
          renderedLayouts: renderedLayouts?.results || renderedLayouts || [],
          owners,
        }
      })
      .catch((ex) => {
        facetsError = ex.message || 'Could not load metadata choices.'
        facetsPromise = null
      })
    return facetsPromise
  }

  function configuredFilters(trigger) {
    return FILTERS.filter(filter => filter.type
      ? Number(trigger?.[filter.key]) > 0
      : String(trigger?.[filter.key] || '').trim() !== '').map(filter => filter.kind)
  }

  function openEditor(a, filters = null) {
    loadFacets()
    editing = a ? JSON.parse(JSON.stringify(a)) : blank()
    editing.triggers = editing.triggers?.length ? editing.triggers : [newTrigger()]
    editing.actions = editing.actions?.length ? editing.actions : [{ type: 'assign_tags', params: { tag_ids: [] } }]
    visibleFilters = filters || editing.triggers.map(configuredFilters)
    mode = 'builder'
    draftErr = ''
    duplicate = null
    queueMicrotask(() => nameInput?.focus())
  }

  function openTemplate(kind) {
    const draft = blank()
    let filters = [[]]
    switch (kind) {
      case 'correspondent':
        draft.name = 'Tag by correspondent'
        filters = [['correspondent']]
        break
      case 'title-route':
        draft.name = 'Route by title'
        draft.actions = [{ type: 'assign_jd_category', params: { jd_category_id: 0 } }]
        filters = [['title']]
        break
      case 'dated-title':
        draft.name = 'Title scans by date'
        draft.triggers[0].filter_filename = '*.pdf'
        draft.actions = [{ type: 'assign_title', params: { template: '{{date}} {{title}}' } }]
        filters = [['filename']]
        break
    }
    openEditor(draft, filters)
  }

  function closeEditor() {
    if (saving) return
    editing = null
    draftErr = ''
    duplicate = null
  }

  function switchMode(m) {
    draftErr = ''
    if (m === 'json') {
      try { jsonDraft = JSON.stringify(editing, null, 2); mode = 'json' }
      catch (ex) { draftErr = ex.message; return }
    } else {
      try {
        editing = JSON.parse(jsonDraft)
        editing.triggers = editing.triggers?.length ? editing.triggers : [newTrigger()]
        editing.actions = editing.actions?.length ? editing.actions : [{ type: 'assign_tags', params: { tag_ids: [] } }]
        visibleFilters = editing.triggers.map(configuredFilters)
        mode = 'builder'
      } catch { draftErr = 'Fix the JSON before switching back to the builder.' }
    }
  }

  function addTrigger() {
    editing.triggers.push(newTrigger())
    visibleFilters.push([])
  }

  function removeTrigger(index) {
    editing.triggers.splice(index, 1)
    visibleFilters.splice(index, 1)
  }

  function filterSpec(kind) {
    return FILTERS.find(filter => filter.kind === kind)
  }

  function availableFilters(index) {
    const shown = new Set(visibleFilters[index] || [])
    return FILTERS.filter(filter => !shown.has(filter.kind) && (!editing.ask || filter.kind !== 'filename'))
  }

  function addFilter(index, event) {
    const kind = event.currentTarget.value
    event.currentTarget.value = ''
    if (!kind || visibleFilters[index]?.includes(kind)) return
    visibleFilters[index] = [...(visibleFilters[index] || []), kind]
  }

  function removeFilter(triggerIndex, kind) {
    const filter = filterSpec(kind)
    if (!filter) return
    editing.triggers[triggerIndex][filter.key] = filter.type ? 0 : ''
    visibleFilters[triggerIndex] = (visibleFilters[triggerIndex] || []).filter(item => item !== kind)
  }
  function answerBranches(ask = editing?.ask) {
    if (!ask) return []
    if (ask.answer?.type === 'yes_no') return ['yes', 'no']
    return (ask.answer?.choices || []).map(choice => choice.trim()).filter(Boolean)
  }
  function availableActionKinds() {
    if (!editing?.ask) return ACTION_KINDS
    const allowed = new Set(ASK_SCHEMA.action_kinds || [])
    return ACTION_KINDS.filter(kind => allowed.has(kind.kind))
  }
  function addAction() {
    const kind = availableActionKinds()[0]?.kind || 'assign_tags'
    const action = { type: kind, params: {} }
    actionKindChanged(action)
    if (editing.ask) action.when = answerBranches()[0] || ''
    editing.actions.push(action)
  }
  function actionKindChanged(a) {
    const spec = ACTION_KINDS.find(k => k.kind === a.type)
    a.params = Object.fromEntries((spec?.params || []).map(p => [
      p.name, p.name === 'tag_ids' ? [] : p.type === 'template' || p.type === 'string' ? '' : 0,
    ]))
  }

  function normalizeActionBranches() {
    const branches = answerBranches()
    for (const action of editing.actions || []) {
      if (!branches.includes(action.when)) action.when = branches[0] || ''
    }
  }

  function choiceAnswerSchema() {
    return ASK_SCHEMA.answers?.find(answer => answer.type === 'choice') || {}
  }

  function setAskEnabled(enabled) {
    if (!enabled) {
      editing.ask = null
      for (const action of editing.actions || []) delete action.when
      return
    }
    editing.ask = { question: '', answer: { type: 'yes_no', choices: [] } }
    editing.triggers = (editing.triggers?.length ? editing.triggers : [newTrigger()]).map(trigger => ({
      ...trigger,
      type: 2,
      filter_filename: '',
      filter_path: '',
      filter_email_from: '',
      filter_email_subject: '',
      filter_email_folder: '',
      filter_email_has_attachment: null,
    }))
    visibleFilters = visibleFilters.map(filters => filters.filter(kind => kind !== 'filename'))
    normalizeActionBranches()
  }

  function askAnswerTypeChanged() {
    editing.ask.answer.choices = editing.ask.answer.type === 'choice' ? ['Option 1', 'Option 2'] : []
    normalizeActionBranches()
  }

  function customFieldFor(action) {
    return facets.customFields.find(field => Number(field.id) === Number(action.params?.field_id))
  }

  function availableCustomFields() {
    return editing?.ask
      ? facets.customFields.filter(field => field.data_type !== 'documentlink')
      : facets.customFields
  }

  function customFieldChoices(field) {
    if (!field?.extra_data) return []
    let extra = field.extra_data
    if (typeof extra === 'string') {
      try { extra = JSON.parse(extra) } catch { return [] }
    }
    return Array.isArray(extra?.choices) ? extra.choices : []
  }

  function customFieldChanged(action) {
    const field = customFieldFor(action)
    action.params.value = field?.data_type === 'multi' ? [] : field?.data_type === 'bool' ? false : ''
  }

  function tagName(id) {
    return facets.tags.find(tag => Number(tag.id) === Number(id))?.name || `Tag #${id}`
  }

  function addTag(action, event) {
    const id = Number(event.currentTarget.value)
    event.currentTarget.value = ''
    if (!id) return
    const selected = (action.params.tag_ids || []).map(Number)
    if (!selected.includes(id)) action.params.tag_ids = [...selected, id]
  }

  function removeTag(action, id) {
    action.params.tag_ids = (action.params.tag_ids || []).map(Number).filter(tagID => tagID !== Number(id))
  }

  async function save() {
    if (saving) return
    draftErr = ''
    let body
    if (mode === 'json') {
      try { body = JSON.parse(jsonDraft) }
      catch (ex) { draftErr = ex.message || 'Not valid JSON.'; return }
    } else body = editing
    if (!body.name?.trim()) { draftErr = 'Give it a name.'; return }
    for (const t of body.triggers || []) for (const k of ['type','filter_has_tag','filter_has_correspondent']) t[k] = Number(t[k]) || 0
    for (const a of body.actions || []) for (const k of Object.keys(a.params || {}))
      if (k.endsWith('_id')) a.params[k] = Number(a.params[k]) || 0
      else if (k === 'tag_ids') a.params[k] = (a.params[k] || []).map(Number)
    saving = true
    try {
      if (body.id) await patchAutomation(body.id, body)
      else await createAutomation(body)
      editing = null
      notify?.('Automation saved')
      await load()
    } catch (ex) {
      if (ex.status === 409 && ex.code === 'duplicate_rule' && ex.data?.match) {
        duplicate = ex.data.match
        return
      }
      draftErr = ex.message || 'The server rejected this spec.'
    } finally {
      saving = false
    }
  }

  let duplicate = $state(null)
  async function enableExistingDuplicate() {
    if (!duplicate) return
    try {
      await patchAutomation(duplicate.id, { enabled: true })
      const openID = duplicate.id
      duplicate = null
      editing = null
      notify?.('Enabled existing automation')
      await load()
      queueMicrotask(() => document.getElementById(`automation-${openID}`)?.scrollIntoView({ behavior: 'smooth', block: 'center' }))
    } catch (ex) { draftErr = ex.message || 'Could not enable the existing automation.' }
  }
  async function openExistingDuplicate() {
    if (!duplicate) return
    const target = items.find(a => a.id === duplicate.id)
    duplicate = null
    if (target) openEditor(target)
  }

  async function toggle(a) {
    if (toggleID !== null) return
    const enabled = !a.enabled
    toggleID = a.id
    try {
      await patchAutomation(a.id, { enabled })
      a.enabled = enabled
      notify?.(enabled ? 'Enabled' : 'Disabled')
    } catch (ex) { notify?.(ex.message || 'Could not update the automation') }
    finally { toggleID = null }
  }
  async function remove(a) {
    if (!confirm(`Delete automation “${a.name}”?`)) return
    try {
      await deleteAutomation(a.id)
      items = items.filter(x => x.id !== a.id)
      if (editing?.id === a.id) editing = null
      notify?.('Deleted')
    } catch (ex) { notify?.(ex.message || 'Could not delete') }
  }

  async function persistOrder(ordered, movedID) {
    if (reorderID !== null) return
    const previous = items
    const builtIns = items.filter(item => item.preset_slug)
    const normalized = ordered.map((item, index) => ({ ...item, order: index }))
    items = [...normalized, ...builtIns]
    reorderID = movedID
    try {
      for (let index = 0; index < normalized.length; index++) {
        const before = ordered[index]
        if (Number(before.order) !== index) await patchAutomation(before.id, { order: index })
      }
    } catch (ex) {
      items = previous
      notify?.(ex.message || 'Could not reorder automations')
    } finally {
      reorderID = null
    }
  }

  function moveAutomation(id, direction) {
    const ordered = [...userItems]
    const from = ordered.findIndex(item => item.id === id)
    const to = Math.max(0, Math.min(ordered.length - 1, from + direction))
    if (from < 0 || from === to) return
    const [moved] = ordered.splice(from, 1)
    ordered.splice(to, 0, moved)
    persistOrder(ordered, id)
  }

  function startDrag(event, id) {
    draggedID = id
    event.dataTransfer.effectAllowed = 'move'
    event.dataTransfer.setData('text/plain', String(id))
  }

  function dropAutomation(event, targetID) {
    event.preventDefault()
    const id = draggedID || Number(event.dataTransfer.getData('text/plain'))
    draggedID = null
    if (!id || id === targetID || reorderID !== null) return
    const ordered = [...userItems]
    const from = ordered.findIndex(item => item.id === id)
    let to = ordered.findIndex(item => item.id === targetID)
    if (from < 0 || to < 0) return
    const after = event.clientY > event.currentTarget.getBoundingClientRect().top + event.currentTarget.offsetHeight / 2
    const [moved] = ordered.splice(from, 1)
    if (from < to) to--
    if (after) to++
    ordered.splice(to, 0, moved)
    persistOrder(ordered, id)
  }

  function ownerLabel(owner) {
    if (!owner) return ''
    return owner.display_name ? `${owner.display_name} · ${owner.email}` : owner.email
  }

  function triggerDescribe(trigger) {
    const raw = TRIGGER_TYPES.find(type => Number(type.code) === Number(trigger.type))?.label || `trigger ${trigger.type}`
    const event = raw.replace(/^After\s+/i, '').replace(/^On\s+/i, '')
    const details = [event.charAt(0).toLowerCase() + event.slice(1)]
    if (trigger.filter_filename) details.push(`filename matches ${trigger.filter_filename}`)
    if (trigger.filter_title_matching) details.push(`title matches ${trigger.filter_title_matching}`)
    if (trigger.filter_content_matching) details.push(`content matches ${trigger.filter_content_matching}`)
    if (trigger.filter_has_tag) details.push(`tag is ${tagName(trigger.filter_has_tag)}`)
    if (trigger.filter_has_correspondent) {
      const name = facets.correspondents.find(item => Number(item.id) === Number(trigger.filter_has_correspondent))?.name
      details.push(`correspondent is ${name || `#${trigger.filter_has_correspondent}`}`)
    }
    return details.join(' · ')
  }

  function summary(a) {
    const triggers = (a.triggers || []).map(triggerDescribe).join(' or ') || 'no trigger'
    const ask = a.ask?.question ? ` · ask “${a.ask.question}”` : ''
    const actions = (a.actions || []).map(actionDescribe).join(' · ') || 'no actions'
    return `when ${triggers}${ask} → ${actions}`
  }

  function actionDescribe(a) {
    const spec = ACTION_KINDS.find(kind => kind.kind === a.type)
    const p = a.params || {}
    const branch = a.when ? `if ${a.when}: ` : ''
    switch (a.type) {
      case 'assign_tags': {
        const names = (p.tag_ids || []).map(tagName)
        return branch + (names.length ? `add ${names.length === 1 ? 'tag' : 'tags'} ${names.join(', ')}` : 'add tags')
      }
      case 'assign_correspondent': {
        const name = facets.correspondents.find(item => Number(item.id) === Number(p.correspondent_id))?.name
        return branch + (name ? `set correspondent ${name}` : 'set correspondent')
      }
      case 'assign_jd_category': {
        const jd = jdCategories.find(item => Number(item.id) === Number(p.jd_category_id))
        return branch + (jd ? `file in ${jd.code} · ${jd.name}` : 'file in a category')
      }
      case 'assign_title':
        return branch + (p.template ? `set title “${p.template}”` : 'set title')
      case 'assign_owner': {
        const owner = facets.owners.find(candidate => Number(candidate.id) === Number(p.owner_id))
        return branch + (owner ? `set owner ${ownerLabel(owner)}` : 'set owner')
      }
      case 'assign_storage_path': {
        const layout = facets.renderedLayouts.find(candidate => Number(candidate.id) === Number(p.storage_path_id))
        return branch + (layout ? `use folder layout ${layout.name}` : 'use folder layout')
      }
      case 'assign_custom_field': {
        const field = facets.customFields.find(candidate => Number(candidate.id) === Number(p.field_id))
        const value = Array.isArray(p.value) ? p.value.join(', ') : String(p.value ?? '')
        return branch + (field ? `set ${field.name} to ${value}` : 'set custom field')
      }
      default:
        return branch + (spec?.label?.toLowerCase() || a.type)
    }
  }

  load()
</script>

{#snippet automationRow(a, sortable)}
  <RuleRow class="automation-row"
           id={`automation-${a.id}`}
           title={a.name || `Automation #${a.id}`}
           summary={summary(a)}
           disabled={!a.enabled}
           actionText={readOnly ? (peekID === a.id ? 'Hide JSON' : 'View JSON') : 'Edit'}
           actionLabel={readOnly ? (peekID === a.id ? 'Hide JSON' : 'View JSON') : `Edit ${a.name || `automation ${a.id}`}`}
           detailText={readOnly && peekID === a.id ? JSON.stringify(a, null, 2) : ''}
           onaction={() => readOnly ? (peekID = peekID === a.id ? null : a.id) : openEditor(a)}
           ondragover={(event) => { if (sortable && !readOnly) event.preventDefault() }}
           ondrop={(event) => { if (sortable && !readOnly) dropAutomation(event, a.id) }}>
    {#snippet leading()}
      {#if sortable && !readOnly}
        <button class="drag-handle" type="button" draggable="true"
                disabled={reorderID !== null}
                aria-label={`Move ${a.name || `automation ${a.id}`}`}
                title="Drag to reorder; use arrow keys for precise movement"
                ondragstart={(event) => startDrag(event, a.id)}
                ondragend={() => (draggedID = null)}
                onkeydown={(event) => {
                  if (event.key === 'ArrowUp') {
                    event.preventDefault()
                    moveAutomation(a.id, -1)
                  } else if (event.key === 'ArrowDown') {
                    event.preventDefault()
                    moveAutomation(a.id, 1)
                  }
                }}>
          <span aria-hidden="true">⠿</span>
        </button>
      {:else}
        <span class="row-indent" aria-hidden="true"></span>
      {/if}
    {/snippet}
    {#snippet titleMeta()}
      {#if a.preset_slug}<span class="owner-pill">{a.preset_slug} filing tree</span>{/if}
    {/snippet}
    {#snippet trailing()}
      {#if readOnly}
        <span class="pill" class:ok={a.enabled} class:warn={!a.enabled}>{a.enabled ? 'enabled' : 'disabled'}</span>
      {:else}
        <button type="button" class="switch" role="switch" aria-checked={a.enabled}
                aria-label={`${a.name || `Automation ${a.id}`} enabled`}
                disabled={toggleID !== null}
                onclick={() => toggle(a)}></button>
      {/if}
    {/snippet}
  </RuleRow>
{/snippet}

<div class="automations-page">
  {#if err}<div class="err page-error">{err}</div>{/if}

  {#if editing !== null}
    <RuleWorkspace class="automation-builder"
                   title={editing.id ? 'Edit automation' : 'New automation'}
                   headingId="automation-editor-heading">
      {#snippet actions()}
        <button class="text-action editor-mode" onclick={() => switchMode(mode === 'builder' ? 'json' : 'builder')}>
          {mode === 'builder' ? 'Edit as JSON' : 'Back to builder'}
        </button>
      {/snippet}

      {#if draftErr}<div class="err editor-error">{draftErr}</div>{/if}
      {#if facetsError}
        <div class="err editor-error facet-error">
          <span>{facetsError}</span><button class="btn sm" onclick={loadFacets}>Retry</button>
        </div>
      {/if}
      {#if duplicate}
        <div class="duplicate-warning">
          <div>
            This automation already exists as <b>“{duplicate.name}”</b>
            {duplicate.enabled ? '(currently enabled).' : '(currently disabled).'}
          </div>
          <div class="duplicate-actions">
            {#if duplicate.enabled}
              <button class="btn sm primary" onclick={openExistingDuplicate}>Open existing</button>
            {:else}
              <button class="btn sm primary" onclick={enableExistingDuplicate}>Use existing</button>
              <button class="btn sm" onclick={openExistingDuplicate}>Edit existing</button>
            {/if}
            <button class="btn sm" onclick={() => (duplicate = null)}>Dismiss</button>
          </div>
        </div>
      {/if}

      {#if mode === 'json'}
        <div class="json-editor">
          <textarea class="input" rows="22" bind:value={jsonDraft} spellcheck="false"
                    aria-label="Automation JSON"></textarea>
        </div>
      {:else}
        <div class="builder-name">
          <input class="input" bind:this={nameInput}
                 placeholder="Name, e.g. Tag utility bills"
                 aria-label="Automation name"
                 bind:value={editing.name} />
        </div>

        <RuleSection class="builder-section trigger-section" title="When" headingId="when-heading">
          {#each editing.triggers as trigger, triggerIndex (triggerIndex)}
            <div class="trigger-block">
              <div class="sentence-row trigger-row">
                {#if triggerIndex > 0}<span class="conjunction">or</span>{/if}
                <select class="input sentence-select trigger-select"
                        bind:value={trigger.type}
                        disabled={!!editing.ask}
                        aria-label={`Trigger ${triggerIndex + 1}`}>
                  {#each TRIGGER_TYPES as type}<option value={type.code}>{type.label}</option>{/each}
                </select>
                {#if editing.triggers.length > 1}
                  <button class="icon-button" type="button"
                          onclick={() => removeTrigger(triggerIndex)}
                          aria-label={`Remove trigger ${triggerIndex + 1}`}>
                    <Icon name="x" size={12} />
                  </button>
                {/if}
              </div>

              {#each visibleFilters[triggerIndex] || [] as kind (kind)}
                {@const filter = filterSpec(kind)}
                <div class="sentence-row filter-row">
                  <span class="conjunction">and</span>
                  <span class="field-token">{filter.label}</span>
                  <span class="operator-token">{filter.operator}</span>
                  {#if filter.type === 'tag'}
                    <select class="input sentence-value" bind:value={trigger[filter.key]}
                            aria-label={`Tag filter ${triggerIndex + 1}`}>
                      <option value={0}>Choose tag…</option>
                      {#each facets.tags as tag}<option value={tag.id}>{tag.name}</option>{/each}
                    </select>
                  {:else if filter.type === 'correspondent'}
                    <select class="input sentence-value" bind:value={trigger[filter.key]}
                            aria-label={`Correspondent filter ${triggerIndex + 1}`}>
                      <option value={0}>Choose correspondent…</option>
                      {#each facets.correspondents as correspondent}
                        <option value={correspondent.id}>{correspondent.name}</option>
                      {/each}
                    </select>
                  {:else}
                    <input class="input sentence-value filter-text"
                           aria-label={`${filter.label} filter ${triggerIndex + 1}`}
                           placeholder={filter.placeholder}
                           bind:value={trigger[filter.key]} />
                  {/if}
                  <button class="icon-button" type="button"
                          onclick={() => removeFilter(triggerIndex, kind)}
                          aria-label={`Remove ${filter.label.toLowerCase()} filter`}>
                    <Icon name="x" size={12} />
                  </button>
                </div>
              {/each}

              {#if availableFilters(triggerIndex).length}
                <div class="add-filter-row">
                  <span class="plus-mark" aria-hidden="true">+</span>
                  <select class="add-filter-select" value=""
                          aria-label={`Add filter to trigger ${triggerIndex + 1}`}
                          onchange={(event) => addFilter(triggerIndex, event)}>
                    <option value="">Add a filter</option>
                    {#each availableFilters(triggerIndex) as filter}
                      <option value={filter.kind}>{filter.label}</option>
                    {/each}
                  </select>
                  <span class="available-filter-hint">
                    {availableFilters(triggerIndex).map(filter => filter.label.toLowerCase()).join(' · ')}
                  </span>
                </div>
              {/if}
            </div>
          {/each}
          <button class="inline-add" type="button" onclick={addTrigger}>
            <span aria-hidden="true">+</span> Add another trigger
          </button>
        </RuleSection>

        <RuleSection class="builder-section model-section" tone="quiet">
          <div class="model-heading">
            <button type="button" class="switch" role="switch" aria-checked={!!editing.ask}
                    aria-label="Ask the document model"
                    onclick={() => setAskEnabled(!editing.ask)}></button>
            <div>
              <h3 id="ask-heading">Ask the document model</h3>
              <p>One closed question can choose which fixed actions run.</p>
            </div>
          </div>
          {#if editing.ask}
            <div class="question-grid">
              <label class="question-field question-copy">
                <span>Question</span>
                <input class="input" aria-label="Classifier question" maxlength="1000"
                       placeholder="Is this a utility bill?"
                       bind:value={editing.ask.question} />
              </label>
              <label class="question-field answer-type">
                <span>Answers</span>
                <select class="input" aria-label="Answer type"
                        bind:value={editing.ask.answer.type}
                        onchange={askAnswerTypeChanged}>
                  {#each ASK_SCHEMA.answers as answer}
                    <option value={answer.type}>{answer.name || answer.type}</option>
                  {/each}
                </select>
              </label>
            </div>
            {#if editing.ask.answer.type === 'yes_no'}
              <p class="model-note">Choose Yes or No on each action below. “Unknown” never runs an action.</p>
            {:else}
              <div class="choice-editor">
                {#each editing.ask.answer.choices as choice, choiceIndex (choiceIndex)}
                  <div class="choice-row">
                    <input class="input" aria-label={`Answer choice ${choiceIndex + 1}`} maxlength="40"
                           bind:value={editing.ask.answer.choices[choiceIndex]}
                           onblur={normalizeActionBranches} />
                    {#if editing.ask.answer.choices.length > (choiceAnswerSchema().min_choices || 2)}
                      <button class="icon-button" type="button"
                              aria-label={`Remove answer choice ${choiceIndex + 1}`}
                              onclick={() => {
                                editing.ask.answer.choices.splice(choiceIndex, 1)
                                normalizeActionBranches()
                              }}>
                        <Icon name="x" size={12} />
                      </button>
                    {/if}
                  </div>
                {/each}
                {#if editing.ask.answer.choices.length < (choiceAnswerSchema().max_choices || 10)}
                  <button class="inline-add" type="button"
                          onclick={() => editing.ask.answer.choices.push(`Option ${editing.ask.answer.choices.length + 1}`)}>
                    <span aria-hidden="true">+</span> Add answer
                  </button>
                {/if}
              </div>
            {/if}
          {/if}
        </RuleSection>

        <RuleSection class="builder-section action-section" title="Then" headingId="then-heading">
          {#each editing.actions as action, actionIndex (actionIndex)}
            <div class="sentence-row action-row">
              {#if actionIndex > 0}<span class="conjunction">then</span>{/if}
              {#if editing.ask}
                <select class="input sentence-select branch-select"
                        aria-label={`Answer for action ${actionIndex + 1}`}
                        bind:value={action.when}>
                  {#each answerBranches() as answer}<option value={answer}>If {answer}</option>{/each}
                </select>
              {/if}
              <select class="input sentence-select action-select"
                      aria-label={`Action ${actionIndex + 1}`}
                      value={action.type}
                      onchange={(event) => {
                        action.type = event.currentTarget.value
                        actionKindChanged(action)
                      }}>
                {#if editing.ask && !availableActionKinds().some(kind => kind.kind === action.type)}
                  <option value={action.type}>Unsupported for a question</option>
                {/if}
                {#each availableActionKinds() as kind}<option value={kind.kind}>{kind.label}</option>{/each}
              </select>

              {#if action.type === 'assign_title'}
                <input class="input action-input" aria-label="Title template"
                       placeholder={'e.g. {{correspondent}} {{date}}'}
                       bind:value={action.params.template} />
              {:else if action.type === 'assign_tags'}
                <div class="tag-picker">
                  {#each action.params.tag_ids || [] as tagID (tagID)}
                    <span class="tag-chip">
                      {tagName(tagID)}
                      <button type="button" aria-label={`Remove tag ${tagName(tagID)}`}
                              onclick={() => removeTag(action, tagID)}>×</button>
                    </span>
                  {/each}
                  <select class="tag-add" value=""
                          aria-label={`Tags for action ${actionIndex + 1}`}
                          onchange={(event) => addTag(action, event)}>
                    <option value="">Add tag…</option>
                    {#each facets.tags.filter(tag => !(action.params.tag_ids || []).map(Number).includes(Number(tag.id))) as tag}
                      <option value={tag.id}>{tag.name}</option>
                    {/each}
                  </select>
                </div>
              {:else if action.type === 'assign_correspondent'}
                <select class="input action-value" aria-label="Correspondent" bind:value={action.params.correspondent_id}>
                  <option value={0}>Choose correspondent…</option>
                  {#each facets.correspondents as correspondent}
                    <option value={correspondent.id}>{correspondent.name}</option>
                  {/each}
                </select>
              {:else if action.type === 'assign_jd_category'}
                <select class="input action-value" aria-label="Filing category" bind:value={action.params.jd_category_id}>
                  <option value={0}>Choose filing category…</option>
                  {#each jdCategories as category}
                    <option value={category.id}>{category.code} · {category.name}</option>
                  {/each}
                </select>
              {:else if action.type === 'assign_owner'}
                <select class="input action-value" aria-label="Owner" bind:value={action.params.owner_id}>
                  <option value={0}>Choose owner…</option>
                  {#each facets.owners as owner}<option value={owner.id}>{ownerLabel(owner)}</option>{/each}
                  {#if action.params.owner_id && !facets.owners.some(owner => Number(owner.id) === Number(action.params.owner_id))}
                    <option value={action.params.owner_id}>Unavailable owner</option>
                  {/if}
                </select>
              {:else if action.type === 'assign_storage_path'}
                <select class="input action-value" aria-label="Folder layout" bind:value={action.params.storage_path_id}>
                  <option value={0}>Choose folder layout…</option>
                  {#each facets.renderedLayouts as layout}
                    <option value={layout.id}>{layout.name}{layout.uses_asn ? ' (previous archive number required)' : ''}</option>
                  {/each}
                  {#if action.params.storage_path_id && !facets.renderedLayouts.some(layout => Number(layout.id) === Number(action.params.storage_path_id))}
                    <option value={action.params.storage_path_id}>Unavailable folder layout</option>
                  {/if}
                </select>
              {:else if action.type === 'assign_custom_field'}
                {@const field = customFieldFor(action)}
                <select class="input action-value" aria-label="Custom field"
                        value={action.params.field_id}
                        onchange={(event) => {
                          action.params.field_id = Number(event.currentTarget.value)
                          customFieldChanged(action)
                        }}>
                  <option value={0}>Choose custom field…</option>
                  {#each availableCustomFields() as candidate}
                    <option value={candidate.id}>{candidate.name} · {candidate.data_type}</option>
                  {/each}
                  {#if action.params.field_id && !availableCustomFields().some(candidate => Number(candidate.id) === Number(action.params.field_id))}
                    <option value={action.params.field_id}>Unavailable custom field</option>
                  {/if}
                </select>
                {#if field?.data_type === 'bool'}
                  <select class="input action-value compact-value" aria-label="Custom field value"
                          value={String(action.params.value)}
                          onchange={(event) => (action.params.value = event.currentTarget.value === 'true')}>
                    <option value="true">Yes</option><option value="false">No</option>
                  </select>
                {:else if field?.data_type === 'select'}
                  <select class="input action-value" aria-label="Custom field value" bind:value={action.params.value}>
                    <option value="">Choose value…</option>
                    {#each customFieldChoices(field) as choice}<option value={choice}>{choice}</option>{/each}
                  </select>
                {:else if field?.data_type === 'multi'}
                  <select class="input action-value" aria-label="Custom field value" multiple size="3"
                          onchange={(event) => (action.params.value = [...event.currentTarget.selectedOptions].map(option => option.value))}>
                    {#each customFieldChoices(field) as choice}
                      <option value={choice} selected={(action.params.value || []).includes(choice)}>{choice}</option>
                    {/each}
                  </select>
                {:else if field?.data_type === 'number' || field?.data_type === 'monetary'}
                  <input class="input action-value" aria-label="Custom field value" type="number" step="any"
                         placeholder={field.data_type === 'monetary' ? 'Amount' : 'Number'}
                         bind:value={action.params.value} />
                {:else if field?.data_type === 'date'}
                  <input class="input action-value" aria-label="Custom field value" type="date"
                         bind:value={action.params.value} />
                {:else if field?.data_type === 'url'}
                  <input class="input action-value" aria-label="Custom field value" type="url"
                         placeholder="https://…" bind:value={action.params.value} />
                {:else if field}
                  <input class="input action-value" aria-label="Custom field value"
                         placeholder="Value" bind:value={action.params.value} />
                {:else}
                  <span class="field-help">Choose a field to set its typed value.</span>
                {/if}
              {:else}
                <input class="input compact-value" type="number" placeholder="ID"
                       aria-label={`Value for action ${actionIndex + 1}`}
                       bind:value={action.params[Object.keys(action.params)[0]]} />
              {/if}

              {#if editing.actions.length > 1}
                <button class="icon-button" type="button"
                        onclick={() => editing.actions.splice(actionIndex, 1)}
                        aria-label={`Remove action ${actionIndex + 1}`}>
                  <Icon name="x" size={12} />
                </button>
              {/if}
            </div>
          {/each}
          <button class="inline-add" type="button" onclick={addAction}>
            <span aria-hidden="true">+</span> Add an action
          </button>
        </RuleSection>
      {/if}

      {#snippet footer()}
        <div class="footer-note">
          {#if editing.id && !editing.preset_slug}
            <button class="delete-action" type="button" onclick={() => remove(editing)}>Delete automation</button>
          {:else}
            Saved automations are on by default. Reorder them in the list.
          {/if}
        </div>
        <div class="footer-actions">
          <button class="btn" type="button" disabled={saving} onclick={closeEditor}>Cancel</button>
          <button class="btn primary save-automation" type="button" disabled={saving} onclick={save}>
            {saving ? 'Saving…' : 'Save automation'}
          </button>
        </div>
      {/snippet}
    </RuleWorkspace>
  {:else}
    <RuleWorkspace class="automations-card"
                   title="Automations"
                   description="Rules that tag, title and route documents as they arrive."
                   headingId="automations-heading">
      {#snippet actions()}
        {#if !loading && userItems.length > 1}<span class="list-order">Run top to bottom</span>{/if}
        {#if !readOnly}
          <button class="btn primary new-automation" onclick={() => openEditor(null)}>
            <Icon name="plus" size={15} /> New automation
          </button>
        {/if}
      {/snippet}

      {#if loading}
        <div class="automation-list" role="list" aria-label="Loading automations">
          {#each Array(3) as _}
            <div class="loading-row"><span></span><div class="skel"></div></div>
          {/each}
        </div>
      {:else if userItems.length}
        <div class="automation-list" role="list">
          {#each userItems as automation (automation.id)}
            {@render automationRow(automation, true)}
          {/each}
        </div>
      {:else}
        <RuleEmpty class="automation-empty"
                   title="No automations yet"
                   description={readOnly ? 'An administrator can create filing and metadata rules.' : 'The first one takes about a minute. Start from a pattern:'}
                   patterns={readOnly ? [] : AUTOMATION_PATTERNS}
                   onselect={openTemplate} />
      {/if}
    </RuleWorkspace>

    {#if !loading && builtInItems.length}
      <section class="builtins">
        <button class="builtins-toggle" class:open={builtInsOpen}
                onclick={() => (builtInsOpen = !builtInsOpen)}
                aria-expanded={builtInsOpen}>
          <span class="chev" class:open={builtInsOpen}><Icon name="chev" size={13} /></span>
          <span class="grow">
            <b>Built-in automations</b>
            <span>Installed by your filing tree</span>
          </span>
          <span class="count-pill">{builtInItems.length}</span>
        </button>
        {#if builtInsOpen}
          <div class="automation-list builtins-list" role="list">
            {#each builtInItems as automation (automation.id)}
              {@render automationRow(automation, false)}
            {/each}
          </div>
        {/if}
      </section>
    {/if}
  {/if}
</div>

<style>
  .automations-page {
    width: min(100%, 1080px);
    margin: 0 auto;
  }
  .page-error { margin-bottom: 12px; }
  .builtins {
    overflow: hidden;
    border: 1px solid var(--line);
    border-radius: 12px;
    background: var(--surface);
  }
  .footer-actions {
    display: flex;
    align-items: center;
    gap: 12px;
    flex: none;
  }
  .list-order {
    color: var(--muted);
    font-size: .74rem;
  }
  .new-automation { padding: 9px 15px; }
  .automation-list { background: var(--surface); }
  .drag-handle {
    width: 24px;
    height: 34px;
    padding: 0;
    border: 0;
    background: transparent;
    color: color-mix(in srgb, var(--muted) 55%, transparent);
    cursor: grab;
    font-size: 1.05rem;
    line-height: 1;
  }
  .drag-handle:active { cursor: grabbing; }
  .drag-handle:hover,
  .drag-handle:focus-visible { color: var(--accent); }
  .drag-handle:disabled { cursor: wait; opacity: .45; }
  .row-indent { width: 24px; }
  .owner-pill,
  .count-pill {
    padding: 2px 7px;
    border-radius: 999px;
    background: var(--tint);
    color: var(--accent);
    font-size: .65rem;
    font-weight: 650;
    white-space: nowrap;
  }
  .text-action {
    padding: 5px 2px;
    border: 0;
    background: transparent;
    color: var(--ink);
    font: inherit;
    font-size: .78rem;
    cursor: pointer;
  }
  .text-action:hover { color: var(--accent); }
  .loading-row {
    display: grid;
    grid-template-columns: 24px minmax(0, 1fr);
    gap: 10px;
    padding: 25px 18px;
    border-bottom: 1px solid var(--line);
  }
  .loading-row .skel { width: 48%; }
  .editor-mode { color: var(--muted); }
  .editor-error { margin: 14px 18px 0; }
  .facet-error {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }
  .duplicate-warning {
    display: grid;
    gap: 10px;
    margin: 14px 18px 0;
    padding: 12px;
    border-left: 3px solid var(--accent);
    background: var(--surface-2);
    font-size: .8rem;
  }
  .duplicate-actions { display: flex; gap: 8px; flex-wrap: wrap; }
  .json-editor { padding: 20px; }
  .json-editor textarea {
    width: 100%;
    max-width: none;
    resize: vertical;
    font-family: "Spline Sans Mono", ui-monospace, monospace;
    font-size: .76rem;
  }
  .builder-name {
    padding: 16px 20px;
    border-bottom: 1px solid var(--line);
  }
  .builder-name .input { width: min(100%, 360px); max-width: none; }
  .trigger-block + .trigger-block {
    margin-top: 14px;
    padding-top: 14px;
    border-top: 1px dashed var(--line);
  }
  .sentence-row {
    display: flex;
    align-items: center;
    gap: 8px;
    min-height: 36px;
    flex-wrap: wrap;
  }
  .filter-row,
  .action-row { margin-top: 6px; }
  .conjunction {
    width: 28px;
    flex: 0 0 28px;
    color: var(--muted);
    font-size: .76rem;
    text-align: right;
  }
  .sentence-select,
  .field-token,
  .operator-token {
    width: auto;
    min-width: 0;
    max-width: none;
    flex: none;
  }
  .trigger-select { min-width: 190px; }
  .field-token,
  .operator-token {
    min-height: 34px;
    padding: 8px 11px;
    border: 1px solid var(--line);
    border-radius: 7px;
    background: var(--surface);
    font-size: .75rem;
    line-height: 1.25;
  }
  .field-token { min-width: 92px; font-weight: 600; }
  .operator-token { color: var(--muted); }
  .sentence-value {
    width: min(100%, 270px);
    max-width: 270px;
  }
  .filter-text {
    font-family: "Spline Sans Mono", ui-monospace, monospace;
    font-size: .74rem;
  }
  .icon-button {
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
  .icon-button:hover { background: var(--surface-2); color: var(--ink); }
  .add-filter-row {
    display: flex;
    align-items: center;
    gap: 6px;
    margin: 9px 0 0 36px;
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
  .inline-add {
    margin: 10px 0 0 36px;
    padding: 3px 0;
    border: 0;
    background: transparent;
    color: var(--accent);
    font-size: .75rem;
    font-weight: 650;
    cursor: pointer;
  }
  .model-heading {
    display: flex;
    align-items: flex-start;
    gap: 10px;
  }
  .model-heading .switch { margin-top: 2px; }
  .model-heading h3 { margin: 0; font-size: .82rem; }
  .model-heading p { margin: 3px 0 0; color: var(--muted); font-size: .74rem; }
  .question-grid {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(150px, 210px);
    gap: 12px;
    max-width: 760px;
    margin: 14px 0 0 44px;
  }
  .question-field {
    display: grid;
    gap: 5px;
    color: var(--muted);
    font-size: .7rem;
  }
  .question-field .input { width: 100%; max-width: none; }
  .model-note {
    margin: 8px 0 0 44px;
    color: var(--muted);
    font-size: .7rem;
  }
  .choice-editor {
    display: grid;
    gap: 7px;
    max-width: 430px;
    margin: 10px 0 0 44px;
  }
  .choice-row { display: flex; align-items: center; gap: 5px; }
  .choice-row .input { flex: 1; max-width: none; }
  .choice-editor .inline-add { margin: 2px 0 0; justify-self: start; }
  .action-row { min-height: 38px; }
  .action-row:first-of-type { padding-left: 36px; }
  .branch-select { min-width: 110px; }
  .action-select { min-width: 145px; }
  .action-input,
  .action-value {
    width: min(100%, 300px);
    max-width: 300px;
    flex: 1;
  }
  .compact-value { max-width: 150px; }
  .tag-picker {
    display: flex;
    align-items: center;
    gap: 5px;
    min-height: 34px;
    max-width: 430px;
    flex: 1;
    flex-wrap: wrap;
  }
  .tag-chip {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    min-height: 28px;
    padding: 4px 8px;
    border-radius: 999px;
    background: var(--tint);
    color: var(--accent);
    font-size: .7rem;
    font-weight: 650;
  }
  .tag-chip button {
    padding: 0;
    border: 0;
    background: transparent;
    color: inherit;
    cursor: pointer;
    font-size: .9rem;
    line-height: 1;
  }
  .tag-add {
    min-height: 30px;
    padding: 4px 22px 4px 9px;
    border: 1px dashed var(--line);
    border-radius: 999px;
    background: transparent;
    color: var(--muted);
    font-size: .7rem;
  }
  .field-help { color: var(--muted); font-size: .72rem; }
  .footer-note { min-width: 0; }
  .delete-action {
    padding: 4px 0;
    border: 0;
    background: transparent;
    color: var(--danger, #b42318);
    font-size: .72rem;
    cursor: pointer;
  }
  .save-automation { min-width: 126px; }
  .builtins { margin-top: 14px; box-shadow: none; }
  .builtins-toggle {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    min-height: 58px;
    padding: 10px 18px;
    border: 0;
    background: var(--surface-2);
    color: var(--ink);
    text-align: left;
    cursor: pointer;
  }
  .builtins-toggle.open { border-bottom: 1px solid var(--line); }
  .builtins-toggle:hover { background: var(--tint); }
  .builtins-toggle .grow { display: flex; flex-direction: column; gap: 1px; }
  .builtins-toggle .grow span { color: var(--muted); font-size: .72rem; }
  .builtins-toggle .chev { display: flex; transition: transform .15s; }
  .builtins-toggle .chev.open { transform: rotate(90deg); }
  @media (max-width: 760px) {
    .question-grid { grid-template-columns: 1fr; }
    .sentence-row { align-items: flex-start; }
    .conjunction { padding-top: 9px; }
    .action-row:first-of-type { padding-left: 36px; }
    .action-input,
    .action-value,
    .sentence-value,
    .tag-picker { max-width: none; }
  }
  @media (max-width: 520px) {
    .builtins { border-radius: 9px; }
    .builder-name { padding-left: 14px; padding-right: 14px; }
    .footer-actions { justify-content: flex-end; }
    .question-grid,
    .model-note,
    .choice-editor { margin-left: 0; }
    .available-filter-hint { display: none; }
    .add-filter-row,
    .inline-add { margin-left: 0; }
    .sentence-row {
      display: grid;
      grid-template-columns: 28px minmax(0, 1fr) auto;
    }
    .sentence-row > .conjunction { grid-column: 1; grid-row: 1; }
    .trigger-row .sentence-select,
    .filter-row .field-token,
    .action-row .branch-select,
    .action-row .action-select { grid-column: 2; }
    .filter-row .operator-token,
    .filter-row .sentence-value,
    .action-row .action-input,
    .action-row .action-value,
    .action-row .tag-picker { grid-column: 2 / -1; width: 100%; }
    .filter-row .icon-button,
    .action-row .icon-button { grid-column: 3; grid-row: 1; }
    .action-row:first-of-type { padding-left: 0; }
  }
</style>
