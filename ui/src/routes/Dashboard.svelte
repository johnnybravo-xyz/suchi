<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "../lib/i18n.svelte.js";
  import { scopedHash as filingHref } from '../lib/systems.svelte.js'
  import { onDestroy } from 'svelte'
  import { listIntelligence, listSavedViews } from '../lib/api.js'
  import { documentListHash, parseSavedViewFilters } from '../lib/documentFilters.js'
  import { formatDate, formatDaysAway, formatNumber, plural, sensDot } from '../lib/format.js'
  import { addDays, daysUntil, formatIntelligenceDate, intelligenceRoleLabel, localISODate, upcomingDates } from '../lib/intelligence.js'
  import { session } from '../lib/session.svelte.js'
  import { hasCapability } from '../lib/capabilities.js'
  import Icon from '../lib/Icon.svelte'

  let {
    st, statsError = '', inboxCategory, taxonomyLoaded = false, taxonomyError = '', recent,
    recentError = '', onRetryRecent, setupNeeded = false, setupError = false, onRetrySetup,
    canViewDates = false,
  } = $props()
  const inboxCount = $derived(st?.inbox_count ?? 0)
  const pending = $derived((st?.pending_approvals ?? 0) + (st?.pending_intelligence ?? 0))
  const dead = $derived(st?.dead_jobs ?? 0)

  const total = $derived(st?.documents_total ?? null)
  const dashboardViewLimit = 4
  let views = $state([])
  let viewsLoading = $state(true)
  let viewsError = $state('')
  let viewTotal = $state(0)
  let loadVersion = 0
  let activeController
  // Accepted exact-day due, expiry and renewal dates in the next 30 days.
  // null until loaded; a failed load keeps the card hidden rather than empty.
  const comingUpDays = 30
  const comingUpLimit = 3
  let comingUp = $state(null)
  const comingUpController = new AbortController()

  // Intake routes besides upload, offered while the archive is empty. Watched
  // folders and archive mailboxes are administrator settings.
  const otherWaysIn = $derived([
    ...(session.user?.role === 'admin' ? [{ label: translate("dashboard.watchAFolder"), href: '#/settings?tab=archive&section=sources' }] : []),
    ...(session.user?.role === 'admin' ? [{ label: translate("dashboard.connectAMailbox"), href: '#/settings?tab=archive&section=mail' }]
      : hasCapability(session.user, 'mailboxes') ? [{ label: translate("dashboard.connectAMailbox"), href: '#/settings' }] : []),
    ...(String(session.user?.kind || '').startsWith('demo') ? [] : [{ label: translate("dashboard.pairAPhone"), href: '#/settings?pair=1' }]),
  ])

  const onboarding = $derived.by(() => {
    if (setupNeeded) {
      return {
        step: translate("dashboard.step1Of3"),
        icon: 'settings',
        title: setupError ? translate("dashboard.checkArchiveSetup") : translate("dashboard.chooseYourFilingTree"),
        body: setupError
          ? translate("dashboard.suchiCouldNotConfirmWhetherSetupIsComplete")
          : translate("dashboard.chooseHowDocumentsWillBeOrganizedBeforeAdding"),
        action: translate("dashboard.continueSetup"),
        href: '#/settings?tab=archive&section=filing-tree',
      }
    }
    if (!st) return null
    if (total === 0) {
      return {
        step: translate("dashboard.step2Of3"),
        icon: 'upload',
        title: translate("dashboard.addYourFirstDocument"),
        body: translate("dashboard.startWithOneRepresentativeDocumentSuchiKeepsThe"),
        action: translate("dashboard.uploadADocument"),
        href: '#/upload',
        alternatives: otherWaysIn,
      }
    }
    if (total === 1 && (inboxCount > 0 || pending > 0 || dead > 0)) {
      if (pending > 0) {
        return {
          step: translate("dashboard.step3Of3"),
          icon: 'tasks',
          title: translate("dashboard.finishYourFirstDocument"),
          body: inboxCount > 0
            ? translate("dashboard.reviewTheWaitingSuggestionOrDecisionThenFile")
            : translate("dashboard.aSuggestionOrDecisionIsWaitingForYour"),
          action: translate("dashboard.reviewIt"),
          href: '#/tasks',
          secondaryAction: inboxCount > 0 ? translate("dashboard.openInbox") : '',
          secondaryHref: '#/inbox',
        }
      }
      if (inboxCount > 0) {
        return {
          step: translate("dashboard.step3Of3"),
          icon: 'inbox',
          title: translate("dashboard.finishFilingYourFirstDocument"),
          body: translate("dashboard.theDocumentIsReadyForAFilingDecision"),
          action: translate("dashboard.openInbox"),
          href: '#/inbox',
        }
      }
      return {
        step: translate("dashboard.step3Of3"),
        icon: 'zap',
        title: translate("dashboard.finishProcessingYourFirstDocument"),
        body: translate("dashboard.processingStoppedBeforeTheDocumentWasReadyOpen"),
        action: translate("dashboard.openProcessingIssues"),
        href: '#/tasks',
      }
    }
    return null
  })

  async function load() {
    const version = ++loadVersion
    activeController?.abort()
    const controller = new AbortController()
    activeController = controller
    viewsLoading = true
    viewsError = ''
    try {
      const res = await listSavedViews({ include: 'shared' }, controller.signal)
      if (version !== loadVersion) return
      const raw = res?.results || res || []
      viewTotal = res?.count ?? raw.length
      const sorted = raw.map(v => ({
        ...v,
        filters: parseSavedViewFilters(v.filter_json),
      })).sort((a, b) => a.position - b.position)

      const selected = sorted.slice(0, dashboardViewLimit)
      const shared = sorted.find((view) => view.owner_id)
      if (shared && !selected.includes(shared)) {
        if (selected.length === dashboardViewLimit) selected[selected.length - 1] = shared
        else selected.push(shared)
      }

      views = selected
    } catch (ex) {
      if (version !== loadVersion) return
      views = []
      viewsError = ex.message || translate("dashboard.couldNotLoadViews")
    } finally {
      if (activeController === controller) activeController = undefined
      if (version === loadVersion) viewsLoading = false
    }
  }

  async function loadComingUp() {
    if (!canViewDates) return
    const today = localISODate()
    try {
      const res = await listIntelligence({
        status: 'accepted', type: 'date', precision: 'day',
        sort_from: today, sort_to: addDays(today, comingUpDays), page_size: 50,
      }, comingUpController.signal)
      comingUp = upcomingDates(res?.results, today)
    } catch {
      comingUp = null
    }
  }

  // Recent rows drop the year while it is the current one.
  function recentDate(unix) {
    if (!unix) return ''
    const date = new Date(unix * 1000)
    return date.getFullYear() === new Date().getFullYear()
      ? formatDate(date, { month: 'short', day: 'numeric' })
      : formatDate(date)
  }

  function filterSummary(f) {
    const parts = []
    if (f.q) parts.push(`“${f.q}”`)
    if (f.tags__id__in) parts.push('tag')
    if (f.correspondents__id__in) parts.push('correspondent')
    if (f.jd_category_id) parts.push(translate("dashboard.filingCategory"))
    if (f.sensitivity) parts.push(f.sensitivity)
    return parts.join(' · ') || translate("dashboard.allDocuments2")
  }

  load()
  void loadComingUp()
  onDestroy(() => {
    loadVersion++
    activeController?.abort()
    comingUpController.abort()
  })
