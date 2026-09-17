<script>
  import { scopedHash as filingHref } from '../lib/systems.svelte.js'
  import { listTasks, resolveApprovalTask, retryDeadJob, dismissDeadJob, thumbPath,
           listIntelligence, resolveIntelligence } from '../lib/api.js'
  import { fmtDate } from '../lib/format.js'
  import { DATE_ROLES, formatIntelligenceValue, canReviewDate, reviewReason, reviewFailure } from '../lib/intelligence.js'
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
  let busyTasks = $state(new Set())
  let taskErrors = $state({})
  let dateErrors = $state({})
  let reviewNotice = $state('')

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
      intelligenceSelection = new Set()
      taskErrors = {}
      dateErrors = {}
      onCount?.(tasks.length + jobs.length + intelligence.length)
    } catch (ex) {
      tasks = []
      jobs = []
      intelligence = []
      intelligenceSelection = new Set()
      err = reviewFailure(ex)
    }
    finally { loading = false }
  }

  async function resolve(t, choice) {
    if (busyTasks.has(t.id) || taskErrors[t.id] || !taskChoices(t).includes(choice)) return
    busyTasks = new Set([...busyTasks, t.id])
    try {
      await resolveApprovalTask(t.id, { choice })
      tasks = tasks.filter(x => x.id !== t.id)
      onCount?.(tasks.length + jobs.length + intelligence.length)
      reviewNotice = choice === 'apply'
        ? 'Decision recorded. The change is queued, not yet applied; its source and current value will be checked again before application.'
        : choice === 'reject' || choice === 'dismiss'
          ? 'Suggestion dismissed.'
          : 'Decision recorded. Any requested processing is queued, not yet complete.'
      notify?.(reviewNotice)
    } catch (ex) {
      taskErrors = { ...taskErrors, [t.id]: reviewFailure(ex) }
    } finally {
      const next = new Set(busyTasks)
      next.delete(t.id)
      busyTasks = next
    }
  }

  function deadline(t) {
    if (!t.deadline_at) return null
    const hrs = (t.deadline_at * 1000 - Date.now()) / 36e5
    if (hrs < 0) return { text: 'deadline passed', soon: true }
    if (hrs < 24) return { text: `${Math.max(1, Math.round(hrs))}h left`, soon: true }
    return { text: `due ${fmtDate(t.deadline_at)}`, soon: false }
  }

  function taskChoices(t) {
    const choices = Array.isArray(t.choices) ? t.choices : []
    if (t.approval_name === 'document-change') {
      if (!['jd_category', 'correspondent', 'document_type', 'tag', 'title', 'language'].includes(t.vars?.field)) return []
      return choices.filter(choice => choice === 'reject' || (choice === 'apply' &&
        t.vars?.source_current === true && t.vars?.review_conflict === false &&
        ['review_first', 'low_confidence'].includes(t.vars?.reason)))
    }
    if (t.approval_name === 'rescan-proposal') {
      return choices.filter(choice => ['approve_all', 'approve_sample', 'dismiss'].includes(choice) &&
        (choice !== 'approve_sample' || Number(t.vars?.stale_count || 0) > 20))
    }
    return []
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
        // Recording this choice schedules the existing asynchronous application.
        return {
          jd_category: 'File document',
          correspondent: 'Set correspondent',
          document_type: 'Set document type',
          tag: 'Add tag',
          title: 'Change title',
          language: 'Set language',
        }[t.vars?.field] || 'Apply change'
      }
    }
    const text = choice.replaceAll('_', ' ')
    return text.charAt(0).toUpperCase() + text.slice(1)
  }

  function pipelineLabel(kind) {
    return { llm: 'LLM classification', ocr: 'OCR', content: 'Text extraction' }[kind] || kind
  }

  function rescanTargets(t) {
    return Array.isArray(t.vars?.target_documents) ? t.vars.target_documents : []
  }

  function suggestionValue(vars) {
    return typeof vars?.proposed_value === 'string' ? vars.proposed_value : 'Unavailable'
  }

  function decisionPrompt(t) {
    if (t.approval_name !== 'document-change' || !t.vars) {
      return t.prompt || t.title || `Task #${t.id}`
    }
    const value = suggestionValue(t.vars)
    if (!['jd_category', 'correspondent', 'document_type', 'tag', 'title', 'language'].includes(t.vars.field)) return 'Unsupported action · read-only'
    return {
      jd_category: `File under “${value}”?`,
      correspondent: `Set correspondent to “${value}”?`,
      document_type: `Set document type to “${value}”?`,
      tag: `Add “${value}” tag?`,
      title: `Change title to “${value}”?`,
      language: `Set language to “${value}”?`,
    }[t.vars.field] || t.prompt
  }

  function filingLabel(t) {
    if (!t.doc_jd_category_name) return ''
    return t.doc_jd_category_code
      ? `${t.doc_jd_category_code} ${t.doc_jd_category_name}`
      : t.doc_jd_category_name
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
    const available = items.filter(candidate => canReviewDate(candidate) && !dateErrors[candidate.id])
    const selected = available.filter(item => intelligenceSelection.has(item.id)).length
    return { all: selected === available.length && available.length > 0, some: selected > 0 && selected < available.length, available: available.length }
  }

  function indeterminate(node, value) {
    node.indeterminate = value
    return { update(next) { node.indeterminate = next } }
  }

  function setCandidateSelection(ids, checked) {
    if (intelligenceBusy) return
    const next = new Set(intelligenceSelection)
    const allowed = new Set(intelligence.filter(candidate => canReviewDate(candidate) && !dateErrors[candidate.id]).map(candidate => candidate.id))
    for (const id of ids) {
      if (checked && allowed.has(id)) next.add(id)
      else next.delete(id)
    }
    intelligenceSelection = next
  }

  function toggleAllIntelligence(checked) {
    setCandidateSelection(intelligence.map(candidate => candidate.id), checked)
  }


  async function resolveSelectedIntelligence(decision, candidateIDs = null) {
    const ids = candidateIDs || intelligence.filter(candidate => intelligenceSelection.has(candidate.id) && canReviewDate(candidate) && !dateErrors[candidate.id]).map(candidate => candidate.id)
    if (!ids.length || intelligenceBusy) return
    intelligenceBusy = true
    try {
      const result = await resolveIntelligence({ candidate_ids: ids, decision })
      const outcomes = new Map((result?.results || []).map(item => [item.id, item]))
      const resolved = new Set(ids.filter(id => outcomes.get(id)?.ok === true))
      const failures = { ...dateErrors }
      for (const id of ids) {
        if (!resolved.has(id)) failures[id] = reviewFailure(outcomes.get(id))
      }
      dateErrors = failures
      intelligence = intelligence.filter(candidate => !resolved.has(candidate.id))
      intelligenceSelection = new Set()
      onCount?.(tasks.length + jobs.length + intelligence.length)
      reviewNotice = resolved.size
        ? (decision === 'accepted'
          ? `Added ${resolved.size} date${resolved.size === 1 ? '' : 's'} to Calendar.`
          : `Rejected ${resolved.size} date${resolved.size === 1 ? '' : 's'}.`)
        : 'No dates were changed.'
      if (resolved.size < ids.length) reviewNotice += ' Some decisions could not be completed; check the messages below and refresh.'
      notify?.(reviewNotice)
    } catch (ex) {
      const failures = { ...dateErrors }
      for (const id of ids) failures[id] = reviewFailure(ex)
      dateErrors = failures
      intelligenceSelection = new Set()
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
      const choice = taskChoices(t)[0]
      if (choice) resolve(t, choice)
    }
  }

  load()
