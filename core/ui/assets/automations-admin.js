// Admin UI wiring for /admin/automations. Talks to /api/automations/*
// via fetch — the server-rendered rows carry the current spec as a
// data-spec attribute so opening the editor doesn't need a round-trip.

(() => {
  const dialog = document.getElementById('auto-editor');
  const form   = dialog?.querySelector('form');
  const title  = document.getElementById('auto-editor-title');
  if (!dialog || !form) return;

  let editingID = null;

  function fillForm({ id, name, order, enabled, spec }) {
    editingID = id;
    title.textContent = id ? `Edit: ${name}` : 'New automation';
    form.elements.name.value    = name || '';
    form.elements.order.value   = order ?? 0;
    form.elements.enabled.checked = enabled !== false;
    form.elements.spec.value    = spec || '{"triggers":[],"actions":[]}';
    dialog.showModal();
  }

  // "New" button.
  document.querySelector('[data-new-automation]')?.addEventListener('click', () => {
    fillForm({ id: null, name: '', order: 0, enabled: true });
  });

  // Row buttons — edit / delete / enabled toggle.
  document.querySelectorAll('tr[data-automation-id]').forEach((tr) => {
    const id = Number(tr.dataset.automationId);

    tr.querySelector('[data-edit]')?.addEventListener('click', () => {
      fillForm({
        id,
        name:    tr.dataset.name,
        order:   Number(tr.dataset.order),
        enabled: tr.dataset.enabled === '1',
        spec:    tr.dataset.spec,
      });
    });

    tr.querySelector('[data-delete]')?.addEventListener('click', async () => {
      if (!confirm(`Delete automation "${tr.dataset.name}"?`)) return;
      const r = await fetch(`/api/automations/${id}`, { method: 'DELETE' });
      if (r.ok) tr.remove();
      else alert(`Delete failed: HTTP ${r.status}`);
    });

    tr.querySelector('[data-enabled-toggle]')?.addEventListener('change', async (e) => {
      const enabled = e.target.checked;
      // Send the full spec back — the API replaces triggers+actions on
      // PATCH, so we round-trip what we already have in dataset.spec.
      let spec = {};
      try { spec = JSON.parse(tr.dataset.spec); } catch (_) {}
      const body = {
        name:     tr.dataset.name,
        order:    Number(tr.dataset.order),
        enabled,
        triggers: spec.triggers || [],
        actions:  spec.actions  || [],
      };
      const r = await fetch(`/api/automations/${id}`, {
        method: 'PATCH',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (r.ok) {
        tr.dataset.enabled = enabled ? '1' : '0';
      } else {
        e.target.checked = !enabled;
        alert(`Toggle failed: HTTP ${r.status}`);
      }
    });
  });

  // Close buttons.
  form.querySelectorAll('[data-close]').forEach((b) => {
    b.addEventListener('click', () => dialog.close());
  });

  // Save.
  form.querySelector('[data-save]').addEventListener('click', async (e) => {
    e.preventDefault();
    const name    = form.elements.name.value.trim();
    const order   = Number(form.elements.order.value) || 0;
    const enabled = form.elements.enabled.checked;
    const raw     = form.elements.spec.value;

    if (!name) { alert('Name required'); return; }
    let spec;
    try { spec = JSON.parse(raw); }
    catch (err) { alert(`Spec is not valid JSON: ${err.message}`); return; }

    const body = {
      name, order, enabled,
      triggers: spec.triggers || [],
      actions:  spec.actions  || [],
    };
    const url    = editingID ? `/api/automations/${editingID}` : '/api/automations/';
    const method = editingID ? 'PATCH' : 'POST';
    const r = await fetch(url, {
      method,
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (r.ok) {
      dialog.close();
      location.reload();
    } else {
      const t = await r.text().catch(() => '');
      alert(`Save failed: HTTP ${r.status}\n${t}`);
    }
  });
})();
