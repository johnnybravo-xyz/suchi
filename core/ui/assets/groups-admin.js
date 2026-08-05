// Admin UI wiring for /admin/groups. Talks to /api/groups/* via
// fetch — the server pre-hydrates each group's members table so the
// initial paint is one query, not one-per-group.

(() => {
  const dialog = document.getElementById('group-editor');
  const form   = dialog?.querySelector('form');
  const title  = document.getElementById('group-editor-title');
  if (!dialog || !form) return;

  let editingID = null;

  function openForm({ id, name, description }) {
    editingID = id;
    title.textContent = id ? `Edit: ${name}` : 'New group';
    form.elements.name.value        = name || '';
    form.elements.description.value = description || '';
    dialog.showModal();
  }

  // New button.
  document.querySelector('[data-new-group]')?.addEventListener('click', () => {
    openForm({ id: null, name: '', description: '' });
  });

  // Per-group buttons.
  document.querySelectorAll('.group-card').forEach((card) => {
    const id = Number(card.dataset.groupId);

    card.querySelector('[data-edit-group]')?.addEventListener('click', () => {
      openForm({
        id,
        name:        card.dataset.name,
        description: card.dataset.description,
      });
    });

    card.querySelector('[data-delete-group]')?.addEventListener('click', async () => {
      if (!confirm(`Delete group "${card.dataset.name}"?\n\nRevoke all grants first — the server refuses otherwise.`)) return;
      const r = await fetch(`/api/groups/${id}`, { method: 'DELETE' });
      if (r.ok) {
        card.remove();
      } else if (r.status === 409) {
        const body = await r.text();
        alert(`Cannot delete: ${body}`);
      } else {
        alert(`Delete failed: HTTP ${r.status}`);
      }
    });

    // Add member.
    card.querySelector('[data-add-member]')?.addEventListener('click', async () => {
      const sel = card.querySelector('[data-add-user]');
      const uid = Number(sel?.value);
      if (!uid) return;
      const r = await fetch(`/api/groups/${id}/members`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ user_id: uid }),
      });
      if (r.ok) {
        location.reload();
      } else {
        alert(`Add failed: HTTP ${r.status}`);
      }
    });

    // Remove member.
    card.querySelectorAll('[data-remove-member]').forEach((btn) => {
      btn.addEventListener('click', async () => {
        const tr  = btn.closest('tr');
        const uid = Number(tr?.dataset.userId);
        if (!uid) return;
        if (!confirm('Remove this member?')) return;
        const r = await fetch(`/api/groups/${id}/members/${uid}`, { method: 'DELETE' });
        if (r.ok) {
          tr.remove();
        } else {
          alert(`Remove failed: HTTP ${r.status}`);
        }
      });
    });
  });

  // Close buttons.
  form.querySelectorAll('[data-close]').forEach((b) => {
    b.addEventListener('click', () => dialog.close());
  });

  // Save.
  form.querySelector('[data-save]').addEventListener('click', async (e) => {
    e.preventDefault();
    const name        = form.elements.name.value.trim();
    const description = form.elements.description.value.trim();
    if (!name) { alert('Name required'); return; }
    const url    = editingID ? `/api/groups/${editingID}` : '/api/groups/';
    const method = editingID ? 'PATCH' : 'POST';
    const r = await fetch(url, {
      method,
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ name, description }),
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
