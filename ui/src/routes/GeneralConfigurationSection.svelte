<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from '../lib/i18n.svelte.js'
  import { systems } from '../lib/systems.svelte.js'
  import {
    adminListUsers, getIngestSettings, getPreferences, getScanSettings,
    saveIngestSettings, savePreferences, saveScanSettings,
  } from '../lib/api.js'

  let { section, notify } = $props()
  let loading = $state(true)
  let loadError = $state('')
  let busy = $state(false)
  let error = $state('')
  let users = $state([])
  let ingest = $state({ fs_watch_dir: '', fs_watch_owner_email: '', fs_watch_system: '' })
  let ingestRecursive = $state(true)
  let preferences = $state({ backup_interval_hours: 24, ocr_languages: 'eng' })
  let scan = $state({
    scan_asn_mode: 'off',
    scan_asn_prefix: 'ASN',
    scan_split_dpi: 150,
    managed: { scan_asn_mode: false, scan_asn_prefix: false, scan_split_dpi: false },
  })
  let savedScan = { scan_asn_prefix: 'ASN', scan_split_dpi: 150 }
  const scanModes = [
    { id: 'off', label: translate('configurationSection.asnModeOff'), description: translate('configurationSection.asnModeOffDesc') },
    { id: 'assign', label: translate('configurationSection.asnModeAssign'), description: translate('configurationSection.asnModeAssignDesc') },
    { id: 'split', label: translate('configurationSection.asnModeSplit'), description: translate('configurationSection.asnModeSplitDesc') },
  ]
  const scanPrefixValid = $derived(/^[A-Za-z][A-Za-z0-9._:-]*$/.test(scan.scan_asn_prefix))
  const scanDpiValid = $derived(Number.isInteger(Number(scan.scan_split_dpi))
    && Number(scan.scan_split_dpi) >= 72 && Number(scan.scan_split_dpi) <= 600)
  const allScanSettingsManaged = $derived(scan.managed.scan_asn_mode
    && scan.managed.scan_asn_prefix && scan.managed.scan_split_dpi)
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
      } else if (section === 'scan-processing') {
        const settings = await getScanSettings()
        if (version !== loadVersion) return
        scan.scan_asn_mode = settings?.scan_asn_mode || 'off'
        scan.scan_asn_prefix = settings?.scan_asn_prefix || 'ASN'
        scan.scan_split_dpi = settings?.scan_split_dpi ?? 150
        scan.managed = {
          scan_asn_mode: !!settings?.managed?.scan_asn_mode,
          scan_asn_prefix: !!settings?.managed?.scan_asn_prefix,
          scan_split_dpi: !!settings?.managed?.scan_split_dpi,
        }
        savedScan = {
          scan_asn_prefix: scan.scan_asn_prefix,
          scan_split_dpi: scan.scan_split_dpi,
        }
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

  async function saveScan() {
    const inactive = scan.scan_asn_mode === 'off'
    await save(async () => {
      await saveScanSettings({
        scan_asn_mode: scan.scan_asn_mode,
        scan_asn_prefix: inactive ? savedScan.scan_asn_prefix : scan.scan_asn_prefix,
        scan_split_dpi: inactive ? Number(savedScan.scan_split_dpi) : Number(scan.scan_split_dpi),
      })
      await load()
    }, translate('configurationSection.scanProcessingSaved'))
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
    {:else if section === 'scan-processing'}
      <h3>{translate('configurationSection.barcodeAssistedScanFiling')}</h3>
      <p class="wiz-p">{translate('configurationSection.scanProcessingDescription')}</p>
      {#if scan.managed.scan_asn_mode}
        <p class="sub">{translate('configurationSection.modeManagedByServer')}</p>
      {/if}
      <div class="scan-modes" role="radiogroup" aria-label={translate('configurationSection.archiveSerialNumberMode')}>
        {#each scanModes as option (option.id)}
          <label class="scan-mode" class:on={scan.scan_asn_mode === option.id} class:locked={scan.managed.scan_asn_mode}>
            <input type="radio" name="scan-asn-mode" value={option.id} aria-label={option.label}
                   bind:group={scan.scan_asn_mode} disabled={scan.managed.scan_asn_mode} />
            <span><b>{option.label}</b><small>{option.description}</small></span>
          </label>
        {/each}
      </div>
      {#if scan.scan_asn_mode !== 'off'}
        <div class="field"><label for="scan-prefix">{translate('configurationSection.qrLabelPrefix')}</label>
          <input id="scan-prefix" class="input mono" required pattern="[A-Za-z][A-Za-z0-9._:-]*"
                 bind:value={scan.scan_asn_prefix} disabled={scan.managed.scan_asn_prefix} />
          <p class="sub">
            {translate('configurationSection.qrLabelPrefixHelp')}
            <code>{scan.scan_asn_prefix || '…'}4021</code>
            {#if scan.managed.scan_asn_prefix} · {translate('configurationSection.setByServerConfiguration')}{/if}
          </p>
        </div>
        <div class="field"><label for="scan-dpi">{translate('configurationSection.pageAnalysisDpi')}</label>
          <input id="scan-dpi" class="input" type="number" min="72" max="600" step="1"
                 bind:value={scan.scan_split_dpi} disabled={scan.managed.scan_split_dpi} />
          <p class="sub">
            {translate('configurationSection.pageAnalysisDpiHelp')}
            {#if scan.managed.scan_split_dpi} · {translate('configurationSection.setByServerConfiguration')}{/if}
          </p>
        </div>
      {/if}
      <p class="wiz-p sub">{translate('configurationSection.scanSettingsRuntimeHelp')}</p>
      <div class="toolbar">
        <button class="btn primary sm"
                disabled={busy || allScanSettingsManaged || (scan.scan_asn_mode !== 'off' && (!scanPrefixValid || !scanDpiValid))}
                onclick={saveScan}>{translate('configurationSection.saveScanProcessing')}</button>
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
  .scan-modes { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 8px; margin-bottom: 16px; }
  .scan-mode { display: grid; grid-template-columns: auto minmax(0, 1fr); gap: 8px; padding: 11px; border: 1px solid var(--line); border-radius: var(--r-sm); background: var(--surface); cursor: pointer; }
  .scan-mode.on { border-color: var(--accent); box-shadow: inset 0 0 0 1px var(--accent); }
  .scan-mode.locked { cursor: default; opacity: .7; }
  .scan-mode input { margin: 3px 0 0; accent-color: var(--accent); }
  .scan-mode span { display: flex; min-width: 0; flex-direction: column; gap: 3px; }
  .scan-mode b { color: var(--ink); font-size: .82rem; }
  .scan-mode small { color: var(--muted); font-size: .75rem; line-height: 1.4; }
  @media (max-width: 760px) { .scan-modes { grid-template-columns: 1fr; } }
</style>
