<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "../lib/i18n.svelte.js";
  import { scopedHash as filingHref } from '../lib/systems.svelte.js'
  import { emptyTrash, listTrash, permanentlyDeleteDocument, restoreDocument } from '../lib/api.js'
  import { fmtDate, fmtBytes, formatLabel, formatNumber, plural } from '../lib/format.js'
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
      err = ex.message || translate("trash.couldNotLoadTheTrash")
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
      notify?.(translate("trash.restored"))
    } catch (ex) {
      notify?.(ex.message || translate("trash.couldNotRestore"))
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
        notify?.(translate("trash.permanentlyDeleted"))
      } else {
        const response = await emptyTrash()
        const purged = response?.purged ?? total
        rows = []
        total = Math.max(0, total - purged)
        if (total > 0) await load()
        notify?.(plural("trash.permanentlyDeletedDocumentCount", purged))
      }
      deleteRequest = null
    } catch (ex) {
      notify?.(ex.message || translate("trash.couldNotPermanentlyDelete"))
    } finally {
      deleteBusy = false
    }
  }

  load()
</script>

<div class="toolbar trash-toolbar" role="group" aria-label={translate("trash.trashOptions")}>
  <button class="btn sm danger"
          disabled={loading || total === 0}
          title={total === 0 ? translate("trash.trashIsEmpty2") : plural("trash.permanentlyDeleteDocumentCount", total)}
          onclick={() => (deleteRequest = { kind: 'all' })}>
    <Icon name="trash" size={13} /> {translate("trash.emptyTrash")}
    {#if total > 0}<span class="pill">{formatNumber(total)}</span>{/if}
  </button>
</div>
<div class="content-narrow">
  <p class="sub trash-retention">
    {translate("trash.documentsArePermanentlyDeleted30DaysAfter")}
  </p>
  {#if err}<div class="err">{err}</div>{/if}
  {#if loading}
    <div class="index">{#each Array(3) as _}<div class="irow"><div class="skel" style="width:55%"></div></div>{/each}</div>
  {:else if rows.length === 0}
    <div class="empty"><Icon name="trash" size={50} /><b>{translate("trash.trashIsEmpty")}</b></div>
  {:else}
    <div class="index">
      {#each rows as d (d.id)}
        <div class="irow trash-row">
          <a class="trash-document" href={filingHref(`#/doc/${d.id}`)}>
            <span class="file-icon"><Icon name="docs" size={23} /></span>
            <span class="trash-info">
              <span class="title">{d.title || translate("trash.documentValue", { "id": d.id })}</span>
              {#if d.archive_serial_number != null}<span><span class="chip">{translate("common.asnValue", { value: String(d.archive_serial_number) })}</span></span>{/if}
              <span class="sub" title={d.mime_type}>{formatLabel(d.mime_type) || translate("trash.unknownFormat")}{d.original_size ? ' · ' + fmtBytes(d.original_size) : ''}</span>
              <span class="sub">{translate("trash.trashedValueDeletesPermanentlyValue", { "trashed_at": fmtDate(d.trashed_at), "deletes_at": fmtDate(d.deletes_at) })}</span>
            </span>
            <Icon name="chev" size={15} />
          </a>
          <div class="trash-actions">
            {#if d.deletes_at > Math.floor(Date.now() / 1000)}
              <button class="btn sm" disabled={restoringID !== null} onclick={() => restore(d)}>
                <Icon name="refresh" size={13} /> {restoringID === d.id ? translate("trash.restoring") : translate("trash.restore")}
              </button>
            {/if}
            <button class="btn sm danger" onclick={() => (deleteRequest = { kind: 'document', document: d })}>
              <Icon name="trash" size={13} /> {translate("trash.deletePermanently")}
            </button>
          </div>
        </div>
      {/each}
    </div>
  {/if}
</div>

{#if deleteRequest}
  <ConfirmDialog
    title={deleteRequest.kind === 'document' ? translate("trash.deletePermanently2") : translate("trash.emptyTrash2")}
    message={deleteRequest.kind === 'document'
      ? translate("trash.valueWillBePermanentlyDeletedAndAnyShare", { id: deleteRequest.document.title || translate("trash.documentNumber", { id: deleteRequest.document.id }) })
      : plural("trash.emptyTrashDocumentCount", total)}
    confirmLabel={deleteRequest.kind === 'document' ? translate("trash.deletePermanently") : translate("trash.emptyTrash")}
    busyLabel={translate("trash.deleting")}
    busy={deleteBusy}
    onConfirm={confirmDelete}
    onCancel={() => (deleteRequest = null)} />
{/if}

<style>
  .trash-retention { margin:0 0 12px;color:var(--muted);font-size:.86rem }
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
