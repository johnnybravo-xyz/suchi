// detail-actions.js — three small interactions on the doc detail page:
//
//   1. Sensitivity picker — PATCH /api/documents/{id} with the chosen
//      level. Reload on high-sensitivity write so the preview gate can
//      re-render.
//   2. Reveal button — on high-sensitivity docs the veil hides the
//      preview; clicking reveals it in-place.
//   3. Share card — POST /api/share_links/ and echo the public URL.

(function () {
  wireSensitivityPicker();
  wireSensReveal();
  wireShareForm();

  function wireSensitivityPicker() {
    const picker = document.getElementById('sens-picker');
    if (!picker) return;
    const status = document.getElementById('sens-status');
    const docID = picker.getAttribute('data-doc-id');
    const initial = picker.value;
    picker.addEventListener('change', async function () {
      status.textContent = 'Saving…';
      try {
        const r = await fetch('/api/documents/' + docID, {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json',
                     'Accept': 'application/json' },
          credentials: 'same-origin',
          body: JSON.stringify({ sensitivity: picker.value }),
        });
        if (!r.ok) {
          picker.value = initial;
          let err = {};
          try { err = await r.json(); } catch (_) {}
          status.textContent = 'Failed: ' + (err.error || r.statusText);
          return;
        }
        status.textContent = 'Saved.';
        // If either the OLD or NEW value is high-sensitivity, the
        // preview veil state changes — reload so the server-rendered
        // veil is correct.
        if (isHigh(initial) !== isHigh(picker.value)) {
          setTimeout(function () { location.reload(); }, 300);
        }
      } catch (e) {
        picker.value = initial;
        status.textContent = 'Network error: ' + e.message;
      }
    });
  }

  function wireSensReveal() {
    const btn = document.getElementById('sens-reveal');
    if (!btn) return;
    const veil = document.getElementById('sens-veil');
    btn.addEventListener('click', function () {
      veil.classList.add('revealed');
    });
  }

  function wireShareForm() {
    const form = document.querySelector('form[data-share]');
    if (!form) return;
    const status = document.getElementById('share-status');
    const docID = form.getAttribute('data-doc-id');
    form.addEventListener('submit', async function (ev) {
      ev.preventDefault();
      status.textContent = 'Creating link…';
      const label = form.label.value;
      const expires = parseInt(form.expires_in_sec.value, 10) || 0;
      const password = form.password.value;
      try {
        const r = await fetch('/api/share_links/', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json',
                     'Accept': 'application/json' },
          credentials: 'same-origin',
          body: JSON.stringify({
            doc_ids: [parseInt(docID, 10)],
            label: label,
            expires_in_sec: expires,
            password: password,
          }),
        });
        if (!r.ok) {
          let err = {};
          try { err = await r.json(); } catch (_) {}
          status.textContent = 'Failed: ' + (err.error || r.statusText);
          return;
        }
        const body = await r.json();
        const url = location.origin + body.public_url;
        status.innerHTML = 'Link created — <a href="' + url +
          '" target="_blank" rel="noopener">' + escapeHTML(url) + '</a>';
        form.reset();
      } catch (e) {
        status.textContent = 'Network error: ' + e.message;
      }
    });
  }

  function isHigh(v) { return v === 'confidential' || v === 'restricted'; }

  function escapeHTML(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { '&':'&amp;', '<':'&lt;', '>':'&gt;',
               '"':'&quot;', "'":'&#39;' }[c];
    });
  }
})();
