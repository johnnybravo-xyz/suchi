<script>
  import { uploadDocument, getDocument } from '../lib/api.js'
  import { fmtBytes } from '../lib/format.js'
  import Icon from '../lib/Icon.svelte'

  let { notify } = $props()
  let over = $state(false)
  let queue = $state([])
  let fileInput

  // Poll the fresh document a few times so the panel fills in as the
  // pipeline enriches it (title, JD, OCR text). Stops early once filed.
  async function hydrate(entry) {
    for (const wait of [1200, 2500, 4000, 6000]) {
      await new Promise(r => setTimeout(r, wait))
      try {
        const d = await getDocument(entry.id)
        entry.doc = d
        if (d.jd_category_code || d.content) { entry.processing = false; return }
      } catch { return }
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
        entry.status = 'done'
        entry.processing = true
        hydrate(entry)
      } catch (ex) {
        if (ex.status === 409 && ex.data?.id) {
          // The archive already has these bytes — the 409 body carries
          // the matched document, so show it instead of a dead end.
          entry.status = 'dup'
          entry.id = ex.data.id
          entry.match = ex.data.title || `Document #${ex.data.id}`
        } else if (ex.status === 409) { entry.status = 'dup'; entry.msg = 'already in the archive' }
        else if (ex.status === 413) { entry.status = 'error'; entry.msg = 'larger than the server allows' }
        else { entry.status = 'error'; entry.msg = ex.message }
      }
    }
    notify?.('Upload finished — the pipeline is processing')
  }

  function onDrop(e) {
    e.preventDefault(); over = false
    send([...e.dataTransfer.files])
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
    <p style="margin:0;font-size:.8rem">PDF, office docs, images, email files — the pipeline sorts out the rest</p>
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
              <span class="sub">These exact bytes are already filed{q.match ? ` as “${q.match}”` : ''}.</span>
              {#if q.id}<a class="btn sm" href={`#/doc/${q.id}`}>Open the existing document</a>{/if}
            </div>
          {:else if q.status === 'done'}
            <div class="up-detail">
              {#if q.doc?.jd_category_code}
                <span class="chip" title={q.doc.jd_category_name}>{q.doc.jd_category_code}</span>
              {:else if q.processing}
                <span class="pill">processing<span class="ellip"></span></span>
              {/if}
              {#if q.doc?.title && q.doc.title !== q.name}<span class="sub">filed as “{q.doc.title}”</span>{/if}
              {#if q.doc?.sensitivity}<span class="pill" class:warn={q.doc.sensitivity === 'internal'} class:danger={q.doc.sensitivity === 'confidential'}>{q.doc.sensitivity}</span>{/if}
              <span class="spacer"></span>
              <a class="btn sm" href={`#/doc/${q.id}`}>Open</a>
            </div>
          {/if}
        </div>
      {/each}
    </div>
  {/if}
</div>
