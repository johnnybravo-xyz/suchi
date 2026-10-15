<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from '../lib/i18n.svelte.js'
  import { adminListUsers } from '../lib/api.js'
  import EmailAccounts from '../lib/EmailAccounts.svelte'

  let { notify } = $props()
  let users = $state([])
  let loading = $state(true)
  let error = $state('')
  let loadVersion = 0

  async function load() {
    const version = ++loadVersion
    loading = true
    error = ''
    try {
      const result = await adminListUsers()
      if (version === loadVersion) users = result?.results || result || []
    } catch (exception) {
      if (version === loadVersion) error = exception.message || translate('configurationSection.couldNotLoadArchiveUsers')
    } finally {
      if (version === loadVersion) loading = false
    }
  }

  load()
</script>

<div class="configuration-section">
  {#if error}
    <div class="err">{error}</div>
    <div class="toolbar"><button class="btn sm" onclick={load}>{translate('common.retry')}</button></div>
  {:else if loading}
    <div class="configuration-loading" aria-label={translate('configurationSection.loadingConfiguration')}>
      <div class="skel" style="width:32%;height:16px"></div>
      <div class="skel" style="width:78%"></div>
      <div class="skel" style="width:62%"></div>
    </div>
  {:else}
    <h3>{translate('configurationSection.emailIntake')}</h3>
    <p class="wiz-p">{translate('configurationSection.pointSuchiAtOneOrMoreMailboxes')}</p>
    <EmailAccounts {notify} {users} />
  {/if}
</div>

<style>
  .wiz-p { max-width: 46em; margin: 6px 0 16px; color: var(--muted); font-size: .92rem; }
  .configuration-loading { display: grid; gap: 10px; padding: 8px 0; }
</style>
