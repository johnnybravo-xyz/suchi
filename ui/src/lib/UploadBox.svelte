<script>
  import { onMount, onDestroy } from 'svelte'
  import { session } from './session.svelte.js'
  import { uploadDocument, getDocument, patchDocument, listTasks } from './api.js'
  import { SENSITIVITY_OPTIONS, fmtBytes } from './format.js'
  import { markUploaded } from './upload_bus.svelte.js'
  import Icon from './Icon.svelte'

  let { notify, jdCategories = [], initialFiles = [] } = $props()
  let over = $state(false)
  let queue = $state([])
  let previews = $state([])
  let surface
  let fileInput
  let mounted = true
  const timers = new Map()

  onMount(() => { if (initialFiles.length) send([...initialFiles]) })
  onDestroy(() => {
    mounted = false
    clearPreviews()
    for (const [timer, resolve] of timers) { clearTimeout(timer); resolve() }
    timers.clear()
  })

  function current(user) { return mounted && session.user === user }

  function pause() {
    return new Promise(resolve => {
      const timer = setTimeout(() => { timers.delete(timer); resolve() }, 2500)
      timers.set(timer, resolve)
    })
  }

  // Completed jobs are omitted by the API. Read unfinished post-processing
  // before the document so a completed extraction cannot leave a stale receipt.
  async function hydrate(entry, user) {
    if (entry.checking || !current(user)) return
    entry.checking = true
    entry.paused = false
    for (let attempt = 0; attempt < 40 && current(user); attempt++) {
      try {
        const tasks = await listTasks({ include: 'jobs', doc_id: entry.id, kind: 'post-', limit: 200 })
        if (!current(user)) return
        const doc = await getDocument(entry.id)
        if (!current(user)) return
        if (!Array.isArray(tasks?.results)) throw new Error('Invalid processing response')
        entry.doc = doc
        const active = tasks.results.some(job => job.state === 'pending' || job.state === 'running')
        const failed = tasks.results.some(job => job.state === 'dead')
        entry.processing = active ? 'processing' : failed ? 'failed' : 'ready'
        if (!active) { entry.checking = false; markUploaded(); return }
      } catch {
        if (!current(user)) return
        entry.processing = 'unknown'
      }
      if (attempt < 39) await pause()
    }
    if (!current(user)) return
    entry.checking = false
    entry.paused = true
  }

  async function send(files) {
    const user = session.user
    const counts = { uploaded: 0, restored: 0, duplicate: 0, failed: 0 }
    for (const file of files) {
      if (session.user !== user) return
      const entry = $state({ name: file.name, size: file.size, status: 'uploading', doc: null, processing: 'checking', checking: false, paused: false })
      if (mounted) queue = [entry, ...queue]
      try {
        const result = await uploadDocument(file)
        if (session.user !== user) return
        markUploaded()
        if (!mounted) continue
        entry.id = result?.id
        entry.status = result?.restored ? 'restored' : result?.deduplicated ? 'duplicate' : 'uploaded'
        counts[entry.status]++
        hydrate(entry, user)
      } catch (ex) {
        if (session.user !== user) return
        if (!mounted) continue
        counts.failed++
        entry.status = 'error'
        entry.msg = ex.status === 413 ? 'Larger than the server allows' : ex.message || 'Upload failed'
      }
    }
    if (current(user)) notify?.(Object.entries(counts).filter(([, count]) => count).map(([label, count]) => `${count} ${label === 'duplicate' && count > 1 ? 'duplicates' : label}`).join(' · '))
  }

  function onDrop(event) {
    event.preventDefault()
    over = false
    send([...(event.dataTransfer?.files || [])])
  }

  function clearPreviews() {
    for (const preview of previews) URL.revokeObjectURL(preview.url)
    previews = []
  }

  function onPaste(event) {
    const target = event.target
    if (!surface?.contains(target) && !surface?.closest('[role="dialog"]')?.contains(target)) return
    if (target.closest?.('input, textarea, select, [contenteditable]:not([contenteditable="false"])')) return
    const files = [...(event.clipboardData?.files || [])].filter(file => file.type.startsWith('image/'))
    if (!files.length) return
    event.preventDefault()
    previews = [...previews, ...files.map(file => ({ file, url: URL.createObjectURL(file) }))]
  }

  function uploadPasted() {
    const files = previews.map(preview => preview.file)
    clearPreviews()
    send(files)
  }

  // Managed metadata stays read-only here; these fields can be safely changed
  // without overwriting classifier or human-maintained values.
  async function patch(entry, body, message) {
    const user = session.user
    if (!entry.id) return
    try {
      await patchDocument(entry.id, body)
      if (!current(user)) return
      const doc = await getDocument(entry.id)
      if (!current(user)) return
      entry.doc = doc
      markUploaded()
      if (message) notify?.(message)
    } catch (ex) { if (current(user)) notify?.(ex.message || 'Update failed') }
  }
</script>

<svelte:window onpaste={onPaste} />

