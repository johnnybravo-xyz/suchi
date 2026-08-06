<script>
  import { listPendingDecryption, decryptDocument, decryptBatch } from '../lib/api.js'
  import { fmtDate, fmtBytes } from '../lib/format.js'
  import Icon from '../lib/Icon.svelte'

  let { notify, onChanged } = $props()
  let rows = $state([])
  let loading = $state(true)
  let err = $state('')
  let batchPw = $state('')
  let remember = $state(true)
  let busy = $state(false)
  let perDoc = $state({})   // id -> password draft

  async function load() {
    loading = true; err = ''
    try {
      const r = await listPendingDecryption()
      rows = r?.results || r || []
    } catch (ex) { err = ex.message || 'Could not load the locked list.' }
    finally { loading = false }
  }

  async function unlockOne(d) {
    const password = (perDoc[d.id] || '').trim()
    if (!password) return
    busy = true
    try {
      await decryptDocument(d.id, { password, remember })
      rows = rows.filter(x => x.id !== d.id)
      notify?.(`Unlocked “${d.title || 'document #' + d.id}”`)
      onChanged?.()
    } catch (ex) {
      notify?.(ex.status === 422 || ex.status === 400 ? 'That password did not open it' : ex.message || 'Could not decrypt')
    } finally { busy = false }
  }

  async function unlockAll(e) {
    e.preventDefault()
    const password = batchPw.trim()
    if (!password) return
    busy = true
    try {
      const res = await decryptBatch({ password, remember })
      const opened = res?.decrypted ?? res?.opened ?? 0
      notify?.(opened ? `Unlocked ${opened} document${opened === 1 ? '' : 's'}` : 'That password opened nothing')
      batchPw = ''
      load()
      onChanged?.()
    } catch (ex) { notify?.(ex.message || 'Batch decrypt failed') }
    finally { busy = false }
  }

  load()
</script>

<div class="content-narrow">
  <p class="sub" style="color:var(--muted);margin:0 0 14px;font-size:.88rem">
    Password-protected PDFs land here until a password opens them. Unlocked copies go through the
    normal pipeline; the encrypted original is kept untouched. Remembered passwords are tried
    automatically on future uploads.
  </p>

  {#if err}<div class="err">{err}</div>{/if}

  {#if rows.length > 1}
    <form class="card" style="margin-bottom:14px" onsubmit={unlockAll}>
      <h3>Try one password against all {rows.length}</h3>
      <div class="toolbar" style="margin:10px 0 8px">
        <input class="input" type="password" style="flex:1" placeholder="Password (e.g. bank statements share one)"
               bind:value={batchPw} autocomplete="off" />
        <button class="btn primary sm" disabled={busy || !batchPw.trim()}><Icon name="lock" size={13} /> Unlock all it fits</button>
      </div>
      <label class="wiz-check" style="margin:0"><input type="checkbox" bind:checked={remember} />
        Remember this password for future uploads</label>
    </form>
  {/if}

  {#if loading}
    <div class="index">{#each Array(3) as _}<div class="irow"><div class="skel" style="width:55%"></div></div>{/each}</div>
  {:else if rows.length === 0}
    <div class="empty"><Icon name="lock" size={50} /><b>Nothing is locked.</b><span>Encrypted PDFs will wait here when they arrive.</span></div>
  {:else}
    <div class="index">
      {#each rows as d (d.id)}
        <div class="irow" style="flex-wrap:wrap">
          <span class="dot warn"></span>
          <span class="grow" style="min-width:180px">
            <span class="title" style="display:block">{d.title || `Document #${d.id}`}</span>
            <span class="sub">{d.mime_type || 'application/pdf'} · {fmtBytes(d.original_size)} · {fmtDate(d.created_at)}</span>
          </span>
          <input class="input" type="password" style="max-width:200px;padding:5px 10px;font-size:.82rem"
                 placeholder="password" autocomplete="off"
                 bind:value={perDoc[d.id]}
                 onkeydown={(e) => e.key === 'Enter' && unlockOne(d)} />
          <button class="btn sm" disabled={busy || !(perDoc[d.id] || '').trim()} onclick={() => unlockOne(d)}>Unlock</button>
        </div>
      {/each}
    </div>
  {/if}
</div>
