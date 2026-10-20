<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "../lib/i18n.svelte.js";
  import { onDestroy, onMount, tick } from 'svelte'
  import { login } from '../lib/api.js'
  import { twoFactorErrorMessage } from '../lib/twoFactorErrors.js'
  import { getLoginPath, usesExternalLogin } from '../lib/login.js'
  import { session, refreshSession } from '../lib/session.svelte.js'
  import BrandMark from '../lib/BrandMark.svelte'

  let { onSignedIn } = $props()
  let email = $state('')
  let password = $state('')
  let twoFactorCode = $state('')
  let needsTwoFactor = $state(false)
  let err = $state('')
  let busy = $state(false)
  let twoFactorInput = $state()
  let form = $state()
  const externalLogin = usesExternalLogin()

  onMount(() => {
    if (externalLogin) location.assign(getLoginPath())
  })

  function clearCredentials() {
    password = ''
    twoFactorCode = ''
    needsTwoFactor = false
  }

  function changeEmail() {
    clearCredentials()
    err = ''
  }

  function changePassword() {
    if (needsTwoFactor) {
      twoFactorCode = ''
      needsTwoFactor = false
    }
    err = ''
  }

  async function submit(e) {
    e.preventDefault()
    if (busy) return
    err = ''
    busy = true
    try {
      await login(email.trim(), password, needsTwoFactor ? twoFactorCode.replace(/\s/g, '') : '')
      await refreshSession()
      if (!session.user) throw new Error(translate("login.signInDidNotStickCheckTheServer"))
      clearCredentials()
      onSignedIn?.()
    } catch (ex) {
      if (ex?.code === 'two_factor_required') {
        needsTwoFactor = true
        twoFactorCode = ''
        await tick()
        twoFactorInput?.focus()
      } else {
        err = twoFactorErrorMessage(ex) || (ex?.status === 401
          ? (needsTwoFactor ? translate("login.twoFactorCodeDidNotMatch") : translate("login.emailOrPasswordDidNotMatch"))
          : ex?.message || translate("login.couldNotSignIn"))
        twoFactorCode = ''
      }
    } finally {
      busy = false
    }
  }

  // A full authenticator code submits on its own; recovery codes still use the button.
  function codeInput(event) {
    twoFactorCode = event.currentTarget.value
    if (/^\d{6}$/.test(twoFactorCode.replace(/\s/g, ''))) form?.requestSubmit()
  }

  onDestroy(clearCredentials)
</script>

{#if !externalLogin}
  <div class="login-wrap">
    <form class="card login-card" bind:this={form} onsubmit={submit}>
      <div class="brand">
        <BrandMark />
        <b style="font-size:1.2rem">suchi</b>
      </div>
      {#if err}<div class="err">{err}</div>{/if}
      <div class="field">
        <label for="email">{translate("login.email")}</label>
        <input id="email" class="input" type="email" bind:value={email} oninput={changeEmail} autocomplete="username" required />
      </div>
      <div class="field">
        <label for="pw">{translate("login.password")}</label>
        <input id="pw" class="input" type="password" bind:value={password} oninput={changePassword} autocomplete="current-password" required />
      </div>
      {#if needsTwoFactor}
        <div class="field">
          <label for="login-two-factor">{translate("login.authenticatorOrRecoveryCode")}</label>
          <input id="login-two-factor" class="input" type="text" autocomplete="one-time-code"
                 autocapitalize="characters" spellcheck="false" bind:this={twoFactorInput}
                 bind:value={twoFactorCode} oninput={codeInput} required />
          <span class="two-factor-help">{translate("login.enterTheCodeFromYourAuthenticatorOrARecoveryCode")}</span>
        </div>
      {/if}
      <button class="btn primary" style="width:100%;justify-content:center" disabled={busy}>
        {busy ? translate("login.signingIn") : needsTwoFactor ? translate("login.verifyAndSignIn") : translate("login.signIn")}
      </button>
    </form>
  </div>
{/if}

<style>
  .two-factor-help { display:block;margin-top:4px;color:var(--muted);font-size:.8rem;line-height:1.45 }
</style>
