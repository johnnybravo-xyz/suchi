<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "../lib/i18n.svelte.js";
  import { formatNumber, plural } from '../lib/format.js'
  import { scopedHash as filingHref, systems } from '../lib/systems.svelte.js'
  import { setupState, adminListUsers, getIngestSettings, getArchiveMatchingSettings, getLLMSettings,
           getPreferences, listEmailAccounts, listAutomations, listRenderedLayouts } from '../lib/api.js'
  import { archiveSettingsGroups, archiveSettingsItems, FILING_TREE_SETTINGS_ITEM } from '../lib/configuration.js'
  import { hasFeature } from '../lib/capabilities.js'
  import { session } from '../lib/session.svelte.js'
  import Icon from '../lib/Icon.svelte'
  import Lazy from '../lib/Lazy.svelte'
  import SystemSettings from '../lib/SystemSettings.svelte'
  import FilingTreeSettings from './FilingTreeSettings.svelte'
  import FolderLayoutsSettings from './FolderLayoutsSettings.svelte'
  import PeopleSettings from './PeopleSettings.svelte'

  const CONFIGURATION_LOADERS = {
    sources: () => import('./GeneralConfigurationSection.svelte'),
    preferences: () => import('./GeneralConfigurationSection.svelte'),
    matching: () => import('./ArchiveMatchingSettings.svelte'),
    mail: () => import('./MailConfigurationSection.svelte'),
    llm: () => import('./ConfigurationSection.svelte'),
  }

  let { notify, initialSection = '', initialPeople = '', initialMetadata = '', onTaxonomyChanged } = $props()

  const settingsGroups = $derived(archiveSettingsGroups(session.user))
  const settingsItems = $derived(archiveSettingsItems(session.user))
  const current = $derived(settingsItems.some((item) => item.name === initialSection) ? initialSection : 'overview')
  const currentItem = $derived(settingsItems.find((item) => item.name === current))
  const configurationLoader = $derived(CONFIGURATION_LOADERS[current])
  let statuses = $state({})

  function titleCase(value) {
    return String(value || '').replaceAll('_', ' ').replace(/\b\w/g, (letter) => letter.toUpperCase())
  }

  function rows(response, key = 'results') {
    return response?.[key] || response || []
  }

  function status(label, detail, tone = '') {
    return { label, detail, tone }
  }

  async function loadOverview() {
    const requests = {
      setup: setupState(),
      users: adminListUsers(),
      ingest: getIngestSettings(),
      matching: getArchiveMatchingSettings(),
      preferences: getPreferences(),
      automations: listAutomations(),
      layouts: listRenderedLayouts(),
    }
    if (hasFeature(session.user, 'email_ingestion')) requests.mail = listEmailAccounts()
    if (hasFeature(session.user, 'llm_connections')) requests.llm = getLLMSettings()
    const entries = Object.entries(requests)
    const results = await Promise.allSettled(entries.map(([, request]) => request))
    const settled = Object.fromEntries(entries.map(([name], index) => [name, results[index]]))
    const next = Object.fromEntries(settingsItems.map((item) => [
      item.name, status(translate("archiveSettings.unavailable"), translate("archiveSettings.couldNotLoadStatus"), 'warn'),
    ]))

    if (settled.setup.status === 'fulfilled') {
      const preset = settled.setup.value?.current_preset
      next['filing-tree'] = status(settled.setup.value?.filing_tree_chosen ? translate("archiveSettings.configured") : translate("archiveSettings.notSet"), preset ? translate("archiveSettings.valueFilingTree", { "preset": titleCase(preset) }) : translate("archiveSettings.customFilingTree"), settled.setup.value?.filing_tree_chosen ? 'ok' : 'warn')
    }
    if (settled.users.status === 'fulfilled') {
      const count = rows(settled.users.value).length
      next.users = status(plural("archiveSettings.userCount", count), translate("archiveSettings.rolesAndArchiveCapabilities"))
    }
    next.metadata = status(translate("archiveSettings.3Catalogs"), translate("archiveSettings.tagsCorrespondentsAndCustomFields"))
    if (settled.layouts.status === 'fulfilled') {
      const count = rows(settled.layouts.value).length
      next['folder-layouts'] = status(translate("archiveSettings.valueSaved", { count: formatNumber(count) }), translate("archiveSettings.localRenderedFilesystemPaths"))
    }
    if (settled.ingest.status === 'fulfilled') {
      const directory = settled.ingest.value?.fs_watch_dir || ''
      next.sources = status(directory ? translate("archiveSettings.active") : translate("archiveSettings.notSet"), directory || translate("archiveSettings.browserUploadsAndConnectedAppsRemainAvailable"), directory ? 'ok' : 'warn')
    }
    if (settled.mail?.status === 'fulfilled') {
      const accounts = rows(settled.mail.value, 'accounts')
      const enabled = accounts.filter((account) => account.enabled).length
      next.mail = status(enabled ? translate("archiveSettings.valueActive", { enabled: formatNumber(enabled) }) : translate("archiveSettings.notSet"), enabled ? plural("archiveSettings.connectedMailboxCount", accounts.length) : translate("archiveSettings.noMailboxesConnected"), enabled ? 'ok' : 'warn')
    }
    if (settled.matching.status === 'fulfilled') {
      const value = settled.matching.value
      next.matching = status(value?.archive_enabled ? translate("archiveSettings.localMatching") : translate("archiveSettings.manual"), translate("archiveSettings.noExternalModel"), value?.archive_enabled ? 'ok' : '')
    }
    if (settled.llm?.status === 'fulfilled') {
      const value = settled.llm.value
      next.llm = status(value?.active ? translate("archiveSettings.modelActive") : translate("archiveSettings.notSet"), value?.active ? value.model || translate("archiveSettings.configuredModel") : translate("archiveSettings.noExternalModel"), value?.active ? 'ok' : '')
    }
    if (settled.automations.status === 'fulfilled') {
      const active = rows(settled.automations.value).filter((automation) => automation.enabled).length
      next.automations = status(translate("archiveSettings.valueActive2", { active: formatNumber(active) }), translate("archiveSettings.filingAndMetadataRules"))
    }
    if (settled.preferences.status === 'fulfilled') {
      const languages = settled.preferences.value?.ocr_languages || []
      const interval = Number(settled.preferences.value?.backup_interval_hours || 0)
      const languageSummary = languages.join(', ') || translate("archiveSettings.noOCRLanguages")
      const backupSummary = interval ? plural("archiveSettings.backupEveryHours", interval) : translate("archiveSettings.noScheduledBackups")
      next.preferences = status(interval ? translate("archiveSettings.scheduled") : translate("archiveSettings.backupsOff"), translate("archiveSettings.valueValue2", { languages: languageSummary, backups: backupSummary }), interval ? 'ok' : 'warn')
    }
    statuses = next
  }

  $effect(() => { if (current === 'overview') loadOverview() })
