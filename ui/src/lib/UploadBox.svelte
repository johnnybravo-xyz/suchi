<script>
  import { uploadDocument, getDocument, patchDocument, listTasks } from '../lib/api.js'
  import { SENSITIVITY_OPTIONS, fmtBytes } from '../lib/format.js'
  import { markUploaded } from '../lib/upload_bus.svelte.js'
  import Icon from '../lib/Icon.svelte'

  let { notify, jdCategories = [] } = $props()
  let over = $state(false)
  let queue = $state([])
  let fileInput

  // Poll the durable post-ingest job while refreshing the document details.
  async function hydrate(entry) {
    for (let attempt = 0; attempt < 40; attempt++) {
      await new Promise(r => setTimeout(r, attempt === 0 ? 1200 : 2500))
      try {
        const [d, tasks] = await Promise.all([
          getDocument(entry.id),
          listTasks({ include: 'jobs', doc_id: entry.id, kind: 'post-ingest', limit: 1 }),
        ])
        entry.doc = d
        const job = tasks?.results?.[0]
        if (!job) { entry.processing = false; return }
        if (job.state === 'dead') {
          entry.processing = false
          entry.processingError = true
          return
        }
      } catch { /* keep the durable processing state until a later poll */ }
    }
    entry.processing = false
  }

  async function send(files) {
    for (const f of files) {
      const entry = $state({ name: f.name, size: f.size, status: 'uploading', doc: null, processing: false })
      queue = [entry, ...queue]
      try {
        const res = await uploadDocument(f)
        entry.id = res?.id
        markUploaded()
        if (res?.deduplicated) {
          entry.status = 'dup'
          entry.match = res.title || `Document #${res.id}`
        } else {
          entry.status = 'done'
          entry.processing = true
          hydrate(entry)
        }
      } catch (ex) {
        if (ex.status === 413) { entry.status = 'error'; entry.msg = 'larger than the server allows' }
        else { entry.status = 'error'; entry.msg = ex.message }
      }
    }
    notify?.('Upload finished')
  }

  function onDrop(e) {
    e.preventDefault(); over = false
    send([...e.dataTransfer.files])
  }

  // The upload queue only exposes fields that are safe to change before
  // processing finishes. Managed metadata remains read-only here.
  async function patch(q, body, msg) {
    if (!q.id) return
    try {
      await patchDocument(q.id, body)
      q.doc = await getDocument(q.id)
      markUploaded()
      if (msg) notify?.(msg)
    } catch (ex) { notify?.(ex.message || 'Update failed') }
  }
</script>

<div>
  <div class="drop" class:over
       role="button" tabindex="0" aria-label="Upload documents"
       ondragover={(e) => { e.preventDefault(); over = true }}
       ondragleave={() => (over = false)}
       ondrop={onDrop}
       onclick={() => fileInput.click()}
       onkeydown={(e) => e.key === 'Enter' && fileInput.click()}>
    <Icon name="upload" size={44} />
    <p style="margin:12px 0 4px;font-size:1.05rem"><b>Drop documents here</b> or click to choose</p>
    <p style="margin:0;font-size:.8rem">PDF, office docs, images, email files. The pipeline sorts out the rest</p>
    <input bind:this={fileInput} type="file" multiple hidden onchange={(e) => send([...e.target.files])} />
  </div>

  {#if queue.length}
    <div style="margin-top:16px;display:flex;flex-direction:column;gap:10px">
      {#each queue as q}
        <div class="card up-card">
          <div class="irow" style="padding:0;border:0">
            <span class="dot" class:ok={q.status === 'done'} class:warn={q.status === 'dup'} class:danger={q.status === 'error'}></span>
            <span class="title grow">{q.name}</span>
            <span class="sub">{fmtBytes(q.size)}</span>
            {#if q.status === 'uploading'}<span class="sub">uploading…</span>
            {:else if q.status === 'error'}<span class="pill danger" title={q.msg}>{q.msg || 'failed'}</span>{/if}
          </div>
          {#if q.status === 'dup'}
            <div class="up-detail">
              <span class="pill warn">duplicate</span>
              <span class="sub">Already filed{q.match ? ` as “${q.match}”` : ''}; source recorded.</span>
              {#if q.id}<a class="btn sm" href={`#/doc/${q.id}`} target="_blank" rel="noopener">Open in new tab</a>{/if}
            </div>
          {:else if q.status === 'done'}
            <div class="up-detail" style="flex-wrap:wrap;row-gap:8px">
              {#if q.processing}
                <span class="pill">processing<span class="ellip"></span></span>
              {:else if q.processingError}
                <span class="pill danger">processing failed</span>
              {/if}
              {#if q.doc?.title && q.doc.title !== q.name}<span class="sub">filed as “{q.doc.title}”</span>{/if}
              {#if q.doc?.tags?.length}
                {#each q.doc.tags.slice(0, 3) as t}<span class="pill">{t}</span>{/each}
              {/if}
              {#if q.doc?.correspondents?.length}
                <span class="sub">from {q.doc.correspondents.map(c => c.name || c).join(', ')}</span>
              {/if}
              <span class="spacer"></span>
              <select class="input" style="max-width:180px;padding:5px 8px;font-size:.8rem"
                      aria-label="File under"
                      onchange={(e) => e.target.value && patch(q, { jd_category_id: Number(e.target.value) }, 'Filed')}
                      value={q.doc?.jd_category_id ?? ''}>
                <option value="" disabled>file under…</option>
                {#each jdCategories as c}<option value={c.id}>{c.code} {c.name}</option>{/each}
              </select>
              <select class="input" style="max-width:140px;padding:5px 8px;font-size:.8rem"
                      aria-label="Sensitivity"
                      value={q.doc?.sensitivity ?? ''}
                      onchange={(e) => patch(q, { sensitivity: e.target.value }, 'Sensitivity set')}>
                <option value="">sensitivity…</option>
                {#each SENSITIVITY_OPTIONS as option (option.value)}
                  <option value={option.value}>{option.label}</option>
                {/each}
              </select>
              <a class="btn sm" href={`#/doc/${q.id}`}>Open</a>
            </div>
          {/if}
        </div>
      {/each}
    </div>
  {/if}
</div>
