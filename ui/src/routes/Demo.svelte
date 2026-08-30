<script>
  import Icon from '../lib/Icon.svelte'
  import { documentListHash } from '../lib/documentFilters.js'

  let { jdCategories = [] } = $props()
  const financeCategory = $derived(jdCategories.find((category) => Number(category.code) === 22))
  const northstarQuery = 'from:"Northstar Cloud" tag:renewal'
  const researchQuestion = 'What must Priya do before the Northstar workspace renews, and what will it cost?'

  const cards = [
    { title: 'Filter by language',
      hint: 'German documents from Berlin. Full-text search is Unicode-aware.',
      href: '#/search?lang=de',
      icon: 'search' },
    { title: 'Browse the Johnny Decimal tree',
      hint: 'Jump to Finance & Tax (category 22) and see the real filing structure.',
      jdCode: 22,
      icon: 'docs' },
    { title: 'Similar documents',
      hint: 'Open any utility bill; the Similar strip finds the rest of its household.',
      href: '#/documents?q=tag%3Autilities',
      icon: 'docs' },
    { title: 'Automations',
      hint: 'Manifest-owned examples tag utility bills and classify invoices.',
      href: '#/automations',
      icon: 'zap' },
    { title: 'Approvals inbox',
      hint: 'Review requests and document suggestions share one durable inbox.',
      href: '#/tasks',
      icon: 'tasks' },
    { title: 'Upload something',
      hint: 'Drop a PDF anywhere. Scratch uploads are cleared on reset.',
      href: '#/upload',
      icon: 'inbox' },
  ]

  function cardHref(card) {
    if (!card.jdCode) return card.href
    return financeCategory ? documentListHash({ jd_category_id: financeCategory.id }) : '#/documents'
  }
</script>

