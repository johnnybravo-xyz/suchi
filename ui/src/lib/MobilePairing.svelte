<script>
  import { onMount, onDestroy } from 'svelte'
  import { createMobilePairing, cancelMobilePairing } from './api.js'
  import { copyText } from './clipboard.js'
  import { session } from './session.svelte.js'
  import Icon from './Icon.svelte'

  let { onClose, notify } = $props()
  let dialog
  let deviceName = $state('Suchi mobile')
  let pairing = $state(null)
  let busy = $state(false)
  let error = $state('')
  let copyMessage = $state('')
  let now = $state(Date.now())
  const user = session.user
  let disposed = false
  const remaining = $derived(Math.max(0, Math.ceil((pairing?.expires_at || 0) - now / 1000)))

  async function cancelCode(code) {
    if (!code || session.user !== user) return
    try { await cancelMobilePairing(code) }
    catch {
      if (session.user === user) notify?.('Could not cancel the pairing code. It will expire within five minutes.')
    }
  }

  async function generate(event) {
    event.preventDefault()
    if (busy || !deviceName.trim()) return
    busy = true
    error = ''
    copyMessage = ''
    try {
      const result = await createMobilePairing(deviceName.trim())
      if (disposed || session.user !== user) {
        await cancelCode(result?.code)
        return
      }
      pairing = result
      now = Date.now()
    } catch (ex) {
      if (!disposed && session.user === user) error = ex.message || 'Could not create a pairing code. Try again.'
    } finally { busy = false }
  }

  async function copyLink() {
    const link = pairing?.pairing_url
    if (!link || !remaining) return
    const copied = await copyText(link)
    if (disposed || session.user !== user || pairing?.pairing_url !== link) return
    copyMessage = copied ? 'Pairing link copied.' : 'Select the pairing link below and copy it manually.'
  }

  onMount(() => {
    dialog.showModal()
    const timer = setInterval(() => now = Date.now(), 1000)
    return () => clearInterval(timer)
  })
  onDestroy(() => {
    disposed = true
    void cancelCode(pairing?.code)
    dialog?.close()
  })
</script>

<dialog bind:this={dialog} class="modal pairing-dialog" aria-labelledby="pairing-title"
        oncancel={(event) => { event.preventDefault(); onClose?.() }}>
  <div class="modal-head">
    <h3 id="pairing-title">Pair mobile app</h3>
    <button class="btn sm" onclick={() => onClose?.()} aria-label="Close mobile pairing"><Icon name="x" size={13} /></button>
  </div>
  <p>In the Suchi app, choose <strong>Scan QR code</strong>, then confirm this archive’s address on your phone.</p>
  <form onsubmit={generate}>
    <div class="field">
      <label for="pairing-device">Device name</label>
      <input id="pairing-device" class="input" bind:value={deviceName} maxlength="64" required disabled={busy} />
    </div>
    <button class="btn primary" disabled={busy || !deviceName.trim()}>
      {busy ? 'Creating…' : pairing ? 'Generate new code' : 'Generate QR code'}
    </button>
  </form>
  {#if error}<p class="err" role="alert">{error}</p>{/if}
  {#if pairing && remaining > 0}
    <div class="pairing-code" aria-busy={busy}>
      <img src={pairing.qr_data_url} alt="Scan this QR code in the Suchi app to pair with this archive" width="320" height="320" />
      <p class="expiry">Expires in {Math.floor(remaining / 60)}:{String(remaining % 60).padStart(2, '0')}. Usable once.</p>
    </div>
    <div class="field">
      <label for="pairing-link">Can’t scan? Paste this pairing link in the app.</label>
      <input id="pairing-link" class="input mono" readonly value={pairing.pairing_url} onclick={(event) => event.currentTarget.select()} />
      <button class="btn sm" type="button" disabled={busy} onclick={copyLink}>Copy pairing link</button>
      {#if copyMessage}<p class="copy-message" role="status">{copyMessage}</p>{/if}
    </div>
  {:else if pairing}
    <p role="status">This code has expired. Generate a new code to pair another device.</p>
  {/if}
  <p class="privacy">Anyone with this code can connect as you with document read and write access. Keep it private. A new code replaces the previous one; closing this prompt cancels an unused code.</p>
  <p class="fallback">You can also enter the server address and sign in manually in the app. Use an API token below if your server uses single sign-on.</p>
  <button class="btn" onclick={() => onClose?.()}>Done</button>
</dialog>

<style>
  .pairing-dialog { width:min(480px,94vw);margin:auto;color:var(--ink);max-height:90dvh;overflow:auto }
  dialog::backdrop { background:rgba(0,0,0,.45) }
  p { font-size:.86rem;line-height:1.5 }
  form { display:flex;gap:12px;align-items:end;flex-wrap:wrap }
  form .field { flex:1;min-width:160px;margin:0 }
  form button { min-height:37px }
  .pairing-code { text-align:center;margin:18px 0 }
  .pairing-code img { display:block;width:min(100%,320px);height:auto;margin:auto;background:white;border-radius:8px }
  .expiry { color:var(--muted);margin:8px 0 }
  .field button { align-self:flex-start }
  .mono { font-size:.76rem;min-width:0 }
  .privacy, .fallback { color:var(--muted);font-size:.8rem }
  .copy-message { margin:0 }
</style>
