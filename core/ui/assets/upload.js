// suchi upload — vanilla JS, no framework, no build step. Loaded as
// an external asset (not inline) so the strict Content-Security-Policy
// default-src 'self' accepts it without needing 'unsafe-inline'.
//
// Behavior:
//   - Native file input + drag-drop, both feed the same file field.
//   - Submit intercepts the form, POSTs multipart to /api/documents/,
//     and redirects to /docs/{id} on 201/200.
//   - i18n strings ride in data-* attributes on the form so this file
//     stays static + cacheable.

(function () {
  const form = document.getElementById('upload-form');
  if (!form) { return; }
  const input = form.querySelector('input[type=file]');
  const filename = document.getElementById('filename');
  const status = document.getElementById('status');
  const btn = document.getElementById('submit-btn');
  const dz = document.getElementById('dropzone');
  const msg = form.dataset;

  input.addEventListener('change', () => {
    filename.textContent = input.files[0] ? input.files[0].name : '';
  });

  ['dragenter', 'dragover'].forEach(evt =>
    dz.addEventListener(evt, e => { e.preventDefault(); dz.classList.add('is-drag'); }));
  ['dragleave', 'drop'].forEach(evt =>
    dz.addEventListener(evt, e => { e.preventDefault(); dz.classList.remove('is-drag'); }));
  dz.addEventListener('drop', e => {
    if (e.dataTransfer.files.length > 0) {
      input.files = e.dataTransfer.files;
      filename.textContent = input.files[0].name;
    }
  });

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    if (!input.files[0]) { status.textContent = msg.msgErrNoFile; return; }
    btn.disabled = true;
    status.className = 'status muted';
    status.textContent = msg.msgUploading;

    const fd = new FormData();
    fd.append('document', input.files[0]);
    try {
      const resp = await fetch('/api/documents/', {
        method: 'POST',
        body: fd,
        headers: { 'Accept': 'application/json' },
        credentials: 'same-origin',
      });
      if (resp.status === 201 || resp.status === 200) {
        const body = await resp.json();
        window.location = '/docs/' + body.id;
        return;
      }
      if (resp.status === 409) {
        const body = await resp.json().catch(() => ({}));
        status.className = 'status is-error';
        status.textContent = msg.msgErrConflict + (body.id ? ' → /docs/' + body.id : '');
      } else {
        const text = await resp.text();
        status.className = 'status is-error';
        status.textContent = msg.msgErrGeneric.replace('%s', resp.status + ' ' + text);
      }
    } catch (err) {
      status.className = 'status is-error';
      status.textContent = msg.msgErrGeneric.replace('%s', err.message);
    } finally {
      btn.disabled = false;
    }
  });
})();
