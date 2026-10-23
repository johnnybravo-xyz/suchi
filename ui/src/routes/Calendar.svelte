<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from '../lib/i18n.svelte.js'
  import { formatDate, formatNumber, plural } from '../lib/format.js'
  import { scopedHash as filingHref } from '../lib/systems.svelte.js'
  import { onDestroy } from 'svelte'
  import { listIntelligence, listSavedViews } from '../lib/api.js'
  import { DATE_ROLES, intelligenceRoleLabel, intelligenceDateValue, formatArchiveDate, formatIntelligenceDate, groupCalendarEvents } from '../lib/intelligence.js'
  import { canExportCalendarDate, downloadCalendarDate } from '../lib/calendarExport.js'
  import { go } from '../lib/router.svelte.js'
  import Icon from '../lib/Icon.svelte'
  import RuleWorkspace from '../lib/RuleWorkspace.svelte'

  let { query = new URLSearchParams(), demo = false } = $props()
  let events = $state([])
  let views = $state([])
  let loading = $state(true)
  let error = $state('')
  let exportError = $state('')
  let eventTotal = $state(0)
  let pageNumber = $state(1)
  let focusedDay = $state('')
  let loadVersion = 0
  let activeController // rapid range changes should release the older read
  let agendaList = $state()

  const calendarPageSize = 500

  const documentIDs = $derived(query.get('document_ids') || '')
  // Fixed demo fixtures should remain discoverable without depending on today's date.
  const allDates = $derived(Boolean(documentIDs) || (demo && !query.has('month') && !query.has('start') && !query.has('days') && !query.has('date')))
  const selectedDay = $derived(allDates ? '' : query.get('date') || '')
  const agendaOnly = $derived(allDates || Boolean(selectedDay))
  const selectedViewID = $derived(allDates ? '' : query.get('view_id') || '')
  const role = $derived(query.get('role') || '')
  const todayISO = isoDate(new Date())
  const today = parseISODate(todayISO)
  const currentMonth = new Date(today.getFullYear(), today.getMonth(), 1)
  const rollingDays = $derived(query.get('days') === '14' ? 14 : query.get('days') === '30' ? 30 : 0)
  const rollingRange = $derived(rollingDays > 0)
  const calendarMonth = $derived(parseMonth(query.get('month')) || currentMonth)
  const rangeStart = $derived(rollingRange ? parseISODate(query.get('start')) || today : calendarMonth)
  const rangeDays = $derived(rollingRange ? rollingDays : daysInMonth(calendarMonth))
  const rangeEnd = $derived(addDays(rangeStart, rangeDays - 1))
  const rangeLabel = $derived(rollingRange
    ? formatRangeLabel(rangeStart, rangeEnd)
    : formatDate(calendarMonth, { month: 'long', year: 'numeric' }))
  const defaultCalendar = $derived(!rollingRange && sameMonth(calendarMonth, currentMonth))
  const currentMonthLabel = $derived(formatDate(currentMonth, { month: 'long' }))
  const documentCount = $derived(new Set(documentIDs.split(',').filter(Boolean)).size)
  const calendarDays = $derived(buildRollingCalendarDays(rangeStart, rangeDays))
  const eventMap = $derived(groupCalendarEvents(events))
  const prevAccepted = event => dateOrigin(event) === 'previously-accepted'
  const reviewedDate = event => dateOrigin(event) === 'reviewed'
  const agendaGroups = $derived.by(() => {
    const groups = []
    const groupsByKey = new Map()
    for (const event of events) {
      const precision = event.value?.precision || 'day'
      const date = intelligenceDateValue(event)
      const key = `${precision}:${date}`
      let group = groupsByKey.get(key)
      if (!group) {
        group = { key, date, precision, items: [] }
        groupsByKey.set(key, group)
        groups.push(group)
      }
      group.items.push(event)
    }
    return groups
  })
  function groupLabel(group) {
    if (group.precision === 'year') return formatArchiveDate(group.date, { year: 'numeric' })
    if (group.precision === 'month') return formatArchiveDate(group.date, { month: 'long', year: 'numeric' })
    return formatArchiveDate(group.date, { weekday: 'short', day: 'numeric', month: 'short' })
  }

  function isoDate(date) {
    const year = date.getFullYear()
    const monthNumber = String(date.getMonth() + 1).padStart(2, '0')
    const day = String(date.getDate()).padStart(2, '0')
    return `${year}-${monthNumber}-${day}`
  }

  function parseISODate(value) {
    if (!/^\d{4}-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])$/.test(value || '')) return null
    const parsed = new Date(`${value}T00:00:00`)
    return isoDate(parsed) === value ? parsed : null
  }

  function monthToken(date) {
    return isoDate(date).slice(0, 7)
  }

  function parseMonth(value) {
    if (!/^\d{4}-(0[1-9]|1[0-2])$/.test(value || '')) return null
    const parsed = new Date(`${value}-01T00:00:00`)
    return monthToken(parsed) === value ? parsed : null
  }

  function sameMonth(left, right) {
    return left.getFullYear() === right.getFullYear() && left.getMonth() === right.getMonth()
  }

  function daysInMonth(value) {
    return new Date(value.getFullYear(), value.getMonth() + 1, 0).getDate()
  }

  function addDays(value, amount) {
    return new Date(value.getFullYear(), value.getMonth(), value.getDate() + amount)
  }

  function rangeBounds(value, days) {
    return {
      sort_from: isoDate(value),
      sort_to: isoDate(addDays(value, days - 1)),
    }
  }

  function formatRangeLabel(start, end) {
    const sameYear = start.getFullYear() === end.getFullYear()
    const startOptions = { month: 'short', day: 'numeric', ...(sameYear ? {} : { year: 'numeric' }) }
    return translate("calendar.valueValue", { "startOptions": formatDate(start, startOptions), "numeric": formatDate(end, { month: 'short', day: 'numeric', year: 'numeric' }) })
  }

  function calendarHref({
    date = selectedDay,
    rollingValue = rollingRange,
    monthValue = calendarMonth,
    startValue = rangeStart,
    days = rollingDays || 30,
    viewID = selectedViewID,
    dateRole = role,
  } = {}) {
    const params = new URLSearchParams()
    if (allDates) {
      if (documentIDs) params.set('document_ids', documentIDs)
    } else {
      if (date) {
        params.set('date', date)
      } else if (rollingValue) {
        params.set('start', isoDate(startValue))
        params.set('days', String(days))
      } else if (demo || !sameMonth(monthValue, currentMonth)) {
        params.set('month', monthToken(monthValue))
      }
      if (viewID) params.set('view_id', viewID)
    }
    if (dateRole) params.set('role', dateRole)
    const suffix = params.toString()
    return `#/calendar${suffix ? `?${suffix}` : ''}`
  }

  function rollingHref(days) {
    return calendarHref({ date: '', rollingValue: true, startValue: today, days })
  }

  function currentCalendarHref() {
    return calendarHref({ date: '', rollingValue: false, monthValue: currentMonth })
  }

  function buildRollingCalendarDays(value, days) {
    const leadingDays = value.getDay()
    const cellCount = Math.ceil((leadingDays + days) / 7) * 7
    return Array.from({ length: cellCount }, (_, index) => {
      const offset = index - leadingDays
      const date = addDays(value, offset)
      return { date, iso: isoDate(date), inRange: offset >= 0 && offset < days }
    })
  }

  function dateOrigin(event) {
    if (event.extractor === 'demo-corpus') return 'demo'
    if (event.reviewed_at) return 'reviewed'
    if (event.status === 'accepted' && !event.reviewed_by && event.policy_version === 'threshold-auto-v1') return 'automatic'
    return 'previously-accepted'
  }

  function dateOriginLabel(event) {
    const key = dateOrigin(event)
    if (key === 'demo') return translate('calendar.demoExample')
    if (key === 'reviewed') return translate('calendar.reviewed')
    if (key === 'automatic') return translate('calendar.automatic')
    return translate('calendar.previouslyAccepted')
  }

  function downloadDate(event) {
    exportError = ''
    try {
      downloadCalendarDate(event)
    } catch (ex) {
      exportError = ex.message || translate("calendar.couldNotDownloadTheCalendarEvent")
    }
  }

  async function load() {
    const version = ++loadVersion
    activeController?.abort()
    const controller = new AbortController()
    activeController = controller
    loading = true
    error = ''
    exportError = ''
    events = []
    try {
      const response = await listIntelligence({
        status: 'accepted', type: 'date', role,
        page_size: calendarPageSize, page: pageNumber,
        ...(allDates ? { document_ids: documentIDs } : {
          view_id: selectedViewID,
          ...(selectedDay ? { sort_from: selectedDay, sort_to: selectedDay, precision: 'day' } : rangeBounds(rangeStart, rangeDays)),
        }),
      }, controller.signal)
      if (version !== loadVersion) return
      events = response?.results || []
      eventTotal = response?.count ?? events.length
      if (agendaOnly) pageNumber = Math.min(pageNumber, Math.max(1, Math.ceil(eventTotal / calendarPageSize)))
    } catch (ex) {
      if (version === loadVersion) error = ex.message || translate("calendar.couldNotLoadDates")
    } finally {
      if (activeController === controller) activeController = undefined
      if (version === loadVersion) loading = false
    }
  }

  async function loadViews() {
    if (allDates) return
    try {
      const response = await listSavedViews({ include: 'shared' })
      views = response?.results || []
    } catch {}
  }

  function moveRange(offset) {
    focusedDay = ''
    if (rollingRange) {
      go(calendarHref({ date: '', startValue: addDays(rangeStart, offset * rangeDays) }))
      return
    }
    const monthValue = new Date(calendarMonth.getFullYear(), calendarMonth.getMonth() + offset, 1)
    go(calendarHref({ date: '', rollingValue: false, monthValue }))
  }


  function selectCalendarDay(day) {
    if (!day.inRange) return
    focusedDay = day.iso
  }

  function scrollToFocusedDay(date) {
    if (!agendaList || typeof requestAnimationFrame !== 'function') return
    requestAnimationFrame(() => {
      const target = agendaList?.querySelector(`[data-feed-date="${date}"]`)
      if (!target) return
      const behavior = window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth'
      if (agendaList.scrollHeight > agendaList.clientHeight + 1) {
        agendaList.scrollTo({ top: target.offsetTop, behavior })
      } else {
        target.scrollIntoView({ behavior, block: 'start' })
      }
    })
  }

  loadViews()
  onDestroy(() => {
    loadVersion++
    activeController?.abort()
  })
  $effect(() => {
    load()
  })
  $effect(() => {
    const date = focusedDay
    const groupCount = agendaGroups.length
    if (!date || loading || !groupCount) return
    scrollToFocusedDay(date)
  })
