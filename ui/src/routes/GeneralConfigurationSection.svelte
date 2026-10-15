<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from '../lib/i18n.svelte.js'
  import { systems } from '../lib/systems.svelte.js'
  import { adminListUsers, getIngestSettings, getPreferences, saveIngestSettings, savePreferences } from '../lib/api.js'

  let { section, notify } = $props()
  let loading = $state(true)
  let loadError = $state('')
  let busy = $state(false)
  let error = $state('')
  let users = $state([])
  let ingest = $state({ fs_watch_dir: '', fs_watch_owner_email: '', fs_watch_system: '' })
  let ingestRecursive = $state(true)
  let preferences = $state({ backup_interval_hours: 24, ocr_languages: 'eng' })
  let loadVersion = 0

  function seedSourceOwner() {
    if (!ingest.fs_watch_owner_email) ingest.fs_watch_owner_email = users.find(user => !user.disabled)?.email || ''
  }

  async function load() {
    const version = ++loadVersion
    loading = true
    loadError = ''
    try {
      if (section === 'sources') {
        const [directory, settings] = await Promise.all([adminListUsers(), getIngestSettings()])
        if (version !== loadVersion) return
        users = directory?.results || directory || []
        ingest.fs_watch_dir = settings?.fs_watch_dir || ''
        ingest.fs_watch_owner_email = settings?.fs_watch_owner_email || ''
        ingest.fs_watch_system = settings?.fs_watch_system || ''
        ingestRecursive = settings?.fs_watch_recursive !== false
        seedSourceOwner()
      } else if (section === 'preferences') {
        const settings = await getPreferences()
        if (version !== loadVersion) return
        preferences.backup_interval_hours = settings?.backup_interval_hours ?? 24
        preferences.ocr_languages = (settings?.ocr_languages || ['eng']).join(',')
      }
    } catch (exception) {
      if (version === loadVersion) loadError = exception.message || translate('configurationSection.couldNotLoadThisConfiguration')
    } finally {
      if (version === loadVersion) loading = false
    }
  }

  async function save(action, message) {
    error = ''
    busy = true
    try {
      await action()
      notify?.(message)
    } catch (exception) {
      error = exception.message || translate('configurationSection.theServerRejectedThat')
    } finally {
      busy = false
    }
  }

  load()
</script>

<div class="configuration-section">
  {#if loadError}
    <div class="err">{loadError}</div>
    <div class="toolbar"><button class="btn sm" onclick={load}>{translate('common.retry')}</button></div>
  {:else if loading}
    <div class="configuration-loading" aria-label={translate('configurationSection.loadingConfiguration')}>
      <div class="skel" style="width:32%;height:16px"></div>
      <div class="skel" style="width:78%"></div>
      <div class="skel" style="width:62%"></div>
    </div>
  {:else}
    {#if error}<div class="err">{error}</div>{/if}
    {#if section === 'sources'}
      <h3>{translate('configurationSection.watchedFolder')}</h3>
      <p class="wiz-p">{translate('configurationSection.suchiReadsNewFilesFromThisServer', { configuration: translate(ingestRecursive ? 'configurationSection.subfoldersIncluded' : 'configurationSection.directFilesOnly') })}</p>
      <div class="field"><label for="i-dir">{translate('configurationSection.watchedDirectoryOnTheServer')}</label>
        <input id="i-dir" class="input mono" placeholder={translate('configurationSection.dataStaging')} bind:value={ingest.fs_watch_dir} /></div>
      <p class="sub">{translate('configurationSection.theDirectoryMustAlreadyExistAndBe')} <span class="mono">{translate('configurationSection.errors')}</span> {translate('configurationSection.directoryAreIgnored')}</p>
      <div class="field"><label for="i-owner">{translate('configurationSection.documentsFromItBelongTo')}</label>
        <select id="i-owner" class="input" bind:value={ingest.fs_watch_owner_email}>
          <option value="">{translate('configurationSection.selectOwner')}</option>
          {#each users.filter(user => !user.disabled) as user}
            <option value={user.email}>{user.display_name || user.email} · {user.email}</option>
          {/each}
        </select></div>
      {#if systems.introduced}
        <div class="field"><label for="i-system">{translate('configurationSection.watchedFolderFilingSystem')}</label>
          <select id="i-system" class="input" bind:value={ingest.fs_watch_system}>
            <option value="">{translate('configurationSection.originalArchiveDefault')}</option>
            {#each systems.results as system (system.code)}<option value={system.code}>{system.code} · {system.name}</option>{/each}
          </select>
          <p class="sub">{translate('configurationSection.thisIsTheServerWideWatchedFolder')}</p>
        </div>
      {/if}
      <div class="toolbar">
        <button class="btn primary sm" disabled={busy || !ingest.fs_watch_dir || !ingest.fs_watch_owner_email}
                onclick={() => save(() => saveIngestSettings(ingest), translate('configurationSection.watchedFolderSaved'))}>{translate('configurationSection.saveFolder')}</button>
      </div>
    {:else if section === 'preferences'}
      <h3>{translate('configurationSection.ocrAndBackups')}</h3>
      <div class="field"><label for="p-ocr">{translate('configurationSection.ocrLanguagesCommaSeparatedTesseractCodes')}</label>
        <input id="p-ocr" class="input mono" placeholder={translate('configurationSection.engHinNep')} bind:value={preferences.ocr_languages} /></div>
      <div class="field"><label for="p-bk">{translate('configurationSection.databaseSnapshotIntervalHours0Disables')}</label>
        <input id="p-bk" class="input" type="number" min="0" bind:value={preferences.backup_interval_hours} /></div>
      <p class="wiz-p sub">{translate('configurationSection.snapshotsIncludeOnlyTheDatabaseAComplete')}</p>
      <div class="toolbar">
        <button class="btn primary sm" disabled={busy}
                onclick={() => save(() => savePreferences({
                  backup_interval_hours: Number(preferences.backup_interval_hours) || 0,
                  ocr_languages: preferences.ocr_languages.split(',').map(value => value.trim()).filter(Boolean),
                }), translate('configurationSection.preferencesSaved'))}>{translate('configurationSection.savePreferences')}</button>
      </div>
    {/if}
  {/if}
</div>

<style>
  .wiz-p { max-width: 46em; margin: 6px 0 16px; color: var(--muted); font-size: .92rem; }
  .wiz-p.sub { font-size: .8rem; }
  .configuration-loading { display: grid; gap: 10px; padding: 8px 0; }
</style>
