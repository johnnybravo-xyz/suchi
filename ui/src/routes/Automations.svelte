<script>
  import { listAutomations, createAutomation, patchAutomation, deleteAutomation,
           listTags, listCorrespondents, listDocumentTypes, automationsSchema } from '../lib/api.js'
  import Icon from '../lib/Icon.svelte'

  let { notify } = $props()
  let items = $state([])
  let loading = $state(true)
  let err = $state('')
  let editing = $state(null)        // null | working copy (id present = edit)
  let mode = $state('builder')      // 'builder' | 'json'
  let jsonDraft = $state('')
  let draftErr = $state('')
  let facets = $state({ tags: [], correspondents: [], types: [] })

  // Enums come from GET /api/automations/schema so new kinds appear
  // without a UI release; these literals are only the offline fallback.
  let TRIGGER_TYPES = $state([
    { code: 2, label: 'Document added' },
    { code: 3, label: 'Document updated' },
    { code: 1, label: 'Consumption (before filing)' },
  ])
  let ACTION_KINDS = $state([
    { kind: 'assign_title',         label: 'Set title',        params: ['template'] },
    { kind: 'assign_tags',          label: 'Add tags',         params: ['tag_ids'] },
    { kind: 'assign_correspondent', label: 'Set correspondent', params: ['correspondent_id'] },
    { kind: 'assign_document_type', label: 'Set document type', params: ['document_type_id'] },
    { kind: 'assign_storage_path',  label: 'Set storage path', params: ['storage_path_id'] },
    { kind: 'assign_owner',         label: 'Set owner',        params: ['owner_id'] },
    { kind: 'assign_custom_field',  label: 'Set custom field', params: ['field_id', 'value'] },
  ])
  automationsSchema().then(sc => {
    if (sc?.triggers?.length) TRIGGER_TYPES = sc.triggers.map(t => ({ code: t.code, label: t.name || t.type }))
    if (sc?.actions?.length) ACTION_KINDS = sc.actions.map(a => ({
      kind: a.kind, label: a.name || a.kind, params: (a.params || []).map(p => p.name),
    }))
  }).catch(() => {})

  const blank = () => ({
    name: '', enabled: true, order: items.length,
    triggers: [{ type: 2, filter_filename: '', filter_path: '', filter_content_matching: '',
                 filter_has_tag: 0, filter_has_correspondent: 0, filter_has_document_type: 0 }],
    actions: [{ type: 'assign_tags', params: { tag_ids: [] } }],
  })

  async function load() {
    loading = true; err = ''
    try {
      const res = await listAutomations()
      items = res?.results || res || []
    } catch (ex) { err = ex.message || 'Could not load automations.' }
    finally { loading = false }
  }
  async function loadFacets() {
    try {
      const [t, c, d] = await Promise.all([listTags(), listCorrespondents(), listDocumentTypes()])
      facets = { tags: t?.results || [], correspondents: c?.results || [], types: d?.results || [] }
    } catch {}
  }

  function openEditor(a) {
    editing = a ? JSON.parse(JSON.stringify(a)) : blank()
    mode = 'builder'
    draftErr = ''
  }
  function switchMode(m) {
    draftErr = ''
    if (m === 'json') { jsonDraft = JSON.stringify(editing, null, 2); mode = 'json' }
    else {
      try { editing = JSON.parse(jsonDraft); mode = 'builder' }
      catch { draftErr = 'Fix the JSON before switching back to the builder.' }
    }
  }

  function addTrigger() { editing.triggers.push({ type: 2, filter_filename: '', filter_path: '', filter_content_matching: '', filter_has_tag: 0, filter_has_correspondent: 0, filter_has_document_type: 0 }) }
  function addAction() { editing.actions.push({ type: 'assign_tags', params: { tag_ids: [] } }) }
  function actionKindChanged(a) {
    const spec = ACTION_KINDS.find(k => k.kind === a.type)
    a.params = Object.fromEntries((spec?.params || []).map(p => [p, p === 'tag_ids' ? [] : p === 'template' || p === 'value' ? '' : 0]))
  }

  async function save() {
    draftErr = ''
    let body = editing
    if (mode === 'json') {
      try { body = JSON.parse(jsonDraft) } catch { draftErr = 'Not valid JSON.'; return }
    }
    if (!body.name?.trim()) { draftErr = 'Give it a name.'; return }
    // numeric coercion for select-bound ids
    for (const t of body.triggers || []) for (const k of ['type','filter_has_tag','filter_has_correspondent','filter_has_document_type']) t[k] = Number(t[k]) || 0
    for (const a of body.actions || []) for (const k of Object.keys(a.params || {}))
      if (k.endsWith('_id')) a.params[k] = Number(a.params[k]) || 0
      else if (k === 'tag_ids') a.params[k] = (a.params[k] || []).map(Number)
    try {
      if (body.id) await patchAutomation(body.id, body)
      else await createAutomation(body)
      editing = null
      notify?.('Automation saved')
      load()
    } catch (ex) { draftErr = ex.message || 'The server rejected this spec.' }
  }

  async function toggle(a) {
    try { await patchAutomation(a.id, { enabled: !a.enabled }); a.enabled = !a.enabled; notify?.(a.enabled ? 'Enabled' : 'Disabled') }
    catch (ex) { notify?.(ex.message || 'Could not toggle') }
  }
  async function remove(a) {
    if (!confirm(`Delete automation “${a.name}”?`)) return
    try { await deleteAutomation(a.id); items = items.filter(x => x.id !== a.id); notify?.('Deleted') }
    catch (ex) { notify?.(ex.message || 'Could not delete') }
  }

  function summary(a) {
    const trig = (a.triggers || []).map(t => TRIGGER_TYPES.find(x => x.code === t.type)?.label || `type ${t.type}`).join(', ')
    return `${trig || 'no trigger'} → ${(a.actions || []).length} action${(a.actions || []).length === 1 ? '' : 's'}`
  }

  load(); loadFacets()
