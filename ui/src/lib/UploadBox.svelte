<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "./i18n.svelte.js";
  import { captureScope, scopeCurrent, scopedHash as filingHref } from './systems.svelte.js'
  import { onMount, onDestroy } from 'svelte'
  import { session } from './session.svelte.js'
  import { uploadDocument, getDocument, listDocuments, listTasks } from './api.js'
  import { fmtBytes, plural } from './format.js'
  import { markUploaded } from './upload_bus.svelte.js'
  import Icon from './Icon.svelte'

  const CHECK_DELAY = 2500
  const MAX_CHECKS = 40
  const CHECK_CONCURRENCY = 4

  let { initialFiles = [], onInitialFilesConsumed } = $props()
  let over = $state(false)
  let queue = $state([])
  let previews = $state([])
  let surface
  let fileInput
  let mounted = true
  let workerRunning = false
  let pollRunning = false
  let pollTimer
  const scope = captureScope()
  const processingQueue = new Set()

  const waitingCount = $derived(queue.filter(entry => entry.state === 'waiting').length)
  const uploadingCount = $derived(queue.filter(entry => entry.state === 'uploading').length)
  const acceptedCount = $derived(queue.filter(entry => entry.receipt).length)
  const readyCount = $derived(queue.filter(entry => entry.state === 'ready' || entry.state === 'no_text' || entry.state === 'split').length)
  const processingCount = $derived(queue.filter(entry => entry.state === 'processing' || entry.state === 'status_unknown').length)
  const failedCount = $derived(queue.filter(entry => entry.state === 'upload_failed' || entry.state === 'processing_failed').length)
  const settledTransfers = $derived(queue.length - waitingCount - uploadingCount)
  const completedCount = $derived(queue.filter(entry => entry.state === 'ready' || entry.state === 'no_text' || entry.state === 'split').length)
  const summary = $derived.by(() => {
    if (!queue.length) return ''
    if (waitingCount || uploadingCount) {
      const parts = [translate("uploadBox.valueOfValueFinished", { "settledTransfers": settledTransfers, "length": queue.length })]
      if (acceptedCount) parts.push(translate("uploadBox.valueUploaded", { "acceptedCount": acceptedCount }))
      if (failedCount) parts.push(translate("uploadBox.valueFailed", { "failedCount": failedCount }))
      return parts.join(' · ')
    }
    const parts = [translate("uploadBox.valueUploaded", { "acceptedCount": acceptedCount })]
    if (readyCount) parts.push(translate("uploadBox.valueReady", { "readyCount": readyCount }))
    if (processingCount) parts.push(translate("uploadBox.valueProcessing", { "processingCount": processingCount }))
    if (failedCount) parts.push(translate("uploadBox.valueFailed", { "failedCount": failedCount }))
    return parts.join(' · ')
  })

  onMount(() => {
    if (!initialFiles.length) return
    const files = [...initialFiles]
    onInitialFilesConsumed?.()
    enqueue(files)
  })

  onDestroy(() => {
    mounted = false
    clearPreviews()
    clearTimeout(pollTimer)
    processingQueue.clear()
  })

  function current(user) {
    return mounted && session.user === user && scopeCurrent(scope)
  }

  function newIdempotencyKey() {
    const bytes = crypto.getRandomValues(new Uint8Array(16))
    bytes[6] = (bytes[6] & 0x0f) | 0x40
    bytes[8] = (bytes[8] & 0x3f) | 0x80
    const hex = [...bytes].map(byte => byte.toString(16).padStart(2, '0'))
    return `${hex.slice(0, 4).join('')}-${hex.slice(4, 6).join('')}-${hex.slice(6, 8).join('')}-${hex.slice(8, 10).join('')}-${hex.slice(10).join('')}`
  }

  function enqueue(files) {
    const additions = []
    for (const file of files) {
      if (!file?.name) continue
      additions.push({
        key: newIdempotencyKey(),
        file,
        name: file.name,
        size: file.size,
        system: scope.code,
        state: 'waiting',
        receipt: '',
        error: '',
        statusCode: 0,
        doc: null,
        id: null,
        children: [],
        address: '',
        checks: 0,
        paused: false,
      })
    }
    if (!additions.length) return
    queue = [...queue, ...additions]
    void processQueue()
  }

  async function processQueue() {
    if (workerRunning) return
    workerRunning = true
    const user = session.user
    while (session.user === user) {
      const entry = queue.find(item => item.state === 'waiting')
      if (!entry) break
      await uploadEntry(entry, user)
    }
    workerRunning = false
  }

  async function uploadEntry(entry, user) {
    entry.state = 'uploading'
    entry.error = ''
    entry.statusCode = 0
    try {
      const result = await uploadDocument(entry.file, entry.system, entry.key)
      if (session.user !== user) return
      markUploaded(scope)
      entry.address = result?.jd_address || ''
      entry.id = result?.id
      entry.receipt = result?.restored ? 'restored' : result?.deduplicated ? 'duplicate' : 'uploaded'
      entry.file = null
      entry.state = 'processing'
      if (current(user)) watchProcessing(entry)
    } catch (ex) {
      if (session.user !== user) return
      entry.state = 'upload_failed'
      entry.statusCode = ex.status || 0
      entry.error = ex.status === 413
        ? translate("uploadBox.chooseASmallerFileOrAskTheAdministrator")
        : ex.message || translate("uploadBox.theUploadDidNotFinish")
    }
  }

  function watchProcessing(entry) {
    entry.checks = 0
    entry.paused = false
    entry.state = 'processing'
    processingQueue.add(entry)
    schedulePoll(0)
  }

  function schedulePoll(delay = CHECK_DELAY) {
    if (!mounted || pollRunning || pollTimer || !processingQueue.size) return
    pollTimer = setTimeout(() => {
      pollTimer = null
      void pollProcessing()
    }, delay)
  }

  async function pollProcessing() {
    if (!mounted || pollRunning) return
    pollRunning = true
    const user = session.user
    const entries = [...processingQueue].slice(0, CHECK_CONCURRENCY)
    await Promise.all(entries.map(entry => checkProcessing(entry, user)))
    pollRunning = false
    if (mounted && processingQueue.size) schedulePoll()
  }

  function pauseChecks(entry) {
    if (entry.checks < MAX_CHECKS) return false
    processingQueue.delete(entry)
    entry.paused = true
    return true
  }

  async function checkProcessing(entry, user) {
    if (!current(user)) {
      processingQueue.delete(entry)
      return
    }
    entry.checks++
    try {
      const tasks = await listTasks({ include: 'jobs', doc_id: entry.id, kind: 'post-', limit: 200 })
      if (!current(user)) return
      if (!Array.isArray(tasks?.results)) throw new Error(translate("uploadBox.invalidProcessingResponse"))
      const active = tasks.results.some(job => job.state === 'pending' || job.state === 'running')
      const failed = tasks.results.some(job => job.state === 'dead')
      if (active) {
        entry.state = 'processing'
        pauseChecks(entry)
        return
      }
      const doc = await getDocument(entry.id)
      if (!current(user)) return
      if (doc?.trashed_at != null) {
        const split = await listDocuments({ split_origin_id: entry.id, page_size: 200 })
        if (!current(user)) return
        const children = Array.isArray(split?.results)
          ? [...split.results].sort((a, b) => (a.split_index ?? a.id) - (b.split_index ?? b.id))
          : []
        if (children.length) {
          processingQueue.delete(entry)
          entry.doc = doc
          entry.children = children
          entry.state = 'split'
          markUploaded()
          return
        }
      }
      processingQueue.delete(entry)
      entry.doc = doc
      entry.state = failed ? 'processing_failed' : doc?.content?.trim() ? 'ready' : 'no_text'
      markUploaded()
    } catch {
      if (!current(user)) return
      entry.state = 'status_unknown'
      pauseChecks(entry)
    }
  }

  function retry(entry) {
    if (!entry.file || entry.statusCode === 413) return
    entry.state = 'waiting'
    entry.error = ''
    void processQueue()
  }

  function remove(entry) {
    processingQueue.delete(entry)
    queue = queue.filter(item => item !== entry)
  }

  function clearCompleted() {
    queue = queue.filter(entry => entry.state !== 'ready' && entry.state !== 'no_text' && entry.state !== 'split')
  }

  function statusLabel(entry) {
    switch (entry.state) {
      case 'waiting': return translate("uploadBox.waiting")
      case 'uploading': return translate("uploadBox.uploading")
      case 'processing': return entry.receipt === 'duplicate'
        ? translate("uploadBox.alreadyInTheArchiveProcessing")
        : entry.receipt === 'restored'
          ? translate("uploadBox.restoredProcessing")
          : translate("uploadBox.uploadedProcessing")
      case 'ready': return entry.receipt === 'duplicate'
        ? translate("uploadBox.alreadyInTheArchiveReady")
        : entry.receipt === 'restored'
          ? translate("uploadBox.restoredReady")
          : translate("uploadBox.ready")
      case 'no_text': return entry.receipt === 'duplicate'
        ? translate("uploadBox.alreadyInTheArchiveNoTextFound")
        : entry.receipt === 'restored'
          ? translate("uploadBox.restoredNoTextFound")
          : translate("uploadBox.readyNoTextFound")
      case 'split': return translate("uploadBox.splitIntoDocuments", { count: entry.children.length })
      case 'upload_failed': return translate("uploadBox.uploadFailed")
      case 'processing_failed': return translate("uploadBox.processingNeedsAttention")
      case 'status_unknown': return translate("uploadBox.couldNotCheckProcessingStatus")
      default: return ''
    }
  }

  function onDrop(event) {
    event.preventDefault()
    over = false
    enqueue([...(event.dataTransfer?.files || [])])
  }

  function clearPreviews() {
    for (const preview of previews) URL.revokeObjectURL(preview.url)
    previews = []
  }

  function onPaste(event) {
    const target = event.target
    if (!surface?.contains(target)) return
    if (target.closest?.('input, textarea, select, [contenteditable]:not([contenteditable="false"])')) return
    const files = [...(event.clipboardData?.files || [])].filter(file => file.type.startsWith('image/'))
    if (!files.length) return
    event.preventDefault()
    previews = [...previews, ...files.map(file => ({ file, url: URL.createObjectURL(file) }))]
  }

  function uploadPasted() {
    const files = previews.map(preview => preview.file)
    clearPreviews()
    enqueue(files)
  }
