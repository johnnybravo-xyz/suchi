// Inbox pill count refresher. Reads the workflow_open value from
// /api/tasks/?include=workflow&limit=1 and reflects it on the topbar
// pill. Runs every 60s. The pill is server-rendered but starts hidden
// when the boot count is zero; JS toggles visibility so the pill can
// appear without a full nav reload.

(function () {
  const pill = document.getElementById('inbox-pill');
  if (!pill) return;
  const countEl = pill.querySelector('.count');

  async function refresh() {
    try {
      const r = await fetch('/api/tasks/?include=workflow&limit=1', {
        headers: { 'Accept': 'application/json' },
        credentials: 'same-origin',
      });
      if (!r.ok) return;
      const body = await r.json();
      const n = (body.counts && body.counts.workflow_open) | 0;
      countEl.textContent = String(n);
      pill.hidden = n === 0;
    } catch (_) {
      // silent — no toast for a background counter
    }
  }

  refresh();
  setInterval(refresh, 60000);
  document.addEventListener('visibilitychange', function () {
    if (!document.hidden) refresh();
  });
})();
