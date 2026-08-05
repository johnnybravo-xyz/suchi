// Theme toggle. Binary flip: dark ↔ light. No "auto" cycle position —
// first click sets an explicit preference against whatever the effective
// theme currently is (OS default until then). Preference lives in
// localStorage under `suchi.theme`; the `<html data-theme>` attribute
// is the single source of truth for CSS.
//
// Loaded synchronously in <head> (not defer) so the attribute is set
// before first paint — otherwise the page flashes light-then-dark on
// dark-preferring users.

(function () {
  const KEY = 'suchi.theme';
  const root = document.documentElement;

  function apply(v) {
    if (v === 'dark' || v === 'light') {
      root.setAttribute('data-theme', v);
    } else {
      root.removeAttribute('data-theme');
    }
  }

  function readPref() {
    try { return localStorage.getItem(KEY) || ''; } catch (_) { return ''; }
  }
  function writePref(v) {
    try { localStorage.setItem(KEY, v); } catch (_) { /* private mode: ignore */ }
  }

  // Effective theme: explicit pref wins; otherwise fall back to OS.
  function effective() {
    const v = readPref();
    if (v === 'dark' || v === 'light') return v;
    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }

  apply(readPref());

  // Modifier-key label swap: elements marked data-mod-key ship the mac
  // glyph by default; on non-mac platforms swap in "Ctrl". Runs at
  // DOMContentLoaded — cheap enough that the sync inline script owns it
  // rather than shipping a second file.
  function fixModKey() {
    const isMac = /Mac|iP(?:hone|ad|od)/.test(navigator.platform || '') ||
                  /Mac|iP(?:hone|ad|od)/.test(navigator.userAgent || '');
    if (isMac) return;
    document.querySelectorAll('[data-mod-key]').forEach(function (el) {
      el.textContent = el.textContent.replace(/⌘/g, 'Ctrl+');
    });
  }

  document.addEventListener('DOMContentLoaded', function () {
    fixModKey();
    const btn = document.getElementById('theme-toggle');
    if (!btn) return;

    function label() {
      const e = effective();
      // Show the icon of the target you'll get by clicking, not the
      // current — reads more like an action than a status.
      btn.textContent = e === 'dark' ? '☀' : '☾';
      btn.setAttribute('aria-label', 'Switch to ' + (e === 'dark' ? 'light' : 'dark') + ' theme');
    }
    label();

    btn.addEventListener('click', function () {
      const next = effective() === 'dark' ? 'light' : 'dark';
      writePref(next);
      apply(next);
      label();
    });

    // Track OS changes when the user hasn't set an explicit preference.
    window.matchMedia('(prefers-color-scheme: dark)')
      .addEventListener('change', function () {
        if (!readPref()) { apply(''); label(); }
      });
  });
})();
