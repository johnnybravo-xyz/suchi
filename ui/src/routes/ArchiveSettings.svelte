<script>
  import { setupState, adminListUsers, getIngestSettings, getLLMSettings,
           getPreferences, listEmailAccounts, listAutomations } from '../lib/api.js'
  import { ARCHIVE_SETTINGS_GROUPS, ARCHIVE_SETTINGS_ITEMS } from '../lib/configuration.js'
  import Icon from '../lib/Icon.svelte'
  import ConfigurationSection from './ConfigurationSection.svelte'
  import Admin from './Admin.svelte'

  let { notify, initialSection = '', onTaxonomyChanged } = $props()

  const configurableSections = new Set(ARCHIVE_SETTINGS_ITEMS.filter((item) => !item.external).map((item) => item.name))
  const current = $derived(configurableSections.has(initialSection) ? initialSection : 'overview')
  const currentItem = $derived(ARCHIVE_SETTINGS_ITEMS.find((item) => item.name === current))
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
    const [setup, users, ingest, llm, preferences, mail, automations] = await Promise.allSettled([
      setupState(), adminListUsers(), getIngestSettings(), getLLMSettings(),
      getPreferences(), listEmailAccounts(), listAutomations(),
    ])
    const next = {}

    if (setup.status === 'fulfilled') {
      const preset = setup.value?.current_preset
      next.archive = status(setup.value?.filing_tree_chosen ? 'Configured' : 'Not set', preset ? `${titleCase(preset)} filing tree` : 'Custom filing tree', setup.value?.filing_tree_chosen ? 'ok' : 'warn')
    }
    if (users.status === 'fulfilled') {
      const count = rows(users.value).length
      next.users = status(`${count} ${count === 1 ? 'user' : 'users'}`, 'Roles and archive capabilities')
    }
    if (ingest.status === 'fulfilled') {
      const directory = ingest.value?.fs_watch_dir || ''
      next.sources = status(directory ? 'Active' : 'Not set', directory || 'Uploads and API remain available', directory ? 'ok' : 'warn')
    }
    if (mail.status === 'fulfilled') {
      const accounts = rows(mail.value, 'accounts')
      const enabled = accounts.filter((account) => account.enabled).length
      next.mail = status(enabled ? `${enabled} active` : 'Not set', enabled ? `${accounts.length} connected ${accounts.length === 1 ? 'mailbox' : 'mailboxes'}` : 'No mailboxes connected', enabled ? 'ok' : 'warn')
    }
    if (llm.status === 'fulfilled') {
      const value = llm.value
      next.llm = status(value?.active ? 'Model active' : value?.archive_enabled ? 'Archive learning' : 'Manual', value?.active ? value.model || 'Configured model' : 'No external model', value?.active || value?.archive_enabled ? 'ok' : '')
    }
    if (automations.status === 'fulfilled') {
      const active = rows(automations.value).filter((automation) => automation.enabled).length
      next.automations = status(`${active} active`, 'Filing and metadata rules')
    }
    if (preferences.status === 'fulfilled') {
      const languages = preferences.value?.ocr_languages || []
      const interval = Number(preferences.value?.backup_interval_hours || 0)
      next.preferences = status(interval ? 'Scheduled' : 'Backups off', `${languages.join(', ') || 'No OCR languages'} · ${interval ? `every ${interval}h` : 'no snapshots'}`, interval ? 'ok' : 'warn')
    }
    statuses = next
  }

  $effect(() => { if (current === 'overview') loadOverview() })
</script>

