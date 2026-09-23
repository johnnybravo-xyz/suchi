<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { adminCreateUser } from './api.js'
  import { USER_CAPABILITIES } from './capabilities.js'
  import { systems } from './systems.svelte.js'
  import Icon from './Icon.svelte'

  let { notify, onCreated, onSkip } = $props()
  let user = $state({ email: '', display_name: '', password: '', role: 'member', capabilities: [] })
  let busy = $state(false)
  let error = $state('')

  function toggleCapability(key) {
    user.capabilities = user.capabilities.includes(key)
      ? user.capabilities.filter(cap => cap !== key)
      : [...user.capabilities, key]
  }

  async function create(event) {
    event.preventDefault()
    if (busy) return
    busy = true
    error = ''
    try {
      await adminCreateUser({ ...user, capabilities: user.role === 'admin' ? [] : user.capabilities })
      notify?.(`Created ${user.email}`)
      user = { email: '', display_name: '', password: '', role: 'member', capabilities: [] }
      await onCreated?.()
    } catch (ex) { error = ex.message || 'Could not create the user.' }
    finally { busy = false }
  }
</script>

<form onsubmit={create} aria-label="Create a user">
  <fieldset disabled={busy} class="user-fields">
    <div class="identity-fields">
      <label class="field">Email
        <input class="input" type="email" bind:value={user.email} autocomplete="off" required />
      </label>
      <label class="field">Display name
        <input class="input" bind:value={user.display_name} autocomplete="off" />
      </label>
      <label class="field">Password
        <input class="input" type="password" bind:value={user.password} autocomplete="new-password" required />
      </label>
      <label class="field">Role
        <select class="input" bind:value={user.role}><option value="member">Member</option><option value="admin">Admin</option></select>
      </label>
    </div>
    <p class="role-note">
      {#if user.role === 'admin'}
        Admins manage archive settings and can access all documents.
      {:else}
        Members see their own documents and documents shared with them.
        {#if systems.introduced}They also need access to the document's filing system.{/if}
      {/if}
    </p>
    {#if user.role === 'member'}
      <details class="additional-access">
        <summary>Additional access <span>{user.capabilities.length ? `${user.capabilities.length} selected` : 'Optional'}</span></summary>
        <div class="capability-options">
          {#each USER_CAPABILITIES as cap (cap.key)}
            <label class="capability-option">
              <input type="checkbox" checked={user.capabilities.includes(cap.key)} onchange={() => toggleCapability(cap.key)} />
              <span><b>{cap.label}</b><small>{cap.description}</small></span>
            </label>
          {/each}
        </div>
      </details>
    {/if}
    {#if error}<p class="err" role="alert">{error}</p>{/if}
    <div class="form-actions">
      {#if onSkip}<button type="button" class="btn" onclick={onSkip}>Just me for now</button>{/if}
      <button class="btn primary create-user"><Icon name="plus" size={14} />{busy ? 'Creating…' : 'Create user'}</button>
    </div>
  </fieldset>
</form>

<style>
  .user-fields { border: 0; padding: 0; margin: 0; min-width: 0; }
  .identity-fields { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px; }
  .field { margin: 0; min-width: 0; color: var(--muted); font-size: .78rem; font-weight: 600; }
  .input { min-width: 0; min-height: 42px; font-weight: 400; }
  .role-note { margin: 12px 0 0; color: var(--muted); font-size: .78rem; }
  .additional-access { margin-top: 16px; border-top: 1px solid var(--line); padding-top: 12px; }
  summary { cursor: pointer; font-size: .82rem; font-weight: 600; }
  summary > span { margin-left: 8px; color: var(--muted); font-size: .74rem; font-weight: 400; }
  .capability-options { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 220px), 1fr)); gap: 14px 20px; padding-top: 16px; }
  .capability-option { display: flex; align-items: flex-start; gap: 9px; font-size: .8rem; }
  .capability-option input { margin: 4px 0 0; accent-color: var(--accent); flex: none; }
  .capability-option span { display: flex; flex-direction: column; gap: 3px; }
  .capability-option b { font-weight: 600; }
  .capability-option small { color: var(--muted); line-height: 1.5; }
  .form-actions { display: flex; flex-wrap: wrap; gap: 10px; border-top: 1px solid var(--line); margin-top: 18px; padding-top: 16px; }
  .create-user { margin-left: auto; }
  @media (max-width: 600px) {
    .identity-fields { grid-template-columns: minmax(0, 1fr); }
    .form-actions .btn { flex: 1 1 auto; justify-content: center; min-height: 42px; }
  }
</style>
