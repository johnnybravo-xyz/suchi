<script>
  import { scopedHash as filingHref } from '../lib/systems.svelte.js'
  import { setupState } from '../lib/api.js'
  import { session } from '../lib/session.svelte.js'
  import AccountSettings from './AccountSettings.svelte'
  import Lazy from '../lib/Lazy.svelte'

  const loadArchive = () => import('./ArchiveSettings.svelte')

  let {
    notify, initialTab = '', initialSection = '', onTaxonomyChanged,
    setupEngaged = false, onSetupEngaged,
  } = $props()

  const isAdmin = session.user?.role === 'admin'
  const archiveSelected = $derived(isAdmin && initialTab === 'archive')
  let setup = $state(undefined)

  async function loadSetup() {
    try { setup = await setupState() }
    catch { setup = undefined }
  }

  async function taxonomyChanged() {
    await Promise.all([loadSetup(), onTaxonomyChanged?.()])
  }

  if (isAdmin) loadSetup()

  const setupNeedsAttention = $derived(
    isAdmin && setup !== undefined && !setup?.filing_tree_chosen && !setupEngaged
  )
</script>

<div class="content-narrow settings-page">
  {#if isAdmin}
    <nav class="settings-tabs" aria-label="Settings areas">
      <a class:on={!archiveSelected} href={filingHref("#/settings")}>My account</a>
      <a class:on={archiveSelected} href={filingHref("#/settings?tab=archive")}>Archive configuration</a>
    </nav>
  {/if}

  <div class="settings-body" class:account-body={!archiveSelected}>
  {#if setupNeedsAttention}
    <section class="setup-row settings-section" aria-label="Archive setup">
      <div>
        <b>Archive setup is incomplete</b>
        <span>Choose a filing tree to finish archive setup.</span>
      </div>
      <a role="button" class="btn sm primary" href={filingHref("#/settings?tab=archive&section=archive")} onclick={onSetupEngaged}>Continue setup</a>
    </section>
  {/if}

  {#if archiveSelected}
    <Lazy load={loadArchive} props={{ notify, initialSection, onTaxonomyChanged: taxonomyChanged, setupSnapshot: setup }} />
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
  .settings-body { flex:1;min-height:0;overflow-y:auto }
  .account-body { padding-inline-end:16px;scrollbar-gutter:stable }
  .setup-row { display:flex;align-items:center;justify-content:space-between;gap:20px;padding:2px 0 8px }
  .setup-row > div { display:flex;flex-direction:column;gap:3px }
  .setup-row b { font-size:.86rem }
  .setup-row span { color:var(--muted);font-size:.8rem }
  .settings-section { padding:6px 0 28px;margin-bottom:26px;border-bottom:1px solid var(--line) }
  .build-info { flex:none;padding:16px 0 0;color:var(--muted);font-size:.78rem;overflow-wrap:anywhere }
  @media (max-width: 520px) {
    .setup-row { align-items:flex-start;flex-direction:column }
  }
</style>
