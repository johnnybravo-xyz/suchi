<script>
  // Import a suchi-taxonomy/v1 file. The server chooses replace or merge.
  import { importTaxonomy } from './api.js'
  import Icon from './Icon.svelte'

  let { notify, onApplied } = $props()
  let content = $state('')
  let fileName = $state('')
  let diff = $state(null)
  let err = $state('')
  let busy = $state(false)
  let skipSeeds = $state(false)
  let remaps = $state({})

  function resetResult() {
    diff = null
    err = ''
    remaps = {}
  }

  function setDiff(res) {
    diff = res
    const next = { ...remaps }
    for (const collision of res?.collisions || []) {
      if (next[collision.code] === undefined) next[collision.code] = collision.proposed_code || 0
    }
    remaps = next
  }

  function pick(e) {
    const f = e.target.files?.[0]
    if (!f) return
    fileName = f.name
    const r = new FileReader()
    r.onload = () => { content = String(r.result || ''); resetResult() }
    r.readAsText(f)
  }

  async function run(apply) {
    busy = true; err = ''
    try {
      const body = { content, apply, skip_seeds: skipSeeds }
      if (apply && diff?.mode === 'merge') body.remaps = remaps
      const res = await importTaxonomy(body)
      setDiff(res)
      if (apply && res?.applied) {
        notify?.(`Taxonomy applied: ${res.preset_id}@${res.preset_version}`)
        onApplied?.(res)
      }
    } catch (ex) {
      if (ex.status === 409 && ex.data?.collisions) {
        setDiff(ex.data)
        err = 'Resolve each taxonomy collision, then apply again.'
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
      <input type="file" accept=".huml,.toml,text/plain" hidden onchange={pick} />
    </label>
    {#if fileName}<span class="pill">{fileName}</span>{/if}
    <span class="sub" style="font-size:.76rem;color:var(--faint)">HuML preferred · TOML accepted</span>
  </div>
  <textarea class="input mono" rows="7" style="width:100%;font-size:.74rem;resize:vertical"
            placeholder={'format: "suchi-taxonomy/v1"\nid: "my-index"\n...paste a preset file, or choose one above'}
            bind:value={content} oninput={resetResult}></textarea>

  <div class="toolbar" style="margin:10px 0 0">
    <button class="btn primary sm" disabled={busy || !content.trim()} onclick={() => run(false)}>Dry run</button>
    {#if diff && !diff.applied}
      <button class="btn sm" disabled={busy} onclick={() => run(true)}>
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
              <select class="input" style="width:auto;font-size:.78rem"
                      value={remaps[c.code] ?? c.proposed_code ?? 0}
                      onchange={(e) => { remaps = { ...remaps, [c.code]: Number(e.currentTarget.value) } }}>
                {#if c.proposed_code}<option value={c.proposed_code}>Move to {c.proposed_code}</option>{/if}
                <option value="0">Skip incoming</option>
              </select>
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
