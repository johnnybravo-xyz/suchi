// Inbox resolve buttons. One click → POST to
// /api/approvals/tasks/{id}/resolve with {choice}. On success the item
// fades and is removed; the topbar inbox pill refreshes so the count
// tracks the visible list.

(function () {
  const items = document.querySelectorAll('.inbox-item');
  if (!items.length) return;

  document.querySelectorAll('button.inbox-choice').forEach(function (btn) {
    btn.addEventListener('click', async function () {
      const id = btn.getAttribute('data-task-id');
      const choice = btn.getAttribute('data-choice');
      const card = btn.closest('.inbox-item');
      const buttons = card.querySelectorAll('button.inbox-choice');
      buttons.forEach(b => b.disabled = true);
      try {
        const r = await fetch(`/api/approvals/tasks/${id}/resolve`, {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            'Accept': 'application/json',
          },
          credentials: 'same-origin',
          body: JSON.stringify({ choice }),
        });
        if (!r.ok) {
          buttons.forEach(b => b.disabled = false);
          const err = await r.json().catch(() => ({}));
          alert('Failed: ' + (err.error || r.statusText));
          return;
        }
        card.style.transition = 'opacity 180ms ease-out, transform 180ms ease-out';
        card.style.opacity = '0';
        card.style.transform = 'translateY(-4px)';
        setTimeout(function () {
          card.remove();
          // Trigger a pill refresh — inbox.js exposes nothing so we
          // reload if the list is now empty, else let the periodic
          // refresh catch it.
          if (!document.querySelector('.inbox-item')) location.reload();
        }, 200);
      } catch (e) {
        buttons.forEach(b => b.disabled = false);
        alert('Network error: ' + e.message);
      }
    });
  });
})();
