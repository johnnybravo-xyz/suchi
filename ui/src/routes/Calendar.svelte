<script>
  import { onDestroy } from 'svelte'
  import { listIntelligence, listSavedViews } from '../lib/api.js'
  import { DATE_ROLES, intelligenceRoleLabel, intelligenceDateValue, formatArchiveDate, formatIntelligenceDate, groupCalendarEvents } from '../lib/intelligence.js'
  import { calendarDateFile, canExportCalendarDate } from '../lib/calendarExport.js'
  import { go } from '../lib/router.svelte.js'
  import Icon from '../lib/Icon.svelte'

  let { query = new URLSearchParams(), demo = false } = $props()
  let events = $state([])
  let views = $state([])
  let loading = $state(true)
  let error = $state('')
  let exportError = $state('')
  let eventTotal = $state(0)
  let pageNumber = $state(1)
  let loadVersion = 0
  let activeController // rapid month changes should release the older read

  const calendarPageSize = 500

  const documentIDs = $derived(query.get('document_ids') || '')
  // Fixed demo fixtures should remain discoverable after their calendar month passes.
  const allDates = $derived(Boolean(documentIDs) || (demo && !query.has('month') && !query.has('date')))
  const selectedDay = $derived(allDates ? '' : query.get('date') || '')
  const agendaOnly = $derived(allDates || Boolean(selectedDay))
  const selectedViewID = $derived(allDates ? '' : query.get('view_id') || '')
  const role = $derived(query.get('role') || '')
  const month = $derived.by(() => {
    const value = query.get('month') || selectedDay.slice(0, 7)
    const now = new Date()
    if (!/^\d{4}-(0[1-9]|1[0-2])$/.test(value)) return new Date(now.getFullYear(), now.getMonth(), 1)
    return new Date(`${value}-01T00:00:00`)
  })
  const documentCount = $derived(new Set(documentIDs.split(',').filter(Boolean)).size)
  const monthLabel = $derived(month.toLocaleDateString(undefined, { month: 'long', year: 'numeric' }))
  const calendarDays = $derived(buildCalendarDays(month))
  const eventMap = $derived(groupCalendarEvents(events))

  function isoDate(date) {
    const year = date.getFullYear()
    const monthNumber = String(date.getMonth() + 1).padStart(2, '0')
    const day = String(date.getDate()).padStart(2, '0')
    return `${year}-${monthNumber}-${day}`
  }

  function monthBounds(value) {
    return {
      sort_from: isoDate(new Date(value.getFullYear(), value.getMonth(), 1)),
      sort_to: isoDate(new Date(value.getFullYear(), value.getMonth() + 1, 0)),
    }
  }

  function calendarHref({ date = selectedDay, monthValue = month, viewID = selectedViewID, dateRole = role } = {}) {
    const params = new URLSearchParams()
    if (allDates) {
      if (documentIDs) params.set('document_ids', documentIDs)
    }
    else {
      params.set('month', isoDate(monthValue).slice(0, 7))
      if (date) params.set('date', date)
      if (viewID) params.set('view_id', viewID)
    }
    if (dateRole) params.set('role', dateRole)
    return `#/calendar?${params}`
  }

  function buildCalendarDays(value) {
    const first = new Date(value.getFullYear(), value.getMonth(), 1)
    const start = new Date(value.getFullYear(), value.getMonth(), 1 - first.getDay())
    return Array.from({ length: 42 }, (_, index) => {
      const date = new Date(start.getFullYear(), start.getMonth(), start.getDate() + index)
      return { date, iso: isoDate(date), current: date.getMonth() === value.getMonth() }
    })
  }

  function dateOriginLabel(event) {
    if (event.extractor === 'demo-corpus') return 'Demo example'
    return event.reviewed_at ? 'Reviewed' : 'Automatic'
  }

  function downloadDate(event) {
    exportError = ''
    try {
      const file = calendarDateFile(event, window.location.href)
      const link = document.createElement('a')
      const url = URL.createObjectURL(new Blob([file.contents], { type: 'text/calendar;charset=utf-8' }))
      link.href = url
      link.download = file.filename
      setTimeout(() => URL.revokeObjectURL(url), 10_000)
      link.click()
    } catch (ex) {
      exportError = ex.message || 'Could not download the calendar event.'
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
          ...(selectedDay ? { sort_from: selectedDay, sort_to: selectedDay, precision: 'day' } : monthBounds(month)),
        }),
      }, controller.signal)
      if (version !== loadVersion) return
      events = response?.results || []
      eventTotal = response?.count ?? events.length
      if (agendaOnly) pageNumber = Math.min(pageNumber, Math.max(1, Math.ceil(eventTotal / calendarPageSize)))
    } catch (ex) {
      if (version === loadVersion) error = ex.message || 'Could not load dates.'
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

  function moveMonth(offset) {
    go(calendarHref({ monthValue: new Date(month.getFullYear(), month.getMonth() + offset, 1) }))
  }

  function useCurrentMonth() {
    const now = new Date()
    go(calendarHref({ monthValue: new Date(now.getFullYear(), now.getMonth(), 1) }))
  }

  loadViews()
  onDestroy(() => {
    loadVersion++
    activeController?.abort()
  })
  $effect(() => {
    load()
  })
</script>

<div class="calendar-page">
  <header class="calendar-intro">
    <div>
      <span class="eyebrow">Dates from your documents</span>
      <h2>Calendar</h2>
      <p>{demo ? 'Read-only examples from the demo documents. Dates are curated from the sources, not generated by a live model.' : 'Dates added automatically or approved in Approvals appear here. Each date links to the document it came from.'}</p>
    </div>
    <div class="calendar-filters">
      {#if !allDates}
      <label>
        <span>Document view</span>
        <select class="input" onchange={event => go(calendarHref({ viewID: event.currentTarget.value }))} autocomplete="off">
          <option value="" selected={!selectedViewID}>All accessible documents</option>
          {#each views as view (view.id)}<option value={view.id} selected={String(view.id) === selectedViewID}>{view.name}</option>{/each}
        </select>
      </label>
      {/if}
      <label>
        <span>Date role</span>
        <select class="input" value={role} onchange={event => go(calendarHref({ dateRole: event.currentTarget.value }))}>
          <option value="">Every role</option>
          {#each DATE_ROLES as value}<option value={value}>{intelligenceRoleLabel(value)}</option>{/each}
        </select>
      </label>
    </div>
  </header>

  {#if allDates}
    <div class="calendar-scope">
      <p>{documentIDs ? `Dates from ${documentCount} selected document${documentCount === 1 ? '' : 's'}` : 'Demo document dates'} · All months and years</p>
      <a class="btn sm" href={demo ? `#/calendar?month=${isoDate(month).slice(0, 7)}` : '#/calendar'}>Open full calendar</a>
    </div>
  {:else if selectedDay}
    <div class="calendar-scope">
      <p>{formatArchiveDate(selectedDay, { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric' })}</p>
      <a class="btn sm" href={calendarHref({ date: '' })}>Back to {monthLabel}</a>
    </div>
  {:else}
  <div class="calendar-nav">
    <button class="btn sm" onclick={() => moveMonth(-1)} aria-label="Previous month"><Icon name="left" size={13} /></button>
    <button class="month-title" onclick={useCurrentMonth}>{monthLabel}</button>
    <button class="btn sm" onclick={() => moveMonth(1)} aria-label="Next month"><Icon name="chev" size={13} /></button>
  </div>
  {/if}

  {#if error}
    <div class="err calendar-error"><span>{error}</span><button class="btn sm" onclick={load}>Retry</button></div>
  {/if}
  {#if exportError}<p class="err" role="alert">{exportError}</p>{/if}

  <div class="calendar-layout" class:agenda-only={agendaOnly}>
    {#if !agendaOnly}
    <section class="month-grid" aria-label={monthLabel}>
      <div class="weekday-row" aria-hidden="true">
        {#each ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'] as day}<span>{day}</span>{/each}
      </div>
      <div class="day-grid" class:loading>
        {#each calendarDays as day (day.iso)}
          {@const dayEvents = eventMap.get(day.iso) || []}
          <div class="day" class:outside={!day.current} class:has-events={dayEvents.length > 0}>
            <a class="day-number" href={calendarHref({ date: day.iso })} aria-label={`Open agenda for ${formatArchiveDate(day.iso)}`}>{day.date.getDate()}</a>
            <div class="day-events">
              {#each dayEvents.slice(0, 3) as event (event.id)}
                <a href={`#/doc/${event.document_id}`} title={`${intelligenceRoleLabel(event.role)} · ${event.document_title}`}>
                  <span class={`role-dot role-${event.role}`}></span>
                  <span class="day-event-title">{event.document_title || `Document #${event.document_id}`}</span>
                  <small class="date-origin">{dateOriginLabel(event)}</small>
                </a>
              {/each}
              {#if dayEvents.length > 3}<a class="day-more" href={calendarHref({ date: day.iso })}>+{dayEvents.length - 3} more</a>{/if}
            </div>
          </div>
        {/each}
      </div>
    </section>
    {/if}

    <aside class="agenda" aria-labelledby="agenda-title">
      <header>
        <span class="eyebrow">{allDates ? 'All dates' : 'Agenda'}</span>
        <h3 id="agenda-title">
          {#if loading}Loading dates…
          {:else if error}Dates unavailable
          {:else}{events.length}{eventTotal > events.length ? ` of ${eventTotal}` : ''} date{eventTotal === 1 ? '' : 's'}{/if}
        </h3>
        {#if !agendaOnly && !loading && eventTotal > events.length}<small class="agenda-limit">Showing the first {calendarPageSize}. Narrow the view or date role to see the rest.</small>{/if}
      </header>
      {#if loading && !events.length}
        {#each Array(4) as _}<div class="agenda-skeleton"><div class="skel"></div><div class="skel"></div></div>{/each}
      {:else if error}
        <div class="agenda-empty"><Icon name="calendar" size={34} /><b>Dates unavailable</b><span>Retry to load dates for this scope.</span></div>
      {:else if !events.length}
        <div class="agenda-empty"><Icon name="calendar" size={34} /><b>{allDates ? 'No matching dates' : selectedDay ? 'No dates this day' : 'No dates this month'}</b><span>{allDates ? 'Check the date role, or review uncertain dates in Approvals.' : 'Review uncertain dates in Approvals, or choose another document view.'}</span></div>
      {:else}
        <div class="agenda-list">
          {#each events as event (event.id)}
            {@const date = intelligenceDateValue(event)}
            {@const precision = event.value?.precision || 'day'}
            <article class="agenda-event">
              <span class="agenda-date">
                {#if precision === 'year'}
                  <b>{formatArchiveDate(date, { year: 'numeric' })}</b>
                {:else if precision === 'month'}
                  <b>{formatArchiveDate(date, { month: 'short' })}</b>
                  <small>{formatArchiveDate(date, { year: 'numeric' })}</small>
                {:else}
                  <b>{formatArchiveDate(date, { day: 'numeric' })}</b>
                  <small>{formatArchiveDate(date, { month: 'short' })}</small>
                  {#if agendaOnly}<small>{formatArchiveDate(date, { year: 'numeric' })}</small>{/if}
                {/if}
              </span>
              <div class="agenda-copy">
                <div class="agenda-meta">
                  <strong>{intelligenceRoleLabel(event.role)}</strong>
                  {#if event.extractor === 'demo-corpus'}
                    <span class="date-origin">Demo example</span>
                  {:else}
                  <details class="date-details">
                    <summary class="date-origin">{dateOriginLabel(event)}</summary>
                    <p>LLM Classifier confidence: {Math.round(Number(event.confidence || 0) * 100)}%</p>
                  </details>
                  {/if}
                </div>
                <a class="agenda-document" href={`#/doc/${event.document_id}`}><b>{event.document_title || `Document #${event.document_id}`}</b><Icon name="chev" size={13} /></a>
                <span>“{event.evidence_text}”</span>
                <small>{formatIntelligenceDate(event, { weekday: 'short', day: 'numeric', month: 'short', year: 'numeric' })}</small>
                {#if canExportCalendarDate(event)}
                  <details class="calendar-export">
                    <summary>Add to calendar</summary>
                    <p>The .ics file includes the document title, date, and a private document link. Importing it into a synced calendar shares those details with your calendar provider. The document still requires Suchi access.</p>
                    <button class="btn sm" onclick={() => downloadDate(event)}><Icon name="download" size={13} />Download .ics</button>
                  </details>
                {/if}
              </div>
            </article>
          {/each}
        </div>
      {/if}
      {#if agendaOnly && !loading && !error && eventTotal > calendarPageSize}
        <nav class="agenda-pages" aria-label="Date pages">
          <button class="btn sm" disabled={pageNumber === 1} onclick={() => { pageNumber-- }}>Previous</button>
          <span>Page {pageNumber} of {Math.ceil(eventTotal / calendarPageSize)}</span>
          <button class="btn sm" disabled={pageNumber * calendarPageSize >= eventTotal} onclick={() => { pageNumber++ }}>Next</button>
        </nav>
      {/if}
    </aside>
  </div>
</div>

<style>
  .calendar-page { max-width: 1240px; margin: 0 auto; }
  .calendar-intro { display: flex; align-items: flex-end; justify-content: space-between; gap: 28px; margin: 8px 0 20px; }
  .calendar-intro > div:first-child { max-width: 630px; }
  .eyebrow { display: block; margin-bottom: 6px; color: var(--accent); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .63rem; font-weight: 700; }
  .calendar-intro h2 { font-size: 1.62rem; }
  .calendar-intro p { margin: 7px 0 0; color: var(--muted); font-size: .86rem; line-height: 1.5; }
  .calendar-filters { display: flex; gap: 9px; }
  .calendar-filters label { display: flex; flex-direction: column; gap: 4px; }
  .calendar-filters label > span { color: var(--faint); font-size: .63rem; }
  .calendar-filters .input { min-width: 165px; font-size: .74rem; }
  .calendar-nav { display: flex; align-items: center; justify-content: center; gap: 7px; margin-bottom: 10px; }
  .month-title { min-width: 190px; border: 0; background: transparent; color: var(--ink); font-family: inherit; font-size: 1rem; font-weight: 700; cursor: pointer; }
  .calendar-error { display: flex; justify-content: space-between; margin-bottom: 10px; }
  .calendar-layout { display: grid; grid-template-columns: minmax(0, 1fr) 320px; gap: 14px; align-items: start; }
  .calendar-scope { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 12px; margin-bottom: 14px; }
  .calendar-scope p { margin: 0; color: var(--muted); font-size: .8rem; }
  .calendar-layout.agenda-only { display: block; }
  .agenda-only .agenda, .agenda-only .agenda-list { max-height: none; }
  .agenda-only .agenda-event { grid-template-columns: 48px minmax(0, 1fr); }
  .agenda-pages { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 12px 15px; border-top: 1px solid var(--line); font-size: .75rem; }
  .month-grid, .agenda { border: 1px solid var(--line); border-radius: var(--r); background: var(--surface); overflow: hidden; }
  .weekday-row { display: grid; grid-template-columns: repeat(7, 1fr); border-bottom: 1px solid var(--line-strong); background: var(--surface-2); }
  .weekday-row span { padding: 8px; color: var(--faint); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .61rem; font-weight: 700; text-align: center; }
  .day-grid { display: grid; grid-template-columns: repeat(7, minmax(0, 1fr)); opacity: 1; transition: opacity .16s ease; }
  .day-grid.loading { opacity: .58; }
  .day { position: relative; min-height: 112px; padding: 8px; border-right: 1px solid var(--line); border-bottom: 1px solid var(--line); background: var(--surface); }
  .day:nth-child(7n) { border-right: 0; }
  .day:nth-last-child(-n + 7) { border-bottom: 0; }
  .day.outside { background: var(--bg); color: var(--faint); }
  .day.has-events { background: color-mix(in srgb, var(--accent) 3%, var(--surface)); }
  .day-number { display: grid; place-items: center; width: 24px; height: 24px; margin-left: auto; border-radius: 50%; color: inherit; font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .67rem; text-decoration: none; }
  .day-number::after { position: absolute; inset: 0; content: ''; }
  .day-number:hover::after { background: color-mix(in srgb, var(--accent) 5%, transparent); }
  .day-number:focus-visible { outline: none; }
  .day-number:focus-visible::after { outline: 2px solid var(--accent); outline-offset: -2px; }
  .day-events { display: grid; gap: 4px; margin-top: 4px; }
  .day-events a { position: relative; display: grid; grid-template-columns: 5px minmax(0, 1fr); align-items: center; gap: 2px 5px; min-width: 0; padding: 4px 5px; border-radius: 5px; background: var(--tint); color: var(--ink); font-size: .63rem; text-decoration: none; }
  .day-events .day-more { display: block; background: transparent; color: var(--muted); }
  .day-event-title { grid-column: 2; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .day-events a .role-dot { grid-row: 1 / span 2; }
  .day-events a .date-origin { grid-column: 2; justify-self: start; margin-left: 0; }
  .day-events small { color: var(--faint); font-size: .59rem; }
  .date-origin { flex: none; padding: 1px 4px; border: 1px solid var(--line); border-radius: 999px; color: var(--muted); font-size: .55rem; font-style: normal; font-weight: 650; line-height: 1.4; }
  .role-dot { width: 5px; height: 5px; flex: none; border-radius: 50%; background: var(--accent); }
  .role-expiry, .role-due { background: var(--danger); }
  .role-issued, .role-start { background: var(--success); }
  .agenda { max-height: 690px; }
  .agenda > header { padding: 14px 15px 11px; border-bottom: 1px solid var(--line); background: var(--surface-2); }
  .agenda h3 { font-size: .87rem; }
  .agenda-limit { display: block; margin-top: 4px; color: var(--muted); font-size: .62rem; line-height: 1.35; }
  .agenda-list { max-height: 620px; overflow-y: auto; }
  .agenda-event { display: grid; grid-template-columns: 40px minmax(0, 1fr); align-items: start; gap: 10px; padding: 13px; border-bottom: 1px solid var(--line); }
  .agenda-event:last-child { border-bottom: 0; }
  .agenda-date { display: flex; align-items: center; flex-direction: column; padding: 5px 3px; border: 1px solid var(--line); border-radius: 8px; background: var(--bg); }
  .agenda-date b { font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .9rem; }
  .agenda-date small { color: var(--muted); font-size: .55rem; }
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
  .agenda-copy > span { color: var(--muted); font-size: .68rem; line-height: 1.4; overflow-wrap: anywhere; }
  .agenda-empty { display: flex; align-items: center; flex-direction: column; padding: 58px 20px; color: var(--muted); text-align: center; }
  .agenda-empty :global(.ico) { margin-bottom: 12px; color: var(--accent); }
  .agenda-empty b { color: var(--ink); font-size: .82rem; }
  .agenda-empty span { max-width: 230px; margin-top: 6px; font-size: .7rem; line-height: 1.45; }
  .agenda-skeleton { display: grid; gap: 8px; padding: 14px; border-bottom: 1px solid var(--line); }
  .agenda-skeleton .skel:last-child { width: 65%; }

  @media (max-width: 900px) {
    .calendar-intro { align-items: flex-start; flex-direction: column; }
    .calendar-filters { width: 100%; }
    .calendar-filters label { flex: 1; }
    .calendar-filters .input { width: 100%; min-width: 0; }
    .calendar-layout { grid-template-columns: 1fr; }
    .agenda { grid-row: 1; max-height: none; }
    .agenda-list { max-height: none; }
    .month-grid { grid-row: 2; overflow-x: auto; }
    .weekday-row, .day-grid { min-width: 720px; }
  }

  @media (max-width: 560px) {
    .calendar-filters { flex-direction: column; }
    .day { min-height: 96px; }
  }
</style>
