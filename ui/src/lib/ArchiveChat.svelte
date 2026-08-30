<script>
  import { onDestroy } from 'svelte'
  import { askArchive } from './api.js'
  import Icon from './Icon.svelte'
  import ArchiveChatSource from './ArchiveChatSource.svelte'

  let {
    open = false,
    request = { id: 0, question: '', scope: { label: 'All archive' } },
    status = { provider: '', local: false },
    canReviewIntelligence = false,
    onClose,
    onPark,
    onReturnFocus,
  } = $props()
  let turns = $state([])
  let draft = $state('')
  let includeSensitive = $state(false)
  let sending = $state(false)
  let activeRequest
  let composer = $state(), drawer = $state()
  let previousFocus
  let wasOpen = false
  let handledRequest = 0
  let sessionKey = ''

  const prompts = [
    'Find the next renewal dates',
    'Summarize documents about a topic',
    'Compare what two documents say',
  ]

  function completedTurns() {
    return turns.filter(turn => turn.answer && !turn.error)
  }

  function history() {
    const encoder = new TextEncoder()
    const pairs = []
    let bytes = 0
    for (const turn of completedTurns().slice().reverse()) {
      const pairBytes = encoder.encode(turn.question).byteLength + encoder.encode(turn.answer).byteLength
      if (pairs.length === 2 || bytes + pairBytes > 12 * 1024) break
      pairs.unshift([
        { role: 'user', content: turn.question },
        { role: 'assistant', content: turn.answer },
      ])
      bytes += pairBytes
    }
    return pairs.flat()
  }

  function contextSourceIDs() {
    const prior = completedTurns().at(-1)
    if (!prior) return []
    const cited = new Set(prior.citations || [])
    const sources = prior.sources.filter((_, index) => cited.has(index + 1))
    return sources.slice(0, 3).map(source => source.id)
  }

  function requestScope() {
    const scope = request?.scope || {}
    return {
      query: scope.query || '',
      document_ids: scope.document_ids || [],
      jd_category_id: scope.jd_category_id || 0,
      sensitivity: scope.sensitivity || '',
      document_type_id: scope.document_type_id || 0,
      tag_ids: scope.tag_ids || [],
      correspondent_ids: scope.correspondent_ids || [],
      created_at_gte: scope.created_at_gte ?? null,
      created_at_lte: scope.created_at_lte ?? null,
      language: scope.language || '',
    }
  }

  function updateTurn(id, patch) {
    turns = turns.map(turn => turn.id === id ? { ...turn, ...patch } : turn)
  }

  function revealTurn(id) {
    requestAnimationFrame(() => {
      document.getElementById(`research-turn-${id}`)?.scrollIntoView({ block: 'start', behavior: 'smooth' })
    })
  }

  async function send(question = draft) {
    question = question.trim()
    if (!question || question.length > 2000) return
    if (sending) { draft = question; return }
    const prior = history()
    const contextIDs = contextSourceIDs()
    const turn = {
      id: Date.now(), question, answer: '', sources: [], citations: [], grounded: false,
      error: '',
    }
    turns = [...turns, turn]
    draft = ''
    sending = true
    const requestState = { controller: new AbortController(), turnID: turn.id, restoreDraft: false, invalidated: false }
    activeRequest = requestState
    revealTurn(turn.id)
    try {
      const result = await askArchive({
        question,
        history: prior,
        context_source_ids: contextIDs,
        scope: requestScope(),
        include_sensitive: includeSensitive,
      }, requestState.controller.signal)
      if (activeRequest !== requestState || requestState.invalidated) return
      updateTurn(turn.id, {
        answer: result.answer || '',
        sources: result.sources || [],
        citations: result.citations || [],
        grounded: !!result.grounded,
        intelligence: result.intelligence || {},
      })
    } catch (ex) {
      if (activeRequest !== requestState || requestState.invalidated) return
      const canceled = ex?.name === 'AbortError'
      const error = canceled
        ? 'Request canceled.'
        : ex?.code === 'invalid_provider_response'
          ? 'The model returned an answer without valid citations. Try again.'
          : (ex?.message || 'The archive question could not be answered.')
      updateTurn(turn.id, { error })
      if (canceled && requestState.restoreDraft && !draft) draft = question
    } finally {
      if (activeRequest === requestState) {
        sending = false
        activeRequest = null
        queueMicrotask(() => composer?.focus())
      }
    }
  }

  function cancel() {
    if (!activeRequest) return
    activeRequest.restoreDraft = true
    activeRequest.controller.abort()
  }

  function invalidateRequest() {
    if (!activeRequest) return
    activeRequest.invalidated = true
    activeRequest.controller.abort()
    activeRequest = null
    sending = false
  }

  function clearConversation() {
    invalidateRequest()
    turns = []
    draft = ''
    includeSensitive = false
    queueMicrotask(() => composer?.focus())
  }

  function close() {
    onClose?.()
    queueMicrotask(() => {
      if (onReturnFocus) onReturnFocus()
      else previousFocus?.focus?.()
    })
  }

  function closeForNavigation() {
    onPark?.()
  }

  function sourceItems(turn) {
    return turn.sources.map((source, index) => ({ source, number: index + 1 }))
  }

  function sourceGroups(turn) {
    const cited = new Set(turn.citations || [])
    const items = sourceItems(turn)
    const primary = cited.size ? items.filter(item => cited.has(item.number)) : items
    const primaryNumbers = new Set(primary.map(item => item.number))
    return { primary, extra: items.filter(item => !primaryNumbers.has(item.number)) }
  }

  function answerParts(answer) {
    return String(answer || '').split(/(\[\d+\])/g).filter(Boolean).map(part => {
      const match = part.match(/^\[(\d+)\]$/)
      return match ? { text: part, citation: Number(match[1]) } : { text: part, citation: 0 }
    })
  }


  function providerLabel() {
    if (status?.local) return status.provider ? `Local model · ${status.provider}` : 'Local model'
    return status?.provider || 'Configured hosted model'
  }

  function acceptedDateCount(turn) {
    return Number(turn.intelligence?.accepted?.date || 0)
  }

  function pendingDateCount(turn) {
    return Number(turn.intelligence?.pending?.date || 0)
  }

  function sourceIDQuery(turn) {
    return turn.sources.map(source => source.id).join(',')
  }

  function conversationKey() {
    return JSON.stringify([requestScope(), status?.provider || '', !!status?.local])
  }

  function resetSession() {
    invalidateRequest()
    turns = []
    draft = ''
    includeSensitive = false
  }

  function setSensitive(next) {
    if (!next && includeSensitive) {
      invalidateRequest()
      turns = []
    }
    includeSensitive = next
  }

  function onKey(e) {
    if (e.key === 'Escape') { e.preventDefault(); close(); return }
    if (e.key !== 'Tab' || !drawer) return
    const focusable = [...drawer.querySelectorAll('button:not([disabled]), a[href], textarea:not([disabled]), input:not([disabled]), summary')]
    if (!focusable.length) return
    const first = focusable[0], last = focusable[focusable.length - 1]
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus() }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus() }
  }

  $effect(() => {
    if (open && !wasOpen) {
      previousFocus = document.activeElement
      queueMicrotask(() => composer?.focus())
    }
    wasOpen = open
  })

  $effect(() => {
    if (!open || !request?.id || request.id === handledRequest) return
    handledRequest = request.id
    const nextSessionKey = conversationKey()
    if (sessionKey && sessionKey !== nextSessionKey) resetSession()
    sessionKey = nextSessionKey
    if (request.question?.trim()) {
      if (sending) draft = request.question.trim()
      else send(request.question)
    }
    else queueMicrotask(() => composer?.focus())
  })

  $effect(() => {
    if (!sessionKey) return
    const nextSessionKey = conversationKey()
    if (sessionKey !== nextSessionKey) {
      resetSession()
      sessionKey = nextSessionKey
    }
  })

  onDestroy(invalidateRequest)
