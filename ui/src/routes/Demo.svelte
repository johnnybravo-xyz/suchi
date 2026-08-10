<script>
  // /app/#/demo — landing panel for the public demo instance. Only
  // reachable when the backend reports demo mode enabled; App.svelte
  // hides the link otherwise. No forced tour: each card is one line of
  // copy + one link into a real SPA view that already works. The point
  // is to shortcut a first-time visitor into a populated state so they
  // see suchi doing something in ~30 seconds.

  import Icon from '../lib/Icon.svelte'

  const cards = [
    { title: 'Search across the archive',
      hint: 'Try "bescom" — surfaces every electricity bill by content, not filename.',
      href: '#/search?q=bescom',
      icon: 'search' },
    { title: 'Filter by language',
      hint: 'German docs from a Berlin trip. Full-text search is Unicode-aware.',
      href: '#/search?lang=de',
      icon: 'search' },
    { title: 'Browse the Johnny Decimal tree',
      hint: 'Jump to Finance & Tax (JD 22) and see the real filing structure.',
      href: '#/documents?jd=22',
      icon: 'docs' },
    { title: 'Similar documents',
      hint: 'Click any bill; the "Similar" strip clusters the rest of its household.',
      href: '#/documents',
      icon: 'docs' },
    { title: 'Automations',
      hint: 'Three seeded automations — tag utilities, flag high-value, route tax letters.',
      href: '#/automations',
      icon: 'zap' },
    { title: 'Approvals inbox',
      hint: 'One pending expense-review approval + heuristics-proposal cards.',
      href: '#/tasks',
      icon: 'tasks' },
    { title: 'Upload something',
      hint: 'Drop a PDF anywhere — the pipeline files it. Auto-cleared on reset.',
      href: '#/upload',
      icon: 'inbox' },
    { title: 'Admin surface',
      hint: 'Read-only from the demo — see the shape without editing anyone else’s world.',
      href: '#/admin',
      icon: 'settings' },
  ]
</script>

<div class="demo-wrap">
  <header>
    <h1>Welcome to the suchi demo</h1>
    <p class="lede">This is a live suchi instance with a representative archive. Everything you do here is real; nothing you do here survives the next reset. Poke at whatever catches your eye — the shortcuts below jump straight into a populated view.</p>
  </header>

  <div class="grid">
    {#each cards as c}
      <a class="card" href={c.href}>
        <span class="ico"><Icon name={c.icon} size={18} /></span>
        <div class="body">
          <b>{c.title}</b>
          <span>{c.hint}</span>
        </div>
      </a>
    {/each}
  </div>

  <footer>
    <p>Want your own? See <a href="https://docs.suchi.page/getting-started" target="_blank" rel="noopener">install docs</a> — one binary or one container.</p>
  </footer>
</div>

<style>
  .demo-wrap { max-width: 900px; margin: 0 auto; padding: 24px 16px 48px; }
  header h1 { font-size: 1.7rem; margin: 0 0 8px; }
  header .lede { color: var(--muted); line-height: 1.5; margin: 0 0 24px; max-width: 65ch; }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
    gap: 12px;
  }
  .card {
    display: flex;
    gap: 12px;
    padding: 14px;
    border: 1px solid var(--line);
    border-radius: 10px;
    background: var(--surface);
    text-decoration: none;
    color: inherit;
    transition: border-color .15s, transform .05s;
  }
  .card:hover { border-color: var(--accent); }
  .card:active { transform: translateY(1px); }
  .card .ico {
    flex: 0 0 auto;
    width: 32px; height: 32px;
    display: grid; place-items: center;
    border-radius: 8px;
    background: color-mix(in oklab, var(--accent) 12%, transparent);
    color: var(--accent);
  }
  .card .body { display: flex; flex-direction: column; gap: 4px; }
  .card .body b { font-size: .95rem; }
  .card .body span { color: var(--muted); font-size: .85rem; line-height: 1.35; }
  footer { margin-top: 28px; color: var(--muted); font-size: .9rem; }
</style>
