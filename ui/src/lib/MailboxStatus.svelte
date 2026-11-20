<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { plural } from './format.js'
  import Icon from './Icon.svelte'

  let { count, href } = $props()
  const label = $derived(plural('app.mailboxNeedsAttention', count))
</script>

<a class="mailbox-error-ribbon" {href} aria-label={label} title={label}>
  <span class="mailbox-error-ribbon-dot" aria-hidden="true"></span>
  <Icon name="mail" size={14} />
  <span class="mailbox-error-ribbon-label">{label}</span>
  <span class="mailbox-error-ribbon-count" aria-hidden="true">{count}</span>
</a>
