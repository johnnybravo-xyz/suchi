// setup wizard — vanilla JS. Every step form declares its intent via
// data-* attributes; this file wires generic submit + skip + redirect.
//
// Behaviors:
//   data-step="<name>"                     — required, matches API step slug.
//   data-endpoint="/api/..."               — POST target for the step's payload.
//   data-mode="mark-done"                  — no body, just record the step done.
//   data-skip on any <button type="button">— records the step skipped.
//   <input data-confirms="confirm_blank">  — reveals a hidden checkbox
//                                             matching the referenced name.

(function () {
  const forms = document.querySelectorAll('form.setup-step-form');
  forms.forEach(bind);

  const complete = document.getElementById('setup-complete-form');
  if (complete) complete.addEventListener('submit', finalize);

  function bind(form) {
    // Reveal a confirm checkbox when a "confirms=NAME" radio is picked.
    form.querySelectorAll('input[data-confirms]').forEach(inp => {
      const name = inp.dataset.confirms;
      const wrap = form.querySelector('#confirm-blank-wrap') ||
                    form.querySelector(`[data-confirm-wrap="${name}"]`);
      if (!wrap) return;
      const sync = () => { wrap.hidden = !inp.checked; };
      inp.addEventListener('change', sync);
      // Also hide when a sibling radio (different value) fires.
      form.querySelectorAll(`input[type=radio][name="${inp.name}"]`).forEach(r => {
        r.addEventListener('change', () => {
          if (r !== inp) wrap.hidden = true;
        });
      });
    });

    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      await runStep(form, 'done');
    });

    form.querySelectorAll('[data-skip]').forEach(btn => {
      btn.addEventListener('click', () => runStep(form, 'skipped'));
    });
  }

  async function runStep(form, status) {
    const step = form.dataset.step;
    const statusEl = form.querySelector('[data-status]');
    const buttons = form.querySelectorAll('button');
    buttons.forEach(b => b.disabled = true);
    if (statusEl) statusEl.textContent = status === 'skipped' ? 'Skipping…' : 'Saving…';

    try {
      // Step-specific side effect first (only when Done + endpoint set).
      if (status === 'done' && form.dataset.endpoint) {
        const body = buildBody(form);
        const resp = await fetch(form.dataset.endpoint, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' },
          credentials: 'same-origin',
          body: JSON.stringify(body),
        });
        if (!resp.ok) {
          const data = await resp.json().catch(() => ({}));
          const msg = (data.error && data.error.message) || resp.statusText;
          if (statusEl) { statusEl.textContent = 'Error: ' + msg; }
          buttons.forEach(b => b.disabled = false);
          return;
        }
      }

      // Then record the step outcome.
      const rec = await fetch(`/api/admin/setup/step/${encodeURIComponent(step)}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'same-origin',
        body: JSON.stringify({ status }),
      });
      if (!rec.ok) {
        if (statusEl) statusEl.textContent = 'Failed to record step: ' + rec.statusText;
        buttons.forEach(b => b.disabled = false);
        return;
      }

      // Advance to the next incomplete step, or the recap.
      window.location = window.location.pathname + advanceParam(step);
    } catch (err) {
      if (statusEl) statusEl.textContent = 'Error: ' + err.message;
      buttons.forEach(b => b.disabled = false);
    }
  }

  async function finalize(e) {
    e.preventDefault();
    const btn = e.currentTarget.querySelector('button');
    btn.disabled = true;
    const resp = await fetch('/api/admin/setup/complete', {
      method: 'POST',
      credentials: 'same-origin',
    });
    if (resp.ok) {
      window.location = '/';
    } else {
      btn.disabled = false;
      alert('Failed to complete setup: ' + resp.statusText);
    }
  }

  function buildBody(form) {
    const fd = new FormData(form);
    const out = {};
    for (const [k, v] of fd.entries()) {
      // Number-coerce numeric inputs; boolean for checkboxes marked true.
      const el = form.querySelector(`[name="${CSS.escape(k)}"]`);
      if (el && el.type === 'number') out[k] = parseInt(v, 10);
      else if (el && el.type === 'checkbox') out[k] = true;
      else out[k] = v;
    }
    // OCR languages come in as CSV; split.
    if (out.ocr_languages_csv !== undefined) {
      out.ocr_languages = String(out.ocr_languages_csv)
        .split(/[,\s]+/).map(s => s.trim()).filter(Boolean);
      delete out.ocr_languages_csv;
    }
    return out;
  }

  // Wizard step order — same as the server allowlist. After finishing
  // step N, the next href picks step N+1 (skipping "done", which is
  // just the recap state).
  const ORDER = ['welcome', 'users', 'mail', 'llm', 'jd', 'rules', 'sources', 'preferences'];
  function advanceParam(current) {
    const i = ORDER.indexOf(current);
    if (i === -1 || i === ORDER.length - 1) return '';
    return '?step=' + ORDER[i + 1];
  }
})();
