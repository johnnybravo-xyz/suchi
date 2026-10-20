<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "./i18n.svelte.js";
  let {
    url,
    label = translate("linkQR.showQrCode"),
    open = $bindable(false),
    allowedProtocols = ['http:', 'https:'],
  } = $props()
  let code = $state(null)
  let error = $state('')

  $effect(() => {
    if (!open) return
    const value = url
    const protocols = allowedProtocols
    let current = true
    code = null
    error = ''
    import('qrcode-generator').then(({ default: qrcode }) => {
      if (!current) return
      // URL serialization encodes Unicode without changing the encoder's globals.
      const link = new URL(value)
      if (!protocols.includes(link.protocol) || link.username || link.password) {
        throw new Error(translate("linkQR.invalidLink"))
      }
      const qr = qrcode(0, 'M')
      qr.addData(link.href)
      qr.make()
      const count = qr.getModuleCount()
      let path = ''
      for (let row = 0; row < count; row++) {
        for (let column = 0; column < count; column++) {
          if (qr.isDark(row, column)) path += translate("linkQR.mValueValueH1v1h1z", { "column": column + 4, "row": row + 4 })
        }
      }
      code = { size: count + 8, path }
    }).catch(() => {
      if (current) error = translate("linkQR.qrCodeUnavailableCopyTheLinkInstead")
    })
    return () => { current = false }
  })
</script>

<div class="link-qr">
  <button class="btn sm" aria-expanded={open} onclick={() => (open = !open)}>{open ? translate("linkQR.hideQrCode") : label}</button>
  {#if open}
    <div class="qr-result" aria-live="polite">
      {#if code}
        <svg viewBox={translate("linkQR.00ValueValue", { "size": code.size, "size2": code.size })} role="img" aria-label={translate("linkQR.qrCodeForTheDisplayedLink")} shape-rendering="crispEdges">
          <rect width={code.size} height={code.size} fill="white" />
          <path d={code.path} fill="black" />
        </svg>
      {:else}
        <p class="sub">{error || translate("linkQR.preparingQrCode")}</p>
      {/if}
    </div>
  {/if}
</div>

<style>
  .link-qr { min-width: 0; }
  .qr-result { margin-top: 10px; }
  svg { display: block; width: 240px; max-width: 100%; height: auto; margin-inline: auto; border-radius: 6px; }
</style>
