<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "../lib/i18n.svelte.js";
  import { scopedHash as filingHref } from '../lib/systems.svelte.js'
  import { emptyTrash, listTrash, permanentlyDeleteDocument, restoreDocument } from '../lib/api.js'
  import { fmtDate, fmtBytes, formatDate, formatLabel, formatNumber, plural } from '../lib/format.js'
  import ConfirmDialog from '../lib/ConfirmDialog.svelte'
  import Icon from '../lib/Icon.svelte'
  import RuleWorkspace from '../lib/RuleWorkspace.svelte'

  let { notify } = $props()
  let rows = $state([])
  let total = $state(0)
  let loading = $state(true)
  let err = $state('')
  let deleteRequest = $state(null)
  let deleteBusy = $state(false)
  let restoringID = $state(null)

  const DAY_SECONDS = 24 * 60 * 60

  function filingLabel(document) {
    return [document.jd_address || document.jd_category_code, document.jd_category_name].filter(Boolean).join(' ')
  }

  function rowMetadata(document) {
    const parts = [
      formatLabel(document.mime_type) || translate("trash.unknownFormat"),
      document.original_size != null ? fmtBytes(document.original_size) : '',
      translate("trash.trashedValue", { date: fmtDate(document.trashed_at) }),
    ].filter(Boolean)
    // The address chip leads the row, so the line names only the category.
    if (document.jd_category_name) parts.push(translate("trash.wasFiledUnderValue", { category: document.jd_category_name }))
    // A no-break space keeps each separator at the end of a wrapped line.
    return parts.join('\u00a0· ')
  }

  function deletionStatus(document) {
    const secondsRemaining = Number(document.deletes_at || 0) - Math.floor(Date.now() / 1000)
    if (secondsRemaining <= 0) {
      return { label: translate("trash.deletionDue"), urgent: true }
    }
    const daysRemaining = Math.ceil(secondsRemaining / DAY_SECONDS)
    const deadline = new Date(document.deletes_at * 1000)
    const options = deadline.getFullYear() === new Date().getFullYear()
      ? { month: 'short', day: 'numeric' }
      : { year: 'numeric', month: 'short', day: 'numeric' }
    return {
      label: plural("trash.deletesInDayCount", daysRemaining),
      date: translate("trash.deletesOnValue", { date: formatDate(deadline, options) }),
      urgent: daysRemaining <= 7,
    }
  }

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

