<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "../lib/i18n.svelte.js";
  import { scopedHash as filingHref } from '../lib/systems.svelte.js'
  import { listTasks, resolveApprovalTask, retryDeadJob, dismissDeadJob, thumbPath,
           listIntelligence, resolveIntelligence } from '../lib/api.js'
  import { fmtDate, formatNumber, isHighSensitivity, plural } from '../lib/format.js'
  import { DATE_ROLES, formatIntelligenceValue, canReviewDate, reviewFailure } from '../lib/intelligence.js'
  import Icon from '../lib/Icon.svelte'

  let { notify, onCount, canReviewIntelligence = false, canManageJobs = false } = $props()
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
        canManageJobs
          ? listTasks({ include: 'jobs', state: 'dead', limit: 50 })
          : Promise.resolve({ results: [] }),
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
        ? translate("tasks.decisionRecordedTheDocumentAndCurrentValueWill")
        : choice === 'reject' || choice === 'dismiss'
          ? translate("tasks.suggestionDismissed")
          : translate("tasks.decisionRecordedAnyRequestedProcessingWillContinueShortly")
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
    if (hrs < 0) return { text: translate("tasks.deadlinePassed"), soon: true }
    if (hrs < 24) return { text: translate("tasks.valueHLeft", { "hrs": Math.max(1, Math.round(hrs)) }), soon: true }
    return { text: translate("tasks.dueValue", { "deadline_at": fmtDate(t.deadline_at) }), soon: false }
  }

  function taskChoices(t) {
    const choices = Array.isArray(t.choices) ? t.choices : []
    if (t.approval_name === 'document-change') {
      if (!['jd_category', 'correspondent', 'tag', 'title', 'language', 'custom_field'].includes(t.vars?.field)) return []
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
      if (choice === 'approve_all') return count ? translate("tasks.rescanAllValue", { count: formatNumber(count) }) : translate("tasks.rescanAll")
      if (choice === 'approve_sample') return translate("tasks.try20First")
      if (choice === 'dismiss') return translate("tasks.dismiss")
    }
    if (t.approval_name === 'document-change') {
      if (choice === 'reject') return translate("tasks.dismiss")
      if (choice === 'apply') {
        // Recording this choice schedules the existing asynchronous application.
        return {
          jd_category: translate("tasks.fileDocument"),
          correspondent: translate("tasks.setCorrespondent"),
          tag: translate("tasks.addTag"),
          title: translate("tasks.changeTitle"),
          language: translate("tasks.setLanguage"),
          custom_field: translate("tasks.setCustomField"),
        }[t.vars?.field] || translate("tasks.applyChange")
      }
    }
    return {
      approve: translate("tasks.approve"),
      reject: translate("tasks.reject"),
    }[choice] || choice
  }

  function pipelineLabel(kind) {
    return { llm: translate("tasks.documentSuggestions"), ocr: translate("tasks.textRecognitionOcr"), content: translate("tasks.documentText") }[kind] || kind
  }

  function producerLabel(producer) {
    return {
      'llm-classifier': translate("tasks.documentModel"),
      'demo-corpus': translate("tasks.demoExample"),
      legacy: translate("tasks.earlierSuchiVersion"),
    }[producer] || String(producer || '').replace(/[-_:]+/g, ' ')
  }

  function jobKindLabel(kind) {
    const labels = {
      'post-ingest': translate("tasks.readAndFileDocument"),
      'post-classify': translate("tasks.suggestDocumentDetails"),
      render: translate("tasks.updateFiledCopy"),
      'approval:advance': translate("tasks.applyApproval"),
      'approval:timeout-sweep': translate("tasks.updateExpiredApprovals"),
      taxonomy_index: translate("tasks.updateFilingIndex"),
      retention: translate("tasks.emptyExpiredTrash"),
      maintenance: translate("tasks.archiveMaintenance"),
    }
    return labels[kind] || kind || translate("tasks.processingStep")
  }

  function rescanTargets(t) {
    return Array.isArray(t.vars?.target_documents) ? t.vars.target_documents : []
  }

  function suggestionValue(vars) {
    return typeof vars?.proposed_value === 'string' ? vars.proposed_value : translate("tasks.unavailable")
  }

  function sourceLabel(source) {
    return {
      archive: translate("tasks.archiveMatch"),
      llm: translate("tasks.modelSuggestion"),
      'language-detector': translate("tasks.languageDetection"),
    }[source] || source
  }

  function supportingContextLabel(vars) {
    const count = Array.isArray(vars?.sources) ? vars.sources.length : 0
    return count ? plural("tasks.similarDocumentCount", count) : translate("tasks.textBehindThisSuggestion")
  }

  function decisionPrompt(t) {
    if (t.approval_name !== 'document-change' || !t.vars) {
      return t.prompt || t.title || translate("tasks.taskValue", { "id": t.id })
    }
    const value = suggestionValue(t.vars)
    if (!['jd_category', 'correspondent', 'tag', 'title', 'language', 'custom_field'].includes(t.vars.field)) return translate("tasks.unsupportedActionReadOnly")
    const action = {
      jd_category: translate("tasks.filingUnderValue", { "value": value }),
      correspondent: translate("tasks.settingTheCorrespondentToValue", { "value": value }),
      tag: translate("tasks.addingTheValueTag", { "value": value }),
      title: translate("tasks.changingTheTitleToValue", { "value": value }),
      language: translate("tasks.settingTheLanguageToValue", { "value": value }),
      custom_field: translate("tasks.settingValueToValue", { field: t.vars.field_name || translate("tasks.customField"), value }),
    }[t.vars.field]
    if (t.vars.automation_name) return translate("tasks.valueProposesValue", { "automation_name": t.vars.automation_name, "action": action })
    return {
      jd_category: translate("tasks.fileUnderQuestion", { value }),
      correspondent: translate("tasks.setCorrespondentQuestion", { value }),
      tag: translate("tasks.addTagQuestion", { value }),
      title: translate("tasks.changeTitleQuestion", { value }),
      language: translate("tasks.setLanguageQuestion", { value }),
      custom_field: translate("tasks.setCustomFieldQuestion", { field: t.vars.field_name || translate("tasks.customField"), value }),
    }[t.vars.field]
  }

  function filingLabel(t) {
    if (!t.doc_jd_category_name) return ''
    return t.doc_jd_category_code
      ? translate("tasks.valueValue", { "doc_jd_category_code": t.doc_jd_category_code, "doc_jd_category_name": t.doc_jd_category_name })
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
    return translate("tasks.filedUnderValueReviewTheRemainingMetadataSuggestions", { "filed": filed })
  }

  function intelligenceGroups() {
    const groups = new Map()
    for (const candidate of intelligence) {
      if (!groups.has(candidate.document_id)) {
        groups.set(candidate.document_id, {
          documentID: candidate.document_id,
          title: candidate.document_title,
          thumbnail: candidate.document_has_thumbnail,
          sensitivity: candidate.document_sensitivity,
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
          ? plural("tasks.addedDateCountToCalendar", resolved.size)
          : plural("tasks.rejectedDateCount", resolved.size))
        : translate("tasks.noDatesWereChanged")
      if (resolved.size < ids.length) reviewNotice += translate("tasks.someDecisionsCouldNotBeCompletedCheckThe")
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
    return count === 0 ? translate("tasks.notRetried") : plural("tasks.retryCount", count)
  }

  async function actOnDeadJob(job, action) {
    busyJobs = new Set([...busyJobs, job.id])
    try {
      if (action === 'retry') await retryDeadJob(job.id)
      else await dismissDeadJob(job.id)
      notify?.(action === 'retry' ? translate("tasks.processingStartedAgain") : translate("tasks.processingIssueDismissed"))
      await load()
    } catch (ex) {
      notify?.(ex.message || translate("tasks.couldNotValueThisProcessingItem", { "action": action }))
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
<div class="toolbar review-toolbar" role="group" aria-label={translate("tasks.approvalOptions")}>
  <button class="btn sm"
          disabled={loading || intelligenceBusy || busyTasks.size > 0} onclick={load}>
    <Icon name="refresh" size={13} /> {translate("tasks.refresh")}
  </button>
  {#if !loading}<span class="pill">{translate("tasks.valuePending", { "length": tasks.length + jobs.length + intelligence.length })}</span>{/if}
  <p role="status">{reviewNotice}</p>
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
          <span class="eyebrow">{translate("tasks.datesNeedingReview")}</span>
          <h2 id="intelligence-review-title">{translate("tasks.checkDatesBeforeTheyReachCalendar")}</h2>
          <p>{translate("tasks.dateReviewSummary", { dates: plural("tasks.dateCount", intelligence.length), documents: plural("tasks.documentCount", reviewGroups.length) })}</p>
        </div>
        <label class="select-all">
          <input type="checkbox" disabled={intelligenceBusy || !allIntelligence.available} checked={allIntelligence.all} use:indeterminate={allIntelligence.some}
                 onchange={(event) => toggleAllIntelligence(event.currentTarget.checked)} />
          {translate("tasks.selectAllDates")}
        </label>
      </header>

      <div class="intelligence-actions" role="group" aria-label={translate("tasks.reviewSelectedDates")}>
        <span><b>{formatNumber(intelligenceSelection.size)}</b> {translate("tasks.ofDateCountSelected", { dates: plural("tasks.dateCount", intelligence.length) })}</span>
        <button class="btn sm" disabled={!intelligenceSelection.size || intelligenceBusy}
                onclick={() => resolveSelectedIntelligence('rejected')}>{translate("tasks.rejectValue", { "size": intelligenceSelection.size })}</button>
        <button class="btn primary sm" disabled={!intelligenceSelection.size || intelligenceBusy}
                onclick={() => resolveSelectedIntelligence('accepted')}>
          <Icon name="check" size={13} /> {intelligenceBusy ? translate("common.saving") : translate("tasks.addValueToCalendar", { "size": intelligenceSelection.size })}
        </button>
      </div>

      <div class="approval-grid" class:approval-grid-many={reviewGroups.length > 2} data-approval-kind="date">
        {#each reviewGroups as group (group.documentID)}
          {@const groupSelection = selectionState(group.candidates)}
          <article class="approval-card intelligence-card">
            <header class="approval-card-header intelligence-document">
              <input type="checkbox" aria-label={translate("tasks.selectEverySuggestedDateFromValue", { "documentID": group.title || `document ${group.documentID}` })}
                     disabled={intelligenceBusy || !groupSelection.available} checked={groupSelection.all} use:indeterminate={groupSelection.some}
                     onchange={(event) => setCandidateSelection(group.candidates.map(candidate => candidate.id), event.currentTarget.checked)} />
              <a class="task-thumb intelligence-thumb" class:placeholder={!group.thumbnail} class:blurred={isHighSensitivity(group.sensitivity)}
                 href={filingHref(`#/doc/${group.documentID}`)} aria-label={translate("tasks.openValue", { "documentID": group.title || `document ${group.documentID}` })}>
                {#if group.thumbnail}
                  <img src={thumbPath(group.documentID, isHighSensitivity(group.sensitivity))} alt="" loading="lazy" />
                {:else}
                  <Icon name="docs" size={20} />
                {/if}
              </a>
              <div>
                <a href={filingHref(`#/doc/${group.documentID}`)}>{group.title || translate("tasks.documentValue2", { "documentID": group.documentID })}</a>
                <small>{plural("tasks.dateCountToCheck", group.candidates.length)}</small>
              </div>
            </header>
            <div class="approval-card-body intelligence-candidates">
              {#each group.candidates as candidate (candidate.id)}
                <div class="intelligence-candidate">
                  <input type="checkbox" aria-label={translate("tasks.selectSuggestedValue", { "candidate": formatIntelligenceValue(candidate) })}
                         checked={intelligenceSelection.has(candidate.id)}
                         disabled={intelligenceBusy || !canReviewDate(candidate) || !!dateErrors[candidate.id]}
                         onchange={(event) => setCandidateSelection([candidate.id], event.currentTarget.checked)} />
                  <div class="intelligence-copy">
                    <div class="intelligence-value">
                      <strong>{translate("tasks.suggestedValue", { "candidate": formatIntelligenceValue(candidate) })}</strong>
                      <span class="pill">{candidate.type === 'date' ? translate("tasks.date") : translate("tasks.readOnly")}</span>
                    </div>
                    {#if candidate.source_current === false}<span class="pill warn">{translate("tasks.documentChanged")}</span>{/if}
                    {#if candidate.evidence_text}
                      <blockquote class="intelligence-evidence">“{candidate.evidence_text}”</blockquote>
                      <small>{candidate.source_current === true ? translate("tasks.exactTextFromTheLinkedDocument") : translate("tasks.thisSavedQuoteNoLongerMatchesTheDocument")}</small>
                    {:else}
                      <small>{translate("tasks.theMatchingTextIsNotShownHere")}</small>
                    {/if}
                    {#if typeof candidate.confidence === 'number'}
                      <details class="task-details">
                        <summary>{translate("tasks.howThisWasSuggested")}</summary>
                        <p>{translate("tasks.modelConfidenceValue", { "confidence": Math.round(candidate.confidence * 100) })}</p>
                        {#if candidate.extractor}<p>{translate("tasks.suggestedByValue", { "extractor": producerLabel(candidate.extractor) })}</p>{/if}
                      </details>
                    {/if}
                    {#if dateErrors[candidate.id]}<p class="review-error" role="alert">{dateErrors[candidate.id]}</p>{/if}
                    {#if candidate.type === 'date' && DATE_ROLES.includes(candidate.role) && candidate.status === 'pending' && candidate.source_current === false}
                      <button class="btn sm" disabled={intelligenceBusy}
                              onclick={() => resolveSelectedIntelligence('rejected', [candidate.id])}>{translate("tasks.rejectThisSuggestion")}</button>
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
    <div class="empty"><Icon name="tasks" size={56} /><b>{translate("tasks.nothingNeedsYou")}</b><span>{translate("tasks.theArchiveIsRunningItself")}</span></div>
  {:else if tasks.length > 0}
    {@const workflowGroups = approvalGroups(tasks)}
    <div class="approval-grid workflow-approval-grid" class:approval-grid-many={workflowGroups.length > 2} data-approval-kind="workflow">
      {#each workflowGroups as group (group.key)}
        <article class="approval-card workflow-approval-card">
          {#if group.document}
            <header class="approval-card-header document-header">
              <a class="task-thumb" class:placeholder={!group.document.doc_has_thumbnail} class:blurred={isHighSensitivity(group.document.doc_sensitivity)}
                 href={filingHref(`#/doc/${group.document.doc_id}`)}
                 aria-label={translate("tasks.openValue2", { "doc_id": group.document.doc_title || `document ${group.document.doc_id}` })}>
                {#if group.document.doc_has_thumbnail}
                  <img src={thumbPath(group.document.doc_id, isHighSensitivity(group.document.doc_sensitivity))} alt="" loading="lazy" />
                {:else}
                  <Icon name="docs" size={22} />
                {/if}
              </a>
              <div class="document-identity">
                <a href={filingHref(`#/doc/${group.document.doc_id}`)}>
                  {group.document.doc_title || translate("tasks.documentValue", { "doc_id": group.document.doc_id })}
                </a>
                <div class="document-meta">
                  <span>#{group.document.doc_id}</span>
                  {#if filingLabel(group.document)}<span class="pill">{filingLabel(group.document)}</span>{/if}
                  {#if group.tasks.length > 1}<span>{plural("tasks.suggestionCount", group.tasks.length)}</span>{/if}
                </div>
              </div>
            </header>
            {#if groupContext(group)}<div class="group-context">{groupContext(group)}</div>{/if}
          {:else}
            <header class="approval-card-header operation-header">
              <span class="operation-mark"><Icon name="refresh" size={17} /></span>
              <span><small>{translate("tasks.archiveOperation")}</small><b>{group.tasks[0]?.approval_name === 'rescan-proposal' ? translate("tasks.processingUpdate") : translate("tasks.approvalRequest")}</b></span>
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
                  <b>{pipelineLabel(t.vars.kind)}</b> {plural("tasks.improvedDocumentCount", t.vars.stale_count)}
                </div>
              {/if}
              {#if t.approval_name === 'document-change' && t.vars}
                <div class="metadata-transition"
                     aria-label={`Current ${typeof t.vars.current_value === 'string' ? (t.vars.current_value || translate("tasks.notSet")) : translate("tasks.unavailable")}; suggested ${suggestionValue(t.vars)}`}>
                  <span>{typeof t.vars.current_value === 'string' ? (t.vars.current_value || translate("tasks.notSet")) : translate("tasks.unavailable")}</span>
                  <span class="transition-arrow" aria-hidden="true">→</span>
                  <strong>{suggestionValue(t.vars)}</strong>
                </div>
                {#if t.vars.source}
                  <div class="suggestion-source">
                    <span>{translate("tasks.whyThisWasSuggested")}</span>
                    <strong>{t.vars.automation_name ? translate("tasks.automationValue", { "automation_name": t.vars.automation_name }) : sourceLabel(t.vars.source)}</strong>
                  </div>
                {/if}
                {#if t.vars.evidence_text || t.vars.sources?.length}
                  <details class="review-rationale">
                    <summary>{supportingContextLabel(t.vars)}</summary>
                    {#if t.vars.evidence_text}<blockquote class="intelligence-evidence">“{t.vars.evidence_text}”</blockquote>{/if}
                    {#if t.vars.sources?.length}
                      <ul class="review-sources">
                        {#each t.vars.sources as source}
                          <li><a href={filingHref(`#/doc/${source.document_id}`)}>{source.title || translate("tasks.openDocument")}</a></li>
                        {/each}
                      </ul>
                    {/if}
                  </details>
                {/if}
              {/if}
              {#if taskErrors[t.id]}<p class="review-error" role="alert">{taskErrors[t.id]}</p>{/if}
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
                  <summary>{translate("tasks.affectedDocuments")}</summary>
                  <div class="rescan-targets">
                    <ul>
                      {#each targets as target (target.id)}
                        <li><a href={filingHref(`#/doc/${target.id}`)}>{target.title || translate("tasks.documentValue3", { "id": target.id })}</a></li>
                      {/each}
                    </ul>
                    {#if Number(t.vars?.stale_count || 0) > targets.length}
                      <span>{translate("tasks.andValueMore", { "length": Number(t.vars.stale_count) - targets.length })}</span>
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

  {#if canManageJobs && jobs.length}
    <h3 style="font-size:.9rem;color:var(--muted);margin:0 0 8px">{translate("tasks.processingIssues")}</h3>
    <div class="index">
      {#each jobs as j (j.id)}
        <div class="irow processing-issue">
          <span class="dot danger"></span>
          <div class="grow processing-issue-copy">
            <span class="title" style="display:block;font-size:.8rem">{jobKindLabel(j.kind)}</span>
            <span class="sub">{translate("tasks.processingStoppedBeforeThisStepCompleted")}</span>
            <details class="processing-details">
              <summary>{translate("tasks.technicalDetails")}</summary>
              <code>{j.kind}</code>
              {#if j.last_error}<pre>{j.last_error}</pre>{/if}
            </details>
          </div>
          <span class="sub">{retryLabel(j)}</span>
          {#if j.doc_id}<a class="btn sm" href={filingHref(`#/doc/${j.doc_id}`)}>{translate("tasks.documentValue", { "doc_id": j.doc_id })}</a>{/if}
          <button class="btn sm" disabled={busyJobs.has(j.id)} onclick={() => actOnDeadJob(j, 'retry')} title={translate("tasks.tryThisProcessingStepAgain")}>
            <Icon name="refresh" size={13} /> {translate("common.retry")}
          </button>
          <button class="btn sm" disabled={busyJobs.has(j.id)} onclick={() => actOnDeadJob(j, 'dismiss')} title={translate("tasks.removeThisIssueWithoutRetrying")}>
            <Icon name="x" size={13} /> {translate("tasks.dismiss")}
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
  .task-thumb.blurred img { filter:blur(6px) }
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
  .rescan-context { color:var(--muted);font-size:.84rem;line-height:1.45;margin-top:10px }
  .choices { display:flex;flex-wrap:wrap;gap:8px;margin-top:14px }
  .task-details { margin-top:10px;color:var(--muted);font-size:.75rem }
  .task-details summary { cursor:pointer;width:max-content }
  .rescan-targets { margin-top:9px;max-width:720px }
  .rescan-targets ul { margin:5px 0 3px;padding-left:18px }
  .rescan-targets li { margin:3px 0;overflow-wrap:anywhere }
  .rescan-targets a { color:var(--accent) }
  .review-toolbar p { flex:1;min-width:0;margin:0;text-align:right;font-size:.8rem;color:var(--muted) }
  .review-toolbar button { flex:none }
  .metadata-transition { display:flex;align-items:baseline;gap:10px;margin:12px 0;font-size:.84rem;overflow-wrap:anywhere }
  .metadata-transition > span:first-child { color:var(--muted);min-width:0 }
  .metadata-transition strong { min-width:0 }
  .transition-arrow { color:var(--faint);flex:none }
  .suggestion-source { display:flex;align-items:baseline;gap:10px;margin:8px 0;font-size:.78rem }
  .suggestion-source span { color:var(--muted) }
  .review-rationale { margin:8px 0;color:var(--muted);font-size:.78rem }
  .review-rationale summary { cursor:pointer;width:max-content;max-width:100% }
  .intelligence-evidence { margin:8px 0 6px;white-space:pre-wrap;overflow-wrap:anywhere }
  .review-sources { margin:8px 0;padding-left:18px;font-size:.78rem;overflow-wrap:anywhere }
  .review-error { color:var(--danger);font-size:.8rem;line-height:1.45;overflow-wrap:anywhere }
  .processing-issue { align-items:flex-start;flex-wrap:wrap;cursor:default }
  .processing-issue-copy { display:flex;flex:1 1 260px;min-width:0;flex-direction:column;gap:3px }
  .processing-details { color:var(--muted);font-size:.7rem }
  .processing-details summary { width:max-content;cursor:pointer }
  .processing-details code { display:block;margin-top:6px;color:var(--faint) }
  .processing-details pre { max-width:720px;margin:6px 0 0;overflow:auto;white-space:pre-wrap;overflow-wrap:anywhere;color:var(--muted);font:inherit }
  @media (max-width: 1050px) {
    .approval-grid.approval-grid-many { grid-template-columns: 1fr; }
  }
  @media (max-width: 560px) {
    .task-thumb { flex-basis:42px;width:42px }
    .intelligence-head { align-items: flex-start; flex-direction: column; }
    .intelligence-actions { flex-wrap: wrap; }
    .intelligence-actions > span { width: 100%; }
    .intelligence-actions .btn { flex: 1; justify-content: center; }
  }
</style>
