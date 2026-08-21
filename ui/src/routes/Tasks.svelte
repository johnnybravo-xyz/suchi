<script>
  import { listTasks, resolveApprovalTask, retryDeadJob, dismissDeadJob } from '../lib/api.js'
  import { fmtDate } from '../lib/format.js'
  import Icon from '../lib/Icon.svelte'

  let { notify, onCount } = $props()
  let tasks = $state([])
  let jobs = $state([])
  let loading = $state(true)
  let err = $state('')
  let busyJobs = $state(new Set())

  async function load() {
    loading = true; err = ''
    try {
      const [wf, jb] = await Promise.all([
        listTasks({ include: 'approvals', state: 'pending', limit: 200 }),
        listTasks({ include: 'jobs', state: 'dead', limit: 50 }),
      ])
      // `include=approvals` skips the classic tasks branch → server returns
      // an empty `results` array; the actual approval-task list lives under
      // `approval_tasks`. Jobs still ride the classic `results` shape.
      tasks = wf?.approval_tasks || []
      jobs = jb?.results || jb || []
      onCount?.(tasks.length + jobs.length)
    } catch (ex) { err = ex.message || 'Could not load tasks.' }
    finally { loading = false }
  }

  async function resolve(t, choice) {
    try {
      await resolveApprovalTask(t.id, { choice })
      tasks = tasks.filter(x => x.id !== t.id)
      onCount?.(tasks.length + jobs.length)
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

  function taskChoices(t) {
    const choices = t.choices?.length ? t.choices : ['approve', 'reject']
    if (t.approval_name === 'rescan-proposal' && Number(t.vars?.stale_count || 0) <= 20) {
      return choices.filter(c => c !== 'approve_sample')
    }
    return choices
  }

  function choiceLabel(t, choice) {
    if (t.approval_name === 'rescan-proposal') {
      const count = Number(t.vars?.stale_count || 0)
      if (choice === 'approve_all') return count ? `Rescan all ${count}` : 'Rescan all'
      if (choice === 'approve_sample') return 'Try 20 first'
      if (choice === 'dismiss') return 'Dismiss'
    }
    const text = choice.replaceAll('_', ' ')
    return text.charAt(0).toUpperCase() + text.slice(1)
  }

  function pipelineLabel(kind) {
    return { llm: 'LLM classification', ocr: 'OCR', content: 'content extraction' }[kind] || kind
  }

  function fieldLabel(field) {
    return { jd_category: 'Filing category', correspondent: 'Correspondent', document_type: 'Document type', tag: 'Tag', title: 'Title' }[field] || field
  }

  function suggestionValue(vars) {
    return vars?.label || vars?.value || `#${vars?.value_id}`
  }

  function retryLabel(job) {
    const count = Number(job.attempts || 0)
    return count === 0 ? 'Not retried' : `${count} ${count === 1 ? 'retry' : 'retries'}`
  }

  async function actOnDeadJob(job, action) {
    busyJobs = new Set([...busyJobs, job.id])
    try {
      if (action === 'retry') await retryDeadJob(job.id)
      else await dismissDeadJob(job.id)
      notify?.(action === 'retry' ? 'Job queued again' : 'Job dismissed')
      await load()
    } catch (ex) {
      notify?.(ex.message || `Could not ${action} the job`)
    } finally {
      const next = new Set(busyJobs)
      next.delete(job.id)
      busyJobs = next
    }
  }

  // Cmd/Ctrl+Enter resolves the top task with its first (primary) choice.
  function onKey(e) {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter' && tasks[0]) {
      e.preventDefault()
      const t = tasks[0]
      resolve(t, taskChoices(t)[0])
    }
  }

  load()
</script>

<svelte:window onkeydown={onKey} />

{#if err}<div class="err">{err}</div>{/if}

{#if loading}
  <div class="index">{#each Array(3) as _}<div class="irow"><div class="skel" style="width:55%"></div></div>{/each}</div>
{:else}
  {#if tasks.length === 0 && jobs.length === 0}
    <div class="empty"><Icon name="tasks" size={56} /><b>Nothing needs you.</b><span>The archive is running itself.</span></div>
  {:else if tasks.length > 0}
    <h3 style="font-size:.9rem;color:var(--muted);margin:14px 0 8px">Approvals</h3>
    <div style="display:flex;flex-direction:column;gap:12px;margin-bottom:22px">
      {#each tasks as t (t.id)}
        {@const dl = deadline(t)}
        <div class="card task-card">
          <div class="prompt">{t.prompt || t.title || `Task #${t.id}`}</div>
          {#if t.approval_name === 'rescan-proposal' && t.vars}
            <div style="font-size:.85rem;color:var(--muted);margin-top:2px">
              <b>{pipelineLabel(t.vars.kind)}</b> has a newer processing revision.
              {t.vars.stale_count} document{t.vars.stale_count === 1 ? '' : 's'} can be updated to v{t.vars.current_version}.
            </div>
          {/if}
          {#if t.approval_name === 'document-change' && t.vars}
            <div class="suggestion">
              <span class="chip mono">{fieldLabel(t.vars.field)}</span>
              <b>{suggestionValue(t.vars)}</b>
              <span class="sub">{Math.round(Number(t.vars.confidence || 0) * 100)}% confidence</span>
              {#if t.vars.source === 'archive' && t.vars.based_on?.length}
                <span class="sub">from {t.vars.based_on.length} similar documents</span>
              {:else if t.vars.source === 'llm'}
                <span class="sub">suggested by the configured LLM</span>
              {/if}
            </div>
          {/if}
          <div class="meta">
            {#if t.approval_name}<span class="pill ok">{t.approval_name}</span>{/if}
            {#if t.assignee}<span class="pill">{t.assignee}</span>{/if}
            <span>step <code>{t.state_key}</code></span>
            {#if t.doc_id}<a href={`#/doc/${t.doc_id}`}>document #{t.doc_id}</a>{/if}
            <span>opened {fmtDate(t.created_at)}</span>
            {#if dl}<span class:deadline-soon={dl.soon}>{dl.text}</span>{/if}
          </div>
          <div class="choices">
            {#each taskChoices(t) as c, i}
              <button class="btn sm" class:primary={i === 0} class:danger={/reject|deny|decline/i.test(c)}
                      onclick={() => resolve(t, c)}>
                {#if i === 0}<Icon name="check" size={13} />{/if}{choiceLabel(t, c)}
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
          <span class="sub">{retryLabel(j)}</span>
          {#if j.doc_id}<a class="btn sm" href={`#/doc/${j.doc_id}`}>Doc #{j.doc_id}</a>{/if}
          <button class="btn sm" disabled={busyJobs.has(j.id)} onclick={() => actOnDeadJob(j, 'retry')} title="Run this job again">
            <Icon name="refresh" size={13} /> Retry
          </button>
          <button class="btn sm" disabled={busyJobs.has(j.id)} onclick={() => actOnDeadJob(j, 'dismiss')} title="Remove this alert without retrying">
            <Icon name="x" size={13} /> Dismiss
          </button>
        </div>
      {/each}
    </div>
  {/if}

{/if}

<style>
  .suggestion { display:flex;align-items:center;gap:10px;flex-wrap:wrap;margin-top:8px }
</style>
