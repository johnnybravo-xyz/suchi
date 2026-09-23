<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { scopedHash as filingHref } from './systems.svelte.js'
  import Icon from './Icon.svelte'

  let { placement = 'side', error = false, onRetry, onContinue } = $props()
</script>

<aside class="setup-reminder" class:setup-reminder-side={placement === 'side'}
       class:setup-reminder-mobile={placement === 'mobile'}
       class:setup-reminder-settings={placement === 'settings'} aria-label="Archive setup">
  <div class="setup-reminder-head">
    <span class="setup-reminder-icon"><Icon name="settings" size={16} /></span>
    <div>
      <b>{error ? 'Archive setup status unavailable' : 'Choose your filing tree'}</b>
      <span>{error ? 'Suchi could not verify whether setup is complete.' : 'Choose how documents are organized to finish archive setup.'}</span>
    </div>
  </div>
  <div class="setup-reminder-actions">
    {#if error}<button class="btn sm" onclick={() => onRetry?.()}>Retry</button>{/if}
    <a role="button" class="btn primary sm" href={filingHref("#/settings?tab=archive&section=archive")} onclick={onContinue}>Continue setup</a>
  </div>
</aside>
