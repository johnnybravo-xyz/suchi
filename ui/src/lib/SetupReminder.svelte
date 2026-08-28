<script>
  import Icon from './Icon.svelte'

  let { placement = 'side', onClose, onContinue } = $props()
</script>

<aside class="setup-reminder setup-reminder-{placement}" aria-label="Setup wizard">
  <div class="setup-reminder-head">
    <span class="setup-reminder-icon"><Icon name="settings" size={16} /></span>
    <div>
      <b>Choose your filing tree</b>
      <span>Required to finish setup. You can reopen Setup anytime from Settings.</span>
    </div>
    <button class="btn sm setup-reminder-close" onclick={onClose}
            title="Close setup reminder" aria-label="Close setup reminder">
      <Icon name="x" size={13} />
    </button>
  </div>
  <a role="button" class="btn primary sm" href="#/setup" onclick={onContinue}>Continue setup</a>
</aside>