</script>

{#if open}
  <button class="research-veil" aria-label="Close archive research" onclick={close}></button>
  <div class="research-drawer" bind:this={drawer} role="dialog" aria-modal="true" aria-labelledby="research-title" tabindex="-1" onkeydown={onKey}>
    <header class="research-head">
      <div>
        <h2 id="research-title">Archive research</h2>
        <p class="research-intro">Ask questions about your documents.</p>
        <span class="scope-chip"><Icon name="search" size={11} />{request?.scope?.label || 'All archive'}</span>
      </div>
      <div class="research-head-actions">
        <button class="btn sm" onclick={clearConversation} disabled={!turns.length && !includeSensitive}>Clear</button>
        <button class="btn sm icon-only" onclick={close} aria-label="Close archive research" title="Close"><Icon name="x" size={14} /></button>
      </div>
    </header>

    <div class="research-transcript" aria-live="polite">
      {#if turns.length === 0}
        <div class="research-empty">
          <span class="empty-index">R / 01</span>
          <span class="research-empty-mark"><Icon name="ask" size={24} /></span>
          <b>Start with a question, not a search query</b>
          <p>Suchi retrieves only documents in this scope, then keeps the evidence attached to the answer.</p>
          <div class="prompt-list">
            {#each prompts as prompt}
              <button onclick={() => { draft = prompt; queueMicrotask(() => composer?.focus()) }}>{prompt}<Icon name="chev" size={11} /></button>
            {/each}
          </div>
        </div>
      {/if}

      {#each turns as turn (turn.id)}
        <article class="research-turn" id={`research-turn-${turn.id}`}>
          <header class="turn-question"><span>Question</span><p>{turn.question}</p></header>

          {#if sending && turn === turns.at(-1) && !turn.answer && !turn.error}
            <div class="research-thinking"><span></span><span></span><span></span><em>Retrieving authorized evidence</em></div>
          {/if}

          {#if turn.answer}
            <div class="answer-block" class:weak={!turn.grounded}>
              <p class="research-answer">
                {#each answerParts(turn.answer) as part}
                  {#if part.citation}
                    {@const citedSource = turn.sources[part.citation - 1]}
                    <a class="inline-citation" href={`#/doc/${citedSource.id}`} onclick={closeForNavigation}
                       aria-label={`Open cited document ${part.citation}: ${citedSource.title || `Document #${citedSource.id}`}`}>{part.text}</a>
                  {:else}{part.text}{/if}
                {/each}
              </p>
            </div>
          {/if}

          {#if turn.error}
            <div class="research-error" role="alert">
              <span>{turn.error}</span>
              <button class="btn sm" disabled={sending} onclick={() => send(turn.question)}>Retry</button>
            </div>
          {/if}

          {#if turn.sources.length}
            {@const groups = sourceGroups(turn)}
            <div class="research-actions" aria-label="Research actions">
              <a class="research-action" href={`#/views?new=1&ids=${turn.sources.map(source => source.id).join(',')}`} onclick={closeForNavigation}>
                <Icon name="eye" size={14} /><span><b>Save retrieved documents as a view</b><small>{turn.sources.length} document{turn.sources.length === 1 ? '' : 's'}</small></span>
              </a>
              {#if canReviewIntelligence}
                {#if pendingDateCount(turn) > 0}
                  <a class="research-action" href="#/tasks" onclick={closeForNavigation}>
                    <Icon name="tasks" size={14} /><span><b>Review {pendingDateCount(turn)} date{pendingDateCount(turn) === 1 ? '' : 's'}</b><small>Validate candidates in Approvals</small></span>
                  </a>
                {:else if acceptedDateCount(turn) > 0}
                  <a class="research-action" href={`#/calendar?document_ids=${sourceIDQuery(turn)}`} onclick={closeForNavigation}>
                    <Icon name="calendar" size={14} /><span><b>Open {acceptedDateCount(turn)} accepted date{acceptedDateCount(turn) === 1 ? '' : 's'}</b><small>Calendar uses validated facts only</small></span>
                  </a>
                {/if}
              {/if}
            </div>

            <section class="evidence-stack" aria-label="Evidence sources">
              <header><span>Evidence</span><small>{turn.citations.length || 0} cited / {turn.sources.length} retrieved</small></header>
              {#each groups.primary as item (item.source.id)}
                <ArchiveChatSource {item} turnID={turn.id} cited onOpen={closeForNavigation} />
              {/each}
              {#if groups.extra.length}
                <details class="extra-sources">
                  <summary>{groups.extra.length} additional retrieved source{groups.extra.length === 1 ? '' : 's'}</summary>
                  {#each groups.extra as item (item.source.id)}
                    <ArchiveChatSource {item} turnID={turn.id} onOpen={closeForNavigation} />
                  {/each}
                </details>
              {/if}
            </section>
          {/if}
        </article>
      {/each}
    </div>

    <footer class="research-compose">
      <label class="sensitive-toggle">
        <input type="checkbox" checked={includeSensitive} disabled={sending} onchange={(event) => setSensitive(event.currentTarget.checked)} />
        <span><b>Include Confidential and Restricted</b><small>Text from the listed documents is sent to {providerLabel()} for this conversation</small></span>
      </label>
      <form onsubmit={(event) => { event.preventDefault(); send() }}>
        <textarea bind:this={composer} bind:value={draft} maxlength="2000" rows="2" placeholder="Ask a question about this scope"
                  aria-label="Question" disabled={sending}
                  onkeydown={(event) => { if (event.key === 'Enter' && !event.shiftKey) { event.preventDefault(); send() } }}></textarea>
        {#if sending}
          <button type="button" class="btn research-send" onclick={cancel}>Cancel</button>
        {:else}
          <button class="btn primary research-send" disabled={!draft.trim()}><Icon name="ask" size={14} /> Ask</button>
        {/if}
      </form>
      <small class="research-note">Accepted intelligence can support answers; original documents remain the evidence.</small>
    </footer>
  </div>
{/if}

<style>
  .research-veil { position: fixed; inset: 0; z-index: 90; border: 0; background: color-mix(in srgb, var(--ink) 28%, transparent); backdrop-filter: blur(1px); }
  .research-drawer { position: fixed; z-index: 91; inset: 0 0 0 auto; width: min(520px, 100vw); display: grid; grid-template-rows: auto minmax(0, 1fr) auto; border-left: 1px solid var(--line-strong); background: var(--bg); box-shadow: -22px 0 54px color-mix(in srgb, var(--ink) 17%, transparent); animation: research-in .2s ease-out; }
  .research-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; padding: 18px 19px 15px; border-bottom: 1px solid var(--line-strong); background: linear-gradient(145deg, var(--surface), var(--bg)); }
  .research-intro { margin: 3px 0 0; color: var(--muted); font-size: .72rem; line-height: 1.4; }
  .research-head h2 { font-size: 1.13rem; }
  .scope-chip { display: inline-flex; align-items: center; gap: 5px; margin-top: 7px; padding: 3px 7px; border: 1px solid var(--line); border-radius: 999px; color: var(--muted); font-size: .63rem; }
  .research-head-actions { display: flex; gap: 7px; }.icon-only { padding-inline: 9px; }
  .research-transcript { overflow-y: auto; padding: 21px 18px 34px; scroll-padding-top: 16px; overscroll-behavior: contain; }
  .research-empty { position: relative; min-height: 72%; display: flex; align-items: center; justify-content: center; flex-direction: column; text-align: center; color: var(--muted); }
  .empty-index { position: absolute; top: 0; left: 0; color: var(--faint); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .6rem; }
  .research-empty-mark { display: grid; place-items: center; width: 52px; height: 52px; margin-bottom: 14px; border: 1px solid var(--line-strong); border-radius: 50%; background: var(--surface); color: var(--accent); }
  .research-empty b { color: var(--ink); font-size: .95rem; }
  .research-empty p { max-width: 340px; margin: 7px 0 18px; font-size: .8rem; line-height: 1.55; }
  .prompt-list { display: grid; width: min(350px, 100%); gap: 6px; }
  .prompt-list button { display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 9px 11px; border: 1px solid var(--line); border-radius: 8px; background: var(--surface); color: var(--muted); font: inherit; font-size: .72rem; text-align: left; cursor: pointer; }
  .prompt-list button:hover { border-color: var(--line-strong); color: var(--ink); transform: translateX(2px); }
  .research-turn + .research-turn { margin-top: 30px; padding-top: 27px; border-top: 1px solid var(--line-strong); }
  .turn-question { display: flex; align-items: flex-start; justify-content: flex-end; gap: 9px; }
  .turn-question > span { margin-top: 8px; color: var(--faint); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .56rem; text-transform: uppercase; }
  .turn-question p { width: fit-content; max-width: 86%; margin: 0; padding: 9px 12px; border-radius: 12px 12px 3px 12px; background: var(--tint); color: var(--ink); font-size: .82rem; line-height: 1.45; white-space: pre-wrap; }
  .answer-block { margin-top: 15px; padding-left: 12px; border-left: 2px solid var(--accent); }.answer-block.weak { border-color: var(--faint); }
  .research-answer { margin: 0; color: var(--ink); font-size: .87rem; line-height: 1.68; white-space: pre-wrap; }
  .inline-citation { display: inline; margin: 0 1px; padding: 0 2px; border: 0; border-radius: 3px; background: var(--tint); color: var(--accent); font: inherit; font-size: .76rem; font-weight: 750; text-decoration: none; cursor: pointer; vertical-align: baseline; }
  .inline-citation:hover { text-decoration: underline; }
  .research-error { display: flex; align-items: center; justify-content: space-between; gap: 10px; margin-top: 13px; padding: 10px 12px; border-left: 2px solid var(--danger); background: var(--danger-soft); color: var(--muted); font-size: .78rem; }
  .research-thinking { display: flex; align-items: center; gap: 4px; margin-top: 16px; color: var(--muted); }.research-thinking span { width: 5px; height: 5px; border-radius: 50%; background: var(--accent); animation: pulse 1s infinite alternate; }.research-thinking span:nth-child(2) { animation-delay: .16s; }.research-thinking span:nth-child(3) { animation-delay: .32s; }.research-thinking em { margin-left: 5px; font-size: .73rem; font-style: normal; }
  .research-actions { display: grid; grid-template-columns: 1fr 1fr; gap: 7px; margin-top: 17px; }
  .research-action { display: flex; align-items: center; gap: 9px; min-width: 0; padding: 9px 10px; border: 1px solid var(--line); border-radius: 9px; background: var(--surface); color: inherit; font: inherit; text-align: left; text-decoration: none; cursor: pointer; }
  .research-action:hover { border-color: color-mix(in srgb, var(--accent) 38%, var(--line)); background: var(--tint); }.research-action:disabled { opacity: .55; cursor: default; }
  .research-action > :global(.ico) { flex: none; color: var(--accent); }.research-action span { display: flex; min-width: 0; flex-direction: column; gap: 1px; }.research-action b { font-size: .7rem; }.research-action small { overflow: hidden; color: var(--faint); font-size: .6rem; text-overflow: ellipsis; white-space: nowrap; }
  .evidence-stack { margin-top: 12px; border: 1px solid var(--line); border-radius: 11px; overflow: hidden; background: var(--surface); }
  .evidence-stack > header { display: flex; justify-content: space-between; padding: 8px 10px; border-bottom: 1px solid var(--line); background: var(--surface-2); }.evidence-stack > header span { font-size: .63rem; font-weight: 700; text-transform: uppercase; }.evidence-stack > header small { color: var(--faint); font-size: .64rem; }
  .extra-sources summary { padding: 9px 11px; color: var(--muted); font-size: .67rem; cursor: pointer; }.extra-sources[open] summary { border-bottom: 1px solid var(--line); background: var(--surface-2); }
  .research-compose { padding: 12px 16px 14px; border-top: 1px solid var(--line-strong); background: var(--surface); }
  .sensitive-toggle { display: flex; align-items: flex-start; gap: 8px; margin: 0 1px 10px; cursor: pointer; }.sensitive-toggle input { margin-top: 2px; }.sensitive-toggle > span { display: flex; flex-direction: column; }.sensitive-toggle b { font-size: .7rem; font-weight: 650; }.sensitive-toggle small { color: var(--faint); font-size: .6rem; line-height: 1.35; }
  .research-compose form { display: grid; grid-template-columns: minmax(0, 1fr) auto; align-items: end; gap: 8px; }.research-compose textarea { width: 100%; resize: none; min-height: 50px; max-height: 130px; padding: 9px 10px; border: 1px solid var(--line-strong); border-radius: 10px; background: var(--bg); color: var(--ink); font: inherit; font-size: .8rem; line-height: 1.4; }.research-compose textarea:focus { border-color: var(--accent); outline: 2px solid color-mix(in srgb, var(--accent) 18%, transparent); }.research-send { min-height: 38px; }.research-note { display: block; margin-top: 7px; color: var(--faint); font-size: .59rem; }
  @keyframes research-in { from { transform: translateX(26px); opacity: .72; } } @keyframes pulse { to { opacity: .25; transform: translateY(-2px); } }
  @media (max-width: 640px) { .research-drawer { width: 100vw; border-left: 0; }.research-veil { display: none; }.research-head { padding-top: max(16px, env(safe-area-inset-top)); }.research-compose { padding-bottom: max(14px, env(safe-area-inset-bottom)); }.research-actions { grid-template-columns: 1fr; } }
  @media (prefers-reduced-motion: reduce) { .research-drawer, .research-thinking span { animation: none; }.prompt-list button { transform: none; } }
</style>
