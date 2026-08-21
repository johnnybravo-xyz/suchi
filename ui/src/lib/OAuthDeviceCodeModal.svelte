<script>
  import { startEmailOAuth, completeEmailOAuth } from './api.js'
  import Icon from './Icon.svelte'

  let { provider, onSuccess, onClose, notify } = $props()

  let flow = $state(null)      // {flow_handle, user_code, verification_url, expires_at, message}
  let err = $state('')
  let expired = $state(false)
  let now = $state(Math.floor(Date.now() / 1000))
  let starting = $state(false)
  let tickHandle = null
  let controller = null

  async function begin() {
    starting = true; err = ''; expired = false
    try {
      const r = await startEmailOAuth(provider)
      flow = r
      now = Math.floor(Date.now() / 1000)
      schedule()
    } catch (ex) {
      err = ex.data?.message || ex.message || 'Could not start sign-in.'
    } finally { starting = false }
  }

  function schedule() {
    stop()
    controller = new AbortController()
    tickHandle = setInterval(() => {
      now = Math.floor(Date.now() / 1000)
      if (flow && now >= flow.expires_at) {
        expired = true
        stop()
      }
    }, 1000)
    complete(controller.signal)
  }

  function stop() {
    if (tickHandle) { clearInterval(tickHandle); tickHandle = null }
    if (controller) { controller.abort(); controller = null }
  }

  async function complete(signal) {
    while (flow && !expired && !signal.aborted) {
      try {
        const r = await completeEmailOAuth(flow.flow_handle, { signal })
        if (signal.aborted || expired) return
        if (r?.ok) {
          stop()
          onSuccess?.({
            username: r.username,
            oauth_account_id: r.oauth_account_id,
            sealed_secret_b64: r.sealed_secret_b64,
          })
          onClose?.(true)
          return
        }
        await pollDelay(signal)
      } catch (ex) {
        if (signal.aborted) return
        err = ex.data?.error || ex.data?.message || ex.message || 'Microsoft sign-in failed.'
        stop()
        return
      }
    }
  }

  function pollDelay(signal) {
    return new Promise((resolve) => {
      if (signal.aborted) {
        resolve()
        return
      }
      let timer
      const finish = () => {
        clearTimeout(timer)
        signal.removeEventListener('abort', finish)
        resolve()
      }
      timer = setTimeout(finish, 1500)
      signal.addEventListener('abort', finish, { once: true })
    })
  }

  function copyCode() {
    if (!flow?.user_code) return
    navigator.clipboard?.writeText(flow.user_code)
    notify?.('Code copied')
  }

  const remaining = $derived(flow ? Math.max(0, flow.expires_at - now) : 0)
  const mm = $derived(String(Math.floor(remaining / 60)).padStart(1, '0'))
  const ss = $derived(String(remaining % 60).padStart(2, '0'))

  $effect(() => { begin(); return stop })

  function onKey(e) { if (e.key === 'Escape') onClose?.(false) }
</script>

<svelte:window onkeydown={onKey} />

<div class="modal-veil" onclick={() => onClose?.(false)} role="presentation">
  <div class="modal" style="width:min(520px,94vw)"
       onclick={(e) => e.stopPropagation()}
       onkeydown={(e) => e.stopPropagation()}
       role="dialog" aria-modal="true" aria-label="Sign in with Microsoft" tabindex="-1">
    <div class="modal-head">
      <h3>Sign in with Microsoft</h3>
      <button class="btn sm" onclick={() => onClose?.(false)}><Icon name="x" size={13} /></button>
    </div>

    {#if err}
      <div class="err">{err}</div>
      <div class="toolbar" style="margin-top:10px">
        <button class="btn sm" onclick={begin} disabled={starting}>Retry</button>
      </div>
    {:else if !flow}
      <p class="sub">Starting sign-in…</p>
    {:else if expired}
      <p class="sub" style="color:var(--danger)">Sign-in expired — close and retry.</p>
      <div class="toolbar" style="margin-top:10px">
        <button class="btn primary sm" onclick={begin} disabled={starting}>Retry</button>
        <button class="btn sm" onclick={() => onClose?.(false)}>Close</button>
      </div>
    {:else}
      <ol class="sub" style="margin:0 0 12px 18px;line-height:1.7;color:var(--muted)">
        <li>Open the link below in a browser.</li>
        <li>Enter the code.</li>
        <li>Sign in. This window closes automatically.</li>
      </ol>
      <div class="field">
        <label for="devc-url">Verification URL</label>
        <a id="devc-url" class="input mono" style="text-decoration:none;color:var(--accent);display:block"
           href={flow.verification_url} target="_blank" rel="noopener noreferrer">
          {flow.verification_url}
        </a>
      </div>
      <div class="field">
        <label for="devc-code">Code</label>
        <div class="toolbar" style="margin:0;gap:8px">
          <span id="devc-code" class="input mono" style="flex:1;font-size:1.15rem;letter-spacing:2px;font-weight:600">
            {flow.user_code}
          </span>
          <button class="btn sm" onclick={copyCode}>Copy</button>
        </div>
      </div>
      <p class="sub" style="margin:4px 0 0;font-size:.82rem;color:var(--muted)">
        Waiting for sign-in… code expires in {mm}m {ss}s.
      </p>
    {/if}
  </div>
</div>
