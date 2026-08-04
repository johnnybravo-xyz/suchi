// suchi mail-setup wizard — vanilla JS. External so CSP default-src
// 'self' passes without 'unsafe-inline'.
//
// Behavior:
//   - Provider radio buttons apply host/port/SSL defaults + a help
//     hint. Values still fully editable.
//   - Submit POSTs JSON to /api/admin/mail-setup, renders a status
//     string from the response.

(function () {
  const form = document.getElementById('mailsetup-form');
  if (!form) { return; }
  const status = document.getElementById('mailsetup-status');
  const submit = document.getElementById('mailsetup-submit');
  const hint = document.getElementById('provider-hint');
  const msg = form.dataset;

  const defaults = {
    proton:   { host: 'protonmail-bridge',   port: 143, ssl: 'None',    hint: msg.hintProton   },
    gmail:    { host: 'imap.gmail.com',      port: 993, ssl: 'IMAPS',   hint: msg.hintGmail    },
    fastmail: { host: 'imap.fastmail.com',   port: 993, ssl: 'IMAPS',   hint: msg.hintFastmail },
    generic:  { host: '',                    port: 993, ssl: 'IMAPS',   hint: ''               },
  };

  function apply(provider) {
    const d = defaults[provider];
    form.mail_host.value = d.host;
    form.mail_port.value = d.port;
    form.mail_ssl.value = d.ssl;
    hint.textContent = d.hint || '';
  }
  // Initial default matches the checked radio at render.
  apply(form.querySelector('input[name=provider]:checked').value);

  for (const el of form.querySelectorAll('input[name=provider]')) {
    el.addEventListener('change', e => apply(e.target.value));
  }

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    submit.disabled = true;
    status.className = 'status muted';
    status.textContent = msg.msgSaving;
    const fd = new FormData(form);
    const body = {
      provider: fd.get('provider'),
      mail_host: fd.get('mail_host'),
      mail_port: parseInt(fd.get('mail_port'), 10),
      mail_ssl: fd.get('mail_ssl'),
      mail_user: fd.get('mail_user'),
      mail_password: fd.get('mail_password'),
      mail_folders: fd.get('mail_folders'),
      max_messages: parseInt(fd.get('max_messages'), 10),
      max_size: fd.get('max_size'),
      sync_interval_seconds: parseInt(fd.get('sync_interval'), 10),
    };
    try {
      const resp = await fetch('/api/admin/mail-setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' },
        credentials: 'same-origin',
        body: JSON.stringify(body),
      });
      const data = await resp.json().catch(() => ({}));
      if (!resp.ok) {
        status.className = 'status is-error';
        status.textContent = 'Error: ' + ((data.error && data.error.message) || resp.statusText);
        submit.disabled = false;
        return;
      }
      const parts = [msg.msgWrote];
      if (data.restarted) {
        parts.push(msg.msgRestarted);
      } else if (data.restart_attempted) {
        parts.push('Restart attempted but failed: ' + (data.restart_error || 'unknown'));
      } else {
        parts.push(msg.msgNoRestart);
      }
      status.className = 'status is-ok';
      status.textContent = parts.join(' ');
    } catch (err) {
      status.className = 'status is-error';
      status.textContent = 'Error: ' + err.message;
    } finally {
      submit.disabled = false;
    }
  });
})();
