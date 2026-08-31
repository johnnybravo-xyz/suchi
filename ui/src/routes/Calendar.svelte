<script>
  import { listIntelligence, listSavedViews } from '../lib/api.js'
  import { DATE_ROLES, intelligenceRoleLabel, intelligenceDateValue, formatArchiveDate } from '../lib/intelligence.js'
  import Icon from '../lib/Icon.svelte'

  let { initialDocumentIDs = '' } = $props()
  let month = $state(new Date(new Date().getFullYear(), new Date().getMonth(), 1))
  let events = $state([])
  let views = $state([])
  let selectedViewID = $state('')
  let role = $state('')
  let loading = $state(true)
  let error = $state('')
  let loadVersion = 0

  const monthLabel = $derived(month.toLocaleDateString(undefined, { month: 'long', year: 'numeric' }))
  const calendarDays = $derived(buildCalendarDays(month))
  const eventMap = $derived(groupEvents(events))

  function isoDate(date) {
    const year = date.getFullYear()
    const monthNumber = String(date.getMonth() + 1).padStart(2, '0')
    const day = String(date.getDate()).padStart(2, '0')
    return `${year}-${monthNumber}-${day}`
  }

  function monthBounds(value) {
    return {
      from: isoDate(new Date(value.getFullYear(), value.getMonth(), 1)),
      to: isoDate(new Date(value.getFullYear(), value.getMonth() + 1, 0)),
    }
  }

  function buildCalendarDays(value) {
    const first = new Date(value.getFullYear(), value.getMonth(), 1)
    const start = new Date(value.getFullYear(), value.getMonth(), 1 - first.getDay())
    return Array.from({ length: 42 }, (_, index) => {
      const date = new Date(start.getFullYear(), start.getMonth(), start.getDate() + index)
      return { date, iso: isoDate(date), current: date.getMonth() === value.getMonth() }
    })
  }

  function groupEvents(items) {
    const grouped = new Map()
    for (const event of items) {
      const key = intelligenceDateValue(event)
      if (!grouped.has(key)) grouped.set(key, [])
      grouped.get(key).push(event)
    }
    return grouped
  }


  function viewParams() {
    if (selectedViewID) return { view_id: selectedViewID }
    return { document_ids: initialDocumentIDs }
  }

  function dateOriginLabel(event) {
    return event.reviewed_at ? 'Reviewed' : 'Automatic'
  }

  async function load() {
    const version = ++loadVersion
    loading = true
    error = ''
    try {
      const bounds = monthBounds(month)
      const response = await listIntelligence({
        status: 'accepted', type: 'date', role,
        sort_from: bounds.from, sort_to: bounds.to,
        page_size: 500, ...viewParams(),
      })
      if (version !== loadVersion) return
      events = response?.results || []
    } catch (ex) {
      if (version === loadVersion) error = ex.message || 'Could not load dates.'
    } finally {
      if (version === loadVersion) loading = false
    }
  }

  async function loadViews() {
    try {
      const response = await listSavedViews({ include: 'shared' })
      views = response?.results || []
      selectedViewID = ''
    } catch {}
  }

  function moveMonth(offset) {
    month = new Date(month.getFullYear(), month.getMonth() + offset, 1)
  }

  function useCurrentMonth() {
    const now = new Date()
    month = new Date(now.getFullYear(), now.getMonth(), 1)
  }

  loadViews()
  $effect(() => {
    // Reload when the visible Calendar filters change. A saved view takes
    // precedence over direct document IDs.
    `${month.getTime()}:${role}:${selectedViewID || `documents:${initialDocumentIDs}`}`
    load()
  })
</script>

