<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  let {
    title = '',
    headingId = '',
    tone = 'default',
    class: className = '',
    children,
  } = $props()
</script>

<section class={`rule-section ${className}`} class:quiet={tone === 'quiet'} aria-labelledby={headingId || undefined}>
  {#if title}<h3 id={headingId || undefined}>{title}</h3>{/if}
  {@render children?.()}
</section>

<style>
  .rule-section {
    padding: 18px 20px;
    border-bottom: 1px solid var(--line);
  }
  .rule-section.quiet { background: var(--surface-2); }
  .rule-section > h3 {
    margin: 0 0 12px;
    color: var(--muted);
    font-family: "Spline Sans Mono", ui-monospace, monospace;
    font-size: .58rem;
    font-weight: 700;
    letter-spacing: .09em;
    text-transform: uppercase;
  }
  @media (max-width: 520px) {
    .rule-section { padding-right: 14px; padding-left: 14px; }
  }
</style>
