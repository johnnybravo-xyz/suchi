// Detail-page decrypt form. Extracted from an inline <script> so the
// page can stay under a strict CSP (default-src 'self', no unsafe-inline).
// The form carries data-doc-id; success → 700ms wait, then reload.

(function () {
  const form = document.querySelector('form[data-decrypt]');
  if (!form) return;
  const statusEl = document.getElementById('decrypt-one-status');
  const docId = form.getAttribute('data-doc-id');

  form.addEventListener('submit', async function (ev) {
    ev.preventDefault();
    statusEl.textContent = 'Decrypting…';
    try {
      const resp = await fetch(`/api/documents/${docId}/decrypt`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' },
        credentials: 'same-origin',
        body: JSON.stringify({
          password: form.password.value,
          remember: form.remember.checked,
          label: form.label.value,
        }),
      });
      if (resp.status === 204) {
        statusEl.textContent = 'Decrypted. Reloading…';
        setTimeout(() => location.reload(), 700);
      } else {
        let err = {};
        try { err = await resp.json(); } catch (_) { err = { error: resp.statusText }; }
        statusEl.textContent = 'Failed: ' + (err.error || resp.status);
      }
    } catch (e) {
      statusEl.textContent = 'Network error: ' + e.message;
    }
  });
})();
