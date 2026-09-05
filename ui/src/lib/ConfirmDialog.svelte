<script>
  import Icon from './Icon.svelte'

  let { title, message, confirmLabel = 'Confirm', busyLabel = 'Moving…', busy = false, onConfirm, onCancel } = $props()
  let dialog
  let cancelButton

  function cancel() {
    if (busy) return
    dialog.close()
    onCancel?.()
  }

  $effect(() => {
    dialog.showModal()
    cancelButton.focus()
    return () => dialog.close()
  })
</script>

<dialog bind:this={dialog} class="modal" style="width:min(430px,94vw)"
        oncancel={(event) => { event.preventDefault(); cancel() }}
        role="alertdialog" aria-labelledby="confirm-dialog-title">
    <div class="modal-head">
      <h3 id="confirm-dialog-title">{title}</h3>
      <button class="btn sm" disabled={busy} onclick={cancel} title="Close" aria-label="Close confirmation"><Icon name="x" size={13} /></button>
    </div>
    <p class="sub" style="margin:0;color:var(--muted)">{message}</p>
    <div class="toolbar" style="margin:16px 0 0">
      <button class="btn sm danger" disabled={busy} onclick={() => onConfirm?.()}>
        {busy ? busyLabel : confirmLabel}
      </button>
      <button class="btn sm" disabled={busy} onclick={cancel} bind:this={cancelButton}>Cancel</button>
    </div>
</dialog>

<style>
  dialog { margin: auto; color: var(--ink); }
  dialog::backdrop { background: rgba(0, 0, 0, .45); }
</style>
