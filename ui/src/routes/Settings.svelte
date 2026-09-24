<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { session } from '../lib/session.svelte.js'
  import { scopedHash as filingHref } from '../lib/systems.svelte.js'
  import SetupReminder from '../lib/SetupReminder.svelte'
  import AccountSettings from './AccountSettings.svelte'
  import Lazy from '../lib/Lazy.svelte'

  const loadArchive = () => import('./ArchiveSettings.svelte')

  let { notify, initialTab = '', initialSection = '', initialPeople = '', initialMetadata = '', setupNeeded = false, setupError = false, onRetrySetup, onTaxonomyChanged } = $props()

  const isAdmin = $derived(session.user?.role === 'admin')
  const archiveSelected = $derived(isAdmin && initialTab === 'archive')
  async function taxonomyChanged() {
    await onTaxonomyChanged?.()
  }

  const setupNeedsAttention = $derived(isAdmin && setupNeeded)
</script>

<div class="content-narrow settings-page">
  {#if isAdmin}
    <nav class="settings-tabs" aria-label="Settings areas">
      <a class:on={!archiveSelected} href={filingHref("#/settings")}>My account</a>
      <a class:on={archiveSelected} href={filingHref("#/settings?tab=archive")}>Archive configuration</a>
    </nav>
  {/if}

  <div class="settings-body" class:account-body={!archiveSelected} class:archive-body={archiveSelected}>
  {#if setupNeedsAttention}
    <SetupReminder placement="settings" error={setupError} onRetry={onRetrySetup} />
  {/if}

  {#if archiveSelected}
    <div class="archive-slot">
      <Lazy load={loadArchive} props={{ notify, initialSection, initialPeople, initialMetadata, onTaxonomyChanged: taxonomyChanged }} />
    </div>
  {:else}
    <AccountSettings {notify} />
  {/if}
  </div>

  {#if session.user?.build_version}
    <footer class="build-info" aria-label="Suchi build">
      Suchi {session.user.build_version}
      {#if session.user.build_version === 'dev' || session.user.build_version.endsWith('-dev') || session.user.build_version.startsWith('snapshot-')}
        · {session.user.build_revision || 'revision unavailable'}
      {/if}
    </footer>
  {/if}
</div>

<style>
  .settings-page { display:flex;flex-direction:column;width:100%;height:100%;min-height:0;max-width:1120px;gap:0 }
  .settings-tabs { display:flex;flex:none;gap:4px;margin:0 0 28px;border-bottom:1px solid var(--line) }
  .settings-tabs a { padding:9px 14px;border-bottom:2px solid transparent;color:var(--muted);font-size:.84rem;font-weight:600;text-decoration:none }
  .settings-tabs a:hover { color:var(--ink) }
  .settings-tabs a.on { border-color:var(--accent);color:var(--accent) }
  .settings-body { flex:1;min-height:0 }
  .account-body { overflow-y:auto;padding-inline-end:28px;scrollbar-gutter:stable }
  .archive-body { display:flex;flex-direction:column;overflow:hidden;padding-inline-end:20px }
  .archive-slot { flex:1;min-height:0 }
  .build-info { flex:none;padding:16px 0 0;color:var(--muted);font-size:.78rem;overflow-wrap:anywhere }
</style>