</script>

<section class="archive-settings" aria-label={translate("archiveSettings.archiveConfiguration")}>
  <aside class="archive-sidebar">
    <nav class="archive-rail" aria-label={translate("archiveSettings.archiveSettingsSections")}>
      <a class:on={current === 'overview'} aria-current={current === 'overview' ? 'page' : undefined}
         href={filingHref("#/settings?tab=archive")}><Icon name="settings" size={15} /><span>{translate("archiveSettings.overview")}</span></a>
      <a class:on={current === FILING_TREE_SETTINGS_ITEM.name} aria-current={current === FILING_TREE_SETTINGS_ITEM.name ? 'page' : undefined}
         href={filingHref(FILING_TREE_SETTINGS_ITEM.href)}><Icon name={FILING_TREE_SETTINGS_ITEM.icon} size={15} /><span>{FILING_TREE_SETTINGS_ITEM.label}</span></a>
      {#each settingsGroups as group (group.name)}
        <div class="rail-group">
          <span class="rail-group-label">{group.label}</span>
          {#each group.items as item (item.name)}
            <a class:on={current === item.name} aria-current={current === item.name ? 'page' : undefined}
               href={filingHref(item.href)}><Icon name={item.icon} size={15} /><span>{item.label}</span></a>
          {/each}
        </div>
      {/each}
    </nav>
  </aside>

  <div class="archive-content" role="region" aria-label={translate("archiveSettings.configurationContent")}>
    {#if systems.introduced && (current === 'overview' || current === 'users' || current === 'metadata' || current === 'filing-tree' || current === 'folder-layouts')}
      <SystemSettings {notify} />
    {/if}
    {#if current === 'overview'}
      <header class="archive-intro">
        <div>
          <span class="eyebrow">{translate("archiveSettings.archive")}</span>
          <h2>{translate("archiveSettings.configureHowYourArchiveWorks")}</h2>
          <p>{translate("archiveSettings.filingTreesMetadataMailboxesAndAutomationsLive", { "archive": systems.code || 'this archive' })}</p>
        </div>
      </header>

      {@const filingStatus = statuses[FILING_TREE_SETTINGS_ITEM.name]}
      {#if filingStatus && filingStatus.tone !== 'ok'}
        <div class="filing-hero">
          <span class="configuration-icon"><Icon name={FILING_TREE_SETTINGS_ITEM.icon} size={18} /></span>
          <span class="configuration-copy">
            <b>{translate("archiveSettings.chooseYourFilingTree")}</b>
            <small>{translate("archiveSettings.requiredBeforeSuchiCanFileAnythingEverything")}</small>
          </span>
          <a class="btn primary sm" href={filingHref(FILING_TREE_SETTINGS_ITEM.href)}>{translate("archiveSettings.chooseFilingTree")}</a>
        </div>
      {:else}
        <a class="configuration-row filing-row" href={filingHref(FILING_TREE_SETTINGS_ITEM.href)}>
          <span class="configuration-icon"><Icon name={FILING_TREE_SETTINGS_ITEM.icon} size={15} /></span>
          <span class="configuration-copy">
            <b>{FILING_TREE_SETTINGS_ITEM.label}</b>
            <small>{filingStatus?.detail || FILING_TREE_SETTINGS_ITEM.description}</small>
          </span>
          {#if filingStatus}<span class="status" class:ok={filingStatus.tone === 'ok'}>{filingStatus.label}</span>{/if}
          <Icon name="chev" size={13} />
        </a>
      {/if}
      <div class="configuration-groups">
        {#each settingsGroups as group (group.name)}
          <section class="configuration-group" aria-labelledby={`group-${group.name}`}>
            <header>
              <h3 id={`group-${group.name}`}>{group.label}</h3>
            </header>
            {#each group.items as item (item.name)}
              {@const itemStatus = statuses[item.name]}
              <a class="configuration-row" href={filingHref(item.href)}>
                <span class="configuration-icon"><Icon name={item.icon} size={15} /></span>
                <span class="configuration-copy">
                  <b>{item.label}</b>
                  <small>{itemStatus?.detail || item.description}</small>
                </span>
                {#if itemStatus}<span class="status" class:ok={itemStatus.tone === 'ok'} class:warn={itemStatus.tone === 'warn'}>{itemStatus.label}</span>{/if}
                <Icon name="chev" size={13} />
              </a>
            {/each}
          </section>
        {/each}
      </div>
    {:else if currentItem}
      <header class="section-intro">
        <a href={filingHref("#/settings?tab=archive")}><Icon name="left" size={13} /> {translate("archiveSettings.overview")}</a>
        <span>{currentItem.description}</span>
      </header>
      {#if current === 'filing-tree'}
        <FilingTreeSettings {notify} {onTaxonomyChanged} />
      {:else if current === 'folder-layouts'}
        <FolderLayoutsSettings {notify} />
      {:else if current === 'users'}
        <PeopleSettings view="people" {notify} {initialPeople} {initialMetadata} />
      {:else if current === 'metadata'}
        <PeopleSettings view="metadata" {notify} {initialPeople} {initialMetadata} />
      {:else if current === 'automations'}
        <div class="card handoff-card">
          <span class="handoff-icon"><Icon name="zap" size={20} /></span>
          <span class="handoff-copy">
            <h3>{translate("archiveSettings.buildAndManageFilingRules")}</h3>
            <p>{translate("archiveSettings.automationsHaveADedicatedWorkspaceForOrdering")}</p>
          </span>
          <a role="button" class="btn primary handoff-action" href={filingHref("#/automations")}>{translate("archiveSettings.openAutomations")} <Icon name="chev" size={13} /></a>
        </div>
      {:else if configurationLoader}
        <div class="card section-card">
          <Lazy load={configurationLoader} props={{ section: current, notify }} />
        </div>
      {/if}
    {/if}
  </div>
</section>

<style>
  .archive-settings { display: grid; grid-template-columns: 212px minmax(0, 1fr); grid-template-rows: minmax(0, 1fr); height: 100%; min-height: 0; overflow: hidden; border: 1px solid var(--line); border-radius: var(--r); background: var(--surface); }
  .archive-sidebar { min-width: 0; min-height: 0; overflow: hidden; border-right: 1px solid var(--line); border-radius: var(--r) 0 0 var(--r); background: var(--bg); }
  .archive-rail { display: flex; flex-direction: column; height: 100%; overflow-y: auto; overscroll-behavior: contain; padding: 12px; scrollbar-width: thin; }
  .rail-group { display: flex; flex-direction: column; gap: 2px; margin-top: 12px; padding-top: 12px; border-top: 1px solid var(--line); }
  .rail-group-label { padding: 2px 10px 6px; color: var(--muted); font-size: .62rem; font-weight: 650; letter-spacing: .04em; line-height: 1.4; }
  .archive-rail a { display: flex; align-items: center; gap: 9px; min-height: 36px; padding: 8px 10px; border-radius: 7px; color: var(--muted); font-size: .78rem; line-height: 1.4; text-decoration: none; }
  .archive-rail a :global(svg) { flex: none; }
  .archive-rail a:hover { background: var(--surface-2); color: var(--ink); }
  .archive-rail a.on { background: var(--tint); color: var(--accent); font-weight: 650; }
  .archive-content { min-width: 0; min-height: 0; overflow-y: auto; overscroll-behavior: contain; scrollbar-gutter: stable; padding: 22px; padding-inline-end: 34px; }
  .archive-intro { display: flex; align-items: flex-end; justify-content: space-between; gap: 20px; margin-bottom: 20px; }
  .eyebrow { display: block; margin-bottom: 5px; color: var(--accent); font-family: ui-monospace, monospace; font-size: .64rem; font-weight: 700; letter-spacing: .07em; }
  .archive-intro h2 { font-size: 1.35rem; line-height: 1.2; }
  .archive-intro p { max-width: 590px; margin: 6px 0 0; color: var(--muted); font-size: .82rem; }
  .filing-row { margin-bottom: 14px; border: 1px solid var(--line); border-radius: var(--r); background: var(--surface); }
  .filing-hero { display: flex; align-items: center; gap: 14px; margin-bottom: 16px; padding: 16px 18px; border: 1px solid color-mix(in srgb, var(--accent) 40%, var(--line)); border-radius: var(--r); background: var(--tint); }
  .filing-hero .configuration-icon { width: 40px; height: 40px; background: var(--surface); }
  .filing-hero b { font-size: .98rem; letter-spacing: -.01em }
  .filing-hero small { display: block; margin-top: 2px; color: var(--muted); font-size: .8rem }
  .filing-hero .btn { margin-left: auto; flex: none; white-space: nowrap }
  .configuration-groups { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
  .configuration-group { overflow: hidden; border: 1px solid var(--line); border-radius: var(--r); background: var(--surface); }
  .configuration-group > header { padding: 13px 15px 11px; border-bottom: 1px solid var(--line); cursor: default; }
  .configuration-group h3 { color: var(--ink); font-size: .92rem; font-weight: 700; line-height: 1.2; }
  .configuration-row { display: grid; grid-template-columns: 32px minmax(0, 1fr) auto auto; gap: 10px; align-items: center; min-height: 61px; padding: 10px 12px; border-bottom: 1px solid var(--line); color: inherit; text-decoration: none; }
  .configuration-row:last-child { border-bottom: 0; }
  .configuration-row:hover { background: var(--tint); }
  .configuration-icon { display: grid; place-items: center; width: 30px; height: 30px; border: 1px solid var(--line); border-radius: 8px; background: var(--bg); color: var(--accent); }
  .configuration-copy { min-width: 0; }
  .configuration-copy b, .configuration-copy small { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .configuration-copy b { font-size: .8rem; }
  .configuration-copy small { margin-top: 2px; color: var(--muted); font-size: .67rem; }
  .status { padding: 2px 7px; border-radius: 99px; background: var(--surface-2); color: var(--muted); font-size: .62rem; white-space: nowrap; }
  .status.ok { background: var(--ok-soft); color: var(--ok); }
  .status.warn { background: var(--warn-soft); color: var(--warn); }
  .section-intro { display: flex; align-items: center; justify-content: space-between; gap: 14px; margin-bottom: 10px; }
  .section-intro a { display: inline-flex; align-items: center; gap: 4px; color: var(--accent); font-size: .76rem; font-weight: 600; text-decoration: none; }
  .section-intro > span { color: var(--muted); font-size: .72rem; text-align: right; }
  .section-card { min-height: 360px; }
  .handoff-card { display:grid;grid-template-columns:42px minmax(0,1fr) auto;gap:14px;align-items:center;min-height:112px }
  .handoff-icon { display:grid;place-items:center;width:42px;height:42px;border-radius:10px;background:var(--tint);color:var(--accent) }
  .handoff-copy h3 { margin:0;font-size:.92rem }
  .handoff-copy p { margin:4px 0 0;color:var(--muted);font-size:.78rem;line-height:1.45 }
  .handoff-action { white-space:nowrap;text-decoration:none }
  @media (max-width: 900px) {
    .archive-settings { grid-template-columns: 1fr; grid-template-rows: auto minmax(0, 1fr); }
    .archive-sidebar { border-right: 0; border-bottom: 1px solid var(--line); border-radius: var(--r) var(--r) 0 0; }
    .archive-rail { flex-direction: row; gap: 4px; height: auto; overflow-x: auto; overflow-y: hidden; padding: 10px; scroll-padding-inline: 10px; }
    .rail-group { display: contents; }
    .rail-group-label { display: none; }
    .archive-rail a { flex: none; min-height: 40px; white-space: nowrap; }
  }
  @media (max-width: 680px) {
    .archive-content { padding: 16px; padding-inline-end: 28px; }
    .configuration-groups { grid-template-columns: 1fr; }
    .archive-intro { align-items: flex-start; flex-direction: column; }
    .configuration-row { grid-template-columns: 32px minmax(0, 1fr) auto; }
    .configuration-row .status { display: none; }
    .section-intro { align-items: flex-start; flex-direction: column; }
    .section-intro > span { text-align: left; }
    .handoff-card { grid-template-columns:42px minmax(0,1fr) }
    .handoff-action { grid-column:1 / -1;justify-content:center }
  }
</style>
