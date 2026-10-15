<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { onDestroy, onMount } from 'svelte'
  import pdfWorkerURL from 'pdfjs-dist/legacy/build/pdf.worker.min.mjs?url'
  import { t as translate } from './i18n.svelte.js'

  let { src, downloadSrc, title } = $props()

  let canvas = $state()
  let viewportElement = $state()
  let state = $state('loading')
  let rendering = $state(false)
  let pageNumber = $state(1)
  let pageCount = $state(0)
  let zoom = $state(1)

  let pdfDocument
  let loadingTask
  let renderTask
  let resizeObserver
  let resizeFrame = 0
  let loadVersion = 0
  let renderVersion = 0

  const clamp = (value, minimum, maximum) => Math.min(maximum, Math.max(minimum, value))

  function pdfAssetURL(directory) {
    return new URL(`${import.meta.env.BASE_URL}assets/pdfjs/${directory}/`, document.baseURI).href
  }

  function releaseDocument() {
    renderVersion++
    renderTask?.cancel()
    renderTask = null
    const task = loadingTask
    loadingTask = null
    pdfDocument = null
    void task?.destroy()
  }

  async function load(source) {
    const version = ++loadVersion
    releaseDocument()
    state = 'loading'
    rendering = false
    pageNumber = 1
    pageCount = 0
    zoom = 1

    try {
      const pdfjs = await import('pdfjs-dist/legacy/build/pdf.mjs')
      if (version !== loadVersion) return
      pdfjs.GlobalWorkerOptions.workerSrc = pdfWorkerURL
      const task = pdfjs.getDocument({
        url: source,
        withCredentials: true,
        cMapUrl: pdfAssetURL('cmaps'),
        cMapPacked: true,
        iccUrl: pdfAssetURL('iccs'),
        standardFontDataUrl: pdfAssetURL('standard_fonts'),
        wasmUrl: pdfAssetURL('wasm'),
        useWasm: false,
        enableXfa: false,
        isEvalSupported: false,
        maxImageSize: 16_000_000,
        canvasMaxAreaInBytes: 64 * 1024 * 1024,
      })
      loadingTask = task
      const loaded = await task.promise
      if (version !== loadVersion) {
        void loaded.destroy()
        return
      }
      pdfDocument = loaded
      pageCount = loaded.numPages
      state = 'ready'
      await renderCurrentPage()
    } catch {
      if (version === loadVersion) state = 'error'
    }
  }

  async function renderCurrentPage() {
    if (!pdfDocument || !canvas || !viewportElement || state !== 'ready') return
    const version = ++renderVersion
    renderTask?.cancel()
    renderTask = null
    rendering = true
    let page
    try {
      page = await pdfDocument.getPage(pageNumber)
      if (version !== renderVersion) return
      const unscaled = page.getViewport({ scale: 1 })
      const availableWidth = Math.max(1, viewportElement.clientWidth - 32)
      const scale = clamp((availableWidth / unscaled.width) * zoom, 0.1, 4)
      const viewport = page.getViewport({ scale })
      const outputScale = Math.min(window.devicePixelRatio || 1, 2)
      const context = canvas.getContext('2d', { alpha: false })
      canvas.width = Math.ceil(viewport.width * outputScale)
      canvas.height = Math.ceil(viewport.height * outputScale)
      canvas.style.width = `${Math.ceil(viewport.width)}px`
      canvas.style.height = `${Math.ceil(viewport.height)}px`
      renderTask = page.render({
        canvasContext: context,
        viewport,
        transform: outputScale === 1 ? undefined : [outputScale, 0, 0, outputScale, 0, 0],
      })
      await renderTask.promise
    } catch (error) {
      if (version === renderVersion && error?.name !== 'RenderingCancelledException') state = 'error'
    } finally {
      page?.cleanup()
      if (version === renderVersion) rendering = false
    }
  }

  function scheduleRender() {
    if (state !== 'ready') return
    cancelAnimationFrame(resizeFrame)
    resizeFrame = requestAnimationFrame(() => void renderCurrentPage())
  }

  function showPage(nextPage) {
    const next = clamp(nextPage, 1, pageCount)
    if (next === pageNumber) return
    pageNumber = next
    void renderCurrentPage()
  }

  function setZoom(nextZoom) {
    const next = clamp(nextZoom, 0.5, 3)
    if (next === zoom) return
    zoom = next
    void renderCurrentPage()
  }

  function retry() {
    void load(src)
  }

  $effect(() => {
    void load(src)
  })

  onMount(() => {
    resizeObserver = new ResizeObserver(scheduleRender)
    resizeObserver.observe(viewportElement)
  })

  onDestroy(() => {
    loadVersion++
    cancelAnimationFrame(resizeFrame)
    resizeObserver?.disconnect()
    releaseDocument()
  })
