<script>
  import { onDestroy } from 'svelte'
  import { importTaxonomy } from './api.js'
  import { systems, captureScope, scopeCurrent, refreshSystems, selectSystem } from './systems.svelte.js'
  import ConfirmDialog from './ConfirmDialog.svelte'

  let { notify, onApplied, busy = $bindable(false) } = $props()
  let content = $state('')
  let format = $state('huml')
  let fileName = $state('')
  let diff = $state(null)
  let collisions = $state([])
  let err = $state('')
  let reading = $state(false)
  let skipSeeds = $state(false)
  let remaps = $state({})
  let confirmBlank = $state(false)
  let firstDestination = $state(false)
  let separate = $state(false)
  let existingCode = $state('A00')
  const scope = captureScope()
  let generation = 0
  let reader
  let controller
  const canApply = $derived(!!diff?.state_hash && !diff.applied && !diff.collisions?.some(c => !c.resolved))

  function invalidate(newContent = false) {
    generation++
    reader?.abort()
    reader = null
    reading = false
    diff = null
    err = ''
    confirmBlank = false
    if (newContent) {
      collisions = []
      remaps = {}
      firstDestination = false
      separate = false
      existingCode = 'A00'
    }
  }

  function pick(event) {
    const file = event.currentTarget.files?.[0]
    event.currentTarget.value = ''
    if (!file) return
    invalidate(true)
    fileName = file.name
    content = ''
    const extension = file.name.split('.').pop()?.toLowerCase()
    if (!['huml', 'toml'].includes(extension)) {
      err = 'Choose a .huml or .toml file, or paste content and select its serialization.'
      return
    }
    format = extension
    const version = generation
    const nextReader = new FileReader()
    reader = nextReader
    reading = true
    nextReader.onload = () => {
      if (version !== generation) return
      content = String(nextReader.result || '')
      reading = false
      reader = null
    }
    nextReader.onerror = () => {
      if (version !== generation) return
      err = `Could not read ${file.name}. Choose the file again or paste its content.`
      reading = false
      reader = null
    }
    try { nextReader.readAsText(file) }
    catch { nextReader.onerror() }
  }

  async function run(apply) {
    if (busy || reading || !content.trim() || (apply && !canApply)) return
    const version = ++generation
    const body = { content, format, apply, skip_seeds: skipSeeds, remaps: { ...remaps } }
    if (separate && firstDestination) body.existing_system_code = existingCode
    if (apply) body.expected_state_hash = diff.state_hash
    busy = true
    err = ''
    controller = new AbortController()
    try {
      const result = await importTaxonomy(body, controller.signal)
      if (version !== generation) return
      diff = result
      if (!systems.introduced && result.systems_introduced) firstDestination = true
      collisions = result.collisions || []
      if (apply && result.applied) {
        confirmBlank = false
        notify?.(`Imported ${result.name}: content revision ${result.preset_version}.`)
        try {
          if (!result.system_code) await onApplied?.(result)
          if (!scopeCurrent(scope)) return
          await refreshSystems()
          if (scopeCurrent(scope) && result.system_code) selectSystem(result.system_code)
        } catch { err = 'Import succeeded, but refreshing the archive failed. Reload this page to see the current filing tree.' }
      }
    } catch (ex) {
      if (version !== generation) return
      diff = null
      confirmBlank = false
      err = ex.status === 409
        ? 'The preview is no longer current. Your input and choices are preserved. Preview again before applying.'
        : (ex.message || 'Could not validate the file. Try Preview again.')
    } finally {
      if (version === generation) busy = false
    }
  }

  function applyImport() {
    if (!canApply || busy) return
    if (diff.categories_incoming === 0) confirmBlank = true
    else run(true)
  }

  onDestroy(() => {
    generation++
    reader?.abort()
    controller?.abort()
    busy = false
  })
</script>

