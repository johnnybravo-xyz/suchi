<script>
  // Import a suchi-taxonomy/v1 file (HuML primary, TOML, YAML legacy).
  // Dry run by default; the server decides replace vs merge from the
  // archive's state. Merge apply currently 501s (merge_not_wired)
  // until the server wires the remap flow; this UI degrades honestly.
  import { importTaxonomy } from './api.js'
  import Icon from './Icon.svelte'

  let { notify, onApplied } = $props()
  let content = $state('')
  let fileName = $state('')
  let diff = $state(null)
  let err = $state('')
  let busy = $state(false)
  let skipSeeds = $state(false)

  function pick(e) {
    const f = e.target.files?.[0]
    if (!f) return
    fileName = f.name
    const r = new FileReader()
    r.onload = () => { content = String(r.result || ''); diff = null; err = '' }
    r.readAsText(f)
  }

  async function run(apply) {
    busy = true; err = ''
    try {
      const res = await importTaxonomy({ content, apply, skip_seeds: skipSeeds })
      diff = res
      if (apply && res?.applied) {
        notify?.(`Taxonomy applied: ${res.preset_id}@${res.preset_version}`)
        onApplied?.(res)
      }
    } catch (ex) {
      if (ex.status === 501 || ex.code === 'merge_not_wired') {
        err = 'This archive has documents, so the import is a merge. The server has the dry run ready but merge apply is not wired yet; collisions below are what it will ask about.'
      } else {
        err = ex.message || 'The file did not validate.'
        diff = null
      }
    } finally { busy = false }
  }
</script>

<div class="taximp">
  <div class="toolbar" style="margin:0 0 8px">
    <label class="btn sm" style="cursor:pointer">
      <Icon name="upload" size={13} /> Choose file
      <input type="file" accept=".huml,.toml,.yaml,.yml,text/plain" hidden onchange={pick} />
    </label>
    {#if fileName}<span class="pill">{fileName}</span>{/if}
    <span class="sub" style="font-size:.76rem;color:var(--faint)">HuML preferred · TOML accepted · YAML legacy</span>
  </div>
  <textarea class="input mono" rows="7" style="width:100%;font-size:.74rem;resize:vertical"
            placeholder={'format: "suchi-taxonomy/v1"\nid: "my-index"\n...paste a preset file, or choose one above'}
            bind:value={content} oninput={() => { diff = null; err = '' }}></textarea>

  <div class="toolbar" style="margin:10px 0 0">
    <button class="btn primary sm" disabled={busy || !content.trim()} onclick={() => run(false)}>Dry run</button>
    {#if diff && !diff.applied}
      <button class="btn sm" disabled={busy || (diff.mode === 'merge' && diff.collisions?.length > 0)}
              onclick={() => run(true)} title={diff.mode === 'merge' && diff.collisions?.length ? 'Resolve collisions first (server support pending)' : ''}>
        Apply {diff.mode}
      </button>
    {/if}
    <label class="wiz-check" style="margin:0;font-size:.8rem"><input type="checkbox" bind:checked={skipSeeds} />
      Tree only, skip rule and automation seeds</label>
  </div>

  {#if err}<div class="err" style="margin-top:10px">{err}</div>{/if}

  {#if diff}
    <div class="card" style="margin-top:12px;padding:14px 16px">
      <div class="toolbar" style="margin:0 0 8px">
        <span class="chip">{diff.preset_id}@{diff.preset_version}</span>
        <span class="pill" class:warn={diff.mode === 'merge'}>{diff.mode}</span>
        <span class="sub">{diff.areas_incoming} areas · {diff.categories_incoming} categories</span>
      </div>
      {#if diff.categories_to_add?.length}
        <p class="sub" style="margin:0 0 6px;font-size:.82rem">
          Will add {diff.categories_to_add.length} categor{diff.categories_to_add.length === 1 ? 'y' : 'ies'}:
          {#each diff.categories_to_add as c}<span class="chip" style="margin:0 3px 3px 0">{c}</span>{/each}
        </p>
      {/if}
      {#if diff.collisions?.length}
        <div class="index" style="border:0;margin-top:6px">
          {#each diff.collisions as c (c.code)}
            <div class="irow" style="padding:7px 2px">
              <span class="dot warn"></span>
              <span class="chip">{c.code}</span>
              <span class="title grow" style="font-size:.84rem">
                yours: “{c.existing}” · incoming: “{c.incoming}”
              </span>
              <span class="pill warn" title="Skip or remap lands with the server's merge apply">collision</span>
            </div>
          {/each}
        </div>
      {/if}
      <p class="sub" style="margin:8px 0 0;font-size:.76rem;color:var(--faint)">
        {skipSeeds ? 'Seeds skipped.' : `Seeds: ${diff.keywords_to_seed} keyword rules · ${diff.automations_to_seed} automations.`}
        sha256 {diff.content_sha256?.slice(0, 12)}…
      </p>
    </div>
  {/if}
</div>