</script>

<svelte:window onpaste={onPaste} />

<div class="upload-surface" bind:this={surface}>
  {#if queue.length}
    <section class="queue-summary" aria-label={translate("uploadBox.uploadProgress")}>
      <div>
        <b>{translate("uploadBox.uploadFiles")}</b>
        <span role="status" aria-live="polite" aria-atomic="true">{summary}</span>
      </div>
      <progress max={queue.length} value={settledTransfers} aria-label={summary}></progress>
    </section>
  {/if}

  <div class="drop" class:over class:compact={queue.length > 0}
       role="button" tabindex="0" aria-label={translate("uploadBox.addFiles")}
       ondragover={(event) => { event.preventDefault(); over = true }}
       ondragleave={() => (over = false)}
       ondrop={onDrop}
       onclick={() => fileInput.click()}
       onkeydown={(event) => { if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); fileInput.click() } }}>
    <Icon name="upload" size={queue.length ? 20 : 44} />
    {#if queue.length}
      <b>{translate("uploadBox.addMoreFiles")}</b>
    {:else}
      <p class="drop-title"><b>{translate("uploadBox.dropDocumentsHere")}</b> {translate("uploadBox.orClickToChoose")}</p>
      <p class="drop-help">{translate("uploadBox.pdfOfficeDocumentsImagesAndEmailFiles")}</p>
    {/if}
    <input bind:this={fileInput} type="file" multiple hidden
      onchange={(event) => { const files = [...event.target.files]; event.target.value = ''; enqueue(files) }} />
  </div>

  {#if previews.length}
    <section class="card pasted" aria-label={translate("uploadBox.pastedImages")}>
      <b>{translate("uploadBox.readyToUpload")}</b>
      <p class="sub">{translate("uploadBox.theseImagesStayInThisBrowserUntil")}</p>
      <div class="pasted-images">
        {#each previews as preview}
          <figure>
            <img src={preview.url} alt={translate("uploadBox.previewOfValue", { "name": preview.file.name })} />
            <figcaption>{preview.file.name} · {fmtBytes(preview.file.size)}</figcaption>
          </figure>
        {/each}
      </div>
      <div class="queue-actions">
        <button class="btn primary" onclick={uploadPasted}>{translate("uploadBox.uploadValue", { images: plural("uploadBox.imageCount", previews.length) })}</button>
        <button class="btn" onclick={clearPreviews}>{translate("uploadBox.discard")}</button>
      </div>
    </section>
  {/if}

  {#if queue.length}
    <section class="upload-queue" aria-label={translate("uploadBox.files")}>
      {#each queue as entry (entry.key)}
        <article class="card queue-row">
          <span class="state-dot"
                class:active={entry.state === 'uploading' || entry.state === 'processing'}
                class:ok={entry.state === 'ready' || entry.state === 'no_text' || entry.state === 'split'}
                class:danger={entry.state === 'upload_failed' || entry.state === 'processing_failed'}
                aria-hidden="true"></span>
          <div class="file-copy">
            <b title={entry.name}>{entry.name}</b>
            <div class="file-meta">
              <span class="row-status">{statusLabel(entry)}</span>
              <span>{fmtBytes(entry.size)}</span>
            </div>
            {#if entry.doc?.title && entry.doc.title !== entry.name}
              <span title={entry.doc.title}>{translate("uploadBox.filedAsValue", { "title": entry.doc.title })}</span>
            {/if}
            {#if entry.children.length}
              <div class="split-results" role="group" aria-label={translate("uploadBox.documentsCreatedFromThisScan")}>
                {#each entry.children as child (child.id)}
                  <a href={filingHref(`#/doc/${child.id}`)}>
                    {#if child.archive_serial_number != null}
                      <span class="chip">{translate("common.asnValue", { value: String(child.archive_serial_number) })}</span>
                    {/if}
                    <span>{child.title || translate("uploadBox.documentValue", { id: child.id })}</span>
                  </a>
                {/each}
              </div>
            {/if}
            {#if entry.error}<span class="row-error" role="alert">{entry.error}</span>{/if}
            {#if entry.paused}<span>{translate("uploadBox.automaticChecksPausedProcessingContinuesOnThe")}</span>{/if}
          </div>
          <div class="row-actions">
            {#if entry.state === 'upload_failed' && entry.statusCode !== 413}
              <button class="btn sm" onclick={() => retry(entry)}>{translate("common.retry")}</button>
            {/if}
            {#if entry.state === 'upload_failed'}
              <button class="btn sm" onclick={() => remove(entry)}>{translate("uploadBox.remove")}</button>
            {/if}
            {#if entry.id && (entry.paused || entry.state === 'processing_failed')}
              <button class="btn sm" onclick={() => watchProcessing(entry)}>{translate("uploadBox.checkAgain")}</button>
            {/if}
            {#if entry.id && !entry.children.length}<a class="btn sm" href={filingHref(`#/doc/${entry.id}`)}>{translate("uploadBox.openDocument")}</a>{/if}
          </div>
        </article>
      {/each}
    </section>

    <div class="queue-footer">
      <button class="btn" disabled={!completedCount} onclick={clearCompleted}>{translate("uploadBox.clearCompleted")}</button>
      <a class="btn" href={filingHref('#/documents')}>{translate("uploadBox.viewDocuments")}</a>
    </div>
  {/if}
</div>

<style>
  .upload-surface { display:flex;flex-direction:column;gap:14px;max-width:760px;margin:0 auto }
  .queue-summary { position:sticky;top:0;z-index:4;display:grid;gap:9px;padding:13px 16px;border:1px solid var(--line);border-radius:var(--r-sm);background:var(--surface);box-shadow:var(--shadow) }
  .queue-summary div { display:flex;align-items:baseline;justify-content:space-between;gap:16px }
  .queue-summary span { color:var(--muted);font-size:.82rem;text-align:right }
  progress { width:100%;height:8px;accent-color:var(--accent) }
  .drop.compact { display:flex;align-items:center;justify-content:center;gap:9px;padding:12px 16px }
  .drop-title { margin:12px 0 4px;font-size:1.05rem }
  .drop-help { margin:0;font-size:.8rem }
  .upload-queue { display:flex;flex-direction:column;gap:8px }
  .queue-row { display:grid;grid-template-columns:auto minmax(0,1fr) auto;align-items:center;gap:12px;padding:13px 15px }
  .state-dot { width:10px;height:10px;border-radius:50%;background:var(--line-strong) }
  .state-dot.active { background:var(--accent);animation:pulse 1.2s infinite }
  .state-dot.ok { background:var(--ok) }
  .state-dot.danger { background:var(--danger) }
  .file-copy { display:flex;min-width:0;flex-direction:column;gap:3px }
  .file-copy b,.file-copy span { overflow:hidden;text-overflow:ellipsis;white-space:nowrap }
  .file-copy span { color:var(--muted);font-size:.8rem }
  .file-meta { display:flex;align-items:center;gap:7px;min-width:0 }
  .file-meta span + span::before { content:'·';margin-right:7px }
  .file-copy .row-status { color:var(--ink);font-weight:600 }
  .file-copy .row-error { color:var(--danger);white-space:normal }
  .split-results { display:flex; flex-wrap:wrap; gap:5px; margin-top:3px; }
  .split-results a { display:inline-flex; align-items:center; gap:5px; max-width:100%; padding:3px 7px; border:1px solid var(--line); border-radius:6px; color:var(--ink); font-size:.75rem; text-decoration:none; }
  .split-results a:hover { border-color:var(--accent); }
  .split-results a > span:last-child { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .row-actions,.queue-actions,.queue-footer { display:flex;align-items:center;justify-content:flex-end;gap:8px;flex-wrap:wrap }
  .pasted { padding:16px }
  .pasted .sub { margin:6px 0 12px;color:var(--muted);font-size:.82rem }
  .pasted-images { display:flex;flex-wrap:wrap;gap:12px;margin-bottom:14px }
  figure { margin:0;max-width:100% }
  figure img { display:block;width:180px;max-width:100%;height:140px;object-fit:contain;border:1px solid var(--line);border-radius:6px }
  figcaption { max-width:180px;margin-top:5px;color:var(--muted);font-size:.75rem;overflow-wrap:anywhere }
  @media (max-width:620px) {
    .queue-summary { position:static }
    .queue-summary div { align-items:flex-start;flex-direction:column;gap:3px }
    .queue-summary span { text-align:left }
    .queue-row { grid-template-columns:auto minmax(0,1fr) }
    .row-actions { grid-column:2;justify-content:flex-start }
  }
</style>