<section class="taximp" aria-label="Import taxonomy">
  <h3>Import taxonomy</h3>
  <div class="toolbar">
    <label class="btn sm">
      Choose .huml / .toml
      <input type="file" aria-label="Choose taxonomy file" accept=".huml,.toml" disabled={busy} onchange={pick} />
    </label>
    {#if fileName}<span class="file-name sub">{fileName}</span>{/if}
    <label class="serialization">Serialization
      <select class="input" bind:value={format} disabled={busy} onchange={() => invalidate()}>
        <option value="huml">HuML</option><option value="toml">TOML</option>
      </select>
    </label>
  </div>
  <label class="content-label" for="taxonomy-content">Paste content</label>
  <textarea id="taxonomy-content" class="input mono" rows="7" bind:value={content} disabled={busy}
            oninput={() => { fileName = ''; invalidate(true) }} placeholder="Paste a suchi-taxonomy/v1 file"></textarea>
  {#if reading}<p class="sub" role="status">Reading file…</p>{/if}
  <label class="wiz-check"><input type="checkbox" checked={!skipSeeds} disabled={busy}
    onchange={(event) => { skipSeeds = !event.currentTarget.checked; invalidate() }} /> Include starter rules</label>
  <p class="sub">Validation checks supported structure, not domain correctness or whether starter automations are trustworthy. Review the tree and rules before applying.</p>
  {#if firstDestination && !diff?.applied}
    <fieldset disabled={busy} style="margin:12px 0;padding:12px;border:1px solid var(--line)">
      <legend>First system import destination</legend>
      <label><input type="radio" name="first-destination" checked={!separate}
        onchange={() => { separate = false; invalidate() }} /> Use this archive: name its existing documents and filing tree with the imported code</label>
      <label style="display:block;margin-top:8px"><input type="radio" name="first-destination" checked={separate}
        onchange={() => { separate = true; invalidate() }} /> Create separately: preserve this archive under another code</label>
      {#if separate}
        <label>Existing archive code <input class="input mono" bind:value={existingCode} maxlength="3"
          pattern="[A-Z][0-9]{2}" required oninput={() => invalidate()} /></label>
      {/if}
      <p class="sub">Preview again after changing the destination. System codes are permanent once applied.</p>
    </fieldset>
  {/if}

  {#if err}<div class="err" role="alert">{err}</div>{/if}

  {#if diff}
    <div class="card preview" aria-label="Taxonomy preview">
      <h4>{diff.name} · {diff.preset_id} · content revision {diff.preset_version}</h4>
      <p>{diff.story}</p>
      <p class="sub">{diff.format} · {diff.mode === 'replace' ? 'Initial setup replacement' : 'Additive merge'}</p>
      {#if diff.system_code}
        <p><b>{diff.system_code} · {diff.system_name}</b> — {diff.system_created ? 'Create a new filing system' : 'Import into the existing archive'}.</p>
        {#if diff.systems_introduced}
          <p>{diff.existing_system_code ? `Existing documents, memberships and permissions stay in ${diff.existing_system_code}.` : `Existing documents keep their IDs, memberships and permissions in ${diff.system_code}.`} Full filing addresses and system-rooted rendered paths are introduced by this Apply.</p>
        {/if}
        {#if diff.system_created}<p>Initially only server administrators can enter the new system. Grant direct membership in Settings after import.</p>{/if}
        <p class="sub">Filing-index and render-only path refreshes are queued; import does not rerun document extraction or classification.</p>
      {/if}
      <div class="trees">
        <div>
          <h4>User areas and categories</h4>
          {#each diff.user_areas || [] as area (area.code)}
            <p><b>{area.code}–{area.code + 9} {area.name}</b></p>
            <ul>{#each area.categories || [] as category (category.code)}
              <li>{category.code} {category.name}{#if category.description}<span class="sub description">{category.description}</span>{/if}</li>
            {/each}</ul>
          {:else}<p>No user categories (blank tree).</p>{/each}
        </div>
        <div>
          <h4>Generated by Suchi</h4>
          <p><b>00–09 System index</b><span class="sub description">00.00 archive.huml is an on-disk filing map, not a filing destination or automation backup.</span></p>
          {#each diff.generated_areas || [] as area (area.code)}
            <p><b>{area.code}–{area.code + 9} {area.name}</b></p>
            <ul>{#each area.categories || [] as category (category.code)}<li>{category.code} {category.name}</li>{/each}</ul>
          {/each}
        </div>
      </div>
      <h4>{diff.applied ? 'Applied' : 'Planned changes'}</h4>
      <p>{diff.applied ? 'Added' : 'Adds'} {diff.categories_to_add?.length || 0} categories and {diff.rules_to_add?.length || 0} rules.</p>
      {#if diff.categories_to_add?.length}<p class="sub">Category codes: {diff.categories_to_add.join(', ')}</p>{/if}
      {#if diff.rules_to_add?.length}<h4>Rules to add</h4><ul>{#each diff.rules_to_add as name}<li>{name}</li>{/each}</ul>{/if}
      {#if diff.rules_preserved?.length}<h4>Existing rules preserved</h4><ul>{#each diff.rules_preserved as rule}<li>{rule.name} — {rule.enabled ? 'enabled' : 'disabled (stays disabled)'}</li>{/each}</ul>{/if}
      {#if diff.rules_skipped?.length}<h4>Rules skipped</h4><ul>{#each diff.rules_skipped as rule}<li>{rule.name} — {rule.reason}</li>{/each}</ul>{/if}
      <p class="sub">{skipSeeds ? 'Starter rules excluded.' : `Starter additions: ${diff.keywords_to_seed} keyword phrases · ${diff.automations_to_seed} explicit automations.`}</p>
      <p class="sub hash">Source SHA-256: <code>{diff.content_sha256}</code></p>
      {#if diff.applied && diff.index_refresh_pending}<p role="status">Filing-index refresh queued. The on-disk index updates when the server processes the job.</p>{/if}
    </div>
  {/if}

  {#if collisions.length && !diff?.applied}
    <div class="collisions" aria-label="Category collisions">
      <h4>Category collisions</h4>
      <p class="sub">Keep each existing category. Explicitly skip the incoming category or move it to an unused code in the same area. Skipping also skips dependent starter rules. Preview every changed choice.</p>
      {#each collisions as collision (collision.code)}
        <div class="collision">
          <p><b>{collision.code}</b> — existing: “{collision.existing}” · incoming: “{collision.incoming}”</p>
          <label>Choice for {collision.code}
            <select class="input" disabled={busy} value={remaps[collision.code] ?? ''}
              onchange={(event) => {
                const next = { ...remaps }
                if (event.currentTarget.value === '') delete next[collision.code]
                else next[collision.code] = Number(event.currentTarget.value)
                remaps = next
                invalidate()
              }}>
              <option value="">Choose a resolution</option>
              {#if collision.proposed_code}<option value={collision.proposed_code}>Move to {collision.proposed_code}</option>{/if}
              <option value={0}>Skip incoming</option>
            </select>
          </label>
        </div>
      {/each}
    </div>
  {/if}
  <div class="toolbar">
    <button class="btn primary sm" disabled={busy || reading || !content.trim()} onclick={() => run(false)}>{busy && !confirmBlank ? 'Working…' : 'Preview'}</button>
    <button class="btn sm" disabled={busy || !canApply} onclick={applyImport}>Apply import</button>
    {#if !diff && collisions.length}<span class="sub">Choices changed. Preview again to review their effects.</span>{/if}
  </div>
</section>

{#if confirmBlank}
  <ConfirmDialog title="Import a blank filing tree?"
    message="This file has no user categories. Documents remain in Suchi’s Inbox until you build categories. A merge keeps existing categories; it does not reset your archive."
    confirmLabel="Apply blank import" busyLabel="Applying…" {busy} onConfirm={() => run(true)} onCancel={() => { confirmBlank = false }} />
{/if}

<style>
  .taximp { min-width: 0; }
  .taximp h4 { margin: 12px 0 8px; }
  .taximp p, .taximp li { overflow-wrap: anywhere; }
  .taximp input[type=file] { width: 1px; height: 1px; position: absolute; opacity: 0; }
  .taximp label:focus-within { outline: 2px solid var(--accent); outline-offset: 3px; }
  .serialization { display: flex; align-items: center; gap: 8px; }
  .file-name { overflow-wrap: anywhere; min-width: 0; }
  .content-label { display: block; margin: 8px 0; }
  textarea { width: 100%; resize: vertical; font-size: .78rem; }
  .preview { margin: 12px 0; padding: 14px; }
  .trees { display: grid; grid-template-columns: 1fr 1fr; gap: 20px; }
  .trees > div { min-width: 0; }
  ul { padding-left: 20px; }
  .description { display: block; }
  .hash { overflow-wrap: anywhere; }
  .collision { padding: 8px 0; border-bottom: 1px solid var(--line); }
  .collision select { max-width: 100%; margin-top: 6px; }
  @media (max-width: 600px) { .trees { grid-template-columns: 1fr; gap: 0; } }
</style>