<section class="archive-settings" aria-label="Archive configuration">
  <aside class="archive-rail" aria-label="Archive settings sections">
    <a class:on={current === 'overview'} href="#/settings?tab=archive">Overview</a>
    {#each ARCHIVE_SETTINGS_GROUPS as group (group.name)}
      <span>{group.label}</span>
      {#each group.items as item (item.name)}
        <a class:on={current === item.name} href={item.href}>{item.label}</a>
      {/each}
    {/each}
  </aside>

  <div class="archive-content">
    {#if current === 'overview'}
      <header class="archive-intro">
        <div>
          <span class="eyebrow">Archive</span>
          <h2>Configure how your archive works</h2>
          <p>Manage filing, intake, classification, and resilience separately from your personal account.</p>
        </div>
        <span class="admin-pill"><Icon name="shield" size={13} /> Administrators</span>
      </header>

      <div class="configuration-groups">
        {#each ARCHIVE_SETTINGS_GROUPS as group (group.name)}
          <section class="configuration-group" aria-labelledby={`group-${group.name}`}>
            <header>
              <h3 id={`group-${group.name}`}>{group.label}</h3>
              <p>{group.description}</p>
            </header>
            {#each group.items as item (item.name)}
              {@const itemStatus = statuses[item.name]}
              <a class="configuration-row" href={item.href}>
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
      <p class="archive-note">Changes here apply to the archive, not only to your account.</p>
    {:else if currentItem}
      <header class="section-intro">
        <a href="#/settings?tab=archive"><Icon name="left" size={13} /> Archive overview</a>
        <span>{currentItem.description}</span>
      </header>
      {#if current === 'users'}
        <div class="admin-panel"><Admin {notify} /></div>
      {:else if current === 'automations'}
        <div class="card handoff-card">
          <span class="handoff-icon"><Icon name="zap" size={20} /></span>
          <span class="handoff-copy">
            <h3>Build and manage filing rules</h3>
            <p>Automations have a dedicated workspace for ordering rules, editing triggers, and reviewing built-in preset behavior.</p>
          </span>
          <a role="button" class="btn primary handoff-action" href="#/automations">Open automations <Icon name="chev" size={13} /></a>
        </div>
      {:else}
        <div class="card section-card">
          <ConfigurationSection section={current} {notify} {onTaxonomyChanged} />
        </div>
      {/if}
    {/if}
  </div>
</section>

<style>
  .archive-settings { display: grid; grid-template-columns: 190px minmax(0, 1fr); gap: 22px; align-items: start; }
  .archive-rail { position: sticky; top: 0; display: flex; flex-direction: column; padding: 7px; border: 1px solid var(--line); border-radius: var(--r); background: var(--surface); }
  .archive-rail > span { margin: 9px 6px 2px; padding: 11px 3px 0; border-top: 1px solid var(--line); color: var(--faint); font-family: ui-monospace, monospace; font-size: .58rem; font-weight: 700; letter-spacing: .07em; line-height: 1.35; text-transform: uppercase; cursor: default; }
  .archive-rail a { padding: 7px 9px; border-radius: 7px; color: var(--muted); font-size: .78rem; text-decoration: none; }
  .archive-rail a:hover { background: var(--surface-2); color: var(--ink); }
  .archive-rail a.on { background: var(--tint); color: var(--accent); font-weight: 650; }
  .archive-content { min-width: 0; }
  .archive-intro { display: flex; align-items: flex-end; justify-content: space-between; gap: 20px; margin-bottom: 20px; }
  .eyebrow { display: block; margin-bottom: 5px; color: var(--accent); font-family: ui-monospace, monospace; font-size: .64rem; font-weight: 700; letter-spacing: .07em; text-transform: uppercase; }
  .archive-intro h2 { font-size: 1.35rem; line-height: 1.2; }
  .archive-intro p { max-width: 590px; margin: 6px 0 0; color: var(--muted); font-size: .82rem; }
  .admin-pill { display: inline-flex; align-items: center; gap: 6px; flex: none; padding: 5px 9px; border-radius: 99px; background: var(--surface-2); color: var(--muted); font-size: .68rem; font-weight: 600; }
  .configuration-groups { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; }
  .configuration-group { overflow: hidden; border: 1px solid var(--line); border-radius: var(--r); background: var(--surface); }
  .configuration-group > header { padding: 14px 15px 12px; border-top: 3px solid var(--accent); border-bottom: 1px solid var(--line); background: var(--bg); cursor: default; }
  .configuration-group h3 { color: var(--ink); font-size: .92rem; font-weight: 700; line-height: 1.2; }
  .configuration-group header p { margin: 4px 0 0; color: var(--muted); font-size: .72rem; line-height: 1.35; }
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
  .archive-note { margin: 16px 0 0; padding: 10px 12px; border-left: 3px solid var(--accent); background: var(--tint); color: var(--muted); font-size: .72rem; }
  .section-intro { display: flex; align-items: center; justify-content: space-between; gap: 14px; margin-bottom: 10px; }
  .section-intro a { display: inline-flex; align-items: center; gap: 4px; color: var(--accent); font-size: .76rem; font-weight: 600; text-decoration: none; }
  .section-intro > span { color: var(--muted); font-size: .72rem; text-align: right; }
  .section-card { min-height: 360px; }
  .admin-panel :global(.admin-tabs) { margin-bottom: 14px; }
  .handoff-card { display:grid;grid-template-columns:42px minmax(0,1fr) auto;gap:14px;align-items:center;min-height:112px }
  .handoff-icon { display:grid;place-items:center;width:42px;height:42px;border-radius:10px;background:var(--tint);color:var(--accent) }
  .handoff-copy h3 { margin:0;font-size:.92rem }
  .handoff-copy p { margin:4px 0 0;color:var(--muted);font-size:.78rem;line-height:1.45 }
  .handoff-action { white-space:nowrap;text-decoration:none }
  @media (max-width: 900px) {
    .archive-settings { grid-template-columns: 1fr; }
    .archive-rail { position: static; display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); }
    .archive-rail > span { grid-column: 1 / -1; }
  }
  @media (max-width: 680px) {
    .archive-rail { display:flex;flex-direction:row;gap:3px;overflow-x:auto;scrollbar-width:none }
    .archive-rail > span { display:none }
    .archive-rail a { flex:none;white-space:nowrap }
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
