// detail-actions.js — four small interactions on the doc detail page:
//
//   1. Sensitivity picker — PATCH /api/documents/{id} with the chosen
//      level. Reload on high-sensitivity write so the preview gate can
//      re-render.
//   2. Reveal button — on high-sensitivity docs the veil hides the
//      preview; clicking reveals it in-place.
//   3. Share card — POST /api/share_links/ and echo the public URL.
//   4. Custom-field value editor — PUT / DELETE
//      /api/documents/{id}/custom_fields/{field}. Widget per data-type,
//      change-driven auto-save, per-field status indicator.

(function () {
  wireSensitivityPicker();
  wireSensReveal();
  wireShareForm();
  wireCustomFields();

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

  function wireCustomFields() {
    const section = document.querySelector('.custom-fields[data-doc-id]');
    if (!section) return;
    const docID = section.dataset.docId;

    // For multi: mark the initial checkboxes from data-multi-raw.
    section.querySelectorAll('.cf-multi[data-multi-raw]').forEach((wrap) => {
      let arr = [];
      try { arr = JSON.parse(wrap.dataset.multiRaw || '[]'); } catch (_) {}
      if (!Array.isArray(arr)) return;
      wrap.querySelectorAll('input[type="checkbox"]').forEach((cb) => {
        cb.checked = arr.includes(cb.value);
      });
    });

    section.querySelectorAll('form.cf-value').forEach((form) => {
      const name     = form.dataset.fieldName;
      const dataType = form.dataset.dataType;
      const status   = form.querySelector('.cf-status');

      const save = debounce(async () => {
        const value = collectValue(form, dataType);
        status.textContent = 'Saving…';
        status.className = 'cf-status';
        try {
          let resp;
          if (value === null) {
            resp = await fetch(cfURL(docID, name), { method: 'DELETE' });
          } else {
            resp = await fetch(cfURL(docID, name), {
              method: 'PUT',
              headers: { 'content-type': 'application/json' },
              body: JSON.stringify({ value }),
            });
          }
          if (resp.ok) {
            status.textContent = 'Saved';
            status.className = 'cf-status saved';
          } else {
            const txt = await resp.text().catch(() => '');
            status.textContent = `HTTP ${resp.status}`;
            status.className = 'cf-status error';
            status.title = txt;
          }
        } catch (e) {
          status.textContent = 'Network error';
          status.className = 'cf-status error';
          status.title = e.message;
        }
      }, 300);

      form.addEventListener('input', save);
      form.addEventListener('change', save);
      form.addEventListener('submit', (e) => e.preventDefault());
    });
  }

  function collectValue(form, dataType) {
    if (dataType === 'multi') {
      const arr = [];
      form.querySelectorAll('input[type="checkbox"]:checked').forEach((cb) => arr.push(cb.value));
      return arr.length > 0 ? arr : null;
    }
    const raw = (form.elements.value?.value ?? '').trim();
    if (raw === '') return null; // treat empty as clear
    switch (dataType) {
      case 'number':
      case 'monetary': {
        const n = Number(raw);
        return Number.isFinite(n) ? n : null;
      }
      case 'bool':
        return raw === 'true';
      case 'date': {
        // <input type="date"> gives YYYY-MM-DD. suchi stores date as
        // unix seconds; the API accepts an epoch int here.
        const t = Date.parse(raw + 'T00:00:00Z');
        return Number.isFinite(t) ? Math.floor(t / 1000) : null;
      }
      default:
        return raw;
    }
  }

  function cfURL(docID, fieldName) {
    return `/api/documents/${encodeURIComponent(docID)}/custom_fields/${encodeURIComponent(fieldName)}`;
  }

  function debounce(fn, ms) {
    let h;
    return function () {
      clearTimeout(h);
      h = setTimeout(fn, ms);
    };
  }

  function isHigh(v) { return v === 'confidential' || v === 'restricted'; }

  function escapeHTML(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { '&':'&amp;', '<':'&lt;', '>':'&gt;',
               '"':'&quot;', "'":'&#39;' }[c];
    });
  }
})();