<div class="trash-page">
  <RuleWorkspace
    class="trash-workspace"
    title={translate("app.trash")}
    description={translate("trash.documentsArePermanentlyDeleted30DaysAfter")}
    headingId="trash-workspace-heading">
    {#snippet actions()}
      <button class="btn sm empty-trash"
              disabled={loading || total === 0}
              title={total === 0 ? translate("trash.trashIsEmpty2") : plural("trash.permanentlyDeleteDocumentCount", total)}
              onclick={() => (deleteRequest = { kind: 'all' })}>
        <Icon name="trash" size={13} />
        {translate("trash.emptyTrash")}
        <span class="trash-count">{formatNumber(total)}</span>
      </button>
    {/snippet}

    {#if err}<div class="err trash-error">{err}</div>{/if}
    {#if loading}
      <div class="trash-list" aria-label={translate("common.loading")}>
        {#each Array(3) as _}
          <div class="trash-row trash-row-loading"><div class="skel" style="width:55%"></div></div>
        {/each}
      </div>
    {:else if rows.length === 0}
      <div class="trash-empty" role="status">
        <Icon name="trash" size={42} />
        <b>{translate("trash.trashIsEmpty")}</b>
      </div>
    {:else}
      <div class="trash-list">
        {#each rows as d (d.id)}
          {@const deadline = deletionStatus(d)}
          <article class="trash-row">
            <a class="trash-document filing-row" href={filingHref(`#/doc/${d.id}`)}
               aria-label={d.title || translate("trash.documentValue", { "id": d.id })}>
              <div class="trash-info">
                <span class="filing-code filing-code-column"
                      title={filingLabel(d) ? translate("trash.wasFiledUnderValue", { category: filingLabel(d) }) : undefined}>{d.jd_address || d.jd_category_code || ''}</span>
                <div class="trash-title-line">
                  <span class="title trash-title">{d.title || translate("trash.documentValue", { "id": d.id })}</span>
                  {#if d.archive_serial_number != null}
                    <span class="chip">{translate("common.asnValue", { value: String(d.archive_serial_number) })}</span>
                  {/if}
                </div>
                <div class="trash-meta" title={d.mime_type}>{rowMetadata(d)}</div>
              </div>
            </a>
            <div class="trash-outcome">
              <span class="deletion-chip" class:urgent={deadline.urgent} title={deadline.date}>{deadline.label}</span>
              <div class="trash-actions">
                {#if d.deletes_at > Math.floor(Date.now() / 1000)}
                  <button class="btn sm restore-action" disabled={restoringID !== null} onclick={() => restore(d)}>
                    {restoringID === d.id ? translate("trash.restoring") : translate("trash.restore")}
                  </button>
                {/if}
                <button class="btn sm" onclick={() => (deleteRequest = { kind: 'document', document: d })}>
                  {translate("trash.deletePermanently")}
                </button>
              </div>
            </div>
          </article>
        {/each}
      </div>
    {/if}
  </RuleWorkspace>
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
  .trash-page { width:min(100%,1400px);margin:0 auto }
  .trash-error { margin:14px 20px 0 }
  .empty-trash { gap:7px }
  .trash-count {
    display:inline-grid;place-items:center;min-width:21px;height:21px;padding:0 6px;
    border:1px solid var(--line);border-radius:999px;background:var(--surface-2);
    color:var(--muted);font-size:.72rem;font-variant-numeric:tabular-nums;
  }
  .trash-list { width:100% }
  .trash-row {
    display:grid;grid-template-columns:minmax(0,1fr) auto;align-items:center;gap:24px;
    position:relative;min-height:72px;padding:12px 20px;border-bottom:1px solid var(--line);
  }
  .trash-row:last-child { border-bottom:0 }
  .trash-row:not(.trash-row-loading):hover { background:var(--tint) }
  .trash-row-loading { display:flex;align-items:center }
  .trash-document { display:block;min-width:0;color:inherit;text-decoration:none }
  .trash-document:focus-visible { border-radius:5px;outline:2px solid var(--accent);outline-offset:4px }
  /* The link covers the whole row, like Documents rows; recovery controls stay above it. */
  .trash-document::after { content:'';position:absolute;inset:0 }
  /* Same order as Documents and Search rows: address chip, then title; the meta line sits under the title. */
  .trash-info { position:relative;z-index:1;display:grid;grid-template-columns:auto minmax(0,1fr);align-items:baseline;gap:5px 12px;min-width:0 }
  .trash-meta { grid-column:2;color:var(--muted);font-size:.8rem;white-space:normal;overflow-wrap:anywhere }
  .trash-title-line { display:flex;align-items:center;gap:8px;min-width:0;flex-wrap:wrap }
  .trash-title { min-width:0;color:var(--ink);font-weight:550;overflow-wrap:anywhere }
  .trash-outcome { position:relative;z-index:1;display:flex;align-items:center;justify-content:flex-end;gap:16px }
  .deletion-chip { white-space:nowrap;color:var(--muted);font-size:.76rem }
  /* Only a deadline within a week earns a pill. */
  .deletion-chip.urgent {
    display:inline-flex;align-items:center;min-height:25px;padding:2px 10px;font-weight:600;
    border:1px solid color-mix(in srgb,var(--warn) 28%,var(--line));border-radius:999px;
    background:var(--warn-soft);color:var(--warn);
  }
  .trash-actions { display:flex;align-items:center;gap:8px;flex-wrap:wrap }
  /* Restore is a standard button with accent text; its hover is the shared .btn hover, independent of the row tint. */
  .restore-action { color:var(--accent) }
  .trash-empty {
    display:grid;place-items:center;gap:8px;min-height:240px;padding:36px 20px;
    color:var(--muted);text-align:center;
  }
  .trash-empty b { color:var(--ink);font-size:1rem }
  @media (max-width: 800px) {
    .trash-row { grid-template-columns:minmax(0,1fr);gap:14px;padding:16px 20px }
    .trash-outcome { justify-content:space-between;padding-top:14px;border-top:1px solid var(--line) }
    .trash-actions .btn { min-height:40px }
  }
  @media (max-width: 520px) {
    .trash-outcome { align-items:stretch;flex-direction:column }
    .deletion-chip { align-self:flex-start }
    .trash-actions { display:grid;grid-template-columns:1fr 1fr }
    .trash-actions .btn { width:100% }
  }
</style>