</script>

<div class="toolbar">
  <span class="sub" style="color:var(--muted)">When a trigger fires and its filters match, the actions run in order.</span>
  <span class="spacer"></span>
  <button class="btn primary sm" onclick={() => openEditor(null)}><Icon name="plus" size={13} /> New automation</button>
</div>

{#if err}<div class="err">{err}</div>{/if}

{#if editing !== null}
  <div class="card" style="margin-bottom:16px">
    <div class="toolbar" style="margin-bottom:12px">
      <h3 style="margin:0">{editing.id ? `Edit “${editing.name}”` : 'New automation'}</h3>
      <span class="spacer"></span>
      <span class="seg">
        <button class:on={mode === 'builder'} onclick={() => switchMode('builder')}>Builder</button>
        <button class:on={mode === 'json'} onclick={() => switchMode('json')}>JSON</button>
      </span>
    </div>
    {#if draftErr}<div class="err">{draftErr}</div>{/if}

    {#if mode === 'json'}
      <textarea class="input" rows="16" bind:value={jsonDraft} spellcheck="false"></textarea>
    {:else}
      <div class="toolbar">
        <input class="input" style="flex:1" placeholder="Name, e.g. Tag utility bills" bind:value={editing.name} />
        <label class="wiz-check" style="margin:0"><input type="checkbox" bind:checked={editing.enabled} /> Enabled</label>
      </div>

      <div class="side-head" style="padding-left:0">When</div>
      {#each editing.triggers as t, i (i)}
        <div class="brow">
          <select class="input" bind:value={t.type}>
            {#each TRIGGER_TYPES as tt}<option value={tt.code}>{tt.label}</option>{/each}
          </select>
          <input class="input" placeholder="filename matches (glob)" bind:value={t.filter_filename} />
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
            <button class="btn sm" onclick={() => editing.triggers.splice(i, 1)} title="Remove"><Icon name="x" size={12} /></button>
          {/if}
        </div>
      {/each}
      <button class="btn sm" style="margin:8px 0 4px" onclick={addTrigger}><Icon name="plus" size={12} /> Trigger</button>

      <div class="side-head" style="padding-left:0">Then</div>
      {#each editing.actions as a, i (i)}
        <div class="brow">
          <select class="input" value={a.type} onchange={(e) => { a.type = e.target.value; actionKindChanged(a) }}>
            {#each ACTION_KINDS as k}<option value={k.kind}>{k.label}</option>{/each}
          </select>
          {#if a.type === 'assign_title'}
            <input class="input" style="flex:1;max-width:none" placeholder={'title template, e.g. {correspondent} {created_year}'} bind:value={a.params.template} />
          {:else if a.type === 'assign_tags'}
            <select class="input" multiple size="3" style="max-width:220px"
                    onchange={(e) => (a.params.tag_ids = [...e.target.selectedOptions].map(o => Number(o.value)))}>
              {#each facets.tags as x}<option value={x.id} selected={a.params.tag_ids?.includes(x.id)}>{x.name}</option>{/each}
            </select>
          {:else if a.type === 'assign_correspondent'}
            <select class="input" bind:value={a.params.correspondent_id}>
              <option value={0}>choose…</option>
              {#each facets.correspondents as x}<option value={x.id}>{x.name}</option>{/each}
            </select>
          {:else if a.type === 'assign_document_type'}
            <select class="input" bind:value={a.params.document_type_id}>
              <option value={0}>choose…</option>
              {#each facets.types as x}<option value={x.id}>{x.name}</option>{/each}
            </select>
          {:else if a.type === 'assign_custom_field'}
            <input class="input" style="max-width:110px" type="number" placeholder="field id" bind:value={a.params.field_id} />
            <input class="input" placeholder="value" bind:value={a.params.value} />
          {:else}
            <input class="input" style="max-width:130px" type="number" placeholder="id" bind:value={a.params[Object.keys(a.params)[0]]} />
          {/if}
          {#if editing.actions.length > 1}
            <button class="btn sm" onclick={() => editing.actions.splice(i, 1)} title="Remove"><Icon name="x" size={12} /></button>
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

{#if loading}
  <div class="index">{#each Array(3) as _}<div class="irow"><div class="skel" style="width:50%"></div></div>{/each}</div>
{:else if items.length === 0 && editing === null}
  <div class="empty"><Icon name="zap" size={56} /><b>No automations yet.</b><span>Create one to tag, title, and route documents as they arrive.</span></div>
{:else}
  <div class="index">
    {#each items as a (a.id)}
      <div class="irow">
        <span class="dot" class:ok={a.enabled}></span>
        <span class="grow">
          <span class="title" style="display:block">{a.name || `Automation #${a.id}`}</span>
          <span class="sub">{summary(a)}</span>
        </span>
        <button class="btn sm" onclick={() => toggle(a)}>{a.enabled ? 'Disable' : 'Enable'}</button>
        <button class="btn sm" onclick={() => openEditor(a)}>Edit</button>
        <button class="btn sm danger" onclick={() => remove(a)}><Icon name="trash" size={13} /></button>
      </div>
    {/each}
  </div>
{/if}
