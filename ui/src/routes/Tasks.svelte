<script>
  import { listTasks, resolveApprovalTask, resolveBulkProposals } from '../lib/api.js'
  import { fmtDate } from '../lib/format.js'
  import Icon from '../lib/Icon.svelte'

  let { notify, onCount } = $props()
  let tasks = $state([])
  let jobs = $state([])
  let proposalCards = $state([])       // HeuristicsProposalCard[] from /api/tasks/
  let checked = $state(new Set())       // set of proposal ids selected across ANY card
  let loading = $state(true)
  let err = $state('')

  async function load() {
    loading = true; err = ''
    try {
      const [wf, jb, allTasks] = await Promise.all([
        listTasks({ include: 'workflow', state: 'pending', limit: 200 }),
        listTasks({ include: 'jobs', state: 'dead', limit: 50 }),
        listTasks({ limit: 200 }),   // default: proposals ride the "include=both" reply
      ])
      tasks = wf?.results || wf || []
      jobs = jb?.results || jb || []
      proposalCards = allTasks?.heuristics_proposals || []
      onCount?.(tasks.length + proposalCards.length)
    } catch (ex) { err = ex.message || 'Could not load tasks.' }
    finally { loading = false }
  }

  async function resolve(t, choice) {
    try {
      await resolveApprovalTask(t.id, { choice, decision: choice })
      tasks = tasks.filter(x => x.id !== t.id)
      onCount?.(tasks.length + proposalCards.length)
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

  // ---------- proposals: multi-select across cards ----------
  function toggle(proposalId) {
    const s = new Set(checked)
    if (s.has(proposalId)) s.delete(proposalId); else s.add(proposalId)
    checked = s
  }
  function toggleCardAll(card) {
    const s = new Set(checked)
    const cardIds = card.proposals.map(p => p.id)
    const allOn = cardIds.every(id => s.has(id))
    if (allOn) cardIds.forEach(id => s.delete(id))
    else cardIds.forEach(id => s.add(id))
    checked = s
  }
  function clearChecks() { checked = new Set() }

  const checkedCount = $derived(checked.size)
  const docsSpanned = $derived(new Set(proposalCards
    .filter(c => c.proposals.some(p => checked.has(p.id)))
    .map(c => c.doc_id)).size)

  async function bulkApply(action) {
    const ids = [...checked]
    if (!ids.length) return
    try {
      const res = await resolveBulkProposals(ids, action)
      const results = res?.results || []
      const ok = results.filter(r => r.ok).length
      const fail = results.filter(r => !r.ok).length
      notify?.(`${action === 'apply' ? 'Applied' : 'Rejected'} ${ok}${fail ? ` · ${fail} failed` : ''}`)
      clearChecks()
      await load()
    } catch (ex) { notify?.(ex.message || `Bulk ${action} failed`) }
  }

  async function resolveOne(proposal, action) {
    try {
      const res = await resolveBulkProposals([proposal.id], action)
      const okRow = (res?.results || [])[0]
      if (!okRow?.ok) throw new Error(okRow?.message || `${action} failed`)
      notify?.(action === 'apply' ? `Applied ${proposal.field}: ${proposal.label}` : 'Rejected')
      await load()
    } catch (ex) { notify?.(ex.message || `Could not ${action} the proposal`) }
  }

  function fieldLabel(field) {
    return { jd_category: 'JD', correspondent: 'From', document_type: 'Type', tag: 'Tag' }[field] || field
  }

  // Cmd/Ctrl+Enter resolves the top task with its first (primary) choice.
  function onKey(e) {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter' && tasks[0]) {
      e.preventDefault()
      const t = tasks[0]
      resolve(t, (t.choices?.length ? t.choices : ['approve'])[0])
    }
  }

  load()
</script>

<svelte:window onkeydown={onKey} />

{#if err}<div class="err">{err}</div>{/if}

{#if loading}
  <div class="index">{#each Array(3) as _}<div class="irow"><div class="skel" style="width:55%"></div></div>{/each}</div>
{:else}
  {#if proposalCards.length > 0}
    <h3 style="font-size:.9rem;color:var(--muted);margin:0 0 8px">Auto-file from archive</h3>
    <div style="display:flex;flex-direction:column;gap:12px;margin-bottom:22px">
      {#each proposalCards as card (card.doc_id)}
        {@const cardAll = card.proposals.every(p => checked.has(p.id))}
        <div class="card">
          <div style="display:flex;align-items:center;gap:10px;margin-bottom:8px">
            <input type="checkbox" checked={cardAll} onchange={() => toggleCardAll(card)}
                   aria-label="Select every proposal in this card" />
            <div style="flex:1;min-width:0">
              <b>{card.doc_title || `Document #${card.doc_id}`}</b>
              <span class="pill ok" style="margin-left:8px">Built-in</span>
            </div>
            <a class="btn sm" href={`#/doc/${card.doc_id}`}>Open →</a>
          </div>
          <p class="sub" style="margin:0 0 8px;color:var(--muted);font-size:.82rem">
            Because {card.proposals[0]?.based_on?.length || card.proposals[0]?.supporters?.length || '?'}
            similar docs share these traits:
          </p>
          <div style="display:flex;flex-direction:column;gap:6px">
            {#each card.proposals as p (p.id)}
              <div style="display:flex;align-items:center;gap:10px">
                <input type="checkbox" checked={checked.has(p.id)} onchange={() => toggle(p.id)}
                       aria-label={`Select ${fieldLabel(p.field)}: ${p.label}`} />
                <span class="chip mono" style="min-width:70px;justify-content:center">{fieldLabel(p.field)}</span>
                <span class="grow" style="min-width:0;overflow:hidden;text-overflow:ellipsis">{p.label}</span>
                <span class="sub" style="font-size:.78rem;color:var(--muted)">
                  {Math.round(p.confidence * 100)}% · {(p.supporters || []).length} supporters
                </span>
                <button class="btn sm primary" onclick={() => resolveOne(p, 'apply')}>Apply</button>
                <button class="btn sm" onclick={() => resolveOne(p, 'reject')}>Skip</button>
              </div>
            {/each}
          </div>
        </div>
      {/each}
    </div>
  {/if}

  {#if tasks.length === 0 && proposalCards.length === 0}
    <div class="empty"><Icon name="tasks" size={56} /><b>Nothing needs you.</b><span>The archive is running itself.</span></div>
  {:else if tasks.length > 0}
    <h3 style="font-size:.9rem;color:var(--muted);margin:14px 0 8px">Approvals</h3>
    <div style="display:flex;flex-direction:column;gap:12px;margin-bottom:22px">
      {#each tasks as t (t.id)}
        {@const dl = deadline(t)}
        <div class="card task-card">
          <div class="prompt">{t.prompt || t.title || `Task #${t.id}`}</div>
          {#if t.workflow_name === 'rescan-proposal' && t.vars}
            <div style="font-size:.85rem;color:var(--muted);margin-top:2px">
              Pipeline <b>{t.vars.kind}</b> · {t.vars.stale_count} document{t.vars.stale_count === 1 ? '' : 's'} stale (v{(t.vars.current_version ?? 1) - 1} → v{t.vars.current_version})
            </div>
          {/if}
          <div class="meta">
            {#if t.workflow_name}<span class="pill ok">{t.workflow_name}</span>{/if}
            {#if t.assignee}<span class="pill">{t.assignee}</span>{/if}
            <span>step <code>{t.state_key}</code></span>
            {#if t.doc_id}<a href={`#/doc/${t.doc_id}`}>document #{t.doc_id}</a>{/if}
            <span>opened {fmtDate(t.created_at)}</span>
            {#if dl}<span class:deadline-soon={dl.soon}>{dl.text}</span>{/if}
          </div>
          <div class="choices">
            {#each (t.choices?.length ? t.choices : ['approve', 'reject']) as c, i}
              <button class="btn sm" class:primary={i === 0} class:danger={/reject|deny|decline/i.test(c)}
                      onclick={() => resolve(t, c)}>
                {#if i === 0}<Icon name="check" size={13} />{/if}{c}
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
          <span class="sub">{j.attempts} attempts</span>
          {#if j.doc_id}<a class="btn sm" href={`#/doc/${j.doc_id}`}>Doc #{j.doc_id}</a>{/if}
        </div>
      {/each}
    </div>
  {/if}

  <!-- Bulk bar — fixed at the bottom when any proposal is checked. -->
  {#if checkedCount > 0}
    <div style="position:fixed;left:50%;bottom:22px;transform:translateX(-50%);z-index:80;
                display:flex;align-items:center;gap:12px;padding:10px 16px;border-radius:999px;
                background:var(--surface);border:1px solid var(--line);box-shadow:var(--shadow)">
      <b>{checkedCount}</b> proposals selected across <b>{docsSpanned}</b> {docsSpanned === 1 ? 'document' : 'documents'}
      <button class="btn primary sm" onclick={() => bulkApply('apply')}>Apply selected</button>
      <button class="btn sm" onclick={() => bulkApply('reject')}>Reject</button>
      <button class="btn sm" onclick={clearChecks} aria-label="Clear selection">Clear</button>
    </div>
  {/if}
{/if}
