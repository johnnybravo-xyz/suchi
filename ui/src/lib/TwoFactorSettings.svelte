<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { onDestroy, tick, untrack } from 'svelte'
  import { t as translate } from './i18n.svelte.js'
  import {
    disableTwoFactor,
    enableTwoFactor,
    getTwoFactorStatus,
    regenerateTwoFactorRecoveryCodes,
    startTwoFactorEnrollment,
  } from './api.js'
  import { session } from './session.svelte.js'
  import Icon from './Icon.svelte'
  import LinkQR from './LinkQR.svelte'
  import { twoFactorErrorMessage } from './twoFactorErrors.js'

  let { notify } = $props()
  let status = $state({ enabled: false, recovery_codes_remaining: 0 })
  let loading = $state(true)
  let statusError = $state('')
  let dialog = $state('')
  let dialogError = $state('')
  let busy = $state(false)
  let currentPassword = $state('')
  let factorCode = $state('')
  let enrollmentSecret = $state('')
  let provisioningURI = $state('')
  let enrollmentToken = $state('')
  let recoveryCodes = $state([])
  let activeTokens = $state(0)
  let acknowledged = $state(false)
  let qrOpen = $state(false)
  let dialogElement = $state()
  let firstInput = $state()
  let trigger
  let statusGeneration = 0
  let accountGeneration = 0
  let disposed = false

  function clearSensitive() {
    currentPassword = ''
    factorCode = ''
    enrollmentSecret = ''
    provisioningURI = ''
    enrollmentToken = ''
    recoveryCodes = []
    activeTokens = 0
    acknowledged = false
    qrOpen = false
  }

  async function loadStatus(userID = session.user?.user_id) {
    if (!userID || disposed) return
    const current = ++statusGeneration
    loading = true
    statusError = ''
    try {
      const result = await getTwoFactorStatus()
      if (disposed || current !== statusGeneration || session.user?.user_id !== userID) return
      status = {
        enabled: !!result?.enabled,
        recovery_codes_remaining: Number(result?.recovery_codes_remaining || 0),
      }
    } catch (error) {
      if (!disposed && current === statusGeneration && session.user?.user_id === userID) {
        statusError = error?.message || translate('twoFactorSettings.couldNotLoad')
      }
    } finally {
      if (!disposed && current === statusGeneration && session.user?.user_id === userID) loading = false
    }
  }

  async function openDialog(kind, event) {
    clearSensitive()
    dialogError = ''
    dialog = kind
    trigger = event?.currentTarget
    await tick()
    firstInput?.focus()
  }

  async function closeDialog() {
    if (busy || (dialog === 'codes' && !acknowledged)) return
    const returnFocus = trigger
    clearSensitive()
    dialogError = ''
    dialog = ''
    trigger = undefined
    await tick()
    returnFocus?.focus()
  }

  function onKey(event) {
    event.stopPropagation()
    if (event.key === 'Escape') {
      event.preventDefault()
      void closeDialog()
      return
    }
    if (event.key !== 'Tab' || !dialogElement) return
    const focusable = [...dialogElement.querySelectorAll('button:not([disabled]), input:not([disabled])')]
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

  async function fail(error, close = false) {
    const message = twoFactorErrorMessage(error)
      || (error?.code === 'invalid_credentials' ? translate('twoFactorErrors.passwordOrCodeDidNotMatch') : '')
      || error?.message || translate('twoFactorSettings.requestFailed')
    clearSensitive()
    busy = false
    if (close) {
      statusError = message
      dialog = ''
      const returnFocus = trigger
      trigger = undefined
      await tick()
      returnFocus?.focus()
    } else {
      dialogError = message
      await tick()
      firstInput?.focus()
    }
  }

  function accountChanged(operationGeneration) {
    return disposed || operationGeneration !== accountGeneration
  }

  async function startEnrollment(event) {
    event.preventDefault()
    if (busy || !currentPassword) return
    const operationGeneration = accountGeneration
    busy = true
    dialogError = ''
    try {
      const result = await startTwoFactorEnrollment(currentPassword)
      if (accountChanged(operationGeneration)) return
      currentPassword = ''
      enrollmentSecret = result.secret
      provisioningURI = result.provisioning_uri
      enrollmentToken = result.enrollment_token
      factorCode = ''
      dialog = 'verify'
      busy = false
      await tick()
      firstInput?.focus()
    } catch (error) {
      if (!accountChanged(operationGeneration)) await fail(error)
    }
  }

  async function confirmEnrollment(event) {
    event.preventDefault()
    if (busy || !enrollmentToken || !factorCode.trim()) return
    const operationGeneration = accountGeneration
    busy = true
    dialogError = ''
    try {
      const result = await enableTwoFactor(enrollmentToken, factorCode.trim())
      if (accountChanged(operationGeneration)) return
      clearSensitive()
      recoveryCodes = result?.recovery_codes || []
      activeTokens = Number(result?.active_api_tokens || 0)
      dialog = 'codes'
      busy = false
      status = { enabled: true, recovery_codes_remaining: recoveryCodes.length }
      void loadStatus()
      notify?.(translate('twoFactorSettings.enabledNotice'))
      await tick()
      firstInput?.focus()
    } catch (error) {
      if (!accountChanged(operationGeneration)) await fail(error, true)
    }
  }

  async function regenerateCodes(event) {
    event.preventDefault()
    if (busy || !currentPassword || !factorCode.trim()) return
    const operationGeneration = accountGeneration
    busy = true
    dialogError = ''
    try {
      const result = await regenerateTwoFactorRecoveryCodes(currentPassword, factorCode.trim())
      if (accountChanged(operationGeneration)) return
      clearSensitive()
      recoveryCodes = result?.recovery_codes || []
      dialog = 'codes'
      busy = false
      status = { enabled: true, recovery_codes_remaining: recoveryCodes.length }
      void loadStatus()
      notify?.(translate('twoFactorSettings.recoveryCodesRegeneratedNotice'))
      await tick()
      firstInput?.focus()
    } catch (error) {
      if (!accountChanged(operationGeneration)) await fail(error)
    }
  }

  async function disable(event) {
    event.preventDefault()
    if (busy || !currentPassword || !factorCode.trim()) return
    const operationGeneration = accountGeneration
    busy = true
    dialogError = ''
    try {
      await disableTwoFactor(currentPassword, factorCode.trim())
      if (accountChanged(operationGeneration)) return
      clearSensitive()
      dialog = ''
      busy = false
      status = { enabled: false, recovery_codes_remaining: 0 }
      void loadStatus()
      notify?.(translate('twoFactorSettings.disabledNotice'))
      const returnFocus = trigger
      trigger = undefined
      await tick()
      returnFocus?.focus()
    } catch (error) {
      if (!accountChanged(operationGeneration)) await fail(error)
    }
  }

  async function copyText(value, notice) {
    try {
      await navigator.clipboard.writeText(value)
      notify?.(notice)
    } catch {
      dialogError = translate('twoFactorSettings.couldNotCopy')
    }
  }

  function downloadCodes() {
    const blob = new Blob([`${recoveryCodes.join('\n')}\n`], { type: 'text/plain;charset=utf-8' })
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = 'suchi-recovery-codes.txt'
    link.click()
    URL.revokeObjectURL(url)
  }

  $effect(() => {
    const userID = session.user?.user_id
    accountGeneration++
    statusGeneration++
    clearSensitive()
    dialog = ''
    dialogError = ''
    busy = false
    trigger = undefined
    status = { enabled: false, recovery_codes_remaining: 0 }
    loading = !!userID
    if (userID) untrack(() => void loadStatus(userID))
  })

  onDestroy(() => {
    accountGeneration++
    statusGeneration++
    clearSensitive()
  })
</script>

<section class="settings-section" aria-labelledby="security-heading">
  <div class="section-heading">
    <div>
      <h2 id="security-heading">{translate('twoFactorSettings.security')}</h2>
      <p>{translate('twoFactorSettings.addAnAuthenticator')}</p>
    </div>
    {#if status.enabled}<span class="pill enabled">{translate('twoFactorSettings.enabled')}</span>{/if}
  </div>
  <div class="security-body">
    {#if statusError}
      <div class="err" role="alert">{statusError}</div>
      <button class="btn sm" type="button" onclick={() => void loadStatus()}>{translate('common.retry')}</button>
    {:else if loading}
      <p class="sub" role="status">{translate('twoFactorSettings.loading')}</p>
    {:else if status.enabled}
      <div>
        <strong>{translate('twoFactorSettings.twoFactorIsOn')}</strong>
        <p class="sub">{translate('twoFactorSettings.recoveryCodesRemaining', { count: status.recovery_codes_remaining })}</p>
        {#if status.recovery_codes_remaining <= 2}
          <p class="warn" role="status">
            {status.recovery_codes_remaining === 0 ? translate('twoFactorSettings.recoveryCodesNoneLeft')
              : status.recovery_codes_remaining === 1 ? translate('twoFactorSettings.recoveryCodesOneLeft')
                : translate('twoFactorSettings.recoveryCodesFewLeft', { count: status.recovery_codes_remaining })}
          </p>
        {/if}
      </div>
      <div class="security-actions">
        <button class="btn sm" type="button" onclick={(event) => openDialog('regenerate', event)}>{translate('twoFactorSettings.regenerateRecoveryCodes')}</button>
        <button class="btn sm danger" type="button" onclick={(event) => openDialog('disable', event)}>{translate('twoFactorSettings.disable')}</button>
      </div>
    {:else}
      <div>
        <strong>{translate('twoFactorSettings.twoFactorIsOff')}</strong>
        <p class="sub">{translate('twoFactorSettings.requirePasswordAndCode')}</p>
      </div>
      <button class="btn primary sm" type="button" onclick={(event) => openDialog('enroll', event)}>{translate('twoFactorSettings.setUpAuthenticator')}</button>
    {/if}
  </div>
</section>

{#if dialog}
  <div class="modal-veil" role="presentation" onclick={() => void closeDialog()}>
    <div class="modal security-modal" bind:this={dialogElement} role="dialog" aria-modal="true"
         aria-labelledby="two-factor-dialog-title" tabindex="-1" onclick={(event) => event.stopPropagation()}
         onkeydown={onKey}>
      <div class="modal-head">
        <h3 id="two-factor-dialog-title">
          {dialog === 'enroll' ? translate('twoFactorSettings.setUpAuthenticator')
            : dialog === 'verify' ? translate('twoFactorSettings.connectAuthenticator')
              : dialog === 'regenerate' ? translate('twoFactorSettings.regenerateRecoveryCodes')
                : dialog === 'disable' ? translate('twoFactorSettings.disableTwoFactor')
                  : translate('twoFactorSettings.saveRecoveryCodes')}
        </h3>
        <button class="btn sm" type="button" disabled={busy || (dialog === 'codes' && !acknowledged)}
                onclick={() => void closeDialog()} aria-label={translate('twoFactorSettings.closeSecurityDialog')}>
          <Icon name="x" size={13} />
        </button>
      </div>

      {#if dialog === 'enroll'}
        <p class="sub">{translate('twoFactorSettings.confirmPasswordToBegin')}</p>
        <form onsubmit={startEnrollment}>
          <label class="field" for="two-factor-password">{translate('twoFactorSettings.currentPassword')}
            <input id="two-factor-password" class="input" type="password" autocomplete="current-password"
                   bind:this={firstInput} bind:value={currentPassword} required disabled={busy} />
          </label>
          {#if dialogError}<div class="err" role="alert">{dialogError}</div>{/if}
          <div class="form-actions">
            <button class="btn" type="button" disabled={busy} onclick={() => void closeDialog()}>{translate('common.cancel')}</button>
            <button class="btn primary" disabled={busy || !currentPassword}>{busy ? translate('common.working') : translate('common.continue')}</button>
          </div>
        </form>
      {:else if dialog === 'verify'}
        <p class="sub">{translate('twoFactorSettings.scanQrOrEnterSecret')}</p>
        <LinkQR url={provisioningURI} allowedProtocols={['otpauth:']} bind:open={qrOpen}
                label={translate('twoFactorSettings.showAuthenticatorQrCode')} />
        <div class="field">
          <label for="two-factor-secret">{translate('twoFactorSettings.manualSetupSecret')}</label>
          <div class="copy-row">
            <input id="two-factor-secret" class="input mono" value={enrollmentSecret} readonly />
            <button class="btn sm" type="button" onclick={() => copyText(enrollmentSecret, translate('twoFactorSettings.secretCopied'))}>{translate('common.copy')}</button>
          </div>
        </div>
        <form onsubmit={confirmEnrollment}>
          <label class="field" for="two-factor-auth-code">{translate('twoFactorSettings.authenticatorCode')}
            <input id="two-factor-auth-code" class="input" type="text" inputmode="numeric" autocomplete="one-time-code"
                   bind:this={firstInput} bind:value={factorCode} required disabled={busy} />
          </label>
          {#if dialogError}<div class="err" role="alert">{dialogError}</div>{/if}
          <div class="form-actions">
            <button class="btn" type="button" disabled={busy} onclick={() => void closeDialog()}>{translate('common.cancel')}</button>
            <button class="btn primary" disabled={busy || !factorCode.trim()}>{busy ? translate('common.working') : translate('twoFactorSettings.enableTwoFactor')}</button>
          </div>
        </form>
      {:else if dialog === 'regenerate' || dialog === 'disable'}
        <p class="sub">{dialog === 'disable' ? translate('twoFactorSettings.disablingSignsOutOtherBrowsers') : translate('twoFactorSettings.regeneratingInvalidatesOldCodes')}</p>
        <form onsubmit={dialog === 'disable' ? disable : regenerateCodes}>
          <label class="field" for="two-factor-current-password">{translate('twoFactorSettings.currentPassword')}
            <input id="two-factor-current-password" class="input" type="password" autocomplete="current-password"
                   bind:this={firstInput} bind:value={currentPassword} required disabled={busy} />
          </label>
          <label class="field" for="two-factor-current-code">{translate('twoFactorSettings.authenticatorOrRecoveryCode')}
            <input id="two-factor-current-code" class="input" type="text" autocomplete="one-time-code"
                   autocapitalize="characters" spellcheck="false" bind:value={factorCode} required disabled={busy} />
          </label>
          {#if dialogError}<div class="err" role="alert">{dialogError}</div>{/if}
          <div class="form-actions">
            <button class="btn" type="button" disabled={busy} onclick={() => void closeDialog()}>{translate('common.cancel')}</button>
            <button class="btn" class:danger={dialog === 'disable'} class:primary={dialog !== 'disable'}
                    disabled={busy || !currentPassword || !factorCode.trim()}>
              {busy ? translate('common.working') : dialog === 'disable' ? translate('twoFactorSettings.disableTwoFactor') : translate('twoFactorSettings.regenerateRecoveryCodes')}
            </button>
          </div>
        </form>
      {:else}
        <p>{translate('twoFactorSettings.storeCodesSafely')}</p>
        {#if activeTokens > 0}
          <p class="sub token-note">
            {activeTokens === 1 ? translate('twoFactorSettings.activeTokenStillWorks')
              : translate('twoFactorSettings.activeTokensStillWork', { count: activeTokens })}
          </p>
        {/if}
        <div class="recovery-codes" aria-label={translate('twoFactorSettings.recoveryCodes')}>
          {#each recoveryCodes as code}<code>{code}</code>{/each}
        </div>
        {#if dialogError}<div class="err" role="alert">{dialogError}</div>{/if}
        <div class="code-actions">
          <button class="btn sm" type="button" onclick={() => copyText(recoveryCodes.join('\n'), translate('twoFactorSettings.recoveryCodesCopied'))}>{translate('twoFactorSettings.copyAll')}</button>
          <button class="btn sm" type="button" onclick={downloadCodes}>{translate('twoFactorSettings.download')}</button>
        </div>
        <label class="acknowledge">
          <input bind:this={firstInput} type="checkbox" bind:checked={acknowledged} />
          <span>{translate('twoFactorSettings.savedRecoveryCodes')}</span>
        </label>
        <div class="form-actions">
          <button class="btn primary" type="button" disabled={!acknowledged} onclick={() => void closeDialog()}>{translate('common.done')}</button>
        </div>
      {/if}
    </div>
  </div>
{/if}

<style>
  .settings-section { --pad:22px;flex:none;container-type:inline-size;background:var(--surface);border:1px solid var(--line);border-radius:var(--r);overflow:hidden }
  .section-heading { display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:8px 24px;padding:13px var(--pad);border-bottom:1px solid var(--line) }
  .section-heading > div { display:flex;align-items:baseline;flex-wrap:wrap;column-gap:10px;row-gap:2px;min-width:0;flex:1 1 240px }
  .section-heading h2 { margin:0;font-size:.95rem;white-space:nowrap }
  .section-heading p { margin:0;color:var(--muted);font-size:.82rem;line-height:1.45 }
  .pill.enabled { color:var(--ok);border-color:color-mix(in srgb,var(--ok) 40%,var(--line)) }
  .security-body { display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:14px 24px;padding:16px var(--pad) }
  .security-body p { margin:3px 0 0 }
  .security-actions { display:flex;flex-wrap:wrap;gap:8px }
  .security-modal { width:min(590px,94vw);max-height:94vh;overflow:auto }
  .security-modal > .sub { margin:0 0 16px }
  .security-modal .field { margin-top:14px }
  .copy-row { display:flex;gap:8px;align-items:center }
  .copy-row input { min-width:0 }
  .mono { font-family:'Spline Sans Mono',ui-monospace,Menlo,monospace;letter-spacing:.06em }
  .form-actions,.code-actions { display:flex;justify-content:flex-end;gap:8px;margin-top:18px }
  .warn { margin:6px 0 0;color:var(--warn);font-size:.82rem;line-height:1.45 }
  .token-note { margin:8px 0 0 }
  .recovery-codes { display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:7px 18px;margin:14px 0;padding:14px;border:1px solid var(--line);border-radius:var(--r-sm);background:var(--surface-2) }
  .recovery-codes code { font-size:.86rem;text-align:center;letter-spacing:.04em }
  .acknowledge { display:flex;align-items:flex-start;gap:9px;margin-top:18px;font-size:.86rem;line-height:1.4 }
  .acknowledge input { margin-top:2px }
  @container (max-width:480px) {
    .settings-section { --pad:16px }
    .security-actions,.security-actions .btn { width:100% }
    .security-actions .btn { justify-content:center }
  }
  @media (max-width:520px) {
    .security-modal { width:94vw }
    .form-actions { flex-direction:column-reverse }
    .form-actions .btn,.code-actions .btn { width:100%;justify-content:center }
    .code-actions { flex-direction:column }
    .recovery-codes { grid-template-columns:minmax(0,1fr) }
  }
</style>