</script>

<svelte:window onkeydown={onKey} />

{#if err}<div class="err">{err}</div>{/if}
<div class="review-toolbar">
  <p role="status">{reviewNotice}</p>
  <button class="btn sm" disabled={loading || intelligenceBusy || busyTasks.size > 0} onclick={load}>Refresh reviews</button>
</div>

{#if loading}
  <div class="index">{#each Array(3) as _}<div class="irow"><div class="skel" style="width:55%"></div></div>{/each}</div>
{:else}
  {#if intelligence.length > 0}
    {@const allIntelligence = selectionState(intelligence)}
    {@const reviewGroups = intelligenceGroups()}
    <section class="intelligence-review" aria-labelledby="intelligence-review-title">
      <header class="intelligence-head">
        <div>
          <span class="eyebrow">Dates needing review</span>
          <h2 id="intelligence-review-title">Check dates before they reach Calendar</h2>
          <p>{intelligence.length} date{intelligence.length === 1 ? '' : 's'} from {reviewGroups.length} document{reviewGroups.length === 1 ? '' : 's'} are waiting for your decision.</p>
          <p class="review-guidance">Check each proposed date against its source, then select dates to add to Calendar without replacing existing dates. Scores are not measured accuracy and never select dates for you.</p>
        </div>
        <label class="select-all">
          <input type="checkbox" disabled={intelligenceBusy || !allIntelligence.available} checked={allIntelligence.all} use:indeterminate={allIntelligence.some}
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

      <div class="approval-grid" class:approval-grid-many={reviewGroups.length > 2} data-approval-kind="date">
        {#each reviewGroups as group (group.documentID)}
          {@const groupSelection = selectionState(group.candidates)}
          <article class="approval-card intelligence-card">
            <header class="approval-card-header intelligence-document">
              <input type="checkbox" aria-label={`Select every candidate from ${group.title || `document ${group.documentID}`}`}
                     disabled={intelligenceBusy || !groupSelection.available} checked={groupSelection.all} use:indeterminate={groupSelection.some}
                     onchange={(event) => setCandidateSelection(group.candidates.map(candidate => candidate.id), event.currentTarget.checked)} />
              <a class="task-thumb intelligence-thumb" class:placeholder={!group.thumbnail}
                 href={filingHref(`#/doc/${group.documentID}`)} aria-label={`Open ${group.title || `document ${group.documentID}`}`}>
                {#if group.thumbnail}
                  <img src={thumbPath(group.documentID)} alt="" loading="lazy" />
                {:else}
                  <Icon name="docs" size={20} />
                {/if}
              </a>
              <div>
                <a href={filingHref(`#/doc/${group.documentID}`)}>{group.title || `Document #${group.documentID}`}</a>
                <small>{group.candidates.length} date{group.candidates.length === 1 ? '' : 's'} to check</small>
              </div>
            </header>
            <div class="approval-card-body intelligence-candidates">
              {#each group.candidates as candidate (candidate.id)}
                <div class="intelligence-candidate">
                  <input type="checkbox" aria-label={`Select proposed ${formatIntelligenceValue(candidate)}`}
                         checked={intelligenceSelection.has(candidate.id)}
                         disabled={intelligenceBusy || !canReviewDate(candidate) || !!dateErrors[candidate.id]}
                         onchange={(event) => setCandidateSelection([candidate.id], event.currentTarget.checked)} />
                  <div class="intelligence-copy">
                    <div class="intelligence-value">
                      <strong>Proposed: {formatIntelligenceValue(candidate)}</strong>
                      <span class="pill">{candidate.type === 'date' ? 'Date' : 'Read-only'}</span>
                    </div>
                    <p class="gate-reason">{reviewReason(candidate.type === 'date' ? candidate.reason : 'unsupported')}</p>
                    <small>{candidate.source_current === true ? 'Source checked at refresh; checked again when saving.' : 'Source is not current or could not be verified. Not available for acceptance.'}</small>
                    {#if candidate.evidence_text}
                      <blockquote class="intelligence-evidence">“{candidate.evidence_text}”</blockquote>
                      <small>{candidate.source_current === true ? 'Exact text from the linked document' : 'Stored extraction quote; not verified against the current document'}{Number.isInteger(candidate.evidence_start) ? ` · UTF-8 byte ${candidate.evidence_start}` : ''}</small>
                    {:else}
                      <small>Evidence is unavailable here. Open the source document; sensitive text uses its existing reveal controls.</small>
                    {/if}
                    {#if typeof candidate.confidence === 'number'}
                      <details class="task-details">
                        <summary>Producer detail</summary>
                        <p>Score: {candidate.confidence.toFixed(2)} · not measured accuracy.</p>
                        {#if candidate.extractor}<p>Producer: {candidate.extractor}</p>{/if}
                      </details>
                    {/if}
                    {#if dateErrors[candidate.id]}<p class="review-error" role="alert">{dateErrors[candidate.id]}</p>{/if}
                    {#if candidate.type === 'date' && DATE_ROLES.includes(candidate.role) && candidate.status === 'pending' && candidate.source_current === false}
                      <button class="btn sm" disabled={intelligenceBusy}
                              onclick={() => resolveSelectedIntelligence('rejected', [candidate.id])}>Reject this suggestion</button>
                    {/if}
                  </div>
                </div>
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
    {@const workflowGroups = approvalGroups(tasks)}
    <h3 style="font-size:.9rem;color:var(--muted);margin:14px 0 8px">Approvals</h3>
    <div class="approval-grid workflow-approval-grid" class:approval-grid-many={workflowGroups.length > 2} data-approval-kind="workflow">
      {#each workflowGroups as group (group.key)}
        <article class="approval-card workflow-approval-card">
          {#if group.document}
            <header class="approval-card-header document-header">
              <a class="task-thumb" class:placeholder={!group.document.doc_has_thumbnail}
                 href={filingHref(`#/doc/${group.document.doc_id}`)}
                 aria-label={`Open ${group.document.doc_title || `document ${group.document.doc_id}`}`}>
                {#if group.document.doc_has_thumbnail}
                  <img src={thumbPath(group.document.doc_id)} alt="" loading="lazy" />
                {:else}
                  <Icon name="docs" size={22} />
                {/if}
              </a>
              <div class="document-identity">
                <a href={filingHref(`#/doc/${group.document.doc_id}`)}>
                  {group.document.doc_title || `Document #${group.document.doc_id}`}
                </a>
                <div class="document-meta">
                  <span>#{group.document.doc_id}</span>
                  {#if filingLabel(group.document)}<span class="pill">{filingLabel(group.document)}</span>{/if}
                  {#if group.tasks.length > 1}<span>{group.tasks.length} suggestions</span>{/if}
                </div>
              </div>
            </header>
            {#if groupContext(group)}<div class="group-context">{groupContext(group)}</div>{/if}
          {:else}
            <header class="approval-card-header operation-header">
              <span class="operation-mark"><Icon name="refresh" size={17} /></span>
              <span><small>Archive operation</small><b>{group.tasks[0]?.approval_name === 'rescan-proposal' ? 'Processing update' : 'Approval request'}</b></span>
            </header>
          {/if}
          <div class="approval-card-body decision-list">
            {#each group.tasks as t (t.id)}
              {@const dl = deadline(t)}
              {@const targets = rescanTargets(t)}
              <section class="decision-row">
                <div class="prompt decision">{decisionPrompt(t)}</div>
              {#if t.approval_name === 'rescan-proposal' && t.vars}
                <div class="rescan-context">
                  <b>{pipelineLabel(t.vars.kind)}</b> has improved.
                  {t.vars.stale_count} document{t.vars.stale_count === 1 ? '' : 's'} can be updated.
                </div>
              {/if}
              {#if t.approval_name === 'document-change' && t.vars}
                <dl class="metadata-values">
                  <div><dt>Current</dt><dd>{typeof t.vars.current_value === 'string' ? (t.vars.current_value || 'Not set') : 'Unavailable'}</dd></div>
                  <div><dt>Proposed</dt><dd>{suggestionValue(t.vars)}</dd></div>
                </dl>
                <p class="gate-reason">{reviewReason(t.vars.reason)}</p>
                <p class="review-context">{t.vars.source_current === true ? 'Source checked at refresh; checked again before application.' : 'Source is not current or could not be verified.'}{t.vars.review_conflict ? ' The current value conflicts with this suggestion.' : ''}</p>
                {#if t.vars.evidence_text}<blockquote class="intelligence-evidence">“{t.vars.evidence_text}”</blockquote>{/if}
                {#if t.vars.sources?.length}
                  <ul class="review-sources">
                    {#each t.vars.sources as source}
                      <li><a href={filingHref(`#/doc/${source.document_id}`)}>{source.title || 'Open source document'}</a></li>
                    {/each}
                  </ul>
                {/if}
                {#if typeof t.vars.confidence === 'number'}
                  <details class="task-details">
                    <summary>Producer detail</summary>
                    <p>Score: {t.vars.confidence.toFixed(2)} · not measured accuracy.</p>
                    {#if t.vars.source === 'archive'}<p>Suggested from authorized archive sources.</p>
                    {:else if t.vars.source === 'llm'}<p>Suggested by the configured model.</p>{/if}
                  </details>
                {/if}
              {/if}
              {#if taskErrors[t.id]}<p class="review-error" role="alert">{taskErrors[t.id]}</p>{/if}
              {#if !taskChoices(t).length}<p class="review-context">Read-only: this action is not available for review here.</p>{/if}
              <div class="choices">
                {#each taskChoices(t) as c, i}
                  <button class="btn sm" class:primary={i === 0} class:danger={/reject|deny|decline/i.test(c)}
                          disabled={busyTasks.has(t.id) || !!taskErrors[t.id]} onclick={() => resolve(t, c)}>
                    {#if i === 0}<Icon name="check" size={13} />{/if}{choiceLabel(t, c)}
                  </button>
                {/each}
              </div>
              {#if dl}<div class="meta" class:deadline-soon={dl.soon}>{dl.text}</div>{/if}
              {#if targets.length}
                <details class="task-details">
                  <summary>Affected documents</summary>
                  <div class="rescan-targets">
                    <ul>
                      {#each targets as target (target.id)}
                        <li><a href={filingHref(`#/doc/${target.id}`)}>{target.title || `Document #${target.id}`}</a></li>
                      {/each}
                    </ul>
                    {#if Number(t.vars?.stale_count || 0) > targets.length}
                      <span>and {Number(t.vars.stale_count) - targets.length} more</span>
                    {/if}
                  </div>
                </details>
              {/if}
              </section>
            {/each}
          </div>
        </article>
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
          {#if j.doc_id}<a class="btn sm" href={filingHref(`#/doc/${j.doc_id}`)}>Doc #{j.doc_id}</a>{/if}
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
  .approval-grid { display: grid; max-width: 820px; gap: 12px; align-items: start; }
  .intelligence-review .approval-grid { padding: 14px; }
  .approval-grid.approval-grid-many { max-width: none; grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .approval-card { min-width: 0; border: 1px solid var(--line); border-radius: 11px; overflow: hidden; background: var(--bg); }
  .approval-card-header { border-bottom: 1px solid var(--line); background: var(--surface-2); }
  .approval-card-body { background: var(--bg); }
  .intelligence-document { display: grid; grid-template-columns: auto auto minmax(0, 1fr); align-items: center; gap: 11px; padding: 10px 12px; border-bottom: 1px solid var(--line); background: var(--surface-2); }
  .intelligence-document > div { display: flex; min-width: 0; flex-direction: column; gap: 3px; }
  .intelligence-document a { overflow: hidden; color: var(--ink); font-size: .84rem; font-weight: 650; text-overflow: ellipsis; white-space: nowrap; }
  .intelligence-document small { color: var(--muted); font-size: .68rem; }
  .intelligence-thumb { width: 38px; flex-basis: 38px; }
  .intelligence-candidates { display: flex; flex-direction: column; }
  .intelligence-candidate { display: grid; grid-template-columns: auto minmax(0, 1fr); gap: 11px; padding: 13px 14px; border-bottom: 1px solid var(--line); }
  .intelligence-candidate:last-child { border-bottom: 0; }
  .intelligence-candidate:hover { background: var(--tint); }
  .intelligence-candidate > input { margin-top: 3px; }
  .intelligence-copy { display: flex; min-width: 0; flex-direction: column; gap: 5px; }
  .intelligence-value { display: flex; align-items: center; gap: 7px; flex-wrap: wrap; }
  .intelligence-value strong { font-size: .87rem; text-transform: capitalize; }
  .intelligence-evidence { color: var(--muted); font-size: .78rem; line-height: 1.5; }
  .intelligence-copy > small { color: var(--faint); font-size: .67rem; }
  .intelligence-actions { position: sticky; top: 0; z-index: 12; display: flex; align-items: center; justify-content: flex-end; gap: 8px; padding: 10px 14px; border-bottom: 1px solid var(--line-strong); background: color-mix(in srgb, var(--surface) 96%, transparent); box-shadow: 0 8px 18px color-mix(in srgb, var(--ink) 7%, transparent); backdrop-filter: blur(8px); }
  .intelligence-actions > span { margin-right: auto; color: var(--muted); font-size: .72rem; }
  .intelligence-actions > span b { color: var(--ink); }
  .workflow-approval-grid { margin-bottom:22px }
  .document-header { display:flex;gap:11px;align-items:center;padding:11px 13px }
  .document-identity { flex:1;min-width:0 }
  .document-identity > a { color:var(--ink);font-weight:650;overflow-wrap:anywhere }
  .document-meta { display:flex;align-items:center;gap:7px;flex-wrap:wrap;color:var(--muted);font-size:.76rem;margin-top:6px }
  .decision { font-size:1rem;line-height:1.35;overflow-wrap:anywhere }
  .task-thumb {
    display:flex;align-items:center;justify-content:center;flex:0 0 46px;width:46px;aspect-ratio:3 / 4;
    border:1px solid var(--line);border-radius:6px;overflow:hidden;background:var(--surface-2)
  }
  .task-thumb img { width:100%;height:100%;object-fit:cover;display:block }
  .task-thumb.placeholder { color:var(--faint) }
  .group-context { padding:9px 13px;border-bottom:1px solid var(--line);color:var(--muted);font-size:.78rem;line-height:1.45;background:var(--surface) }
  .decision-list { padding:0 13px }
  .decision-row { padding:15px 0;border-bottom:1px solid var(--line) }
  .decision-row:last-child { border-bottom:0 }
  .operation-header { display:flex;align-items:center;gap:10px;padding:11px 13px }
  .operation-header > span:last-child { display:flex;flex-direction:column;gap:2px }
  .operation-header small { color:var(--faint);font-size:.61rem }
  .operation-header b { font-size:.82rem }
  .operation-mark { display:grid;place-items:center;width:35px;height:35px;border:1px solid var(--line);border-radius:8px;color:var(--accent);background:var(--bg) }
  .review-context, .rescan-context { color:var(--muted);font-size:.84rem;line-height:1.45;margin-top:10px }
  .choices { display:flex;flex-wrap:wrap;gap:8px;margin-top:14px }
  .task-details { margin-top:10px;color:var(--muted);font-size:.75rem }
  .task-details summary { cursor:pointer;width:max-content }
  .rescan-targets { margin-top:9px;max-width:720px }
  .rescan-targets ul { margin:5px 0 3px;padding-left:18px }
  .rescan-targets li { margin:3px 0;overflow-wrap:anywhere }
  .rescan-targets a { color:var(--accent) }
  .review-toolbar { display:flex;align-items:center;justify-content:space-between;gap:12px;margin:10px 0 }
  .review-toolbar p { min-width:0;font-size:.8rem;color:var(--muted) }
  .review-toolbar button { flex:none }
  .metadata-values { display:grid;gap:8px;margin:12px 0 }
  .metadata-values > div { display:grid;grid-template-columns:65px minmax(0,1fr);gap:10px }
  .metadata-values dt { color:var(--muted);font-size:.76rem }
  .metadata-values dd { margin:0;font-size:.84rem;overflow-wrap:anywhere }
  .gate-reason { margin:6px 0;font-size:.8rem;line-height:1.45 }
  .intelligence-evidence { margin:6px 0;white-space:pre-wrap;overflow-wrap:anywhere }
  .review-sources { margin:8px 0;padding-left:18px;font-size:.78rem;overflow-wrap:anywhere }
  .review-error { color:var(--danger);font-size:.8rem;line-height:1.45;overflow-wrap:anywhere }
  @media (max-width: 1050px) {
    .approval-grid.approval-grid-many { grid-template-columns: 1fr; }
  }
  @media (max-width: 560px) {
    .task-thumb { flex-basis:42px;width:42px }
    .review-toolbar { align-items:flex-start;flex-direction:column }
    .intelligence-head { align-items: flex-start; flex-direction: column; }
    .intelligence-actions { flex-wrap: wrap; }
    .intelligence-actions > span { width: 100%; }
    .intelligence-actions .btn { flex: 1; justify-content: center; }
  }
</style>
