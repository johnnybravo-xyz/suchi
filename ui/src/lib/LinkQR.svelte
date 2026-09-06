<script>
  let { url, label = 'Show QR code', open = $bindable(false) } = $props()
  let code = $state(null)
  let error = $state('')

  $effect(() => {
    if (!open) return
    const value = url
    let current = true
    code = null
    error = ''
    import('qrcode-generator').then(({ default: qrcode }) => {
      if (!current) return
      // URL serialization encodes Unicode without changing the encoder's globals.
      const link = new URL(value)
      if (!['http:', 'https:'].includes(link.protocol) || link.username || link.password) {
        throw new Error('Invalid link')
      }
      const qr = qrcode(0, 'M')
      qr.addData(link.href)
      qr.make()
      const count = qr.getModuleCount()
      let path = ''
      for (let row = 0; row < count; row++) {
        for (let column = 0; column < count; column++) {
          if (qr.isDark(row, column)) path += `M${column + 4},${row + 4}h1v1h-1z`
        }
      }
      code = { size: count + 8, path }
    }).catch(() => {
      if (current) error = 'QR code unavailable. Copy the link instead.'
    })
    return () => { current = false }
  })
</script>

<div class="link-qr">
  <button class="btn sm" aria-expanded={open} onclick={() => (open = !open)}>{open ? 'Hide QR code' : label}</button>
  {#if open}
    <div class="qr-result" aria-live="polite">
      {#if code}
        <svg viewBox={`0 0 ${code.size} ${code.size}`} role="img" aria-label="QR code for the displayed link" shape-rendering="crispEdges">
          <rect width={code.size} height={code.size} fill="white" />
          <path d={code.path} fill="black" />
        </svg>
      {:else}
        <p class="sub">{error || 'Preparing QR code…'}</p>
      {/if}
    </div>
  {/if}
</div>

<style>
  .link-qr { min-width: 0; }
  .qr-result { margin-top: 10px; }
  svg { display: block; width: 240px; max-width: 100%; height: auto; border-radius: 6px; }
</style>
