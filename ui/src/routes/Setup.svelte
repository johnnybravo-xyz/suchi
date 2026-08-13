<script>
  import { setupState, setupStep, setupComplete, adminCreateUser, applyJDPreset,
           saveLLMSettings, savePreferences, saveIngestSettings, listJDPresets } from '../lib/api.js'
  import { go } from '../lib/router.svelte.js'
  import Icon from '../lib/Icon.svelte'
  import MailForm from '../lib/MailForm.svelte'
  import TaxonomyImport from '../lib/TaxonomyImport.svelte'

  let { notify, onDone } = $props()

  const STEPS = [
    { name: 'welcome',     label: 'Welcome' },
    { name: 'users',       label: 'People' },
    { name: 'jd',          label: 'Filing tree' },
    { name: 'sources',     label: 'Ingest sources' },
    { name: 'mail',        label: 'Email intake' },
    { name: 'llm',         label: 'Classification' },
    { name: 'rules',       label: 'Rules' },
    { name: 'preferences', label: 'OCR & backups' },
  ]
  // Server is the source of truth (GET /api/jd/presets/); this list is
  // only the offline fallback so the step never renders empty.
  const FALLBACK_PRESETS = [
    { id: 'solo', name: 'Solo', description: 'One person: life admin, money, health, home.', areas: [] },
    { id: 'household', name: 'Household', description: 'A family: shared areas plus per-person categories.', areas: [] },
    { id: 'freelance', name: 'Freelance', description: 'Clients, invoicing, taxes, contracts.', areas: [] },
    { id: 'smb_billing', name: 'Small business', description: 'AP/AR heavy: vendors, invoices, compliance.', areas: [] },
    { id: 'blank', name: 'Blank', description: 'No tree. Build your own from scratch.', blank: true, areas: [] },
  ]
  let presets = $state(FALLBACK_PRESETS)
  listJDPresets().then(r => { const rows = r?.results || r || []; if (rows.length) presets = rows }).catch(() => {})

  let steps = $state({})          // name -> 'done' | 'skipped'
  let cur = $state('welcome')
  let busy = $state(false)
  let err = $state('')

  // step-local form state
  let user = $state({ email: '', password: '', display_name: '', role: 'member' })
  let preset = $state({ preset_id: 'solo', confirm_blank: false, refile: false })
  let jdTab = $state('presets')
  let llm = $state({ endpoint_url: '', model: '', api_key: '', egress_ack: false })
  let prefs = $state({ backup_interval_hours: 24, ocr_languages: 'eng' })
  let ingest = $state({ fs_watch_dir: '', fs_watch_owner_email: '' })

  setupState().then(st => { steps = st?.steps || {} }).catch(() => {})

  const idx = $derived(STEPS.findIndex(s => s.name === cur))
  const doneCount = $derived(Object.values(steps).filter(v => v === 'done' || v === 'skipped').length)

  async function mark(status) {
    err = ''
    try {
      await setupStep(cur, status)
      steps = { ...steps, [cur]: status }
      if (idx < STEPS.length - 1) cur = STEPS[idx + 1].name
    } catch (ex) { err = ex.message || 'Could not record the step.' }
  }

  async function saveAnd(fn, label) {
    err = ''; busy = true
    try { await fn(); notify?.(label); await mark('done') }
    catch (ex) { err = ex.message || 'The server rejected that.' }
    finally { busy = false }
  }

  async function finish() {
    err = ''; busy = true
    try {
      await setupComplete()
      notify?.('Setup complete — you can revisit any step from Settings')
      onDone?.()
    } catch (ex) { err = ex.message || 'Could not finish setup.' }
    finally { busy = false }
  }

  const llmIsRemote = $derived((() => {
    try {
      const h = new URL(llm.endpoint_url).hostname
      return !['localhost', '127.0.0.1', '::1', ''].includes(h)
    } catch { return false }
  })())
</script>

