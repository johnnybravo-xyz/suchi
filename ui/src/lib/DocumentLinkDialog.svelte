<script>
  import Icon from './Icon.svelte'
  import LinkQR from './LinkQR.svelte'
  import { copyText } from './clipboard.js'

  let { id, onClose } = $props()
  let dialog
  let copied = $state('')
  const url = $derived(new URL(`/app/#/doc/${id}`, location.origin).href)
  const loopback = ['localhost', '127.0.0.1', '[::1]'].includes(location.hostname)

  $effect(() => {
    dialog.showModal()
    return () => dialog.close()
  })

  async function copy() {
    copied = await copyText(url) ? 'Link copied' : 'Select the link and copy it manually.'
  }
</script>

<dialog bind:this={dialog} class="modal" aria-labelledby="document-link-title" onclose={onClose}>
  <div class="modal-head">
    <h3 id="document-link-title">Open on my phone</h3>
    <button class="btn sm" onclick={() => dialog.close()} aria-label="Close document link"><Icon name="x" size={13} /></button>
  </div>
  <p class="sub">Scan with your phone's camera. Sign in with an account that can read this document. This does not create a public share link.</p>
  {#if loopback}
    <p class="sub">This address only works on this computer. Open Suchi using its network address before scanning on your phone.</p>
  {:else}
    <p class="sub">Your phone needs access to this server's network address.</p>
  {/if}
  <div class="toolbar">
    <input class="input mono" style="flex:1;min-width:0" aria-label="Document link" readonly value={url} onclick={(event) => event.currentTarget.select()} />
    <button class="btn sm" onclick={copy}>Copy link</button>
  </div>
  <p class="sub" role="status">{copied}</p>
  <LinkQR {url} open />
</dialog>

<style>
  dialog { margin: auto; width: min(460px, 94vw); color: var(--ink); }
  dialog::backdrop { background: rgba(0, 0, 0, .45); }
  p { color: var(--muted); }
</style>
