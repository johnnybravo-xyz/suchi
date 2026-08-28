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
{:else}
  <div class="route-loading" aria-label="Loading view">
    <div class="skel"></div>
    <div class="skel"></div>
    <div class="skel"></div>
  </div>
{/if}

<style>
  .route-loading { display:grid;gap:12px;width:min(100%,760px);padding:8px 0; }
  .route-loading .skel:nth-child(1) { width:34%;height:16px; }
  .route-loading .skel:nth-child(2) { width:100%; }
  .route-loading .skel:nth-child(3) { width:72%; }
</style>
