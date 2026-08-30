<script>
  import { askArchive } from './api.js'
  import { go } from './router.svelte.js'
  import { sensitivityLabel, sensDot } from './format.js'
  import Icon from './Icon.svelte'

  let { open = false, request = { id: 0, question: '' }, onClose, onReturnFocus } = $props()
  let turns = $state([])
  let draft = $state('')
  let includeSensitive = $state(false)
  let sending = $state(false)
  let controller
  let composer = $state(), drawer = $state()
  let previousFocus
  let wasOpen = false
  let handledRequest = 0

  function history() {
    return turns.slice(-2).flatMap(turn => [
      { role: 'user', content: turn.question },
      ...(turn.answer ? [{ role: 'assistant', content: turn.answer }] : []),
    ]).slice(-4)
  }

  function updateTurn(id, patch) {
    turns = turns.map(turn => turn.id === id ? { ...turn, ...patch } : turn)
  }

  async function send(question = draft) {
    question = question.trim()
    if (!question || sending || question.length > 2000) return
    const prior = history()
    const turn = { id: Date.now(), question, answer: '', sources: [], viewQuery: '', error: '' }
    turns = [...turns, turn]
    draft = ''
    sending = true
    controller = new AbortController()
    try {
      const result = await askArchive({ question, history: prior, include_sensitive: includeSensitive }, controller.signal)
      updateTurn(turn.id, {
        answer: result.answer || '',
        sources: result.sources || [],
        viewQuery: result.view_query || '',
      })
    } catch (ex) {
      updateTurn(turn.id, {
        error: ex?.name === 'AbortError' ? 'Request canceled.' : (ex?.message || 'The archive question could not be answered.'),
      })
    } finally {
      sending = false
      controller = null
      queueMicrotask(() => composer?.focus())
    }
  }

  function cancel() { controller?.abort() }

  function clearConversation() {
    cancel()
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

  function openSource(id) {
    close()
    go(`#/doc/${id}`)
  }

  function onKey(e) {
    if (e.key === 'Escape') { e.preventDefault(); close(); return }
    if (e.key !== 'Tab' || !drawer) return
    const focusable = [...drawer.querySelectorAll('button:not([disabled]), a[href], textarea:not([disabled]), input:not([disabled])')]
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
    if (request.question?.trim()) send(request.question)
    else queueMicrotask(() => composer?.focus())
  })
</script>

