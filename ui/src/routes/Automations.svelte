<script>
  import { listAutomations, createAutomation, patchAutomation, deleteAutomation } from '../lib/api.js'
  import Icon from '../lib/Icon.svelte'

  let { notify } = $props()
  let items = $state([])
  let loading = $state(true)
  let err = $state('')
  let editing = $state(null)      // null | {} (new) | automation
  let draft = $state('')
  let draftErr = $state('')

  const TEMPLATE = JSON.stringify({
    name: 'Tag utility bills',
    trigger: 'document_added',
    enabled: true,
    conditions: [{ kind: 'content_regex', value: 'electricity|water bill' }],
    actions: [{ kind: 'assign_tags', value: ['utilities'] }],
  }, null, 2)

  async function load() {
    loading = true; err = ''
    try {
      const res = await listAutomations()
      items = res?.results || res || []
    } catch (ex) { err = ex.message || 'Could not load automations.' }
    finally { loading = false }
  }

  function openEditor(a) {
    editing = a || {}
    draft = a ? JSON.stringify(a, null, 2) : TEMPLATE
    draftErr = ''
  }

  async function saveDraft() {
    let body
    try { body = JSON.parse(draft) } catch { draftErr = 'Not valid JSON.'; return }
    try {
      if (editing?.id) await patchAutomation(editing.id, body)
      else await createAutomation(body)
      editing = null
      notify?.('Automation saved')
      load()
    } catch (ex) { draftErr = ex.message || 'The server rejected this spec.' }
  }

  async function toggle(a) {
    try {
      await patchAutomation(a.id, { enabled: !a.enabled })
      a.enabled = !a.enabled
      notify?.(a.enabled ? 'Enabled' : 'Disabled')
    } catch (ex) { notify?.(ex.message || 'Could not toggle') }
  }

  async function remove(a) {
    if (!confirm(`Delete automation “${a.name}”?`)) return
    try { await deleteAutomation(a.id); items = items.filter(x => x.id !== a.id); notify?.('Deleted') }
    catch (ex) { notify?.(ex.message || 'Could not delete') }
  }

  load()
</script>

<div class="toolbar">
  <span class="sub" style="color:var(--muted)">Trigger → conditions → actions. Runs on ingest and edits.</span>
  <span class="spacer"></span>
  <button class="btn primary sm" onclick={() => openEditor(null)}><Icon name="plus" size={13} /> New automation</button>
</div>

{#if err}<div class="err">{err}</div>{/if}

{#if editing !== null}
  <div class="card" style="margin-bottom:16px">
    <h3>{editing.id ? `Edit “${editing.name}”` : 'New automation'}</h3>
    {#if draftErr}<div class="err">{draftErr}</div>{/if}
    <textarea class="input" rows="14" bind:value={draft} spellcheck="false"></textarea>
    <div class="toolbar" style="margin:10px 0 0">
      <button class="btn primary sm" onclick={saveDraft}>Save automation</button>
      <button class="btn sm" onclick={() => (editing = null)}>Cancel</button>
    </div>
  </div>
{/if}

{#if loading}
  <div class="index">{#each Array(3) as _}<div class="irow"><div class="skel" style="width:50%"></div></div>{/each}</div>
{:else if items.length === 0 && editing === null}
  <div class="empty"><Icon name="zap" size={56} /><b>No automations yet.</b><span>Create one to tag, file, and route documents as they arrive.</span></div>
{:else}
  <div class="index">
    {#each items as a (a.id)}
      <div class="irow">
        <span class="dot" class:ok={a.enabled}></span>
        <span class="grow">
          <span class="title" style="display:block">{a.name || `Automation #${a.id}`}</span>
          <span class="sub mono">{a.trigger} · {(a.conditions || []).length} condition{(a.conditions || []).length === 1 ? '' : 's'} · {(a.actions || []).length} action{(a.actions || []).length === 1 ? '' : 's'}</span>
        </span>
        <button class="btn sm" onclick={() => toggle(a)}>{a.enabled ? 'Disable' : 'Enable'}</button>
        <button class="btn sm" onclick={() => openEditor(a)}>Edit</button>
        <button class="btn sm danger" onclick={() => remove(a)}><Icon name="trash" size={13} /></button>
      </div>
    {/each}
  </div>
{/if}
