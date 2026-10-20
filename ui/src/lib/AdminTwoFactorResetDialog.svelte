<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { onDestroy, onMount, tick, untrack } from 'svelte'
  import { t as translate } from './i18n.svelte.js'
  import { adminResetTwoFactor } from './api.js'
  import { twoFactorErrorMessage } from './twoFactorErrors.js'
  import { session } from './session.svelte.js'
  import Icon from './Icon.svelte'

  let { user, onClose, onReset } = $props()
  let currentPassword = $state('')
  let twoFactorCode = $state('')
  let error = $state('')
  let busy = $state(false)
  let dialog = $state()
  let passwordInput = $state()
  let disposed = false
  const account = untrack(() => session.user)

  function clearSecrets() {
    currentPassword = ''
    twoFactorCode = ''
  }

  function close() {
    if (busy) return
    clearSecrets()
    error = ''
    onClose?.()
  }

  function onKey(event) {
    event.stopPropagation()
    if (event.key === 'Escape') {
      event.preventDefault()
      close()
      return
    }
    if (event.key !== 'Tab' || !dialog) return
    const focusable = [...dialog.querySelectorAll('button:not([disabled]), input:not([disabled])')]
    if (!focusable.length) return
    const first = focusable[0]
    const last = focusable.at(-1)
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault()
      last.focus()
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault()
      first.focus()
    }
  }

  async function submit(event) {
    event.preventDefault()
    if (busy || !currentPassword) return
    busy = true
    error = ''
    try {
      await adminResetTwoFactor(user.id, currentPassword, twoFactorCode.trim())
      if (disposed || session.user !== account) return
      clearSecrets()
      busy = false
      await onReset?.(user)
    } catch (exception) {
      if (disposed || session.user !== account) return
      clearSecrets()
      error = exception?.code === 'two_factor_required'
        ? translate('adminTwoFactorResetDialog.yourTwoFactorCodeIsRequired')
        : twoFactorErrorMessage(exception)
          || (exception?.code === 'invalid_credentials' ? translate('twoFactorErrors.passwordOrCodeDidNotMatch') : '')
          || exception?.message || translate('adminTwoFactorResetDialog.couldNotReset')
      busy = false
      await tick()
      passwordInput?.focus()
    }
  }

  $effect(() => {
    if (session.user !== account) {
      clearSecrets()
      if (!disposed) onClose?.()
    }
  })

  onMount(async () => {
    await tick()
    passwordInput?.focus()
  })

  onDestroy(() => {
    disposed = true
    clearSecrets()
  })
</script>

<div class="modal-veil" role="presentation" onclick={close}>
  <div class="modal reset-modal" bind:this={dialog} role="dialog" aria-modal="true"
       aria-labelledby="admin-two-factor-reset-title" tabindex="-1"
       onclick={(event) => event.stopPropagation()} onkeydown={onKey}>
    <div class="modal-head">
      <h3 id="admin-two-factor-reset-title">{translate('adminTwoFactorResetDialog.resetTwoFactor')}</h3>
      <button class="btn sm" type="button" disabled={busy} onclick={close}
              aria-label={translate('adminTwoFactorResetDialog.closeResetDialog')}>
        <Icon name="x" size={13} />
      </button>
    </div>
    <p>{translate('adminTwoFactorResetDialog.resetFor', { email: user.email })}</p>
    <p class="sub">{translate('adminTwoFactorResetDialog.sessionsEndTokensRemain')}</p>
    <form onsubmit={submit}>
      <label class="field" for="admin-reset-password">{translate('adminTwoFactorResetDialog.yourCurrentPassword')}
        <input id="admin-reset-password" class="input" type="password" autocomplete="current-password"
               bind:this={passwordInput} bind:value={currentPassword} required disabled={busy} />
      </label>
      <label class="field" for="admin-reset-factor">{translate('adminTwoFactorResetDialog.yourAuthenticatorOrRecoveryCode')}</label>
      <input id="admin-reset-factor" class="input" type="text" autocomplete="one-time-code"
             autocapitalize="characters" spellcheck="false" bind:value={twoFactorCode} disabled={busy} />
      <span class="sub optional">{translate('adminTwoFactorResetDialog.onlyIfEnabled')}</span>
      {#if error}<div class="err" role="alert">{error}</div>{/if}
      <div class="form-actions">
        <button class="btn" type="button" disabled={busy} onclick={close}>{translate('common.cancel')}</button>
        <button class="btn danger" disabled={busy || !currentPassword}>
          {busy ? translate('common.working') : translate('adminTwoFactorResetDialog.resetTwoFactor')}
        </button>
      </div>
    </form>
  </div>
</div>

<style>
  .reset-modal { width:min(540px,94vw);max-height:94vh;overflow:auto }
  .reset-modal > p { margin:0 0 8px;overflow-wrap:anywhere }
  .reset-modal .field { margin-top:14px }
  .optional { display:block;margin-top:4px }
  .form-actions { display:flex;justify-content:flex-end;gap:8px;margin-top:18px }
  @media (max-width:520px) {
    .reset-modal { width:94vw }
    .form-actions { flex-direction:column-reverse }
    .form-actions .btn { width:100%;justify-content:center }
  }
</style>
