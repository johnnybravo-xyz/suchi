<script>
  import { listTrash, restoreDocument } from '../lib/api.js'
  import { fmtDate, fmtBytes } from '../lib/format.js'
  import Icon from '../lib/Icon.svelte'

  let { notify } = $props()
  let rows = $state([])
  let loading = $state(true)
  let err = $state('')

  async function load() {
    loading = true; err = ''
    try {
      const r = await listTrash()
      rows = r?.results || r || []
    } catch (ex) { err = ex.message || 'Could not load the trash.' }
    finally { loading = false }
  }

  async function restore(d) {
    try {
      await restoreDocument(d.id)
      rows = rows.filter(x => x.id !== d.id)
      notify?.('Restored')
    } catch (ex) { notify?.(ex.message || 'Could not restore') }
  }

  load()
</script>

<div class="content-narrow">
  <p class="sub" style="color:var(--muted);margin:0 0 12px;font-size:.86rem">
    Trashed documents wait here until the retention window purges them. Restore puts one back exactly where it was filed.
  </p>
  {#if err}<div class="err">{err}</div>{/if}
  {#if loading}
    <div class="index">{#each Array(3) as _}<div class="irow"><div class="skel" style="width:55%"></div></div>{/each}</div>
  {:else if rows.length === 0}
    <div class="empty"><Icon name="trash" size={50} /><b>Trash is empty.</b></div>
  {:else}
    <div class="index">
      {#each rows as d (d.id)}
        <div class="irow">
          <span class="dot"></span>
          <span class="grow">
            <span class="title" style="display:block">{d.title || `Document #${d.id}`}</span>
            <span class="sub">{d.mime_type || ''} {d.original_size ? '· ' + fmtBytes(d.original_size) : ''} · trashed {fmtDate(d.trashed_at)}</span>
          </span>
          <button class="btn sm" onclick={() => restore(d)}><Icon name="left" size={13} /> Restore</button>
        </div>
      {/each}
    </div>
  {/if}
</div>
