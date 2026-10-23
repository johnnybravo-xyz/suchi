<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { tick } from 'svelte'
  import { t as translate } from "../lib/i18n.svelte.js";
  import { scopedHash as filingHref } from '../lib/systems.svelte.js'
  import { route, go } from '../lib/router.svelte.js'
  import { SUGGESTION_FIELDS, canApplySuggestion } from '../lib/suggestions.js'
  import { listTasks, resolveApprovalTask, retryDeadJob, dismissDeadJob, thumbPath,
           listIntelligence, resolveIntelligence } from '../lib/api.js'
  import { fmtDate, formatNumber, isHighSensitivity, plural } from '../lib/format.js'
  import { DATE_ROLES, formatIntelligenceValue, canReviewDate, reviewFailure } from '../lib/intelligence.js'
  import Icon from '../lib/Icon.svelte'
  import RuleWorkspace from '../lib/RuleWorkspace.svelte'

  const REVIEW_ENTRY_LIMIT = 100
  let { notify, onCount, canReviewIntelligence = false, canManageJobs = false } = $props()
  const SUGGESTION_TABS = ['all', 'tags', 'filing', 'fields']
  let tasks = $state([])
  let jobs = $state([])
  let loading = $state(true)
  let err = $state('')
  let intelligence = $state([])
  let intelligenceBusy = $state(false)
  const addableDates = $derived(intelligence.filter(candidate => canReviewDate(candidate) && !dateErrors[candidate.id]))
  let busyJobs = $state(new Set())
  let busyTasks = $state(new Set())
  let taskErrors = $state({})
  let dateErrors = $state({})
  // The open tab lives in the URL (?tab=issues) so links can open it directly.
  const tabFromURL = route.query.get('tab') || ''
  let approvalFilter = $state(tabFromURL || 'all')
  let expandedSuggestionGroups = $state(new Set())
  let expandedReviewContext = $state(new Set())
  let suggestionGroupsOpen = $state(false)
  // The hovered or focused side of the synchronized queues and its task IDs;
  // the opposite side highlights the same decisions.
  let linkedHover = $state(null)
  let linkHoverVersion = 0
  let revealTimer
  let documentReviewScroll = $state()
  let suggestionReviewScroll = $state()
  let suggestionGroupsToggle = $state()
  const metadataTasks = $derived(tasks.filter(isMetadataTask))
  const visibleMetadataTasks = $derived(metadataTasks.filter(task => matchesApprovalFilter(task, activeTab)))
  const documentGroups = $derived(approvalGroups(visibleMetadataTasks))
  const displayedDocumentGroups = $derived(documentGroups.slice(0, REVIEW_ENTRY_LIMIT))
  const hiddenDocumentGroupCount = $derived(Math.max(0, documentGroups.length - displayedDocumentGroups.length))
  // Built in document order, so groups appear as their first document does and
  // both panes read top to bottom together.
  const suggestionGroups = $derived(suggestionApprovalGroups(documentGroups.flatMap(group => group.tasks)))
  const filterCounts = $derived(countApprovalFilters(metadataTasks))
  const displayedSuggestionGroups = $derived(suggestionGroups.slice(0, REVIEW_ENTRY_LIMIT))
  const hiddenSuggestionGroupCount = $derived(Math.max(0, suggestionGroups.length - displayedSuggestionGroups.length))
  const otherTasks = $derived(tasks.filter(task => !isMetadataTask(task)))
  const otherTaskGroups = $derived(approvalGroups(otherTasks))
  // One view per tab. The metadata tabs that All counts come first; Dates,
  // counted separately, follows them. Decisions and processing issues appear
  // only when present; processing issues sit apart at the end as an attention
  // notice.
  const approvalTabs = $derived([
    { id: 'all', label: translate("tasks.allSuggestionFilter"), count: filterCounts.all },
    { id: 'tags', label: translate("tasks.tagSuggestionFilter"), count: filterCounts.tags },
    { id: 'filing', label: translate("tasks.filingSuggestionFilter"), count: filterCounts.filing },
    { id: 'fields', label: translate("tasks.fieldSuggestionFilter"), count: filterCounts.fields },
    ...(canReviewIntelligence ? [{ id: 'dates', label: translate("tasks.dateSuggestionFilter"), count: intelligence.length }] : []),
    ...(otherTasks.length ? [{ id: 'decisions', label: translate("tasks.decisions"), count: otherTasks.length }] : []),
    ...(canManageJobs && jobs.length ? [{ id: 'issues', label: translate("tasks.processingIssues"), count: jobs.length, attention: true }] : []),
  ])
  const activeTab = $derived(approvalTabs.some(tab => tab.id === approvalFilter) ? approvalFilter : 'all')
  const suggestionTab = $derived(SUGGESTION_TABS.includes(activeTab))
  const hasApprovals = $derived(tasks.length > 0 || intelligence.length > 0 || (canManageJobs && jobs.length > 0))

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
      taskErrors = {}
      dateErrors = {}
      expandedSuggestionGroups = new Set()
      expandedReviewContext = new Set()
      onCount?.(tasks.length + jobs.length + intelligence.length)
      // Land on the first tab with work when there are no metadata suggestions.
      // A tab named in the URL wins only while it exists for this person.
      const namedTab = tabFromURL && approvalTabs.some(tab => tab.id === tabFromURL)
      if (!namedTab && !metadataTasks.length && SUGGESTION_TABS.includes(activeTab)) {
        approvalFilter = intelligence.length ? 'dates' : otherTasks.length ? 'decisions' : jobs.length ? 'issues' : approvalFilter
      }
    } catch (ex) {
      tasks = []
      jobs = []
      intelligence = []
      err = reviewFailure(ex)
    }
    finally { loading = false }
  }

  // Decided items fade out of both queues together so their shared state is
  // visible. The Web Animations API keeps Svelte's transition runtime out of
  // the initial bundle.
  async function fadeOutDecided(ids) {
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return
    const leaving = [...document.querySelectorAll('.approvals-workspace [data-task-id]')]
      .filter(node => ids.has(Number(node.dataset.taskId)))
    for (const card of document.querySelectorAll('.document-review-card, .suggestion-card')) {
      const rows = [...card.querySelectorAll('[data-task-id]')]
      if (rows.length && rows.every(row => ids.has(Number(row.dataset.taskId)))) leaving.push(card)
    }
    await Promise.all(leaving.map(node =>
      node.animate([{ opacity: 1 }, { opacity: 0 }], { duration: 150, fill: 'forwards' }).finished))
  }

  async function resolve(t, choice) {
    await resolveMany([t], choice)
  }

  async function resolveMany(items, choice) {
    const eligible = items.filter(task =>
      !busyTasks.has(task.id) && !taskErrors[task.id] && taskChoices(task).includes(choice))
    if (!eligible.length) return
    const ids = new Set(eligible.map(task => task.id))
    busyTasks = new Set([...busyTasks, ...ids])
    const resolved = new Set()
    const failures = { ...taskErrors }
    try {
      for (const task of eligible) {
        try {
          await resolveApprovalTask(task.id, { choice })
          resolved.add(task.id)
          delete failures[task.id]
        } catch (ex) {
          failures[task.id] = reviewFailure(ex)
        }
      }
      if (resolved.size) {
        await fadeOutDecided(resolved)
        tasks = tasks.filter(task => !resolved.has(task.id))
      }
      taskErrors = failures
      onCount?.(tasks.length + jobs.length + intelligence.length)
      notify?.(resolutionNotice(choice, resolved.size, eligible.length))
    } finally {
      const next = new Set(busyTasks)
      for (const id of ids) next.delete(id)
      busyTasks = next
    }
  }

  function resolutionNotice(choice, resolved, attempted) {
    if (attempted === 1 && resolved === 1) {
      return choice === 'apply'
        ? translate("tasks.decisionRecordedTheDocumentAndCurrentValueWill")
        : choice === 'reject' || choice === 'dismiss'
          ? translate("tasks.suggestionDismissed")
          : translate("tasks.decisionRecordedAnyRequestedProcessingWillContinueShortly")
    }
    if (resolved === attempted) {
      return choice === 'apply'
        ? plural("tasks.approvedSuggestionCount", resolved)
        : plural("tasks.dismissedSuggestionCount", resolved)
    }
    return translate("tasks.recordedSuggestionDecisionCount", { resolved, attempted })
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
      if (!SUGGESTION_FIELDS.includes(t.vars?.field)) return []
      return choices.filter(choice => choice === 'reject' || (choice === 'apply' && canApplySuggestion(t)))
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
    if (!SUGGESTION_FIELDS.includes(t.vars.field)) return translate("tasks.unsupportedActionReadOnly")
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

  function isMetadataTask(task) {
    return task.approval_name === 'document-change' && !!task.doc_id
  }

  function taskFilterKind(task) {
    if (task.vars?.field === 'tag') return 'tags'
    if (task.vars?.field === 'jd_category') return 'filing'
    return 'fields'
  }

  function matchesApprovalFilter(task, filter) {
    return filter === 'all' || taskFilterKind(task) === filter
  }

  function countApprovalFilters(items) {
    const counts = { all: items.length, tags: 0, filing: 0, fields: 0 }
    for (const task of items) counts[taskFilterKind(task)]++
    return counts
  }

  function addTaskToGroup(group, task) {
    group.tasks.push(task)
    const choices = taskChoices(task)
    if (choices.includes('apply')) group.applyTasks.push(task)
    if (choices.includes('reject')) group.rejectTasks.push(task)
  }

  function approvalGroups(items) {
    const groups = []
    const documents = new Map()
    for (const task of items) {
      if (isMetadataTask(task)) {
        let group = documents.get(task.doc_id)
        if (!group) {
          group = {
            key: `document-${task.doc_id}`,
            document: task,
            tasks: [],
            applyTasks: [],
            rejectTasks: [],
          }
          documents.set(task.doc_id, group)
          groups.push(group)
        }
        addTaskToGroup(group, task)
      } else {
        const group = {
          key: `task-${task.id}`,
          document: task.doc_id ? task : null,
          tasks: [],
          applyTasks: [],
          rejectTasks: [],
        }
        addTaskToGroup(group, task)
        groups.push(group)
      }
    }
    return groups
  }

  function suggestionApprovalGroups(items) {
    const groups = []
    const suggestions = new Map()
    for (const task of items) {
      const key = JSON.stringify([
        task.vars?.field || '',
        task.vars?.field_name || '',
        suggestionValue(task.vars),
        task.vars?.source || '',
        task.vars?.automation_id || '',
        task.vars?.automation_name || '',
      ])
      let group = suggestions.get(key)
      if (!group) {
        group = {
          key,
          representative: task,
          tasks: [],
          applyTasks: [],
          rejectTasks: [],
        }
        suggestions.set(key, group)
        groups.push(group)
      }
      addTaskToGroup(group, task)
    }
    return groups
  }

  function suggestionActionLabel(task) {
    return {
      jd_category: translate("tasks.fileDocument"),
      correspondent: translate("tasks.setCorrespondent"),
      tag: translate("tasks.addTag"),
      title: translate("tasks.changeTitle"),
      language: translate("tasks.setLanguage"),
      custom_field: task.vars?.field_name
        ? translate("tasks.setNamedField", { field: task.vars.field_name })
        : translate("tasks.setCustomField"),
    }[task.vars?.field] || translate("tasks.unsupportedActionReadOnly")
  }

  // A pointer crossing one pane on its way elsewhere would otherwise scroll
  // the other pane at every entry it passes. The tint follows at once; the
  // other pane scrolls only once the pointer rests. Focus is deliberate, so it
  // reveals at once.
  const REVEAL_DELAY_MS = 300

  function linkHover(side, groupTasks, source, immediate = false) {
    const version = ++linkHoverVersion
    linkedHover = { side, taskIDs: new Set(groupTasks.map(task => task.id)) }
    clearTimeout(revealTimer)
    const reveal = async () => {
      await tick()
      if (version !== linkHoverVersion) return
      if (side === 'document') revealLinked(suggestionReviewScroll, '.suggestion-card.linked', source)
      else revealLinked(documentReviewScroll, '.document-review-card.linked', source)
    }
    if (immediate) reveal()
    else revealTimer = setTimeout(reveal, REVEAL_DELAY_MS)
  }

  function clearLinkHover() {
    linkHoverVersion++
    clearTimeout(revealTimer)
    linkedHover = null
  }

  // Side by side, each pane scrolls on its own, so a linked entry can sit
  // outside the other pane's view. Stacked panes share the page scroll and are
  // skipped: scrolling there would move the hovered entry away.
  function revealLinked(scroller, selector, source) {
    if (!scroller || !source || getComputedStyle(scroller).overflowY !== 'auto') return
    const linked = [...scroller.querySelectorAll(selector)]
    if (!linked.length) return
    const view = scroller.getBoundingClientRect()
    if (linked.some(entry => {
      const box = entry.getBoundingClientRect()
      return box.top >= view.top && box.bottom <= view.bottom
    })) return
    // Level the entry with the hovered one, so moving straight across lands on
    // it, while keeping it inside the pane; a tall entry keeps its top in view.
    const box = linked[0].getBoundingClientRect()
    const from = source.getBoundingClientRect()
    const margin = 8
    const levelTop = from.top + from.height / 2 - box.height / 2
    const top = Math.max(view.top + margin, Math.min(levelTop, view.bottom - margin - box.height))
    const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches
    scroller.scrollBy({ top: box.top - top, behavior: reduceMotion ? 'auto' : 'smooth' })
  }

  function toggleSuggestionGroup(key) {
    const next = new Set(expandedSuggestionGroups)
    if (next.has(key)) next.delete(key)
    else next.add(key)
    expandedSuggestionGroups = next
  }

  function toggleReviewContext(taskID) {
    const next = new Set(expandedReviewContext)
    if (next.has(taskID)) next.delete(taskID)
    else next.add(taskID)
    expandedReviewContext = next
  }

  async function setSuggestionGroupsOpen(open) {
    suggestionGroupsOpen = open
    await tick()
    suggestionGroupsToggle?.focus()
  }

  function tasksAreBusy(items) {
    return items.some(task => busyTasks.has(task.id))
  }

  function tasksHaveErrors(items) {
    return items.some(task => !!taskErrors[task.id])
  }

  function firstTaskError(items) {
    for (const task of items) if (taskErrors[task.id]) return taskErrors[task.id]
    return ''
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

  // Stale dates can still be rejected; only current, reviewable dates can be added.
  function canRejectDate(candidate) {
    return candidate?.type === 'date' && DATE_ROLES.includes(candidate.role) && candidate.status === 'pending'
  }

  async function resolveDates(decision, ids) {
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
      onCount?.(tasks.length + jobs.length + intelligence.length)
      let reviewNotice = resolved.size
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
    } finally {
      intelligenceBusy = false
    }
  }

  function retryLabel(job) {
    const count = Number(job.attempts || 0)
    return count === 0 ? translate("tasks.notRetried") : plural("tasks.retryCount", count)
  }

  function selectTab(id) {
    approvalFilter = id
    go(filingHref(id === 'all' ? '#/tasks' : `#/tasks?tab=${id}`), { replace: true })
  }

  async function retryAllJobs() {
    const ids = jobs.map(job => job.id)
    busyJobs = new Set(ids)
    let failed = 0
    for (const id of ids) {
      try { await retryDeadJob(id) } catch { failed++ }
    }
    notify?.(failed
      ? translate("tasks.someStepsCouldNotRestart", { started: ids.length - failed, failed })
      : plural("tasks.stepsStartedAgain", ids.length))
    busyJobs = new Set()
    await load()
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

  // Cmd/Ctrl+Enter resolves the first visible task with its primary choice.
  function onKey(e) {
    const task = visibleMetadataTasks[0] || otherTasks[0]
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter' && task) {
      e.preventDefault()
      const choice = taskChoices(task)[0]
      if (choice) resolve(task, choice)
    }
  }

  load()
</script>

<svelte:window onkeydown={onKey} />

<div class="approvals-page">
  <RuleWorkspace class="approvals-workspace {suggestionTab && visibleMetadataTasks.length ? 'fitted' : ''}"
                 title={translate("app.approvals")}
                 description={translate("tasks.suggestedChangesDescription")}
                 headingId="approvals-workspace-heading">
    {#snippet actions()}
      <div class="review-toolbar" role="group" aria-label={translate("tasks.approvalOptions")}>
        <button class="btn sm"
                disabled={loading || intelligenceBusy || busyTasks.size > 0} onclick={load}>
          <Icon name="refresh" size={13} /> {translate("tasks.refresh")}
        </button>
        {#if metadataTasks.length && suggestionTab}
          <button bind:this={suggestionGroupsToggle} type="button"
                  class="btn sm suggestion-groups-toggle" class:primary={suggestionGroupsOpen}
                  aria-expanded={suggestionGroupsOpen}
                  aria-controls={suggestionGroupsOpen ? 'suggestion-review-pane' : undefined}
                  onclick={() => setSuggestionGroupsOpen(!suggestionGroupsOpen)}>
            {suggestionGroupsOpen
              ? translate("tasks.hideSuggestionGroups")
              : translate("tasks.showSuggestionGroups", { count: suggestionGroups.length })}
          </button>
        {/if}
      </div>
    {/snippet}

    {#if err}<div class="err approvals-error">{err}</div>{/if}

    {#if loading || hasApprovals}
      <div class="approval-filters" role="group" aria-label={translate("tasks.approvalFilters")}>
        {#each approvalTabs as tab (tab.id)}
          <button type="button" class:active={activeTab === tab.id} class:attention={tab.attention}
                  aria-pressed={activeTab === tab.id} onclick={() => selectTab(tab.id)}>
            <span>{tab.label}</span>
            <span class="filter-count">{formatNumber(tab.count)}</span>
          </button>
        {/each}
      </div>
    {/if}

    {#if loading}
      <div class="review-skeleton">
        {#each Array(4) as _}<div class="approval-loading-row"><div class="skel"></div></div>{/each}
      </div>
    {:else if !hasApprovals}
      <div class="empty"><Icon name="tasks" size={56} /><b>{translate("tasks.nothingNeedsYou")}</b><span>{translate("tasks.theArchiveIsRunningItself")}</span></div>
    {:else if activeTab === 'dates'}
      <div class="tab-panel">
        {#if intelligence.length}
          {@const reviewGroups = intelligenceGroups()}
          {#if addableDates.length > 1}
            <div class="tab-actions">
              <button class="btn sm" disabled={intelligenceBusy}
                      onclick={() => resolveDates('accepted', addableDates.map(candidate => candidate.id))}>
                <Icon name="check" size={13} /> {plural("tasks.addAllDatesToCalendar", addableDates.length)}
              </button>
            </div>
          {/if}
          <div class="approval-grid" class:approval-grid-many={reviewGroups.length > 2} data-approval-kind="date">
            {#each reviewGroups as group (group.documentID)}
              <article class="approval-card">
                <header class="approval-card-header document-header">
                  <a class="task-thumb" class:placeholder={!group.thumbnail} class:blurred={isHighSensitivity(group.sensitivity)}
                     href={filingHref(`#/doc/${group.documentID}`)} aria-label={translate("tasks.openValue", { "documentID": group.title || `document ${group.documentID}` })}>
                    {#if group.thumbnail}
                      <img src={thumbPath(group.documentID, isHighSensitivity(group.sensitivity))} alt="" loading="lazy" />
                    {:else}
                      <Icon name="docs" size={22} />
                    {/if}
                  </a>
                  <div class="document-identity">
                    <a href={filingHref(`#/doc/${group.documentID}`)}>{group.title || translate("tasks.documentValue2", { "documentID": group.documentID })}</a>
                    <div class="document-meta">
                      <span>#{group.documentID}</span>
                      {#if group.candidates.length > 1}<span>{plural("tasks.dateCount", group.candidates.length)}</span>{/if}
                    </div>
                  </div>
                </header>
                <div class="approval-card-body decision-list">
                  {#each group.candidates as candidate (candidate.id)}
                    <section class="decision-row intelligence-candidate">
                      <div class="decision date-value">
                        {formatIntelligenceValue(candidate)}
                        {#if candidate.type !== 'date'}<span class="pill">{translate("tasks.readOnly")}</span>{/if}
                        {#if candidate.source_current === false}<span class="pill warn">{translate("tasks.documentChanged")}</span>{/if}
                      </div>
                      {#if candidate.evidence_text}
                        <blockquote class="intelligence-evidence">“{candidate.evidence_text}”</blockquote>
                        {#if candidate.source_current !== true}<p class="evidence-note">{translate("tasks.thisSavedQuoteNoLongerMatchesTheDocument")}</p>{/if}
                      {:else}
                        <p class="evidence-note">{translate("tasks.theMatchingTextIsNotShownHere")}</p>
                      {/if}
                      {#if dateErrors[candidate.id]}<p class="review-error" role="alert">{dateErrors[candidate.id]}</p>{/if}
                      {#if canRejectDate(candidate)}
                        <div class="choices">
                          {#if canReviewDate(candidate)}
                            <button class="btn sm primary" disabled={intelligenceBusy || !!dateErrors[candidate.id]}
                                    onclick={() => resolveDates('accepted', [candidate.id])}>
                              <Icon name="check" size={13} />{translate("tasks.addToCalendar")}
                            </button>
                          {/if}
                          <button class="btn sm danger" disabled={intelligenceBusy || !!dateErrors[candidate.id]}
                                  onclick={() => resolveDates('rejected', [candidate.id])}>{translate("tasks.reject")}</button>
                        </div>
                      {/if}
                      {#if typeof candidate.confidence === 'number'}
                        <details class="task-details">
                          <summary>{translate("tasks.howThisWasSuggested")}</summary>
                          <p>{translate("tasks.modelConfidenceValue", { "confidence": Math.round(candidate.confidence * 100) })}</p>
                          {#if candidate.extractor}<p>{translate("tasks.suggestedByValue", { "extractor": producerLabel(candidate.extractor) })}</p>{/if}
                        </details>
                      {/if}
                    </section>
                  {/each}
                </div>
              </article>
            {/each}
          </div>
        {:else}
          <p class="filtered-empty">{translate("tasks.noSuggestionsInThisFilter")}</p>
        {/if}
      </div>
    {:else if activeTab === 'decisions'}
      <div class="tab-panel">
        <div class="approval-grid" class:approval-grid-many={otherTaskGroups.length > 2} data-approval-kind="workflow">
          {#each otherTaskGroups as group (group.key)}
            <article class="approval-card workflow-approval-card">
              <header class="approval-card-header operation-header">
                <span class="operation-mark"><Icon name="refresh" size={17} /></span>
                <span><small>{translate("tasks.archiveOperation")}</small><b>{group.tasks[0]?.approval_name === 'rescan-proposal' ? translate("tasks.processingUpdate") : translate("tasks.approvalRequest")}</b></span>
              </header>
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
      </div>
    {:else if activeTab === 'issues'}
      <div class="tab-panel">
        {#if jobs.length > 1}
          <div class="issues-actions">
            <button class="btn sm" disabled={busyJobs.size > 0} onclick={retryAllJobs}>
              <Icon name="refresh" size={13} /> {translate("tasks.retryAll")}
            </button>
          </div>
        {/if}
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
              {#if j.doc_id}<a class="btn sm issue-document" href={filingHref(`#/doc/${j.doc_id}`)}>{j.doc_title || translate("tasks.documentValue", { "doc_id": j.doc_id })}</a>{/if}
              <button class="btn sm" disabled={busyJobs.has(j.id)} onclick={() => actOnDeadJob(j, 'retry')} title={translate("tasks.tryThisProcessingStepAgain")}>
                <Icon name="refresh" size={13} /> {translate("common.retry")}
              </button>
              <button class="btn sm" disabled={busyJobs.has(j.id)} onclick={() => actOnDeadJob(j, 'dismiss')} title={translate("tasks.removeThisIssueWithoutRetrying")}>
                <Icon name="x" size={13} /> {translate("tasks.dismiss")}
              </button>
            </div>
          {/each}
        </div>
      </div>
    {:else if visibleMetadataTasks.length === 0}
      <p class="filtered-empty">{translate("tasks.noSuggestionsInThisFilter")}</p>
    {:else}
      <div class="review-panes" class:with-suggestion-groups={suggestionGroupsOpen}>
        <section class="document-review-pane" aria-label={translate("tasks.byDocument")}>
          {#if suggestionGroupsOpen}
            <header class="pane-heading">
              <div class="pane-heading-label">
                <h2>{translate("tasks.byDocument")}</h2>
                <span>{translate("tasks.inQueue", { count: documentGroups.length })}</span>
              </div>
            </header>
          {/if}
          <div class="pane-scroll" bind:this={documentReviewScroll}>
          <div class="document-review-list">
            {#each displayedDocumentGroups as group (group.key)}
              <article class="document-review-card"
                       class:linked={linkedHover?.side === 'suggestion' && group.tasks.some(task => linkedHover.taskIDs.has(task.id))}
                       onmouseenter={(event) => linkHover('document', group.tasks, event.currentTarget)} onmouseleave={clearLinkHover}
                       onfocusin={(event) => linkHover('document', group.tasks, event.currentTarget, true)} onfocusout={clearLinkHover}>
                  <a class="task-thumb" class:placeholder={!group.document.doc_has_thumbnail} class:blurred={isHighSensitivity(group.document.doc_sensitivity)}
                     href={filingHref(`#/doc/${group.document.doc_id}`)}
                     aria-label={translate("tasks.openValue2", { "doc_id": group.document.doc_title || `document ${group.document.doc_id}` })}>
                    {#if group.document.doc_has_thumbnail}
                      <img src={thumbPath(group.document.doc_id, isHighSensitivity(group.document.doc_sensitivity))} alt="" loading="lazy" />
                    {:else}
                      <Icon name="docs" size={16} />
                    {/if}
                  </a>
                <div class="document-review-body">
                <header class="document-header">
                  <div class="document-identity">
                    <div class="document-title-line">
                      <a href={filingHref(`#/doc/${group.document.doc_id}`)}>
                        {group.document.doc_title || translate("tasks.documentValue", { "doc_id": group.document.doc_id })}
                      </a>
                      <span>#{group.document.doc_id}</span>
                      {#if filingLabel(group.document)}<span class="filing-pill">{filingLabel(group.document)}</span>{/if}
                    </div>
                  </div>
                  <!-- A single suggestion's row controls already settle the document. -->
                  {#if group.tasks.length > 1 && (group.rejectTasks.length || group.applyTasks.length)}
                    <div class="document-bulk-actions">
                      {#if group.rejectTasks.length}
                        <button class="btn sm" disabled={tasksAreBusy(group.tasks) || tasksHaveErrors(group.tasks)}
                                onclick={() => resolveMany(group.rejectTasks, 'reject')}>
                          {group.rejectTasks.length > 1 ? translate("tasks.dismissAll") : translate("tasks.dismiss")}
                        </button>
                      {/if}
                      {#if group.applyTasks.length}
                        <button class="btn sm primary" disabled={tasksAreBusy(group.tasks) || tasksHaveErrors(group.tasks)}
                                onclick={() => resolveMany(group.applyTasks, 'apply')}>
                          {group.applyTasks.length > 1
                            ? translate("tasks.approveAll", { count: group.applyTasks.length })
                            : translate("tasks.approve")}
                        </button>
                      {/if}
                    </div>
                  {/if}
                </header>
                <div class="document-decisions">
                  {#each group.tasks as t (t.id)}
                    {@const choices = taskChoices(t)}
                    {@const dl = deadline(t)}
                    {@const reviewContextOpen = expandedReviewContext.has(t.id)}
                    <section class="decision-row document-decision" data-task-id={t.id}
                             class:linked={linkedHover?.side === 'suggestion' && linkedHover.taskIDs.has(t.id)}>
                      <div class="decision-kind">{suggestionActionLabel(t)}</div>
                      <div class="metadata-transition">
                        {#if t.vars?.field === 'tag'}
                          <span class="addition-mark" aria-hidden="true">+</span>
                        {:else}
                          <span>{typeof t.vars?.current_value === 'string' ? (t.vars.current_value || translate("tasks.notSet")) : translate("tasks.unavailable")}</span>
                          <span class="transition-arrow" aria-hidden="true">→</span>
                        {/if}
                        <strong>{suggestionValue(t.vars)}</strong>
                      </div>
                      {#if t.vars?.source}
                        <span class="source-pill decision-source">
                          {t.vars.automation_name
                            ? translate("tasks.automationValue", { "automation_name": t.vars.automation_name })
                            : sourceLabel(t.vars.source)}
                        </span>
                      {/if}
                      {#if t.vars?.evidence_text || t.vars?.sources?.length}
                        <button type="button" class="review-rationale-toggle"
                                aria-expanded={reviewContextOpen} aria-controls={`review-context-${t.id}`}
                                onclick={() => toggleReviewContext(t.id)}>
                          {supportingContextLabel(t.vars)}
                        </button>
                      {/if}
                      <div class="decision-controls">
                        {#if choices.includes('reject')}
                          <button class="decision-button dismiss" disabled={busyTasks.has(t.id) || !!taskErrors[t.id]}
                                  aria-label={choiceLabel(t, 'reject')} title={choiceLabel(t, 'reject')}
                                  onclick={() => resolve(t, 'reject')}><Icon name="x" size={16} /></button>
                        {/if}
                        {#if choices.includes('apply')}
                          <button class="decision-button approve" disabled={busyTasks.has(t.id) || !!taskErrors[t.id]}
                                  aria-label={choiceLabel(t, 'apply')} title={choiceLabel(t, 'apply')}
                                  onclick={() => resolve(t, 'apply')}><Icon name="check" size={16} /></button>
                        {/if}
                      </div>
                      {#if reviewContextOpen || taskErrors[t.id] || dl}
                        <div class="decision-followup">
                          {#if reviewContextOpen}
                            <div id={`review-context-${t.id}`} class="review-rationale-content">
                              {#if t.vars.evidence_text}<blockquote class="intelligence-evidence">“{t.vars.evidence_text}”</blockquote>{/if}
                              {#if t.vars.sources?.length}
                                <ul class="review-sources">
                                  {#each t.vars.sources as source}
                                    <li><a href={filingHref(`#/doc/${source.document_id}`)}>{source.title || translate("tasks.openDocument")}</a></li>
                                  {/each}
                                </ul>
                              {/if}
                            </div>
                          {/if}
                          {#if taskErrors[t.id]}<p class="review-error" role="alert">{taskErrors[t.id]}</p>{/if}
                          {#if dl}<div class="meta" class:deadline-soon={dl.soon}>{dl.text}</div>{/if}
                        </div>
                      {/if}
                    </section>
                  {/each}
                </div>
                </div>
              </article>
            {/each}
          </div>
          {#if hiddenDocumentGroupCount}
            <p class="review-overflow">{plural("tasks.moreDocumentsBelow", hiddenDocumentGroupCount)}</p>
          {/if}
          </div>
        </section>

        {#if suggestionGroupsOpen}
          <section id="suggestion-review-pane" class="suggestion-review-pane" aria-label={translate("tasks.bySuggestion")}>
            <header class="pane-heading">
              <div class="pane-heading-label">
                <h2>{translate("tasks.bySuggestion")}</h2>
                <span>{plural("tasks.groupCount", suggestionGroups.length)}</span>
              </div>
              <span class="sync-mark" role="img"
                    aria-label={translate("tasks.suggestionGroupsExplanation", { count: visibleMetadataTasks.length })}
                    title={translate("tasks.suggestionGroupsExplanation", { count: visibleMetadataTasks.length })}>
                <Icon name="sync" size={14} />
              </span>
            </header>
            <div class="pane-scroll" bind:this={suggestionReviewScroll}>
            <div class="suggestion-review-list">
              {#each displayedSuggestionGroups as group (group.key)}
                {@const representative = group.representative}
                {@const expanded = expandedSuggestionGroups.has(group.key)}
                <article class="approval-card suggestion-card"
                         class:linked={linkedHover?.side === 'document' && group.tasks.some(task => linkedHover.taskIDs.has(task.id))}
                         onmouseenter={(event) => linkHover('suggestion', group.tasks, event.currentTarget)} onmouseleave={clearLinkHover}
                         onfocusin={(event) => linkHover('suggestion', group.tasks, event.currentTarget, true)} onfocusout={clearLinkHover}>
                  <header class="approval-card-header suggestion-card-header">
                    <div>
                      <span>{suggestionActionLabel(representative)}</span>
                      <strong>{#if representative.vars?.field === 'tag'}<span class="addition-mark" aria-hidden="true">+</span>{/if}{suggestionValue(representative.vars)}</strong>
                    </div>
                    {#if representative.vars?.source}
                      <span class="source-pill">
                        {representative.vars.automation_name
                          ? translate("tasks.automationValue", { "automation_name": representative.vars.automation_name })
                          : sourceLabel(representative.vars.source)}
                      </span>
                    {/if}
                  </header>
                  <div class="suggestion-documents">
                    {#each group.tasks as task, index (task.id)}
                      {#if expanded || index < 3}
                        {@const name = task.doc_title || translate("tasks.documentValue", { "doc_id": task.doc_id })}
                        <a href={filingHref(`#/doc/${task.doc_id}`)} data-task-id={task.id}>
                          <strong title={name}>{name}</strong>
                          <span>#{task.doc_id}</span>
                        </a>
                      {/if}
                    {/each}
                    {#if group.tasks.length > 3}
                      <button type="button" class="more-documents" onclick={() => toggleSuggestionGroup(group.key)}
                              aria-expanded={expanded}>
                        {expanded
                          ? translate("tasks.showFewerDocuments")
                          : translate("tasks.moreDocuments", { count: group.tasks.length - 3 })}
                      </button>
                    {/if}
                  </div>
                  {#if firstTaskError(group.tasks)}
                    <p class="review-error suggestion-error" role="alert">{firstTaskError(group.tasks)}</p>
                  {/if}
                  {#if group.rejectTasks.length || group.applyTasks.length}
                    <footer class="suggestion-card-actions">
                      {#if group.rejectTasks.length}
                        <button class="btn sm" disabled={tasksAreBusy(group.tasks) || tasksHaveErrors(group.tasks)}
                                onclick={() => resolveMany(group.rejectTasks, 'reject')}>
                          {group.rejectTasks.length > 1
                            ? translate("tasks.dismissAllCount", { count: group.rejectTasks.length })
                            : translate("tasks.dismiss")}
                        </button>
                      {/if}
                      {#if group.applyTasks.length}
                        <button class="btn sm primary" disabled={tasksAreBusy(group.tasks) || tasksHaveErrors(group.tasks)}
                                onclick={() => resolveMany(group.applyTasks, 'apply')}>
                          {group.applyTasks.length > 1
                            ? translate("tasks.applyToAll", { count: group.applyTasks.length })
                            : translate("tasks.approve")}
                        </button>
                      {/if}
                    </footer>
                  {/if}
                </article>
              {/each}
            </div>
              {#if hiddenSuggestionGroupCount}
                <p class="review-overflow">{plural("tasks.moreSuggestionGroupsBelow", hiddenSuggestionGroupCount)}</p>
              {/if}
            </div>
          </section>
        {/if}
      </div>
    {/if}
  </RuleWorkspace>
</div>

<style>
  .issues-actions { display:flex; justify-content:flex-end; margin-bottom:10px }
  .issue-document { max-width:220px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap }
  .approvals-page { width:min(100%,1400px);margin:0 auto }
  .approvals-error { margin:14px 20px 0 }
  .review-toolbar { display:flex;align-items:center;justify-content:flex-end;gap:10px;min-width:0 }
  .review-toolbar button { flex:none }
  .suggestion-groups-toggle { border-radius:999px }
  /* The divider is an inset shadow the active underline can cover without
     overflowing, so the horizontally scrollable bar never shows a vertical
     scrollbar. */
  .approval-filters {
    display:flex;align-items:center;gap:4px;margin:0;padding:0 20px;
    box-shadow:inset 0 -1px var(--line);overflow-x:auto;overflow-y:hidden
  }
  .approval-filters button {
    position:relative;display:flex;align-items:center;gap:6px;flex:none;padding:13px 12px;
    border:0;border-bottom:2px solid transparent;background:transparent;color:var(--muted);
    font:inherit;font-size:.74rem;font-weight:650;cursor:pointer
  }
  .approval-filters button:hover { color:var(--ink) }
  .approval-filters button.active { border-bottom-color:var(--accent);color:var(--accent) }
  /* Processing issues are a different kind of notice, kept apart at the end. */
  .approval-filters button.attention { margin-left:auto }
  .approval-filters button.attention .filter-count {
    border-color:color-mix(in srgb,var(--danger) 30%,var(--line));background:var(--danger-soft);color:var(--danger)
  }
  .filter-count {
    display:inline-grid;place-items:center;box-sizing:border-box;min-width:19px;height:19px;padding:0 5px;
    border:1px solid var(--line);border-radius:999px;background:var(--surface-2);
    color:var(--muted);font-size:.61rem;font-variant-numeric:tabular-nums;line-height:1
  }
  .approval-filters button.active .filter-count {
    border-color:color-mix(in srgb,var(--accent) 28%,var(--line));background:var(--tint);color:var(--accent)
  }
  .review-skeleton { display:grid }
  .approval-loading-row { padding:24px 28px;border-bottom:1px solid var(--line) }
  .approval-loading-row:last-child { border-bottom:0 }
  .approval-loading-row .skel { width:68% }
  .filtered-empty { margin:0;padding:32px 22px;color:var(--muted);text-align:center }
  .review-panes { display:grid;grid-template-columns:minmax(0,1fr);width:100%;margin:0;background:var(--surface) }
  .review-panes.with-suggestion-groups { grid-template-columns:minmax(0,1fr) 400px }
  /* On a suggestion tab side by side, the card fills the space below the
     topbar (64px) and inside .content's padding (20px top, 40px bottom); each
     pane then scrolls under its own caption, so the header, tabs and captions
     always stay in view. Other tabs flow with the page. */
  @media (min-width:1051px) {
    :global(.rule-workspace.approvals-workspace.fitted) { display:flex;flex-direction:column;max-height:calc(100dvh - 124px) }
    .approval-filters { flex:none }
    .review-panes { flex:1 1 auto;min-height:0;grid-template-rows:minmax(0,1fr) }
    .document-review-pane,.suggestion-review-pane { display:flex;flex-direction:column;min-height:0 }
    .pane-scroll { flex:1;min-height:0;overflow-y:auto }
  }
  .document-review-pane { min-width:0;background:var(--surface) }
  .suggestion-review-pane {
    min-width:0;border-left:1px solid var(--line);
    background:color-mix(in srgb,var(--surface-2) 28%,var(--surface))
  }
  .pane-heading { display:flex;align-items:center;min-height:40px;padding:9px 28px;border-bottom:1px solid var(--line) }
  .pane-heading-label { display:flex;align-items:baseline;gap:9px;min-width:0 }
  .pane-heading h2 { margin:0;color:var(--muted);font-size:.7rem;font-weight:750;letter-spacing:.08em;text-transform:uppercase }
  .pane-heading span { color:var(--muted);font-size:.68rem }
  .sync-mark { display:inline-flex;margin-left:auto;color:var(--muted);cursor:help }
  .document-review-card,.document-review-card .decision-row,.suggestion-card { transition:background-color .15s,box-shadow .15s,border-color .15s }
  .document-review-card.linked { background:color-mix(in srgb,var(--accent) 5%,var(--surface)) }
  /* Only a card with several decisions marks which of them are linked. */
  .document-review-card .decision-row.linked:not(:only-child) {
    border-radius:6px;background:color-mix(in srgb,var(--accent) 7%,var(--surface));
    box-shadow:0 0 0 5px color-mix(in srgb,var(--accent) 7%,var(--surface))
  }
  .suggestion-card.linked .suggestion-card-header,.suggestion-card.linked .suggestion-card-actions { background:transparent }
  .suggestion-card.linked { border-color:color-mix(in srgb,var(--accent) 40%,var(--line));background:color-mix(in srgb,var(--accent) 5%,var(--surface)) }
  .document-review-list { display:block }
  .suggestion-review-list { display:grid;gap:12px;padding:14px 14px 16px }
  .review-overflow { margin:0;padding:14px 28px;border-top:1px solid var(--line);color:var(--muted);font-size:.72rem }
  .tab-panel { padding-top:20px }
  .tab-actions { display:flex;justify-content:flex-end;margin:0 20px 12px }
  .approval-grid { display:grid;max-width:820px;gap:12px;align-items:start;margin:0 20px 22px }
  .approval-grid.approval-grid-many { max-width:none;grid-template-columns:repeat(2,minmax(0,1fr)) }
  .approval-card {
    min-width:0;border:1px solid var(--line);border-radius:11px;overflow:hidden;
    background:var(--bg);box-shadow:0 10px 24px color-mix(in srgb,var(--ink) 4%,transparent)
  }
  .document-review-card { display:flex;gap:12px;align-items:flex-start;padding:16px 28px;background:var(--surface) }
  .document-review-card + .document-review-card { border-top:1px solid var(--line) }
  .document-review-card .task-thumb { flex-basis:32px;width:32px;border-radius:5px }
  .document-review-body { display:grid;gap:4px;flex:1;min-width:0 }
  .approval-card-header { border-bottom:1px solid var(--line);background:var(--surface-2) }
  .approval-card-body { background:var(--bg) }
  .document-header { display:flex;gap:11px;align-items:center;min-height:26px }
  .document-identity { flex:1;min-width:0 }
  .document-identity > a { color:var(--ink);font-weight:650;overflow-wrap:anywhere }
  .document-title-line { display:flex;align-items:baseline;gap:8px;min-width:0 }
  .document-title-line a { min-width:0;overflow:hidden;color:var(--ink);font-size:.82rem;font-weight:700;text-overflow:ellipsis;white-space:nowrap }
  .document-title-line > span:not(.filing-pill),.document-meta { color:var(--muted);font-family:"Spline Sans Mono",ui-monospace,monospace;font-size:.61rem }
  .document-meta { display:flex;align-items:center;gap:7px;flex-wrap:wrap;margin-top:6px }
  .filing-pill {
    display:inline-flex;flex:none;max-width:100%;padding:2px 7px;border-radius:999px;border:1px solid var(--line);
    background:var(--bg);color:var(--muted);font-size:.61rem;font-weight:600;overflow-wrap:anywhere
  }
  .document-bulk-actions { display:flex;align-items:center;justify-content:flex-end;gap:6px;flex-wrap:wrap }
  .document-decisions { display:grid }
  .document-review-card .decision-row { padding:2px 0;border-bottom:0 }
  .document-decision {
    display:grid;grid-template-columns:126px minmax(0,1fr) 148px 96px 68px;
    column-gap:14px;align-items:center
  }
  .decision-kind { grid-column:1;color:var(--muted);font-size:.72rem;font-weight:650;line-height:1.35 }
  .metadata-transition {
    grid-column:2;display:flex;min-width:0;align-items:baseline;gap:7px;
    margin:0;font-size:.76rem;line-height:1.4;overflow-wrap:anywhere
  }
  .metadata-transition > span:first-child { min-width:0;color:var(--muted);text-decoration:line-through }
  .metadata-transition > strong { min-width:0;color:var(--ink);font-weight:700 }
  .transition-arrow { flex:none;color:var(--faint) }
  .metadata-transition > span.addition-mark { flex:none;color:var(--accent);font-weight:700;text-decoration:none }
  .decision-source { grid-column:3;justify-self:start;max-width:100% }
  .review-rationale-toggle {
    grid-column:4;justify-self:start;max-width:100%;padding:3px 0;border:0;background:transparent;
    color:var(--muted);font:inherit;font-size:.7rem;line-height:1.3;overflow:hidden;text-overflow:ellipsis;
    white-space:nowrap;cursor:pointer
  }
  .review-rationale-toggle:hover { color:var(--accent) }
  .decision-controls { grid-column:5;display:flex;justify-content:flex-end;gap:6px }
  .decision-button {
    display:grid;place-items:center;width:31px;height:31px;padding:0;border:1px solid var(--line);
    border-radius:7px;background:var(--bg);color:var(--muted);cursor:pointer
  }
  .decision-button:hover:not(:disabled) { border-color:var(--line-strong);color:var(--ink) }
  .decision-button.approve { border-color:color-mix(in srgb,var(--accent) 45%,var(--line));color:var(--accent) }
  .decision-button.approve:hover:not(:disabled) { background:var(--accent);color:white }
  .decision-button:disabled { opacity:.45;cursor:default }
  .decision-followup { grid-column:2 / 5;min-width:0;margin-top:8px;color:var(--muted);font-size:.73rem }
  .review-rationale-content { padding:8px 10px;border-left:2px solid var(--line);background:var(--surface) }
  .intelligence-evidence { margin:0 0 6px;color:var(--muted);font-size:.78rem;line-height:1.5;white-space:pre-wrap;overflow-wrap:anywhere }
  .review-sources { margin:0;padding-left:18px;font-size:.78rem;overflow-wrap:anywhere }
  .review-error { color:var(--danger);font-size:.75rem;line-height:1.45;overflow-wrap:anywhere }
  .suggestion-card { background:var(--surface) }
  .suggestion-card-header { display:flex;align-items:center;justify-content:space-between;gap:10px;padding:12px 13px;background:var(--surface) }
  .suggestion-card-header > div { display:flex;min-width:0;align-items:baseline;gap:9px;overflow:hidden }
  .suggestion-card-header > div > span { flex:none;color:var(--ink);font-size:.74rem;font-weight:700 }
  .suggestion-card-header strong { min-width:0;font-size:.76rem;line-height:1.35;overflow:hidden;text-overflow:ellipsis;white-space:nowrap }
  .suggestion-card-header .addition-mark { margin-right:6px;color:var(--accent) }
  .source-pill { display:inline-flex;flex:none;max-width:46%;padding:3px 8px;border:1px solid var(--line);border-radius:999px;color:var(--muted);font-size:.59rem;overflow:hidden;text-overflow:ellipsis;white-space:nowrap }
  .source-pill.decision-source { max-width:100% }
  /* minmax(0,…) lets a long unbroken name ellipsize instead of widening the
     column past the card and pushing the document number out of view. */
  .suggestion-documents { display:grid;grid-template-columns:minmax(0,1fr);padding:4px 13px }
  .suggestion-documents a { display:flex;align-items:baseline;justify-content:space-between;gap:9px;padding:6px 0;color:var(--ink);font-size:.74rem }
  .suggestion-documents a strong { min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap }
  .suggestion-documents a span { flex:none;color:var(--faint);font-family:"Spline Sans Mono",ui-monospace,monospace;font-size:.58rem }
  .more-documents { justify-self:start;padding:8px 0;border:0;background:transparent;color:var(--accent);font:inherit;font-size:.68rem;font-weight:650;cursor:pointer }
  .suggestion-card-actions { display:flex;align-items:center;justify-content:flex-end;gap:7px;padding:6px 13px 13px;background:var(--surface) }
  .suggestion-error { margin:8px 13px }
  .date-value { display:flex;align-items:center;gap:7px;flex-wrap:wrap }
  .decision { font-size:1rem;line-height:1.35;overflow-wrap:anywhere }
  .task-thumb {
    display:flex;align-items:center;justify-content:center;flex:0 0 46px;width:46px;aspect-ratio:3 / 4;
    border:1px solid var(--line);border-radius:6px;overflow:hidden;background:var(--surface-2)
  }
  .task-thumb img { width:100%;height:100%;object-fit:cover;display:block }
  .task-thumb.blurred img { filter:blur(6px) }
  .task-thumb.placeholder { color:var(--faint) }
  .decision-list { padding:0 13px }
  .decision-row { padding:13px 0;border-bottom:1px solid var(--line) }
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
  .processing-issue { align-items:flex-start;flex-wrap:wrap;cursor:default }
  .processing-issue-copy { display:flex;flex:1 1 260px;min-width:0;flex-direction:column;gap:3px }
  .processing-details { color:var(--muted);font-size:.7rem }
  .processing-details summary { width:max-content;cursor:pointer }
  .processing-details code { display:block;margin-top:6px;color:var(--faint) }
  .processing-details pre { max-width:720px;margin:6px 0 0;overflow:auto;white-space:pre-wrap;overflow-wrap:anywhere;color:var(--muted);font:inherit }
  .approvals-page .index { margin:0 20px 22px }
  .review-panes.with-suggestion-groups .document-decision { grid-template-columns:126px minmax(0,1fr) 68px }
  .review-panes.with-suggestion-groups .decision-source,
  .review-panes.with-suggestion-groups .review-rationale-toggle,
  .review-panes.with-suggestion-groups .review-rationale-content { display:none }
  .review-panes.with-suggestion-groups .decision-controls { grid-column:3 }
  .review-panes.with-suggestion-groups .decision-followup { grid-column:2 }

  @media (max-width:1050px) {
    .review-panes.with-suggestion-groups { grid-template-columns:1fr }
    .suggestion-review-pane { border-top:1px solid var(--line);border-left:0 }
    .review-panes.with-suggestion-groups .document-decision {
      grid-template-columns:126px minmax(0,1fr) 148px 96px 68px
    }
    .review-panes.with-suggestion-groups .decision-source { display:inline-flex }
    .review-panes.with-suggestion-groups .review-rationale-toggle { display:block }
    .review-panes.with-suggestion-groups .review-rationale-content { display:block }
    .review-panes.with-suggestion-groups .decision-controls { grid-column:5 }
    .review-panes.with-suggestion-groups .decision-followup { grid-column:2 / 5 }
    .approval-grid.approval-grid-many { grid-template-columns:1fr }
  }
  @media (max-width:780px) {
    .document-decision,
    .review-panes.with-suggestion-groups .document-decision {
      grid-template-columns:108px minmax(0,1fr) 68px;row-gap:8px
    }
    .decision-kind { grid-column:1 }
    .metadata-transition { grid-column:2 }
    .decision-source { grid-column:2 }
    .review-rationale-toggle { grid-column:2 }
    .decision-controls,
    .review-panes.with-suggestion-groups .decision-controls { grid-column:3 }
    .decision-followup,
    .review-panes.with-suggestion-groups .decision-followup { grid-column:2 }
  }
  @media (max-width:620px) {
    .review-toolbar { width:100%;justify-content:space-between }
    .approval-filters { padding-inline:8px }
    .document-review-card { padding-inline:16px }
    .document-decisions { gap:14px }
    .document-header { flex-wrap:wrap }
    .document-bulk-actions { width:100%;justify-content:flex-start }
    .document-decision,
    .review-panes.with-suggestion-groups .document-decision {
      grid-template-columns:minmax(0,1fr) 68px
    }
    .decision-kind { grid-column:1 / -1 }
    .metadata-transition,.decision-source,.review-rationale-toggle { grid-column:1 }
    .decision-controls,
    .review-panes.with-suggestion-groups .decision-controls { grid-column:2 }
    .decision-followup,
    .review-panes.with-suggestion-groups .decision-followup { grid-column:1 / -1 }
    .task-thumb { flex-basis:42px;width:42px }
    .pane-heading { padding-inline:16px }
    .approval-grid { margin-inline:14px }
    .tab-actions { margin-inline:14px }
  }
</style>