</script>

<div class="pdf-preview">
  {#if state === 'ready'}
    <div class="pdf-toolbar" role="toolbar" aria-label={translate('documentDetail.pdfControls')}>
      <button type="button" class="pdf-control" aria-label={translate('documentDetail.previous')}
              disabled={pageNumber <= 1 || rendering} onclick={() => showPage(pageNumber - 1)}>‹</button>
      <span class="page-status" aria-live="polite">
        {translate('documentDetail.pdfPageValueOfValue', { page: pageNumber, pageCount })}
      </span>
      <button type="button" class="pdf-control" aria-label={translate('documentDetail.next')}
              disabled={pageNumber >= pageCount || rendering} onclick={() => showPage(pageNumber + 1)}>›</button>
      <span class="toolbar-divider" aria-hidden="true"></span>
      <button type="button" class="pdf-control" aria-label={translate('documentDetail.zoomOut')}
              disabled={zoom <= 0.5 || rendering} onclick={() => setZoom(zoom - 0.25)}>−</button>
      <button type="button" class="pdf-control fit" disabled={zoom === 1 || rendering}
              onclick={() => setZoom(1)}>{translate('documentDetail.fitWidth')}</button>
      <button type="button" class="pdf-control" aria-label={translate('documentDetail.zoomIn')}
              disabled={zoom >= 3 || rendering} onclick={() => setZoom(zoom + 0.25)}>+</button>
    </div>
  {/if}

  <div class="pdf-viewport" bind:this={viewportElement}>
    <canvas bind:this={canvas} aria-label={title} hidden={state !== 'ready'}></canvas>
    {#if state === 'loading'}
      <div class="pdf-message" role="status">{translate('documentDetail.pdfLoading')}</div>
    {:else if state === 'error'}
      <div class="pdf-message" role="alert">
        <b>{translate('documentDetail.pdfPreviewUnavailable')}</b>
        <div class="message-actions">
          <button type="button" class="pdf-control fit" onclick={retry}>{translate('common.retry')}</button>
          <a class="pdf-control fit" href={downloadSrc} download>{translate('documentDetail.downloadOriginal')}</a>
        </div>
      </div>
    {:else if rendering}
      <div class="rendering" role="status" aria-label={translate('documentDetail.pdfRendering')}></div>
    {/if}
  </div>
</div>

<style>
  .pdf-preview {
    display: flex;
    flex-direction: column;
    width: 100%;
    height: 78vh;
    min-height: 360px;
    overflow: hidden;
    background: #2b2e32;
  }
  .pdf-toolbar {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 6px;
    min-height: 44px;
    padding: 6px 10px;
    color: var(--ink);
    background: var(--surface);
    border-bottom: 1px solid var(--line-strong);
  }
  .pdf-control {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    min-width: 32px;
    height: 32px;
    padding: 0 9px;
    color: var(--ink);
    background: var(--surface-2);
    border: 1px solid var(--line-strong);
    border-radius: 7px;
    font-weight: 700;
    line-height: 1;
    text-decoration: none;
  }
  .pdf-control:hover:not(:disabled) { border-color: var(--accent); }
  .pdf-control:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  .pdf-control:disabled { cursor: default; opacity: .42; }
  .pdf-control.fit { min-width: auto; font-size: .8rem; }
  .page-status { min-width: 7.5rem; text-align: center; font-size: .82rem; font-weight: 700; }
  .toolbar-divider { width: 1px; height: 22px; margin: 0 3px; background: var(--line-strong); }
  .pdf-viewport {
    position: relative;
    flex: 1;
    min-height: 0;
    overflow: auto;
    padding: 16px;
    text-align: center;
  }
  canvas {
    display: block;
    max-width: none;
    margin: 0 auto;
    background: #fff;
    box-shadow: 0 4px 18px rgba(0, 0, 0, .32);
  }
  .pdf-message {
    position: absolute;
    inset: 0;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: 14px;
    padding: 24px;
    color: #f3f2ed;
    text-align: center;
  }
  .message-actions { display: flex; flex-wrap: wrap; justify-content: center; gap: 8px; }
  .rendering {
    position: absolute;
    top: 12px;
    right: 12px;
    width: 10px;
    height: 10px;
    border-radius: 50%;
    background: var(--accent);
    box-shadow: 0 0 0 5px rgba(79, 168, 220, .2);
  }
  @media (min-width: 861px) {
    .pdf-preview { flex: 1; height: 100%; min-height: 0; }
  }
  @media (max-width: 520px) {
    .pdf-toolbar { justify-content: flex-start; overflow-x: auto; }
    .page-status { min-width: 6.5rem; }
  }
</style>
