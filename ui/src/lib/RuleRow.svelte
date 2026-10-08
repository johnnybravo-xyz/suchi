<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  let {
    id = undefined,
    title,
    summary = '',
    href = '',
    disabled = false,
    class: className = '',
    leading,
    titleMeta,
    trailing,
    details,
    actionText = '',
    actionLabel = '',
    detailText = '',
    actionDisabled = false,
    onaction,
    ondragover,
    ondrop,
  } = $props()
</script>

<div {id}
     class={`rule-row ${className}`}
     class:has-leading={!!leading}
     class:disabled
     role="listitem"
     {ondragover}
     {ondrop}>
  {#if leading}
    <div class="rule-leading">{@render leading()}</div>
  {/if}

  {#if href}
    <a class="rule-main" {href}>
      <span class="rule-title">
        <strong>{title}</strong>
        {@render titleMeta?.()}
      </span>
      {#if summary}<span class="rule-sentence">{summary}</span>{/if}
    </a>
  {:else}
    <div class="rule-main">
      <span class="rule-title">
        <strong>{title}</strong>
        {@render titleMeta?.()}
      </span>
      {#if summary}<span class="rule-sentence">{summary}</span>{/if}
    </div>
  {/if}

  {#if trailing || actionText}
    <div class="rule-trailing">
      {@render trailing?.()}
      {#if actionText}
        <button type="button" class="rule-action" disabled={actionDisabled}
                aria-label={actionLabel || actionText} onclick={onaction}>
          {actionText}
        </button>
      {/if}
    </div>
  {/if}

  {#if details || detailText}
    <div class="rule-details">
      {#if detailText}<pre class="rule-detail">{detailText}</pre>{/if}
      {@render details?.()}
    </div>
  {/if}
</div>

<style>
  .rule-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    align-items: center;
    gap: 10px;
    min-height: 68px;
    padding: 10px 18px;
    border-bottom: 1px solid var(--line);
    transition: background .12s ease;
  }
  .rule-row.has-leading { grid-template-columns: 24px minmax(0, 1fr) auto; }
  .rule-row:last-child { border-bottom: 0; }
  .rule-row:hover { background: var(--surface-2); }
  .rule-row.disabled .rule-main { opacity: .55; }
  .rule-leading {
    display: grid;
    place-items: center;
    width: 24px;
  }
  .rule-main {
    display: flex;
    min-width: 0;
    align-self: stretch;
    justify-content: center;
    flex-direction: column;
    color: inherit;
    text-decoration: none;
  }
  a.rule-main:focus-visible {
    border-radius: 5px;
    outline: 2px solid var(--accent);
    outline-offset: 4px;
  }
  .rule-title {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
    font-size: .84rem;
  }
  .rule-title strong {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .rule-sentence {
    margin-top: 3px;
    overflow: hidden;
    color: var(--muted);
    font-family: "Spline Sans Mono", ui-monospace, monospace;
    font-size: .7rem;
    line-height: 1.4;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .rule-trailing {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: 12px;
    min-width: 0;
  }
  .rule-action {
    min-width: 30px;
    padding: 5px 2px;
    border: 0;
    background: transparent;
    color: var(--ink);
    font: inherit;
    font-size: .78rem;
    cursor: pointer;
  }
  .rule-detail {
    max-height: 320px;
    margin: 7px 0 2px;
    padding: 12px;
    overflow: auto;
    border: 1px solid var(--line);
    border-radius: 7px;
    background: var(--surface-2);
    font-size: .72rem;
    white-space: pre-wrap;
  }
  .rule-action:hover { color: var(--accent); }
  .rule-action:disabled { cursor: not-allowed; opacity: .5; }
  .rule-details { grid-column: 1 / -1; min-width: 0; }

  @media (max-width: 520px) {
    .rule-row.has-leading { grid-template-columns: 24px minmax(0, 1fr); }
    .rule-row:not(.has-leading) { grid-template-columns: minmax(0, 1fr); }
    .rule-row.has-leading .rule-trailing { grid-column: 2; }
    .rule-row:not(.has-leading) .rule-trailing { grid-column: 1; }
    .rule-trailing { justify-self: end; }
    .rule-sentence {
      overflow: visible;
      text-overflow: clip;
      white-space: normal;
    }
  }
</style>
