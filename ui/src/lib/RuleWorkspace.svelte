<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  let {
    title,
    description = '',
    headingId = 'rule-workspace-heading',
    class: className = '',
    actions,
    children,
    footer,
  } = $props()
</script>

<section class={`rule-workspace ${className}`} aria-labelledby={headingId}>
  <header class="rule-workspace-header">
    <div class="rule-workspace-title">
      <h2 id={headingId}>{title}</h2>
      {#if description}<p>{description}</p>{/if}
    </div>
    {#if actions}
      <div class="rule-workspace-actions">{@render actions()}</div>
    {/if}
  </header>

  {@render children?.()}

  {#if footer}
    <footer class="rule-workspace-footer">{@render footer()}</footer>
  {/if}
</section>

<style>
  .rule-workspace {
    overflow: hidden;
    border: 1px solid var(--line);
    border-radius: 12px;
    background: var(--surface);
    box-shadow: 0 12px 32px color-mix(in srgb, var(--ink) 5%, transparent);
  }
  .rule-workspace-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 24px;
    min-height: 68px;
    padding: 14px 20px;
    border-bottom: 1px solid var(--line);
  }
  .rule-workspace-title {
    display: flex;
    align-items: baseline;
    gap: 10px;
    min-width: 0;
  }
  .rule-workspace-title h2 {
    margin: 0;
    flex: none;
    font-size: 1rem;
    line-height: 1.3;
  }
  .rule-workspace-title p {
    margin: 0;
    overflow: hidden;
    color: var(--muted);
    font-size: .82rem;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .rule-workspace-actions {
    display: flex;
    align-items: center;
    gap: 12px;
    flex: none;
  }
  .rule-workspace-footer {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 18px;
    min-height: 68px;
    padding: 14px 20px;
    background: var(--surface);
  }

  @media (max-width: 620px) {
    .rule-workspace-header {
      align-items: flex-start;
      gap: 14px;
      padding: 16px 20px;
      flex-wrap: wrap;
    }
    .rule-workspace-title {
      width: 100%;
      align-items: flex-start;
      flex-direction: column;
      gap: 5px;
    }
    .rule-workspace-title p {
      overflow: visible;
      line-height: 1.45;
      text-overflow: clip;
      white-space: normal;
    }
    .rule-workspace-actions {
      width: 100%;
      justify-content: space-between;
    }
    .rule-workspace-footer {
      align-items: stretch;
      flex-direction: column;
      gap: 12px;
    }
  }
</style>
