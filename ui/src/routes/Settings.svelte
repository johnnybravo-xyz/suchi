<script>
  import { listTokens, createToken, deleteToken } from '../lib/api.js'
  import { session } from '../lib/session.svelte.js'
  import { fmtDate } from '../lib/format.js'
  import Icon from '../lib/Icon.svelte'

  let { notify } = $props()
  let tokens = $state([])
  let err = $state('')
  let newName = $state('')
  let minted = $state('')   // freshly created secret, shown once

  async function load() {
    try {
      const res = await listTokens()
      tokens = res?.results || res || []
    } catch (ex) { err = ex.message || 'Could not load tokens.' }
  }

  async function mint(e) {
    e.preventDefault()
    try {
      const res = await createToken({ name: newName.trim() || 'api token' })
      minted = res?.token || ''
      newName = ''
      notify?.('Token created — copy it now, it is shown once')
      load()
    } catch (ex) { notify?.(ex.message || 'Could not create a token') }
  }

  async function revoke(t) {
    if (!confirm(`Revoke “${t.name}”? Anything using it stops working immediately.`)) return
    try { await deleteToken(t.id); tokens = tokens.filter(x => x.id !== t.id); notify?.('Token revoked') }
    catch (ex) { notify?.(ex.message || 'Could not revoke') }
  }

  load()
</script>

<div class="content-narrow" style="display:flex;flex-direction:column;gap:16px">
  <div class="card">
    <h3>Account</h3>
    <dl class="kv">
      <dt>Signed in as</dt><dd>{session.user?.email}</dd>
      <dt>Role</dt><dd><span class="pill">{session.user?.role}</span></dd>
      <dt>Auth</dt><dd>{session.user?.authn_by}</dd>
    </dl>
  </div>

  <div class="card">
    <h3>API tokens</h3>
    <p class="sub" style="color:var(--muted);margin:0 0 12px;font-size:.84rem">
      For mobile apps, agents, and <code>suchi mcp</code>. Sent as <code>Authorization: Token …</code>
    </p>
    {#if err}<div class="err">{err}</div>{/if}
    {#if minted}
      <div class="field">
        <label for="minted">New token — copy it now, it is not shown again</label>
        <input id="minted" class="input mono" style="font-size:.76rem" readonly value={minted}
               onclick={(e) => { e.target.select(); navigator.clipboard?.writeText(minted) }} />
      </div>
    {/if}
    <form class="toolbar" onsubmit={mint} style="margin-bottom:12px">
      <input class="input" style="flex:1" placeholder="Token name, e.g. phone, bravo-agent" bind:value={newName} />
      <button class="btn primary sm"><Icon name="plus" size={13} /> Create token</button>
    </form>
    {#if tokens.length}
      <div class="index">
        {#each tokens as t (t.id)}
          <div class="irow">
            <span class="dot"></span>
            <span class="title grow">{t.name}</span>
            <span class="sub">{t.created_at ? fmtDate(t.created_at) : ''}</span>
            <button class="btn sm danger" onclick={() => revoke(t)}>Revoke</button>
          </div>
        {/each}
      </div>
    {/if}
  </div>

  <div class="card">
    <h3>About this interface</h3>
    <p class="sub" style="color:var(--muted);font-size:.84rem;margin:0">
      Svelte SPA, zero runtime dependencies, talking to the same API the mobile apps use.
      Theme follows your system; the toggle in the sidebar overrides it.
    </p>
  </div>
</div>