{#if open}
  <button class="chat-veil" aria-label="Close archive questions" onclick={close}></button>
  <div class="chat-drawer" bind:this={drawer} role="dialog" aria-modal="true" aria-labelledby="chat-title" tabindex="-1" onkeydown={onKey}>
    <header class="chat-head">
      <div>
        <span class="chat-kicker">Grounded in your documents</span>
        <h2 id="chat-title">Ask the archive</h2>
      </div>
      <div class="chat-head-actions">
        <button class="btn sm" onclick={clearConversation} disabled={!turns.length && !includeSensitive}>Clear</button>
        <button class="btn sm icon-only" onclick={close} aria-label="Close archive questions" title="Close"><Icon name="x" size={14} /></button>
      </div>
    </header>

    <div class="chat-transcript" aria-live="polite">
      {#if turns.length === 0}
        <div class="chat-empty">
          <span class="chat-empty-mark"><Icon name="ask" size={23} /></span>
          <b>Ask about what you’ve filed</b>
          <p>Answers use documents you can already access and always show the retrieved sources.</p>
        </div>
      {/if}
      {#each turns as turn (turn.id)}
        <article class="chat-turn">
          <p class="chat-question">{turn.question}</p>
          {#if turn.answer}<p class="chat-answer">{turn.answer}</p>{/if}
          {#if turn.error}<div class="chat-error">{turn.error}</div>{/if}
          {#if sending && turn === turns[turns.length - 1] && !turn.answer && !turn.error}
            <div class="chat-thinking"><span></span><span></span><span></span><em>Reading the archive</em></div>
          {/if}
          {#if turn.sources.length}
            <div class="evidence" aria-label="Sources">
              <div class="evidence-head"><span>Sources</span><small>{turn.sources.length} retrieved</small></div>
              {#each turn.sources as source, i (source.id)}
                <button class="source-card" onclick={() => openSource(source.id)} aria-label={`Open source ${i + 1}: ${source.title}`}>
                  <span class="source-number">[{i + 1}]</span>
                  <span class="source-copy">
                    <strong>{source.title || `Document #${source.id}`}</strong>
                    <span>{source.snippet}</span>
                    <small><i class:danger={sensDot(source.sensitivity) === 'danger'}></i>{sensitivityLabel(source.sensitivity)}</small>
                  </span>
                  <Icon name="chev" size={13} />
                </button>
              {/each}
            </div>
            {#if turn.viewQuery}
              <a class="save-view" href={`#/views?new=1&q=${encodeURIComponent(turn.viewQuery)}`} onclick={close}>
                <Icon name="eye" size={14} /> Save search as view
              </a>
            {/if}
          {/if}
        </article>
      {/each}
    </div>

    <footer class="chat-compose">
      <label class="sensitive-toggle">
        <input type="checkbox" bind:checked={includeSensitive} />
        <span><b>Include sensitive documents</b><small>For this conversation only</small></span>
      </label>
      <form onsubmit={(e) => { e.preventDefault(); send() }}>
        <textarea bind:this={composer} bind:value={draft} maxlength="2000" rows="2" placeholder="Ask a question about your documents"
                  aria-label="Question" disabled={sending}
                  onkeydown={(e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send() } }}></textarea>
        {#if sending}
          <button type="button" class="btn chat-send" onclick={cancel}>Cancel</button>
        {:else}
          <button class="btn primary chat-send" disabled={!draft.trim()}><Icon name="ask" size={14} /> Send</button>
        {/if}
      </form>
      <small class="chat-note">Answers can be incomplete. Check the cited documents.</small>
    </footer>
  </div>
{/if}

<style>
  .chat-veil { position: fixed; inset: 0; z-index: 90; border: 0; background: color-mix(in srgb, var(--ink) 24%, transparent); }
  .chat-drawer { position: fixed; z-index: 91; inset: 0 0 0 auto; width: min(470px, 100vw); display: grid;
    grid-template-rows: auto minmax(0, 1fr) auto; background: var(--bg); border-left: 1px solid var(--line-strong);
    box-shadow: -18px 0 48px color-mix(in srgb, var(--ink) 14%, transparent); animation: chat-in .18s ease-out; }
  .chat-head { display: flex; align-items: center; justify-content: space-between; gap: 16px; padding: 18px 19px 15px; border-bottom: 1px solid var(--line); }
  .chat-kicker { display: block; margin-bottom: 3px; color: var(--accent); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .61rem; font-weight: 700; text-transform: uppercase; }
  .chat-head h2 { font-size: 1.08rem; }
  .chat-head-actions { display: flex; gap: 7px; }
  .icon-only { padding-inline: 9px; }
  .chat-transcript { overflow-y: auto; padding: 20px 18px 28px; overscroll-behavior: contain; }
  .chat-empty { min-height: 55%; display: flex; flex-direction: column; align-items: center; justify-content: center; text-align: center; color: var(--muted); }
  .chat-empty-mark { display: grid; place-items: center; width: 48px; height: 48px; margin-bottom: 13px; border: 1px solid var(--line-strong); border-radius: 50%; color: var(--accent); background: var(--surface); }
  .chat-empty b { color: var(--ink); font-size: .94rem; }
  .chat-empty p { max-width: 300px; margin: 6px 0; font-size: .81rem; line-height: 1.55; }
  .chat-turn + .chat-turn { margin-top: 27px; padding-top: 25px; border-top: 1px solid var(--line); }
  .chat-question { width: fit-content; max-width: 88%; margin: 0 0 14px auto; padding: 9px 12px; border-radius: 12px 12px 3px 12px; background: var(--tint); color: var(--ink); font-size: .84rem; line-height: 1.45; white-space: pre-wrap; }
  .chat-answer { margin: 0; color: var(--ink); font-size: .88rem; line-height: 1.65; white-space: pre-wrap; }
  .chat-error { padding: 10px 12px; border-left: 2px solid var(--danger); background: color-mix(in srgb, var(--danger) 7%, transparent); color: var(--muted); font-size: .8rem; }
  .chat-thinking { display: flex; align-items: center; gap: 4px; color: var(--muted); }
  .chat-thinking span { width: 5px; height: 5px; border-radius: 50%; background: var(--accent); animation: pulse 1s infinite alternate; }
  .chat-thinking span:nth-child(2) { animation-delay: .16s; }.chat-thinking span:nth-child(3) { animation-delay: .32s; }
  .chat-thinking em { margin-left: 5px; font-size: .76rem; font-style: normal; }
  .evidence { margin-top: 17px; border: 1px solid var(--line); border-radius: 12px; overflow: hidden; background: var(--surface); }
  .evidence-head { display: flex; justify-content: space-between; padding: 9px 11px; border-bottom: 1px solid var(--line); background: var(--surface-2); }
  .evidence-head span { font-size: .67rem; font-weight: 700; text-transform: uppercase; }.evidence-head small { color: var(--faint); font-size: .68rem; }
  .source-card { width: 100%; display: grid; grid-template-columns: auto minmax(0, 1fr) auto; align-items: start; gap: 9px; padding: 11px; border: 0; border-bottom: 1px solid var(--line); background: transparent; color: inherit; text-align: left; cursor: pointer; }
  .source-card:last-child { border-bottom: 0; }.source-card:hover { background: var(--tint); }
  .source-number { color: var(--accent); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .69rem; font-weight: 700; }
  .source-copy { min-width: 0; display: flex; flex-direction: column; gap: 4px; }
  .source-copy strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: .78rem; }
  .source-copy > span { display: -webkit-box; overflow: hidden; line-clamp: 2; -webkit-line-clamp: 2; -webkit-box-orient: vertical; color: var(--muted); font-size: .72rem; line-height: 1.4; }
  .source-copy small { display: flex; align-items: center; gap: 5px; color: var(--faint); font-size: .65rem; }
  .source-copy i { width: 6px; height: 6px; border-radius: 50%; background: var(--line-strong); }.source-copy i.danger { background: var(--danger); }
  .source-card > :global(.ico) { margin-top: 3px; color: var(--faint); }
  .save-view { display: inline-flex; align-items: center; gap: 6px; margin-top: 11px; color: var(--accent); font-size: .76rem; font-weight: 650; text-decoration: none; }
  .chat-compose { padding: 13px 17px 15px; border-top: 1px solid var(--line-strong); background: var(--surface); }
  .sensitive-toggle { display: flex; align-items: center; gap: 8px; margin: 0 1px 10px; cursor: pointer; }
  .sensitive-toggle > span { display: flex; flex-direction: column; }.sensitive-toggle b { font-size: .72rem; font-weight: 600; }.sensitive-toggle small { color: var(--faint); font-size: .63rem; }
  .chat-compose form { display: grid; grid-template-columns: minmax(0, 1fr) auto; align-items: end; gap: 8px; }
  .chat-compose textarea { width: 100%; resize: none; min-height: 50px; max-height: 130px; padding: 9px 10px; border: 1px solid var(--line-strong); border-radius: 10px; background: var(--bg); color: var(--ink); font: inherit; font-size: .82rem; line-height: 1.4; }
  .chat-send { min-height: 38px; }.chat-note { display: block; margin-top: 7px; color: var(--faint); font-size: .62rem; }
  @keyframes chat-in { from { transform: translateX(24px); opacity: .7; } }
  @keyframes pulse { to { opacity: .25; transform: translateY(-2px); } }
  @media (max-width: 640px) {
    .chat-drawer { width: 100vw; border-left: 0; }.chat-veil { display: none; }
    .chat-head { padding-top: max(16px, env(safe-area-inset-top)); }.chat-compose { padding-bottom: max(14px, env(safe-area-inset-bottom)); }
  }
  @media (prefers-reduced-motion: reduce) { .chat-drawer, .chat-thinking span { animation: none; } }
</style>
