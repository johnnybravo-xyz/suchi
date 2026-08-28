<script>
  import { listAutomations, createAutomation, patchAutomation, deleteAutomation,
           listTags, listCorrespondents, listDocumentTypes,
           automationsSchema } from '../lib/api.js'
  import Icon from '../lib/Icon.svelte'

  let { notify, readOnly = false, jdCategories = [] } = $props()
  let items = $state([])
  let loading = $state(true)
	let err = $state('')
	let editing = $state(null)        // null | working copy (id present = edit)
  let mode = $state('builder')      // 'builder' | 'json'
  let jsonDraft = $state('')
  let draftErr = $state('')
  let facets = $state({ tags: [], correspondents: [], types: [] })
  let peekID = $state(null)
  let builtInsOpen = $state(false)
  let toggleID = $state(null)

  const userItems = $derived(items.filter(a => !a.preset_slug))
  const builtInItems = $derived(items.filter(a => a.preset_slug))

  let TRIGGER_TYPES = $state([])
  let ACTION_KINDS = $state([])
  automationsSchema().then(sc => {
    if (sc?.triggers?.length) TRIGGER_TYPES = sc.triggers.map(t => ({ code: t.code, label: t.name || t.type }))
    if (sc?.actions?.length) ACTION_KINDS = sc.actions.map(a => ({
      kind: a.kind, label: a.name || a.kind, params: (a.params || []).map(p => p.name),
    }))
  }).catch(ex => { err = ex.message || 'Could not load the automation schema.' })

  const blank = () => ({
    name: '', enabled: true, order: items.length,
    triggers: [{ type: 2, filter_filename: '', filter_path: '', filter_title_matching: '', filter_content_matching: '',
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
      facets = {
        tags: t?.results || [],
        correspondents: c?.results || [],
        types: d?.results || [],
      }
    } catch {}
  }

	function openEditor(a) {
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

  function addTrigger() { editing.triggers.push({ type: 2, filter_filename: '', filter_path: '', filter_title_matching: '', filter_content_matching: '', filter_has_tag: 0, filter_has_correspondent: 0, filter_has_document_type: 0 }) }
  function addAction() { editing.actions.push({ type: 'assign_tags', params: { tag_ids: [] } }) }
  function actionKindChanged(a) {
    const spec = ACTION_KINDS.find(k => k.kind === a.type)
    a.params = Object.fromEntries((spec?.params || []).map(p => [p, p === 'tag_ids' ? [] : p === 'template' || p === 'value' ? '' : 0]))
	}

  async function save() {
    draftErr = ''
    let body
    if (mode === 'json') {
	  try { body = JSON.parse(jsonDraft) }
      catch (ex) { draftErr = ex.message || 'Not valid JSON.'; return }
    } else {
	  try { body = editing }
      catch (ex) { draftErr = ex.message; return }
    }
    if (!body.name?.trim()) { draftErr = 'Give it a name.'; return }
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
      items = [...items]
      notify?.(enabled ? 'Enabled' : 'Disabled')
    } catch (ex) { notify?.(ex.message || 'Could not update the automation') }
    finally { toggleID = null }
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

  function actionDescribe(a) {
    const spec = ACTION_KINDS.find(k => k.kind === a.type)
    const p = a.params || {}
    switch (a.type) {
      case 'assign_tags': {
        const names = (p.tag_ids || []).map(id => facets.tags.find(t => t.id === id)?.name).filter(Boolean)
        return names.length ? `Add tags: ${names.join(', ')}` : 'Add tags'
      }
      case 'assign_correspondent': {
        const name = facets.correspondents.find(c => c.id === p.correspondent_id)?.name
        return name ? `Set correspondent → ${name}` : 'Set correspondent'
      }
      case 'assign_document_type': {
        const name = facets.types.find(t => t.id === p.document_type_id)?.name
        return name ? `Set document type → ${name}` : 'Set document type'
      }
      case 'assign_jd_category': {
        const jd = jdCategories.find(x => x.id === p.jd_category_id)
        return jd ? `Assign JD category → ${jd.code} · ${jd.name}` : 'Assign JD category'
      }
      case 'assign_title':
        return p.template ? `Set title → “${p.template}”` : 'Set title'
      case 'assign_owner':
        return p.owner_id ? `Set owner → user #${p.owner_id}` : 'Set owner'
      case 'assign_storage_path':
        return p.storage_path_id ? `Set storage path → #${p.storage_path_id}` : 'Set storage path'
      case 'assign_custom_field':
        return `Set custom field #${p.field_id ?? '?'} → ${p.value ?? ''}`
      default:
        return spec?.label || a.type
    }
  }

  load(); loadFacets()
</script>

<div class="toolbar">
  <span class="sub" style="color:var(--muted)">When a trigger fires and its filters match, the actions run in order.</span>
  <span class="spacer"></span>
  {#if !readOnly}<button class="btn primary sm" onclick={() => openEditor(null)}><Icon name="plus" size={13} /> New automation</button>{/if}
</div>

{#if err}<div class="err">{err}</div>{/if}

{#if editing !== null}
  <div class="card" style="margin-bottom:16px">
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
          <select class="input" bind:value={t.type}>
            {#each TRIGGER_TYPES as tt}<option value={tt.code}>{tt.label}</option>{/each}
          </select>
          <input class="input" placeholder="filename matches (glob)" bind:value={t.filter_filename} />
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
            <input class="input" style="flex:1;max-width:none" placeholder={'title template, e.g. {{correspondent}} {{date}}'} bind:value={a.params.template} />
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
          {:else if a.type === 'assign_jd_category'}
            <select class="input" bind:value={a.params.jd_category_id}>
              <option value={0}>choose…</option>
              {#each jdCategories as x}<option value={x.id}>{x.code} · {x.name}</option>{/each}
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
        <button class="btn sm danger" onclick={() => remove(a)}><Icon name="trash" size={13} /></button>
      {/if}
    {/if}
    {#if readOnly && peekID === a.id}
      <pre class="spec">{JSON.stringify(a, null, 2)}</pre>
    {/if}
  </div>
{/snippet}

{#if loading}
  <div class="index">{#each Array(3) as _}<div class="irow"><div class="skel" style="width:50%"></div></div>{/each}</div>
{:else if items.length === 0 && editing === null}
  <div class="empty"><Icon name="zap" size={56} /><b>No automations yet.</b><span>Create one to tag, title, and route documents as they arrive.</span></div>
{:else}
  {#if userItems.length}
    <div class="index">
      {#each userItems as a (a.id)}{@render automationRow(a)}{/each}
    </div>
  {/if}
  {#if builtInItems.length}
    <section class="builtins" class:only={userItems.length === 0}>
      <button class="builtins-toggle" onclick={() => (builtInsOpen = !builtInsOpen)} aria-expanded={builtInsOpen}>
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
{/if}

<style>
  .builtins { margin-top: 14px; }
  .builtins.only { margin-top: 0; }
  .builtins-toggle {
    display: flex; align-items: center; gap: 10px; width: 100%; min-height: 48px;
    padding: 8px 12px; border: 1px solid var(--line); border-radius: var(--r);
    background: var(--surface); color: var(--ink); text-align: left;
  }
  .builtins-toggle:hover { border-color: var(--line-strong); background: var(--tint); }
  .builtins-toggle .grow { display: flex; flex-direction: column; gap: 1px; }
  .builtins-toggle .sub { color: var(--muted); font-size: .76rem; }
  .builtins-toggle .chev { display: flex; transition: transform .15s; }
  .builtins-toggle .chev.open { transform: rotate(90deg); }
  .builtins-list { margin-top: 8px; }
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
  .automation-switch { margin-top: 1px; }
  @media (max-width: 600px) {
    .automation-row { flex-wrap: wrap; gap: 8px; }
    .automation-row > .grow { flex-basis: 100%; }
  }
</style>