<div class="wizard">
  <aside class="wiz-steps">
    <div class="side-head" style="padding-left:0">Setup · {doneCount}/{STEPS.length}</div>
    {#each STEPS as s, i}
      <button class="wiz-step" class:on={cur === s.name} onclick={() => (cur = s.name)}>
        <span class="dot" class:ok={steps[s.name] === 'done'}
              class:warn={steps[s.name] === 'skipped'}
              class:accent={cur === s.name && !steps[s.name]}></span>
        <span class="grow">{s.label}</span>
        {#if steps[s.name] === 'skipped'}<span class="sub">skipped</span>{/if}
      </button>
    {/each}
    <button class="btn primary" style="margin-top:14px;justify-content:center" onclick={finish} disabled={busy}>
      Finish setup
    </button>
    <p class="sub" style="font-size:.72rem;color:var(--faint);margin-top:8px">
      Every step is optional. Nothing is blocked while this is open, and every step can be revisited later.
    </p>
  </aside>

  <div class="card wiz-body">
    {#if err}<div class="err">{err}</div>{/if}

    {#if cur === 'welcome'}
      <h3>Welcome to suchi</h3>
      <p class="wiz-p">This walkthrough sets up the parts worth deciding early: who can sign in, how documents get filed, where they come from, and what happens to them on arrival. Skip anything — the defaults are safe, and each panel exists in Settings afterwards.</p>
      <div class="toolbar"><button class="btn primary sm" onclick={() => mark('done')}>Start</button></div>

    {:else if cur === 'users'}
      <h3>Add another person</h3>
      <p class="wiz-p">You already have the admin account you signed in with. Add family members or teammates here; each gets their own documents and their own inbox.</p>
      <div class="field"><label for="u-email">Email</label><input id="u-email" class="input" type="email" bind:value={user.email} /></div>
      <div class="field"><label for="u-name">Display name</label><input id="u-name" class="input" bind:value={user.display_name} /></div>
      <div class="field"><label for="u-pw">Password</label><input id="u-pw" class="input" type="password" bind:value={user.password} autocomplete="new-password" /></div>
      <div class="field"><label for="u-role">Role</label>
        <select id="u-role" class="input" bind:value={user.role}>
          <option value="member">Member</option><option value="admin">Admin</option>
        </select>
      </div>
      <div class="toolbar">
        <button class="btn primary sm" disabled={busy || !user.email || !user.password}
                onclick={() => saveAnd(() => adminCreateUser(user), 'User created')}>Create user</button>
        <button class="btn sm" onclick={() => mark('skipped')}>Just me for now</button>
      </div>

    {:else if cur === 'jd'}
      <h3>Pick a filing tree</h3>
      <p class="wiz-p">Johnny.Decimal areas and categories, tailored to how you'll use the archive. You can always switch later — refile moves every document to the closest match in the new tree.</p>
      <span class="seg" style="margin-bottom:14px">
        <button class:on={jdTab === 'presets'} onclick={() => (jdTab = 'presets')}>Presets</button>
        <button class:on={jdTab === 'import'} onclick={() => (jdTab = 'import')}>Import a file</button>
      </span>
      {#if jdTab === 'import'}
        <TaxonomyImport {notify} onApplied={() => mark('done')} />
        <div class="toolbar" style="margin-top:10px">
          <button class="btn sm" onclick={() => mark('skipped')}>Keep the current tree</button>
        </div>
      {:else}
      <div class="preset-grid">
        {#each presets as p (p.id)}
          <label class="preset" class:on={preset.preset_id === p.id}>
            <input type="radio" bind:group={preset.preset_id} value={p.id} hidden />
            <b>{p.name}</b><span class="sub">{p.description}</span>
            {#if p.areas?.length}
              <span class="preset-tree">
                {#each p.areas as a}<span class="chip" title={`${a.category_count} categories`}>{a.code}–{a.code + 9} {a.name}</span>{/each}
              </span>
            {/if}
          </label>
        {/each}
      </div>
      {#if presets.find(p => p.id === preset.preset_id)?.blank || preset.preset_id === 'blank'}
        <label class="wiz-check"><input type="checkbox" bind:checked={preset.confirm_blank} />
          I understand documents will pile up in the inbox until I build categories.</label>
      {/if}
      <label class="wiz-check"><input type="checkbox" bind:checked={preset.refile} />
        Refile existing documents into the new tree now.</label>
      <div class="toolbar">
        <button class="btn primary sm" disabled={busy || (preset.preset_id === 'blank' && !preset.confirm_blank)}
                onclick={() => saveAnd(() => applyJDPreset(preset), 'Filing tree applied')}>Apply preset</button>
        <button class="btn sm" onclick={() => mark('skipped')}>Keep the current tree</button>
      </div>
      {/if}

    {:else if cur === 'sources'}
      <h3>Ingest sources</h3>
      <p class="wiz-p">Point suchi at a folder (a scanner target, a synced directory) and everything dropped there becomes a document. Uploads and the API work regardless.</p>
      <div class="field"><label for="i-dir">Watched directory (on the server)</label>
        <input id="i-dir" class="input mono" placeholder="/data/staging" bind:value={ingest.fs_watch_dir} /></div>
      <div class="field"><label for="i-owner">Documents from it belong to</label>
        <input id="i-owner" class="input" type="email" placeholder="owner email" bind:value={ingest.fs_watch_owner_email} /></div>
      <div class="toolbar">
        <button class="btn primary sm" disabled={busy || !ingest.fs_watch_dir}
                onclick={() => saveAnd(() => saveIngestSettings(ingest), 'Ingest source saved')}>Save source</button>
        <button class="btn sm" onclick={() => mark('skipped')}>Uploads only</button>
      </div>

    {:else if cur === 'mail'}
      <h3>Email intake</h3>
      <p class="wiz-p">Point suchi at a mailbox and forwarded documents file themselves. Credentials stay server-side; the password field never reads back.</p>
      <MailForm {notify} onSaved={() => mark('done')} />
      <div class="toolbar" style="margin-top:12px">
        <button class="btn sm" onclick={() => mark('skipped')}>Skip for now</button>
      </div>

    {:else if cur === 'llm'}
      <h3>Classification model</h3>
      <p class="wiz-p">The rules engine works with no model at all. Add any OpenAI-compatible endpoint — a local Ollama keeps everything on your hardware — and low-confidence documents get a second opinion.</p>
      <div class="field"><label for="l-url">Endpoint URL</label>
        <input id="l-url" class="input mono" placeholder="http://localhost:11434/v1" bind:value={llm.endpoint_url} /></div>
      <div class="field"><label for="l-model">Model</label>
        <input id="l-model" class="input mono" placeholder="qwen2.5:7b" bind:value={llm.model} /></div>
      <div class="field"><label for="l-key">API key (blank for local)</label>
        <input id="l-key" class="input mono" type="password" bind:value={llm.api_key} autocomplete="off" /></div>
      {#if llmIsRemote}
        <label class="wiz-check attn"><input type="checkbox" bind:checked={llm.egress_ack} />
          This endpoint is not local. I acknowledge document text will leave this machine.</label>
      {/if}
      <div class="toolbar">
        <button class="btn primary sm" disabled={busy || !llm.endpoint_url || (llmIsRemote && !llm.egress_ack)}
                onclick={() => saveAnd(() => saveLLMSettings(llm), 'Classifier configured')}>Save classifier</button>
        <button class="btn sm" onclick={() => mark('skipped')}>Rules only</button>
      </div>

    {:else if cur === 'rules'}
      <h3>Rules</h3>
      <p class="wiz-p">Your preset already ships a starter set of filing rules and automations — the ones under the "Owned by <em>&lt;preset&gt;</em> filing tree" pill. Editing or disabling any of them forks a user-owned copy; re-picking the same preset never touches your edits.</p>
      <p class="wiz-p">Full editor for both engines lives in <a href="#/automations">Automations</a>; nothing more to decide here during setup.</p>
      <div class="toolbar">
        <a role="button" class="btn primary sm" href="#/automations">Open automations</a>
        <button class="btn sm" onclick={() => mark('done')}>Done</button>
        <button class="btn sm" onclick={() => mark('skipped')}>Skip</button>
      </div>

    {:else if cur === 'preferences'}
      <h3>OCR & backups</h3>
      <div class="field"><label for="p-ocr">OCR languages (comma-separated tesseract codes)</label>
        <input id="p-ocr" class="input mono" placeholder="eng,hin,nep" bind:value={prefs.ocr_languages} /></div>
      <div class="field"><label for="p-bk">Database snapshot interval (hours, 0 disables)</label>
        <input id="p-bk" class="input" type="number" min="0" bind:value={prefs.backup_interval_hours} /></div>
      <p class="wiz-p sub" style="font-size:.8rem">Snapshots cover the database; blobs are plain files — point restic or borg at the data directory for the full story.</p>
      <div class="toolbar">
        <button class="btn primary sm" disabled={busy}
                onclick={() => saveAnd(() => savePreferences({
                  backup_interval_hours: Number(prefs.backup_interval_hours) || 0,
                  ocr_languages: prefs.ocr_languages.split(',').map(x => x.trim()).filter(Boolean),
                }), 'Preferences saved')}>Save preferences</button>
        <button class="btn sm" onclick={() => mark('skipped')}>Defaults are fine</button>
      </div>
    {/if}
  </div>
</div>

<style>
  .wizard { display: grid; grid-template-columns: 240px 1fr; gap: 20px; align-items: start; max-width: 920px; }
  @media (max-width: 780px) { .wizard { grid-template-columns: 1fr; } }
  .wiz-steps { display: flex; flex-direction: column; gap: 2px; }
  .wiz-step {
    display: flex; align-items: center; gap: 10px; width: 100%;
    padding: 8px 10px; border: 0; background: none; border-radius: var(--r-sm);
    font-size: .88rem; text-align: left; color: var(--muted);
  }
  .wiz-step:hover { background: var(--surface-2); color: var(--ink); }
  .wiz-step.on { background: var(--tint); color: var(--ink); font-weight: 600; }
  .wiz-step .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--line-strong); flex: none; }
  .wiz-step .dot.ok { background: var(--ok); }
  .wiz-step .dot.warn { background: var(--warn); }
  .wiz-step .dot.accent { background: var(--accent); }
  .wiz-step .grow { flex: 1; }
  .wiz-step .sub { font-size: .68rem; color: var(--faint); }
  .wiz-body { min-height: 340px; }
  .wiz-p { color: var(--muted); font-size: .92rem; margin: 6px 0 16px; max-width: 46em; }
  .wiz-check { display: flex; gap: 9px; align-items: baseline; font-size: .86rem; color: var(--muted); margin: 0 0 14px; }
  .wiz-check.attn { color: var(--warn); }
  .preset-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; margin-bottom: 14px; }
  @media (max-width: 640px) { .preset-grid { grid-template-columns: 1fr; } }
  .preset {
    display: flex; flex-direction: column; gap: 3px; cursor: pointer;
    border: 1px solid var(--line-strong); border-radius: var(--r-sm); padding: 12px 14px;
  }
  .preset .sub { font-size: .78rem; color: var(--muted); }
  .preset-tree { display: flex; flex-wrap: wrap; gap: 5px; margin-top: 7px; }
  .preset.on { border-color: var(--accent); background: var(--tint); }
</style>