</script>

  {#if onboarding}
    <section class="onboarding-card card" aria-labelledby="onboarding-heading">
      <span class="onboarding-icon"><Icon name={onboarding.icon} size={20} /></span>
      <div class="onboarding-copy">
        <span class="onboarding-step">{translate("dashboard.gettingStartedValue", { "step": onboarding.step })}</span>
        <h2 id="onboarding-heading">{onboarding.title}</h2>
        <p>{onboarding.body}</p>
        <div class="onboarding-actions">
          <a class="btn primary" href={filingHref(onboarding.href)}>{onboarding.action}</a>
          {#if onboarding.secondaryAction}
            <a class="btn" href={filingHref(onboarding.secondaryHref)}>{onboarding.secondaryAction}</a>
          {/if}
          {#if setupError}
            <button class="btn" type="button" onclick={() => onRetrySetup?.()}>{translate("dashboard.retrySetupStatus")}</button>
          {/if}
        </div>
        {#if onboarding.alternatives?.length}
          <p class="onboarding-alternatives">
            {translate("dashboard.otherWaysIn")}
            {#each onboarding.alternatives as way, i (way.href)}{#if i}{' · '}{/if}<a href={filingHref(way.href)}>{way.label}</a>{/each}
          </p>
        {/if}
      </div>
    </section>
  {/if}

<div class="metrics">
  <a class="metric card" href={filingHref("#/documents")}>
    <span class="m-label"><Icon name="docs" size={14} /> {translate("dashboard.totalDocuments")}</span>
    <span class="m-value">{total ?? '—'}</span>
    <span class="m-sub">{st?.ingested_7d ? translate("dashboard.valueAddedInTheLast7Days", { "ingested_7d": st.ingested_7d }) : translate("dashboard.acrossTheWholeArchive")}</span>
  </a>
  <a class="metric card" href={filingHref("#/inbox")} class:attn={inboxCount > 0}>
    <span class="m-label"><Icon name="inbox" size={14} /> {translate("dashboard.inbox")}</span>
    <span class="m-value">{inboxCategory ? inboxCount : '—'}</span>
    <span class="m-sub">{inboxCategory
      ? (inboxCount > 0 ? translate("dashboard.waitingToBeFiled") : translate("dashboard.everythingIsFiled"))
      : (taxonomyLoaded ? (taxonomyError || translate("dashboard.inboxUnavailable")) : translate("dashboard.loadingArchiveStructure"))}</span>
  </a>
  <a class="metric card" href={filingHref("#/tasks")} class:attn={pending > 0}>
    <span class="m-label"><Icon name="tasks" size={14} /> {translate("dashboard.approvals")}</span>
    <span class="m-value">{st ? pending : '—'}</span>
    <span class="m-sub">{st ? (pending > 0 ? translate("dashboard.waitingOnYou") : translate("dashboard.nonePending")) : (statsError || translate("dashboard.loadingArchiveStatus"))}</span>
  </a>
  <a class="metric card" href={filingHref(dead > 0 ? "#/tasks?tab=issues" : "#/tasks")} class:bad={dead > 0}>
    <span class="m-label"><Icon name="zap" size={14} /> {translate("dashboard.failedJobs")}</span>
    <span class="m-value">{st ? dead : '—'}</span>
    <span class="m-sub">{st ? (dead > 0 ? translate("dashboard.deadLetteredNeedsAttention") : translate("dashboard.pipelineHealthy")) : (statsError || translate("dashboard.loadingArchiveStatus"))}</span>
  </a>
</div>

<div class="dash-grid">
  <div>
    <div class="dash-head">
      <h3>{translate("dashboard.recentlyAdded")}</h3>
      <a class="btn sm" href={filingHref("#/documents")}>{translate("dashboard.allDocuments")}</a>
    </div>
    {#if recent === undefined}
      <div class="index" aria-label={translate("dashboard.loadingRecentDocuments")}>
        {#each Array(3) as _}<div class="irow"><div class="skel" style="width:68%"></div></div>{/each}
      </div>
    {:else if recentError}
      <div class="card" style="display:flex;align-items:center;justify-content:space-between;gap:12px;color:var(--muted);font-size:.86rem">
        <span>{recentError}</span><button class="btn sm" onclick={() => onRetryRecent?.({ background: false })}>{translate("common.retry")}</button>
      </div>
    {:else if recent.length}
      <div class="index">
        {#each recent as d (d.id)}
          <a class="irow filing-row" href={filingHref(`#/doc/${d.id}`)}>
            <span class="dot {sensDot(d.sensitivity)}" class:accent={!d.sensitivity}></span>
            <span class="filing-code filing-code-column" title={d.jd_category_name}>{d.jd_address || d.jd_category_code || ''}</span>
            <span class="title grow">{d.title || translate("dashboard.documentValue", { "id": d.id })}</span>
            <span class="sub">{recentDate(d.added_at)}</span>
          </a>
        {/each}
      </div>
    {:else}
      <div class="empty" style="padding:36px 20px">
        <Icon name="docs" size={44} />
        <b>{translate("dashboard.nothingHereYet")}</b>
        <span><a href={filingHref("#/upload")}>{translate("dashboard.uploadTheFirstDocument")}</a>{translate("dashboard.pointAWatchedFolderAtItOr")}</span>
      </div>
    {/if}
  </div>

  <div>
    {#if comingUp}
      <div class="dash-head">
        <h3>{translate("dashboard.comingUp")}{#if comingUp.length > comingUpLimit}<span class="coming-count" title={plural("dashboard.comingUpCount", comingUp.length, { days: comingUpDays })}>{' · '}{formatNumber(comingUp.length)}</span>{/if}</h3>
        <a class="btn sm" href={filingHref("#/calendar")}>{translate("dashboard.calendar")}</a>
      </div>
    {/if}
    {#if comingUp && !comingUp.length}
      <div class="card coming-up" style="color:var(--muted);font-size:.86rem">
        {translate("dashboard.nothingInTheNextDays", { days: comingUpDays })}
      </div>
    {:else if comingUp}
      <div class="index coming-up">
        {#each comingUp.slice(0, comingUpLimit) as event (event.id)}
          <a class="irow" href={filingHref(`#/doc/${event.document_id}`)}>
            <span class="coming-when">
              <b>{formatIntelligenceDate(event, { month: 'short', day: 'numeric' })}</b>
              <small>{formatDaysAway(daysUntil(event.value.date))}</small>
            </span>
            <span class="grow coming-what">
              <span class="title">{event.document_title || translate("dashboard.documentValue", { "id": event.document_id })}</span>
              <span class="sub">{intelligenceRoleLabel(event.role)}</span>
            </span>
          </a>
        {/each}
      </div>
    {/if}
    <div class="dash-head">
      <h3>{translate("dashboard.views")}</h3>
      <a class="btn sm" href={filingHref("#/views?new=1")}><Icon name="plus" size={13} /> {translate("dashboard.newView")}</a>
    </div>
    {#if viewsLoading}
      <div class="card" style="color:var(--muted);font-size:.86rem">{translate("dashboard.loadingViews")}</div>
    {:else if viewsError}
      <div class="card" style="display:flex;align-items:center;justify-content:space-between;gap:12px;color:var(--muted);font-size:.86rem">
        <span>{viewsError}</span><button class="btn sm" onclick={load}>{translate("common.retry")}</button>
      </div>
    {:else if views.length}
      <div class="views">
        {#each views as v (v.id)}
          <a class="card view" href={filingHref(documentListHash(v.filters))}>
            <span class="view-name">{v.name}</span>
            <span class="m-sub mono" style="font-size:.68rem">{filterSummary(v.filters)}</span>
          </a>
        {/each}
      </div>
      {#if viewTotal > views.length}<a class="sub" href={filingHref("#/views")}>{translate("dashboard.viewAllValueSavedViews", { "viewTotal": viewTotal })}</a>{/if}
    {:else}
      <div class="card" style="color:var(--muted);font-size:.86rem">
        {translate("dashboard.noSavedViewsYet")}
      </div>
    {/if}
  </div>
</div>
