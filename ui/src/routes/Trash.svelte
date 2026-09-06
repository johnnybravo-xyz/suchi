<script>
  import { emptyTrash, listTrash, permanentlyDeleteDocument, restoreDocument } from '../lib/api.js'
  import { fmtDate, fmtBytes } from '../lib/format.js'
  import ConfirmDialog from '../lib/ConfirmDialog.svelte'
  import Icon from '../lib/Icon.svelte'

  let { notify } = $props()
  let rows = $state([])
  let total = $state(0)
  let loading = $state(true)
  let err = $state('')
  let deleteRequest = $state(null)
  let deleteBusy = $state(false)
  let restoringID = $state(null)

  async function load() {
    loading = true
    err = ''
    try {
      const response = await listTrash()
      rows = response?.results || response || []
      total = response?.count ?? rows.length
    } catch (ex) {
      err = ex.message || 'Could not load the trash.'
    } finally {
      loading = false
    }
  }

  async function restore(d) {
    restoringID = d.id
    try {
      await restoreDocument(d.id)
      rows = rows.filter(x => x.id !== d.id)
      total = Math.max(0, total - 1)
      notify?.('Restored')
    } catch (ex) {
      notify?.(ex.message || 'Could not restore')
    } finally {
      restoringID = null
    }
  }

  async function confirmDelete() {
    const request = deleteRequest
    if (!request) return
    deleteBusy = true
    try {
      if (request.kind === 'document') {
        await permanentlyDeleteDocument(request.document.id)
        rows = rows.filter(x => x.id !== request.document.id)
        total = Math.max(0, total - 1)
        notify?.('Permanently deleted')
      } else {
        const response = await emptyTrash()
        const purged = response?.purged ?? total
        rows = []
        total = Math.max(0, total - purged)
        if (total > 0) await load()
        notify?.(`Permanently deleted ${purged} document${purged === 1 ? '' : 's'}`)
      }
      deleteRequest = null
    } catch (ex) {
      notify?.(ex.message || 'Could not permanently delete')
    } finally {
      deleteBusy = false
    }
  }

  load()
</script>

<div class="content-narrow">
  <div class="toolbar" style="align-items:flex-start;margin:0 0 12px">
    <p class="sub grow" style="color:var(--muted);margin:0;font-size:.86rem">
      Documents are permanently deleted 30 days after being moved to Trash. Restore puts one back where it was filed.
    </p>
    <button class="btn sm danger"
            disabled={loading || total === 0}
            title={total === 0 ? 'Trash is empty' : `Permanently delete all ${total} documents in Trash`}
            onclick={() => (deleteRequest = { kind: 'all' })}>
      <Icon name="trash" size={13} /> Empty trash
      {#if total > 0}<span class="pill">{total}</span>{/if}
    </button>
  </div>
  {#if err}<div class="err">{err}</div>{/if}
  {#if loading}
    <div class="index">{#each Array(3) as _}<div class="irow"><div class="skel" style="width:55%"></div></div>{/each}</div>
  {:else if rows.length === 0}
    <div class="empty"><Icon name="trash" size={50} /><b>Trash is empty.</b></div>
  {:else}
    <div class="index">
      {#each rows as d (d.id)}
        <div class="irow trash-row">
          <a class="trash-document" href={`#/doc/${d.id}`}>
            <span class="file-icon"><Icon name="docs" size={23} /></span>
            <span class="trash-info">
              <span class="title">{d.title || `Document #${d.id}`}</span>
              <span class="sub">{d.mime_type || 'Unknown format'}{d.original_size ? ' · ' + fmtBytes(d.original_size) : ''}</span>
              <span class="sub">Trashed {fmtDate(d.trashed_at)} · Deletes permanently {fmtDate(d.deletes_at)}</span>
            </span>
            <Icon name="chev" size={15} />
          </a>
          <div class="trash-actions">
            {#if d.deletes_at > Math.floor(Date.now() / 1000)}
              <button class="btn sm" disabled={restoringID !== null} onclick={() => restore(d)}>
                <Icon name="refresh" size={13} /> {restoringID === d.id ? 'Restoring…' : 'Restore'}
              </button>
            {/if}
            <button class="btn sm danger" onclick={() => (deleteRequest = { kind: 'document', document: d })}>
              <Icon name="trash" size={13} /> Delete permanently
            </button>
          </div>
        </div>
      {/each}
    </div>
  {/if}
</div>

{#if deleteRequest}
  <ConfirmDialog
    title={deleteRequest.kind === 'document' ? 'Delete permanently?' : 'Empty Trash?'}
    message={deleteRequest.kind === 'document'
      ? `“${deleteRequest.document.title || `Document #${deleteRequest.document.id}`}” will be permanently deleted, and any share link containing it will be revoked. This cannot be undone.`
      : `All ${total} document${total === 1 ? '' : 's'} you can see in Trash will be permanently deleted. This cannot be undone.`}
    confirmLabel={deleteRequest.kind === 'document' ? 'Delete permanently' : 'Empty trash'}
    busyLabel="Deleting…"
    busy={deleteBusy}
    onConfirm={confirmDelete}
    onCancel={() => (deleteRequest = null)} />
{/if}

<style>
  .trash-row { display: grid; grid-template-columns: minmax(0, 1fr) auto; gap: 16px; cursor: default; }
  .trash-document { display: flex; align-items: center; gap: 12px; min-width: 0; text-decoration: none; }
  .file-icon { display: grid; place-items: center; width: 36px; height: 44px; flex: none; color: var(--muted); background: var(--surface-2); border-radius: var(--r-sm); }
  .trash-info { display: grid; gap: 3px; min-width: 0; flex: 1; }
  .trash-row .title, .trash-row .sub { white-space: normal; overflow-wrap: anywhere; }
  .trash-document:hover .title { color: var(--accent); }
  .trash-actions { display: flex; gap: 8px; flex-wrap: wrap; }
  .trash-actions .btn { min-height: 40px; }
  @media (max-width: 700px) {
    .trash-row { grid-template-columns: minmax(0, 1fr); gap: 12px; padding: 16px; }
    .trash-document { align-items: flex-start; }
    .trash-actions { padding-top: 12px; border-top: 1px solid var(--line); }
  }
</style>
