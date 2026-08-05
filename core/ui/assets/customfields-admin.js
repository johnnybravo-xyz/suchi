// Admin CRUD for /admin/custom-fields. Talks to /api/custom_fields/*
// via fetch. Choices for select/multi are stored under extra_data
// as {"choices": ["A","B","C"]} — the modal has a textarea, one
// choice per line.

(() => {
  const dialog = document.getElementById('cf-editor');
  const form   = dialog?.querySelector('form');
  const title  = document.getElementById('cf-editor-title');
  if (!dialog || !form) return;

  const choicesRow = form.querySelector('[data-choices-row]');
  const dtSelect   = form.elements.data_type;
  let editingID    = null;

  function syncChoicesRow() {
    const needs = dtSelect.value === 'select' || dtSelect.value === 'multi';
    choicesRow.style.display = needs ? '' : 'none';
  }
  dtSelect.addEventListener('change', syncChoicesRow);

  function openForm({ id, name, dataType, choices }) {
    editingID = id;
    title.textContent = id ? `Edit: ${name}` : 'New custom field';
    form.elements.name.value      = name || '';
    form.elements.data_type.value = dataType || 'text';
    form.elements.data_type.disabled = !!id; // type is immutable after create
    form.elements.choices.value   = (choices || []).join('\n');
    syncChoicesRow();
    dialog.showModal();
  }

  document.querySelector('[data-new-field]')?.addEventListener('click', () => {
    openForm({ id: null, name: '', dataType: 'text', choices: [] });
  });

  document.querySelectorAll('tr[data-field-id]').forEach((tr) => {
    const id = Number(tr.dataset.fieldId);

    tr.querySelector('[data-edit]')?.addEventListener('click', () => {
      let choices = [];
      try {
        const extra = JSON.parse(tr.dataset.extra || '{}');
        if (Array.isArray(extra.choices)) choices = extra.choices;
      } catch (_) {}
      openForm({
        id,
        name:     tr.dataset.name,
        dataType: tr.dataset.dataType,
        choices,
      });
    });

    tr.querySelector('[data-delete]')?.addEventListener('click', async () => {
      if (!confirm(`Delete custom field "${tr.dataset.name}"?\n\nThis is destructive — every doc's value for this field is lost.`)) return;
      const r = await fetch(`/api/custom_fields/${id}`, { method: 'DELETE' });
      if (r.ok) {
        tr.remove();
      } else {
        alert(`Delete failed: HTTP ${r.status}`);
      }
    });
  });

  form.querySelectorAll('[data-close]').forEach((b) => {
    b.addEventListener('click', () => dialog.close());
  });

  form.querySelector('[data-save]').addEventListener('click', async (e) => {
    e.preventDefault();
    const name     = form.elements.name.value.trim();
    const dataType = form.elements.data_type.value;
    if (!name) { alert('Name required'); return; }

    const body = { name };
    if (!editingID) body.data_type = dataType; // only on create

    if (dataType === 'select' || dataType === 'multi') {
      const choices = form.elements.choices.value
        .split('\n').map(s => s.trim()).filter(Boolean);
      body.extra_data = JSON.stringify({ choices });
    }

    const url    = editingID ? `/api/custom_fields/${editingID}` : '/api/custom_fields/';
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
