<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import Icon from './Icon.svelte'

  let { id, value = $bindable(''), label = 'Date', title = label,
        includeTime = false, compact = false, onchange } = $props()
  let textInput
  let nativePicker
  let draft = $state(value)
  let committed = $state(value)
  const format = $derived(includeTime ? 'yyyy-mm-ddThh:mm' : 'yyyy-mm-dd')
  const nativeValue = $derived(validValue(draft) ? draft : '')

  $effect(() => {
    if (value !== committed) {
      committed = value
      draft = value
    }
  })

  function validDate(value) {
    const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value)
    if (!match) return false
    const year = Number(match[1]), month = Number(match[2]), day = Number(match[3])
    const parsed = new Date(Date.UTC(year, month - 1, day))
    return parsed.getUTCFullYear() === year && parsed.getUTCMonth() === month - 1 && parsed.getUTCDate() === day
  }

  function validValue(next) {
    if (next === '') return true
    if (!includeTime) return validDate(next)
    const match = /^(\d{4}-\d{2}-\d{2})T(\d{2}):(\d{2})$/.exec(next)
    return !!match && validDate(match[1]) && Number(match[2]) < 24 && Number(match[3]) < 60
  }

  function edit(event) {
    draft = event.currentTarget.value
    event.currentTarget.setCustomValidity(validValue(draft) ? '' : `Use ${format}`)
  }

  function commit(next) {
    if (!validValue(next)) {
      textInput?.reportValidity()
      return
    }
    draft = next
    committed = next
    value = next
    textInput?.setCustomValidity('')
    onchange?.(next)
  }

  function choose(event) {
    commit(event.currentTarget.value)
  }

  function openPicker() {
    try {
      if (nativePicker?.showPicker) nativePicker.showPicker()
      else nativePicker?.click()
    } catch {
      nativePicker?.focus()
      nativePicker?.click()
    }
  }
</script>

<span class:compact class="iso-date-input">
  <input bind:this={textInput} {id} class="input mono" type="text" value={draft}
         placeholder={format} title={title} aria-label={label} maxlength={includeTime ? 16 : 10}
         inputmode="numeric" autocomplete="off" spellcheck="false"
         oninput={edit} onchange={() => commit(draft)} />
  <button class="calendar-button" type="button" title={`Choose ${label.toLowerCase()}`}
          aria-label={`Choose ${label.toLowerCase()}`} onclick={openPicker}>
    <Icon name="calendar" size={17} />
  </button>
  <input bind:this={nativePicker} class="native-picker" type={includeTime ? 'datetime-local' : 'date'}
         value={nativeValue} tabindex="-1" aria-hidden="true" onchange={choose} />
</span>

<style>
  .iso-date-input { position: relative; display: inline-flex; width: 100%; }
  .iso-date-input.compact { flex: 0 1 168px; width: 168px; max-width: 100%; }
  .input { padding-right: 38px; font-variant-numeric: tabular-nums; }
  .calendar-button {
    position: absolute; top: 1px; right: 1px; bottom: 1px; display: grid;
    width: 36px; place-items: center; border: 0; border-left: 1px solid var(--line);
    border-radius: 0 8px 8px 0; background: transparent; color: var(--muted);
  }
  .calendar-button:hover { color: var(--accent); }
  .calendar-button:focus-visible { outline: 2px solid var(--accent); outline-offset: -2px; }
  .native-picker {
    position: absolute; right: 18px; bottom: 0; width: 1px; height: 1px;
    opacity: 0; pointer-events: none;
  }
</style>
