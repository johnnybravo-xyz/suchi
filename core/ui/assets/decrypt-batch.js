// Pending-decryption batch flow. Extracted from an inline <script> so
// the page stays under the strict CSP (default-src 'self').
//
// Wires the "select-all" checkbox and the submit handler to /api/documents/
// decrypt-batch. On success, reports counts and reloads to refresh the
// queue.

(function () {
  const form = document.getElementById('decrypt-form');
  if (!form) return;
  const status = document.getElementById('decrypt-status');
  const selectAll = document.getElementById('select-all');

  if (selectAll) {
    selectAll.addEventListener('change', function () {
      document.querySelectorAll('input[name=doc]')
        .forEach(function (i) { i.checked = selectAll.checked; });
    });
  }

  form.addEventListener('submit', async function (ev) {
    ev.preventDefault();
    const password = form.querySelector('input[name=password]').value;
    const remember = form.querySelector('input[name=remember]').checked;
    const label    = form.querySelector('input[name=label]').value;
    const ids = Array.from(form.querySelectorAll('input[name=doc]:checked'))
      .map(function (el) { return parseInt(el.value, 10); });
    if (!ids.length) {
      status.textContent = 'Select at least one document.';
      return;
    }
    status.textContent = 'Decrypting…';
    try {
      const resp = await fetch('/api/documents/decrypt-batch', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' },
        credentials: 'same-origin',
        body: JSON.stringify({ doc_ids: ids, password: password, remember: remember, label: label }),
      });
      if (!resp.ok) {
        let err = {};
        try { err = await resp.json(); } catch (_) { err = { error: resp.statusText }; }
        status.textContent = 'Failed: ' + (err.error || resp.status);
        return;
      }
      const body = await resp.json();
      const ok = body.results.filter(function (r) { return r.ok; }).length;
      const bad = body.results.length - ok;
      status.textContent = 'Decrypted ' + ok + ', failed ' + bad + '. Reloading…';
      setTimeout(function () { location.reload(); }, 900);
    } catch (e) {
      status.textContent = 'Network error: ' + e.message;
    }
  });
})();