<div class="demo-wrap">
  <header class="demo-hero">
    <span class="eyebrow">Live archive · resets daily</span>
    <h1>See how Suchi finds, then explains</h1>
    <p class="lede">Start with the two new workflows below. A small Northstar workspace cluster makes the examples easy to recognize, then leaves you in the real product to inspect every result.</p>
    <div class="hero-note"><Icon name="help" size={14} /><span>Use the help icon in the top-right from any screen to return here.</span></div>
  </header>

  <section class="guided" aria-labelledby="guided-title">
    <div class="section-head">
      <div>
        <span class="eyebrow">Two-minute guided tour</span>
        <h2 id="guided-title">From a precise search to an answer with sources</h2>
      </div>
      <span class="tour-time">2 steps · anonymized data</span>
    </div>

    <div class="tour-grid">
      <article class="tour-step query-step">
        <span class="step-number">01</span>
        <span class="step-icon"><Icon name="search" size={20} /></span>
        <div class="step-copy">
          <span class="step-kicker">Rich query language</span>
          <h3>Narrow the archive without building a filter form</h3>
          <p>Combine correspondent and tag filters. Suggestions understand the same language in Search, Documents, Views, and MCP.</p>
          <code>{northstarQuery}</code>
          <a class="btn primary tour-action" href={`#/search?q=${encodeURIComponent(northstarQuery)}`}>
            Run the guided query <Icon name="chev" size={13} />
          </a>
          <small>Notice the three related documents and the active filter tokens.</small>
        </div>
      </article>

      <article class="tour-step research-step">
        <span class="step-number">02</span>
        <span class="step-icon"><Icon name="ask" size={20} /></span>
        <div class="step-copy">
          <span class="step-kicker">Archive research</span>
          <h3>Ask across related documents and keep the evidence attached</h3>
          <p>On a private installation, Suchi retrieves only the scoped Northstar documents, validates every citation, and can save the retrieved documents as a View.</p>
          <blockquote>{researchQuestion}</blockquote>
          <a class="btn tour-action" href="https://docs.suchi.page/archive-chat" target="_blank" rel="noopener">
            Read the grounded workflow <Icon name="chev" size={13} />
          </a>
          <small>Public demo identities cannot invoke a model. That boundary remains closed to prevent shared-provider abuse and cross-session leakage.</small>
        </div>
      </article>
    </div>
  </section>

  <section class="explore" aria-labelledby="explore-title">
    <div class="section-head compact">
      <div>
        <span class="eyebrow">Explore more</span>
        <h2 id="explore-title">The rest of the archive is yours</h2>
      </div>
    </div>
    <div class="grid">
      {#each cards as card}
        <a class="card" href={cardHref(card)}>
          <span class="card-icon"><Icon name={card.icon} size={17} /></span>
          <div class="body">
            <b>{card.title}</b>
            <span>{card.hint}</span>
          </div>
          <Icon name="chev" size={13} />
        </a>
      {/each}
    </div>
  </section>

  <footer>
    <p>Want your own? See <a href="https://docs.suchi.page/getting-started" target="_blank" rel="noopener">install docs</a> — one binary or one container.</p>
  </footer>
</div>

<style>
  .demo-wrap { max-width: 1040px; margin: 0 auto; padding: 18px 12px 48px; }
  .demo-hero { position: relative; overflow: hidden; padding: 28px 30px; border: 1px solid var(--line-strong); border-radius: 16px; background: linear-gradient(135deg, var(--surface), color-mix(in srgb, var(--accent) 8%, var(--surface))); }
  .demo-hero::after { content: ''; position: absolute; right: -65px; top: -95px; width: 250px; height: 250px; border: 1px solid color-mix(in srgb, var(--accent) 22%, transparent); border-radius: 50%; box-shadow: 0 0 0 35px color-mix(in srgb, var(--accent) 4%, transparent), 0 0 0 72px color-mix(in srgb, var(--accent) 3%, transparent); pointer-events: none; }
  .eyebrow { display: block; margin-bottom: 7px; color: var(--accent); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .63rem; font-weight: 700; }
  .demo-hero h1 { position: relative; z-index: 1; max-width: 700px; margin: 0; font-size: clamp(1.7rem, 3vw, 2.35rem); line-height: 1.08; }
  .lede { position: relative; z-index: 1; max-width: 720px; margin: 11px 0 17px; color: var(--muted); font-size: .9rem; line-height: 1.58; }
  .hero-note { position: relative; z-index: 1; display: inline-flex; align-items: center; gap: 7px; padding: 7px 10px; border: 1px solid var(--line); border-radius: 999px; background: var(--bg); color: var(--muted); font-size: .7rem; }
  .hero-note :global(.ico) { color: var(--accent); }
  .guided, .explore { margin-top: 28px; }
  .section-head { display: flex; align-items: flex-end; justify-content: space-between; gap: 20px; margin-bottom: 12px; }
  .section-head.compact { margin-bottom: 10px; }
  .section-head h2 { font-size: 1.2rem; }
  .tour-time { flex: none; color: var(--faint); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .63rem; }
  .tour-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
  .tour-step { position: relative; display: grid; grid-template-columns: auto minmax(0, 1fr); gap: 12px; min-height: 320px; padding: 19px; border: 1px solid var(--line); border-radius: 14px; background: var(--surface); }
  .tour-step::before { content: ''; position: absolute; inset: 0; border-radius: inherit; background: linear-gradient(145deg, color-mix(in srgb, var(--accent) 5%, transparent), transparent 55%); pointer-events: none; }
  .step-number { position: absolute; top: 14px; right: 15px; color: var(--line-strong); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: 1.55rem; font-weight: 700; }
  .step-icon { position: relative; display: grid; place-items: center; width: 38px; height: 38px; border: 1px solid color-mix(in srgb, var(--accent) 25%, var(--line)); border-radius: 10px; background: var(--tint); color: var(--accent); }
  .step-copy { position: relative; display: flex; align-items: flex-start; min-width: 0; flex-direction: column; }
  .step-kicker { margin: 2px 45px 5px 0; color: var(--accent); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .6rem; font-weight: 700; }
  .step-copy h3 { max-width: 360px; font-size: 1rem; line-height: 1.3; }
  .step-copy p { margin: 8px 0 12px; color: var(--muted); font-size: .76rem; line-height: 1.5; }
  .step-copy code, .step-copy blockquote { width: 100%; margin: auto 0 12px; padding: 10px 11px; border: 1px solid var(--line); border-radius: 8px; background: var(--bg); color: var(--ink); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .67rem; line-height: 1.45; }
  .step-copy blockquote { font-family: inherit; font-style: normal; }
  .tour-action { display: inline-flex; align-items: center; justify-content: center; gap: 7px; min-width: 205px; text-decoration: none; }
  .step-copy > small { margin-top: 8px; color: var(--faint); font-size: .63rem; line-height: 1.4; }
  .grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 9px; }
  .card { display: grid; grid-template-columns: auto minmax(0, 1fr) auto; align-items: center; gap: 10px; min-height: 92px; padding: 12px; border: 1px solid var(--line); border-radius: 10px; background: var(--surface); color: inherit; text-decoration: none; transition: border-color .15s ease, transform .15s ease; }
  .card:hover { border-color: var(--accent); transform: translateY(-1px); }
  .card-icon { display: grid; place-items: center; width: 31px; height: 31px; border-radius: 8px; background: var(--tint); color: var(--accent); }
  .card .body { display: flex; min-width: 0; flex-direction: column; gap: 3px; }
  .card .body b { font-size: .78rem; }
  .card .body span { color: var(--muted); font-size: .68rem; line-height: 1.4; }
  .card > :global(.ico) { color: var(--faint); }
  footer { margin-top: 28px; color: var(--muted); font-size: .8rem; }
  @media (max-width: 800px) { .tour-grid { grid-template-columns: 1fr; }.tour-step { min-height: 300px; }.grid { grid-template-columns: 1fr 1fr; } }
  @media (max-width: 560px) { .demo-wrap { padding: 6px 0 35px; }.demo-hero { padding: 22px 18px; }.section-head { align-items: flex-start; flex-direction: column; }.tour-time { display: none; }.tour-step { grid-template-columns: 1fr; }.step-icon { margin-bottom: 2px; }.grid { grid-template-columns: 1fr; }.tour-action { width: 100%; } }
</style>
