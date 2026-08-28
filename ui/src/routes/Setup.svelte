<script>
  import { setupComplete } from '../lib/api.js'
  import { SETUP_STEPS } from '../lib/configuration.js'
  import ConfigurationSection from './ConfigurationSection.svelte'

  let { notify, onDone, onTaxonomyChanged } = $props()

  let current = $state('archive')
  let filingTreeChosen = $state(false)
  let busy = $state(false)
  let err = $state('')

  const currentIndex = $derived(SETUP_STEPS.findIndex((step) => step.name === current))

  function advance() {
    if (currentIndex < SETUP_STEPS.length - 1) current = SETUP_STEPS[currentIndex + 1].name
  }

  async function finish() {
    err = ''
    busy = true
    try {
      await setupComplete()
      notify?.('Setup complete — archive settings remain available in Settings')
      onDone?.()
    } catch (ex) {
      err = ex.message || 'Could not finish setup.'
    } finally { busy = false }
  }
</script>

<div class="wizard">
  <aside class="wiz-steps" aria-label="Setup steps">
    <div class="side-head" style="padding-left:0">Setup</div>
    {#each SETUP_STEPS as step}
      <button class="wiz-step" class:on={current === step.name} onclick={() => (current = step.name)}>
        <span class="dot" class:accent={current === step.name}></span>
        <span class="grow">{step.label}</span>
      </button>
    {/each}
    <button class="btn primary finish" onclick={finish} disabled={busy || !filingTreeChosen}
            title={filingTreeChosen ? '' : 'Choose a filing tree first'}>Finish setup</button>
    <p class="finish-copy">Choose a filing tree to finish setup. Every other step is optional and can be revisited later.</p>
  </aside>

  <div class="card wiz-body">
    {#if err}<div class="err">{err}</div>{/if}
    <ConfigurationSection
      section={current}
      {notify}
      setup
      onAdvance={advance}
      {onTaxonomyChanged}
      onFilingTreeChosen={(chosen) => { if (chosen) filingTreeChosen = true }}
    />
  </div>
</div>

<style>
  .wizard { display: grid; grid-template-columns: 240px minmax(0, 1fr); gap: 20px; align-items: start; max-width: 920px; }
  .wiz-steps { display: flex; flex-direction: column; gap: 2px; }
  .wiz-step { display: flex; align-items: center; gap: 10px; width: 100%; padding: 8px 10px; border: 0; border-radius: var(--r-sm); background: none; color: var(--muted); font-size: .88rem; text-align: left; }
  .wiz-step:hover { background: var(--surface-2); color: var(--ink); }
  .wiz-step.on { background: var(--tint); color: var(--ink); font-weight: 600; }
  .wiz-step .dot { width: 8px; height: 8px; flex: none; border-radius: 50%; background: var(--line-strong); }
  .wiz-step .dot.accent { background: var(--accent); }
  .wiz-step .grow { flex: 1; }
  .finish { justify-content: center; margin-top: 14px; }
  .finish-copy { margin: 8px 0 0; color: var(--faint); font-size: .72rem; }
  .wiz-body { min-height: 340px; }
  @media (max-width: 780px) {
    .wizard { grid-template-columns: 1fr; gap: 12px; }
    .wiz-steps { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 2px 6px; }
    .wiz-steps > .side-head, .wiz-steps > .finish { grid-column: 1 / -1; }
    .finish-copy { display: none; }
    .wiz-step { min-width: 0; padding: 7px 8px; font-size: .8rem; }
  }
</style>
