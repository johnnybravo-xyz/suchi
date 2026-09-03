<script>
  import Icon from './Icon.svelte'

  let { title, message, confirmLabel = 'Confirm', busyLabel = 'Moving…', busy = false, onConfirm, onCancel } = $props()
  let cancelButton

  function cancel() {
    if (!busy) onCancel?.()
  }

  function onKey(e) {
    if (e.key === 'Escape') cancel()
  }

  $effect(() => { cancelButton?.focus() })
</script>

<svelte:window onkeydown={onKey} />

<div class="modal-veil" onclick={cancel} role="presentation">
  <div class="modal" style="width:min(430px,94vw)"
       onclick={(e) => e.stopPropagation()}
       onkeydown={(e) => e.stopPropagation()}
       role="alertdialog" aria-modal="true" aria-labelledby="confirm-dialog-title" tabindex="-1">
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
  </div>
</div>
