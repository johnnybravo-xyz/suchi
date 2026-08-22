<script>
  import { login, setDemoAnonToken, setToken } from '../lib/api.js'
  import { session, refreshSession } from '../lib/session.svelte.js'
  import BrandMark from '../lib/BrandMark.svelte'

  let { onSignedIn } = $props()
  let email = $state('')
  let password = $state('')
  let err = $state('')
  let busy = $state(false)

  async function submit(e) {
    e.preventDefault()
    err = ''; busy = true
    try {
      // A revoked token would make auth middleware reject the public login
      // request before it reaches the password handler.
      setToken(null)
      setDemoAnonToken(null)
      await login(email.trim(), password)
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
      <BrandMark />
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
