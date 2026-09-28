<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { onDestroy, onMount, tick, untrack } from 'svelte'
  import { changeMyEmail } from './api.js'
  import { session } from './session.svelte.js'
  import Icon from './Icon.svelte'

  let { currentEmail = '', onClose, onChanged } = $props()
  let newEmail = $state('')
  let confirmEmail = $state('')
  let currentPassword = $state('')
  let error = $state('')
  let busy = $state(false)
  let dialog
  let newEmailInput
  let confirmEmailInput
  let passwordInput
  let disposed = false
  const account = untrack(() => session.user)

  function clearPassword() {
    currentPassword = ''
  }

  function clearForm() {
    newEmail = ''
    confirmEmail = ''
    clearPassword()
    error = ''
  }

  function close() {
    if (busy) return
    clearForm()
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
    if (busy) return
    error = ''
    if (!newEmailInput?.validity.valid || !confirmEmailInput?.validity.valid) {
      const invalidInput = !newEmailInput?.validity.valid ? newEmailInput : confirmEmailInput
      clearPassword()
      error = 'Enter a valid email address in both fields.'
      await tick()
      invalidInput?.focus()
      return
    }
    if (newEmail !== confirmEmail) {
      clearPassword()
      error = 'The email addresses must match exactly.'
      await tick()
      newEmailInput?.focus()
      return
    }

    busy = true
    const targetEmail = newEmail.trim()
    try {
      await changeMyEmail(targetEmail, currentPassword)
    } catch (exception) {
      clearPassword()
      error = exception?.message || 'Could not change the sign-in email.'
      busy = false
      await tick()
      passwordInput?.focus()
      return
    }

    clearPassword()
    busy = false
    await onChanged?.(targetEmail)
  }

  $effect(() => {
    const current = session.user
    if (current !== account) {
      clearForm()
      if (!disposed) onClose?.()
    }
  })

  onMount(async () => {
    await tick()
    newEmailInput?.focus()
  })

  onDestroy(() => {
    disposed = true
    clearForm()
  })
</script>

<div class="modal-veil" onclick={close} role="presentation">
  <div class="modal email-change-modal" bind:this={dialog} onclick={(event) => event.stopPropagation()}
       onkeydown={onKey} role="dialog" aria-modal="true" aria-labelledby="email-change-title"
       aria-describedby="email-change-impact" tabindex="-1">
    <div class="modal-head">
      <h3 id="email-change-title">Change sign-in email</h3>
      <button class="btn sm" type="button" disabled={busy} onclick={close}
              title="Close" aria-label="Close email change"><Icon name="x" size={13} /></button>
    </div>

    <p class="sub current-email">Current sign-in email <strong>{currentEmail}</strong></p>
    <div id="email-change-impact" class="email-change-impact">
      <p>Changing this updates your sign-in identifier immediately. Suchi does not send a confirmation or recovery email.</p>
      <ul>
        <li>Your account, documents, and permissions stay the same.</li>
        <li>Every other browser session will be signed out. This browser stays signed in with a rotated session.</li>
      </ul>
    </div>

    <form onsubmit={submit} novalidate>
      <div class="email-fields">
        <label class="field" for="new-sign-in-email">New email
          <input id="new-sign-in-email" class="input" bind:this={newEmailInput} bind:value={newEmail}
                 type="email" autocomplete="username" required disabled={busy} />
        </label>
        <label class="field" for="confirm-sign-in-email">Confirm new email
          <input id="confirm-sign-in-email" class="input" bind:this={confirmEmailInput}
                 bind:value={confirmEmail} type="email" autocomplete="off" required disabled={busy} />
        </label>
        <label class="field full" for="current-account-password">Current password
          <input id="current-account-password" class="input" bind:this={passwordInput}
                 bind:value={currentPassword} type="password" autocomplete="current-password"
                 required disabled={busy} />
        </label>
      </div>
      {#if error}<div class="err email-change-error" role="alert">{error}</div>{/if}
      <div class="form-actions">
        <button class="btn" type="button" disabled={busy} onclick={close}>Cancel</button>
        <button class="btn primary" disabled={busy || !newEmail || !confirmEmail || !currentPassword}>
          {busy ? 'Changing…' : 'Change sign-in email'}
        </button>
      </div>
    </form>
  </div>
</div>

<style>
  .email-change-modal { width:min(620px,94vw);max-height:min(760px,94vh);overflow:auto }
  .current-email { margin:0 0 14px;overflow-wrap:anywhere }
  .current-email strong { color:var(--ink) }
  .email-change-impact { margin:0 0 18px;padding:12px 14px;border:1px solid var(--line);border-radius:6px;background:var(--surface-2);font-size:.84rem;line-height:1.5 }
  .email-change-impact p { margin:0 0 8px }
  .email-change-impact ul { margin:0;padding-inline-start:20px }
  .email-fields { display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1fr);gap:12px }
  .email-fields .field { margin:0;min-width:0 }
  .email-fields .full { grid-column:1 / -1 }
  .email-change-error { margin-top:12px }
  .form-actions { display:flex;justify-content:flex-end;gap:8px;margin-top:18px }
  @media (max-width: 520px) {
    .email-change-modal { width:94vw }
    .email-fields { grid-template-columns:minmax(0,1fr) }
    .email-fields .full { grid-column:auto }
    .form-actions { flex-direction:column-reverse }
    .form-actions .btn { width:100%;justify-content:center }
  }
</style>
