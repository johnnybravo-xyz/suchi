<script>
  import { listDocuments, listSavedViews } from '../lib/api.js'
  import { fmtDate, sensDot } from '../lib/format.js'
  import Icon from '../lib/Icon.svelte'

  let { st, inboxCategory, recent } = $props()
  const inboxCount = $derived(st?.inbox_count ?? 0)
  const pending = $derived(st?.pending_approvals ?? 0)
  const dead = $derived(st?.dead_jobs ?? 0)

  const total = $derived(st?.documents_total ?? null)
  let views = $state([])          // saved views + live counts

  async function load() {
    try {
      const res = await listSavedViews()
      const raw = res?.results || res || []
      views = raw.map(v => {
        let filters = {}
        try { filters = JSON.parse(v.filter_json || '{}') } catch {}
        return { ...v, filters, count: null }
      }).sort((a, b) => a.position - b.position)
      // live counts, one cheap page_size=1 call per view
      for (const v of views) {
        listDocuments({ ...v.filters, page_size: 1 })
          .then(r => { v.count = r?.count ?? 0; views = [...views] })
          .catch(() => {})
      }
    } catch {}
  }

  function viewHash(v) {
    const p = new URLSearchParams()
    for (const [k, val] of Object.entries(v.filters)) if (val !== '' && val != null) p.set(k, val)
    return `#/documents?${p.toString()}`
  }

  function filterSummary(f) {
    const parts = []
    if (f.q) parts.push(`“${f.q}”`)
    if (f.tags__id__in) parts.push('tag')
    if (f.correspondents__id__in) parts.push('correspondent')
    if (f.document_type__id) parts.push('type')
    if (f.jd_category_id) parts.push('jd')
    if (f.sensitivity) parts.push(f.sensitivity)
    return parts.join(' · ') || 'all documents'
  }

  load()
</script>

<div class="metrics">
  <a class="metric card" href="#/documents">
    <span class="m-label"><Icon name="docs" size={14} /> Total documents</span>
    <span class="m-value">{total ?? '—'}</span>
    <span class="m-sub">{st?.ingested_7d ? `${st.ingested_7d} added in the last 7 days` : 'across the whole archive'}</span>
  </a>
  <a class="metric card" href="#/inbox" class:attn={inboxCount > 0}>
    <span class="m-label"><Icon name="inbox" size={14} /> Inbox</span>
    <span class="m-value">{inboxCategory ? inboxCount : '—'}</span>
    <span class="m-sub">{inboxCount > 0 ? 'waiting to be filed' : 'everything is filed'}</span>
  </a>
  <a class="metric card" href="#/tasks" class:attn={pending > 0}>
    <span class="m-label"><Icon name="tasks" size={14} /> Approvals</span>
    <span class="m-value">{pending}</span>
    <span class="m-sub">{pending > 0 ? 'waiting on you' : 'none pending'}</span>
  </a>
  <a class="metric card" href="#/tasks" class:bad={dead > 0}>
    <span class="m-label"><Icon name="zap" size={14} /> Failed jobs</span>
    <span class="m-value">{dead}</span>
    <span class="m-sub">{dead > 0 ? 'dead-lettered — needs attention' : 'pipeline healthy'}</span>
  </a>
</div>

<div class="dash-grid">
  <div>
    <div class="dash-head">
      <h3>Recently added</h3>
      <a class="btn sm" href="#/documents">All documents</a>
    </div>
    {#if recent?.length}
      <div class="index">
        {#each recent as d (d.id)}
          <a class="irow" href={`#/doc/${d.id}`}>
            <span class="dot {sensDot(d.sensitivity)}" class:accent={!d.sensitivity}></span>
            {#if d.jd_category_code}<span class="chip" title={d.jd_category_name}>{d.jd_category_code}</span>{/if}
            <span class="title grow">{d.title || `Document #${d.id}`}</span>
            <span class="sub">{fmtDate(d.created_at)}</span>
          </a>
        {/each}
      </div>
    {:else}
      <div class="empty" style="padding:36px 20px">
        <Icon name="docs" size={44} />
        <b>Nothing here yet.</b>
        <span><a href="#/upload">Upload the first document</a>, point a watched folder at it, or forward an email.</span>
      </div>
    {/if}
  </div>

  <div>
    <div class="dash-head">
      <h3>Your views</h3>
      <a class="btn sm" href="#/settings">Manage</a>
    </div>
    {#if views.length}
      <div class="views">
        {#each views as v (v.id)}
          <a class="card view" href={viewHash(v)}>
            <span class="m-label">{v.name}</span>
            <span class="m-value" style="font-size:1.6rem">{v.count ?? '…'}</span>
            <span class="m-sub mono" style="font-size:.68rem">{filterSummary(v.filters)}</span>
          </a>
        {/each}
      </div>
    {:else}
      <div class="card" style="color:var(--muted);font-size:.86rem">
        No custom views yet. Create one in <a href="#/views">Views</a> — a saved filter
        (say, <i>“confidential in 22 Tax”</i>) shows up here with a live count.
      </div>
    {/if}
  </div>
</div>
