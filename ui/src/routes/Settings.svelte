<script>
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
  let setupError = $state('')

  async function loadSetup() {
    setupError = ''
    try { setup = await setupState() }
    catch (ex) { setupError = ex.message || 'Could not load setup state.' }
  }

  if (isAdmin) loadSetup()

  const setupNeedsAttention = $derived(
    isAdmin && setup !== undefined && !setup?.completed_at &&
    !setup?.filing_tree_chosen && !setupEngaged
  )
</script>

<div class="content-narrow settings-page">
  {#if isAdmin}
    <nav class="settings-tabs" aria-label="Settings areas">
      <a class:on={!archiveSelected} href="#/settings">My account</a>
      <a class:on={archiveSelected} href="#/settings?tab=archive">Archive configuration</a>
    </nav>
  {/if}

  {#if setupNeedsAttention}
    <section class="setup-row settings-section" aria-label="Setup wizard">
      <div>
        <b>Setup is incomplete</b>
        <span>Choose a filing tree to finish the guided archive setup.</span>
      </div>
      <a role="button" class="btn sm primary" href="#/setup" onclick={onSetupEngaged}>Continue setup</a>
    </section>
  {/if}

  {#if archiveSelected}
    {#if setupError}
      <div class="err settings-error">
        <span>{setupError}</span>
        <button class="btn sm" onclick={loadSetup}>Retry</button>
      </div>
    {:else if setup === undefined}
      <div class="archive-loading" aria-label="Loading archive settings">
        <div class="skel" style="width:28%"></div>
        <div class="skel" style="width:76%"></div>
      </div>
    {:else if !setupNeedsAttention}
      <Lazy load={loadArchive} props={{ notify, initialSection, onTaxonomyChanged, setupSnapshot: setup }} />
    {/if}
  {:else}
    <AccountSettings {notify} />
  {/if}

  {#if session.user?.build_version}
    <footer class="build-info" aria-label="Suchi build">
      Suchi {session.user.build_version}{session.user.build_revision ? ` · ${session.user.build_revision}` : session.user.build_version === 'dev' ? ' · revision unavailable' : ''}
    </footer>
  {/if}
</div>

<style>
  .settings-page { width:100%;max-width:1120px;gap:0 }
  .settings-tabs { display:flex;gap:4px;margin:-4px 0 28px;border-bottom:1px solid var(--line) }
  .settings-tabs a { padding:9px 14px;border-bottom:2px solid transparent;color:var(--muted);font-size:.84rem;font-weight:600;text-decoration:none }
  .settings-tabs a:hover { color:var(--ink) }
  .settings-tabs a.on { border-color:var(--accent);color:var(--accent) }
  .archive-loading { display:grid;gap:14px;padding:20px 0 }
  .settings-error { display:flex;align-items:center;justify-content:space-between;gap:12px }
  .setup-row { display:flex;align-items:center;justify-content:space-between;gap:20px;padding:2px 0 8px }
  .setup-row > div { display:flex;flex-direction:column;gap:3px }
  .setup-row b { font-size:.86rem }
  .setup-row span { color:var(--muted);font-size:.8rem }
  .settings-section { padding:6px 0 28px;margin-bottom:26px;border-bottom:1px solid var(--line) }
  .build-info { padding:20px 0;color:var(--muted);font-size:.78rem;overflow-wrap:anywhere }
  @media (max-width: 520px) {
    .setup-row { align-items:flex-start;flex-direction:column }
  }
</style>
