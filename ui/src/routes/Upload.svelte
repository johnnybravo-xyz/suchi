<script>
  import { uploadDocument } from '../lib/api.js'
  import Icon from '../lib/Icon.svelte'

  let { notify } = $props()
  let over = $state(false)
  let queue = $state([])   // { name, status: 'uploading'|'done'|'dup'|'error', id?, msg? }
  let fileInput

  async function send(files) {
    for (const f of files) {
      const entry = $state({ name: f.name, status: 'uploading' })
      queue = [entry, ...queue]
      try {
        const res = await uploadDocument(f)
        entry.status = res?.restored ? 'dup' : 'done'
        entry.id = res?.id
      } catch (ex) {
        if (ex.status === 409) { entry.status = 'dup'; entry.msg = 'already in the archive' }
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

<div class="content-narrow">
  <div class="drop" class:over
       role="button" tabindex="0" aria-label="Upload documents"
       ondragover={(e) => { e.preventDefault(); over = true }}
       ondragleave={() => (over = false)}
       ondrop={onDrop}
       onclick={() => fileInput.click()}
       onkeydown={(e) => e.key === 'Enter' && fileInput.click()}>
    <Icon name="upload" size={40} />
    <p style="margin:10px 0 4px"><b>Drop documents here</b> or click to choose</p>
    <p style="margin:0;font-size:.8rem">PDF, office docs, images, email files — the pipeline sorts out the rest</p>
    <input bind:this={fileInput} type="file" multiple hidden onchange={(e) => send([...e.target.files])} />
  </div>

  {#if queue.length}
    <div class="index" style="margin-top:16px">
      {#each queue as q}
        <div class="irow">
          <span class="dot" class:ok={q.status === 'done'} class:warn={q.status === 'dup'} class:danger={q.status === 'error'}></span>
          <span class="title grow">{q.name}</span>
          <span class="end">
            {#if q.status === 'uploading'}<span class="sub">uploading…</span>
            {:else if q.status === 'done'}<a class="btn sm" href={`#/doc/${q.id}`}>Open</a>
            {:else if q.status === 'dup'}<span class="pill warn">duplicate</span>
            {:else}<span class="pill danger" title={q.msg}>failed</span>{/if}
          </span>
        </div>
      {/each}
    </div>
  {/if}
</div>
