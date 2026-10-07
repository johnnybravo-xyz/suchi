<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { listAutomations, createAutomation, patchAutomation, deleteAutomation,
           listTags, listCorrespondents, listCustomFields, listRenderedLayouts,
           adminListUsers, systemMembers, automationsSchema } from '../lib/api.js'
  import { systems } from '../lib/systems.svelte.js'
  import Icon from '../lib/Icon.svelte'

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

  const userItems = $derived(items.filter(a => !a.preset_slug))
  const builtInItems = $derived(items.filter(a => a.preset_slug))

  let TRIGGER_TYPES = $state([])
  let ACTION_KINDS = $state([])
  let ASK_SCHEMA = $state({
    max_enabled: 5,
    answers: [{ type: 'yes_no', name: 'Yes / No' }, { type: 'choice', name: 'Multiple choice' }],
    action_kinds: ['assign_tags', 'assign_correspondent', 'assign_jd_category', 'assign_custom_field'],
  })
  automationsSchema().then(sc => {
    if (sc?.triggers?.length) TRIGGER_TYPES = sc.triggers.map(t => ({ code: t.code, label: t.name || t.type }))
    if (sc?.actions?.length) ACTION_KINDS = sc.actions.map(a => ({
      kind: a.kind, label: a.name || a.kind, description: a.description || '', params: a.params || [],
    }))
    if (sc?.ask) ASK_SCHEMA = sc.ask
  }).catch(ex => { err = ex.message || 'Could not load the automation schema.' })

  const blank = () => ({
    name: '', enabled: true, order: items.length,
    triggers: [{ type: 2, filter_filename: '', filter_path: '', filter_title_matching: '', filter_content_matching: '',
                 filter_has_tag: 0, filter_has_correspondent: 0 }],
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

  function openEditor(a) {
    loadFacets()
    editing = a ? JSON.parse(JSON.stringify(a)) : blank()
    mode = 'builder'
    draftErr = ''
  }
  function switchMode(m) {
    draftErr = ''
    if (m === 'json') {
      try { jsonDraft = JSON.stringify(editing, null, 2); mode = 'json' }
      catch (ex) { draftErr = ex.message; return }
    }
    else {
      try { editing = JSON.parse(jsonDraft); mode = 'builder' }
      catch { draftErr = 'Fix the JSON before switching back to the builder.' }
    }
  }

  function addTrigger() { editing.triggers.push({ type: 2, filter_filename: '', filter_path: '', filter_title_matching: '', filter_content_matching: '', filter_has_tag: 0, filter_has_correspondent: 0 }) }
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
    editing.triggers = (editing.triggers?.length ? editing.triggers : [blank().triggers[0]]).map(trigger => ({
      ...trigger,
      type: 2,
      filter_filename: '',
      filter_path: '',
      filter_email_from: '',
      filter_email_subject: '',
      filter_email_folder: '',
      filter_email_has_attachment: null,
    }))
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

  async function save() {
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
    try {
      if (body.id) await patchAutomation(body.id, body)
      else await createAutomation(body)
      editing = null
      notify?.('Automation saved')
      load()
    } catch (ex) {
      if (ex.status === 409 && ex.code === 'duplicate_rule' && ex.data?.match) {
        duplicate = ex.data.match
        return
      }
      draftErr = ex.message || 'The server rejected this spec.'
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
    try { await deleteAutomation(a.id); items = items.filter(x => x.id !== a.id); notify?.('Deleted') }
    catch (ex) { notify?.(ex.message || 'Could not delete') }
  }

  function ownerLabel(owner) {
    if (!owner) return ''
    return owner.display_name ? `${owner.display_name} · ${owner.email}` : owner.email
  }

  function summary(a) {
    const trig = (a.triggers || []).map(t => TRIGGER_TYPES.find(x => x.code === t.type)?.label || `type ${t.type}`).join(', ')
    const ask = a.ask?.question ? ` · asks “${a.ask.question}”` : ''
    return `${trig || 'no trigger'}${ask} → ${(a.actions || []).length} action${(a.actions || []).length === 1 ? '' : 's'}`
  }

  function actionDescribe(a) {
    const spec = ACTION_KINDS.find(k => k.kind === a.type)
    const p = a.params || {}
    const branch = a.when ? `If “${a.when}”: ` : ''
    switch (a.type) {
      case 'assign_tags': {
        const names = (p.tag_ids || []).map(id => facets.tags.find(t => t.id === id)?.name).filter(Boolean)
        return branch + (names.length ? `Add tags: ${names.join(', ')}` : 'Add tags')
      }
      case 'assign_correspondent': {
        const name = facets.correspondents.find(c => c.id === p.correspondent_id)?.name
        return branch + (name ? `Set correspondent → ${name}` : 'Set correspondent')
      }
      case 'assign_jd_category': {
        const jd = jdCategories.find(x => x.id === p.jd_category_id)
        return branch + (jd ? `Assign filing category → ${jd.code} · ${jd.name}` : 'Assign filing category')
      }
      case 'assign_title':
        return branch + (p.template ? `Set title → “${p.template}”` : 'Set title')
      case 'assign_owner': {
        const owner = facets.owners.find(candidate => Number(candidate.id) === Number(p.owner_id))
        return branch + (owner ? `Set owner → ${ownerLabel(owner)}` : 'Set owner')
      }
      case 'assign_storage_path': {
        const layout = facets.renderedLayouts.find(candidate => Number(candidate.id) === Number(p.storage_path_id))
        return branch + (layout ? `Assign folder layout → ${layout.name}` : 'Assign folder layout')
      }
      case 'assign_custom_field': {
        const field = facets.customFields.find(candidate => Number(candidate.id) === Number(p.field_id))
        const value = Array.isArray(p.value) ? p.value.join(', ') : String(p.value ?? '')
        return branch + (field ? `Set ${field.name} → ${value}` : 'Set custom field')
      }
      default:
        return branch + (spec?.label || a.type)
    }
  }

  load()
</script>

<div class="automations-page">
  <header class="automations-intro">
    <div>
      <span class="eyebrow">Filing rules</span>
      <h2>Automate your filing</h2>
      <p>When a trigger fires and its filters match, the actions run in order.</p>
    </div>
    {#if !readOnly}
      <button class="btn primary new-automation" onclick={() => openEditor(null)}><Icon name="plus" size={15} /> New automation</button>
    {/if}
  </header>

{#if err}<div class="err">{err}</div>{/if}

{#if editing !== null}
  <div class="card automation-editor" style="margin-bottom:16px">
    <div class="toolbar" style="margin-bottom:12px">
      <h3 style="margin:0">
        {#if editing.id}Edit “{editing.name}”
        {:else}New automation
        {/if}
      </h3>
      <span class="spacer"></span>
      <span class="seg">
        <button class:on={mode === 'builder'} onclick={() => switchMode('builder')}>Builder</button>
        <button class:on={mode === 'json'} onclick={() => switchMode('json')}>JSON</button>
      </span>
    </div>
    {#if draftErr}<div class="err">{draftErr}</div>{/if}
    {#if facetsError}
      <div class="err" style="display:flex;align-items:center;justify-content:space-between;gap:12px">
        <span>{facetsError}</span><button class="btn sm" onclick={loadFacets}>Retry</button>
      </div>
    {/if}
    {#if duplicate}
      <div class="err" style="border-left:3px solid var(--accent);background:var(--surface-2)">
        <div style="margin-bottom:6px">
          This automation already exists as <b>“{duplicate.name}”</b>
          {#if duplicate.enabled}(currently enabled).{:else}(currently disabled).{/if}
        </div>
        <div style="display:flex;gap:8px;flex-wrap:wrap">
          {#if duplicate.enabled}
            <button class="btn sm primary" onclick={openExistingDuplicate}>Open existing</button>
          {:else}
            <button class="btn sm primary" onclick={enableExistingDuplicate}>Use existing</button>
            <button class="btn sm" onclick={openExistingDuplicate}>Edit existing</button>
          {/if}
          <button class="btn sm" onclick={() => (duplicate = null)}>Cancel</button>
        </div>
      </div>
    {/if}

    {#if mode === 'json'}
      <textarea class="input" rows="16" bind:value={jsonDraft} spellcheck="false"></textarea>
    {:else}
      <div class="toolbar">
        <input class="input" style="flex:1" placeholder="Name, e.g. Tag utility bills" bind:value={editing.name} />
        <span class="switch-control">
          <span>{editing.enabled ? 'Enabled' : 'Disabled'}</span>
          <button type="button" class="switch" role="switch" aria-checked={editing.enabled}
                  aria-label="Automation enabled"
                  onclick={() => (editing.enabled = !editing.enabled)}></button>
        </span>
      </div>

      <div class="side-head" style="padding-left:0">When</div>
      {#each editing.triggers as t, i (i)}
        <div class="brow">
          <select class="input" bind:value={t.type} disabled={!!editing.ask} aria-label={`Trigger ${i + 1}`}>
            {#each TRIGGER_TYPES as tt}<option value={tt.code}>{tt.label}</option>{/each}
          </select>
          {#if !editing.ask}
            <input class="input" placeholder="filename matches (glob)" bind:value={t.filter_filename} />
          {/if}
          <input class="input" placeholder="title matches (regex)" bind:value={t.filter_title_matching} />
          <input class="input" placeholder="content matches (regex)" bind:value={t.filter_content_matching} />
          <select class="input" bind:value={t.filter_has_tag}>
            <option value={0}>any tag</option>
            {#each facets.tags as x}<option value={x.id}>has: {x.name}</option>{/each}
          </select>
          <select class="input" bind:value={t.filter_has_correspondent}>
            <option value={0}>any correspondent</option>
            {#each facets.correspondents as x}<option value={x.id}>from: {x.name}</option>{/each}
          </select>
          {#if editing.triggers.length > 1}
            <button class="btn sm" onclick={() => editing.triggers.splice(i, 1)} title="Remove trigger" aria-label={`Remove trigger ${i + 1}`}><Icon name="x" size={12} /></button>
          {/if}
        </div>
      {/each}
      <button class="btn sm" style="margin:8px 0 4px" onclick={addTrigger}><Icon name="plus" size={12} /> Trigger</button>

      <section class="ask-editor" aria-labelledby="ask-heading">
        <div class="ask-toggle">
          <div>
            <b id="ask-heading">Ask the classifier</b>
            <p>Ask one closed question after content extraction, then run actions for the selected answer. Up to {ASK_SCHEMA.max_enabled || 5} enabled questions may match a document.</p>
          </div>
          <input type="checkbox" aria-label="Ask the classifier"
                 checked={!!editing.ask}
                 onchange={(event) => setAskEnabled(event.currentTarget.checked)} />
        </div>
        {#if editing.ask}
          <label class="ask-field">
            <span>Question</span>
            <input class="input" aria-label="Classifier question" maxlength="1000"
                   placeholder="Is this valid warranty proof?"
                   bind:value={editing.ask.question} />
          </label>
          <div class="ask-answer">
            <label class="ask-field">
              <span>Answers</span>
              <select class="input" aria-label="Answer type" bind:value={editing.ask.answer.type}
                      onchange={askAnswerTypeChanged}>
                {#each ASK_SCHEMA.answers as answer}
                  <option value={answer.type}>{answer.name || answer.type}</option>
                {/each}
              </select>
            </label>
            {#if editing.ask.answer.type === 'yes_no'}
              <span class="sub">Branches: Yes and No. “Unknown” never runs an action.</span>
            {:else}
              <div class="ask-choices">
                {#each editing.ask.answer.choices as choice, choiceIndex (choiceIndex)}
                  <div class="ask-choice">
                    <input class="input" aria-label={`Answer choice ${choiceIndex + 1}`} maxlength="40"
                           bind:value={editing.ask.answer.choices[choiceIndex]}
                           onblur={normalizeActionBranches} />
                    {#if editing.ask.answer.choices.length > (choiceAnswerSchema().min_choices || 2)}
                      <button class="btn sm" type="button" aria-label={`Remove answer choice ${choiceIndex + 1}`}
                              onclick={() => { editing.ask.answer.choices.splice(choiceIndex, 1); normalizeActionBranches() }}>
                        <Icon name="x" size={12} />
                      </button>
                    {/if}
                  </div>
                {/each}
                {#if editing.ask.answer.choices.length < (choiceAnswerSchema().max_choices || 10)}
                  <button class="btn sm" type="button"
                          onclick={() => editing.ask.answer.choices.push(`Option ${editing.ask.answer.choices.length + 1}`)}>
                    <Icon name="plus" size={12} /> Choice
                  </button>
                {/if}
              </div>
            {/if}
          </div>
          <p class="sub ask-note">Question rules use persisted title, content, tags, and correspondent filters only. Answers are validated against the configured choices and document evidence.</p>
        {/if}
      </section>


      <div class="side-head" style="padding-left:0">Then</div>
      {#each editing.actions as a, i (i)}
        <div class="brow">
          {#if editing.ask}
            <select class="input action-branch" aria-label={`Answer for action ${i + 1}`} bind:value={a.when}>
              {#each answerBranches() as answer}<option value={answer}>If {answer}</option>{/each}
            </select>
          {/if}
          <select class="input" aria-label={`Action ${i + 1}`} value={a.type} onchange={(e) => { a.type = e.target.value; actionKindChanged(a) }}>
            {#if editing.ask && !availableActionKinds().some(kind => kind.kind === a.type)}
              <option value={a.type}>Unsupported for a question</option>
            {/if}
            {#each availableActionKinds() as k}<option value={k.kind}>{k.label}</option>{/each}
          </select>
          {#if a.type === 'assign_title'}
            <input class="input" style="flex:1;max-width:none" placeholder={'title template, e.g. {{correspondent}} {{date}}'} bind:value={a.params.template} />
          {:else if a.type === 'assign_tags'}
            <select class="input" multiple size="3" style="max-width:220px"
                    aria-label={`Tags for action ${i + 1}`}
                    onchange={(e) => (a.params.tag_ids = [...e.target.selectedOptions].map(o => Number(o.value)))}>
              {#each facets.tags as x}<option value={x.id} selected={a.params.tag_ids?.includes(x.id)}>{x.name}</option>{/each}
            </select>
          {:else if a.type === 'assign_correspondent'}
            <select class="input" bind:value={a.params.correspondent_id}>
              <option value={0}>choose…</option>
              {#each facets.correspondents as x}<option value={x.id}>{x.name}</option>{/each}
            </select>
          {:else if a.type === 'assign_jd_category'}
            <select class="input" bind:value={a.params.jd_category_id}>
              <option value={0}>choose…</option>
              {#each jdCategories as x}<option value={x.id}>{x.code} · {x.name}</option>{/each}
            </select>
          {:else if a.type === 'assign_owner'}
            <select class="input action-value" aria-label="Owner" bind:value={a.params.owner_id}>
              <option value={0}>Choose owner…</option>
              {#each facets.owners as owner}
                <option value={owner.id}>{ownerLabel(owner)}</option>
              {/each}
              {#if a.params.owner_id && !facets.owners.some(owner => Number(owner.id) === Number(a.params.owner_id))}
                <option value={a.params.owner_id}>Unavailable owner</option>
              {/if}
            </select>
          {:else if a.type === 'assign_storage_path'}
            <select class="input action-value" aria-label="Folder layout" bind:value={a.params.storage_path_id}>
              <option value={0}>Choose folder layout…</option>
              {#each facets.renderedLayouts as layout}
                <option value={layout.id}>{layout.name}{layout.uses_asn ? ' (previous archive number required)' : ''}</option>
              {/each}
              {#if a.params.storage_path_id && !facets.renderedLayouts.some(layout => Number(layout.id) === Number(a.params.storage_path_id))}
                <option value={a.params.storage_path_id}>Unavailable folder layout</option>
              {/if}
            </select>
          {:else if a.type === 'assign_custom_field'}
            {@const field = customFieldFor(a)}
            <select class="input" aria-label="Custom field" value={a.params.field_id}
                    onchange={(event) => { a.params.field_id = Number(event.currentTarget.value); customFieldChanged(a) }}>
              <option value={0}>Choose custom field…</option>
              {#each availableCustomFields() as candidate}<option value={candidate.id}>{candidate.name} · {candidate.data_type}</option>{/each}
              {#if a.params.field_id && !availableCustomFields().some(candidate => Number(candidate.id) === Number(a.params.field_id))}
                <option value={a.params.field_id}>Unavailable custom field</option>
              {/if}
            </select>
            {#if field?.data_type === 'bool'}
              <select class="input action-value" aria-label="Custom field value" value={String(a.params.value)}
                      onchange={(event) => (a.params.value = event.currentTarget.value === 'true')}>
                <option value="true">Yes</option><option value="false">No</option>
              </select>
            {:else if field?.data_type === 'select'}
              <select class="input action-value" aria-label="Custom field value" bind:value={a.params.value}>
                <option value="">Choose value…</option>
                {#each customFieldChoices(field) as choice}<option value={choice}>{choice}</option>{/each}
              </select>
            {:else if field?.data_type === 'multi'}
              <select class="input action-value" aria-label="Custom field value" multiple size="3"
                      onchange={(event) => (a.params.value = [...event.currentTarget.selectedOptions].map(option => option.value))}>
                {#each customFieldChoices(field) as choice}<option value={choice} selected={(a.params.value || []).includes(choice)}>{choice}</option>{/each}
              </select>
            {:else if field?.data_type === 'number' || field?.data_type === 'monetary'}
              <input class="input action-value" aria-label="Custom field value" type="number" step="any" placeholder={field.data_type === 'monetary' ? 'Amount' : 'Number'} bind:value={a.params.value} />
            {:else if field?.data_type === 'date'}
              <input class="input action-value" aria-label="Custom field value" type="date" bind:value={a.params.value} />
            {:else if field?.data_type === 'url'}
              <input class="input action-value" aria-label="Custom field value" type="url" placeholder="https://…" bind:value={a.params.value} />
            {:else if field?.data_type === 'documentlink'}
              <input class="input action-value" aria-label="Custom field value" type="number" min="1" placeholder="Linked document ID" bind:value={a.params.value} />
            {:else if field}
              <input class="input action-value" aria-label="Custom field value" placeholder="Value" bind:value={a.params.value} />
            {:else}
              <span class="sub">Choose a field to set its typed value.</span>
            {/if}
          {:else}
            <input class="input" style="max-width:130px" type="number" placeholder="id" bind:value={a.params[Object.keys(a.params)[0]]} />
          {/if}
          {#if editing.actions.length > 1}
            <button class="btn sm" onclick={() => editing.actions.splice(i, 1)} title="Remove action" aria-label={`Remove action ${i + 1}`}><Icon name="x" size={12} /></button>
          {/if}
        </div>
      {/each}
      <button class="btn sm" style="margin:8px 0 0" onclick={addAction}><Icon name="plus" size={12} /> Action</button>
    {/if}

    <div class="toolbar" style="margin:14px 0 0">
      <button class="btn primary sm" onclick={save}>Save automation</button>
      <button class="btn sm" onclick={() => (editing = null)}>Cancel</button>
    </div>
  </div>
{/if}

{#snippet automationRow(a)}
  <div class="irow automation-row" id={`automation-${a.id}`} style="align-items:flex-start">
    <span class="grow">
      <span class="title" style="display:block">
        {a.name || `Automation #${a.id}`}
        {#if a.preset_slug}<span class="pill" style="margin-left:8px;font-size:.7rem;background:#eef2ff;color:#3730a3">Owned by {a.preset_slug} filing tree</span>{/if}
      </span>
      <span class="sub">{summary(a)}</span>
      {#if (a.actions || []).length > 0}
        <ul class="acts">
          {#each a.actions as act (act.order_index ?? act.id)}
            <li>{actionDescribe(act)}</li>
          {/each}
        </ul>
      {/if}
    </span>
    {#if readOnly}
      <span class="pill" class:ok={a.enabled} class:warn={!a.enabled}>{a.enabled ? 'enabled' : 'disabled'}</span>
      <button class="btn sm" onclick={() => (peekID = peekID === a.id ? null : a.id)}>{peekID === a.id ? 'Hide JSON' : 'View JSON'}</button>
    {:else}
      <span class="switch-control automation-switch">
        <span>{a.enabled ? 'Enabled' : 'Disabled'}</span>
        <button type="button" class="switch" role="switch" aria-checked={a.enabled}
                aria-label={`${a.name || `Automation ${a.id}`} enabled`}
                disabled={toggleID !== null}
                onclick={() => toggle(a)}></button>
      </span>
      <button class="btn sm" onclick={() => openEditor(a)}>Edit</button>
      {#if !a.preset_slug}
        <button class="btn sm danger" onclick={() => remove(a)} title="Delete automation" aria-label={`Delete ${a.name}`}><Icon name="trash" size={13} /></button>
      {/if}
    {/if}
    {#if readOnly && peekID === a.id}
      <pre class="spec">{JSON.stringify(a, null, 2)}</pre>
    {/if}
  </div>
{/snippet}

<section class="automations-panel" aria-labelledby="custom-automations-heading">
  <div class="panel-head">
    <div>
      <h3 id="custom-automations-heading">Your automations</h3>
      <span class="panel-count" class:chip={!loading && userItems.length > 0} aria-live="polite">{loading ? '…' : userItems.length}</span>
    </div>
    <span class="panel-hint">Rules run in the order shown</span>
  </div>

  {#if loading}
    <div class="automation-list" aria-label="Loading automations">
      {#each Array(3) as _}
        <div class="automation-loading-row"><div class="skel" style="width:50%"></div></div>
      {/each}
    </div>
  {:else if userItems.length}
    <div class="index automation-list">
      {#each userItems as a (a.id)}{@render automationRow(a)}{/each}
    </div>
  {:else}
    <div class="empty automation-empty">
      <b>No custom automations yet</b>
      <span>{readOnly ? 'An administrator can create filing and metadata rules.' : 'Create one to tag, title, and route documents as they arrive.'}</span>
    </div>
  {/if}
</section>

{#if !loading && builtInItems.length}
  <section class="builtins">
    <button class="builtins-toggle" class:open={builtInsOpen} onclick={() => (builtInsOpen = !builtInsOpen)} aria-expanded={builtInsOpen}>
      <span class="chev" class:open={builtInsOpen}><Icon name="chev" size={13} /></span>
      <span class="grow">
        <b>Built-in automations</b>
        <span class="sub">Installed by your filing tree</span>
      </span>
      <span class="chip">{builtInItems.length}</span>
    </button>
    {#if builtInsOpen}
      <div class="index builtins-list">
        {#each builtInItems as a (a.id)}{@render automationRow(a)}{/each}
      </div>
    {/if}
  </section>
{/if}
</div>

<style>
  .automations-page { max-width: 960px; margin: 0 auto; }
  .automations-intro { display: flex; align-items: flex-end; justify-content: space-between; gap: 28px; margin: 10px 0 26px; }
  .automations-intro > div { max-width: 610px; }
  .eyebrow { display: block; margin-bottom: 7px; color: var(--accent); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .66rem; font-weight: 700; }
  .automations-intro h2 { font-size: 1.55rem; line-height: 1.18; }
  .automations-intro p { margin: 8px 0 0; color: var(--muted); font-size: .9rem; }
  .new-automation { flex: none; padding: 9px 16px; }
  .automation-editor { padding: 16px; }
  .automations-panel, .builtins { overflow: hidden; background: var(--surface); border: 1px solid var(--line); border-radius: var(--r); }
  .panel-head { display: flex; align-items: center; justify-content: space-between; gap: 18px; padding: 14px 17px; border-bottom: 1px solid var(--line); background: var(--surface-2); }
  .panel-head > div { display: flex; align-items: baseline; gap: 9px; }
  .panel-head h3 { font-size: .9rem; }
  .panel-head > span { color: var(--muted); font-size: .72rem; }
  .panel-count { color: var(--muted); font-size: .72rem; }
  .panel-count.chip { color: var(--accent); }
  .panel-hint { text-align: right; }
  .automation-list.index { border: 0; border-radius: 0; }
  .automation-loading-row { padding: 24px 17px; border-bottom: 1px solid var(--line); }
  .automation-loading-row:last-child { border-bottom: 0; }
  .automation-empty { padding: 48px 20px; }
  .builtins { margin-top: 14px; }
  .builtins-toggle {
    display: flex; align-items: center; gap: 10px; width: 100%; min-height: 58px;
    padding: 10px 16px; border: 0; border-radius: 0;
    background: var(--surface-2); color: var(--ink); text-align: left;
  }
  .builtins-toggle.open { border-bottom: 1px solid var(--line); }
  .builtins-toggle:hover { background: var(--tint); }
  .builtins-toggle .grow { display: flex; flex-direction: column; gap: 1px; }
  .builtins-toggle .sub { color: var(--muted); font-size: .76rem; }
  .builtins-toggle .chev { display: flex; transition: transform .15s; }
  .builtins-toggle .chev.open { transform: rotate(90deg); }
  .builtins-list { margin: 0; border: 0; border-radius: 0; }
  .acts {
    margin: 6px 0 2px;
    padding: 0 0 0 16px;
    color: var(--muted);
    font-size: .8rem;
    line-height: 1.4;
    white-space: normal;
  }
  .acts li { margin: 2px 0; }
  .spec {
    flex: 1 0 100%;
    max-height: 320px;
    margin: 10px 0 0;
    padding: 12px;
    overflow: auto;
    border: 1px solid var(--line);
    border-radius: 6px;
    background: var(--surface-2);
    font-size: .75rem;
    white-space: pre-wrap;
  }
  .action-value { flex:1; max-width:260px; }
  select.action-value[multiple] { min-width:180px; }
  .automation-switch { margin-top: 1px; }
  .ask-editor {
    margin: 14px 0 8px;
    padding: 14px;
    border: 1px solid var(--line);
    border-radius: 8px;
    background: var(--surface-2);
  }
  .ask-toggle { display: flex; align-items: flex-start; justify-content: space-between; gap: 18px; }
  .ask-toggle p { max-width: 680px; margin: 4px 0 0; color: var(--muted); font-size: .8rem; line-height: 1.45; }
  .ask-toggle input { width: 18px; height: 18px; flex: none; }
  .ask-field { display: grid; gap: 5px; margin-top: 12px; color: var(--muted); font-size: .75rem; }
  .ask-field .input { max-width: none; }
  .ask-answer { display: grid; grid-template-columns: minmax(160px, 220px) 1fr; align-items: end; gap: 12px; }
  .ask-choices { display: grid; gap: 7px; padding-top: 12px; }
  .ask-choice { display: flex; gap: 6px; }
  .ask-choice .input { flex: 1; max-width: none; }
  .ask-note { display: block; margin-top: 10px; line-height: 1.45; }
  .action-branch { max-width: 180px; }
  @media (max-width: 620px) {
    .automations-intro { align-items: flex-start; flex-direction: column; gap: 16px; margin-top: 2px; }
    .new-automation { width: 100%; justify-content: center; }
    .panel-head { align-items: flex-start; }
    .panel-hint { display: none; }
    .automation-row { flex-wrap: wrap; gap: 8px; }
    .ask-answer { grid-template-columns: 1fr; }
    .automation-row > .grow { flex-basis: 100%; }
  }
</style>
