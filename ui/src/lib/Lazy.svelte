<script>
  // Route-level code splitting. Each rarely-visited route ships as its
  // own chunk; this shim awaits the dynamic import and mounts it.
  // Keyed on `load` so switching routes swaps components; a failed
  // chunk fetch (stale deploy) gets a reload affordance instead of a
  // blank pane.
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
