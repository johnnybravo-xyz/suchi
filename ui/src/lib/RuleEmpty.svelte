<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  let {
    title,
    description = '',
    patterns = [],
    onselect,
    actions,
    class: className = '',
  } = $props()
</script>

<div class={`rule-empty ${className}`}>
  <h3>{title}</h3>
  {#if description}<p>{description}</p>{/if}
  {#if patterns.length}
    <div class="rule-patterns">
      {#each patterns as pattern (pattern.value)}
        <button type="button" class="rule-pattern" onclick={() => onselect?.(pattern.value)}>
          {pattern.label}
        </button>
      {/each}
    </div>
  {/if}
  {@render actions?.()}
</div>

<style>
  .rule-empty {
    display: grid;
    justify-items: center;
    gap: 6px;
    padding: 54px 22px 58px;
    text-align: center;
  }
  .rule-empty h3 { margin: 0; font-size: .95rem; }
  .rule-empty p { margin: 0; color: var(--muted); font-size: .8rem; }
  .rule-patterns {
    display: flex;
    justify-content: center;
    gap: 8px;
    margin-top: 10px;
    flex-wrap: wrap;
  }
  .rule-pattern {
    padding: 7px 12px;
    border: 1px solid var(--line);
    border-radius: 7px;
    background: var(--surface);
    color: var(--ink);
    font-size: .75rem;
    font-weight: 600;
    cursor: pointer;
  }
  .rule-pattern:hover { border-color: var(--accent); color: var(--accent); }
</style>
