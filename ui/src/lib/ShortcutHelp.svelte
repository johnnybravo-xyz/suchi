<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import { t as translate } from "./i18n.svelte.js";
  import Icon from './Icon.svelte'

  // Lists the keyboard shortcuts the app already handles; opened with ?.
  let { onClose } = $props()
  let dialog
  const apple = /Mac|iPhone|iPad/.test(navigator.platform)
  const mod = apple ? '⌘' : 'Ctrl'
  const shortcuts = [
    { keys: [mod, 'K'], label: translate("shortcutHelp.search") },
    { keys: [mod, 'Z'], label: translate("shortcutHelp.undo") },
    { keys: ['J', 'K'], label: translate("shortcutHelp.moveThroughDocuments"), alternatives: true },
    { keys: ['X'], label: translate("shortcutHelp.selectDocument") },
    { keys: ['Enter'], label: translate("shortcutHelp.openDocument") },
    { keys: [mod, 'Enter'], label: translate("shortcutHelp.applySuggestion") },
    { keys: ['Esc'], label: translate("shortcutHelp.close") },
    { keys: ['?'], label: translate("shortcutHelp.showShortcuts") },
  ]

  function close() {
    dialog.close()
    onClose?.()
  }

  $effect(() => {
    dialog.showModal()
    return () => dialog.close()
  })
</script>

<dialog bind:this={dialog} class="modal" style="width:min(440px,94vw)"
        oncancel={(event) => { event.preventDefault(); close() }}
        aria-labelledby="shortcut-help-title">
  <div class="modal-head">
    <h3 id="shortcut-help-title">{translate("shortcutHelp.keyboardShortcuts")}</h3>
    <button class="btn sm" onclick={close} title={translate("common.close")} aria-label={translate("common.close")}><Icon name="x" size={13} /></button>
  </div>
  <dl class="shortcuts">
    {#each shortcuts as shortcut (shortcut.label)}
      <dt>
        {#each shortcut.keys as key, i}
          {#if i}<span class="joiner">{shortcut.alternatives ? '/' : '+'}</span>{/if}<kbd>{key}</kbd>
        {/each}
      </dt>
      <dd>{shortcut.label}</dd>
    {/each}
  </dl>
</dialog>

<style>
  dialog { margin: auto; color: var(--ink); }
  dialog::backdrop { background: rgba(0, 0, 0, .45); }
  .shortcuts { display: grid; grid-template-columns: max-content 1fr; gap: 9px 18px; margin: 0; align-items: center; }
  dt { display: flex; align-items: center; gap: 4px; white-space: nowrap; }
  dd { margin: 0; color: var(--muted); font-size: .84rem; }
  kbd { min-width: 22px; padding: 2px 6px; border: 1px solid var(--line-strong); border-radius: 5px;
    background: var(--surface-2); font: 600 .74rem ui-monospace, monospace; text-align: center; }
  .joiner { color: var(--muted); font-size: .72rem; }
</style>
