<script>
  import Icon from './Icon.svelte'

  let { state, expanded = false } = $props()
</script>

{#if state === 'decrypted'}
  <span class="unlock-status" role="img" aria-label="Password unlocked"
        title="Password unlocked — Suchi unlocked a working copy. The original remains password-protected.">
    <Icon name="unlock" size={15} />
    {#if expanded}<span aria-hidden="true">Password unlocked</span>{/if}
  </span>
{/if}

<style>
  .unlock-status { display:inline-flex; align-items:center; gap:6px; flex-shrink:0; color:var(--muted); font-size:.8rem; vertical-align:middle }
</style>