<div class="calendar-page">
  <header class="calendar-intro">
    <div>
      <span class="eyebrow">Dates from your documents</span>
      <h2>Calendar</h2>
      <p>Dates added automatically or approved in Approvals appear here. Each date links to the document it came from.</p>
    </div>
    <div class="calendar-filters">
      <label>
        <span>Document view</span>
        <select class="input" bind:value={selectedViewID} autocomplete="off">
          <option value="">All accessible documents</option>
          {#each views as view (view.id)}<option value={view.id}>{view.name}</option>{/each}
        </select>
      </label>
      <label>
        <span>Date role</span>
        <select class="input" bind:value={role}>
          <option value="">Every role</option>
          {#each DATE_ROLES as value}<option value={value}>{intelligenceRoleLabel(value)}</option>{/each}
        </select>
      </label>
    </div>
  </header>

  <div class="calendar-nav">
    <button class="btn sm" onclick={() => moveMonth(-1)} aria-label="Previous month"><Icon name="left" size={13} /></button>
    <button class="month-title" onclick={useCurrentMonth}>{monthLabel}</button>
    <button class="btn sm" onclick={() => moveMonth(1)} aria-label="Next month"><Icon name="chev" size={13} /></button>
  </div>

  {#if error}
    <div class="err calendar-error"><span>{error}</span><button class="btn sm" onclick={load}>Retry</button></div>
  {/if}

  <div class="calendar-layout">
    <section class="month-grid" aria-label={monthLabel}>
      <div class="weekday-row" aria-hidden="true">
        {#each ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'] as day}<span>{day}</span>{/each}
      </div>
      <div class="day-grid" class:loading>
        {#each calendarDays as day (day.iso)}
          {@const dayEvents = eventMap.get(day.iso) || []}
          <div class="day" class:outside={!day.current} class:has-events={dayEvents.length > 0}>
            <span class="day-number">{day.date.getDate()}</span>
            <div class="day-events">
              {#each dayEvents.slice(0, 3) as event (event.id)}
                <a href={`#/doc/${event.document_id}`} title={`${intelligenceRoleLabel(event.role)} · ${event.document_title}`}>
                  <span class={`role-dot role-${event.role}`}></span>
                  <span>{event.document_title || `Document #${event.document_id}`}</span>
                  <small class="date-origin">{dateOriginLabel(event)}</small>
                </a>
              {/each}
              {#if dayEvents.length > 3}<small>+{dayEvents.length - 3} more</small>{/if}
            </div>
          </div>
        {/each}
      </div>
    </section>

    <aside class="agenda" aria-labelledby="agenda-title">
      <header>
        <span class="eyebrow">Agenda</span>
        <h3 id="agenda-title">{events.length} date{events.length === 1 ? '' : 's'}</h3>
      </header>
      {#if loading && !events.length}
        {#each Array(4) as _}<div class="agenda-skeleton"><div class="skel"></div><div class="skel"></div></div>{/each}
      {:else if !events.length}
        <div class="agenda-empty"><Icon name="calendar" size={34} /><b>No dates this month</b><span>Review uncertain dates in Approvals, or choose another document view.</span></div>
      {:else}
        <div class="agenda-list">
          {#each events as event (event.id)}
            <a class="agenda-event" href={`#/doc/${event.document_id}`}>
              <span class="agenda-date"><b>{formatArchiveDate(intelligenceDateValue(event), { day: 'numeric' })}</b><small>{formatArchiveDate(intelligenceDateValue(event), { month: 'short' })}</small></span>
              <span class="agenda-copy">
                <span><strong>{intelligenceRoleLabel(event.role)}</strong><small>{Math.round(Number(event.confidence || 0) * 100)}% · {event.value?.precision || 'day'}</small><em class="date-origin">{dateOriginLabel(event)}</em></span>
                <b>{event.document_title || `Document #${event.document_id}`}</b>
                <span>“{event.evidence_text}”</span>
                <small>{formatArchiveDate(intelligenceDateValue(event), { weekday: 'short', day: 'numeric', month: 'short', year: 'numeric' })}</small>
              </span>
              <Icon name="chev" size={13} />
            </a>
          {/each}
        </div>
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
  .day-number { display: grid; place-items: center; width: 24px; height: 24px; margin-left: auto; border-radius: 50%; font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .67rem; }
  .day-events { display: grid; gap: 4px; margin-top: 4px; }
  .day-events a { display: flex; align-items: center; gap: 5px; min-width: 0; padding: 3px 5px; border-radius: 5px; background: var(--tint); color: var(--ink); font-size: .63rem; text-decoration: none; }
  .day-events a span:last-child { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .day-events a .date-origin { margin-left: auto; }
  .day-events small { color: var(--faint); font-size: .59rem; }
  .date-origin { flex: none; padding: 1px 4px; border: 1px solid var(--line); border-radius: 999px; color: var(--muted); font-size: .55rem; font-style: normal; font-weight: 650; line-height: 1.4; }
  .role-dot { width: 5px; height: 5px; flex: none; border-radius: 50%; background: var(--accent); }
  .role-expiry, .role-due { background: var(--danger); }
  .role-issued, .role-start { background: var(--success); }
  .agenda { max-height: 690px; }
  .agenda > header { padding: 14px 15px 11px; border-bottom: 1px solid var(--line); background: var(--surface-2); }
  .agenda h3 { font-size: .87rem; }
  .agenda-list { max-height: 620px; overflow-y: auto; }
  .agenda-event { display: grid; grid-template-columns: 40px minmax(0, 1fr) auto; align-items: start; gap: 10px; padding: 13px; border-bottom: 1px solid var(--line); color: inherit; text-decoration: none; }
  .agenda-event:last-child { border-bottom: 0; }
  .agenda-event:hover { background: var(--tint); }
  .agenda-date { display: flex; align-items: center; flex-direction: column; padding: 5px 3px; border: 1px solid var(--line); border-radius: 8px; background: var(--bg); }
  .agenda-date b { font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .9rem; }
  .agenda-date small { color: var(--muted); font-size: .55rem; }
  .agenda-copy { display: flex; min-width: 0; flex-direction: column; gap: 4px; }
  .agenda-copy > span:first-child { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; }
  .agenda-copy > span:first-child strong { color: var(--accent); font-size: .65rem; }
  .agenda-copy > span:first-child small, .agenda-copy > small { color: var(--faint); font-size: .59rem; }
  .agenda-copy > b { overflow: hidden; font-size: .76rem; text-overflow: ellipsis; white-space: nowrap; }
  .agenda-copy > span:not(:first-child) { color: var(--muted); font-size: .68rem; line-height: 1.4; }
  .agenda-event > :global(.ico) { margin-top: 15px; color: var(--faint); }
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