<div bind:this={surface}>
  <div class="drop" class:over
       role="button" tabindex="0" aria-label="Upload documents"
       ondragover={(event) => { event.preventDefault(); over = true }}
       ondragleave={() => (over = false)}
       ondrop={onDrop}
       onclick={() => fileInput.click()}
       onkeydown={(event) => { if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); fileInput.click() } }}>
    <Icon name="upload" size={44} />
    <p style="margin:12px 0 4px;font-size:1.05rem"><b>Drop documents here</b> or click to choose</p>
    <p style="margin:0;font-size:.8rem">PDF, office docs, images, email files. Or focus here and paste a screenshot.</p>
    <input bind:this={fileInput} type="file" multiple hidden onchange={(event) => { const files = [...event.target.files]; event.target.value = ''; send(files) }} />
  </div>

  {#if previews.length}
    <section class="card pasted" aria-label="Pasted images">
      <b>Ready to upload?</b>
      <p class="sub">These images stay in this browser until you confirm.</p>
      <div class="pasted-images">
        {#each previews as preview}
          <figure>
            <img src={preview.url} alt={`Preview of ${preview.file.name}`} />
            <figcaption>{preview.file.name} · {fmtBytes(preview.file.size)}</figcaption>
          </figure>
        {/each}
      </div>
      <div class="up-detail">
        <button class="btn primary" onclick={uploadPasted}>Upload {previews.length === 1 ? 'image' : `${previews.length} images`}</button>
        <button class="btn" onclick={clearPreviews}>Discard</button>
      </div>
    </section>
  {/if}

  {#if queue.length}
    <div class="upload-queue" aria-label="Upload results">
      {#each queue as entry}
        <div class="card up-card">
          <div class="irow" style="padding:0;border:0">
            <span class="dot" class:ok={entry.status === 'uploaded' || entry.status === 'restored'} class:warn={entry.status === 'duplicate'} class:danger={entry.status === 'error'}></span>
            <span class="title grow" title={entry.name}>{entry.name}</span>
            <span class="sub">{fmtBytes(entry.size)}</span>
          </div>
          <div class="up-detail" aria-live="polite">
            {#if entry.status === 'uploading'}<span class="pill">Uploading…</span>
            {:else if entry.status === 'error'}<span class="pill danger">Upload failed</span><span class="receipt-message">{entry.msg}</span>
            {:else}
              {#if entry.status === 'duplicate'}
                <span class="pill warn">duplicate</span><span class="sub">Already in your archive; source recorded.</span>
              {:else if entry.status === 'restored'}
                <span class="pill">Restored from Trash</span><span class="sub">Existing document and metadata kept.</span>
              {:else}<span class="pill">Uploaded</span>{/if}
              {#if entry.processing === 'ready'}
                <span class="pill" class:warn={!entry.doc?.content?.trim()}>{entry.doc?.content?.trim() ? 'Text extracted' : 'No text found'}</span>
              {:else if entry.processing === 'failed'}
                <span class="pill danger">Processing needs attention</span>
              {:else if entry.processing === 'unknown'}
                <span class="pill warn">Processing status unavailable</span>
              {:else}<span class="pill">Still processing{entry.checking ? '…' : ''}</span>{/if}
              {#if entry.paused}<span class="sub">Automatic checks paused. Server processing continues.</span>{/if}
              {#if !entry.checking && (entry.paused || entry.processing === 'failed')}
                <button class="btn sm" onclick={() => hydrate(entry, session.user)}>Check again</button>
              {/if}
            {/if}
          </div>
          {#if entry.id}
            <div class="up-detail">
              {#if entry.doc?.title && entry.doc.title !== entry.name}<span class="receipt-message">“{entry.doc.title}”</span>{/if}
              {#each entry.doc?.tags?.slice(0, 3) || [] as tag}<span class="pill">{tag}</span>{/each}
              {#if entry.doc?.correspondents?.length}<span class="sub">from {entry.doc.correspondents.map(c => c.name || c).join(', ')}</span>{/if}
              <span class="spacer"></span>
              <select class="input" style="max-width:180px;padding:5px 8px;font-size:.8rem"
                      aria-label="File under" value={entry.doc?.jd_category_id ?? ''}
                      onchange={(event) => event.target.value && patch(entry, { jd_category_id: Number(event.target.value) }, 'Filed')}>
                <option value="" disabled>file under…</option>
                {#each jdCategories as category}<option value={category.id}>{category.code} {category.name}</option>{/each}
              </select>
              <select class="input" style="max-width:140px;padding:5px 8px;font-size:.8rem"
                      aria-label="Sensitivity" value={entry.doc?.sensitivity ?? ''}
                      onchange={(event) => patch(entry, { sensitivity: event.target.value }, 'Sensitivity set')}>
                <option value="">sensitivity…</option>
                {#each SENSITIVITY_OPTIONS as option (option.value)}<option value={option.value}>{option.label}</option>{/each}
              </select>
              <a class="btn sm" href={`#/doc/${entry.id}`}>Open</a>
            </div>
          {/if}
        </div>
      {/each}
    </div>
  {/if}
</div>

<style>
  .upload-queue { margin-top:16px;display:flex;flex-direction:column;gap:10px }
  .receipt-message { overflow-wrap:anywhere;min-width:0 }
  .pasted { margin-top:16px;padding:16px }
  .pasted .sub { margin:6px 0 12px;color:var(--muted);font-size:.82rem }
  .pasted-images { display:flex;flex-wrap:wrap;gap:12px;margin-bottom:14px }
  figure { margin:0;max-width:100% }
  figure img { display:block;max-width:100%;width:180px;height:140px;object-fit:contain;border:1px solid var(--line);border-radius:6px }
  figcaption { max-width:180px;font-size:.75rem;overflow-wrap:anywhere;color:var(--muted);margin-top:5px }
</style>
