<script>
  import { listTasks, resolveApprovalTask } from '../lib/api.js'
  import { fmtDate } from '../lib/format.js'
  import Icon from '../lib/Icon.svelte'

  let { notify, onCount } = $props()
  let tasks = $state([])
  let jobs = $state([])
  let loading = $state(true)
  let err = $state('')

  async function load() {
    loading = true; err = ''
    try {
      const [wf, jb] = await Promise.all([
        listTasks({ include: 'workflow', state: 'pending', limit: 200 }),
        listTasks({ include: 'jobs', state: 'dead', limit: 50 }),
      ])
      tasks = wf?.results || wf || []
      jobs = jb?.results || jb || []
      onCount?.(tasks.length)
    } catch (ex) { err = ex.message || 'Could not load tasks.' }
    finally { loading = false }
  }

  async function resolve(t, decision) {
    try {
      await resolveApprovalTask(t.id, { decision })
      tasks = tasks.filter(x => x.id !== t.id)
      onCount?.(tasks.length)
      notify?.(decision === 'approve' ? 'Approved' : 'Rejected')
    } catch (ex) { notify?.(ex.message || 'Could not resolve the task') }
  }

  load()
</script>

{#if err}<div class="err">{err}</div>{/if}

{#if loading}
  <div class="index">{#each Array(3) as _}<div class="irow"><div class="skel" style="width:55%"></div></div>{/each}</div>
{:else}
  {#if tasks.length === 0}
    <div class="empty"><Icon name="tasks" size={56} /><b>No approvals waiting.</b><span>Human-in-the-loop tasks from approval chains land here.</span></div>
  {:else}
    <div class="index" style="margin-bottom:20px">
      {#each tasks as t (t.id)}
        <div class="irow" role="listitem">
          <span class="dot warn"></span>
          <span class="grow">
            <span class="title" style="display:block">{t.title || t.kind || `Task #${t.id}`}</span>
            <span class="sub">{t.doc_id ? `document #${t.doc_id} · ` : ''}{t.created_at ? fmtDate(t.created_at) : ''}</span>
          </span>
          {#if t.doc_id}<a class="btn sm" href={`#/doc/${t.doc_id}`}>Open doc</a>{/if}
          <button class="btn sm" onclick={() => resolve(t, 'approve')}><Icon name="check" size={13} /> Approve</button>
          <button class="btn sm danger" onclick={() => resolve(t, 'reject')}><Icon name="x" size={13} /> Reject</button>
        </div>
      {/each}
    </div>
  {/if}

  {#if jobs.length}
    <h3 style="font-size:.9rem;color:var(--muted);margin:0 0 8px">Dead jobs — needs attention</h3>
    <div class="index">
      {#each jobs as j (j.id)}
        <div class="irow">
          <span class="dot danger"></span>
          <span class="title grow mono" style="font-size:.8rem">{j.kind}</span>
          {#if j.doc_id}<a class="btn sm" href={`#/doc/${j.doc_id}`}>Doc #{j.doc_id}</a>{/if}
          <span class="sub">{j.last_error ? j.last_error.slice(0, 60) : ''}</span>
        </div>
      {/each}
    </div>
  {/if}
{/if}
