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

  async function resolve(t, choice) {
    try {
      await resolveApprovalTask(t.id, { choice, decision: choice })
      tasks = tasks.filter(x => x.id !== t.id)
      onCount?.(tasks.length)
      notify?.(`Resolved: ${choice}`)
    } catch (ex) { notify?.(ex.message || 'Could not resolve the task') }
  }

  function deadline(t) {
    if (!t.deadline_at) return null
    const hrs = (t.deadline_at * 1000 - Date.now()) / 36e5
    if (hrs < 0) return { text: 'deadline passed', soon: true }
    if (hrs < 24) return { text: `${Math.max(1, Math.round(hrs))}h left`, soon: true }
    return { text: `due ${fmtDate(t.deadline_at)}`, soon: false }
  }

  // Cmd/Ctrl+Enter resolves the top task with its first (primary) choice.
  function onKey(e) {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter' && tasks[0]) {
      e.preventDefault()
      const t = tasks[0]
      resolve(t, (t.choices?.length ? t.choices : ['approve'])[0])
    }
  }

  load()
</script>

<svelte:window onkeydown={onKey} />

{#if err}<div class="err">{err}</div>{/if}

{#if loading}
  <div class="index">{#each Array(3) as _}<div class="irow"><div class="skel" style="width:55%"></div></div>{/each}</div>
{:else}
  {#if tasks.length === 0}
    <div class="empty"><Icon name="tasks" size={56} /><b>No approvals waiting.</b><span>Human-in-the-loop steps from approval chains land here.</span></div>
  {:else}
    <div style="display:flex;flex-direction:column;gap:12px;margin-bottom:22px">
      {#each tasks as t (t.id)}
        {@const dl = deadline(t)}
        <div class="card task-card">
          <div class="prompt">{t.prompt || t.title || `Task #${t.id}`}</div>
          <div class="meta">
            {#if t.workflow_name}<span class="pill ok">{t.workflow_name}</span>{/if}
            {#if t.assignee}<span class="pill">{t.assignee}</span>{/if}
            <span>step <code>{t.state_key}</code></span>
            {#if t.doc_id}<a href={`#/doc/${t.doc_id}`}>document #{t.doc_id}</a>{/if}
            <span>opened {fmtDate(t.created_at)}</span>
            {#if dl}<span class:deadline-soon={dl.soon}>{dl.text}</span>{/if}
          </div>
          <div class="choices">
            {#each (t.choices?.length ? t.choices : ['approve', 'reject']) as c, i}
              <button class="btn sm" class:primary={i === 0} class:danger={/reject|deny|decline/i.test(c)}
                      onclick={() => resolve(t, c)}>
                {#if i === 0}<Icon name="check" size={13} />{/if}{c}
              </button>
            {/each}
          </div>
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
          <span class="grow">
            <span class="title mono" style="display:block;font-size:.8rem">{j.kind}</span>
            {#if j.last_error}<span class="sub" title={j.last_error}>{j.last_error.slice(0, 90)}</span>{/if}
          </span>
          <span class="sub">{j.attempts} attempts</span>
          {#if j.doc_id}<a class="btn sm" href={`#/doc/${j.doc_id}`}>Doc #{j.doc_id}</a>{/if}
        </div>
      {/each}
    </div>
  {/if}
{/if}