</script>

<div class="calendar-page">
  <RuleWorkspace class="calendar-panel"
                 title={translate("calendar.documentDates")}
                 description={demo ? translate("calendar.readOnlyExamplesFromTheDemoDocuments") : translate("calendar.datesFoundInYourDocumentsSuggestionsAwaitingReview")}
                 headingId="calendar-heading">
    {#snippet actions()}
      {#if !agendaOnly}
        <nav class="calendar-ranges" aria-label={translate("calendar.calendarRange")}>
          <a class:active={defaultCalendar} aria-current={defaultCalendar ? 'page' : undefined} aria-label={defaultCalendar ? translate("calendar.valueCurrentCalendarMonth", { "currentMonthLabel": currentMonthLabel }) : translate("calendar.returnToValue", { "currentMonthLabel": currentMonthLabel })} title={defaultCalendar ? translate("calendar.currentCalendarMonth") : translate("calendar.returnToValue", { "currentMonthLabel": currentMonthLabel })} href={filingHref(currentCalendarHref())}>{currentMonthLabel}</a>
          <a class:active={rollingDays === 30} aria-current={rollingDays === 30 ? 'page' : undefined} href={filingHref(rollingHref(30))}>{translate("calendar.30Days")}</a>
          <a class:active={rollingDays === 14} aria-current={rollingDays === 14 ? 'page' : undefined} href={filingHref(rollingHref(14))}>{translate("calendar.2Weeks")}</a>
        </nav>
      {/if}
    {/snippet}

    <div class="calendar-body">
      <div class="calendar-toolbar" class:agenda-toolbar={agendaOnly}>
        {#if allDates}
          <div class="calendar-scope">
            <p>{translate("calendar.valueAllDates", { dates: documentIDs ? plural("calendar.selectedDocumentDateCount", documentCount) : translate("calendar.demoDocumentDates") })}</p>
            <a class="btn sm" href={filingHref(demo ? `#/calendar?month=${monthToken(currentMonth)}` : '#/calendar')}>{translate("calendar.openFullCalendar")}</a>
          </div>
        {:else if selectedDay}
          <div class="calendar-scope">
            <p>{formatArchiveDate(selectedDay, { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric' })}</p>
            <a class="btn sm" href={filingHref(currentCalendarHref())}>{translate("calendar.backToCalendar")}</a>
          </div>
        {:else}
          <nav class="calendar-nav" aria-label={translate("calendar.calendarPeriod")}>
            <button class="btn sm" onclick={() => moveRange(-1)} aria-label={rollingRange ? translate("calendar.previousValueDays", { "rangeDays": rangeDays }) : translate("calendar.previousMonth")}><Icon name="left" size={13} /></button>
            <strong class="range-title">{rangeLabel}</strong>
            <button class="btn sm" onclick={() => moveRange(1)} aria-label={rollingRange ? translate("calendar.nextValueDays", { "rangeDays": rangeDays }) : translate("calendar.nextMonth")}><Icon name="chev" size={13} /></button>
          </nav>
        {/if}
        <div class="calendar-filters">
          {#if !allDates}
            <label>
              <select class="input" aria-label={translate("calendar.documentView")} onchange={event => go(calendarHref({ viewID: event.currentTarget.value }))} autocomplete="off">
                <option value="" selected={!selectedViewID}>{translate("calendar.allDocuments")}</option>
                {#each views as view (view.id)}<option value={view.id} selected={String(view.id) === selectedViewID}>{view.name}</option>{/each}
              </select>
            </label>
          {/if}
          <label>
            <select class="input" aria-label={translate("calendar.dateRole")} value={role} onchange={event => go(calendarHref({ dateRole: event.currentTarget.value }))}>
              <option value="">{translate("calendar.everyRole")}</option>
              {#each DATE_ROLES as value}<option value={value}>{intelligenceRoleLabel(value)}</option>{/each}
            </select>
          </label>
        </div>
      </div>

      {#if error}
        <div class="err calendar-error"><span>{error}</span><button class="btn sm" onclick={load}>{translate("common.retry")}</button></div>
      {/if}
      {#if exportError}<p class="err" role="alert">{exportError}</p>{/if}

      <div class="calendar-layout" class:agenda-only={agendaOnly}>
    {#if !agendaOnly}
    <section class="month-grid" aria-label={rangeLabel}>
      <div class="weekday-row" aria-hidden="true">
        {#each [translate("calendar.sun"), translate("calendar.mon"), translate("calendar.tue"), translate("calendar.wed"), translate("calendar.thu"), translate("calendar.fri"), translate("calendar.sat")] as day}<span>{day}</span>{/each}
      </div>
      <div class="day-grid" class:loading>
        {#each calendarDays as day (day.iso)}
          {@const dayEvents = day.inRange ? eventMap.get(day.iso) || [] : []}
          <div class="day" class:outside={!day.inRange} class:today={day.iso === todayISO} class:selected={day.iso === focusedDay} class:has-events={dayEvents.length > 0}>
            {#if day.inRange}
              <button class="day-number" onclick={() => selectCalendarDay(day)} aria-pressed={day.iso === focusedDay} aria-label={translate("calendar.showDatesForValue", { "iso": formatArchiveDate(day.iso) })}>{formatDate(day.date, { day: 'numeric' })}</button>
              <div class="day-events">
                {#each dayEvents.slice(0, 3) as event (event.id)}
                  <a class:prev={prevAccepted(event)} class:rev={reviewedDate(event)} href={filingHref(`#/doc/${event.document_id}`)} title={translate("calendar.valueValueValue", { "role": intelligenceRoleLabel(event.role), "event": dateOriginLabel(event), "document_title": event.document_title })}>
                    <span class="day-event-title">{event.document_title || translate("calendar.documentValue", { "document_id": event.document_id })}</span>
                  </a>
                {/each}
                {#if dayEvents.length > 3}<button class="day-more" onclick={() => selectCalendarDay(day)}>{translate("calendar.valueMore", { "length": dayEvents.length - 3 })}</button>{/if}
              </div>
            {:else}
              <span class="day-number" aria-hidden="true">{formatDate(day.date, { day: 'numeric' })}</span>
            {/if}
          </div>
        {/each}
      </div>
    </section>
    {/if}

    <aside class="agenda" aria-labelledby="agenda-title">
      <header>
        <b class="agenda-month">{allDates ? translate("calendar.allDates") : selectedDay ? formatArchiveDate(selectedDay, { day: 'numeric', month: 'long' }) : rangeLabel}</b>
        <h3 id="agenda-title" class="agenda-count">
          {#if loading}{translate("common.loading")}
          {:else if error}{translate("calendar.unavailable")}
          {:else if eventTotal > events.length}{translate("calendar.shownOfDates", { shown: formatNumber(events.length), dates: plural("calendar.dateCount", eventTotal) })}
          {:else}{plural("calendar.dateCount", eventTotal)}{/if}
        </h3>
      </header>
      {#if !agendaOnly && !loading && eventTotal > events.length}<small class="agenda-limit">{translate("calendar.showingTheFirstValueNarrowTheView", { "calendarPageSize": calendarPageSize })}</small>{/if}
      {#if loading && !events.length}
        {#each Array(4) as _}<div class="agenda-skeleton"><div class="skel"></div><div class="skel"></div></div>{/each}
      {:else if error}
        <div class="agenda-empty"><Icon name="calendar" size={34} /><b>{translate("calendar.datesUnavailable")}</b><span>{translate("calendar.retryToLoadDatesForThisScope")}</span></div>
      {:else if !events.length}
        <div class="agenda-empty"><Icon name="calendar" size={34} /><b>{allDates ? translate("calendar.noMatchingDates") : selectedDay ? translate("calendar.noDatesThisDay") : translate("calendar.noDatesInThisRange")}</b></div>
      {:else}
        <div class="agenda-list" bind:this={agendaList}>
          {#each agendaGroups as group (group.key)}
            <section class="feed-group" class:focused={group.precision === 'day' && group.date === focusedDay} data-feed-date={group.precision === 'day' ? group.date : undefined}>
              <div class="feed-day"><b>{groupLabel(group)}</b>{#if group.items.length > 1}<span>{plural("calendar.dateCount", group.items.length)}</span>{/if}</div>
              {#each group.items as event (event.id)}
                <article class="agenda-event">
                  <span class="feed-thumb" aria-hidden="true"></span>
                  <div class="agenda-copy">
                    <a class="agenda-document" href={filingHref(`#/doc/${event.document_id}`)}><b>{event.document_title || translate("calendar.documentValue", { "document_id": event.document_id })}</b><Icon name="chev" size={13} /></a>
                    <div class="agenda-meta">
                      <strong>{intelligenceRoleLabel(event.role)}</strong>
                      {#if prevAccepted(event)}<span class="pill-prev">{translate("calendar.previouslyAccepted")}</span>{:else if reviewedDate(event)}<span class="pill-rev">{translate("calendar.reviewed")}</span>{/if}
                      {#if event.extractor === 'demo-corpus'}
                        <span class="date-origin">{translate("calendar.demoExample")}</span>
                      {:else if typeof event.confidence === 'number' || event.evidence_text}
                        <details class="date-details">
                          <summary class="date-origin">{translate("calendar.fromTheDocument")}</summary>
                          {#if event.evidence_text}<p>“{event.evidence_text}”</p>{/if}
                          {#if typeof event.confidence === 'number'}<p>{translate("calendar.modelConfidenceValue", { "confidence": Math.round(event.confidence * 100) })}</p>{/if}
                        </details>
                      {/if}
                    </div>
                    {#if agendaOnly}<small>{formatIntelligenceDate(event, { weekday: 'short', day: 'numeric', month: 'short', year: 'numeric' })}</small>{/if}
                    {#if canExportCalendarDate(event)}
                      <details class="calendar-export">
                        <summary>{translate("calendar.addToCalendar")}</summary>
                        <p>{translate("calendar.theIcsFileIncludesTheDocumentTitle")}</p>
                        <button class="btn sm" onclick={() => downloadDate(event)}><Icon name="download" size={13} />{translate("calendar.downloadIcs")}</button>
                      </details>
                    {/if}
                  </div>
                </article>
              {/each}
            </section>
          {/each}
        </div>
      {/if}
      {#if agendaOnly && !loading && !error && eventTotal > calendarPageSize}
        <nav class="agenda-pages" aria-label={translate("calendar.datePages")}>
          <button class="btn sm" disabled={pageNumber === 1} onclick={() => { pageNumber-- }}>{translate("calendar.previous")}</button>
          <span>{translate("calendar.pageValueOfValue", { "pageNumber": pageNumber, "calendarPageSize": Math.ceil(eventTotal / calendarPageSize) })}</span>
          <button class="btn sm" disabled={pageNumber * calendarPageSize >= eventTotal} onclick={() => { pageNumber++ }}>{translate("calendar.next")}</button>
        </nav>
      {/if}
    </aside>
  </div>
    </div>
  </RuleWorkspace>
</div>

<style>
  .calendar-page { max-width: 1240px; margin: 0 auto; }
  .calendar-body { padding: 14px; }
  .calendar-filters { display: flex; flex: none; justify-content: flex-end; gap: 8px; }
  .calendar-filters .input { width: auto; min-width: 150px; padding: 7px 30px 7px 10px; border: 1px solid var(--line-strong); border-radius: 8px; background-color: var(--bg); color: var(--ink); font-size: .75rem; font-weight: 600; cursor: pointer; }
  .calendar-toolbar { display: grid; grid-template-columns: minmax(0, 1fr) 340px; align-items: center; gap: 14px; margin-bottom: 10px; }
  .calendar-nav { display: flex; align-items: center; justify-content: center; gap: 7px; }
  .range-title { min-width: 230px; color: var(--ink); font-size: .94rem; font-weight: 700; text-align: center; }
  .calendar-ranges { display: flex; justify-content: flex-end; gap: 8px; }
  .calendar-ranges a { padding: 7px 16px; border: 1px solid var(--line-strong); border-radius: 999px; background: var(--surface); color: var(--muted); font-size: .78rem; font-weight: 650; text-decoration: none; }
  .calendar-ranges a:hover { border-color: var(--accent); color: var(--accent); }
  .calendar-ranges a.active { border-color: var(--accent); background: var(--accent); color: #fff; }
  .calendar-error { display: flex; justify-content: space-between; margin-bottom: 10px; }
  .calendar-layout { display: grid; grid-template-columns: minmax(0, 1fr) 340px; gap: 14px; align-items: stretch; }
  .calendar-scope { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 12px; }
  .calendar-scope p { margin: 0; color: var(--muted); font-size: .8rem; }
  .calendar-layout.agenda-only { display: block; }
  .agenda-only .agenda, .agenda-only .agenda-list { max-height: none; }
  .agenda-only .agenda-event { grid-template-columns: 30px minmax(0, 1fr); }
  .agenda-pages { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 12px 15px; border-top: 1px solid var(--line); font-size: .75rem; }
  .month-grid, .agenda { border: 1px solid var(--line); border-radius: var(--r); background: var(--surface); overflow: hidden; }
  .weekday-row { display: grid; height: 29px; grid-template-columns: repeat(7, 1fr); border-bottom: 1px solid var(--line-strong); background: var(--surface-2); }
  .weekday-row span { display: grid; padding: 0 8px; place-items: center; color: var(--faint); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .61rem; font-weight: 700; text-align: center; }
  .day-grid { display: grid; grid-template-columns: repeat(7, minmax(0, 1fr)); opacity: 1; transition: opacity .16s ease; }
  .day-grid.loading { opacity: .58; }
  .day { position: relative; min-height: 112px; padding: 8px; border-right: 1px solid var(--line); border-bottom: 1px solid var(--line); background: var(--surface); }
  .day:nth-child(7n) { border-right: 0; }
  .day:nth-last-child(-n + 7) { border-bottom: 0; }
  .day.outside { background: var(--bg); color: var(--faint); }
  .day.selected { z-index: 1; background: color-mix(in srgb, var(--accent) 8%, var(--surface)); box-shadow: inset 0 0 0 2px var(--accent); }
  .day.today .day-number { border-radius: 8px; background: var(--accent); color: #fff; }
  .day-number { display: grid; place-items: center; width: 24px; height: 24px; margin-left: auto; padding: 0; border: 0; border-radius: 50%; background: transparent; color: inherit; font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .67rem; text-decoration: none; }
  button.day-number { cursor: pointer; }
  button.day-number::after { position: absolute; inset: 0; content: ''; }
  button.day-number:hover::after { background: color-mix(in srgb, var(--accent) 5%, transparent); }
  button.day-number:focus-visible { outline: none; }
  button.day-number:focus-visible::after { outline: 2px solid var(--accent); outline-offset: -2px; }
  .day-events { display: grid; gap: 4px; margin-top: 4px; }
  .day-events a { position: relative; display: block; min-width: 0; padding: 3px 7px; border: 1px solid var(--line); border-left: 3px solid var(--accent); border-radius: 7px; background: var(--surface); color: var(--ink); font-size: .64rem; font-weight: 600; text-decoration: none; }
  .day-events .day-more { position: relative; justify-self: start; border: 0; padding: 1px 2px; background: transparent; color: var(--accent); font: inherit; font-size: .64rem; font-weight: 600; cursor: pointer; }
  .day-events a.prev { border-left-color: var(--warn); background: color-mix(in srgb, var(--manila) 45%, var(--surface)); }
  .day-events a.rev { border-left-color: var(--ok); background: var(--ok-soft); }
  .day-event-title { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .date-origin { flex: none; padding: 1px 4px; border: 1px solid var(--line); border-radius: 999px; color: var(--muted); font-size: .55rem; font-style: normal; font-weight: 650; line-height: 1.4; }
  .agenda { display: flex; flex-direction: column; min-height: 0; }
  .agenda > header { display: flex; align-items: center; height: 29px; gap: 10px; padding: 0 15px; border-bottom: 1px solid var(--line); }
  .agenda-month { font-size: .85rem; font-weight: 650; letter-spacing: -.01em; }
  .agenda h3.agenda-count { margin-left: auto; color: var(--faint); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .62rem; font-weight: 600; letter-spacing: .05em; text-transform: uppercase; }
  .agenda-limit { display: block; margin: 0; padding: 6px 15px; border-bottom: 1px solid var(--line); color: var(--muted); font-size: .62rem; line-height: 1.35; }
  .agenda-list { position: relative; flex: 1; min-height: 0; overflow-y: auto; overscroll-behavior: contain; }
  .feed-group + .feed-group { border-top: 1px solid var(--line); }
  .feed-group.focused { background: color-mix(in srgb, var(--accent) 7%, var(--surface)); }
  .feed-group.focused .feed-day b { color: var(--accent); }
  .feed-day { display: flex; align-items: baseline; gap: 8px; padding: 11px 15px 5px; }
  .feed-day b { font-size: .76rem; font-weight: 700; }
  .feed-day span { color: var(--faint); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .56rem; font-weight: 600; letter-spacing: .05em; text-transform: uppercase; }
  .feed-thumb { width: 30px; height: 38px; border: 1px solid color-mix(in srgb, var(--manila) 55%, var(--ink) 12%); border-radius: 5px; background: var(--manila); }
  .pill-prev, .pill-rev { padding: 2px 7px; border-radius: 999px; background: var(--manila); color: var(--ink); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .53rem; font-weight: 650; letter-spacing: .04em; text-transform: uppercase; }
  .pill-rev { background: var(--ok-soft); color: var(--ok); }
  .agenda-event { display: grid; grid-template-columns: 30px minmax(0, 1fr); align-items: start; gap: 11px; padding: 8px 15px 10px; }
  .agenda-copy { display: flex; min-width: 0; flex-direction: column; gap: 4px; }
  .agenda-meta { display: flex; align-items: baseline; flex-wrap: wrap; gap: 8px; }
  .agenda-meta > strong { color: var(--accent); font-size: .65rem; }
  .date-details { min-width: 0; }
  .date-details summary { width: fit-content; cursor: pointer; }
  .date-details p { max-width: 38ch; margin: 6px 0; color: var(--muted); font-size: .65rem; line-height: 1.5; }
  .calendar-export { margin-top: 5px; }
  .calendar-export summary { width: fit-content; color: var(--accent); font-size: .7rem; cursor: pointer; }
  .calendar-export p { margin: 7px 0; color: var(--muted); font-size: .68rem; line-height: 1.5; }
  .agenda-copy > small { color: var(--faint); font-size: .59rem; }
  .agenda-document { display: flex; align-items: center; justify-content: space-between; gap: 8px; font-size: .76rem; text-decoration: none; }
  .agenda-document b { overflow-wrap: anywhere; }
  .agenda-document:hover { color: var(--accent); text-decoration: underline; }
  .agenda-document > :global(.ico) { flex: none; color: var(--faint); }
  .agenda-empty { display: flex; align-items: center; flex-direction: column; padding: 58px 20px; color: var(--muted); text-align: center; }
  .agenda-empty :global(.ico) { margin-bottom: 12px; color: var(--accent); }
  .agenda-empty b { color: var(--ink); font-size: .82rem; }
  .agenda-empty span { max-width: 230px; margin-top: 6px; font-size: .7rem; line-height: 1.45; }
  .agenda-skeleton { display: grid; gap: 8px; padding: 14px; border-bottom: 1px solid var(--line); }
  .agenda-skeleton .skel:last-child { width: 65%; }

  @media (min-width: 901px) {
    .calendar-layout:not(.agenda-only) { position: relative; }
    .calendar-layout:not(.agenda-only) .agenda {
      position: absolute;
      inset: 0 0 0 auto;
      width: 340px;
    }
  }

  @media (max-width: 900px) {
    .calendar-filters { width: 100%; }
    .calendar-filters label { flex: 1; min-width: 0; }
    .calendar-filters .input { width: 100%; min-width: 0; }
    .calendar-toolbar, .calendar-layout { grid-template-columns: 1fr; }
    .calendar-ranges { justify-content: flex-end; }
    .agenda { grid-row: 1; max-height: none; }
    .agenda-list { max-height: none; }
    .month-grid { grid-row: 2; overflow-x: auto; }
    .weekday-row, .day-grid { min-width: 720px; }
  }

  @media (max-width: 560px) {
    .calendar-body { padding: 10px; }
    .calendar-ranges { width: 100%; }
    .calendar-ranges a { flex: 1; padding-inline: 10px; text-align: center; }
    .calendar-filters { flex-direction: column; }
    .calendar-nav { justify-content: space-between; }
    .range-title { min-width: 0; }
    .day { min-height: 96px; }
  }
</style>
