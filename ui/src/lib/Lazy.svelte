<script>
  // Awaits a dynamic import and mounts it; keyed on `load` so route
  // switches swap components. Failed fetch (stale deploy) -> reload link.
  let { load, props = {} } = $props()
  let C = $state(null)
  let failed = $state(false)
  $effect(() => {
    let live = true
    C = null; failed = false
    load().then(m => { if (live) C = m.default })
          .catch(() => { if (live) failed = true })
    return () => { live = false }
  })
</script>

{#if C}
  <C {...props} />
{:else if failed}
  <div class="empty">
    This view failed to load, likely because the server was updated.
    <a href={location.href} onclick={() => location.reload()}>Reload</a>
  </div>
{/if}
