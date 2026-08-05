<script>
  import { login, setToken } from '../lib/api.js'
  import { session, refreshSession } from '../lib/session.svelte.js'

  let { onSignedIn } = $props()
  let email = $state('')
  let password = $state('')
  let err = $state('')
  let busy = $state(false)

  async function submit(e) {
    e.preventDefault()
    err = ''; busy = true
    try {
      const res = await login(email.trim(), password)
      if (res?.token) setToken(res.token)
      await refreshSession()
      if (!session.user) throw new Error('Sign-in did not stick — check the server log.')
      onSignedIn?.()
    } catch (ex) {
      err = ex.status === 401 ? 'Email or password did not match.' : (ex.message || 'Could not sign in.')
    } finally { busy = false }
  }
</script>

<div class="login-wrap">
  <form class="card login-card" onsubmit={submit}>
    <div class="brand">
      <svg viewBox="0 0 64 64" width="30" height="30" aria-hidden="true"><rect x="8" y="8" width="48" height="48" rx="8" fill="var(--manila)" stroke="currentColor" stroke-width="3.5"/><circle cx="17.5" cy="19" r="2.2" fill="currentColor"/><line x1="23" y1="19" x2="48" y2="19" stroke="currentColor" stroke-width="3.5" stroke-linecap="round"/><circle cx="17.5" cy="28" r="2.2" fill="currentColor"/><line x1="23" y1="28" x2="48" y2="28" stroke="currentColor" stroke-width="3.5" stroke-linecap="round"/><circle cx="16.8" cy="37.25" r="3.2" fill="var(--accent)"/><rect x="22.5" y="33.5" width="29.5" height="7.5" rx="3.75" fill="var(--accent)"/><circle cx="17.5" cy="46" r="2.2" fill="currentColor"/><line x1="23" y1="46" x2="48" y2="46" stroke="currentColor" stroke-width="3.5" stroke-linecap="round"/></svg>
      <b style="font-size:1.2rem">suchi</b>
    </div>
    {#if err}<div class="err">{err}</div>{/if}
    <div class="field">
      <label for="email">Email</label>
      <input id="email" class="input" type="email" bind:value={email} autocomplete="username" required />
    </div>
    <div class="field">
      <label for="pw">Password</label>
      <input id="pw" class="input" type="password" bind:value={password} autocomplete="current-password" required />
    </div>
    <button class="btn primary" style="width:100%;justify-content:center" disabled={busy}>
      {busy ? 'Signing in…' : 'Sign in'}
    </button>
  </form>
</div>
