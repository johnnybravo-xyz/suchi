<script>
  // Mail settings panel. Server contract (task #141) is PROPOSED:
  //   GET/PUT /api/admin/settings/mail  · POST /api/admin/settings/mail/test
  // Secrets are write-only: GET returns password masked/absent; leaving the
  // field blank on save means "keep the stored one". Degrades on 404.
  import { getMailSettings, putMailSettings, testMailSettings } from './api.js'
  import Icon from './Icon.svelte'

  let { notify, onSaved } = $props()
  let m = $state({ imap_host: '', imap_port: 993, username: '', password: '', folder: 'INBOX', poll_interval_min: 10, owner_email: '' })
  let supported = $state(true)
  let busy = $state(false)
  let testMsg = $state(null)   // {ok, text}

  getMailSettings().then(cfg => { if (cfg) m = { ...m, ...cfg, password: '' } })
    .catch(ex => { if (ex.status === 404 || ex.status === 405) supported = false })

  async function save() {
    busy = true
    try {
      const body = { ...m, imap_port: Number(m.imap_port) || 993, poll_interval_min: Number(m.poll_interval_min) || 10 }
      if (!body.password) delete body.password   // blank = keep stored secret
      await putMailSettings(body)
      notify?.('Mail intake saved')
      onSaved?.()
    } catch (ex) {
      if (ex.status === 404 || ex.status === 405) supported = false
      else notify?.(ex.message || 'Could not save mail settings')
    } finally { busy = false }
  }

  async function test() {
    busy = true; testMsg = null
    try {
      const res = await testMailSettings({})
      testMsg = { ok: true, text: res?.message || 'Connected — mailbox reachable.' }
    } catch (ex) {
      if (ex.status === 404 || ex.status === 405) supported = false
      else testMsg = { ok: false, text: ex.message || 'Connection failed.' }
    } finally { busy = false }
  }
</script>

{#if !supported}
  <div class="err" style="background:var(--warn-soft);color:var(--warn)">
    This server doesn't expose the mail-settings API yet (backend task #141).
    Until it lands, configure mail intake via <code>suchi.toml</code> — see <code>docs/config.mdx</code>.
  </div>
{:else}
  <div class="toolbar" style="margin-bottom:0">
    <div class="field" style="flex:2;min-width:180px"><label for="m-host">IMAP host</label>
      <input id="m-host" class="input mono" placeholder="imap.fastmail.com" bind:value={m.imap_host} /></div>
    <div class="field" style="max-width:110px"><label for="m-port">Port</label>
      <input id="m-port" class="input mono" type="number" bind:value={m.imap_port} /></div>
  </div>
  <div class="toolbar" style="margin-bottom:0">
    <div class="field" style="flex:1;min-width:160px"><label for="m-user">Username</label>
      <input id="m-user" class="input mono" bind:value={m.username} autocomplete="off" /></div>
    <div class="field" style="flex:1;min-width:160px"><label for="m-pass">Password / app token</label>
      <input id="m-pass" class="input mono" type="password" bind:value={m.password}
             placeholder="unchanged if left blank" autocomplete="new-password" /></div>
  </div>
  <div class="toolbar" style="margin-bottom:6px">
    <div class="field" style="max-width:160px"><label for="m-folder">Folder</label>
      <input id="m-folder" class="input mono" bind:value={m.folder} /></div>
    <div class="field" style="max-width:140px"><label for="m-poll">Poll every (min)</label>
      <input id="m-poll" class="input" type="number" min="1" bind:value={m.poll_interval_min} /></div>
    <div class="field" style="flex:1;min-width:170px"><label for="m-owner">Documents belong to</label>
      <input id="m-owner" class="input" type="email" placeholder="owner email" bind:value={m.owner_email} /></div>
  </div>
  {#if testMsg}
    <p class="sub" style="margin:0 0 10px;color:{testMsg.ok ? 'var(--ok)' : 'var(--danger)'};font-size:.84rem">{testMsg.text}</p>
  {/if}
  <div class="toolbar" style="margin:0">
    <button class="btn primary sm" disabled={busy || !m.imap_host} onclick={save}>Save mail intake</button>
    <button class="btn sm" disabled={busy || !m.imap_host} onclick={test}><Icon name="mail" size={13} /> Test connection</button>
  </div>
{/if}
