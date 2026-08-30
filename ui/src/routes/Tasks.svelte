<script>
  import { listTasks, resolveApprovalTask, retryDeadJob, dismissDeadJob, thumbPath,
           listIntelligence, resolveIntelligence } from '../lib/api.js'
  import { fmtDate } from '../lib/format.js'
  import { formatIntelligenceValue } from '../lib/intelligence.js'
  import Icon from '../lib/Icon.svelte'

  let { notify, onCount, canReviewIntelligence = false } = $props()
  let tasks = $state([])
  let jobs = $state([])
  let loading = $state(true)
  let err = $state('')
  let intelligence = $state([])
  let intelligenceSelection = $state(new Set())
  let intelligenceBusy = $state(false)
  let busyJobs = $state(new Set())

  async function load() {
    loading = true; err = ''
    try {
      const [wf, jb, facts] = await Promise.all([
        listTasks({ include: 'approvals', state: 'pending', limit: 200 }),
        listTasks({ include: 'jobs', state: 'dead', limit: 50 }),
        canReviewIntelligence
          ? listIntelligence({ status: 'pending', page_size: 200 })
          : Promise.resolve({ results: [] }),
      ])
      // `include=approvals` skips the classic tasks branch; approval tasks
      // and generic intelligence candidates use their own response fields.
      tasks = wf?.approval_tasks || []
      jobs = jb?.results || jb || []
      intelligence = facts?.results || []
      intelligenceSelection = new Set(
        intelligence.filter(candidate => Number(candidate.confidence || 0) >= 0.8).map(candidate => candidate.id),
      )
      onCount?.(tasks.length + jobs.length + intelligence.length)
    } catch (ex) { err = ex.message || 'Could not load tasks.' }
    finally { loading = false }
  }

  async function resolve(t, choice) {
    try {
      await resolveApprovalTask(t.id, { choice })
      tasks = tasks.filter(x => x.id !== t.id)
      onCount?.(tasks.length + jobs.length + intelligence.length)
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
    if (categoryAlreadyApplied(t)) return choices.filter(c => c === 'apply')
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
    if (t.approval_name === 'document-change') {
      if (choice === 'reject') return 'Dismiss'
      if (choice === 'apply') {
        if (categoryAlreadyApplied(t)) return 'Close review'
        return {
          jd_category: 'File document',
          correspondent: 'Set correspondent',
          document_type: 'Set document type',
          tag: 'Add tag',
          title: 'Change title',
        }[t.vars?.field] || 'Apply change'
      }
    }
    const text = choice.replaceAll('_', ' ')
    return text.charAt(0).toUpperCase() + text.slice(1)
  }

  function pipelineLabel(kind) {
    return { llm: 'LLM classification', ocr: 'OCR', content: 'content extraction' }[kind] || kind
  }

  function rescanTargets(t) {
    return Array.isArray(t.vars?.target_documents) ? t.vars.target_documents : []
  }

  function suggestionValue(vars) {
    return vars?.label || vars?.value || `#${vars?.value_id}`
  }

  function decisionPrompt(t) {
    if (t.approval_name !== 'document-change' || !t.vars) {
      return t.prompt || t.title || `Task #${t.id}`
    }
    const value = suggestionValue(t.vars)
    if (categoryAlreadyApplied(t)) return `Already filed under “${value}”`
    return {
      jd_category: `File under “${value}”?`,
      correspondent: `Set correspondent to “${value}”?`,
      document_type: `Set document type to “${value}”?`,
      tag: `Add “${value}” tag?`,
      title: `Change title to “${value}”?`,
    }[t.vars.field] || t.prompt
  }

  function filingLabel(t) {
    if (!t.doc_jd_category_name) return ''
    return t.doc_jd_category_code
      ? `${t.doc_jd_category_code} ${t.doc_jd_category_name}`
      : t.doc_jd_category_name
  }

  function categoryAlreadyApplied(t) {
    return t.vars?.field === 'jd_category' &&
      Number(t.doc_jd_category_id || 0) === Number(t.vars?.value_id || 0)
  }

  function reviewContext(t) {
    if (categoryAlreadyApplied(t)) {
      return 'The document is already filed there. No metadata change is needed, so this review can close.'
    }
    return ''
  }

  function evidenceLabel(t) {
    if (t.vars?.source === 'archive' && t.vars.based_on?.length) {
      return `Based on ${t.vars.based_on.length} similar documents`
    }
    if (t.vars?.source === 'llm') return 'Suggested by the configured LLM'
    return ''
  }

  function approvalGroups(items) {
    const groups = []
    const documents = new Map()
    for (const task of items) {
      if (task.approval_name === 'document-change' && task.doc_id) {
        let group = documents.get(task.doc_id)
        if (!group) {
          group = { key: `document-${task.doc_id}`, document: task, tasks: [] }
          documents.set(task.doc_id, group)
          groups.push(group)
        }
        group.tasks.push(task)
      } else {
        groups.push({ key: `task-${task.id}`, document: task.doc_id ? task : null, tasks: [task] })
      }
    }
    return groups
  }

  function groupContext(group) {
    const filed = filingLabel(group.document || {})
    if (!filed || !group.tasks.some(t => t.vars?.field !== 'jd_category')) return ''
    return `Filed under ${filed}. Review the remaining metadata suggestions independently.`
  }

  function intelligenceGroups() {
    const groups = new Map()
    for (const candidate of intelligence) {
      if (!groups.has(candidate.document_id)) {
        groups.set(candidate.document_id, {
          documentID: candidate.document_id,
          title: candidate.document_title,
          thumbnail: candidate.document_has_thumbnail,
          candidates: [],
        })
      }
      groups.get(candidate.document_id).candidates.push(candidate)
    }
    return [...groups.values()]
  }

  function selectionState(items) {
    const selected = items.filter(item => intelligenceSelection.has(item.id)).length
    return { all: selected === items.length && items.length > 0, some: selected > 0 && selected < items.length }
  }

  function indeterminate(node, value) {
    node.indeterminate = value
    return { update(next) { node.indeterminate = next } }
  }

  function setCandidateSelection(ids, checked) {
    const next = new Set(intelligenceSelection)
    for (const id of ids) checked ? next.add(id) : next.delete(id)
    intelligenceSelection = next
  }

  function toggleAllIntelligence(checked) {
    intelligenceSelection = checked
      ? new Set(intelligence.map(candidate => candidate.id))
      : new Set()
  }


  async function resolveSelectedIntelligence(decision) {
    const ids = [...intelligenceSelection]
    if (!ids.length || intelligenceBusy) return
    intelligenceBusy = true
    try {
      const result = await resolveIntelligence({ candidate_ids: ids, decision })
      const resolved = new Set((result?.results || []).filter(item => item.ok).map(item => item.id))
      intelligence = intelligence.filter(candidate => !resolved.has(candidate.id))
      intelligenceSelection = new Set([...intelligenceSelection].filter(id => !resolved.has(id)))
      onCount?.(tasks.length + jobs.length + intelligence.length)
      notify?.(decision === 'accepted'
        ? `Added ${resolved.size} date${resolved.size === 1 ? '' : 's'} to Calendar`
        : `Rejected ${resolved.size} date${resolved.size === 1 ? '' : 's'}`)
    } catch (ex) {
      notify?.(ex.message || 'Could not update dates')
    } finally {
      intelligenceBusy = false
    }
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
  {#if intelligence.length > 0}
    {@const allIntelligence = selectionState(intelligence)}
    {@const reviewGroups = intelligenceGroups()}
    <section class="intelligence-review" aria-labelledby="intelligence-review-title">
      <header class="intelligence-head">
        <div>
          <span class="eyebrow">Dates needing a quick check</span>
          <h2 id="intelligence-review-title">Check dates before they reach Calendar</h2>
          <p>{intelligence.length} date{intelligence.length === 1 ? '' : 's'} from {reviewGroups.length} document{reviewGroups.length === 1 ? '' : 's'} are waiting for your decision.</p>
          <p class="review-guidance">Uncheck anything incorrect, then add the selected dates to Calendar. Reject removes selected dates from this review.</p>
        </div>
        <label class="select-all">
          <input type="checkbox" checked={allIntelligence.all} use:indeterminate={allIntelligence.some}
                 onchange={(event) => toggleAllIntelligence(event.currentTarget.checked)} />
          Select all dates
        </label>
      </header>

      <div class="intelligence-actions" role="group" aria-label="Review selected dates">
        <span><b>{intelligenceSelection.size}</b> of {intelligence.length} date{intelligence.length === 1 ? '' : 's'} selected</span>
        <button class="btn sm" disabled={!intelligenceSelection.size || intelligenceBusy}
                onclick={() => resolveSelectedIntelligence('rejected')}>Reject {intelligenceSelection.size}</button>
        <button class="btn primary sm" disabled={!intelligenceSelection.size || intelligenceBusy}
                onclick={() => resolveSelectedIntelligence('accepted')}>
          <Icon name="check" size={13} /> {intelligenceBusy ? 'Saving…' : `Add ${intelligenceSelection.size} to Calendar`}
        </button>
      </div>

      <div class="intelligence-groups" class:review-grid={reviewGroups.length > 2}>
        {#each reviewGroups as group (group.documentID)}
          {@const groupSelection = selectionState(group.candidates)}
          <article class="intelligence-card">
            <header class="intelligence-document">
              <input type="checkbox" aria-label={`Select every candidate from ${group.title || `document ${group.documentID}`}`}
                     checked={groupSelection.all} use:indeterminate={groupSelection.some}
                     onchange={(event) => setCandidateSelection(group.candidates.map(candidate => candidate.id), event.currentTarget.checked)} />
              <a class="task-thumb intelligence-thumb" class:placeholder={!group.thumbnail}
                 href={`#/doc/${group.documentID}`} aria-label={`Open ${group.title || `document ${group.documentID}`}`}>
                {#if group.thumbnail}
                  <img src={thumbPath(group.documentID)} alt="" loading="lazy" />
                {:else}
                  <Icon name="docs" size={20} />
                {/if}
              </a>
              <div>
                <a href={`#/doc/${group.documentID}`}>{group.title || `Document #${group.documentID}`}</a>
                <small>{group.candidates.length} date{group.candidates.length === 1 ? '' : 's'} to check</small>
              </div>
            </header>
            <div class="intelligence-candidates">
              {#each group.candidates as candidate (candidate.id)}
                <label class="intelligence-candidate">
                  <input type="checkbox" checked={intelligenceSelection.has(candidate.id)}
                         onchange={(event) => setCandidateSelection([candidate.id], event.currentTarget.checked)} />
                  <span class="intelligence-copy">
                    <span class="intelligence-value">
                      <strong>{formatIntelligenceValue(candidate)}</strong>
                      <span class="pill">{candidate.type === 'date' ? 'Date' : candidate.type}</span>
                      <small>{Math.round(Number(candidate.confidence || 0) * 100)}% confidence</small>
                    </span>
                    <span class="intelligence-evidence">Document text: “{candidate.evidence_text}”</span>
                    <small>Date text: {candidate.raw_text}</small>
                  </span>
                </label>
              {/each}
            </div>
          </article>
        {/each}
      </div>

    </section>
  {/if}
  {#if tasks.length === 0 && jobs.length === 0 && intelligence.length === 0}
    <div class="empty"><Icon name="tasks" size={56} /><b>Nothing needs you.</b><span>The archive is running itself.</span></div>
  {:else if tasks.length > 0}
    <h3 style="font-size:.9rem;color:var(--muted);margin:14px 0 8px">Approvals</h3>
    <div class="approval-list">
      {#each approvalGroups(tasks) as group (group.key)}
        <div class="card task-card">
          {#if group.document}
            <div class="document-header">
              <a class="task-thumb" class:placeholder={!group.document.doc_has_thumbnail}
                 href={`#/doc/${group.document.doc_id}`}
                 aria-label={`Open ${group.document.doc_title || `document ${group.document.doc_id}`}`}>
                {#if group.document.doc_has_thumbnail}
                  <img src={thumbPath(group.document.doc_id)} alt="" loading="lazy" />
                {:else}
                  <Icon name="docs" size={22} />
                {/if}
              </a>
              <div class="document-identity">
                <a href={`#/doc/${group.document.doc_id}`}>
                  {group.document.doc_title || `Document #${group.document.doc_id}`}
                </a>
                <div class="document-meta">
                  <span>#{group.document.doc_id}</span>
                  {#if filingLabel(group.document)}<span class="pill">{filingLabel(group.document)}</span>{/if}
                  {#if group.tasks.length > 1}<span>{group.tasks.length} suggestions</span>{/if}
                </div>
              </div>
            </div>
            {#if groupContext(group)}<div class="group-context">{groupContext(group)}</div>{/if}
          {/if}
          <div class="decision-list">
            {#each group.tasks as t (t.id)}
              {@const dl = deadline(t)}
              {@const targets = rescanTargets(t)}
              <section class="decision-row">
                <div class="prompt decision">{decisionPrompt(t)}</div>
              {#if t.approval_name === 'rescan-proposal' && t.vars}
                <div class="rescan-context">
                  <b>{pipelineLabel(t.vars.kind)}</b> has a newer processing revision.
                  {t.vars.stale_count} document{t.vars.stale_count === 1 ? '' : 's'} can be updated to v{t.vars.current_version}.
                </div>
              {/if}
              {#if t.approval_name === 'document-change' && t.vars}
                {#if reviewContext(t)}<div class="review-context">{reviewContext(t)}</div>{/if}
                <div class="evidence">
                  <span>{Math.round(Number(t.vars.confidence || 0) * 100)}% confidence</span>
                  {#if evidenceLabel(t)}<span>{evidenceLabel(t)}</span>{/if}
                </div>
              {/if}
              <div class="choices">
                {#each taskChoices(t) as c, i}
                  <button class="btn sm" class:primary={i === 0} class:danger={/reject|deny|decline/i.test(c)}
                          onclick={() => resolve(t, c)}>
                    {#if i === 0}<Icon name="check" size={13} />{/if}{choiceLabel(t, c)}
                  </button>
                {/each}
              </div>
              <details class="task-details">
                <summary>Details</summary>
                {#if targets.length}
                  <div class="rescan-targets">
                    <b>Affected documents</b>
                    <ul>
                      {#each targets as target (target.id)}
                        <li><a href={`#/doc/${target.id}`}>{target.title || `Document #${target.id}`}</a></li>
                      {/each}
                    </ul>
                    {#if Number(t.vars?.stale_count || 0) > targets.length}
                      <span>and {Number(t.vars.stale_count) - targets.length} more</span>
                    {/if}
                  </div>
                {/if}
                <div class="meta">
                  {#if t.approval_name}<span>{t.approval_name}</span>{/if}
                  {#if t.assignee}<span>{t.assignee}</span>{/if}
                  <span>step <code>{t.state_key}</code></span>
                  <span>opened {fmtDate(t.created_at)}</span>
                  {#if dl}<span class:deadline-soon={dl.soon}>{dl.text}</span>{/if}
                </div>
              </details>
              </section>
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
  .intelligence-review { margin: 10px 0 28px; border: 1px solid var(--line-strong); border-radius: var(--r); background: var(--surface); }
  .intelligence-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 20px; padding: 20px; border-bottom: 1px solid var(--line); border-radius: var(--r) var(--r) 0 0; background: linear-gradient(135deg, var(--tint), var(--surface)); }
  .intelligence-head .eyebrow { display: block; margin-bottom: 5px; color: var(--accent); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .63rem; font-weight: 700; }
  .intelligence-head h2 { font-size: 1.22rem; }
  .intelligence-head p { max-width: 780px; margin: 5px 0 0; color: var(--muted); font-size: .8rem; line-height: 1.45; }
  .intelligence-head .review-guidance { color: var(--ink); font-size: .76rem; }
  .select-all { display: flex; align-items: center; gap: 7px; flex: none; font-size: .75rem; font-weight: 650; cursor: pointer; }
  .intelligence-groups { display: grid; gap: 12px; padding: 14px; }
  .intelligence-groups.review-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); align-items: start; }
  .intelligence-card { border: 1px solid var(--line); border-radius: 11px; overflow: hidden; background: var(--bg); }
  .intelligence-document { display: grid; grid-template-columns: auto auto minmax(0, 1fr); align-items: center; gap: 11px; padding: 10px 12px; border-bottom: 1px solid var(--line); background: var(--surface-2); }
  .intelligence-document > div { display: flex; min-width: 0; flex-direction: column; gap: 3px; }
  .intelligence-document a { overflow: hidden; color: var(--ink); font-size: .84rem; font-weight: 650; text-overflow: ellipsis; white-space: nowrap; }
  .intelligence-document small { color: var(--muted); font-size: .68rem; }
  .intelligence-thumb { width: 38px; flex-basis: 38px; }
  .intelligence-candidates { display: flex; flex-direction: column; }
  .intelligence-candidate { display: grid; grid-template-columns: auto minmax(0, 1fr); gap: 11px; padding: 13px 14px; border-bottom: 1px solid var(--line); cursor: pointer; }
  .intelligence-candidate:last-child { border-bottom: 0; }
  .intelligence-candidate:hover { background: var(--tint); }
  .intelligence-candidate > input { margin-top: 3px; }
  .intelligence-copy { display: flex; min-width: 0; flex-direction: column; gap: 5px; }
  .intelligence-value { display: flex; align-items: center; gap: 7px; flex-wrap: wrap; }
  .intelligence-value strong { font-size: .87rem; text-transform: capitalize; }
  .intelligence-value small { color: var(--faint); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .64rem; }
  .intelligence-evidence { color: var(--muted); font-size: .78rem; line-height: 1.5; }
  .intelligence-copy > small { color: var(--faint); font-size: .67rem; }
  .intelligence-actions { position: sticky; top: 0; z-index: 12; display: flex; align-items: center; justify-content: flex-end; gap: 8px; padding: 10px 14px; border-bottom: 1px solid var(--line-strong); background: color-mix(in srgb, var(--surface) 96%, transparent); box-shadow: 0 8px 18px color-mix(in srgb, var(--ink) 7%, transparent); backdrop-filter: blur(8px); }
  .intelligence-actions > span { margin-right: auto; color: var(--muted); font-size: .72rem; }
  .intelligence-actions > span b { color: var(--ink); }
  .approval-list { display:flex;flex-direction:column;gap:12px;margin-bottom:22px }
  .document-header { display:flex;gap:14px;align-items:center }
  .document-identity { flex:1;min-width:0 }
  .document-identity > a { color:var(--ink);font-weight:650;overflow-wrap:anywhere }
  .document-meta { display:flex;align-items:center;gap:7px;flex-wrap:wrap;color:var(--muted);font-size:.76rem;margin-top:6px }
  .decision { font-size:1rem;line-height:1.35;overflow-wrap:anywhere }
  .task-thumb {
    display:flex;align-items:center;justify-content:center;flex:0 0 64px;width:64px;aspect-ratio:3 / 4;
    border:1px solid var(--line);border-radius:6px;overflow:hidden;background:var(--surface-2)
  }
  .task-thumb img { width:100%;height:100%;object-fit:cover;display:block }
  .task-thumb.placeholder { color:var(--faint) }
  .group-context { color:var(--muted);font-size:.82rem;line-height:1.45;margin-top:10px }
  .decision-list { margin-top:14px;border-top:1px solid var(--line) }
  .decision-row { padding:15px 0;border-bottom:1px solid var(--line) }
  .decision-row:last-child { padding-bottom:0;border-bottom:0 }
  .task-card > .decision-list:first-child { margin-top:0;border-top:0 }
  .review-context, .rescan-context { color:var(--muted);font-size:.84rem;line-height:1.45;margin-top:10px }
  .evidence { display:flex;gap:6px 14px;flex-wrap:wrap;color:var(--muted);font-size:.78rem;margin-top:5px }
  .choices { margin-top:14px }
  .task-details { margin-top:10px;color:var(--muted);font-size:.75rem }
  .task-details summary { cursor:pointer;width:max-content }
  .task-details .meta { margin-top:6px }
  .rescan-targets { margin-top:9px;max-width:720px }
  .rescan-targets b { color:var(--ink);font-size:.78rem }
  .rescan-targets ul { margin:5px 0 3px;padding-left:18px }
  .rescan-targets li { margin:3px 0;overflow-wrap:anywhere }
  .rescan-targets a { color:var(--accent) }
  @media (max-width: 1050px) {
    .intelligence-groups.review-grid { grid-template-columns: 1fr; }
  }
  @media (max-width: 560px) {
    .task-thumb { flex-basis:52px;width:52px }
    .intelligence-head { align-items: flex-start; flex-direction: column; }
    .intelligence-actions { flex-wrap: wrap; }
    .intelligence-actions > span { width: 100%; }
    .intelligence-actions .btn { flex: 1; justify-content: center; }
  }
</style>
