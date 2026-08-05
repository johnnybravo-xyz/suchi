// Command palette. Cmd/Ctrl+K opens a modal <dialog>; type to filter
// static nav actions + live document search; arrow keys move selection;
// Enter navigates. Escape closes. No dependencies.
//
// Dialog + search UI is injected on demand so the palette is invisible
// (and inert) until first invocation. Zero overhead on page load beyond
// this one keydown listener.

(function () {
  const NAV = [
    { label: 'Documents',           href: '/',                     hint: 'g d' },
    { label: 'Inbox',               href: '/inbox',                hint: 'g i' },
    { label: 'Upload a document',   href: '/upload',               hint: 'g u' },
    { label: 'Pending decryption',  href: '/pending-decryption',   hint: 'g p' },
    { label: 'Mail setup',          href: '/admin/mail-setup',     hint: '' },
    { label: 'Setup wizard',        href: '/admin/setup',          hint: '' },
    { label: 'Docs (external)',     href: 'https://github.com/johnnybravo-xyz/suchi', hint: '' },
  ];

  let el = null;
  let items = [];
  let selected = 0;
  let searchToken = 0;

  function build() {
    const wrap = document.createElement('dialog');
    wrap.id = 'palette';
    wrap.innerHTML = `
      <div class="palette-box">
        <input type="text" id="palette-input" placeholder="Search or jump to…"
               autocomplete="off" autocapitalize="off" spellcheck="false">
        <ul id="palette-list" role="listbox"></ul>
        <footer class="palette-foot">
          <span><span class="kbd">↑↓</span> select</span>
          <span><span class="kbd">Enter</span> open</span>
          <span><span class="kbd">Esc</span> close</span>
        </footer>
      </div>
    `;
    document.body.appendChild(wrap);
    return wrap;
  }

  function ensure() {
    if (el) return el;
    el = document.getElementById('palette') || build();
    const input = el.querySelector('#palette-input');
    const list  = el.querySelector('#palette-list');
    input.addEventListener('input', function () {
      render(input.value.trim());
    });
    input.addEventListener('keydown', function (ev) {
      if (ev.key === 'ArrowDown') { ev.preventDefault(); move(1); }
      else if (ev.key === 'ArrowUp')   { ev.preventDefault(); move(-1); }
      else if (ev.key === 'Enter')     { ev.preventDefault(); commit(); }
    });
    // Click backdrop → close.
    el.addEventListener('click', function (ev) {
      const box = el.querySelector('.palette-box');
      if (!box.contains(ev.target)) el.close();
    });
    list.addEventListener('click', function (ev) {
      const li = ev.target.closest('li[data-idx]');
      if (!li) return;
      selected = parseInt(li.dataset.idx, 10);
      commit();
    });
    return el;
  }

  function move(delta) {
    if (!items.length) return;
    selected = (selected + delta + items.length) % items.length;
    paint();
  }

  function commit() {
    const it = items[selected];
    if (!it) return;
    el.close();
    if (it.external) window.open(it.href, '_blank', 'noopener');
    else             window.location.href = it.href;
  }

  function paint() {
    const list = el.querySelector('#palette-list');
    list.innerHTML = items.map(function (it, i) {
      return `<li role="option" data-idx="${i}"${i===selected?' class="on"':''}>
        <span class="palette-label">${escape(it.label)}</span>
        ${it.hint ? `<span class="kbd">${escape(it.hint)}</span>` : ''}
      </li>`;
    }).join('');
  }

  function escape(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c];
    });
  }

  function render(q) {
    const nav = NAV.filter(function (n) {
      return !q || n.label.toLowerCase().includes(q.toLowerCase());
    });
    items = nav.slice();
    selected = 0;
    paint();
    if (q.length < 2) return;

    const token = ++searchToken;
    fetch('/api/documents/?query=' + encodeURIComponent(q) + '&page_size=6',
          { headers: {'Accept':'application/json'}, credentials:'same-origin' })
      .then(r => r.ok ? r.json() : null)
      .then(function (body) {
        if (token !== searchToken || !body) return;
        const results = (body.results || []).map(function (d) {
          return { label: 'Doc: ' + (d.title || 'Untitled'),
                   href:  '/docs/' + d.id, hint: '#' + d.id };
        });
        items = nav.concat(results);
        paint();
      })
      .catch(function () { /* ignore */ });
  }

  function open() {
    ensure();
    el.querySelector('#palette-input').value = '';
    items = NAV.slice();
    selected = 0;
    paint();
    el.showModal();
    setTimeout(function () { el.querySelector('#palette-input').focus(); }, 0);
  }

  document.addEventListener('keydown', function (ev) {
    if ((ev.metaKey || ev.ctrlKey) && (ev.key === 'k' || ev.key === 'K')) {
      ev.preventDefault();
      open();
    }
  });

  document.addEventListener('DOMContentLoaded', function () {
    const btn = document.getElementById('palette-open');
    if (btn) btn.addEventListener('click', open);
  });
})();
